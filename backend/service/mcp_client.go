package service

// MCP（Model Context Protocol）客户端。
//
// 为什么引擎里要有客户端
//
//	FlowEdge 之前对 MCP 只有**出**的那一半：把自己包成 MCP 服务器给别人用
//	（backend/mcp/server.go + /api/mcp/config）。但「别人写的 MCP 服务器」进不来——
//	用户机器上早就装了一堆（序策桥、浏览器、数据库…），在 FlowEdge 里却一个都看不见，
//	更别说编进图里。这就是「工具生态只出不进」。
//
//	补上客户端后，MCP 服务器和内置节点是同一等公民：能在工具台里列出、探活、
//	看它有哪些工具，并且用 mcp_call 节点直接调用。
//
// 两种传输（覆盖现有全部实现）
//
//	stdio  按行分隔的 JSON-RPC（MCP 规范：每条消息一行 JSON，没有 LSP 那种 Content-Length 头）。
//	       这是 npx / node / python 起的本地服务器，最常见。
//	http   一次 POST 一个 JSON-RPC 请求，响应可能是 application/json（单对象）
//	       也可能是 text/event-stream（SSE 里带一条 data: 消息）。两种都收。
//
// 纪律（对齐 AGENTS §0.6）
//
//	「连不上」与「连上但没结果」必须分开报：前者是服务器没起/命令不对，
//	后者是它真的没有工具。两者混成空列表，用户会以为是"这服务器没用"。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// mcpProtocolVersion 握手声明的协议版本。
// 用 2024-11-05（最广兼容）而不是最新：老服务器收到不认识的版本会直接断开。
const mcpProtocolVersion = "2024-11-05"

// MCPTool 服务器暴露的一个工具
type MCPTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// MCPCallResult 一次工具调用的归一化结果
type MCPCallResult struct {
	// Text 把 content 里的文本块拼起来，给下游节点当 text 用
	Text string `json:"text"`
	// IsError 服务器自己说这次调用失败了（协议层成功、业务层失败）
	IsError bool `json:"is_error"`
	// Content 原始 content 数组（图/音频块也在里面）
	Content []map[string]any `json:"content,omitempty"`
	// Raw 完整 result，调试时看这个
	Raw       json.RawMessage `json:"raw,omitempty"`
	ElapsedMS int64           `json:"elapsed_ms"`
}

// MCPServerSpec 一个 MCP 服务器的连接参数（与 mcp.json 的字段一一对应）
type MCPServerSpec struct {
	Name    string            `json:"name"`
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Transport 归一化传输类型。配置文件里 type 的花样很多，这里只认两种结果。
func (s MCPServerSpec) Transport() string {
	t := strings.ToLower(strings.TrimSpace(s.Type))
	switch t {
	case "http", "streamable-http", "streamable_http", "streamablehttp", "sse":
		return "http"
	case "stdio", "":
		if strings.TrimSpace(s.URL) != "" && strings.TrimSpace(s.Command) == "" {
			return "http"
		}
		return "stdio"
	}
	return t
}

// Validate 连接参数是否齐备。返回的错误直接给人看。
func (s MCPServerSpec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("MCP 服务器缺 name")
	}
	switch s.Transport() {
	case "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("stdio 型服务器 %s 缺 command", s.Name)
		}
	case "http":
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("http 型服务器 %s 缺 url", s.Name)
		}
	default:
		return fmt.Errorf("服务器 %s 的 type=%q 不支持（应为 stdio / http / sse）", s.Name, s.Type)
	}
	return nil
}

// ===== 会话 =====

// mcpSession 一次连接的生命周期：握手 → 若干次 call → 关闭。
//
// 不做长期连接池：MCP 服务器的进程由它自己管（可能几十个），
// 常驻会把用户机器的进程数推上去；而一次图运行里同一服务器的调用是串行的，
// 每次都重握手只多花几百毫秒，换来的是"不会漏杀、不会泄漏"。
type mcpSession struct {
	spec MCPServerSpec

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	msgs   chan json.RawMessage
	readTo time.Duration
	errBuf *ringBuffer

	httpClient *http.Client
	sessionID  string

	mu     sync.Mutex
	nextID int
	closed bool
	// initResult 服务器在 initialize 里自报的家门，面板用来显示"这是什么"
	initResult map[string]any
}

