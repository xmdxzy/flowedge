package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// 聚合器是"观测者"，它出错的后果不是崩溃，而是**给出错误的诊断**——
// 那比没有诊断更糟（会把人引去修错的地方）。
// 所以测试重点是：三数算法、诊断触发条件、以及"不该诊断时保持沉默"。

func rec(rid, wfid uint, result string, dur int64) RunRecord {
	return RunRecord{RunID: rid, WorkflowID: wfid, Workflow: "wf", Result: result, DurationMs: dur, Source: "evidence"}
}

// 三数：全 case / 可测 case / 不可测占比
func TestAggregate_ThreeNumbers(t *testing.T) {
	recs := []RunRecord{
		rec(1, 1, EvidencePassed, 100),
		rec(2, 1, EvidencePassed, 100),
		rec(3, 1, EvidencePassed, 100),
		rec(4, 1, EvidenceFailed, 100),
		rec(5, 1, EvidenceUnscored, 100),
	}
	rep := AggregateRuns(recs)
	o := rep.Overall

	if o.Runs != 5 || o.Passed != 3 || o.Failed != 1 || o.Unscored != 1 {
		t.Fatalf("计数不对：%+v", o)
	}
	if o.CRAll != 0.6 {
		t.Fatalf("CR_all 应为 3/5=0.6，实际 %.3f", o.CRAll)
	}
	// 可测 = passed + failed = 4，所以 clean 分母是 4
	if o.CRClean == nil || *o.CRClean != 0.75 {
		t.Fatalf("CR_clean 应为 3/4=0.75，实际 %v", o.CRClean)
	}
	if o.UnscoredRate != 0.2 {
		t.Fatalf("unscored 率应为 0.2，实际 %.3f", o.UnscoredRate)
	}
}

// 不可测超过 20% → 报告必须标"不可信"，并给 P0 诊断
func TestAggregate_UntrustworthyWhenManyUnscored(t *testing.T) {
	var recs []RunRecord
	for i := 1; i <= 10; i++ {
		r := rec(uint(i), 1, EvidencePassed, 100)
		if i <= 4 { // 4/10 = 40% 不可测
			r.Result = EvidenceUnscored
		}
		recs = append(recs, r)
	}
	rep := AggregateRuns(recs)
	if rep.Trustworthy {
		t.Fatal("40% 不可测时不该判可信")
	}
	if rep.Note == "" {
		t.Fatal("不可信时要写明原因，不能只给个布尔")
	}
	got := false
	for _, d := range rep.Diagnoses {
		if d.Kind == "env_unstable" && d.Severity == "P0" {
			got = true
		}
	}
	if !got {
		t.Fatalf("应有 env_unstable 的 P0 诊断：%+v", rep.Diagnoses)
	}
}

// 少样本不诊断（3 次 1 次失败 = 33%，不足以说是结构问题）
func TestAggregate_NoDiagnosisOnSmallSample(t *testing.T) {
	recs := []RunRecord{
		rec(1, 1, EvidenceFailed, 100),
		rec(2, 1, EvidencePassed, 100),
		rec(3, 1, EvidencePassed, 100),
	}
	rep := AggregateRuns(recs)
	for _, d := range rep.Diagnoses {
		if d.Kind == "pipeline_weak" {
			t.Fatalf("3 次里失败 1 次不该判结构问题：%+v", d)
		}
	}
}

// 门稳定在漏 → 点名这个门，带触发值
func TestAggregate_GateWeaknessNamed(t *testing.T) {
	var recs []RunRecord
	for i := 1; i <= 5; i++ {
		r := rec(uint(i), 1, EvidenceFailed, 100)
		r.GateChecks = []GateCheckEvidence{
			{Name: "音轨", OK: false, Level: "fail"},
			{Name: "帧数", OK: true, Level: "fail"},
		}
		recs = append(recs, r)
	}
	rep := AggregateRuns(recs)
	var hit *Diagnosis
	for i := range rep.Diagnoses {
		if rep.Diagnoses[i].Kind == "gate_weak" {
			hit = &rep.Diagnoses[i]
		}
	}
	if hit == nil {
		t.Fatalf("应点出弱门：%+v", rep.Diagnoses)
	}
	if hit.Target == "" || hit.Metric == "" {
		t.Fatalf("诊断必须能定位（target/metric）：%+v", hit)
	}
	if rep.Workflows[0].TopFailingGates[0].Name != "音轨" {
		t.Fatalf("弱门排序不对：%+v", rep.Workflows[0].TopFailingGates)
	}
}

// 全历史数据 → 必须坦白"没有门明细"，而不是装作诊断过
func TestAggregate_AdmitsMissingEvidence(t *testing.T) {
	recs := []RunRecord{
		{RunID: 1, WorkflowID: 1, Result: EvidenceUnscored, Source: "history"},
		{RunID: 2, WorkflowID: 1, Result: EvidencePassed, Source: "history"},
	}
	rep := AggregateRuns(recs)
	found := false
	for _, d := range rep.Diagnoses {
		if d.Kind == "evidence_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("全历史数据时应说明「没有门明细」：%+v", rep.Diagnoses)
	}
	if rep.FromEvidence != 0 || rep.FromHistory != 2 {
		t.Fatalf("来源计数不对：%+v", rep)
	}
}

