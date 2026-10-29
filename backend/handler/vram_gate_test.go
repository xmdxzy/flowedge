package handler

import (
	"testing"

	"flowedge/backend/service"
)

// 显存预检门只拦「连权重都装不下」这一档。
//
// 由来（2026-10-26）：之前只记日志不拦，于是每一轮都先跑满十几分钟再撞节点超时。
// 但也不能一刀切拦所有非 OK 档——reserve 档（装得下、保不住预留）实测仍能 82–112s 出片，
// 拦掉它等于把「慢」升级成「不可用」。
func TestVRAMGateBlocks(t *testing.T) {
	cases := []struct {
		name         string
		level        string
		allowPartial bool
		want         bool
	}{
		{"装得下", service.VRAMLevelOK, false, false},
		{"保不住预留但不拦", service.VRAMLevelReserve, false, false},
		{"装不下要拦", service.VRAMLevelTooSmall, false, true},
		{"显式硬跑放行", service.VRAMLevelTooSmall, true, false},
		{"未知档不拦（宁可放行也不误杀）", "weird", false, false},
	}
	for _, c := range cases {
		adv := service.VRAMAdvisory{Level: c.level}
		if got := vramGateBlocks(adv, c.allowPartial); got != c.want {
			t.Fatalf("%s: vramGateBlocks = %v, want %v", c.name, got, c.want)
		}
	}
}
