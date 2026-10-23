package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// canonicalWorkflowPath 参考实现工作流（data/workflows/canonical-film.json）。
// 它是「出片最佳实践」的引擎内落点：换片子只改 plan，门与拓扑不动。
func canonicalWorkflowPath() string {
	return filepath.Join("..", "..", "data", "workflows", "canonical-film.json")
}

// loadCanonicalWorkflow 读参考实现。文件缺失 = 参考实现被删了，测试必须红——
// 否则「canonical 工作流」只剩文档里一句话，引擎里没有对应物。
func loadCanonicalWorkflow(t *testing.T) model.Workflow {
	t.Helper()
	b, err := os.ReadFile(canonicalWorkflowPath())
	if err != nil {
		t.Fatalf("读不到 canonical 工作流（%s）：%v", canonicalWorkflowPath(), err)
	}
	var wf model.Workflow
	if err := json.Unmarshal(b, &wf); err != nil {
		t.Fatalf("canonical 工作流不是合法 JSON：%v", err)
	}
	return wf
}

// canonicalPlanOf 取出 shotlist 节点内嵌的分镜表原文。
func canonicalPlanOf(t *testing.T, wf model.Workflow) (string, *model.NodeInstance) {
	t.Helper()
	var nodes []model.NodeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		t.Fatalf("解析 nodes 失败：%v", err)
	}
	for i := range nodes {
		if nodes[i].Type != "shotlist" {
			continue
		}
		raw, _ := nodes[i].Data["plan"].(string)
		if strings.TrimSpace(raw) == "" {
			t.Fatalf("shotlist 节点没有内嵌 plan：canonical 工作流必须是自包含的")
		}
		return raw, &nodes[i]
	}
	t.Fatal("canonical 工作流里没有 shotlist 节点")
	return "", nil
}

// 参考分镜必须过得掉五层门——走的是执行器同一条路
// （ParseShotPlan → buildGatePlanMap → CheckFilmGates(requireGates=true)）。
// 这道测试盯着 canonical 文件本身：谁把 plan 改违约了，这里就红。
func TestCanonicalPlanPassesFiveLayerGates(t *testing.T) {
	wf := loadCanonicalWorkflow(t)
	rawPlan, _ := canonicalPlanOf(t, wf)

	plan, err := ParseShotPlan(rawPlan)
	if err != nil {
		t.Fatalf("canonical plan 解析失败：%v", err)
	}
	if len(plan.Shots) != 3 {
		t.Fatalf("参考实现应是 3 段（setup/clash/finish），实际 %d", len(plan.Shots))
	}
	if len(plan.Gates) == 0 {
		t.Fatal("canonical plan 必须带 gates：没有阈值的路子不是参考实现")
	}

	// 与 execShotListNode 同样解析参考图（相对路径按 CWD 绝对化）
	refImgs := append([]string{}, plan.Characters...)
	for i := range refImgs {
		if !filepath.IsAbs(refImgs[i]) {
			if abs, aerr := filepath.Abs(refImgs[i]); aerr == nil {
				refImgs[i] = abs
			}
		}
	}

	verdict, gates, stats := CheckFilmGates(buildGatePlanMap(plan, refImgs, 608, 352), true)
	if verdict.HasFail() {
		t.Fatalf("canonical plan 必须全部门通过，实际有 FAIL：\n%s", gateFailMessage(verdict))
	}
	if len(stats.Shots) != 3 {
		t.Errorf("门检应数到 3 段，实际 %d（%v）", len(stats.Shots), stats.Shots)
	}
	// 时长铁律：30s 目标（729 帧 / 24fps = 30.375s，容差 0.75s 内）
	if got := gateFloat(gateMapGet(gates, "timing", "target_seconds"), 0); got != 30.0 {
		t.Errorf("timing.target_seconds 应为 30.0，实际 %v", got)
	}
	for _, w := range verdict.Warns() {
		t.Logf("[canonical] 门告警（不阻断，但参考实现应当干净）：%s: %s", w.Gate, w.Msg)
	}
}

