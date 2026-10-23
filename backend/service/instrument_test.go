package service

import (
	"testing"

	"flowedge/backend/model"
)

// 接尺子的测试盯三件事：
//  ① 门必须接在终点前（插中间会切断产物传递，下游拿不到 video 字段）
//  ② 阈值必须从图里的生成参数反推（凭空给会把能跑的图改死）
//  ③ 已有门的图绝不动（不替人决定判据范围）

func edge(id, from, to string) model.EdgeInstance {
	return model.EdgeInstance{ID: id, Source: from, Target: to}
}

func filmGraph() ([]model.NodeInstance, []model.EdgeInstance) {
	nodes := []model.NodeInstance{
		cnode("s1", "start", map[string]interface{}{"prompt": "x"}),
		cnode("v1", "text2video", map[string]interface{}{
			"width": float64(608), "height": float64(352), "frames": float64(124),
		}),
		cnode("pp1", "post_production", map[string]interface{}{"loudness": float64(-14)}),
		cnode("e1", "end", map[string]interface{}{}),
	}
	edges := []model.EdgeInstance{
		edge("a", "s1", "v1"), edge("b", "v1", "pp1"), edge("c", "pp1", "e1"),
	}
	return nodes, edges
}

func TestInstrumentGates_InsertsBeforeEnd(t *testing.T) {
	nodes, edges := filmGraph()
	plan := InstrumentGates(nodes, edges, InstrumentOptions{})
	if !plan.Applied || len(plan.Gates) != 1 {
		t.Fatalf("应插入 1 个门，实得 applied=%v gates=%v skipped=%v", plan.Applied, plan.Gates, plan.Skipped)
	}
	gateID := plan.Gates[0]

	// 门必须是终点的直接前驱，且原前驱改为指向门
	var intoEnd, intoGate int
	for _, e := range plan.Edges {
		switch e.Target {
		case "e1":
			intoEnd++
			if e.Source != gateID {
				t.Fatalf("终点入边必须来自门，实得 %s", e.Source)
			}
		case gateID:
			intoGate++
			if e.Source != "pp1" {
				t.Fatalf("门的入边应来自原来的产物节点 pp1，实得 %s", e.Source)
			}
		}
	}
	if intoEnd != 1 || intoGate != 1 {
		t.Fatalf("接线错误：end 入边 %d 条、gate 入边 %d 条", intoEnd, intoGate)
	}

	// 产物节点本身不能被改接到门以外的地方（插中间就废了）
	for _, e := range plan.Edges {
		if e.Source == "pp1" && e.Target != gateID {
			t.Fatalf("pp1 仍应直连下游以外的唯一去处是门，实得指向 %s", e.Target)
		}
	}

	// 阈值必须来自图里的真实生成参数
	var g *model.NodeInstance
	for i := range plan.Nodes {
		if plan.Nodes[i].ID == gateID {
			g = &plan.Nodes[i]
		}
	}
	if g == nil || g.Type != "quality_gate" {
		t.Fatal("新节点必须是 quality_gate")
	}
	if got, _ := g.Data["min_frames"].(int); got != 117 {
		t.Fatalf("min_frames 应由 124 帧 × 0.95 反推为 117，实得 %v", g.Data["min_frames"])
	}
	if got, _ := g.Data["min_width"].(int); got != 608 {
		t.Fatalf("min_width 应等于生成宽 608，实得 %v", g.Data["min_width"])
	}
	if g.Data["require_audio"] != true {
		t.Fatalf("上游是后期混音节点，应要求音轨，实得 %v", g.Data["require_audio"])
	}
	// 门自身的阈值不许凭空给响度/静音（要按片子定）
	if _, ok := g.Data["loudness_min_db"]; ok {
		t.Fatal("不许自动给 loudness_min_db：那是判据，必须人按片子定")
	}
	if len(plan.Derived) == 0 {
		t.Fatal("必须报告每个阈值是从哪推出来的")
	}
}

func TestInstrumentGates_IdempotentWhenGateExists(t *testing.T) {
	nodes, edges := filmGraph()
	nodes = append(nodes, cnode("g0", "quality_gate", map[string]interface{}{"min_frames": float64(100)}))
	plan := InstrumentGates(nodes, edges, InstrumentOptions{})
	if plan.Applied {
		t.Fatal("已有门的图不许再插门")
	}
	if len(plan.Skipped) == 0 {
		t.Fatal("跳过必须给理由")
	}
}

