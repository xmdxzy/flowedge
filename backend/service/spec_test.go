package service

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 契约表是模块的唯一门面：注册了执行体却没有契约，任务书就会缺一段；
// 契约指向不存在的类型，用户会在文档里看到一个跑不起来的模块。
// 这条测试是「万物皆可模块」的地基——加节点必须同时给契约。
func TestModuleContractCoversRegistry(t *testing.T) {
	e := &EngineV2{}
	e.registerBuiltinNodes()

	for typ := range e.nodes {
		if _, ok := ModuleContractOf(typ); !ok {
			t.Errorf("节点 %q 已注册执行体，但没有模块契约（端口/参数/门层都无从得知）", typ)
		}
	}
	for typ := range Modules {
		if _, ok := e.nodes[typ]; !ok {
			t.Errorf("契约声明了 %q，但没有注册执行体——任务书会写一个跑不起来的模块", typ)
		}
		if _, ok := NodeSpecs[typ]; !ok {
			t.Errorf("契约声明了 %q，但 NodeSpecs 没有它（调度与任务书会各说各话）", typ)
		}
	}
}

// 端口自洽：非 source 必须有入端口，非 sink 必须有出端口，端口类型必须是已知 kind。
func TestModuleContractPortsWellFormed(t *testing.T) {
	validKind := map[PortKind]bool{
		PortText: true, PortImage: true, PortVideo: true,
		PortAudio: true, PortJSON: true, PortAny: true,
	}
	validRole := map[ModuleRole]bool{
		RoleSource: true, RoleTransform: true, RoleGenerator: true,
		RoleControl: true, RoleGate: true, RoleSink: true,
	}
	for typ, c := range Modules {
		if c.Type != typ {
			t.Errorf("%s: 契约的 Type 字段是 %q，与 key 不一致", typ, c.Type)
		}
		if strings.TrimSpace(c.Title) == "" {
			t.Errorf("%s: 缺 Title（前端卡片与任务书标题都靠它）", typ)
		}
		if !validRole[c.Role] {
			t.Errorf("%s: Role=%q 不是已知角色", typ, c.Role)
		}
		if c.Role != RoleSource && len(c.Inputs) == 0 {
			t.Errorf("%s: 角色 %s 必须有入端口", typ, c.Role)
		}
		if c.Role != RoleSink && len(c.Outputs) == 0 {
			t.Errorf("%s: 角色 %s 必须有出端口", typ, c.Role)
		}
		for _, p := range append(append([]Port{}, c.Inputs...), c.Outputs...) {
			if strings.TrimSpace(p.Name) == "" {
				t.Errorf("%s: 存在空名端口", typ)
			}
			if !validKind[p.Kind] {
				t.Errorf("%s 端口 %s: Kind=%q 不是已知类型", typ, p.Name, p.Kind)
			}
		}
		for _, kp := range c.KeyParams {
			if strings.TrimSpace(kp.Key) == "" || strings.TrimSpace(kp.Label) == "" {
				t.Errorf("%s: 参数条目缺 Key 或 Label", typ)
			}
		}
		for _, g := range c.GateLayers {
			if _, ok := gateParamKeys[g]; !ok {
				t.Errorf("%s: 声明了未知门层 %q", typ, g)
			}
		}
	}
}

// 端口类型只允许两种关系：同类型互通、any 与任何类型互通。
// 引擎不许出现"哪些节点能接哪些"的白名单——那就是分类。
func TestPortKindAccepts(t *testing.T) {
	cases := []struct {
		from, to PortKind
		want     bool
	}{
		{PortText, PortText, true},
		{PortText, PortImage, false},
		{PortAny, PortVideo, true},
		{PortAudio, PortAny, true},
		{PortJSON, PortText, false},
	}
	for _, c := range cases {
		if got := PortKindAccepts(c.from, c.to); got != c.want {
			t.Errorf("%s → %s 期望 %v，实际 %v", c.from, c.to, c.want, got)
		}
	}
}

