package service

import (
	"strings"
	"testing"
)

// ref2va 的六段式是"正常念稿"的开关：缺 subject_definitions / retention_analysis，
// 模型会把参考音频当成风格提示重新生成，于是台词又变成伪中文。
func TestEnsureRef2VAPrompt(t *testing.T) {
	in := "integrated_multimodal_description: [Shot 1] A middle-aged man sits at a table.\n" +
		"overall_soundscape: low room tone.\nnon_diegetic_music: N/A"
	got := ensureRef2VAPrompt(in, 1, 1)

	for _, want := range []string{
		"subject_definitions:",
		"<Picture 1>",
		"<Audio 1>",
		"summary: [reference generation + audio reuse]",
		"retention_analysis:",
		"fully_copy - is reused 1:1 as the target video's complete final audio track",
		"detailed_description:",
		"A middle-aged man sits at a table",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q：\n%s", want, got)
		}
	}
	// 音频 1:1 透传时，整体声景必须 N/A，否则模型会往音轨里加东西
	if !strings.Contains(got, "overall_soundscape: N/A") {
		t.Fatalf("fully_copy 下声景应写 N/A：\n%s", got)
	}
	// 不能再冒出旧的 integrated_ 字段（六段式里它叫 detailed_description）
	if strings.Contains(got, "integrated_multimodal_description:") {
		t.Fatalf("旧字段没被换掉：\n%s", got)
	}

	// 已经写好六段式的不能被二次包装
	six := "subject_definitions: <Picture 1> is a man.\nsummary: [reference generation]\ndetailed_description: [Shot 1] x"
	if out := ensureRef2VAPrompt(six, 1, 0); out != six {
		t.Fatalf("重复包装：\n%s", out)
	}
}

// 没有参考音频时不该写 audio reuse / fully_copy
func TestEnsureRef2VAPromptImageOnly(t *testing.T) {
	got := ensureRef2VAPrompt("[Shot 1] a monk draws his sword", 2, 0)
	if strings.Contains(got, "audio reuse") || strings.Contains(got, "fully_copy") {
		t.Fatalf("无参考音频却声明了音频复用：\n%s", got)
	}
	if !strings.Contains(got, "<Picture 2>") {
		t.Fatalf("多参考图没被逐个定义：\n%s", got)
	}
}

func TestStringList(t *testing.T) {
	cases := []struct {
		in   interface{}
		want int
	}{
		{"a.wav\nb.wav", 2},
		{"a.wav；b.wav", 2},
		{"a.wav", 1},
		{[]interface{}{"a.wav", "b.wav", ""}, 2},
		{nil, 0},
	}
	for _, c := range cases {
		if got := stringList(map[string]interface{}{"k": c.in}, "k"); len(got) != c.want {
			t.Fatalf("stringList(%v) 得到 %d 项，期望 %d", c.in, len(got), c.want)
		}
	}
}
