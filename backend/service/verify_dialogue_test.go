package service

import (
	"strings"
	"testing"
)

// 2026-10-30 run 357 的真实案例：W4A8 权重上成片音轨把台词念对了，但 ASR 回
// 「晚安今晚的海很平静」（无标点），验词门拿「晚安，今晚的海很平静。」去
// Contains 必然判死——连 TTS 念轨本尊都转不出全角句号。验词只比字。
func TestStripASRPunct(t *testing.T) {
	cases := []struct {
		name   string
		asr    string
		expect string
		pass   bool
	}{
		{"无标点对全角", "晚安今晚的海很平静", "晚安，今晚的海很平静。", true},
		{"半角对全角", "晚安,今晚的海很平静", "晚安，今晚的海很平静。", true},
		{"含识别误差但台词在内", "晚安,今晚的还很平静", "晚安，今晚的海很平静。", false},
		{"念错判死", "今天天气不错", "晚安，今晚的海很平静。", false},
	}
	for _, c := range cases {
		got := strings.Contains(stripASRPunct(c.asr), stripASRPunct(c.expect))
		if got != c.pass {
			t.Errorf("%s：期望 %v，实际 %v", c.name, c.pass, got)
		}
	}
}
