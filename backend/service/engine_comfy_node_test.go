package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flowedge/backend/model"
)

func mkComfy(data map[string]interface{}) *model.NodeInstance {
	return &model.NodeInstance{ID: "c1", Type: "comfy", Data: data}
}

// 超时：默认 20 分钟；timeout_min 夹在 [1, 120]；类型硬上限同步到 NodeSpecs。
func TestComfyNodeTimeoutBudget(t *testing.T) {
	if got := comfyNodeTimeout(mkComfy(nil)); got != comfyDefaultTimeout {
		t.Fatalf("默认超时应为 %v，实际 %v", comfyDefaultTimeout, got)
	}
	if got := comfyNodeTimeout(mkComfy(map[string]interface{}{"timeout_min": float64(45)})); got != 45*time.Minute {
		t.Fatalf("timeout_min=45 应为 45m，实际 %v", got)
	}
	// 0.5 分钟 → 抬到 1 分钟下限（太短的超时会杀掉正在冷加载的实例）
	if got := comfyNodeTimeout(mkComfy(map[string]interface{}{"timeout_min": float64(0.5)})); got != time.Minute {
		t.Fatalf("timeout_min=0.5 应抬到 1m，实际 %v", got)
	}
	// 999 分钟 → 压到 2 小时硬上限
	if got := comfyNodeTimeout(mkComfy(map[string]interface{}{"timeout_min": float64(999)})); got != comfyMaxTimeout {
		t.Fatalf("timeout_min=999 应压到 %v，实际 %v", comfyMaxTimeout, got)
	}
	if nodeMaxTimeoutFor("comfy") != comfyMaxTimeout {
		t.Fatalf("comfy 硬上限应同步到 NodeSpecs（%v），实际 %v", comfyMaxTimeout, nodeMaxTimeoutFor("comfy"))
	}
}

func TestComfyInstanceProfile(t *testing.T) {
	cases := map[string]string{
		"": "", "comfyui": "", "default": "", "main": "", "  ": "",
		"h3": "h3", "H3": "h3", "moss": "moss", "MOSS": "moss",
	}
	for in, want := range cases {
		if got := comfyInstanceProfile(mkComfy(map[string]interface{}{"instance": in})); got != want {
			t.Errorf("instance %q → %q，期望 %q", in, got, want)
		}
	}
	// 未知实例原样返回，由执行器显式报错（不许静默退回默认实例）
	if got := comfyInstanceProfile(mkComfy(map[string]interface{}{"instance": "foo"})); got != "foo" {
		t.Errorf("未知实例应原样返回，实际 %q", got)
	}
}

