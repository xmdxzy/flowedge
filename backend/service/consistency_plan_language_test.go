package service

import "testing"

// 这组用例盯的是「镜头语言字段本身」的合法性：
// 2026-10-25 实跑 storyboard 时，angle 被 LLM 填成「近景」，而当时没有任何门会拦它。

func planOf(shots ...Shot) *ShotPlan {
	return &ShotPlan{Shots: shots}
}

func hasPlanIssue(issues []ShotConsistencyIssue, code string) bool {
	for _, it := range issues {
		if it.Code == code {
			return true
		}
	}
	return false
}

func TestAuditPlan_AngleIsShotSizeFails(t *testing.T) {
	p := planOf(
		Shot{ID: 1, ShotSize: "中景", Angle: "平视", Axis: "A侧", Camera: "缓慢推近", Ambience: "雨声持续"},
		Shot{ID: 2, ShotSize: "近景", Angle: "近景", Axis: "A侧", Camera: "固定", Ambience: "雨声持续"},
	)
	issues := AuditPlanConsistency(p)
	if !hasPlanIssue(issues, "plan.angle_is_shot_size") {
		t.Fatalf("angle 填成景别应报 FAIL，实际 %+v", issues)
	}
	if len(consistencyFails(issues)) == 0 {
		t.Fatalf("该问题应是 FAIL 级（会在开机前拦下）")
	}
}

func TestAuditPlan_LegalAnglesPass(t *testing.T) {
	p := planOf(
		Shot{ID: 1, ShotSize: "中景", Angle: "平视", Axis: "A侧", Camera: "缓慢推近", Ambience: "雨声持续"},
		Shot{ID: 2, ShotSize: "近景", Angle: "仰拍", Axis: "A侧", Camera: "固定", Ambience: "雨声持续"},
	)
	for _, it := range AuditPlanConsistency(p) {
		if it.Code == "plan.angle_is_shot_size" || it.Code == "plan.angle_unknown" {
			t.Fatalf("合法机位不该报警：%+v", it)
		}
	}
}

func TestAuditPlan_CameraMonotoneWarns(t *testing.T) {
	p := planOf(
		Shot{ID: 1, ShotSize: "远景", Angle: "平视", Axis: "A侧", Camera: "固定", Ambience: "雨声持续"},
		Shot{ID: 2, ShotSize: "中景", Angle: "平视", Axis: "A侧", Camera: "固定", Ambience: "雨声持续"},
		Shot{ID: 3, ShotSize: "近景", Angle: "平视", Axis: "A侧", Camera: "固定", Ambience: "雨声持续"},
		Shot{ID: 4, ShotSize: "特写", Angle: "平视", Axis: "A侧", Camera: "缓慢推近", Ambience: "雨声持续"},
	)
	issues := AuditPlanConsistency(p)
	if !hasPlanIssue(issues, "plan.camera_monotone") {
		t.Fatalf("4 镜里 3 镜固定应告警，实际 %+v", issues)
	}
	for _, it := range issues {
		if it.Code == "plan.camera_monotone" && it.Level != "WARN" {
			t.Fatalf("运镜单调只该告警不该拦：%+v", it)
		}
	}
}

func TestAuditPlan_ShotSizeRepeatWarns(t *testing.T) {
	p := planOf(
		Shot{ID: 1, ShotSize: "中景", Angle: "平视", Axis: "A侧", Camera: "固定", Ambience: "雨声持续"},
		Shot{ID: 2, ShotSize: "大特写", Angle: "平视", Axis: "A侧", Camera: "固定", Ambience: "雨声持续"},
		Shot{ID: 3, ShotSize: "特写", Angle: "平视", Axis: "A侧", Camera: "固定", Ambience: "雨声持续"},
	)
	if !hasPlanIssue(AuditPlanConsistency(p), "plan.shot_size_repeat") {
		t.Fatalf("相邻两记特写应告警")
	}
}
