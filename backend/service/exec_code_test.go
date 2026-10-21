package service

import (
	"context"
	"sort"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// TestNodeRegistry_HasAllBuiltins 注册表必须包含全部内置类型。
//
// 断言改为与 NodeSpecs（元数据单一来源）对账，不再手抄一份类型清单：
// 以前每加一个节点都要来这里补一行，忘补就红——这个测试本该守漂移，
// 结果自己成了另一处需要维护的清单。
func TestNodeRegistry_HasAllBuiltins(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	types := eng.NodeTypes()
	specs := NodeSpecTypes()
	sort.Strings(specs)
	if len(specs) < 28 {
		t.Fatalf("NodeSpecs 只有 %d 类，疑似被误删", len(specs))
	}
	if len(types) != len(specs) {
		t.Fatalf("注册表 %d 类，NodeSpecs %d 类，两者必须一致：%v vs %v", len(types), len(specs), types, specs)
	}
	for i := range specs {
		if types[i] != specs[i] {
			t.Fatalf("index %d 注册表 %q，NodeSpecs %q", i, types[i], specs[i])
		}
	}
}

// TestEngine_RegisterNodeType 注册新类型后引擎无需改一行即可执行
func TestEngine_RegisterNodeType(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	eng.RegisterNodeType("upper_test", func(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
		in := ""
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					in = t
				}
			}
		}
		return map[string]interface{}{"text": strings.ToUpper(in)}, nil
	})

	nodesJSON := []byte(`[
		{"id":"s","type":"start","data":{"prompt":"hi"}},
		{"id":"u","type":"upper_test","data":{}},
		{"id":"e","type":"end","data":{}}
	]`)
	edgesJSON := []byte(`[
		{"id":"e1","source":"s","target":"u"},
		{"id":"e2","source":"u","target":"e"}
	]`)

	states, err := eng.Execute(nodesJSON, edgesJSON)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	for _, n := range states {
		if n.ID == "u" {
			m, _ := n.Output.(map[string]interface{})
			if m["text"] != "HI" {
				t.Fatalf("custom node output = %v", m["text"])
			}
			return
		}
	}
	t.Fatal("custom node u did not produce output")
}

// TestEngine_UnknownNodeTypeFails 未注册类型必须显式失败
func TestEngine_UnknownNodeTypeFails(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	states, err := eng.Execute(
		[]byte(`[{"id":"s","type":"start","data":{}},{"id":"x","type":"no_such_node","data":{}}]`),
		[]byte(`[{"id":"e1","source":"s","target":"x"}]`),
	)
	if err == nil {
		t.Fatal("unknown node type must fail")
	}
	if !strings.Contains(err.Error(), "unknown node type") {
		t.Fatalf("error should mention unknown node type, got: %v", err)
	}
	for _, n := range states {
		if n.ID == "x" && n.Status != "failed" {
			t.Fatalf("node x should be failed, got %s", n.Status)
		}
	}
}

// ===== 代码节点 =====

func codeNode(code string) model.NodeInstance {
	return model.NodeInstance{ID: "c", Type: "code", Data: map[string]interface{}{"code": code}}
}

func runOneCode(t *testing.T, eng *EngineV2, code string, upstream map[string]interface{}) (interface{}, error) {
	t.Helper()
	node := codeNode(code)
	return eng.executeNode(contextBackground(), &node, upstream)
}