// ringBuffer 只保留最后 N 字节的 buffer（stderr 用）。
// 服务器可能一直往 stderr 刷日志，不设上限会吃掉内存。
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newRingBuffer(max int) *ringBuffer { return &ringBuffer{max: max} }

func (b *ringBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *ringBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// openMCPSession 建连（不握手，握手见 initialize）
func openMCPSession(ctx context.Context, spec MCPServerSpec) (*mcpSession, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	s := &mcpSession{spec: spec, errBuf: newRingBuffer(8 << 10)}

	switch spec.Transport() {
	case "stdio":
		cmd := exec.Command(spec.Command, spec.Args...)
		cmd.SysProcAttr = hiddenWindowAttr() // 桌面端双击启动时不许闪黑框
		cmd.Env = mcpChildEnv(spec.Env)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("建 stdin 管道失败：%w", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("建 stdout 管道失败：%w", err)
		}
		cmd.Stderr = s.errBuf
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("启动 %q 失败：%w（命令不在 PATH 里？）", spec.Command, err)
		}
		s.cmd = cmd
		s.stdin = stdin
		s.msgs = make(chan json.RawMessage, 32)
		go s.readLoop(stdout)
	case "http":
		s.httpClient = &http.Client{Timeout: 60 * time.Second}
	default:
		return nil, fmt.Errorf("不支持的传输：%s", spec.Transport())
	}
	return s, nil
}

// mcpChildEnv 子进程环境。
//
// 白名单 + 显式声明，**不透传代理**（本机常驻 http_proxy，透传会让服务器
// 所有网络请求走代理，表现为"莫名其妙连不上"，见 agent_exec.go 同一处教训）。
func mcpChildEnv(extra map[string]string) []string {
	keep := map[string]bool{
		"PATH": true, "PATHEXT": true, "COMSPEC": true, "SYSTEMROOT": true, "WINDIR": true,
		"TEMP": true, "TMP": true, "HOME": true, "USERPROFILE": true,
		"APPDATA": true, "LOCALAPPDATA": true, "PROGRAMFILES": true,
		"NODE_PATH": true, "PYTHONPATH": true, "LANG": true, "LC_ALL": true,
		"USERNAME": true, "USERDOMAIN": true, "PROCESSOR_ARCHITECTURE": true,
		"NUMBER_OF_PROCESSORS": true, "SYSTEMDRIVE": true, "PROGRAMDATA": true,
		"PROGRAMFILES(X86)": true, "COMMONPROGRAMFILES": true, "ALLUSERSPROFILE": true,
	}
	out := make([]string, 0, len(keep)+len(extra))
	for _, kv := range os.Environ() {
		k := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			k = kv[:i]
		}
		if keep[strings.ToUpper(k)] {
			out = append(out, kv)
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// readLoop 持续读 stdout，按行切 JSON 投进 msgs。EOF 时关闭通道。
func (s *mcpSession) readLoop(stdout io.Reader) {
	defer close(s.msgs)
	br := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			// 空行与明显的日志行（不以 { 开头）直接丢：有些服务器会把
			// 自己的启动横幅打到 stdout，严格解析会把整条连接判成坏的。
			if len(trimmed) > 0 && trimmed[0] == '{' {
				cp := make([]byte, len(trimmed))
				copy(cp, trimmed)
				s.msgs <- json.RawMessage(cp)
			}
		}
		if err != nil {
			return
		}
	}
}

// write 发一条 JSON-RPC 消息（stdio）
func (s *mcpSession) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.stdin.Write(b)
	return err
}

// call 发请求并等对应 id 的响应。
//
// 为什么不复用同一个读 goroutine 做分发：调用是串行的（同一会话不会并发），
// 拿到 id 不匹配的消息（服务器通知 / 上一个请求的迟到响应）就跳过即可。
func (s *mcpSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}

	if s.spec.Transport() == "http" {
		return s.callHTTP(ctx, req, id)
	}

	if err := s.write(req); err != nil {
		return nil, fmt.Errorf("写入失败：%w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s 超时（%s）：%s", method, s.describeTimeout(), s.stderrHint())
		case raw, ok := <-s.msgs:
			if !ok {
				return nil, fmt.Errorf("服务器提前退出（%s）：%s", method, s.stderrHint())
			}
			var env struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				continue // 不是 JSON-RPC（日志/杂音）
			}
			if env.ID == nil || *env.ID != id {
				continue // 通知或其它请求的响应
			}
			if env.Error != nil {
				return nil, fmt.Errorf("%s 返回错误 %d：%s", method, env.Error.Code, env.Error.Message)
			}
			if len(env.Result) == 0 {
				return json.RawMessage("null"), nil
			}
			return env.Result, nil
		}
	}
}

