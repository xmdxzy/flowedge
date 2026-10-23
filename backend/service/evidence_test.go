package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"flowedge/backend/model"
)

// 证据信封的测试重点不是"字段能不能序列化"，而是**三态判定不能退化成二态**：
// 一旦 unscored 被写成 false，"服务没起导致没测"就会被算成"产出不合格"，
// 整个有/无引擎的对比结论会反向。所以每条用例都盯 Passed 指针是否为 nil。

func gateState(id, status string, checks ...GateCheckEvidence) model.NodeInstance {
	out := map[string]interface{}{"gate": "pass", "checks": checks}
	return model.NodeInstance{ID: id, Type: "quality_gate", Status: status, Output: out}
}

func runOf(status string, started, ended *time.Time) model.WorkflowRun {
	return model.WorkflowRun{ID: 7, WorkflowID: 3, Status: status, StartedAt: started, EndedAt: ended}
}

// 成功 + 有门 + 全过 → passed，Passed 必须是 true（不是 nil）
func TestEvidence_SuccessWithAllGatesPassed(t *testing.T) {
	states := []model.NodeInstance{
		gateState("g1", "success",
			GateCheckEvidence{Name: "帧数", OK: true, Level: "fail"},
			GateCheckEvidence{Name: "响度", OK: true, Level: "fail"}),
	}
	ev := BuildEvidence(runOf("success", nil, nil), states, 0, 0)

	if ev.Result != EvidencePassed {
		t.Fatalf("result = %q, want passed", ev.Result)
	}
	if ev.Passed == nil || !*ev.Passed {
		t.Fatalf("passed 应为 true，实际 %v", ev.Passed)
	}
	if ev.GateScore == nil || *ev.GateScore != 1.0 {
		t.Fatalf("gateScore 应为 1.0，实际 %v", ev.GateScore)
	}
}

// 成功但根本没有门 → unscored，不是 passed。
// 这是「门必须有牙齿」的延伸：没验收就算通过，等于奖励拆门。
func TestEvidence_SuccessWithoutGateIsUnscored(t *testing.T) {
	states := []model.NodeInstance{
		{ID: "llm", Type: "llm", Status: "success", Output: map[string]interface{}{"text": "hi"}},
	}
	ev := BuildEvidence(runOf("success", nil, nil), states, 0, 0)

	if ev.Result != EvidenceUnscored {
		t.Fatalf("result = %q, want unscored", ev.Result)
	}
	if ev.Passed != nil {
		t.Fatalf("没有门时 passed 必须为 nil（未测到），实际 %v", *ev.Passed)
	}
	if ev.UnscoredReason != "no_gate" {
		t.Fatalf("reason = %q, want no_gate", ev.UnscoredReason)
	}
	if ev.GateScore != nil {
		t.Fatalf("没有门时 gateScore 必须为 nil，实际 %v", *ev.GateScore)
	}
}

// 失败且门判不过 → failed，Passed 明确是 false（测了，不合格）
func TestEvidence_GateFailedIsFailed(t *testing.T) {
	states := []model.NodeInstance{
		gateState("g1", "failed",
			GateCheckEvidence{Name: "帧数", OK: true, Level: "fail"},
			GateCheckEvidence{Name: "音轨", OK: false, Level: "fail"}),
	}
	ev := BuildEvidence(runOf("failed", nil, nil), states, 0, 0)

	if ev.Result != EvidenceFailed {
		t.Fatalf("result = %q, want failed", ev.Result)
	}
	if ev.Passed == nil || *ev.Passed {
		t.Fatalf("门判不过时 passed 应为 false，实际 %v", ev.Passed)
	}
	// 部分通过 = 1/2，这正是 Partial 分的价值：知道"差在哪一项"
	if ev.GateScore == nil || *ev.GateScore != 0.5 {
		t.Fatalf("gateScore 应为 0.5，实际 %v", ev.GateScore)
	}
}

// 失败但没走到门（依赖没起 / 进程崩）→ unscored。
// 记 failed 会把"没测到"甩锅成"引擎产出不合格"。
func TestEvidence_ExecutionErrorIsUnscoredNotFailed(t *testing.T) {
	states := []model.NodeInstance{
		{ID: "t2i", Type: "text2image", Status: "failed", Error: "connection refused"},
	}
	ev := BuildEvidence(runOf("failed", nil, nil), states, 0, 0)

	if ev.Result != EvidenceUnscored {
		t.Fatalf("result = %q, want unscored", ev.Result)
	}
	if ev.Passed != nil {
		t.Fatalf("执行期故障时 passed 必须为 nil，实际 %v", *ev.Passed)
	}
	if ev.UnscoredReason != "execution_error" {
		t.Fatalf("reason = %q, want execution_error", ev.UnscoredReason)
	}
}

