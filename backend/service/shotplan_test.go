package service

import (
	"strings"
	"testing"
)

// 逐镜提示词必须自带官方三段式字段，并且不能用否定式禁言。
// 这两个点直接决定音频会不会冒人声，属于"改了会静默退化"的地方，用测试焊住。
func TestCompileShotPromptAudioFields(t *testing.T) {
	plan := &ShotPlan{
		Title:     "斗法",
		Character: "一名青袍束发的年轻修士背着一柄青锋剑",
		Ambience:  "远处持续的风声",
	}
	sh := Shot{
		ID: 1, Seconds: 5, ShotSize: "中景", Angle: "平视", Axis: "A侧",
		Camera: "缓慢推近", Action: "拔剑起势，剑尖停在胸前", Light: "冷白侧光",
		Ambience: "远处持续的风声", SFX: "剑出鞘的金属摩擦声",
	}
	got := CompileShotPrompt(plan, sh, false)

	for _, field := range []string{
		"integrated_multimodal_description:",
		"overall_soundscape:",
		"non_diegetic_music: N/A",
	} {
		if !strings.Contains(got, field) {
			t.Fatalf("缺少字段 %q：\n%s", field, got)
		}
	}

	// 否定式禁言是旧写法，官方明确 cfg=1 无负分支，会反效果
	for _, bad := range []string{"无对白", "不发出任何人声", "禁止文字", "不要说话"} {
		if strings.Contains(got, bad) {
			t.Fatalf("残留否定式措辞 %q：\n%s", bad, got)
		}
	}

	// 没台词时要用可见的口型事实
	if !strings.Contains(got, "嘴唇紧闭") {
		t.Fatalf("缺少正面口型约束：\n%s", got)
	}
	// 口型事实属于画面描述，必须出现在 overall_soundscape 之前
	if strings.Index(got, "嘴唇紧闭") > strings.Index(got, "overall_soundscape") {
		t.Fatalf("口型约束被写进了声音字段：\n%s", got)
	}
}

// 有台词时走 <d> 标签，且不再追加"闭嘴"
func TestCompileShotPromptWithDialogue(t *testing.T) {
	plan := &ShotPlan{Title: "t", Character: "青袍修士"}
	sh := Shot{ID: 2, Seconds: 4, ShotSize: "近景", Dialogue: "修士：站住", Ambience: "风声"}
	got := CompileShotPrompt(plan, sh, false)
	if !strings.Contains(got, "<d>[Chinese] 站住</d>") {
		t.Fatalf("台词没有进 <d> 标签：\n%s", got)
	}
	if strings.Contains(got, "嘴唇紧闭") {
		t.Fatalf("有台词时不该再压口型：\n%s", got)
	}
}
