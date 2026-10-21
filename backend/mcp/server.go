// Package mcp 提供 FlowEdge 的 MCP（Model Context Protocol）服务端。
//
// 定位：这是「生态广度」里性价比最高的接入点——一个 MCP server 出口，
// 让 Claude Desktop / Cursor / 任何 MCP 客户端直接调用 FlowEdge 的工作流，
// 而不是自己去维护一百个连接器。MCP 客户端是在增长的那一侧。
//
// 架构选择：**瘦客户端 + HTTP 转发**，不在 stdio 进程里初始化引擎。
//
//	理由：MCP 客户端是短生命周期、可能被拉起多次的；把 DB / ComfyUI / Ollama
//	全栈塞进 stdio 进程，会出现两个引擎抢同一张卡的显存与同一个 sqlite 文件。
//	转发到已在运行的本机引擎，既没有双写风险，用户在 UI 里也能看到同一批运行记录。
//
//	代价：引擎必须在跑。因此每个 tool 调用在引擎不可达时返回一条能照做的错误，
//	而不是超时或空结果。
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const protocolVersion = "2024-11-05"

// Server MCP stdio 服务端
type Server struct {
	BaseURL string // 本机引擎地址，默认 http://127.0.0.1:8080
	Token   string // FLOWEDGE_API_TOKEN（引擎开了鉴权时必填）
	In      io.Reader
	Out     io.Writer

	client *http.Client
}

func NewServer(baseURL, token string, in io.Reader, out io.Writer) *Server {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	return &Server{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		In:      in,
		Out:     out,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve 读循环直到 stdin 关闭
func (s *Server) Serve() error {
	scanner := bufio.NewScanner(s.In)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.write(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
			continue
		}
		// notification（无 id）不回应
		if len(req.ID) == 0 {
			continue
		}
		s.write(s.handle(req))
	}
	return scanner.Err()
}

func (s *Server) write(resp rpcResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	// MCP stdio：一行一条 JSON
	fmt.Fprintf(s.Out, "%s\n", raw)
}

func (s *Server) handle(req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]interface{}{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "flowedge",
				"version": "1.0.0",
			},
		}
	case "ping":
		resp.Result = map[string]interface{}{}
	case "tools/list":
		resp.Result = map[string]interface{}{"tools": toolDefs()}
	case "tools/call":
		res, err := s.callTool(req.Params)
		if err != nil {
			resp.Result = toolError(err.Error())
			return resp
		}
		resp.Result = res
	case "resources/list":
		resp.Result = map[string]interface{}{"resources": []interface{}{}}
	case "prompts/list":
		resp.Result = map[string]interface{}{"prompts": []interface{}{}}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// toolError 把错误包成 MCP 的「工具级错误」而不是协议错误——
// 这样客户端能把它展示给用户，而不是当成连接故障。
func toolError(msg string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": msg}},
		"isError": true,
	}
}

func toolText(text string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"isError": false,
	}
}

type toolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

func (s *Server) callTool(raw json.RawMessage) (map[string]interface{}, error) {
	var p toolCallParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("tools/call 参数解析失败：%w", err)
	}
	switch p.Name {
	case "flowedge_list_workflows":
		body, err := s.request(http.MethodGet, "/api/workflows", nil)
		if err != nil {
			return nil, err
		}
		return toolText(prettyJSON(body)), nil

	case "flowedge_run_workflow":
		id, err := argInt(p.Arguments, "workflow_id")
		if err != nil {
			return nil, err
		}
		payload := map[string]interface{}{}
		if vars, ok := p.Arguments["vars"]; ok {
			payload["vars"] = vars
		}
		body, err := s.request(http.MethodPost, fmt.Sprintf("/api/workflows/%d/run", id), payload)
		if err != nil {
			return nil, err
		}
		return toolText("已提交运行：\n" + prettyJSON(body)), nil

	case "flowedge_run_status":
		id, err := argInt(p.Arguments, "run_id")
		if err != nil {
			return nil, err
		}
		body, err := s.request(http.MethodGet, fmt.Sprintf("/api/runs/%d", id), nil)
		if err != nil {
			return nil, err
		}
		return toolText(prettyJSON(body)), nil

	case "flowedge_estimate":
		id, err := argInt(p.Arguments, "workflow_id")
		if err != nil {
			return nil, err
		}
		body, err := s.request(http.MethodPost, "/api/estimate", map[string]interface{}{"workflow_id": id})
		if err != nil {
			return nil, err
		}
		return toolText("提交前估算（数字是区间，不是承诺）：\n" + prettyJSON(body)), nil

	case "flowedge_generate_image":
		prompt, _ := p.Arguments["prompt"].(string)
		if strings.TrimSpace(prompt) == "" {
			return nil, fmt.Errorf("prompt 不能为空")
		}
		payload := map[string]interface{}{
			"prompt":          prompt,
			"response_format": "url", // 客户端拿 b64 没意义，给可下载地址
		}
		if v, ok := p.Arguments["width"]; ok {
			payload["width"] = v
		}
		if v, ok := p.Arguments["height"]; ok {
			payload["height"] = v
		}
		body, err := s.request(http.MethodPost, "/v1/images/generations", payload)
		if err != nil {
			return nil, err
		}
		return toolText(prettyJSON(body)), nil
	}
	return nil, fmt.Errorf("未知工具 %q（用 tools/list 查可用工具）", p.Name)
}