// 执行器对未知实例必须先拦：ensureComfy 会把不认识的名字静默映射到默认实例，
// 那条路径的后果是「跑成功了但不是用户选的实例」。
func TestExecComfyNodeRejectsUnknownInstance(t *testing.T) {
	e := &EngineV2{}
	_, err := e.execComfyNode(t.Context(), mkComfy(map[string]interface{}{
		"instance": "bogus", "workflow_json": `{"1":{}}`,
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "不是可用实例") {
		t.Fatalf("未知实例应报「不是可用实例」，实际 %v", err)
	}
}

func TestSubstituteComfyPlaceholders(t *testing.T) {
	subs := map[string]string{
		"prompt": "一只猫", "image": "in.png", "width": "512", "height": "512", "seed": "7",
	}
	out, err := substituteComfyPlaceholders(`{"p":"{{prompt}}","i":"{{image}}","w":{{width}},"seed":{{seed}}}`, subs)
	if err != nil {
		t.Fatalf("全量替换不应报错：%v", err)
	}
	if !strings.Contains(out, "一只猫") || strings.Contains(out, "{{") {
		t.Fatalf("占位符没被替换干净：%s", out)
	}

	// 模板用到 {{prompt}} 但这次没有值 → 报缺什么，不填空串（空串会提交一张画不出的图）
	if _, err := substituteComfyPlaceholders("{{prompt}}", map[string]string{"prompt": ""}); err == nil ||
		!strings.Contains(err.Error(), "{{prompt}}") {
		t.Fatalf("缺 prompt 应点名 {{prompt}}，实际 %v", err)
	}
	// 不认识的占位符 → 点名，不许当普通文本提交上去
	if _, err := substituteComfyPlaceholders("{{proimpt}}", subs); err == nil ||
		!strings.Contains(err.Error(), "proimpt") {
		t.Fatalf("不认识的占位符应被点名，实际 %v", err)
	}
	// 没闭合的 {{ → 报结构错误
	if _, err := substituteComfyPlaceholders("{{prompt", subs); err == nil {
		t.Fatal("未闭合占位符应报错")
	}
}

func TestComfySubstitutionsFromUpstream(t *testing.T) {
	// prompt/image 都缺省时从上游取：文本取 ["text"]，图取 ["image"]
	up := map[string]interface{}{
		"s": map[string]interface{}{"text": "上游提示词"},
		"g": map[string]interface{}{"image": "up.png"},
	}
	subs := comfySubstitutions(mkComfy(nil), up)
	if subs["prompt"] != "上游提示词" || subs["image"] != "up.png" {
		t.Fatalf("上游取值失败：%+v", subs)
	}
	// 节点字段优先于上游
	subs = comfySubstitutions(mkComfy(map[string]interface{}{
		"prompt": "节点里的", "image_path": "node.png",
		"width": float64(768), "height": float64(432), "seed": float64(42),
	}), up)
	if subs["prompt"] != "节点里的" || subs["image"] != "node.png" {
		t.Fatalf("节点字段应优先：%+v", subs)
	}
	if subs["width"] != "768" || subs["height"] != "432" || subs["seed"] != "42" {
		t.Fatalf("宽高/种子替换值错误：%+v", subs)
	}
	// 宽高为 0（用户没填）时不写占位符值 —— 模板用了 {{width}} 会在替换时报错
	subs = comfySubstitutions(mkComfy(nil), nil)
	if subs["width"] != "" || subs["seed"] != "" {
		t.Fatalf("未填的数值字段应留空：%+v", subs)
	}
}

func TestResolveComfyGraphText(t *testing.T) {
	// 内联优先
	txt, source, err := resolveComfyGraphText(mkComfy(map[string]interface{}{
		"workflow_json": `{"1":{}}`, "template": "whatever",
	}))
	if err != nil || !strings.Contains(txt, `"1"`) || !strings.Contains(source, "内联") {
		t.Fatalf("内联 JSON 应优先，实际 source=%s err=%v", source, err)
	}
	// 两个都没填 → 说清楚要什么
	if _, _, err := resolveComfyGraphText(mkComfy(nil)); err == nil ||
		!strings.Contains(err.Error(), "模板名") {
		t.Fatalf("缺图来源应提示模板名，实际 %v", err)
	}
	// 模板不存在 → 报路径并说明目录状态（测试目录下没有模板库）
	if _, _, err := resolveComfyGraphText(mkComfy(map[string]interface{}{"template": "nope"})); err == nil ||
		!strings.Contains(err.Error(), "nope.json") {
		t.Fatalf("模板缺失应带文件名，实际 %v", err)
	}
}

func TestKindOfMediaPath(t *testing.T) {
	cases := map[string]string{
		"a.png": "image", "b.MP4": "video", "c.mkv": "video", "d.wav": "audio",
		"e.flac": "audio", "f.txt": "image", // 未知扩展按图处理（ComfyUI 的 SaveImage 是默认结局）
	}
	for p, want := range cases {
		if got := kindOfMediaPath(p); got != want {
			t.Errorf("kindOfMediaPath(%q)=%q，期望 %q", p, got, want)
		}
	}
}

// 画布契约一致性：端口兼容（any 输出可接任何输入）、参数校验、估算进 Uncertain。
func TestComfyCanvasContract(t *testing.T) {
	c, ok := NodeContractOf("comfy")
	if !ok {
		t.Fatal("comfy 没有画布契约（nodecatalog_test 本应红）")
	}
	if len(c.In) == 0 || len(c.Out) != 1 || c.Out[0].Kind != PortAny {
		t.Fatalf("comfy 端口契约不符：%+v", c)
	}

	// 参数校验：template 与 workflow_json 至少一个
	issues := ValidateNodeParams([]model.NodeInstance{*mkComfy(nil)}, nil)
	if len(issues) != 1 {
		t.Fatalf("缺图来源应报 1 条，实际 %d", len(issues))
	}
	issues = ValidateNodeParams([]model.NodeInstance{
		*mkComfy(map[string]interface{}{"workflow_json": `{"1":{}}`}),
	}, nil)
	if len(issues) != 0 {
		t.Fatalf("给了 workflow_json 不该再报：%+v", issues)
	}

	// 估算：图内容未知 ⇒ 进 Uncertain，不编数字（对齐 quality_loop 的口径）
	est := EstimateGraph([]model.NodeInstance{*mkComfy(nil)}, nil)
	if len(est.Uncertain) == 0 {
		t.Fatal("comfy 应进 Uncertain 列表")
	}
	for _, ln := range est.Lines {
		if ln.NodeType == "comfy" {
			t.Fatalf("comfy 不该有时间线：%+v", ln)
		}
	}

	// 连线：comfy 的 any 输出接 text2image 的 image 入口不应报类型不匹配
	nodes := []model.NodeInstance{
		{ID: "c", Type: "comfy"},
		{ID: "t2i", Type: "text2image"},
	}
	issues2 := ValidatePorts(nodes, []model.EdgeInstance{{ID: "e1", Source: "c", Target: "t2i"}})
	for _, is := range issues2 {
		if is.Code == IssueTypeMismatch {
			t.Fatalf("any 输出接 image 入口被误判为类型不匹配：%+v", is)
		}
	}
}

// 端到端（mock ComfyUI）：占位符替换 → 提交 → 轮询 → 取回产物。
// 按 §0.4 的纪律，验证的是「提交到服务器的图里占位符真的被换掉了」，
// 不是"本地没报错"。
func TestIntegration_ComfyEscapeHatch(t *testing.T) {
	m := newMockComfyServer(t)
	svc := newTestT2I(m.srv.URL, t.TempDir())
	e := &EngineV2{t2i: svc}

	wf := `{
		"4": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "flux1-schnell-fp8.safetensors"}},
		"6": {"class_type": "CLIPTextEncode", "inputs": {"text": "{{prompt}}", "clip": ["4", 0]}},
		"9": {"class_type": "SaveImage", "inputs": {"filename_prefix": "escape"}}
	}`
	out, err := e.execComfyNode(t.Context(), mkComfy(map[string]interface{}{
		"workflow_json": wf,
		"prompt":        "一只戴鲸鱼围裙的蓝发角色",
	}), nil)
	if err != nil {
		t.Fatalf("execComfyNode: %v", err)
	}
	res, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("输出应为 map，实际 %T", out)
	}
	if res["kind"] != "image" {
		t.Fatalf("mock 产物是 png，kind 应为 image，实际 %v", res["kind"])
	}
	p, _ := res["image"].(string)
	if p == "" {
		t.Fatalf("应有 image 路径：%+v", res)
	}
	if _, statErr := os.Stat(p); statErr != nil {
		t.Fatalf("产物文件不存在：%v", statErr)
	}
	if res["comfy_source"] != "内联 workflow_json" {
		t.Errorf("comfy_source = %v", res["comfy_source"])
	}

	// 提交到服务器的图：{{prompt}} 必须已替换成节点里的提示词
	g := m.promptAt(0)
	text := nodeInputs(g, "6")["text"]
	if text != "一只戴鲸鱼围裙的蓝发角色" {
		t.Errorf("提交图里 text = %v，占位符没被替换", text)
	}

	// 未知占位符必须显式报错（不许把字面量花括号提交上去）
	_, err = e.execComfyNode(t.Context(), mkComfy(map[string]interface{}{
		"workflow_json": `{"9":{"class_type":"SaveImage","inputs":{"x":"{{oops}}"}}}`,
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("未知占位符应点名，实际 %v", err)
	}
	// 空对象图必须显式报错
	_, err = e.execComfyNode(t.Context(), mkComfy(map[string]interface{}{"workflow_json": `{}`}), nil)
	if err == nil || !strings.Contains(err.Error(), "空对象") {
		t.Fatalf("空图应报错，实际 %v", err)
	}
}

// 模板文件来源：把模板写进 ComfyTemplateDir 后应能按名解析（补 resolveComfyGraphText 的正路径）。
func TestResolveComfyGraphTextTemplate(t *testing.T) {
	dir := ComfyTemplateDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("建不了模板目录 %s：%v", dir, err)
	}
	name := "engine_comfy_node_test_tpl"
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(`{"9":{}}`), 0o644); err != nil {
		t.Skipf("写不了模板文件：%v", err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, name+".json")) })

	txt, source, err := resolveComfyGraphText(mkComfy(map[string]interface{}{"template": name}))
	if err != nil || !strings.Contains(txt, `"9"`) || !strings.Contains(source, name) {
		t.Fatalf("模板解析失败：txt=%s source=%s err=%v", txt, source, err)
	}
}