// notify 发通知（无 id、不等响应）
func (s *mcpSession) notify(method string, params any) error {
	if s.spec.Transport() == "http" {
		// HTTP 传输的通知用同一条 POST，服务器回 202 即可
		req := map[string]any{"jsonrpc": "2.0", "method": method}
		if params != nil {
			req["params"] = params
		}
		b, _ := json.Marshal(req)
		r, err := http.NewRequest(http.MethodPost, s.spec.URL, bytes.NewReader(b))
		if err != nil {
			return err
		}
		r.Header.Set("Content-Type", "application/json")
		resp, err := s.httpClient.Do(r)
		if err != nil {
			return nil // 通知失败不影响主流程
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return nil
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return s.write(msg)
}

// callHTTP 一次 POST。响应支持三种形态：
//   - application/json：单个 JSON-RPC 对象
//   - text/event-stream：SSE，逐行 data: 里找匹配 id 的那条
//   - 202 / 空体：通知已接受（调用方不会走到这里）
func (s *mcpSession) callHTTP(ctx context.Context, req map[string]any, id int) (json.RawMessage, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, s.spec.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range s.spec.Headers {
		hr.Header.Set(k, v)
	}
	if s.sessionID != "" {
		hr.Header.Set("Mcp-Session-Id", s.sessionID)
	}
	resp, err := s.httpClient.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("连不上 %s：%w", s.spec.URL, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		s.sessionID = sid
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d：%.300s", resp.StatusCode, string(raw))
	}

	parse := func(b []byte) (json.RawMessage, bool, error) {
		var env struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(bytes.TrimSpace(b), &env) != nil {
			return nil, false, nil
		}
		if env.ID == nil || *env.ID != id {
			return nil, false, nil
		}
		if env.Error != nil {
			return nil, true, fmt.Errorf("返回错误 %d：%s", env.Error.Code, env.Error.Message)
		}
		if len(env.Result) == 0 {
			return json.RawMessage("null"), true, nil
		}
		return env.Result, true, nil
	}

	if res, ok, err := parse(raw); ok || err != nil {
		return res, err
	}
	// SSE：把所有 data: 行拼起来再看
	var data bytes.Buffer
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			data.Write(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
			data.WriteByte('\n')
			if res, ok, err := parse(data.Bytes()); ok || err != nil {
				return res, err
			}
		}
	}
	return nil, fmt.Errorf("响应里没有 id=%d 的 JSON-RPC 结果：%.200s", id, string(raw))
}

func (s *mcpSession) describeTimeout() string {
	if s.readTo > 0 {
		return s.readTo.String()
	}
	return "超时"
}

func (s *mcpSession) stderrHint() string {
	msg := strings.TrimSpace(s.errBuf.String())
	if msg == "" {
		return "（服务器没有输出错误信息）"
	}
	if len(msg) > 500 {
		msg = msg[len(msg)-500:]
	}
	return msg
}

// initialize 握手。服务器在 result 里自报 serverInfo / capabilities。
func (s *mcpSession) initialize(ctx context.Context) error {
	res, err := s.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "FlowEdge", "version": "1.0"},
	})
	if err != nil {
		return err
	}
	var m map[string]any
	if json.Unmarshal(res, &m) == nil {
		s.initResult = m
	}
	// 规范要求握手后立刻发这条通知，否则部分服务器不响应后续请求
	_ = s.notify("notifications/initialized", map[string]any{})
	return nil
}

// close 关连接。stdio 会杀进程树，避免留下孤儿 npx/node。
func (s *mcpSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		killAgentTree(s.cmd.Process.Pid)
	}
}

// ===== 对外 API =====

// MCPServerInfo 探活结果
type MCPServerInfo struct {
	OK        bool           `json:"ok"`
	Detail    string         `json:"detail,omitempty"`
	Server    map[string]any `json:"server,omitempty"`
	Protocol  string         `json:"protocol,omitempty"`
	Tools     []MCPTool      `json:"tools,omitempty"`
	ElapsedMS int64          `json:"elapsed_ms"`
}