// spec.go 里不许出现任何节点类型字符串。
// 一旦有人写 `if n.Type == "shotlist"`，任务书就开始对某类节点特殊照顾，
// 新加的模态就会漏——这条测试直接把这条路堵死。
func TestSpecCompilerHasNoTypeSwitches(t *testing.T) {
	raw, err := os.ReadFile("spec.go")
	if err != nil {
		t.Fatalf("读 spec.go 失败：%v", err)
	}
	src := string(raw)
	for typ := range NodeSpecs {
		if strings.Contains(src, `"`+typ+`"`) {
			t.Errorf("spec.go 出现了节点类型字面量 %q：任务书必须完全由契约派生，不许按类型分支", typ)
		}
	}
}

// 造一条包含**全部已注册类型**的链，任务书必须每个节点都覆盖到。
// 这条测试替代了"手抄一份节点清单"——加节点自动被覆盖。
func TestSpecCoversEveryNode(t *testing.T) {
	e := &EngineV2{}
	e.registerBuiltinNodes()

	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	prev := ""
	i := 0
	for typ := range e.nodes {
		id := "n" + typ
		nodes = append(nodes, model.NodeInstance{
			ID: id, Type: typ, Data: map[string]interface{}{},
		})
		if prev != "" {
			edges = append(edges, model.EdgeInstance{ID: "e" + id, Source: prev, Target: id})
		}
		prev = id
		i++
	}
	wf := &model.Workflow{ID: 1, Name: "契约覆盖", Nodes: mustJSON(nodes), Edges: mustJSON(edges)}

	doc, err := CompileSpec(wf, "json")
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	got := map[string]int{}
	for _, m := range doc.Modules {
		got[m.ID]++
	}
	if len(got) != len(nodes) {
		t.Errorf("任务书覆盖 %d 个节点，实际有 %d 个", len(got), len(nodes))
	}
	for _, n := range nodes {
		if got[n.ID] != 1 {
			t.Errorf("节点 %s（%s）在任务书里出现 %d 次，期望 1 次", n.ID, n.Type, got[n.ID])
		}
	}
	if doc.Markdown == "" {
		t.Fatal("markdown 为空")
	}
}

// 分镜执行节点：plan.gates 里的真实阈值必须进「验收门」，
// 而不是写一句"承载五层门"就算交代了。
func TestSpecGatesFromPlanJSON(t *testing.T) {
	plan := map[string]interface{}{
		"title":  "test",
		"frames": 192,
		"gates": map[string]interface{}{
			"timing": map[string]interface{}{"target_seconds": 30.0, "max_shot_seconds": 4.5},
			"audio":  map[string]interface{}{"max_silence_gap": 0.8},
		},
	}
	raw, _ := json.Marshal(plan)
	nodes := []model.NodeInstance{
		{ID: "s", Type: "shotlist", Data: map[string]interface{}{"plan": string(raw)}},
	}
	wf := &model.Workflow{ID: 2, Name: "门", Nodes: mustJSON(nodes), Edges: mustJSON([]model.EdgeInstance{})}

	doc, err := CompileSpec(wf, "json")
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	var sawTarget, sawShot bool
	for _, g := range doc.Gates {
		if g.Layer == "timing" && strings.Contains(g.Rule, "target_seconds = 30") {
			sawTarget = true
		}
		if g.Layer == "timing" && strings.Contains(g.Rule, "max_shot_seconds = 4.5") {
			sawShot = true
		}
	}
	if !sawTarget || !sawShot {
		t.Fatalf("plan.gates 的阈值没进任务书：%+v", doc.Gates)
	}
}

