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
		got, _ := dialogueLineMatch(c.asr, c.expect)
		if got != c.pass {
			t.Errorf("%s：期望 %v，实际 %v", c.name, c.pass, got)
		}
	}
}

// 2026-10-30 run 367 第 2 镜的真实转写：H3 把同一句念了两遍，
// 纯 Contains 会判通过（"台词在里面"），但观众听得出重复。
// 这条钉住「多念 = 不过」，否则段级重试永远不触发。
func TestDialogueLineMatch_RejectsRepeat(t *testing.T) {
	const expect = "明天见，做个好梦。"
	cases := []struct {
		name    string
		asr     string
		pass    bool
		count   int
		wantMsg string
	}{
		{"念一遍", "明天见,做个好梦。", true, 1, ""},
		{"念两遍（run 367 实测）", "明天见,做个好梦。明天见,做个好梦。", false, 2, "重复"},
		{"念三遍", "明天见做个好梦 明天见做个好梦 明天见做个好梦", false, 3, "重复"},
		{"没念", "今天天气不错", false, 0, "缺"},
	}
	for _, c := range cases {
		pass, n := dialogueLineMatch(c.asr, expect)
		if pass != c.pass || n != c.count {
			t.Errorf("%s：期望 pass=%v 次数=%d，实际 pass=%v 次数=%d（%s）",
				c.name, c.pass, c.count, pass, n, c.wantMsg)
		}
	}
}

// 空期望词不能让门静默通过（checked=false 的语义在调用方，这里只钉住纯函数）
func TestDialogueLineMatch_EmptyExpect(t *testing.T) {
	if pass, n := dialogueLineMatch("随便什么", "  "); pass || n != 0 {
		t.Fatalf("空期望词应判不过，实际 pass=%v n=%d", pass, n)
	}
	if !strings.Contains(stripASRPunct("S1：明天见"), "明天见") {
		t.Fatal("stripASRPunct 应保留中文字且去掉「：」")
	}
}