// MCPProbe 连上并拿到工具清单 = 真正可用。
//
// 只做 TCP 探测是不够的：本地 stdio 服务器没有端口，而"进程能起但握手失败"
// （缺配置、Node 版本不对）才是最常见的故障。
func MCPProbe(ctx context.Context, spec MCPServerSpec, timeout time.Duration) MCPServerInfo {
	start := time.Now()
	info := MCPServerInfo{}
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sess, err := openMCPSession(cctx, spec)
	if err != nil {
		info.Detail = err.Error()
		info.ElapsedMS = time.Since(start).Milliseconds()
		return info
	}
	sess.readTo = timeout
	defer sess.close()

	if err := sess.initialize(cctx); err != nil {
		info.Detail = err.Error()
		info.ElapsedMS = time.Since(start).Milliseconds()
		return info
	}
	if si, ok := sess.initResult["serverInfo"].(map[string]any); ok {
		info.Server = si
	}
	if pv, ok := sess.initResult["protocolVersion"].(string); ok {
		info.Protocol = pv
	}
	tools, err := sess.listTools(cctx)
	if err != nil {
		// 握手成功但拿不到工具：服务器活着，只是没暴露工具（或没声明 tools 能力）
		info.OK = true
		info.Detail = "已连接，但列不出工具：" + err.Error()
		info.ElapsedMS = time.Since(start).Milliseconds()
		return info
	}
	info.OK = true
	info.Tools = tools
	info.Detail = fmt.Sprintf("已连接 · %d 个工具", len(tools))
	info.ElapsedMS = time.Since(start).Milliseconds()
	return info
}

// listTools 取工具清单（支持 cursor 分页）
func (s *mcpSession) listTools(ctx context.Context) ([]MCPTool, error) {
	var out []MCPTool
	cursor := ""
	for i := 0; i < 20; i++ { // 上限 20 页，防写错的服务器死循环
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := s.call(ctx, "tools/list", params)
		if err != nil {
			return out, err
		}
		var page struct {
			Tools []struct {
				Name        string         `json:"name"`
				Title       string         `json:"title"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return out, fmt.Errorf("tools/list 响应解析失败：%w", err)
		}
		for _, t := range page.Tools {
			out = append(out, MCPTool{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: t.InputSchema})
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// MCPListTools 单独取工具清单（工具台展开某台服务器时用）
func MCPListTools(ctx context.Context, spec MCPServerSpec, timeout time.Duration) ([]MCPTool, error) {
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess, err := openMCPSession(cctx, spec)
	if err != nil {
		return nil, err
	}
	sess.readTo = timeout
	defer sess.close()
	if err := sess.initialize(cctx); err != nil {
		return nil, err
	}
	return sess.listTools(cctx)
}

// MCPCallTool 调用一个工具。
//
// 返回值把 content 里的文本块拼成 Text：MCP 的 content 是数组（可能混着
// 图/音频块），不归一化的话下游每个节点都要自己遍历一遍，二十个节点二十种写法。
func MCPCallTool(ctx context.Context, spec MCPServerSpec, tool string, args map[string]any, timeout time.Duration) (MCPCallResult, error) {
	start := time.Now()
	res := MCPCallResult{}
	if strings.TrimSpace(tool) == "" {
		return res, errors.New("没有指定工具名")
	}
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sess, err := openMCPSession(cctx, spec)
	if err != nil {
		return res, err
	}
	sess.readTo = timeout
	defer sess.close()
	if err := sess.initialize(cctx); err != nil {
		return res, err
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := sess.call(cctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		res.ElapsedMS = time.Since(start).Milliseconds()
		return res, err
	}
	res.Raw = raw
	var payload struct {
		Content []map[string]any `json:"content"`
		IsError bool             `json:"isError"`
	}
	_ = json.Unmarshal(raw, &payload)
	res.Content = payload.Content
	res.IsError = payload.IsError
	var sb strings.Builder
	for _, c := range payload.Content {
		if t, ok := c["text"].(string); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(t)
		}
	}
	if sb.Len() == 0 && len(payload.Content) == 0 {
		// 没有 content 数组：有些服务器直接返回结构化 JSON
		sb.Write(raw)
	}
	res.Text = sb.String()
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}
