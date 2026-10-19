package service

import (
	"strings"
	"testing"
)

// 模型会把分镜写成 "### 拍1 …" / "1. …" / "- …"，这些符号不能进 H3 提示词
func TestStripMarkdownLead(t *testing.T) {
	cases := map[string]string{
		"### 拍1 一名青袍修士站在云海之上": "拍1 一名青袍修士站在云海之上",
		"1. 剑光撕裂空气":        "剑光撕裂空气",
		"2、魔修挥杖":          "魔修挥杖",
		"- 镜头缓慢推进":         "镜头缓慢推进",
		"**加粗的提示**":        "加粗的提示",
		"正常提示词":           "正常提示词",
	}
	for in, want := range cases {
		if got := stripMarkdownLead(in); got != want {
			t.Fatalf("stripMarkdownLead(%q) = %q, want %q", in, got, want)
		}
	}
}

// 分镜行里带编号时也要清掉，且过短的行要丢弃
func TestCleanOptimizedPromptSingleLine(t *testing.T) {
	in := "### 提示词：一名修士\n站在山巅，镜头缓推。"
	got := cleanOptimizedPrompt(in)
	if got == "" {
		t.Fatal("清洗后不应为空")
	}
	if len([]rune(got)) < 5 {
		t.Fatalf("清洗后过短：%q", got)
	}
	for _, bad := range []string{"###", "提示词："} {
		if len(got) >= len(bad) && got[:len(bad)] == bad {
			t.Fatalf("仍残留前缀 %q: %q", bad, got)
		}
	}
}

// 三段式声音字段是「胡言乱语」的正面解药：字段空着，H3 就会自己脑补人声。
// 这层兜底必须与 LLM 是否听话无关。
func TestEnsureH3AudioFields(t *testing.T) {
	// 情况 1：什么都没写 → 三个字段都要补齐
	got, patched := ensureH3AudioFields("A young cultivator stands on a cliff.")
	if !patched {
		t.Fatal("裸文本应当被判为需要补字段")
	}
	for _, field := range []string{"integrated_multimodal_description", "overall_soundscape", "non_diegetic_music"} {
		if !strings.Contains(got, field) {
			t.Fatalf("缺少字段 %s：%q", field, got)
		}
	}
	if !strings.Contains(got, "non_diegetic_music: N/A") {
		t.Fatalf("没要求音乐时应写 N/A：%q", got)
	}

	// 情况 2：已经有完整字段 → 不该重复追加
	full := "integrated_multimodal_description: [Shot 1] wind.\noverall_soundscape: continuous wind.\nnon_diegetic_music: N/A"
	if out, p := ensureH3AudioFields(full); p || out != full {
		t.Fatalf("完整字段被改写：patched=%v %q", p, out)
	}
}

// 结构化字段不能被"压成一段"——压平会丢掉字段边界，等于又回到没写声音。
func TestCleanOptimizedPromptKeepsFields(t *testing.T) {
	in := "### integrated_multimodal_description: [Shot 1] a monk draws his sword.\noverall_soundscape: wind over stone, blade humming.\nnon_diegetic_music: N/A"
	got := cleanOptimizedPrompt(in)
	if strings.Count(got, "\n") < 2 {
		t.Fatalf("字段换行被吞掉了：%q", got)
	}
	for _, field := range []string{"overall_soundscape", "non_diegetic_music"} {
		if !strings.Contains(got, field) {
			t.Fatalf("字段 %s 丢失：%q", field, got)
		}
	}
}

// cfg=1 没有负分支：压制人声只能靠"口型可见事实"，而且要写进画面描述里。
func TestEnsureDialogueFree(t *testing.T) {
	base := "integrated_multimodal_description: [Shot 1] two cultivators face each other.\noverall_soundscape: wind.\nnon_diegetic_music: N/A"
	got := ensureDialogueFree(base)
	if !strings.Contains(got, "Lips stay closed") {
		t.Fatalf("没有加上口型约束：%q", got)
	}
	if strings.Contains(got, "non_diegetic_music: N/A Lips") {
		t.Fatalf("口型约束被写到字段外/文末：%q", got)
	}
	// 有台词时不能加（会把正在说话的口型按住）
	withLine := "integrated_multimodal_description: [Shot 1] (S1) says <d>[English] Hold the line.</d>"
	if out := ensureDialogueFree(withLine); out != withLine {
		t.Fatalf("有台词时不该改动：%q", out)
	}
}
