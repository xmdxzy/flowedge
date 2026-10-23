package service

import (
	"encoding/json"
	"os"
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

// ---- 富模式（scripts/shot-film.py 实战 schema）解析 ----

const richPlanJSON = `{
  "title": "斗法",
  "characters": ["a.png", "b.png"],
  "width": 608, "height": 352,
  "ref_audio": ["voice.wav"],
  "ambience": "amb.wav",
  "gates": {"profile": "duel-30s-3shot", "timing": {"target_seconds": 30.0}},
  "segments": [
    {"id": 1, "beat": "setup", "frames": 192, "lead": 0.25,
     "lines": [{"speaker": "S1", "text": "你输了", "pause": 0.4}],
     "prompt": "subject_definitions: <Picture 1> is S1."},
    {"id": 2, "beat": "clash", "frames": 277,
     "lines": [],
     "prompt": "detailed_description: [Shot 1] Wide."}
  ]
}`

func TestParseShotPlanRichSchema(t *testing.T) {
	p, err := ParseShotPlan(richPlanJSON)
	if err != nil {
		t.Fatalf("富模式解析失败：%v", err)
	}
	// segments 别名合并进 Shots，且别名本身清空（只留一个真相源）
	if len(p.Shots) != 2 || len(p.Segments) != 0 {
		t.Fatalf("segments 别名合并错：shots=%d segments=%d", len(p.Shots), len(p.Segments))
	}
	if len(p.Characters) != 2 || p.Characters[0] != "a.png" {
		t.Fatalf("characters 解析错：%v", p.Characters)
	}
	if len(p.Gates) == 0 {
		t.Fatalf("gates 丢失")
	}
	if p.Width != 608 || p.Height != 352 {
		t.Fatalf("width/height 解析错：%d×%d", p.Width, p.Height)
	}
	// 显式帧数不许被秒数吸附覆盖（总长数学建立在声明值上）
	if p.Shots[0].Frames != 192 || p.Shots[1].Frames != 277 {
		t.Fatalf("显式 frames 被改：%d/%d", p.Shots[0].Frames, p.Shots[1].Frames)
	}
	// 手写提示词原样保留
	if !strings.Contains(p.Shots[0].Prompt, "subject_definitions") {
		t.Fatalf("富模式 prompt 丢失：%q", p.Shots[0].Prompt)
	}
	// 台词表 + 缺省值（pause 缺省 0.35、speed 缺省 1.0、lead 缺省 0.25）
	if len(p.Shots[0].Lines) != 1 || p.Shots[0].Lines[0].Text != "你输了" {
		t.Fatalf("lines 解析错：%+v", p.Shots[0].Lines)
	}
	if p.Shots[0].Lines[0].Pause != 0.4 || p.Shots[0].Lines[0].Speed != 1.0 {
		t.Fatalf("line 参数错：%+v", p.Shots[0].Lines[0])
	}
	if len(p.Shots[1].Lines) != 0 {
		t.Fatalf("空 lines 应为空，got %+v", p.Shots[1].Lines)
	}
	if p.Shots[1].Lead != 0.25 {
		t.Fatalf("lead 缺省应 0.25，got %v", p.Shots[1].Lead)
	}
	// beat 叙事功能名要进来（节奏权重的键）
	if p.Shots[0].Beat != "setup" {
		t.Fatalf("beat 丢失：%q", p.Shots[0].Beat)
	}
}

func TestPlanLineExplicitZeroPause(t *testing.T) {
	// 显式 pause=0 必须保持 0（「没给」与「给了 0」是两回事）
	var ln PlanLine
	if err := json.Unmarshal([]byte(`{"text":"x","pause":0,"speed":0.5}`), &ln); err != nil {
		t.Fatalf("unmarshal：%v", err)
	}
	if ln.Pause != 0 || ln.Speed != 0.5 {
		t.Fatalf("显式值被缺省覆盖：%+v", ln)
	}
}

func TestShotHasDialogueBothSchemas(t *testing.T) {
	rich := Shot{Lines: []PlanLine{{Text: "你输了"}}}
	if !rich.HasDialogue() {
		t.Fatalf("富模式台词表应判定有台词")
	}
	structured := Shot{Dialogue: "修士：站住"}
	if !structured.HasDialogue() {
		t.Fatalf("结构化 dialogue 应判定有台词")
	}
	silent := Shot{Dialogue: "无对白"}
	if silent.HasDialogue() {
		t.Fatalf("无对白不应判定有台词")
	}
	empty := Shot{}
	if empty.HasDialogue() {
		t.Fatalf("空字段不应判定有台词")
	}
}

func TestShotDialogueText(t *testing.T) {
	// 富模式取台词表第一句
	rich := Shot{Lines: []PlanLine{{Text: " "}, {Text: "让开"}}}
	if got := rich.DialogueText(); got != "让开" {
		t.Fatalf("DialogueText = %q，want 让开", got)
	}
	// 结构化模式切「身份：原话」（中文冒号）
	structured := Shot{Dialogue: "黑袍人：你输了"}
	if got := structured.DialogueText(); got != "你输了" {
		t.Fatalf("DialogueText = %q，want 你输了", got)
	}
	// 英文冒号同样切
	if got := (&Shot{Dialogue: "S1: move"}).DialogueText(); got != "move" {
		t.Fatalf("DialogueText = %q，want move", got)
	}
}

func TestParseShotPlanStructuredStillWorks(t *testing.T) {
	// legacy 结构化分镜表（storyboard 产出）行为不回退
	raw := `{"title":"t","character":"青袍修士","ambience":"风声","shots":[
      {"id":1,"seconds":5,"shot_size":"中景","dialogue":"无对白","transition":"cut"}]}`
	p, err := ParseShotPlan(raw)
	if err != nil {
		t.Fatalf("结构化解析失败：%v", err)
	}
	if len(p.Shots) != 1 || p.Shots[0].Frames != SnapFrames(5) {
		t.Fatalf("结构化 frames 吸附错：%d", p.Shots[0].Frames)
	}
	if p.Shots[0].Dialogue != "无对白" || p.Shots[0].Transition != "cut" {
		t.Fatalf("结构化字段丢失：%+v", p.Shots[0])
	}
}

func TestAmbienceFileResolution(t *testing.T) {
	dir := t.TempDir()
	wav := dir + "/amb.wav"
	if err := os.WriteFile(wav, []byte("x"), 0600); err != nil {
		t.Fatalf("写文件：%v", err)
	}
	// 富模式：ambience 是文件路径 → 解析出来
	p := &ShotPlan{Ambience: wav}
	if got := p.AmbienceFile(Shot{}); got != wav {
		t.Fatalf("AmbienceFile = %q，want %q", got, wav)
	}
	// 结构化模式：ambience 是文字描述 → 返回空（不能把描述当路径）
	p2 := &ShotPlan{Ambience: "远处持续的环境底噪"}
	if got := p2.AmbienceFile(Shot{}); got != "" {
		t.Fatalf("文字描述不该解析成文件：%q", got)
	}
	// 段级优先于 plan 级
	segWav := dir + "/seg.wav"
	if err := os.WriteFile(segWav, []byte("x"), 0600); err != nil {
		t.Fatalf("写文件：%v", err)
	}
	if got := p.AmbienceFile(Shot{Ambience: segWav}); got != segWav {
		t.Fatalf("段级 ambience 应优先：%q", got)
	}
}
