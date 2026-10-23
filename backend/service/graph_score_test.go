package service

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 结构评分是用来**淘汰**的，不是用来排名的：
// 它要说清"这张图哪儿不能跑"，而不只是给个数字。
// 所以测试重点是每个分项能不能单独触发、以及 Legal 会不会误判。

func gnode(id, typ string) model.NodeInstance {
	return model.NodeInstance{ID: id, Type: typ, Data: map[string]interface{}{}}
}

func gedge(id, src, dst string) model.EdgeInstance {
	return model.EdgeInstance{ID: id, Source: src, Target: dst}
}

// 一条健康的图：全契约、端口通、接线完整、无环 → 满分 + 可执行
func TestGraphScore_HealthyGraphFullScore(t *testing.T) {
	nodes := []model.NodeInstance{gnode("s", "start"), gnode("l", "llm"), gnode("e", "end")}
	edges := []model.EdgeInstance{gedge("a", "s", "l"), gedge("b", "l", "e")}

	s := ScoreGraph(nodes, edges)
	if !s.Legal {
		t.Fatalf("健康图应可执行，实际：%s", s.Summarize())
	}
	if math.Abs(s.Score-1.0) > 1e-9 {
		t.Fatalf("应得满分，实际 %.4f（%+v）", s.Score, s)
	}
	if s.Layers != 3 {
		t.Fatalf("三层，实际 %d", s.Layers)
	}
}

// 未声明契约的模块 → 契约覆盖下降 + Legal=false（未知类型连端口都不知道怎么接）
func TestGraphScore_UnknownModuleBreaksLegal(t *testing.T) {
	nodes := []model.NodeInstance{gnode("s", "start"), gnode("x", "no_such_node"), gnode("e", "end")}
	edges := []model.EdgeInstance{gedge("a", "s", "x"), gedge("b", "x", "e")}

	s := ScoreGraph(nodes, edges)
	if s.Legal {
		t.Fatal("有未声明模块时不该判可执行")
	}
	if len(s.ContractMissing) != 1 || !strings.Contains(s.ContractMissing[0], "no_such_node") {
		t.Fatalf("要点名是哪个节点：%v", s.ContractMissing)
	}
	if s.ContractCoverage >= 1 {
		t.Fatalf("契约覆盖应低于 1，实际 %.2f", s.ContractCoverage)
	}
}

// 端口不兼容的边 → 记一条带类型的明细（"text→image" 比"有 1 条错误边"有用得多）
func TestGraphScore_PortMismatchReported(t *testing.T) {
	// start 输出 text，text2image 的输入是 image/text（吃 text，能通）；
	// 反过来把 end（吃 any）接成源就构造不出错，所以这里用 start → upscale（要 image）
	nodes := []model.NodeInstance{gnode("s", "start"), gnode("u", "upscale")}
	edges := []model.EdgeInstance{gedge("a", "s", "u")}

	s := ScoreGraph(nodes, edges)
	// upscale 的 image 输入可能声明为非必填，端口判定才是关键
	if len(s.PortMismatches) > 0 {
		if !strings.Contains(s.PortMismatches[0], "→") {
			t.Fatalf("明细要写成 源→目标：%v", s.PortMismatches[0])
		}
		if s.Legal {
			t.Fatal("端口冲突时不该判可执行")
		}
	}
	if s.PortCompatibility > 1 || s.PortCompatibility < 0 {
		t.Fatalf("端口率越界：%.2f", s.PortCompatibility)
	}
}

