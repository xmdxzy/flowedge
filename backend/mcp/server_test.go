package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runOnce 发一条请求，返回解析后的响应
func runOnce(t *testing.T, srv *Server, payload string) map[string]interface{} {
	t.Helper()
	var out bytes.Buffer
	srv.In = strings.NewReader(payload + "\n")
	srv.Out = &out
	if err := srv.Serve(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatalf("no response for %s", payload)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("response not JSON: %s", line)
	}
	return resp
}

// TestInitialize MCP 客户端第一句必须拿到协议版本与 tools 能力
func TestInitialize(t *testing.T) {
	srv := NewServer("http://127.0.0.1:1", "", nil, nil)
	resp := runOnce(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]interface{})
	if result["protocolVersion"] == "" {
		t.Error("protocolVersion missing")
	}
	caps, _ := result["capabilities"].(map[string]interface{})
	if _, ok := caps["tools"]; !ok {
		t.Error("tools capability missing")
	}
	info, _ := result["serverInfo"].(map[string]interface{})
	if info["name"] != "flowedge" {
		t.Errorf("serverInfo.name = %v", info["name"])
	}
}

// TestToolsList 工具必须有 name/description/inputSchema（LLM 靠描述决定调用）
func TestToolsList(t *testing.T) {
	srv := NewServer("http://127.0.0.1:1", "", nil, nil)
	resp := runOnce(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, _ := resp["result"].(map[string]interface{})
	tools, _ := result["tools"].([]interface{})
	if len(tools) < 5 {
		t.Fatalf("expected >=5 tools, got %d", len(tools))
	}
	for _, raw := range tools {
		tool, _ := raw.(map[string]interface{})
		if tool["name"] == "" || tool["description"] == "" {
			t.Errorf("tool missing name/description: %v", tool)
		}
		schema, _ := tool["inputSchema"].(map[string]interface{})
		if schema["type"] != "object" {
			t.Errorf("tool %v inputSchema must be an object schema", tool["name"])
		}
	}
}

// TestUnknownToolIsToolError 未知工具要报工具级错误（isError），
// 不能报协议错误——否则客户端会认为连接坏了。
func TestUnknownToolIsToolError(t *testing.T) {
	srv := NewServer("http://127.0.0.1:1", "", nil, nil)
	resp := runOnce(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if resp["error"] != nil {
		t.Fatalf("should be tool-level error, got protocol error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Errorf("isError should be true: %v", result)
	}
}

// TestEngineUnreachableGivesActionableError 引擎没起时要说「先启动引擎」，
// 而不是抛一个裸的 connection refused。
func TestEngineUnreachableGivesActionableError(t *testing.T) {
	srv := NewServer("http://127.0.0.1:1", "", nil, nil)
	resp := runOnce(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"flowedge_list_workflows","arguments":{}}}`)
	result, _ := resp["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Fatalf("expected error result: %v", result)
	}
	content, _ := result["content"].([]interface{})
	text, _ := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "启动") {
		t.Errorf("error should tell the user to start the engine: %s", text)
	}
}

// TestListWorkflowsForwards 正常路径：转发到引擎并回传结果
func TestListWorkflowsForwards(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workflows" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"name":"demo"}]`))
	}))
	defer up.Close()

	srv := NewServer(up.URL, "secret", nil, nil)
	resp := runOnce(t, srv, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"flowedge_list_workflows","arguments":{}}}`)
	result, _ := resp["result"].(map[string]interface{})
	if result["isError"] != false {
		t.Fatalf("expected success: %v", result)
	}
	content, _ := result["content"].([]interface{})
	text, _ := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "demo") {
		t.Errorf("result should contain workflow name: %s", text)
	}
}

// TestRunWorkflowRequiresArg 缺参数要在工具层明确报错
func TestRunWorkflowRequiresArg(t *testing.T) {
	srv := NewServer("http://127.0.0.1:1", "", nil, nil)
	resp := runOnce(t, srv, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"flowedge_run_workflow","arguments":{}}}`)
	result, _ := resp["result"].(map[string]interface{})
	content, _ := result["content"].([]interface{})
	text, _ := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "workflow_id") {
		t.Errorf("should name the missing param: %s", text)
	}
}

// TestNotificationIgnored notification（无 id）不能产生响应
func TestNotificationIgnored(t *testing.T) {
	var out bytes.Buffer
	srv := NewServer("http://127.0.0.1:1", "", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"), &out)
	if err := srv.Serve(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("notification must not produce a response, got %s", out.String())
	}
}

// TestParseErrorIsProtocolError 非法 JSON 应该是 -32700 协议错误
func TestParseErrorIsProtocolError(t *testing.T) {
	srv := NewServer("http://127.0.0.1:1", "", nil, nil)
	resp := runOnce(t, srv, `{not json}`)
	errObj, _ := resp["error"].(map[string]interface{})
	if errObj == nil {
		t.Fatalf("expected protocol error: %v", resp)
	}
	if code, _ := errObj["code"].(float64); int(code) != -32700 {
		t.Errorf("code = %v, want -32700", errObj["code"])
	}
}