// 任务书必须把五层门的阈值全部写出来，且三个门节点一个都不少。
// 「声明有门却无阈值」会被 spec 记 Warnings——参考实现不允许出现。
func TestCanonicalWorkflowSpecShowsEveryGate(t *testing.T) {
	wf := loadCanonicalWorkflow(t)
	doc, err := CompileSpec(&wf, "md")
	if err != nil {
		t.Fatalf("编译任务书失败：%v", err)
	}

	// 1) 五层门都要在任务书里出现，且都带阈值
	layers := map[string]int{}
	for _, g := range doc.Gates {
		layers[g.Layer]++
	}
	for _, layer := range gateLayerOrder {
		if layers[layer] == 0 {
			t.Errorf("任务书缺少 %s 层门：参考实现必须五层全带", layer)
		}
	}

	// 2) 关键阈值逐一现值（plan.gates 与三个门节点的参数）
	want := []string{
		"timing.target_seconds = 30",
		"timing.min_shot_seconds = 1.2",
		"timing.max_shot_seconds = 4.5",
		"identity.stills = 2",
		"audio.loudness_db = [-20,-11]",
		"technical.min_width = 608",
		"budget.wall_seconds_max = 1500",
		"min_frames = 700",
		"loudness_min_db = -20",
		"max_silence_gap = 5",
		"layout_min = 0.94",
		"palette_min = 0.92",
	}
	joined := doc.Markdown
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("任务书里找不到阈值 %q——门必须有牙齿，看不见阈值等于没门", w)
		}
	}

	// 3) 三个门节点都到场
	types := map[string]bool{}
	for _, m := range doc.Modules {
		types[m.Type] = true
	}
	for _, nt := range []string{"shotlist", "consistency_gate", "post_production", "quality_gate"} {
		if !types[nt] {
			t.Errorf("参考实现缺少 %s 节点", nt)
		}
	}

	// 4) 没有「声明门层却无阈值」的警告
	for _, w := range doc.Warnings {
		if strings.Contains(w, "一个阈值都没有") {
			t.Errorf("参考实现不该有裸门警告：%s", w)
		}
	}
}

// canonical 图结构：无 error 级问题（能存、能排）、必填输入全有来源。
// 端口上 start→shotlist 有一条 text→plan 的已知不匹配（真实库定标 11/12 legal
// 的唯一例外正是这个「plan 直填」模式），这里只锁定「不许再变多」。
func TestCanonicalWorkflowStructureIsRunnable(t *testing.T) {
	wf := loadCanonicalWorkflow(t)
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		t.Fatalf("解析 nodes 失败：%v", err)
	}
	if err := json.Unmarshal(wf.Edges, &edges); err != nil {
		t.Fatalf("解析 edges 失败：%v", err)
	}

	if HasBlockingIssue(ValidateDAG(nodes, edges)) {
		t.Fatalf("canonical 图有阻断级结构问题：%v", FirstBlocking(ValidateDAG(nodes, edges)))
	}

	sc := ScoreGraph(nodes, edges)
	if len(sc.PortMismatches) > 1 {
		t.Fatalf("端口不匹配只允许 start→shotlist 这一条已知例外，实际 %d 条：%v",
			len(sc.PortMismatches), sc.PortMismatches)
	}
	if len(sc.RequiredNoFeed) > 0 {
		t.Fatalf("必填输入没有来源：%v", sc.RequiredNoFeed)
	}
	if len(sc.Cycles) > 0 {
		t.Fatalf("有成环：%v", sc.Cycles)
	}

	// 汇合点纪律：quality_gate 必须同时等一致性门与后期（任一门 FAIL 都到不了交付）
	indeg := map[string]int{}
	for _, e := range edges {
		if e.Target == "qg-1" {
			indeg[e.Source]++
		}
	}
	if indeg["cg-1"] == 0 || indeg["pp-1"] == 0 {
		t.Fatalf("quality_gate 必须同时吃 consistency_gate 与 post_production 两条线，实际入边来源：%v", indeg)
	}
}

// 门牙：违约的 plan 必须在开机前被拦下（enterSide/deps.Start 之前），
// 且错误信息要说清是五层门预检——不是跑到一半显存烧完了才炸。
func TestShotListGateBlocksViolatingPlanBeforeStart(t *testing.T) {
	good := loadCanonicalWorkflow(t)
	rawPlan, _ := canonicalPlanOf(t, good)

	cases := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{"缺 gates（require_gates=true 必须拦）", func(p map[string]interface{}) {
			delete(p, "gates")
		}},
		{"单镜过短（20 帧 = 0.83s < 1.2s 下限）", func(p map[string]interface{}) {
			segs := p["segments"].([]interface{})
			segs[0].(map[string]interface{})["frames"] = float64(20)
		}},
		{"台词与提示词对不上", func(p map[string]interface{}) {
			segs := p["segments"].([]interface{})
			lines := segs[0].(map[string]interface{})["lines"].([]interface{})
			lines[0].(map[string]interface{})["text"] = "你输了。"
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var plan map[string]interface{}
			if err := json.Unmarshal([]byte(rawPlan), &plan); err != nil {
				t.Fatal(err)
			}
			c.mutate(plan)
			bad, _ := json.Marshal(plan)

			e := &EngineV2{} // deps=nil：门检若在开机前，根本走不到服务
			node := &model.NodeInstance{
				ID: "sl-1", Type: "shotlist",
				Data: map[string]interface{}{
					"plan":          string(bad),
					"backend":       "h3",
					"require_gates": true,
				},
			}
			_, err := e.execShotListNode(nil, node, map[string]interface{}{})
			if err == nil {
				t.Fatal("违约 plan 必须被拦下")
			}
			if !strings.Contains(err.Error(), "五层门预检未通过") {
				t.Fatalf("错误信息应指明五层门预检（未开机），got %v", err)
			}
			if strings.Contains(err.Error(), "panic") {
				t.Fatal("不该 panic：门检在任何服务调用之前")
			}
		})
	}
}