// request 调本机引擎。不可达时给一条能照做的错误，而不是裸的连接错误。
func (s *Server) request(method, path string, payload interface{}) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewBuffer(raw)
	}
	req, err := http.NewRequest(method, s.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上本机 FlowEdge 引擎（%s）：%v\n"+
			"请先启动 FlowEdge（双击 FlowEdge.exe 或运行 1-启动 FlowEdge.bat），再重试", s.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("引擎要求 API token：请在 MCP 客户端配置里设置环境变量 FLOWEDGE_API_TOKEN")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("引擎返回 %d：%.400s", resp.StatusCode, string(raw))
	}
	return raw, nil
}

func argInt(args map[string]interface{}, key string) (int64, error) {
	v, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("缺少参数 %s", key)
	}
	switch t := v.(type) {
	case float64:
		return int64(t), nil
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case string:
		var n int64
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("参数 %s 必须是整数", key)
}

func prettyJSON(raw []byte) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// toolDefs MCP 工具定义
//
// 描述写"什么时候用"而不是"这个函数做什么"——LLM 客户端靠描述决定要不要调用。
func toolDefs() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"name":        "flowedge_list_workflows",
			"description": "列出本机 FlowEdge 里已保存的工作流（含 id、名称、触发配置）。当用户问「我有哪些工作流」或需要拿到 workflow_id 时使用。",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		map[string]interface{}{
			"name":        "flowedge_run_workflow",
			"description": "提交一个工作流运行（异步，立即返回 run_id）。当用户要出图、出视频、或跑一条本地多模态流水线时使用。返回后可用 flowedge_run_status 查进度。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"workflow_id": map[string]interface{}{"type": "integer", "description": "工作流 id（先调 flowedge_list_workflows）"},
					"vars":        map[string]interface{}{"type": "object", "description": "运行变量，如 {\"prompt\": \"...\"}"},
				},
				"required": []string{"workflow_id"},
			},
		},
		map[string]interface{}{
			"name":        "flowedge_run_status",
			"description": "查询一次运行的进度、节点状态与产物路径。当用户问「跑完了吗」「出到哪一步了」「产出文件在哪」时使用。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"run_id": map[string]interface{}{"type": "integer", "description": "flowedge_run_workflow 返回的 run_id"},
				},
				"required": []string{"run_id"},
			},
		},
		map[string]interface{}{
			"name":        "flowedge_estimate",
			"description": "提交前预估一次运行的显存峰值与耗时（区间 + 置信度）。当用户问「这条会不会 OOM」「要跑多久」时使用——必须先调这个再决定要不要跑。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"workflow_id": map[string]interface{}{"type": "integer", "description": "工作流 id"},
				},
				"required": []string{"workflow_id"},
			},
		},
		map[string]interface{}{
			"name":        "flowedge_generate_image",
			"description": "用本地 GPU 直接生成一张（或几张）图片，返回可下载的 URL。当用户只想要一张图、不需要跑整条工作流时使用。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"prompt": map[string]interface{}{"type": "string", "description": "画面描述（英文效果更好）"},
					"width":  map[string]interface{}{"type": "integer", "description": "宽，默认 1024"},
					"height": map[string]interface{}{"type": "integer", "description": "高，默认 1024"},
				},
				"required": []string{"prompt"},
			},
		},
	}
}
