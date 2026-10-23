package service

// mcp_call 节点：把外部 MCP 服务器的一个工具当成图里的一个节点。
//
// 为什么不能只做「工具台里点一下试跑」
//
//	试跑只能证明"它现在能通"，进不了流水线。真正有用的是把社区服务器提供的
//	工具（浏览器抓取、数据库查询、某平台的 API）当成一个有入有出的积木，
//	跟出图/转写/合成串在同一张图里。所以必须有节点。
//
// 参数契约（对齐 MCP 的 tools/call）
//
//	server  服务器名（来自工具台扫描到的 mcp.json）
//	tool    工具名（tools/list 里的 name）
//	args    参数 JSON 对象；可写 {{text}} 占位，运行时替换成上游文本
//	        留空 = 把上游文本包成 {"text": "..."}（多数工具的最小可用形态）
//
// 失败语义
//
//	连不上 / 工具不存在 → 硬失败（让图停下来，别让它带着空结果往下跑）
//	服务器返回 isError → 默认硬失败，除非节点上勾了「失败也往下传」

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"flowedge/backend/model"
)

// SetToolchain 注入工具链（mcp_call 节点靠它按名字找服务器）
func (e *EngineV2) SetToolchain(tc *Toolchain) { e.toolchain = tc }

// execMCPCallNode 执行 mcp_call
func (e *EngineV2) execMCPCallNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if e.toolchain == nil {
		return nil, fmt.Errorf("引擎没有装配工具链，mcp_call 不可用")
	}
	server := strings.TrimSpace(asString(node.Data["server"]))
	tool := strings.TrimSpace(asString(node.Data["tool"]))
	if server == "" {
		return nil, fmt.Errorf("mcp_call 没指定 server：在工具台里选一台，或直接填服务器名")
	}
	if tool == "" {
		return nil, fmt.Errorf("mcp_call 没指定 tool（服务器 %s 提供哪些工具可在工具台展开查看）", server)
	}

	args, err := mcpArgsFromNode(node, upstream)
	if err != nil {
		return nil, err
	}

	timeout := 180 * time.Second
	if ms, ok := node.Data["timeout_ms"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}

	res, err := e.toolchain.CallMCP(ctx, server, tool, args, timeout)
	if err != nil {
		return nil, fmt.Errorf("调用 %s.%s 失败：%w", server, tool, err)
	}

	allowFailure := false
	if v, ok := node.Data["allow_failure"].(bool); ok {
		allowFailure = v
	}
	if res.IsError && !allowFailure {
		return nil, fmt.Errorf("%s.%s 返回错误：%s", server, tool, truncateRunes(res.Text, 400))
	}

	out := map[string]interface{}{
		"text":       res.Text,
		"is_error":   res.IsError,
		"elapsed_ms": res.ElapsedMS,
		"server":     server,
		"tool":       tool,
	}
	if len(res.Raw) > 0 {
		// raw 给下游 json 端口用；不塞进 text，避免下游把整坨 JSON 当文案
		out["json"] = json.RawMessage(res.Raw)
	}
	return out, nil
}

// mcpArgsFromNode 把节点参数变成 tools/call 的 arguments。
//
// {{text}} 替换的是**上游文本**：绝大多数 MCP 工具吃一个字符串入参，
// 没有这个占位，用户就得在节点里写死内容，等于不能接上游。
func mcpArgsFromNode(node *model.NodeInstance, upstream map[string]interface{}) (map[string]interface{}, error) {
	raw := strings.TrimSpace(asString(node.Data["args"]))
	text := upstreamText(upstream)
	if raw == "" {
		if text == "" {
			return map[string]interface{}{}, nil
		}
		return map[string]interface{}{"text": text}, nil
	}
	replaced := strings.ReplaceAll(raw, "{{text}}", jsonEscapeFragment(text))
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(replaced), &args); err != nil {
		return nil, fmt.Errorf("args 不是合法 JSON 对象：%v（提示：字符串里的换行要转义）", err)
	}
	return args, nil
}

// jsonEscapeFragment 把文本转成可以安全嵌进 JSON 字符串的值（不带外层引号）。
func jsonEscapeFragment(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	// json.Marshal 一定给出带引号的字符串，去掉首尾引号即可
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return ""
}

func asString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
