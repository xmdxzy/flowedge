package service

import "testing"

func mkPlan(shots ...Shot) *ShotPlan {
	return &ShotPlan{Shots: shots}
}

// 连续镜头（continue）里换轴 = 跳轴：观众会瞬间丢失方位，必须拦。
func TestAuditPlanConsistency_AxisFlipOnContinue(t *testing.T) {
	plan := mkPlan(
		Shot{ID: 1, Axis: "A侧", Light: "暖光 3200K", Transition: "continue"},
		Shot{ID: 2, Axis: "B侧", Light: "暖光 3200K", Transition: "cut"},
	)
	issues := AuditPlanConsistency(plan)
	fails := consistencyFails(issues)
	if len(fails) != 1 || fails[0].Code != "plan.axis_flip" {
		t.Fatalf("continue 换轴应报 plan.axis_flip，实际 %+v", issues)
	}
	if fails[0].ShotID != 2 {
		t.Errorf("问题应指向第 2 镜，实际 %d", fails[0].ShotID)
	}
}

// cut 换轴是合法的反打，不能误伤（误伤会让正常片子跑不起来）。
func TestAuditPlanConsistency_CutMayChangeAxis(t *testing.T) {
	plan := mkPlan(
		Shot{ID: 1, Axis: "A侧", Light: "暖光", Transition: "cut"},
		Shot{ID: 2, Axis: "B侧", Light: "冷光", Transition: "cut"},
	)
	if fails := consistencyFails(AuditPlanConsistency(plan)); len(fails) != 0 {
		t.Fatalf("cut 换轴/换光不该被拦，实际 %+v", fails)
	}
}

// 同一连续镜头里换光比 = 穿帮，必须拦。
func TestAuditPlanConsistency_LightJumpOnContinue(t *testing.T) {
	plan := mkPlan(
		Shot{ID: 1, Axis: "正面", Light: "暖光 3200K", Transition: "continue"},
		Shot{ID: 2, Axis: "正面", Light: "冷光 5600K", Transition: "cut"},
	)
	fails := consistencyFails(AuditPlanConsistency(plan))
	if len(fails) != 1 || fails[0].Code != "plan.light_jump" {
		t.Fatalf("continue 换光应报 plan.light_jump，实际 %+v", fails)
	}
}

// 场景锁按字节全等比对：LLM 改写一个词就算漂移。
// 锁句格式由 reSceneLock 约定：以 `SCENE LOCK:` 开头、以句号结束。
func TestAuditPlanConsistency_SceneLockDrift(t *testing.T) {
	lock := "SCENE LOCK: rainy night inn, wooden interior, warm oil lamp, wet floor."
	drifted := "SCENE LOCK: rainy night inn, wood interior, warm oil lamp, wet floor."
	plan := mkPlan(
		Shot{ID: 1, Prompt: lock + "\n镜头一"},
		Shot{ID: 2, Prompt: drifted + "\n镜头二"},
	)
	fails := consistencyFails(AuditPlanConsistency(plan))
	if len(fails) == 0 || fails[0].Code != "h3.scene_lock_drift" {
		t.Fatalf("锁句被改写应报 scene_lock_drift，实际 %+v", AuditPlanConsistency(plan))
	}
}

// 锁句字节完全一致时不能误报。
func TestAuditPlanConsistency_SceneLockSameIsOK(t *testing.T) {
	lock := "SCENE LOCK: rainy night inn, wooden interior, warm oil lamp, wet floor."
	plan := mkPlan(
		Shot{ID: 1, Prompt: lock + "\n镜头一"},
		Shot{ID: 2, Prompt: lock + "\n镜头二"},
	)
	if fails := consistencyFails(AuditPlanConsistency(plan)); len(fails) != 0 {
		t.Fatalf("同一串锁句不该报错，实际 %+v", fails)
	}
}

// 全片没用锁时不报缺锁（那是 text2video 提示词体检的事，不该在 plan 门重复判）。
func TestAuditPlanConsistency_NoLockIsNotAFail(t *testing.T) {
	plan := mkPlan(
		Shot{ID: 1, Prompt: "镜头一", Transition: "cut"},
		Shot{ID: 2, Prompt: "镜头二", Transition: "cut"},
	)
	if fails := consistencyFails(AuditPlanConsistency(plan)); len(fails) != 0 {
		t.Fatalf("没写锁不该判 FAIL，实际 %+v", fails)
	}
}

// 环境声漂移只告警不拦：换场地换声景是合理创作。
func TestAuditPlanConsistency_AmbienceDriftIsWarn(t *testing.T) {
	plan := mkPlan(
		Shot{ID: 1, Ambience: "持续雨声", Transition: "cut"},
		Shot{ID: 2, Ambience: "持续雨声", Transition: "cut"},
		Shot{ID: 3, Ambience: "持续风声", Transition: "cut"},
	)
	issues := AuditPlanConsistency(plan)
	if len(consistencyFails(issues)) != 0 {
		t.Fatalf("环境声漂移不该判 FAIL：%+v", issues)
	}
	found := false
	for _, it := range issues {
		if it.Code == "plan.ambience_drift" && it.Level == "WARN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应给出 plan.ambience_drift 告警，实际 %+v", issues)
	}
}

// 单镜片没有跨镜一致性可言，直接放行。
func TestAuditPlanConsistency_SingleShot(t *testing.T) {
	if issues := AuditPlanConsistency(mkPlan(Shot{ID: 1, Transition: "continue"})); len(issues) != 0 {
		t.Fatalf("单镜不该有跨镜判定，实际 %+v", issues)
	}
	if issues := AuditPlanConsistency(nil); len(issues) != 0 {
		t.Fatalf("nil plan 不该 panic/报错，实际 %+v", issues)
	}
}
