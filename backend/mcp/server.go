// Package mcp 提供 FlowEdge 的 MCP（Model Context Protocol）服务端。
//
// 定位：一个 MCP 出口，让 Claude Desktop / Cursor / DSH 直接调用本机出片能力，
// 而不是各自维护一套 ComfyUI/TTS 客户端。MCP 客户端是在增长的那一侧。
//
// 架构选择：**瘦客户端 + HTTP 转发**，不在 stdio 进程里初始化引擎。
//
//	理由：MCP 客户端是短生命周期、可能被拉起多次的；把 DB / ComfyUI / Ollama
//	全栈塞进 stdio 进程，会出现两个引擎抢同一张卡的显存与同一个 sqlite 文件。
//
// 工具面（FlowEdge 收敛后只剩 5 个）：
//
//	flowedge_pipelines   ← 我能在本地出什么（流水线目录 + 字段）
//	flowedge_produce     ← 出片（唯一动作）
//	flowedge_run_status  ← 跑完了吗 / 在哪一步 / 产物在哪
//	flowedge_runs        ← 最近跑过什么
//	flowedge_artifacts   ← 产物清单
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
				"version": "2.0.0",
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
	case "flowedge_pipelines":
		body, err := s.request(http.MethodGet, "/api/pipelines", nil)
		if err != nil {
			return nil, err
		}
		return toolText("本机可出的片子（每条流水线的字段与默认值都在 fields 里）：\n" + prettyJSON(body)), nil

	case "flowedge_produce":
		pipeline, _ := p.Arguments["pipeline"].(string)
		if strings.TrimSpace(pipeline) == "" {
			return nil, fmt.Errorf("缺少参数 pipeline（先用 flowedge_pipelines 看可用 id）")
		}
		payload := map[string]interface{}{"pipeline": pipeline}
		if params, ok := p.Arguments["params"]; ok {
			payload["params"] = params
		}
		// 谁在调：调用方自报（workbuddy / dsh / mcp）。不传就记 mcp——
		// "不知道是谁"也要如实写成 mcp，而不是冒充 manual。
		if who, ok := p.Arguments["invoked_by"].(string); ok && who != "" {
			payload["invoked_by"] = who
		} else {
			payload["invoked_by"] = "mcp"
		}
		body, err := s.request(http.MethodPost, "/api/produce", payload)
		if err != nil {
			return nil, err
		}
		return toolText("已提交出片（异步）：\n" + prettyJSON(body) +
			"\n用 flowedge_run_status 查进度；产物在 /api/artifacts。"), nil

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

	case "flowedge_runs":
		limit := int64(20)
		if v, err := argInt(p.Arguments, "limit"); err == nil && v > 0 {
			limit = v
		}
		q := fmt.Sprintf("/api/runs?limit=%d", limit)
		if st, ok := p.Arguments["status"].(string); ok && st != "" {
			q += "&status=" + st
		}
		body, err := s.request(http.MethodGet, q, nil)
		if err != nil {
			return nil, err
		}
		return toolText(prettyJSON(body)), nil

	case "flowedge_artifacts":
		q := "/api/artifacts"
		if kind, ok := p.Arguments["kind"].(string); ok && kind != "" {
			q += "?kind=" + kind
		}
		body, err := s.request(http.MethodGet, q, nil)
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
			"name": "flowedge_pipelines",
			"description": "列出本机 FlowEdge 能出的片子：每条固定流水线的 id、名称、产物类型、" +
				"预计耗时，以及它需要的字段（含默认值与取值范围）。要出片之前先调这个，别猜参数名——" +
				"未知字段会被引擎直接拒绝。",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		map[string]interface{}{
			"name": "flowedge_produce",
			"description": "用本机 GPU/CPU 出片（异步，立即返回 run_id）：出图、出带声音的视频、" +
				"多段短片、图像编辑、文案转配音都走这一条。pipeline 取自 flowedge_pipelines，" +
				"params 只需填想改的字段（其余用默认值）。出片前会自动做依赖预检，缺服务会直接报出来并给出启动方式。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pipeline": map[string]interface{}{"type": "string", "description": "流水线 id，如 image / video / film（先调 flowedge_pipelines）"},
					"params":   map[string]interface{}{"type": "object", "description": "字段值，如 {\"prompt\": \"...\", \"width\": 1024}"},
					"invoked_by": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"workbuddy", "dsh", "mcp", "manual"},
						"description": "谁在调用。请如实填写你自己的名字（workbuddy / dsh），会记进运行记录",
					},
				},
				"required": []string{"pipeline"},
			},
		},
		map[string]interface{}{
			"name":        "flowedge_run_status",
			"description": "查询一次出片的进度、每步状态、生效参数与产物路径。当用户问「跑完了吗」「出到哪一步了」「产出文件在哪」时使用。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"run_id": map[string]interface{}{"type": "integer", "description": "flowedge_produce 返回的 run_id"},
				},
				"required": []string{"run_id"},
			},
		},
		map[string]interface{}{
			"name":        "flowedge_runs",
			"description": "最近跑过什么：按时间倒序列出运行记录（id / 流水线 / 状态 / 参数 / 起止时间）。想知道「上次那条跑成没有」时用。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"limit":  map[string]interface{}{"type": "integer", "description": "条数，默认 20，上限 200"},
					"status": map[string]interface{}{"type": "string", "enum": []string{"running", "success", "failed", "cancelled"}, "description": "只看某种状态"},
				},
			},
		},
		map[string]interface{}{
			"name":        "flowedge_artifacts",
			"description": "列出已产出的图片/视频/音频文件（名字、类型、大小、时间）。要把成品交付给用户时用。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"kind": map[string]interface{}{"type": "string", "enum": []string{"image", "video", "audio"}, "description": "只看某一类产物"},
				},
			},
		},
	}
}
