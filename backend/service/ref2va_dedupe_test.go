package service

import (
	"strings"
	"testing"
)

// 源提示词自带声景字段时，成品里必须只剩一份权威值。
//
// 事故（2026-10-19 22:30）：用户提示词写了 overall_soundscape / non_diegetic_music，
// ensureRef2VAPrompt 把整段原文塞进 detailed_description，末尾又追加一份 N/A，
// 同一份提示词出现两个矛盾的 overall_soundscape。ref_audios 接线是对的、
// retention_analysis 里 fully_copy 也在，但成片音轨仍被重新生成（corr 0.22）。
func TestEnsureRef2VAPromptDedupesSoundFields(t *testing.T) {
	src := "[Shot 1] a monk speaks to camera.\n" +
		"[Shot 2] At 00:07.500, cut to a wider shot.\n" +
		"overall_soundscape: high-altitude wind, cloth flapping.\n" +
		"non_diegetic_music: N/A"
	got := ensureRef2VAPrompt(src, 0, 1)

	if n := strings.Count(got, "overall_soundscape:"); n != 1 {
		t.Fatalf("overall_soundscape must appear exactly once, got %d:\n%s", n, got)
	}
	if n := strings.Count(got, "non_diegetic_music:"); n != 1 {
		t.Fatalf("non_diegetic_music must appear exactly once, got %d:\n%s", n, got)
	}
	if strings.Contains(got, "high-altitude wind") {
		t.Fatalf("source soundscape must be stripped when reusing reference audio:\n%s", got)
	}
	if !strings.Contains(got, "overall_soundscape: N/A") {
		t.Fatalf("audio-reuse mode must force N/A soundscape:\n%s", got)
	}
	// 画面描述不能被剥掉
	if !strings.Contains(got, "[Shot 1] a monk speaks to camera.") || !strings.Contains(got, "At 00:07.500") {
		t.Fatalf("visual description must survive stripping:\n%s", got)
	}
	if !strings.Contains(got, "fully_copy") {
		t.Fatalf("fully_copy must stay:\n%s", got)
	}
}

// 不带声景字段的提示词不受影响（不能把正文误剥）。
func TestEnsureRef2VAPromptKeepsPlainDescription(t *testing.T) {
	got := ensureRef2VAPrompt("a lone tree on a hill at dusk", 0, 0)
	if !strings.Contains(got, "a lone tree on a hill at dusk") {
		t.Fatalf("plain description must survive:\n%s", got)
	}
}