func TestInstrumentGates_SkipsWhenNoEnd(t *testing.T) {
	nodes := []model.NodeInstance{
		cnode("s1", "start", map[string]interface{}{"prompt": "x"}),
		cnode("v1", "text2video", map[string]interface{}{"frames": float64(48)}),
	}
	plan := InstrumentGates(nodes, []model.EdgeInstance{edge("a", "s1", "v1")}, InstrumentOptions{})
	if plan.Applied {
		t.Fatal("没有终点就没有门的落点，不许乱接")
	}
	if len(plan.Skipped) == 0 {
		t.Fatal("必须说明为什么没接")
	}
}

func TestInstrumentGates_SkipsWhenNothingToDerive(t *testing.T) {
	// 上游没有任何生成参数：接上去只会是一条永远不查的空门
	nodes := []model.NodeInstance{
		cnode("s1", "start", map[string]interface{}{"prompt": "x"}),
		cnode("e1", "end", map[string]interface{}{}),
	}
	plan := InstrumentGates(nodes, []model.EdgeInstance{edge("a", "s1", "e1")}, InstrumentOptions{})
	if plan.Applied {
		t.Fatal("反推不出任何阈值时不许接空门（空门只会让证据多一行 gateNodes）")
	}
	if len(plan.Skipped) == 0 {
		t.Fatal("必须说明缺什么")
	}
}

func TestInstrumentGates_MultipleEndsGetOwnGate(t *testing.T) {
	nodes := []model.NodeInstance{
		cnode("v1", "text2video", map[string]interface{}{"frames": float64(100), "fps": float64(24)}),
		cnode("e1", "end", map[string]interface{}{}),
		cnode("e2", "end", map[string]interface{}{}),
	}
	edges := []model.EdgeInstance{edge("a", "v1", "e1"), edge("b", "v1", "e2")}
	plan := InstrumentGates(nodes, edges, InstrumentOptions{})
	if len(plan.Gates) != 2 {
		t.Fatalf("两个终点应各接一个门，实得 %v", plan.Gates)
	}
	// 时长阈值：100 帧 ÷ 24fps ≈ 4.17s，±5%
	var g *model.NodeInstance
	for i := range plan.Nodes {
		if plan.Nodes[i].ID == plan.Gates[0] {
			g = &plan.Nodes[i]
		}
	}
	if d, _ := g.Data["duration_min"].(float64); d <= 3.9 || d >= 4.0 {
		t.Fatalf("duration_min 应约为 3.96s，实得 %v", g.Data["duration_min"])
	}
}

// 阈值反推必须乘 segments：frames 是单段帧数，不是成片帧数。
//
// 真机事故（2026-10-23）：修士斗法 v.frames=124、segments=6，成片 31.05s/1484 帧。
// 只按单段反推得 duration_max=5.43s，门第一次跑就把一条本来合格的成片判成超标 ——
// 这类参数错配比「没接门」更糟，它让门看起来在工作，实际在乱杀。
func TestInstrumentGates_MultipliesSegments(t *testing.T) {
	nodes := []model.NodeInstance{
		{ID: "s", Type: "start", Status: "pending", Data: map[string]interface{}{}},
		{ID: "v", Type: "text2video", Status: "pending", Data: map[string]interface{}{
			"frames": 124.0, "segments": 6.0, "fps": 24.0,
			"width": 736.0, "height": 416.0, "interp": true,
		}},
		{ID: "e", Type: "end", Status: "pending", Data: map[string]interface{}{}},
	}
	edges := []model.EdgeInstance{
		{ID: "e1", Source: "s", Target: "v"},
		{ID: "e2", Source: "v", Target: "e"},
	}
	plan := InstrumentGates(nodes, edges, InstrumentOptions{})
	if !plan.Applied {
		t.Fatalf("应接门，实得 skipped=%v", plan.Skipped)
	}
	var gate *model.NodeInstance
	for i := range plan.Nodes {
		if plan.Nodes[i].Type == "quality_gate" {
			gate = &plan.Nodes[i]
		}
	}
	if gate == nil {
		t.Fatal("没插入 quality_gate")
	}
	// 6 段 × 124 帧 = 744 帧 → 5% 容差下界 706；31.0s ±5% → 29.45 / 32.55
	if got := gate.Data["min_frames"]; got != 706 {
		t.Errorf("min_frames 应按 744 帧反推为 706，实得 %v", got)
	}
	if got := gate.Data["duration_min"]; got != 29.45 {
		t.Errorf("duration_min 应为 29.45，实得 %v", got)
	}
	if got := gate.Data["duration_max"]; got != 32.55 {
		t.Errorf("duration_max 应为 32.55，实得 %v", got)
	}
}
