package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 输入注入的纯函数契约。
//
// 这些断言看着琐碎，但每一条都对应一种"评测结论会失真"的失败：
// 缺 key 变空串 → case 跑了空 prompt 却显示成功；动了 type → 图结构被换掉；
// 嵌套参数没替换 → 一半输入生效一半没生效，比完全不生效更难发现。

func nodesOf(t *testing.T, raw string) []model.NodeInstance {
	t.Helper()
	var ns []model.NodeInstance
	if err := json.Unmarshal([]byte(raw), &ns); err != nil {
		t.Fatalf("测试数据不是合法 nodes JSON：%v", err)
	}
	return ns
}

func TestRenderNodeVars_Basic(t *testing.T) {
	raw := `[{"id":"s","type":"start","position":{"x":0,"y":0},"data":{"prompt":"画一只{{animal}}"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"animal": "黑猫"})
	ns := nodesOf(t, string(out))
	if got := ns[0].Data["prompt"]; got != "画一只黑猫" {
		t.Fatalf("替换失败：%v", got)
	}
}

// 带空格的写法也要认（人写模板时会自然加空格）
func TestRenderNodeVars_SpacesInsideBraces(t *testing.T) {
	raw := `[{"id":"s","type":"start","data":{"prompt":"主体：{{ subject }}"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"subject": "石灯"})
	if got := nodesOf(t, string(out))[0].Data["prompt"]; got != "主体：石灯" {
		t.Fatalf("带空格的模板没替换：%v", got)
	}
}

// 缺失的 key 必须原样保留。
//
// 替换成空串是最坏的选择：case 会跑出一个"空主体"的图，门照样可能过（因为空提示词也能出图），
// 于是评测得出"这个版本更好"的错误结论 —— 而真实原因是输入没传进去。
func TestRenderNodeVars_MissingKeyKeptLiteral(t *testing.T) {
	raw := `[{"id":"s","type":"start","data":{"prompt":"{{a}} 和 {{b}}"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"a": "甲"})
	got, _ := nodesOf(t, string(out))[0].Data["prompt"].(string)
	if got != "甲 和 {{b}}" {
		t.Fatalf("缺失的 key 应原样保留，实际 %q", got)
	}
	if strings.Contains(got, "{{a}}") {
		t.Fatalf("已提供的 key 应被替换：%q", got)
	}
}

// 只动 data：type / id / position 不是"输入"，改了就是换了一张图
func TestRenderNodeVars_DoesNotTouchStructure(t *testing.T) {
	raw := `[{"id":"{{id}}","type":"{{type}}","position":{"x":10,"y":20},"data":{"prompt":"{{id}}"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"id": "start", "type": "llm"})
	ns := nodesOf(t, string(out))
	if ns[0].ID != "{{id}}" {
		t.Fatalf("节点 id 不该被替换，实际 %q", ns[0].ID)
	}
	if ns[0].Type != "{{type}}" {
		t.Fatalf("节点 type 不该被替换，实际 %q", ns[0].Type)
	}
	if ns[0].Data["prompt"] != "start" {
		t.Fatalf("data 里的同名占位应被替换，实际 %v", ns[0].Data["prompt"])
	}
}

// 嵌套结构也要替换（data 里常有 {"items":[...]} / {"opts":{...}}）
func TestRenderNodeVars_NestedValues(t *testing.T) {
	raw := `[{"id":"n","type":"text_split","data":{"mode":"by_count","options":{"title":"{{title}}"},"items":["{{a}}","x"]}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"title": "第一幕", "a": "甲"})
	d := nodesOf(t, string(out))[0].Data
	if opts, ok := d["options"].(map[string]interface{}); !ok || opts["title"] != "第一幕" {
		t.Fatalf("嵌套 map 没替换：%v", d["options"])
	}
	items, ok := d["items"].([]interface{})
	if !ok || items[0] != "甲" || items[1] != "x" {
		t.Fatalf("数组元素没替换：%v", d["items"])
	}
}

// 没给 vars 时零改动（返回原串，且不是"内容相同"就算 —— 要的是完全一致）
func TestRenderNodeVars_NoVarsNoChange(t *testing.T) {
	raw := `[{"id":"s","type":"start","data":{"prompt":"{{x}}"}}]`
	out := renderNodeVars([]byte(raw), nil)
	if string(out) != raw {
		t.Fatalf("没有 vars 时应原样返回：\n%s\n%s", out, raw)
	}
	out2 := renderNodeVars([]byte(raw), map[string]interface{}{})
	if string(out2) != raw {
		t.Fatal("空 vars 也应原样返回")
	}
}

// 没有任何占位的图：内容不变（不因重排 JSON 而产生假 diff）
func TestRenderNodeVars_NoPlaceholderNoRewrite(t *testing.T) {
	raw := `[{"id":"s","type":"start","data":{"prompt":"普通文本"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"a": "甲"})
	if string(out) != raw {
		t.Fatalf("没有占位时不该重写 JSON：%s", out)
	}
}

// 坏 JSON 原样返回：让下游的解析错误去报真实原因，不在这里吞掉
func TestRenderNodeVars_BadJSONPassThrough(t *testing.T) {
	raw := []byte(`{not json`)
	out := renderNodeVars(raw, map[string]interface{}{"a": "甲"})
	if string(out) != string(raw) {
		t.Fatalf("坏 JSON 应原样返回：%s", out)
	}
}

// 数字/布尔参数不该被当成字符串处理（类型要保持）
func TestRenderNodeVars_KeepsNonStringTypes(t *testing.T) {
	raw := `[{"id":"n","type":"text2image","data":{"width":768,"flag":true,"prompt":"{{p}}"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"p": "夜雨"})
	d := nodesOf(t, string(out))[0].Data
	if _, ok := d["width"].(float64); !ok {
		t.Fatalf("数字被改了类型：%T", d["width"])
	}
	if _, ok := d["flag"].(bool); !ok {
		t.Fatalf("布尔被改了类型：%T", d["flag"])
	}
}

// 替换值本身含有 {{...}} 时不递归展开（只替换一轮）
//
// 递归会让"用户文本里恰好写了 {{x}}"变成注入点 —— 评测输入来自外部数据，
// 这条边界必须守住。
func TestRenderNodeVars_NoRecursiveExpansion(t *testing.T) {
	raw := `[{"id":"s","type":"start","data":{"prompt":"{{a}}"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"a": "{{b}}", "b": "不该出现"})
	got, _ := nodesOf(t, string(out))[0].Data["prompt"].(string)
	if got != "{{b}}" {
		t.Fatalf("不该递归展开，实际 %q", got)
	}
}

// MCP 的 vars 声明是 {"type":"object"}，外部智能体最自然会写 {"n": 2}。
// 这一条盯着"数字别变成 2.000000"—— 它会直接进提示词，人被这种输出弄懵过。
func TestRenderNodeVars_NumbersAndBools(t *testing.T) {
	raw := `[{"id":"s","type":"start","data":{"prompt":"{{topic}} 讲 {{n}} 句，竖屏={{vertical}}"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{
		"topic": "月亮", "n": float64(3), "vertical": true,
	})
	got, _ := nodesOf(t, string(out))[0].Data["prompt"].(string)
	if got != "月亮 讲 3 句，竖屏=true" {
		t.Fatalf("数字/布尔替换不对：%q", got)
	}
}

// null 当"没提供"处理：原样保留占位，而不是替换成 "<nil>"
func TestRenderNodeVars_NullKeepsPlaceholder(t *testing.T) {
	raw := `[{"id":"s","type":"start","data":{"prompt":"{{a}}"}}]`
	out := renderNodeVars([]byte(raw), map[string]interface{}{"a": nil})
	got, _ := nodesOf(t, string(out))[0].Data["prompt"].(string)
	if got != "{{a}}" {
		t.Fatalf("null 不该变成 <nil>：%q", got)
	}
}
