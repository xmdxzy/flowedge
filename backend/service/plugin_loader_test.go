package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlugin(t *testing.T, root, dir, manifest, script string) {
	t.Helper()
	full := filepath.Join(root, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if script != "" {
		if err := os.WriteFile(filepath.Join(full, "main.go"), []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLoadRuntimePlugins_HTTP http 型插件：任何语言写的外部服务都能当节点
func TestLoadRuntimePlugins_HTTP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input  map[string]interface{} `json:"input"`
			Config map[string]interface{} `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		text, _ := req.Input["text"].(string)
		_, _ = w.Write([]byte(`{"text":"` + strings.ToUpper(text) + `"}`))
	}))
	defer up.Close()

	root := t.TempDir()
	writePlugin(t, root, "upper", `{"name":"upper_svc","type":"http","url":"`+up.URL+`","description":"转发到大写服务"}`, "")

	plugins, errs := LoadRuntimePlugins(root)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	out, err := plugins[0].Execute(map[string]interface{}{"text": "hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out["text"] != "HELLO" {
		t.Errorf("text = %v", out["text"])
	}
}

// TestLoadRuntimePlugins_Script script 型插件必须真的能在进程内跑起来（yaegi）。
// 这条不通过就等于「运行期脚本插件」是假的。
func TestLoadRuntimePlugins_Script(t *testing.T) {
	script := `func Execute(inputJSON string, configJSON string) (string, error) {
	var in map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
		return "", err
	}
	text, _ := in["text"].(string)
	return "{\"text\":\"" + strings.ToUpper(text) + "-from-script\"}", nil
}
`
	root := t.TempDir()
	writePlugin(t, root, "shout", `{"name":"shout_script","type":"script","entry":"main.go","description":"脚本插件"}`, script)

	plugins, errs := LoadRuntimePlugins(root)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	out, err := plugins[0].Execute(map[string]interface{}{"text": "abc"}, nil)
	if err != nil {
		t.Fatalf("script plugin execute: %v", err)
	}
	if out["text"] != "ABC-from-script" {
		t.Errorf("text = %v", out["text"])
	}
}

// TestLoadRuntimePlugins_BadOneDoesNotBlockOthers 一个坏插件不能影响其他插件加载
func TestLoadRuntimePlugins_BadOneDoesNotBlockOthers(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "badjson", `{not json`, "")
	writePlugin(t, root, "noname", `{"type":"http","url":"http://x"}`, "")
	writePlugin(t, root, "badtype", `{"name":"weird","type":"wasm"}`, "")
	writePlugin(t, root, "good", `{"name":"ok_plugin","type":"http","url":"http://127.0.0.1:1"}`, "")

	plugins, errs := LoadRuntimePlugins(root)
	if len(plugins) != 1 {
		t.Fatalf("good plugin should still load, got %d (errs=%v)", len(plugins), errs)
	}
	if len(errs) != 3 {
		t.Errorf("expected 3 errors, got %d: %v", len(errs), errs)
	}
	if plugins[0].Type() != "ok_plugin" {
		t.Errorf("loaded wrong plugin: %s", plugins[0].Type())
	}
}

// TestLoadRuntimePlugins_RejectsTraversal entry 不能越出插件目录
func TestLoadRuntimePlugins_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "evil", `{"name":"evil","type":"script","entry":"../../etc/passwd"}`, "")
	_, errs := LoadRuntimePlugins(root)
	if len(errs) != 1 || !strings.Contains(errs[0], "entry") {
		t.Errorf("path traversal must be rejected: %v", errs)
	}
}

// TestLoadRuntimePlugins_RejectsBadName name 会成为节点类型，必须是标识符
func TestLoadRuntimePlugins_RejectsBadName(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "x", `{"name":"bad name!","type":"http","url":"http://x"}`, "")
	if _, errs := LoadRuntimePlugins(root); len(errs) != 1 {
		t.Errorf("illegal name must be rejected: %v", errs)
	}
}

// TestLoadRuntimePlugins_MissingDirIsNotAnError 没装插件是正常状态
func TestLoadRuntimePlugins_MissingDirIsNotAnError(t *testing.T) {
	plugins, errs := LoadRuntimePlugins(filepath.Join(t.TempDir(), "nope"))
	if len(plugins) != 0 || len(errs) != 0 {
		t.Errorf("missing dir should be silent: %v %v", plugins, errs)
	}
}

// TestRuntimePlugin_HTTPErrorSurfaces 外部插件返回非 200 时要带状态码与片段
func TestRuntimePlugin_HTTPErrorSurfaces(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom detail"))
	}))
	defer up.Close()

	p := &RuntimePlugin{manifest: pluginManifest{Name: "x", Type: "http", URL: up.URL}}
	_, err := p.Execute(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry status and body: %v", err)
	}
}

// TestRuntimePlugin_NonJSONScriptFallsBackToText 脚本返回非 JSON 时当 text 处理，
// 而不是报错——写插件的人不该被迫学 JSON 输出。
func TestRuntimePlugin_NonJSONScriptFallsBackToText(t *testing.T) {
	got, err := decodePluginOutput("x", "plain text")
	if err != nil {
		t.Fatal(err)
	}
	if got["text"] != "plain text" {
		t.Errorf("got %v", got)
	}
}

// TestExamplePluginsLoad 仓库里的示例插件必须永远能加载。
//
// 理由：示例是文档的一部分，最容易腐化——接口一改，README 还在教旧写法。
// 这条测试把它变成编译期契约。
func TestExamplePluginsLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "plugins")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("examples/plugins not present")
	}
	plugins, errs := LoadRuntimePlugins(dir)
	if len(errs) != 0 {
		t.Fatalf("example plugins must load cleanly: %v", errs)
	}
	if len(plugins) == 0 {
		t.Fatal("no example plugins found")
	}
	for _, p := range plugins {
		out, err := p.Execute(map[string]interface{}{"text": "第一行\n第二行"}, nil)
		if err != nil {
			t.Fatalf("plugin %s execute: %v", p.Type(), err)
		}
		if out == nil {
			t.Errorf("plugin %s returned nil output", p.Type())
		}
	}
}
