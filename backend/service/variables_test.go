package service

import (
	"encoding/json"
	"testing"
)

// TestVariables_ArrayIndex 数组索引引用：{{node.sources[0].title}}
func TestVariables_ArrayIndex(t *testing.T) {
	out := map[string]interface{}{
		"sources": []interface{}{
			map[string]interface{}{"title": "第一份"},
			map[string]interface{}{"title": "第二份"},
		},
		"matrix": []interface{}{
			[]interface{}{1.0, 2.0},
			[]interface{}{3.0, 4.0},
		},
		"tags": []interface{}{"a", "b"},
	}
	ctx := map[string]interface{}{"rag-1": out}

	cases := []struct {
		in   string
		want string
	}{
		{"{{rag-1.sources[0].title}}", "第一份"},
		{"{{rag-1.sources[1].title}}", "第二份"},
		{"{{rag-1.matrix[1][0]}}", "3"},
		{"{{rag-1.tags[0]}}", "a"},
		{"编号 {{rag-1.tags[1]}}", "编号 b"},
	}
	for _, c := range cases {
		if got := resolveString(c.in, ctx); got != c.want {
			t.Fatalf("resolveString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestVariables_ArrayIndexOutOfRange 越界索引必须保留原文，不能静默变空串
func TestVariables_ArrayIndexOutOfRange(t *testing.T) {
	ctx := map[string]interface{}{
		"n": map[string]interface{}{"items": []interface{}{"x"}},
	}
	for _, in := range []string{"{{n.items[5]}}", "{{n.items[-1]}}", "{{n.missing[0]}}", "{{n.items.notafield}}"} {
		if got := resolveString(in, ctx); got != in {
			t.Fatalf("out-of-range/missing %q should stay verbatim, got %q", in, got)
		}
	}
}

// TestVariables_DotPathStillWorks 回归：原有纯点号路径必须行为不变
func TestVariables_DotPathStillWorks(t *testing.T) {
	ctx := map[string]interface{}{
		"a": map[string]interface{}{
			"text":   "hello",
			"nested": map[string]interface{}{"deep": 42.0},
			"code":   200.0,
			"ok":     true,
		},
	}
	cases := []struct{ in, want string }{
		{"{{a.text}}", "hello"},
		{"{{a.nested.deep}}", "42"},
		{"{{a.code}}", "200"},
		{"{{a.ok}}", "true"},
		{"{{input}}", "{{input}}"},         // 无点号占位符原样保留
		{"{{a.missing}}", "{{a.missing}}"}, // 未知字段保留原文
	}
	for _, c := range cases {
		if got := resolveString(c.in, ctx); got != c.want {
			t.Fatalf("resolveString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestJsonPathTokens 路径分词
func TestJsonPathTokens(t *testing.T) {
	toks := jsonPathTokens("a.b[0].c[12]")
	want := []interface{}{"a", "b", 0, "c", 12}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d: %v", len(toks), len(want), toks)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Fatalf("token %d: got %v want %v", i, toks[i], want[i])
		}
	}
}

// TestVariables_ResolveDataDeepCopy 深拷贝不得污染原定义，数组结构也要遍历
func TestVariables_ResolveDataDeepCopy(t *testing.T) {
	data := map[string]interface{}{
		"url":    "http://x/{{p.sources[0]}}",
		"list":   []interface{}{"{{p.text}}", "static"},
		"nested": map[string]interface{}{"k": "{{p.text}}"},
	}
	ctx := map[string]interface{}{
		"p": map[string]interface{}{
			"text":    "T",
			"sources": []interface{}{"SRC"},
		},
	}

	out := resolveData(data, ctx)
	if got := out["url"].(string); got != "http://x/SRC" {
		t.Fatalf("url resolved to %q", got)
	}
	if got := out["list"].([]interface{})[0].(string); got != "T" {
		t.Fatalf("list[0] resolved to %q", got)
	}
	if got := out["nested"].(map[string]interface{})["k"].(string); got != "T" {
		t.Fatalf("nested.k resolved to %q", got)
	}
	// 原数据不被修改
	if data["url"].(string) != "http://x/{{p.sources[0]}}" {
		t.Fatalf("source data was mutated: %q", data["url"])
	}
}

// TestResolveString_JSONOutput 非标量输出序列化为 JSON 字符串
func TestResolveString_JSONOutput(t *testing.T) {
	ctx := map[string]interface{}{
		"n": map[string]interface{}{
			"obj": map[string]interface{}{"a": 1.0},
			"arr": []interface{}{1.0, 2.0},
		},
	}
	if got := resolveString("{{n.arr}}", ctx); got != "[1,2]" {
		t.Fatalf("array should serialize to JSON, got %q", got)
	}
	got := resolveString("{{n.obj}}", ctx)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("object should serialize to JSON, got %q (%v)", got, err)
	}
}