func TestCodeNode_StringResult(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	out, err := runOneCode(t, eng, `result = strings.ToUpper(inputText)`, map[string]interface{}{
		"s": map[string]interface{}{"text": "hello flowedge"},
	})
	if err != nil {
		t.Fatalf("code node failed: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("output should be a map, got %T", out)
	}
	if m["text"] != "HELLO FLOWEDGE" {
		t.Fatalf("text = %v", m["text"])
	}
}

func TestCodeNode_StructuredResult(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	code := `
	type row struct { Name string; Score int }
	rows := []row{{"a", 1}, {"b", 2}}
	result = map[string]interface{}{"count": len(rows), "names": []string{rows[0].Name, rows[1].Name}}
`
	out, err := runOneCode(t, eng, code, nil)
	if err != nil {
		t.Fatalf("code node failed: %v", err)
	}
	m := out.(map[string]interface{})
	if m["count"].(int) != 2 {
		t.Fatalf("count = %v", m["count"])
	}
	switch names := m["names"].(type) {
	case []interface{}:
		if names[0].(string) != "a" {
			t.Fatalf("names = %v", names)
		}
	case []string:
		if names[0] != "a" {
			t.Fatalf("names = %v", names)
		}
	default:
		t.Fatalf("unexpected names type %T: %v", names, names)
	}
}

func TestCodeNode_InputJSON(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	// 多前驱时 input 是完整 JSON，代码可按结构读取
	code := `
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		result = "parse error: " + err.Error()
	} else {
		result = fmt.Sprintf("got %d keys", len(v))
	}
`
	up := map[string]interface{}{
		"a": map[string]interface{}{"text": "x"},
		"b": map[string]interface{}{"text": "y"},
	}
	out, err := runOneCode(t, eng, code, up)
	if err != nil {
		t.Fatalf("code node failed: %v", err)
	}
	if m := out.(map[string]interface{}); m["text"] != "got 2 keys" {
		t.Fatalf("text = %v", m["text"])
	}
}

// TestCodeNode_ForbiddenImport 危险 import 必须被静态拒绝
func TestCodeNode_ForbiddenImport(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	for _, bad := range []string{
		`import "os/exec"`,
		`import "net/http"`,
		`import "os"`,
		`import ( "syscall" )`,
	} {
		code := bad + "\nresult = \"should not run\""
		_, err := runOneCode(t, eng, code, nil)
		if err == nil {
			t.Fatalf("forbidden import %q must be rejected", bad)
		}
	}
}

// TestCodeNode_EmptyCodeFails 空代码显式报错而非返回空成功
func TestCodeNode_EmptyCodeFails(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	if _, err := runOneCode(t, eng, "   \n  ", nil); err == nil {
		t.Fatal("empty code must fail")
	}
}

// TestCodeNode_TimeoutTimeoutMs 死循环 + 短超时必须以 timeout 错误码失败
func TestCodeNode_Timeout(t *testing.T) {
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	node := model.NodeInstance{
		ID:   "c",
		Type: "code",
		Data: map[string]interface{}{
			"code":       "for { }",
			"timeout_ms": 150.0,
		},
	}
	_, err := eng.executeNode(contextBackground(), &node, nil)
	if err == nil {
		t.Fatal("infinite loop must be timed out")
	}
	if classifyError(err) != model.ErrorCodeTimeout {
		t.Fatalf("error code = %q, want timeout", classifyError(err))
	}
}

// TestCheckCodeSafety 静态预检
func TestCheckCodeSafety(t *testing.T) {
	if err := checkCodeSafety(`result = strings.ToUpper(inputText)`); err != nil {
		t.Fatalf("safe code must pass: %v", err)
	}
	if err := checkCodeSafety(`import "os/exec"`); err == nil {
		t.Fatal("os/exec must be rejected")
	}
}

func contextBackground() context.Context { return context.Background() }

// TestCheckCodeSafety_IgnoresComments 注释里提到危险包名不能导致代码被拒。
// 2026-10-21 真实事故：插件示例注释写了 `import "os" 会被拒绝`，
// 旧实现用 strings.Contains 匹配，直接把整个插件拦下。
func TestCheckCodeSafety_IgnoresComments(t *testing.T) {
	code := `// 注意：这里不能用 import "os"，也不能 import "net/http"
// 只能写 import "strings" 这样的白名单包
result := strings.ToUpper("ok")
_ = result`
	if err := checkCodeSafety(code); err != nil {
		t.Fatalf("comments must not trigger the blocklist: %v", err)
	}
	// 真正的 import 仍要拦
	if err := checkCodeSafety(`import (
	"fmt"
	"os"
)`); err == nil {
		t.Error("real forbidden import in a block must still be blocked")
	}
	// 子路径也要拦（os/exec 已在黑名单里，这里验证前缀判定）
	if err := checkCodeSafety(`import "os/exec"`); err == nil {
		t.Error("os/exec must be blocked")
	}
	// 允许的包不能误伤
	if err := checkCodeSafety(`import "strings"`); err != nil {
		t.Errorf("strings must be allowed: %v", err)
	}
}

// TestImportedPackages 解析器本身的行为
func TestImportedPackages(t *testing.T) {
	code := `package x
import "fmt"
import alias "encoding/json"
import (
	"strings"
	// 注释行不算
	"strconv"
)
func f() { s := "import \"os\""; _ = s }`
	got := importedPackages(code)
	want := map[string]bool{"fmt": true, "encoding/json": true, "strings": true, "strconv": true}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected package parsed: %q (all=%v)", p, got)
		}
	}
	if len(got) < 4 {
		t.Errorf("expected 4 packages, got %v", got)
	}
}