// 未声明契约 + 孤立节点 + 必填输入没来源，三种都必须写进 Warnings。
// 任务书的底线是"不确定就写出来"，不是"看起来都齐了"。
func TestSpecWarnsInsteadOfSilentlyPassing(t *testing.T) {
	nodes := []model.NodeInstance{
		{ID: "a", Type: "text2image", Data: map[string]interface{}{"prompt": "x"}}, // 无
		{ID: "b", Type: "no_such_module", Data: map[string]interface{}{}},
		{ID: "c", Type: "tts", Data: map[string]interface{}{}}, // text 必填且无上游
	}
	wf := &model.Workflow{ID: 3, Name: "警告", Nodes: mustJSON(nodes), Edges: mustJSON([]model.EdgeInstance{})}
	doc, err := CompileSpec(wf, "md")
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	joined := strings.Join(doc.Warnings, "\n")
	if !strings.Contains(joined, "no_such_module") {
		t.Errorf("未声明契约的节点没有进 Warnings：%v", doc.Warnings)
	}
	if !strings.Contains(joined, "没有入边") {
		t.Errorf("孤立节点没有进 Warnings：%v", doc.Warnings)
	}
	// tts 的 text 必填、没有上游、也没在参数里给 → 必须点名
	if !strings.Contains(joined, "text") {
		t.Errorf("必填输入缺失没有进 Warnings：%v", doc.Warnings)
	}
	if !strings.Contains(doc.Markdown, "未说清的地方") {
		t.Error("markdown 缺「未说清的地方」段落")
	}
}

// 成环时节点不能凭空消失：任务书要写出来，并明确说顺序未定。
func TestSpecCycleStillListsNodes(t *testing.T) {
	nodes := []model.NodeInstance{
		{ID: "a", Type: "llm", Data: map[string]interface{}{}},
		{ID: "b", Type: "llm", Data: map[string]interface{}{}},
	}
	edges := []model.EdgeInstance{
		{ID: "e1", Source: "a", Target: "b"},
		{ID: "e2", Source: "b", Target: "a"},
	}
	wf := &model.Workflow{ID: 4, Name: "环", Nodes: mustJSON(nodes), Edges: mustJSON(edges)}
	doc, err := CompileSpec(wf, "md")
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	if len(doc.Modules) != 2 {
		t.Fatalf("成环时任务书只有 %d 个模块，期望 2 个", len(doc.Modules))
	}
	if !strings.Contains(strings.Join(doc.Warnings, "\n"), "环") {
		t.Errorf("成环没进 Warnings：%v", doc.Warnings)
	}
	if !strings.Contains(doc.Markdown, "顺序未定") {
		t.Error("markdown 没标出「顺序未定」")
	}
}

// 参数只报真实写的值：默认值不许被凭空补出来。
func TestSpecDoesNotInventDefaults(t *testing.T) {
	nodes := []model.NodeInstance{
		{ID: "i", Type: "text2image", Data: map[string]interface{}{"prompt": "雪夜", "width": float64(768)}},
	}
	wf := &model.Workflow{ID: 5, Name: "默认值", Nodes: mustJSON(nodes), Edges: mustJSON([]model.EdgeInstance{})}
	doc, _ := CompileSpec(wf, "md")

	var keys []string
	for _, p := range doc.Modules[0].Params {
		keys = append(keys, p.Key)
	}
	joined := strings.Join(keys, ",")
	if !strings.Contains(joined, "width") {
		t.Fatalf("真实写的参数丢了：%v", keys)
	}
	// prompt 已不是文生图的契约参数（提示词只来自上游）；写了也照录到「其他参数」，
	// 不许因为"契约里没有"就把它丢掉——那等于改写用户的图。
	var extra []string
	for _, p := range doc.Modules[0].Extra {
		extra = append(extra, p.Key)
	}
	if !strings.Contains(strings.Join(extra, ","), "prompt") {
		t.Errorf("契约外的字段没有照录：%v", extra)
	}
	// height 没写，就不许出现在任务书里
	if strings.Contains(joined, "height") {
		t.Errorf("height 没在画布上写过，却被补进了任务书：%v", keys)
	}
	if strings.Contains(doc.Markdown, "height = ") {
		t.Error("markdown 里出现了未设置的 height")
	}
}

