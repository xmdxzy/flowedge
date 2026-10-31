package service

import "testing"

func TestParseBudget(t *testing.T) {
	// 嵌套写法
	b := ParseBudget(map[string]interface{}{
		"budget": map[string]interface{}{"max_wall_seconds": float64(900), "on_exceed": "stop"},
	})
	if b.MaxWallSeconds != 900 || b.OnExceed != "stop" || !b.Enabled() || !b.ShouldStop() {
		t.Fatalf("嵌套写法解析错误：%+v", b)
	}

	// 简写：只给上限，动作默认 warn（默认 warn 是刻意的：误杀长片代价更大）
	b = ParseBudget(map[string]interface{}{"max_wall_seconds": float64(600)})
	if b.MaxWallSeconds != 600 || b.OnExceed != "warn" || b.ShouldStop() {
		t.Fatalf("简写解析错误：%+v", b)
	}

	// 未声明
	b = ParseBudget(map[string]interface{}{"prompt": "x"})
	if b.Enabled() || b.ShouldStop() {
		t.Fatalf("未声明预算时不应启用：%+v", b)
	}
	if b.Describe() != "未声明预算" {
		t.Errorf("描述错误：%q", b.Describe())
	}

	// 非法动作一律回落 warn，不能让一个拼错的字符串把运行停掉
	for _, bad := range []string{"", "STOPP", "kill", "stop "} {
		got := ParseBudget(map[string]interface{}{
			"budget": map[string]interface{}{"max_wall_seconds": float64(60), "on_exceed": bad},
		})
		if bad == "stop " && got.OnExceed != "stop" {
			t.Errorf("带空格的 stop 应被识别，实际 %q", got.OnExceed)
		}
		if bad != "stop " && got.OnExceed != "warn" {
			t.Errorf("非法动作 %q 应回落 warn，实际 %q", bad, got.OnExceed)
		}
	}

	// nil 参数不 panic
	if got := ParseBudget(nil); got.Enabled() {
		t.Errorf("nil 参数不应启用预算")
	}
}

func TestBudgetCheck_Boundary(t *testing.T) {
	b := Budget{MaxWallSeconds: 100, OnExceed: "stop"}
	// 正好等于上限不算超（边界取「不超过」）
	if ex, _ := b.Check(100); ex {
		t.Error("恰好等于上限不应算超限")
	}
	ex, reason := b.Check(100.5)
	if !ex || reason == "" {
		t.Fatalf("超过上限应告警并给原因，实际 ex=%v reason=%q", ex, reason)
	}

	// 未启用时永不超限
	off := Budget{}
	if ex, _ := off.Check(999999); ex {
		t.Error("未声明预算时不应判超限")
	}
}

func TestBudgetReconcile(t *testing.T) {
	est := GraphEstimate{P50Sec: 100, P90Sec: 200, WorstSec: 900}
	b := Budget{MaxWallSeconds: 150, OnExceed: "stop"}

	cases := []struct {
		actual  float64
		verdict string
		within  *bool
	}{
		{80, "under_p50", boolPtr(true)},
		{150, "in_p50_p90", boolPtr(true)},
		{300, "over_p90", boolPtr(false)},
		{1000, "over_worst", boolPtr(false)},
		{0, "unknown", boolPtr(true)}, // 没跑完：结论 unknown，但 0s 确实在预算内
	}
	for _, c := range cases {
		r := b.Reconcile(est, c.actual)
		if r.Verdict != c.verdict {
			t.Errorf("actual=%.0f 应判 %s，实际 %s", c.actual, c.verdict, r.Verdict)
		}
		if c.within == nil {
			continue
		}
		if r.WithinBudget == nil {
			t.Errorf("actual=%.0f 应给出 within_budget", c.actual)
			continue
		}
		if *r.WithinBudget != *c.within {
			t.Errorf("actual=%.0f within_budget 应为 %v，实际 %v", c.actual, *c.within, *r.WithinBudget)
		}
	}

	// 未声明预算时 within_budget 必须留空（不能凭空说"在预算内"）
	r := Budget{}.Reconcile(est, 100)
	if r.WithinBudget != nil {
		t.Error("未声明预算时 within_budget 应为 nil")
	}
}

func boolPtr(v bool) *bool { return &v }
