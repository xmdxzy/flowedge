package service

import (
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 候选生成最容易犯的错不是"漏提议"，是"乱提议"：
// 提议一个改了没用、或者根本改不了的参数，会让人再也不看这些建议。
// 所以测试重心在"不该提议的时候保持沉默"。

func cnode(id, typ string, data map[string]interface{}) model.NodeInstance {
	return model.NodeInstance{ID: id, Type: typ, Data: data}
}

func gateWeakDiag(gate string) Diagnosis {
	return Diagnosis{
		Severity: "P1", Kind: "gate_weak",
		Target:  "修士斗法 / " + gate,
		Message: "门检查「" + gate + "」失败率偏高",
		Metric:  gate + ".failRate=0.44",
	}
}

// 响度门弱 + 图里 loudness=-20 → 提议改到 -14
func TestCandidate_LoudnessHasStandard(t *testing.T) {
	nodes := []model.NodeInstance{
		cnode("pp", "post_production", map[string]interface{}{"loudness": float64(-20)}),
	}
	out := ProposeCandidates(gateWeakDiag("响度"), 39, "wf", nodes)
	if len(out) == 0 {
		t.Fatal("应提议响度归一")
	}
	c := out[0]
	if c.Confidence != "high" {
		t.Fatalf("有行业标准的项应标 high，实际 %s", c.Confidence)
	}
	if len(c.Edits) != 1 || c.Edits[0].Path != "data.loudness" {
		t.Fatalf("编辑不对：%+v", c.Edits)
	}
	if c.Edits[0].To != -14 {
		t.Fatalf("目标值应为 -14，实际 %v", c.Edits[0].To)
	}
	// from 必须是从图里读出来的真值
	if c.Edits[0].From != float64(-20) {
		t.Fatalf("from 应为现场值 -20，实际 %v", c.Edits[0].From)
	}
	if c.Basis == "" || c.Expected == "" || c.Risk == "" {
		t.Fatal("候选必须写清依据/预期指标/风险")
	}
}

// 已经是标准值 → 不提议（否则每次诊断都跳同一条，人会关掉它）
func TestCandidate_NoProposalWhenAlreadyStandard(t *testing.T) {
	nodes := []model.NodeInstance{
		cnode("pp", "post_production", map[string]interface{}{"loudness": float64(-14)}),
	}
	out := ProposeCandidates(gateWeakDiag("响度"), 39, "wf", nodes)
	if len(out) != 0 {
		t.Fatalf("已在标准值不该提议：%+v", out)
	}
}

// 图里没有这个键 → 不提议。**不能主动新增参数**（那等于替用户做决定，且 from 无从核对）
func TestCandidate_NoProposalOnMissingKey(t *testing.T) {
	nodes := []model.NodeInstance{
		cnode("pp", "post_production", map[string]interface{}{"aspect": "9:16"}),
	}
	out := ProposeCandidates(gateWeakDiag("响度"), 39, "wf", nodes)
	if len(out) != 0 {
		t.Fatalf("键不存在时不该提议新增：%+v", out)
	}
}

// 环境类诊断不产生"改图"的候选 —— 改参数只会掩盖症状
func TestCandidate_NoProposalForEnvIssues(t *testing.T) {
	nodes := []model.NodeInstance{
		cnode("pp", "post_production", map[string]interface{}{"loudness": float64(-20)}),
	}
	cases := []Diagnosis{
		{Kind: "env_unstable", Target: "全部", Message: "不可测占比 40%"},
		{Kind: "long_tail", Target: "wf", Message: "p95 是均值 3 倍"},
		{Kind: "evidence_missing", Target: "全部", Message: "没有门明细"},
	}
	for _, d := range cases {
		if out := ProposeCandidates(d, 39, "wf", nodes); len(out) != 0 {
			t.Fatalf("%s 不该产生改图候选：%+v", d.Kind, out)
		}
	}
}

// 流水线失败率过半 → 给一条"先做结构检查"的候选（不改参数）
func TestCandidate_PipelineWeakPointsToStructure(t *testing.T) {
	nodes := []model.NodeInstance{cnode("s", "start", nil)}
	out := ProposeCandidates(Diagnosis{Kind: "pipeline_weak", Target: "wf", Message: "失败率 66%"}, 39, "wf", nodes)
	if len(out) != 1 {
		t.Fatalf("应给 1 条结构检查候选，实际 %d", len(out))
	}
	if len(out[0].Edits) != 0 {
		t.Fatal("结构检查候选不该带参数改动")
	}
	if !strings.Contains(out[0].Basis, "graph/score") {
		t.Fatalf("要指出去哪看结构分，实际：%s", out[0].Basis)
	}
}

// 多个同类型节点都要改 → 全部列出（不能只改第一个）
func TestCandidate_EditsAllMatchingNodes(t *testing.T) {
	nodes := []model.NodeInstance{
		cnode("g1", "quality_gate", map[string]interface{}{"require_audio": false}),
		cnode("g2", "quality_gate", map[string]interface{}{"require_audio": false}),
	}
	out := ProposeCandidates(gateWeakDiag("音轨"), 1, "wf", nodes)
	if len(out) == 0 {
		t.Fatal("应提议打开音轨校验")
	}
	if len(out[0].Edits) != 2 {
		t.Fatalf("两个质检门都应改，实际 %d 条", len(out[0].Edits))
	}
}

// 转成 spec/apply 入参后，四条硬拦照样生效（复用同一条写路径）
func TestCandidate_ToSpecEditsCompatible(t *testing.T) {
	c := TrialCandidate{Edits: []CandidateEdit{
		{NodeID: "n1", Path: "data.fps", From: float64(24), To: float64(25), Why: "对齐标准"},
	}}
	specs := c.ToSpecEdits()
	if len(specs) != 1 || specs[0].Path != "data.fps" || specs[0].Reason != "对齐标准" {
		t.Fatalf("转换不对：%+v", specs)
	}
	// fps 是门层参数：默认被锁（这是对的），带 AllowGateChange 才能应用。
	// 候选的 Why 会作为 Reason 传下去，满足"改门必须留名"。
	nodes := []model.NodeInstance{cnode("n1", "text2video", map[string]interface{}{"fps": float64(24)})}

	_, _, lockedProbs := ApplySpecEdits(nodes, specs, ApplyOptions{})
	if len(lockedProbs) != 1 || lockedProbs[0].Code != EditGateLocked {
		t.Fatalf("不带 AllowGateChange 时门参数必须被锁：%+v", lockedProbs)
	}

	_, applied, probs := ApplySpecEdits(nodes, specs, ApplyOptions{AllowGateChange: true})
	if len(probs) != 0 || len(applied) != 1 {
		t.Fatalf("带授权后应能应用：probs=%+v applied=%d", probs, len(applied))
	}
}