func mustJSON(v interface{}) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// 声明了门层却没有任何阈值，必须高声警告——这是最危险的静默失败：
// 任务书上写着"承载五层门"，看起来有验收，实际什么都不查。
func TestSpecWarnsWhenGatesDeclaredWithoutThresholds(t *testing.T) {
	plan := map[string]interface{}{"title": "无门的片子", "frames": 192}
	raw, _ := json.Marshal(plan)
	nodes := []model.NodeInstance{
		{ID: "sl", Type: "shotlist", Data: map[string]interface{}{"plan": string(raw)}},
	}
	wf := &model.Workflow{ID: 6, Name: "无门", Nodes: mustJSON(nodes), Edges: mustJSON([]model.EdgeInstance{})}
	doc, err := CompileSpec(wf, "md")
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	joined := strings.Join(doc.Warnings, "\n")
	if !strings.Contains(joined, "等于没设门") {
		t.Fatalf("没有阈值却没警告：%v", doc.Warnings)
	}
	if !strings.Contains(doc.Markdown, "没有任何阈值") {
		t.Error("markdown 的验收门段没有提示阈值缺失")
	}
	if strings.Contains(doc.Markdown, "没有任何模块声明门层") {
		t.Error("模块明明声明了门层，却说「没有任何模块声明门层」")
	}
}

// 端口必须按**源节点类型**解析。曾经把节点 id 当类型查表，结果恒为 out/any，
// 端口不匹配永远检测不出来，任务书上的「输入」全是假的。
func TestSpecResolvesPortsBySourceType(t *testing.T) {
	nodes := []model.NodeInstance{
		{ID: "s", Type: "start", Data: map[string]interface{}{"prompt": "x"}},
		{ID: "b", Type: "storyboard", Data: map[string]interface{}{}},
		{ID: "i", Type: "text2image", Data: map[string]interface{}{"prompt": "y"}},
	}
	edges := []model.EdgeInstance{
		{ID: "e1", Source: "s", Target: "b"},
		{ID: "e2", Source: "b", Target: "i"},
	}
	wf := &model.Workflow{ID: 7, Name: "端口", Nodes: mustJSON(nodes), Edges: mustJSON(edges)}
	doc, _ := CompileSpec(wf, "json")

	var sb, t2i SpecModule
	for _, m := range doc.Modules {
		switch m.ID {
		case "b":
			sb = m
		case "i":
			t2i = m
		}
	}
	if len(sb.Inputs) != 1 || sb.Inputs[0].FromPort != "text" || sb.Inputs[0].Kind != "text" {
		t.Fatalf("start → storyboard 应是 text 端口，实际 %+v", sb.Inputs)
	}
	// storyboard 的第一个输出是 plan(json)，text2image 的第一个输入是 text → 类型不匹配
	if len(t2i.Inputs) != 1 || t2i.Inputs[0].FromPort != "plan" {
		t.Fatalf("storyboard → text2image 应解析出 plan 端口，实际 %+v", t2i.Inputs)
	}
	if !t2i.Inputs[0].Mismatch {
		t.Errorf("json → text 应标为端口不匹配，实际 %+v", t2i.Inputs[0])
	}
}

// 入口来源要进任务书：这条流水线是给 WorkBuddy 还是 DSH 调的，
// 决定智能体拿到的上下文与职责边界，不能只活在 UI 里。
func TestSpecRecordsEntrySource(t *testing.T) {
	nodes := []model.NodeInstance{
		{ID: "s", Type: "start", Data: map[string]interface{}{"prompt": "x", "entry_agent": "dsh"}},
	}
	wf := &model.Workflow{ID: 3, Name: "入口", Nodes: mustJSON(nodes), Edges: mustJSON([]model.EdgeInstance{})}
	doc, err := CompileSpec(wf, "md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Entry != "dsh" {
		t.Fatalf("入口应为 dsh，实际 %q", doc.Entry)
	}
	if !strings.Contains(doc.Markdown, "DSH（外部智能体）") {
		t.Errorf("markdown 没写入口：\n%s", doc.Markdown)
	}
}
