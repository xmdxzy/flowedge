package service

// 工具链与 MCP 客户端的单测。
//
// 覆盖三件最容易悄悄坏掉的事：
//  1. 多来源配置的去重与启停覆盖（写错就会"改了没反应"）
//  2. 插件热注册（写错就会"重启才生效"）
//  3. MCP 的两种 HTTP 响应形态（JSON / SSE，只认一种就会漏掉一半服务器）

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"flowedge/backend/model"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mcpViewByID(t *testing.T, v ToolchainView, id string) ToolEntry {
	t.Helper()
	for _, e := range v.Entries {
		if e.Kind == "mcp" && e.ID == id {
			return e
		}
	}
	t.Fatalf("清单里没有 MCP 服务器 %s（共 %d 条）", id, len(v.Entries))
	return ToolEntry{}
}

// 三个服务器：stdio / http / 自带 enabled:false，外加一次停用覆盖。
func TestToolchain_MCPDiscoveryAndToggle(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "data", "mcp.json"), `{
  "mcpServers": {
    "alpha": {"type": "stdio", "command": "node", "args": ["x.mjs"]},
    "beta":  {"type": "http", "url": "http://127.0.0.1:9/mcp"},
    "zeta":  {"type": "stdio", "command": "node", "enabled": false}
  }
}`)

	tc := NewToolchain(dir, nil, nil)
	v := tc.Snapshot(context.Background(), false)

	alpha := mcpViewByID(t, v, "alpha")
	if !alpha.Enabled || !alpha.Managed {
		t.Fatalf("自有服务器应默认启用且可写：%+v", alpha)
	}
	if got := alpha.Meta["transport"]; got != "stdio" {
		t.Fatalf("alpha 应识别为 stdio，得到 %v", got)
	}
	beta := mcpViewByID(t, v, "beta")
	if got := beta.Meta["transport"]; got != "http" {
		t.Fatalf("beta 应识别为 http，得到 %v", got)
	}
	if z := mcpViewByID(t, v, "zeta"); z.Enabled {
		t.Fatal("enabled:false 的服务器不该算启用")
	}

	// 停用自有服务器：写回 data/mcp.json 的 enabled 字段
	if err := SetMCPEnabled(dir, "alpha", false); err != nil {
		t.Fatal(err)
	}
	if a := mcpViewByID(t, NewToolchain(dir, nil, nil).Snapshot(context.Background(), false), "alpha"); a.Enabled {
		t.Fatal("停用没落到磁盘")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "data", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.MCPServers["alpha"]["enabled"] != false {
		t.Fatalf("mcp.json 里的 enabled 没写进去：%v", f.MCPServers["alpha"])
	}
}

// 外部服务器：启停只能写覆盖层，原文件一个字节都不许动。
func TestToolchain_ExternalMCPToggleDoesNotTouchSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	extPath := filepath.Join(home, ".workbuddy", "mcp.json")
	original := `{"mcpServers":{"ext":{"type":"stdio","command":"node","args":["s.mjs"]}}}`
	writeFile(t, extPath, original)

	dir := t.TempDir()
	tc := NewToolchain(dir, nil, nil)
	e := mcpViewByID(t, tc.Snapshot(context.Background(), false), "ext")
	if e.Managed {
		t.Fatal("外部配置不该标成可写")
	}

	if err := SetMCPEnabled(dir, "ext", false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("外部配置文件被改了：%s", after)
	}
	if e := mcpViewByID(t, NewToolchain(dir, nil, nil).Snapshot(context.Background(), false), "ext"); e.Enabled {
		t.Fatal("覆盖层没生效")
	}
	// 删除外部条目必须被拒绝，并说清该去哪删
	if err := DeleteMCPServer(dir, "ext"); err == nil {
		t.Fatal("删除外部服务器应被拒绝")
	}
}

// 插件热注册：加一个插件 → Reload → 节点类型出现，且能执行。
func TestToolchain_ReloadRegistersPluginNodes(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "data", "plugins", "shout")
	writeFile(t, filepath.Join(pdir, "plugin.json"), `{
  "name": "shout", "title": "喊话", "type": "script", "entry": "main.go"
}`)
	// 脚本插件按约定**不写 package / import**：加载器会前置包与白名单导入，
	// 自带 package 会撞成重复声明（真实插件也是这个写法）
	writeFile(t, filepath.Join(pdir, "main.go"), `func Execute(inputJSON string, configJSON string) (string, error) {
	return "{\"text\":\"ok\"}", nil
}
`)

	eng := &EngineV2{}
	eng.registerBuiltinNodes()
	tc := NewToolchain(dir, eng, nil)

	res := tc.Reload()
	if res.Plugins != 1 {
		t.Fatalf("应加载 1 个插件，得到 %d（问题：%v）", res.Plugins, res.Problems)
	}
	found := false
	for _, typ := range res.NodeTypes {
		if typ == "shout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("节点类型没热注册上：%v", res.NodeTypes)
	}

	fn, ok := eng.lookupNode("shout")
	if !ok {
		t.Fatal("lookupNode 查不到热注册节点")
	}
	node := &model.NodeInstance{ID: "p1", Type: "shout", Data: map[string]interface{}{}}
	out, err := fn(context.Background(), node, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := out.(map[string]interface{})
	if m["text"] != "ok" {
		t.Fatalf("插件执行结果不对：%v", out)
	}

	// 停用后必须消失：节点表是"整体替换"语义
	writeFile(t, filepath.Join(pdir, "plugin.json"), `{
  "name": "shout", "title": "喊话", "type": "script", "entry": "main.go", "enabled": false
}`)
	if res := tc.Reload(); res.Plugins != 0 {
		t.Fatalf("停用的插件不该再加载，得到 %d", res.Plugins)
	}
	if _, ok := eng.lookupNode("shout"); ok {
		t.Fatal("停用后节点类型仍在——整体替换语义没生效")
	}
}