// 门自己崩了（缺输入产物 / ffprobe 不可达）→ unscored/gate_error，不是 failed。
//
// 这一条是本文件最容易写错的地方：门的"执行崩溃"和"判定不过"在 run 上
// 都是 status=failed，合并成一个布尔就会把"没测到"记成"产出不合格"。
// 判据是门有没有留下 checks —— 跑完了必然留痕。
func TestEvidence_GateCrashIsUnscoredNotFailed(t *testing.T) {
	// 场景一：门节点 failed 且没有任何 checks（连探测都没做成就挂了）
	crashed := []model.NodeInstance{
		{ID: "g1", Type: "quality_gate", Status: "failed", Error: "质检门缺少输入产物"},
	}
	ev := BuildEvidence(runOf("failed", nil, nil), crashed, 0, 0)
	if ev.Result != EvidenceUnscored {
		t.Fatalf("门崩溃 result = %q, want unscored", ev.Result)
	}
	if ev.Passed != nil {
		t.Fatalf("门崩溃时 passed 必须为 nil，实际 %v", *ev.Passed)
	}
	if ev.UnscoredReason != "gate_error" {
		t.Fatalf("reason = %q, want gate_error", ev.UnscoredReason)
	}

	// 场景二：门节点 failed 但输出里有 checks → 这才是"测了不合格"
	judged := []model.NodeInstance{
		gateState("g1", "failed", GateCheckEvidence{Name: "音轨", OK: false, Level: "fail"}),
	}
	ev2 := BuildEvidence(runOf("failed", nil, nil), judged, 0, 0)
	if ev2.Result != EvidenceFailed {
		t.Fatalf("门判不过 result = %q, want failed", ev2.Result)
	}
}

// 门过了但下游别的节点崩了 → unscored，不是 passed 也不是 failed：
// 门只证明了它看过的那件产物，整条流程没走完，产物完整性未知。
func TestEvidence_GatePassedButDownstreamFailed(t *testing.T) {
	states := []model.NodeInstance{
		{ID: "g1", Type: "quality_gate", Status: "success",
			Output: map[string]interface{}{"checks": []GateCheckEvidence{{Name: "帧数", OK: true, Level: "fail"}}}},
		{ID: "x", Type: "video_compose", Status: "failed", Error: "ffmpeg 崩了"},
	}
	ev := BuildEvidence(runOf("failed", nil, nil), states, 0, 0)
	if ev.Result != EvidenceUnscored {
		t.Fatalf("result = %q, want unscored", ev.Result)
	}
	if ev.Passed != nil {
		t.Fatalf("passed 必须为 nil，实际 %v", *ev.Passed)
	}
}

// 主动中止 / 暂停都是不计分，但要在 result 上可区分（排查时是完全不同的事实）
func TestEvidence_CancelledAndPausedAreDistinct(t *testing.T) {
	states := []model.NodeInstance{gateState("g1", "success", GateCheckEvidence{Name: "帧数", OK: true, Level: "fail"})}

	c := BuildEvidence(runOf("cancelled", nil, nil), states, 0, 0)
	if c.Result != EvidenceCancelled || c.Passed != nil {
		t.Fatalf("cancelled: result=%q passed=%v", c.Result, c.Passed)
	}
	p := BuildEvidence(runOf("paused", nil, nil), states, 0, 0)
	if p.Result != EvidencePaused || p.Passed != nil {
		t.Fatalf("paused: result=%q passed=%v", p.Result, p.Passed)
	}
}

// token 两种形态都要累加：内存里是 struct，持久化再读回是 map。
// 只处理一种就会出现"重跑后 token 归零"的假数据。
func TestEvidence_TokenSumBothShapes(t *testing.T) {
	states := []model.NodeInstance{
		// 形态一：结构体（刚执行完，内存对象）
		{ID: "a", Type: "llm", Status: "success",
			Output: map[string]interface{}{"text": "x", "_usage": TokenUsage{PromptTokens: 100, CompletionTokens: 20}}},
		// 形态二：JSON 往返后的 map（node_states 读回来）
		{ID: "b", Type: "llm", Status: "success",
			Output: map[string]interface{}{"text": "y", "_usage": map[string]interface{}{
				"prompt_tokens": float64(30), "completion_tokens": float64(5),
			}}},
	}
	u := UsageOfNodes(states)
	if u.Total() != 155 {
		t.Fatalf("token 合计应为 155，实际 prompt=%d completion=%d", u.PromptTokens, u.CompletionTokens)
	}
}

