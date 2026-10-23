package service

import (
	"testing"
)

// 回归监测的测试盯两件事：
//  ① 只有一侧证据时必须判 insufficient，不许把「没测到」当「没退化」
//  ② 判定退化时给出的回滚目标是**基线证据实际跑过的版本**，不是"当前版本-1"

func evoEv(runID uint, caseID, role, result string, score float64, version int, checks ...GateCheckEvidence) Evidence {
	s := score
	return Evidence{
		SchemaVersion: EvidenceSchemaVersion, RunID: runID, WorkflowID: 7,
		CaseID: strPtr(caseID), Role: role, Category: strPtr("short"),
		Result: result, GateScore: &s, WorkflowVersion: version,
		GateChecks: checks, DurationMS: 60000, EffectiveTokens: 1800,
	}
}

func okChecks() []GateCheckEvidence {
	return []GateCheckEvidence{{Name: "min_frames", OK: true, Level: "fail", NodeID: "g1"}}
}

func badChecks() []GateCheckEvidence {
	return []GateCheckEvidence{{Name: "min_frames", OK: false, Level: "fail", NodeID: "g1"}}
}

func TestMonitorRegression_InsufficientWhenOneSideMissing(t *testing.T) {
	evs := []Evidence{
		evoEv(1, "c1", "baseline", EvidenceFailed, 0, 3, badChecks()...),
		evoEv(2, "c2", "baseline", EvidenceFailed, 0, 3, badChecks()...),
	}
	rep := MonitorRegression(evs, DefaultEvoPolicy())
	if rep.Verdict != "insufficient" {
		t.Fatalf("只有基线侧时必须判 insufficient，实得 %s", rep.Verdict)
	}
	if rep.RollbackTo != 0 {
		t.Fatalf("证据不足时不许给回滚目标，实得 v%d", rep.RollbackTo)
	}
}

func TestMonitorRegression_RegressedSuggestsBaselineVersion(t *testing.T) {
	var evs []Evidence
	// 基线 10 对全过（v3）；候选 10 对全挂（v4）
	for i := 0; i < 10; i++ {
		c := "case-" + string(rune('a'+i))
		evs = append(evs, evoEv(uint(100+i), c, "baseline", EvidencePassed, 1.0, 3, okChecks()...))
		evs = append(evs, evoEv(uint(200+i), c, "candidate", EvidenceFailed, 0.5, 4, badChecks()...))
	}
	rep := MonitorRegression(evs, DefaultEvoPolicy())
	if rep.Verdict != "regressed" {
		t.Fatalf("候选明显更差应判 regressed，实得 %s（%v）", rep.Verdict, rep.Reasons)
	}
	if rep.RollbackTo != 3 {
		t.Fatalf("回滚目标应是基线证据所在的 v3，实得 v%d", rep.RollbackTo)
	}
	if rep.Decision.Verdict != "keep_baseline" {
		t.Fatalf("底层配对判定应是 keep_baseline，实得 %s", rep.Decision.Verdict)
	}
}

func TestMonitorRegression_HealthyWhenCandidateBetter(t *testing.T) {
	var evs []Evidence
	for i := 0; i < 10; i++ {
		c := "case-" + string(rune('a'+i))
		evs = append(evs, evoEv(uint(100+i), c, "baseline", EvidenceFailed, 0.5, 3, badChecks()...))
		evs = append(evs, evoEv(uint(200+i), c, "candidate", EvidencePassed, 1.0, 4, okChecks()...))
	}
	rep := MonitorRegression(evs, DefaultEvoPolicy())
	if rep.Verdict != "healthy" {
		t.Fatalf("候选更好应判 healthy，实得 %s（%v）", rep.Verdict, rep.Reasons)
	}
	if rep.RollbackTo != 0 {
		t.Fatalf("健康时不该给回滚目标，实得 v%d", rep.RollbackTo)
	}
}

// 没有 role 标注时按版本号兜底：最小的当基线、最大的当当前，中间版本不参与。
func TestMonitorRegression_FallsBackToVersions(t *testing.T) {
	var evs []Evidence
	for i := 0; i < 10; i++ {
		c := "case-" + string(rune('a'+i))
		evs = append(evs, evoEv(uint(100+i), c, "", EvidencePassed, 1.0, 2, okChecks()...))
		evs = append(evs, evoEv(uint(200+i), c, "", EvidenceFailed, 0.5, 5, badChecks()...))
		// 中间版本：既不当基线也不当候选
		evs = append(evs, evoEv(uint(300+i), c, "", EvidencePassed, 1.0, 4, okChecks()...))
	}
	rep := MonitorRegression(evs, DefaultEvoPolicy())
	if rep.Verdict != "regressed" {
		t.Fatalf("按版本兜底后仍应看出退化，实得 %s（%v）", rep.Verdict, rep.Reasons)
	}
	if rep.RollbackTo != 2 {
		t.Fatalf("回滚目标应是 v2，实得 v%d", rep.RollbackTo)
	}
	if rep.CandidateVersion != 5 {
		t.Fatalf("候选侧版本应是 v5，实得 v%d", rep.CandidateVersion)
	}
}