// HTTP 传输：JSON 与 SSE 两种响应都要认。
func TestMCPHTTPTransport(t *testing.T) {
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": mcpProtocolVersion, "serverInfo": map[string]any{"name": "fake"}},
			})
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "echo", "description": "回显", "inputSchema": map[string]any{"type": "object"}},
				}},
			})
		case "tools/call":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "hello"}}},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer jsonSrv.Close()

	sseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID int `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"tools": []map[string]any{{"name": "sse_only"}}},
		})
		w.Write([]byte("event: message\ndata: " + string(payload) + "\n\n"))
	}))
	defer sseSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := MCPListTools(ctx, MCPServerSpec{Name: "fake", Type: "http", URL: jsonSrv.URL}, 5*time.Second)
	if err != nil {
		t.Fatalf("JSON 传输失败：%v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("工具清单不对：%+v", tools)
	}

	res, err := MCPCallTool(ctx, MCPServerSpec{Name: "fake", Type: "http", URL: jsonSrv.URL}, "echo", map[string]any{"text": "x"}, 5*time.Second)
	if err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if res.Text != "hello" {
		t.Fatalf("文本块没拼出来：%q", res.Text)
	}

	sseTools, err := MCPListTools(ctx, MCPServerSpec{Name: "sse", Type: "http", URL: sseSrv.URL}, 5*time.Second)
	if err != nil {
		t.Fatalf("SSE 传输失败：%v", err)
	}
	if len(sseTools) != 1 || sseTools[0].Name != "sse_only" {
		t.Fatalf("SSE 工具清单不对：%+v", sseTools)
	}
}

// mcp_call 的入参组装：{{text}} 替换、留空回落、非法 JSON 报错。
func TestMCPArgsFromNode(t *testing.T) {
	node := &model.NodeInstance{ID: "m1", Type: "mcp_call", Data: map[string]interface{}{}}
	up := map[string]interface{}{"text": "多行\n\"引号\""}

	args, err := mcpArgsFromNode(node, up)
	if err != nil {
		t.Fatal(err)
	}
	if args["text"] != "多行\n\"引号\"" {
		t.Fatalf("留空时应把上游文本包成 text：%v", args)
	}

	node.Data["args"] = `{"query": "{{text}}", "limit": 3}`
	args, err = mcpArgsFromNode(node, up)
	if err != nil {
		t.Fatal(err)
	}
	if args["query"] != "多行\n\"引号\"" {
		t.Fatalf("{{text}} 替换没处理转义：%v", args["query"])
	}
	if args["limit"] != float64(3) {
		t.Fatalf("其它字段被破坏：%v", args)
	}

	node.Data["args"] = `{not json}`
	if _, err := mcpArgsFromNode(node, up); err == nil {
		t.Fatal("非法 JSON 应该报错")
	}
}

func TestMCPServerSpecTransport(t *testing.T) {
	cases := []struct {
		spec MCPServerSpec
		want string
	}{
		{MCPServerSpec{Type: "stdio", Command: "node"}, "stdio"},
		{MCPServerSpec{Command: "node"}, "stdio"},
		{MCPServerSpec{URL: "http://x/mcp"}, "http"},
		{MCPServerSpec{Type: "streamable-http", URL: "http://x"}, "http"},
		{MCPServerSpec{Type: "sse", URL: "http://x"}, "http"},
	}
	for _, c := range cases {
		if got := c.spec.Transport(); got != c.want {
			t.Fatalf("%+v 应为 %s，得到 %s", c.spec, c.want, got)
		}
	}
	if err := (MCPServerSpec{Name: "x", Type: "stdio"}).Validate(); err == nil {
		t.Fatal("stdio 缺 command 应该校验失败")
	}
	if err := (MCPServerSpec{Name: "x", Type: "http"}).Validate(); err == nil {
		t.Fatal("http 缺 url 应该校验失败")
	}
}
