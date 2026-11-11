package service

import "testing"

// 一采确认的门：开了确认必须给 seed，且二采被压成 0。
func TestFirstPassGate(t *testing.T) {
	// 关着确认：原样放行
	ref, seed, err := firstPassGate(0.35, false, 0)
	if err != nil || ref != 0.35 || seed != 0 {
		t.Fatalf("关确认时不该改动：ref=%v seed=%v err=%v", ref, seed, err)
	}

	// 开着确认 + 有 seed：二采压成 0，seed 保留
	ref, seed, err = firstPassGate(0.35, true, 4242)
	if err != nil {
		t.Fatalf("给了 seed 不该报错：%v", err)
	}
	if ref != 0 {
		t.Fatalf("一采确认必须把二采压成 0，实际 %v", ref)
	}
	if seed != 4242 {
		t.Fatalf("seed 应保留 4242，实际 %v", seed)
	}

	// 开着确认但没 seed：必须报错——否则第二遍的一采是另一次采样，
	// 用户精修的就不是他看过的那版构图
	if _, _, err = firstPassGate(0.35, true, 0); err == nil {
		t.Fatal("开确认但没 seed 必须报错")
	}
	if _, _, err = firstPassGate(0.35, true, -5); err == nil {
		t.Fatal("负 seed 也要报错")
	}

	// 开确认但本来就没开二采：合法（等于只跑一采，不需要 seed 也能跑）
	ref, seed, err = firstPassGate(0, true, 0)
	if err != nil {
		t.Fatalf("没开二采时不该强制 seed：%v", err)
	}
	_ = ref
	_ = seed
}