// 未采集（-1）与采到了 0 必须分开：一个记 null，一个记 0
func TestEvidence_HumanInterventionsNullVsZero(t *testing.T) {
	states := []model.NodeInstance{gateState("g1", "success", GateCheckEvidence{Name: "帧数", OK: true, Level: "fail"})}

	unknown := BuildEvidence(runOf("success", nil, nil), states, 0, -1)
	if unknown.HumanInterventions != nil {
		t.Fatalf("未采集时应为 nil，实际 %d", *unknown.HumanInterventions)
	}

	zero := BuildEvidence(runOf("success", nil, nil), states, 0, 0)
	if zero.HumanInterventions == nil || *zero.HumanInterventions != 0 {
		t.Fatalf("采集到 0 时应为 0，实际 %v", zero.HumanInterventions)
	}
}

// 耗时按起止时间算；缺任一端就是 0（不猜）
func TestEvidence_DurationFromTimestamps(t *testing.T) {
	s := time.Now()
	e := s.Add(90 * time.Second)
	states := []model.NodeInstance{gateState("g1", "success", GateCheckEvidence{Name: "帧数", OK: true, Level: "fail"})}
	ev := BuildEvidence(runOf("success", &s, &e), states, 2, 0)

	if ev.DurationMS != 90000 {
		t.Fatalf("durationMs = %d, want 90000", ev.DurationMS)
	}
	if ev.SideUnloadCount != 2 {
		t.Fatalf("sideUnloadCount = %d, want 2", ev.SideUnloadCount)
	}
	// 峰值显存恒 null：timeline 没有 run 关联，猜一个时间窗会在并发时张冠李戴
	if ev.PeakVramMB != nil {
		t.Fatalf("peakVramMb 当前必须为 nil，实际 %d", *ev.PeakVramMB)
	}
}

// 落盘：原子写、可读回、schema 版本与三态字段都在
func TestEvidence_WriteAndReadBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EVIDENCE_DIR", dir)

	states := []model.NodeInstance{gateState("g1", "success", GateCheckEvidence{Name: "帧数", OK: true, Level: "fail"})}
	ev := BuildEvidence(jsonRun(), states, 1, 0)

	path := WriteEvidence(ev)
	if path == "" {
		t.Fatal("WriteEvidence 返回空路径")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	var got Evidence
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("信封不是合法 JSON：%v", err)
	}
	if got.SchemaVersion != EvidenceSchemaVersion {
		t.Fatalf("schemaVersion = %d", got.SchemaVersion)
	}
	if got.CaseID != nil || got.DatasetFingerprint != nil {
		t.Fatal("引擎不知道 caseId / fingerprint，必须是 null 而不是编一个")
	}
	// 临时文件不能残留
	if _, err := os.Stat(filepath.Join(dir, "run-7.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("临时文件应已 rename 掉")
	}
}

func jsonRun() model.WorkflowRun {
	now := time.Now()
	ended := now.Add(time.Second)
	return model.WorkflowRun{ID: 7, WorkflowID: 3, Status: "success", StartedAt: &now, EndedAt: &ended}
}

// 评测语义必须从 run 原样搬进信封：caseId / role / 版本。
//
// 这三个字段是配对判定的主键。搬丢了，一次"改前改后"就退化成两条互不相干
// 的运行记录 —— 判定会给出 inconclusive，而不是给人一个错误结论（好一点），
// 但整条自进化链就断在这里（更常见）。
func TestEvidence_CarriesEvalSemanticsFromRun(t *testing.T) {
	caseID, cat, fp := "case-07", "short", "ds-2026-10-23"
	run := model.WorkflowRun{
		ID: 42, WorkflowID: 7, Status: "success",
		CaseID: &caseID, Category: &cat, DatasetFingerprint: &fp,
		Role: "candidate", WorkflowVersion: 4, InvokedBy: "evaluate",
	}
	states := []model.NodeInstance{
		gateState("g1", "success", GateCheckEvidence{Name: "min_frames", OK: true, Level: "fail"}),
	}
	ev := BuildEvidence(run, states, 0, 0)
	if ev.CaseID == nil || *ev.CaseID != caseID {
		t.Fatalf("caseId 必须搬过来，实得 %v", ev.CaseID)
	}
	if ev.Category == nil || *ev.Category != cat {
		t.Fatalf("category 必须搬过来，实得 %v", ev.Category)
	}
	if ev.DatasetFingerprint == nil || *ev.DatasetFingerprint != fp {
		t.Fatalf("datasetFingerprint 必须搬过来，实得 %v", ev.DatasetFingerprint)
	}
	if ev.Role != "candidate" {
		t.Fatalf("role 必须搬过来，实得 %q", ev.Role)
	}
	if ev.WorkflowVersion != 4 {
		t.Fatalf("版本号必须用 run 上钉住的那个（4），实得 %d", ev.WorkflowVersion)
	}
	if ev.InvokedBy != "evaluate" {
		t.Fatalf("invokedBy 应记 evaluate，实得 %q", ev.InvokedBy)
	}
}
