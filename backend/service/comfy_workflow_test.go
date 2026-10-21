package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flowedge/backend/model"
)

const apiFormatWorkflow = `{
  "3": {"class_type": "KSampler", "inputs": {"seed": 1, "steps": 20, "cfg": 7.5, "denoise": 1.0}},
  "4": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "sdxl.safetensors"}},
  "6": {"class_type": "CLIPTextEncode", "inputs": {"text": "original prompt"}},
  "9": {"class_type": "SaveImage", "inputs": {"images": ["8", 0], "filename_prefix": "x"}}
}`

// TestParseComfyWorkflow_RejectsUIFormat UI 格式必须被明确拒绝并给出正确动作，
// 而不是产生一张"看着能跑"的错图。
func TestParseComfyWorkflow_RejectsUIFormat(t *testing.T) {
	ui := `{"nodes":[{"id":1,"type":"KSampler"}],"links":[]}`
	_, err := ParseComfyWorkflow([]byte(ui))
	if err == nil {
		t.Fatal("UI format must be rejected")
	}
	if !strings.Contains(err.Error(), "Export (API)") {
		t.Errorf("error must tell the user how to export: %s", err)
	}
}

// TestParseComfyWorkflow_StripsNonNodeKeys API 格式里常混进 extra/version，
// 应该剔除而不是当成节点或报错。
func TestParseComfyWorkflow_StripsNonNodeKeys(t *testing.T) {
	raw := `{"1":{"class_type":"KSampler","inputs":{}},"extra":{"ds":{"scale":1}},"client_id":"abc","version":0.4}`
	graph, err := ParseComfyWorkflow([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(graph) != 1 {
		t.Fatalf("expected only the real node, got %d: %v", len(graph), graph)
	}
	if _, ok := graph["1"]; !ok {
		t.Error("node 1 missing")
	}
}

// TestParseComfyWorkflow_EmptyGraph 没有节点时必须报清楚
func TestParseComfyWorkflow_EmptyGraph(t *testing.T) {
	if _, err := ParseComfyWorkflow([]byte(`{"extra":{}}`)); err == nil {
		t.Fatal("empty graph must error")
	}
	if _, err := ParseComfyWorkflow([]byte(`not json`)); err == nil {
		t.Fatal("broken json must error")
	}
}

// TestApplyBindings_CoercesNumeric 数值字段必须转成 number——
// ComfyUI 拿到字符串 "1024" 会 400 validation failed。
func TestApplyBindings_CoercesNumeric(t *testing.T) {
	graph, err := ParseComfyWorkflow([]byte(apiFormatWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyBindings(graph, map[string]string{
		"6.text":  "{{prompt}}",
		"3.steps": "12",
		"3.cfg":   "4.5",
	}, map[string]string{"prompt": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 3 {
		t.Errorf("applied = %v", applied)
	}
	inputs := graph["3"].(map[string]interface{})["inputs"].(map[string]interface{})
	if v, ok := inputs["steps"].(int64); !ok || v != 12 {
		t.Errorf("steps should be int64(12), got %T %v", inputs["steps"], inputs["steps"])
	}
	if v, ok := inputs["cfg"].(float64); !ok || v != 4.5 {
		t.Errorf("cfg should be float64(4.5), got %T %v", inputs["cfg"], inputs["cfg"])
	}
	text := graph["6"].(map[string]interface{})["inputs"].(map[string]interface{})["text"]
	if text != "a cat" {
		t.Errorf("text = %v", text)
	}
}

// TestApplyBindings_RejectsBadTargets 错目标必须报出来，
// 静默跳过会让用户以为绑定生效了。
func TestApplyBindings_RejectsBadTargets(t *testing.T) {
	graph, _ := ParseComfyWorkflow([]byte(apiFormatWorkflow))
	cases := []struct{ binding, want string }{
		{"999.text", "不存在"},
		{"6", "格式应为"},
		{"6.class_type", "class_type"},
		{"6.text", "没有取值"},
	}
	for _, c := range cases {
		vars := map[string]string{}
		if c.want == "没有取值" {
			// 变量缺失场景：binding 引用了不存在的 {{prompt}}
			_, err := ApplyBindings(graph, map[string]string{c.binding: "{{prompt}}"}, vars)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("binding %q: err = %v, want contains %q", c.binding, err, c.want)
			}
			continue
		}
		_, err := ApplyBindings(graph, map[string]string{c.binding: "x"}, vars)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("binding %q: err = %v, want contains %q", c.binding, err, c.want)
		}
	}
}

// TestLoadComfyWorkflowFile_RejectsPathTraversal 文件名不能当路径用
func TestLoadComfyWorkflowFile_RejectsPathTraversal(t *testing.T) {
	for _, bad := range []string{"../secret.json", "sub/x.json", "..\\x.json"} {
		if _, err := LoadComfyWorkflowFile(bad); err == nil {
			t.Errorf("path traversal must be rejected: %s", bad)
		}
	}
}

// TestCollectWorkflowVars 上游输出必须同时以 nodeId.field 与裸字段名进表
func TestCollectWorkflowVars(t *testing.T) {
	node := &model.NodeInstance{Data: map[string]interface{}{
		"vars": map[string]interface{}{"style": "cinematic"},
	}}
	upstream := map[string]interface{}{
		"7": map[string]interface{}{"text": "上游提示词", "count": float64(3)},
	}
	vars := collectWorkflowVars(node, upstream)
	if vars["7.text"] != "上游提示词" || vars["text"] != "上游提示词" {
		t.Errorf("text vars missing: %v", vars)
	}
	if vars["7.count"] != "3" || vars["count"] != "3" {
		t.Errorf("numeric var should be trimmed: %v", vars)
	}
	if vars["style"] != "cinematic" {
		t.Errorf("static vars missing: %v", vars)
	}
}

// TestExecComfyWorkflowNode_MissingConfig 没有 workflow 时必须给出可执行提示
func TestExecComfyWorkflowNode_MissingConfig(t *testing.T) {
	e := &EngineV2{t2i: &Text2ImageService{BaseURL: "http://127.0.0.1:1"}}
	_, err := e.execComfyWorkflowNode(&model.NodeInstance{Data: map[string]interface{}{}}, nil)
	if err == nil {
		t.Fatal("expected error for missing workflow")
	}
	if !strings.Contains(err.Error(), "(API)") {
		t.Errorf("error should mention API export: %s", err)
	}
	// 连 t2i 都没有时也要说清楚
	e2 := &EngineV2{}
	if _, err := e2.execComfyWorkflowNode(&model.NodeInstance{Data: map[string]interface{}{}}, nil); err == nil {
		t.Error("expected error when comfyui not configured")
	}
}

// TestApplyBindings_RejectsLinkField 绑定到已有连线的输入必须拒绝。
//
// 覆盖的形态来自真实 API 图：上游连接是 ["6", 0]。无条件覆盖成字符串后，
// ComfyUI 把它当字面量，上游节点变孤儿——图提交成功、输出完全不对。
func TestApplyBindings_RejectsLinkField(t *testing.T) {
	graph := map[string]interface{}{
		"3": map[string]interface{}{
			"class_type": "KSampler",
			"inputs": map[string]interface{}{
				"model": []interface{}{"4", float64(0)}, // 连到 checkpoint 的 MODEL 输出
				"steps": float64(20),
			},
		},
	}
	_, err := ApplyBindings(graph, map[string]string{"3.model": "6"}, nil)
	if err == nil {
		t.Fatal("binding a linked input must be rejected")
	}
	if !strings.Contains(err.Error(), "已有连线") {
		t.Errorf("error should explain the link conflict: %v", err)
	}
	// 非链接字段仍然可以绑（数值）
	if _, err := ApplyBindings(graph, map[string]string{"3.steps": "12"}, nil); err != nil {
		t.Errorf("plain field must still be bindable: %v", err)
	}
	// 目标是数组但不是链接形态（如 ["a","b"]）不该误判
	graph2 := map[string]interface{}{
		"5": map[string]interface{}{
			"class_type": "X",
			"inputs":     map[string]interface{}{"tags": []interface{}{"red", "blue"}},
		},
	}
	if _, err := ApplyBindings(graph2, map[string]string{"5.tags": "green"}, nil); err != nil {
		t.Errorf("non-link array must not be treated as a link: %v", err)
	}
}

// TestIsLinkValue 链接判定的边界
func TestIsLinkValue(t *testing.T) {
	cases := []struct {
		v    interface{}
		want bool
	}{
		{[]interface{}{"6", float64(0)}, true},
		{[]interface{}{"6", float64(2)}, true},
		{[]interface{}{"6"}, false},
		{[]interface{}{float64(6), float64(0)}, false},
		{[]interface{}{"", float64(0)}, false},
		{[]interface{}{"a", "b"}, false},
		{"6", false},
		{float64(1), false},
		{nil, false},
	}
	for i, c := range cases {
		if got := isLinkValue(c.v); got != c.want {
			t.Errorf("case %d: isLinkValue(%v) = %v, want %v", i, c.v, got, c.want)
		}
	}
}

// TestParseComfyWorkflow_DetectsSubgraph Blueprint 形态（definitions.subgraphs）
// 的错误信息必须与普通 UI 格式不同——它们的解决办法不一样。
func TestParseComfyWorkflow_DetectsSubgraph(t *testing.T) {
	raw := `{"nodes":[{"id":140,"type":"916dff42-6166-4d45-b028-04eaf69fbb35"}],
		"links":[],"definitions":{"subgraphs":[{"id":"sg1","nodes":[]}]}}`
	_, err := ParseComfyWorkflow([]byte(raw))
	if err == nil {
		t.Fatal("subgraph format must be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "子图") {
		t.Errorf("should name the subgraph problem: %s", msg)
	}
	if !strings.Contains(msg, "展开") {
		t.Errorf("should tell the user to expand subgraphs: %s", msg)
	}
	// 普通 UI 格式的提示里不该出现「子图」
	plain := `{"nodes":[{"id":1,"type":"KSampler"}],"links":[]}`
	_, err2 := ParseComfyWorkflow([]byte(plain))
	if err2 == nil || strings.Contains(err2.Error(), "子图") {
		t.Errorf("plain UI format should get the plain message, got: %v", err2)
	}
}

// TestParseRealBlueprints 用 ComfyUI 官方模板库（本机 89 个真实文件）验证格式检测。
//
// 这条测试的价值在于「用真实数据打自己」：造的数据永远符合自己的假设，
// 而官方 blueprints 全部是 definitions.subgraphs 形态——如果解析器把它当成
// API 格式放行，用户会提交一张空图，然后得到一句看不懂的成功。
//
// 路径不存在时跳过（CI 里没有 ComfyUI 安装）。
func TestParseRealBlueprints(t *testing.T) {
	dir := "D:/AI/ComfyUI/blueprints"
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no local ComfyUI blueprints")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Skip("no blueprint files")
	}
	rejected, leaked := 0, []string{}
	for _, f := range files {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		_, perr := ParseComfyWorkflow(raw)
		if perr == nil {
			// 放行了！说明这个 UI 格式被当成 API 格式吃进去了
			leaked = append(leaked, filepath.Base(f))
			continue
		}
		if strings.Contains(perr.Error(), "子图") || strings.Contains(perr.Error(), "UI 格式") {
			rejected++
		}
	}
	if len(leaked) > 0 {
		t.Errorf("%d/%d 个 blueprint 被误判成 API 格式（会提交空图）：%v",
			len(leaked), len(files), leaked[:minInt(5, len(leaked))])
	}
	if rejected == 0 {
		t.Fatal("no blueprint was rejected — detection is not working at all")
	}
	t.Logf("真实 blueprint 检测：%d/%d 正确拒绝为 UI/子图格式", rejected, len(files))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
