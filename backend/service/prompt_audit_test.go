package service

import (
	"strings"
	"testing"
)

// 提示词体检的意义：官方指南里最容易漏的是结构性缺失，不是文笔。
// 这几条都能确定性判断，错了就必须在出片前拦住或在日志里喊出来。

func issueCodes(issues []h3PromptIssue) []string {
	out := make([]string, 0, len(issues))
	for _, it := range issues {
		out = append(out, it.Level+":"+it.Code)
	}
	return out
}

func hasIssue(issues []h3PromptIssue, code string) bool {
	for _, it := range issues {
		if it.Code == code {
			return true
		}
	}
	return false
}

func TestAuditH3PromptClean(t *testing.T) {
	good := `integrated_multimodal_description: [Shot 1] Live-action, cinematic, a medium-wide shot frames a gate captain in black scale armour standing on the stone steps. The camera pushes in with small amplitude at slow speed as the middle-aged captain with a low, gravelly voice (S1) says: <d>[Chinese] 站住。</d> He closes his lips and holds still.
overall_soundscape: Continuous wind over the city wall behind him, cloth snapping, armour plates shifting as he breathes.
non_diegetic_music: N/A`
	issues := auditH3Prompt(good)
	for _, it := range issues {
		if it.Level == "FAIL" {
			t.Fatalf("合格的提示词不该有 FAIL：%v", issueCodes(issues))
		}
	}
	if hasIssue(issues, "speaker_no_voice") {
		t.Fatalf("说话人前已有音色描述，不该报 speaker_no_voice：%v", issueCodes(issues))
	}
}

// 官方 2.2：只用来定身份的图不得写成 <Picture N> defines/is the visual identity
func TestAuditH3PromptPictureIdentityMisuse(t *testing.T) {
	bad := `subject_definitions: <Picture 1> defines the visual identity, wardrobe and props of the characters.
retention_analysis: <Audio 1>: fully_copy - reused 1:1.
detailed_description: [Shot 1] Live-action, cinematic, a man walks.
overall_soundscape: wind.
non_diegetic_music: N/A`
	issues := auditH3Prompt(bad)
	if !hasIssue(issues, "picture_identity_misuse") {
		t.Fatalf("应报 picture_identity_misuse：%v", issueCodes(issues))
	}
}

// 有参考音频却没标 fully_copy → 念稿会退化成伪中文，必须 FAIL
func TestAuditH3PromptAudioNotFullyCopy(t *testing.T) {
	bad := `subject_definitions: <Subject 1> is the man in <Picture 1>.
detailed_description: [Shot 1] Live-action, cinematic, the man says: (S1) <d>[Chinese] 站住。</d>
overall_soundscape: The ambience from <Audio 1> continues.
non_diegetic_music: N/A`
	issues := auditH3Prompt(bad)
	if !hasIssue(issues, "audio_not_fully_copy") {
		t.Fatalf("应报 audio_not_fully_copy：%v", issueCodes(issues))
	}
}

// 说话人 ID 前没有音色/年龄/性别描述 → 口型与语气失控
func TestAuditH3PromptSpeakerWithoutVoice(t *testing.T) {
	bad := `integrated_multimodal_description: [Shot 1] Live-action, cinematic, a man sits. (S1) says: <d>[Chinese] 走。</d>
overall_soundscape: room tone.
non_diegetic_music: N/A`
	issues := auditH3Prompt(bad)
	if !hasIssue(issues, "speaker_no_voice") {
		t.Fatalf("应报 speaker_no_voice：%v", issueCodes(issues))
	}
}

// 缺声景/配乐字段 = 模型自行脑补 = 即兴人声
func TestAuditH3PromptMissingSoundFields(t *testing.T) {
	bad := "integrated_multimodal_description: [Shot 1] Live-action, cinematic, a man walks."
	issues := auditH3Prompt(bad)
	if !hasIssue(issues, "soundscape_missing") || !hasIssue(issues, "music_missing") {
		t.Fatalf("应报两个声音字段缺失：%v", issueCodes(issues))
	}
}

// 多行 retention_analysis 追加 fully_copy 时不能丢掉已有行
func TestPatchRefAudioRetentionKeepsExistingLines(t *testing.T) {
	in := `subject_definitions:
<Subject 1> is the heir in <Picture 1>.
summary: [reference generation] The heir walks to the gate.
retention_analysis:
<Subject 1> (appears in [Shot 1]): fully_preserved - robe and jade plaque.
detailed_description: [Shot 1] Live-action, cinematic, he walks.
overall_soundscape: N/A
non_diegetic_music: N/A`
	out := patchRefAudioRetention(in, 1)
	if !strings.Contains(out, "<Audio 1>: fully_copy") {
		t.Fatalf("缺少补上的 fully_copy：\n%s", out)
	}
	if !strings.Contains(out, "fully_preserved - robe and jade plaque") {
		t.Fatalf("原有 retention 行被吞掉了：\n%s", out)
	}
	// 不能插到 detailed_description 里
	if strings.Contains(out, "detailed_description: [Shot 1] Live-action, cinematic, he walks. <Audio 1>") {
		t.Fatalf("补丁插错字段：\n%s", out)
	}
	if n := strings.Count(out, "overall_soundscape:"); n != 1 {
		t.Fatalf("字段被重复：%d\n%s", n, out)
	}
}

// 已标过 fully_copy 的不重复补
func TestPatchRefAudioRetentionIdempotent(t *testing.T) {
	in := "retention_analysis:\n<Audio 1>: fully_copy - reused 1:1."
	if out := patchRefAudioRetention(in, 1); out != in {
		t.Fatalf("不应重复追加：\n%s", out)
	}
}

// 字段块解析必须能处理多行值（旧实现只读到行尾）
func TestH3FieldBlockMultiline(t *testing.T) {
	p := "subject_definitions:\n<Subject 1> is a man.\n<Subject 2> is a dog.\nsummary: one shot.\ndetailed_description: [Shot 1] x"
	block, start, _ := h3FieldBlock(p, "subject_definitions")
	if start < 0 || !strings.Contains(block, "<Subject 2> is a dog.") {
		t.Fatalf("多行块没取全：%q", block)
	}
	if strings.Contains(block, "summary") {
		t.Fatalf("块尾截断失败，吃进了下一个字段：%q", block)
	}
}