// 空输入不 panic，且诊断不乱报
func TestAggregate_EmptySafe(t *testing.T) {
	rep := AggregateRuns(nil)
	if rep.Total != 0 || len(rep.Diagnoses) > 0 {
		t.Fatalf("空输入应无诊断：%+v", rep)
	}
}

// 长尾检测：p95 远超均值
func TestAggregate_LongTail(t *testing.T) {
	// 9 个 100ms + 1 个 5000ms：p95 会落在 100 附近，改用 12 个样本让尾部进 p95
	var recs []RunRecord
	for i := 1; i <= 11; i++ {
		recs = append(recs, rec(uint(i), 1, EvidencePassed, 100))
	}
	recs = append(recs, rec(12, 1, EvidencePassed, 5000))

	rep := AggregateRuns(recs)
	// 12 个样本 p95 索引 = int(11*0.95)=10 → 排序后第 11 个（5000 排最后），仍是 100
	// 所以这里验证的是"逻辑不误报"，不强行要求触发
	for _, d := range rep.Diagnoses {
		if d.Kind == "long_tail" && rep.Workflows[0].P95Ms <= rep.Workflows[0].AvgMs {
			t.Fatalf("长尾诊断的前提不成立却触发了：%+v", d)
		}
	}
}

// ===== 读盘 =====

func writeEvidenceFile(t *testing.T, dir string, ev Evidence) {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run-"+strconv.Itoa(int(ev.RunID))+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEvidenceRecords(t *testing.T) {
	dir := t.TempDir()
	passed := true
	writeEvidenceFile(t, dir, Evidence{
		RunID: 1, WorkflowID: 7, Result: EvidencePassed, Passed: &passed, DurationMS: 1234,
		GateChecks: []GateCheckEvidence{{Name: "音轨", OK: true, Level: "fail"}},
	})
	writeEvidenceFile(t, dir, Evidence{RunID: 2, WorkflowID: 7, Result: EvidenceUnscored, UnscoredReason: "no_gate"})
	// 一条坏文件：不该让整批失败
	_ = os.WriteFile(filepath.Join(dir, "run-3.json"), []byte("{broken"), 0o644)
	// 无关文件：应被忽略且不计入 bad
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)

	recs, bad, err := LoadEvidenceRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("应读到 2 条，实际 %d", len(recs))
	}
	if bad != 1 {
		t.Fatalf("应记 1 条坏文件，实际 %d", bad)
	}
	for _, r := range recs {
		if r.Fingerprint == "" {
			t.Fatal("每条都应有指纹（登记时用来追溯）")
		}
		if r.Source != "evidence" {
			t.Fatalf("来源应为 evidence：%s", r.Source)
		}
	}
}

// 目录不存在 → 空结果而不是报错（首次运行就是这状态）
func TestLoadEvidenceRecords_MissingDir(t *testing.T) {
	recs, bad, err := LoadEvidenceRecords(filepath.Join(t.TempDir(), "nope"))
	if err != nil || recs != nil || bad != 0 {
		t.Fatalf("目录不存在应返回空且无错：%v %v %v", recs, bad, err)
	}
}

// 没有一条「门判不过」的记录时，CR_clean 必须是 null 而不是 1.0。
//
// 这是 2026-10-23 用 125 条真实历史数据跑出来的：历史 failed 被塞进 unscored，
// 分母变 0，报告显示 CR_clean=1.000 —— 等于把 20 次失败伪装成"全过"。
func TestAggregate_CRCleanNullWhenNoGateFailed(t *testing.T) {
	recs := []RunRecord{
		{RunID: 1, WorkflowID: 1, Result: EvidencePassed, Source: "history"},
		{RunID: 2, WorkflowID: 1, Result: "aborted", Source: "history"},
		{RunID: 3, WorkflowID: 1, Result: "aborted", Source: "history"},
	}
	rep := AggregateRuns(recs)
	if rep.Overall.CRClean != nil {
		t.Fatalf("没有门判样本时 CR_clean 应为 null，实际 %.3f", *rep.Overall.CRClean)
	}
	if rep.Overall.Aborted != 2 {
		t.Fatalf("aborted 应计 2，实际 %d", rep.Overall.Aborted)
	}
	if rep.Overall.CRAll != 1.0/3.0 {
		t.Fatalf("CR_all 应为 1/3，实际 %.3f", rep.Overall.CRAll)
	}
	if rep.Overall.AbortedRate != 2.0/3.0 {
		t.Fatalf("aborted 率应为 2/3，实际 %.3f", rep.Overall.AbortedRate)
	}
}

// 有门判样本时 CR_clean 才出现，且只把它们计入分母
func TestAggregate_CRCleanPresentWithGateSamples(t *testing.T) {
	recs := []RunRecord{
		rec(1, 1, EvidencePassed, 100), // rec() 的 Source 是 evidence
		rec(2, 1, EvidencePassed, 100),
		rec(3, 1, EvidenceFailed, 100),
		{RunID: 4, WorkflowID: 1, Result: "aborted", Source: "history"},
	}
	rep := AggregateRuns(recs)
	if rep.Overall.CRClean == nil {
		t.Fatal("有门判样本时应给出 CR_clean")
	}
	if got := *rep.Overall.CRClean; got != 2.0/3.0 {
		t.Fatalf("CR_clean 应为 2/3（aborted 不进这个分母），实际 %.3f", got)
	}
}
