package service

import (
	"strings"
	"testing"
)

// 纯音频（无参考图）时不得出现任何 <Picture N> 引用。
//
// 事故（2026-10-19 22:11 实测）：只给 ref_audio、不给 ref_images，
// 旧版兜底会补一句 "<Picture 1> defines the visual identity..."，而图里
// 并没有 LoadImage 节点。模型看到指向不存在输入的 <Picture 1> 后不认
// "照抄音频"，退化成自己生成伪中文——成片 ASR 转出「湘田英臣想话…」。
func TestEnsureRef2VAPromptAudioOnlyHasNoPictureRef(t *testing.T) {
	got := ensureRef2VAPrompt("[Shot 1] a monk speaks to camera.", 0, 1)
	if strings.Contains(got, "<Picture") {
		t.Fatalf("audio-only prompt must not reference <Picture N>:\n%s", got)
	}
	if !strings.Contains(got, "<Audio 1>") {
		t.Fatalf("audio-only prompt must reference <Audio 1>:\n%s", got)
	}
	if !strings.Contains(got, "fully_copy") {
		t.Fatalf("must keep fully_copy so the reference audio is reused 1:1:\n%s", got)
	}
}

// 没有参考图也没有参考音频时，同样不许引用不存在的输入。
func TestEnsureRef2VAPromptNoRefsHasNoDanglingRef(t *testing.T) {
	got := ensureRef2VAPrompt("a lone tree on a hill", 0, 0)
	if strings.Contains(got, "<Picture") || strings.Contains(got, "<Audio") {
		t.Fatalf("no-refs prompt must not reference any <Picture N>/<Audio N>:\n%s", got)
	}
}