// 孤立节点：**报告但不扣分**。
//
// 定标时（2026-10-23）发现 wf2/wf3 两条跑通过的真实图里，tts 都没有入边 ——
// 它的文本写在参数里，是合法接线。给孤立扣分会让它永远拿不到满分，评分器就失去定标意义。
// 只有「必填输入没有来源」才扣分（见下一个用例）。
func TestGraphScore_OrphanReportedNotPenalized(t *testing.T) {
	nodes := []model.NodeInstance{gnode("s", "start"), gnode("l", "llm"), gnode("e", "end")}
	edges := []model.EdgeInstance{gedge("a", "s", "l")} // e 没有入边

	s := ScoreGraph(nodes, edges)
	if len(s.Orphans) != 1 || !strings.Contains(s.Orphans[0], "e(") {
		t.Fatalf("应点名孤立节点 e：%v", s.Orphans)
	}
	if s.Wiring != 1.0 {
		t.Fatalf("孤立（输入可选）不该扣接线分，实际 %.2f", s.Wiring)
	}
}

// 必填输入没有来源 → 扣分并点名。这才是真正"跑不起来"的接线缺陷。
func TestGraphScore_RequiredInputWithoutSource(t *testing.T) {
	// quality_gate 的 any 输入是 required；把 end 直接接到它下游、不给它入边
	nodes := []model.NodeInstance{gnode("s", "start"), gnode("g", "quality_gate"), gnode("e", "end")}
	edges := []model.EdgeInstance{gedge("a", "g", "e")} // g 无入边

	s := ScoreGraph(nodes, edges)
	if len(s.RequiredNoFeed) == 0 {
		t.Fatalf("应报必填无源，实际：%+v", s.RequiredNoFeed)
	}
	if s.Wiring >= 1 {
		t.Fatalf("必填无源必须扣接线分，实际 %.2f", s.Wiring)
	}
}

// 成环 → Acyclic=0 且 Legal=false，并列出环上的节点
func TestGraphScore_CycleDetected(t *testing.T) {
	nodes := []model.NodeInstance{gnode("s", "start"), gnode("a", "llm"), gnode("b", "llm")}
	edges := []model.EdgeInstance{
		gedge("e1", "s", "a"),
		gedge("e2", "a", "b"),
		gedge("e3", "b", "a"), // 环
	}
	s := ScoreGraph(nodes, edges)
	if s.Acyclic != 0 {
		t.Fatalf("应检到环，acyclic=%.0f", s.Acyclic)
	}
	if s.Legal {
		t.Fatal("成环不可执行")
	}
	if len(s.Cycles) == 0 {
		t.Fatal("要列出环上的节点，否则用户不知道从哪断")
	}
}

// 单节点图不该被"没有边"扣分（无边时端口率记 1）
func TestGraphScore_SingleNodeNoEdgeNoPenalty(t *testing.T) {
	s := ScoreGraph([]model.NodeInstance{gnode("s", "start")}, nil)
	if s.PortCompatibility != 1 {
		t.Fatalf("无边时端口率应为 1，实际 %.2f", s.PortCompatibility)
	}
	if !s.Legal {
		t.Fatalf("单起点应可执行：%s", s.Summarize())
	}
}

// 空图不 panic，返回 0 分（而不是 NullPointer）
func TestGraphScore_EmptyGraphSafe(t *testing.T) {
	s := ScoreGraph(nil, nil)
	if s.Score != 0 || s.Legal {
		t.Fatalf("空图应 0 分且不可执行：%+v", s)
	}
	if s.Summarize() == "" {
		t.Fatal("摘要不能为空串")
	}
}

// 权重总和必须是 1（改了权重忘记配平，分数会整体漂移）
func TestGraphScoreWeightsSumToOne(t *testing.T) {
	sum := graphScoreWeights.Contract + graphScoreWeights.Ports + graphScoreWeights.Wiring + graphScoreWeights.Acyclic
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("权重和应为 1，实际 %.4f", sum)
	}
}

// ===== L1：结构相似度（需要配对）=====

func shapeFromJSON(t *testing.T, nodesJSON, edgesJSON string) GraphShape {
	t.Helper()
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	if err := json.Unmarshal([]byte(nodesJSON), &nodes); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(edgesJSON), &edges); err != nil {
		t.Fatal(err)
	}
	return ShapeOf(nodes, edges)
}

