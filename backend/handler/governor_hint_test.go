package handler

import (
	"strings"
	"testing"
)

// TestGovernorHintPointsAtRealCause 拒绝提示必须指向真实原因。
//
// 真实事故（2026-10-23）：WeGame 启动器常驻触发「游戏保护模式」，
// 而提示写的是"停掉占满显存的其他进程"——照着做完全没用，查了半小时。
// 提示指错方向的代价，比不给提示更高。
func TestGovernorHintPointsAtRealCause(t *testing.T) {
	cases := []struct {
		reasons []string
		want    string
		notWant string
	}{
		{[]string{"游戏保护模式"}, "WeGame", "显存"},
		{[]string{"gpu_reserve"}, "显存", "游戏"},
		{[]string{"cpu_pressure"}, "CPU", "显存"},
		{[]string{"剩余磁盘(GB)"}, "磁盘", "游戏"},
		{nil, "reasons", ""},
	}
	for _, c := range cases {
		got := governorHint(c.reasons)
		if !strings.Contains(got, c.want) {
			t.Errorf("reasons=%v 的提示里应有 %q，实际：%s", c.reasons, c.want, got)
		}
		if c.notWant != "" && strings.Contains(got, c.notWant) {
			t.Errorf("reasons=%v 的提示不该提 %q：%s", c.reasons, c.notWant, got)
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("reasons=%v 返回空提示", c.reasons)
		}
	}
}