// 完全一致 → F1 = 1
func TestStructureF1_Identical(t *testing.T) {
	nodes := `[{"id":"a","type":"start"},{"id":"b","type":"llm"},{"id":"c","type":"end"}]`
	edges := `[{"id":"1","source":"a","target":"b"},{"id":"2","source":"b","target":"c"}]`
	sh := shapeFromJSON(t, nodes, edges)
	n, e, _ := StructureF1(sh, sh)
	if n != 1 || e != 1 {
		t.Fatalf("自比应 1/1，实际 %.2f/%.2f", n, e)
	}
}

// 少一个节点 → Node F1 < 1，且明细里点是缺了哪个类型
func TestStructureF1_MissingNode(t *testing.T) {
	got := shapeFromJSON(t,
		`[{"id":"a","type":"start"},{"id":"c","type":"end"}]`,
		`[{"id":"1","source":"a","target":"c"}]`)
	want := shapeFromJSON(t,
		`[{"id":"a","type":"start"},{"id":"b","type":"llm"},{"id":"c","type":"end"}]`,
		`[{"id":"1","source":"a","target":"b"},{"id":"2","source":"b","target":"c"}]`)

	n, e, detail := StructureF1(got, want)
	if n >= 1 {
		t.Fatalf("缺节点时 Node F1 应 <1，实际 %.2f", n)
	}
	miss := detail["missingNodes"].([]string)
	if len(miss) == 0 || !strings.Contains(strings.Join(miss, ","), "llm") {
		t.Fatalf("要指出缺的是 llm：%v", miss)
	}
	if e >= 1 {
		t.Fatalf("缺边时 Edge F1 应 <1，实际 %.2f", e)
	}
}

// 多画的算 FP（画蛇添足也要被看见，否则"越复杂越好"会成为生成策略）
func TestStructureF1_ExtraNodesCounted(t *testing.T) {
	got := shapeFromJSON(t,
		`[{"id":"a","type":"start"},{"id":"x","type":"upscale"},{"id":"c","type":"end"}]`,
		`[{"id":"1","source":"a","target":"x"},{"id":"2","source":"x","target":"c"}]`)
	want := shapeFromJSON(t,
		`[{"id":"a","type":"start"},{"id":"c","type":"end"}]`,
		`[{"id":"1","source":"a","target":"c"}]`)

	_, _, detail := StructureF1(got, want)
	extra := detail["extraNodes"].([]string)
	if len(extra) == 0 || !strings.Contains(strings.Join(extra, ","), "upscale") {
		t.Fatalf("要指出多画了 upscale：%v", extra)
	}
}

// 多重集语义：同类节点数量不同要能反映出来（不是集合去重）
func TestStructureF1_Multiplicity(t *testing.T) {
	got := shapeFromJSON(t, `[{"id":"a","type":"text2image"},{"id":"b","type":"text2image"}]`, `[]`)
	want := shapeFromJSON(t, `[{"id":"a","type":"text2image"},{"id":"b","type":"text2image"},{"id":"c","type":"text2image"}]`, `[]`)

	n, _, _ := StructureF1(got, want)
	if n != 0.8 { // tp=2, fp=0, fn=1 → P=1, R=2/3 → F1=0.8
		t.Fatalf("应为 0.8，实际 %.3f", n)
	}
}

// 两边都空 → 0 而非 NaN（NaN 会让报告整个失效）
func TestStructureF1_EmptyIsZeroNotNaN(t *testing.T) {
	empty := GraphShape{NodeTypes: map[string]int{}, EdgePairs: map[string]int{}}
	n, e, _ := StructureF1(empty, empty)
	if math.IsNaN(n) || math.IsNaN(e) || n != 0 || e != 0 {
		t.Fatalf("空对空应为 0/0，实际 %v/%v", n, e)
	}
}
