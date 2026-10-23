package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// 门检测试钉住三类事：
//  1. RE2 无 lookahead 的 countShots 手工后瞻（引用不是镜头）
//  2. 手册里的实测反例必须被 FAIL 拦住（切点贴脸/台词对不上/图号越界）
//  3. jindan 式合法 plan 必须全过（不许误伤）

func parsePlanJSON(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("plan JSON 无效：%v", err)
	}
	return m
}

func hasGate(items []GateItem, code string) bool {
	for _, it := range items {
		if it.Gate == code {
			return true
		}
	}
	return false
}

// legalPlan 一份完整合法的富模式分镜表（jindan 式）：
// 2 张定妆图、3 段（台词/氛围/台词）、帧数全在网格、总长贴合目标、
// 每段提示词带 [Shot N] + 递增时间戳 + <Picture N> + 与台词表一致的 <d> 块。
const legalPlan = `{
  "title": "验证用",
  "characters": ["a.png", "b.png"],
  "width": 608, "height": 352,
  "gates": {
    "profile": "test",
    "timing": {"fps": 24, "target_seconds": 30.0, "tolerance_seconds": 0.75,
               "min_shot_seconds": 1.2, "max_shot_seconds": 4.5, "require_beats": true},
    "identity": {"stills": 2, "require_ref_images_every_segment": true,
                 "max_ref_images": 9, "require_picture_tags": true},
    "audio": {"dialogue": {"must_transcribe": true}, "ambience": {"must_be_silent": true},
              "loudness_db": [-20, -11], "max_silence_gap": 0.8, "segment_retry": 1},
    "technical": {"min_width": 608, "min_height": 352, "must_have_audio": true, "fade": 0.2},
    "budget": {"wall_seconds_max": 0, "on_exceed": "warn"}
  },
  "segments": [
    {"id": 1, "beat": "setup", "frames": 192,
     "lines": [{"speaker": "S1", "text": "你输了", "pause": 0.4}],
     "prompt": "subject_definitions: <Picture 1> is S1. <Picture 2> is S2.\nsummary: [reference generation + audio reuse] two cultivators face off.\nretention_analysis: <Picture 1> (appears in [Shot 1], [Shot 2]): fully_preserved.\ndetailed_description: [Shot 1] Wide shot, the two stand off at dusk. At 00:04.100 the camera pushes in. [Shot 2] Close on S1 as he says <d>你输了</d>.\noverall_soundscape: wind.\nnon_diegetic_music: N/A"},
    {"id": 2, "beat": "clash", "frames": 277,
     "lines": [],
     "prompt": "detailed_description: [Shot 1] <Picture 1> and <Picture 2> clash, swords spark. At 00:04.500 a quick cut to the staff. [Shot 2] Tracking shot around them. At 00:09.000 the final exchange. [Shot 3] Wide hold.\noverall_soundscape: clashes.\nnon_diegetic_music: N/A"},
    {"id": 3, "beat": "finish", "frames": 260,
     "lines": [{"speaker": "S2", "text": "让开", "pause": 0.35}],
     "prompt": "detailed_description: [Shot 1] Medium shot, <Picture 2> raises the staff. At 00:03.500 cut closer. [Shot 2] On the staff. At 00:07.200 the final beat as S2 says <d>让开</d>. [Shot 3] Wide hold.\noverall_soundscape: wind.\nnon_diegetic_music: N/A"}
  ]
}`

func TestCountShotsSkipsReferences(t *testing.T) {
	// retention_analysis 里的 (appears in [Shot 1], [Shot 2]) 是引用不是镜头；
	// 负向预查在 RE2 里不存在，这里钉住手工后瞻替身的行为。
	prompt := "retention_analysis: <Picture 1> (appears in [Shot 1], [Shot 2]): fully_preserved.\n" +
		"detailed_description: [Shot 1] Wide. [shot 2] Closer. [ Shot 3 ] Hold."
	got := countShots(prompt)
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("countShots = %v， want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("countShots = %v， want %v", got, want)
		}
	}
	if n := len(countShots("没有镜头标记的提示词")); n != 0 {
		t.Fatalf("无标记应返回 0，got %d", n)
	}
}

func TestFilmGatesLegalPlanAllOK(t *testing.T) {
	v, g, stats := CheckFilmGates(parsePlanJSON(t, legalPlan), false)
	if v.HasFail() {
		for _, it := range v.Fails() {
			t.Errorf("合法 plan 被判 FAIL：%s: %s", it.Gate, it.Msg)
		}
	}
	if v.Worst() == "FAIL" {
		t.Fatalf("worst = FAIL")
	}
	// 合并后的门要带上本片覆盖
	if gateFloat(gateMapGet(g, "timing", "target_seconds"), 0) != 30.0 {
		t.Fatalf("target_seconds 合并错：%v", gateMapGet(g, "timing", "target_seconds"))
	}
	// 192+277+260 = 729 帧 → 30.375 - 2×0.2 = 29.975s ↔ 目标 30.0
	// （29.975 浮点上取值 ≥29.975，round 后为 29.98，与 film-gates.py 一致）
	if stats.TotalSeconds != 29.98 {
		t.Fatalf("TotalSeconds = %v，want 29.98", stats.TotalSeconds)
	}
	if stats.Lines != 2 || stats.Stills != 2 {
		t.Fatalf("stats lines/stills = %d/%d，want 2/2", stats.Lines, stats.Stills)
	}
	if len(stats.Shots) != 3 || stats.Shots[2] != 3 {
		t.Fatalf("stats.Shots = %v，want [2 2 3]", stats.Shots)
	}
	if !hasGate(v.Items, "total_seconds") {
		t.Fatalf("缺少 total_seconds 的 OK 记录：%v", v.Items)
	}
}

func TestFilmGatesShotTooShortAndTail(t *testing.T) {
	// 切点贴脸：第二个切点 4.700s，距片尾 5.875s 不足 min_shot 1.2s
	// （同时最后一镜只剩 1.175s，观众读不到信息）
	plan := `{"characters": [], "segments": [
      {"id": 1, "frames": 141, "lines": [],
       "prompt": "detailed_description: [Shot 1] Wide. At 00:04.700 the camera pushes in. [Shot 2] Close."}]}`
	v, _, _ := CheckFilmGates(parsePlanJSON(t, plan), false)
	if !hasGate(v.Fails(), "shot_too_short") {
		t.Fatalf("应拦 shot_too_short，实际：%v", v.Items)
	}
	if !hasGate(v.Fails(), "shot_tail") {
		t.Fatalf("应拦 shot_tail，实际：%v", v.Items)
	}
}

func TestFilmGatesGridFail(t *testing.T) {
	plan := `{"segments": [{"id": 1, "frames": 100, "lines": [], "prompt": "[Shot 1] Wide."}]}`
	v, _, _ := CheckFilmGates(parsePlanJSON(t, plan), false)
	if !hasGate(v.Fails(), "grid") {
		t.Fatalf("100 帧不在 17k+5 网格，应 FAIL grid，实际：%v", v.Items)
	}
}

func TestFilmGatesTotalSeconds(t *testing.T) {
	// 124+124 = 248 帧 → 10.333 - 0.2 = 10.133s，与目标 8.0 相差 2.13 > 容差 0.75
	plan := `{"gates": {"timing": {"target_seconds": 8.0, "tolerance_seconds": 0.75}},
      "segments": [{"id": 1, "frames": 124, "lines": [], "prompt": "[Shot 1] Wide."},
                   {"id": 2, "frames": 124, "lines": [], "prompt": "[Shot 1] Close."}]}`
	v, _, stats := CheckFilmGates(parsePlanJSON(t, plan), false)
	if !hasGate(v.Fails(), "total_seconds") {
		t.Fatalf("应拦 total_seconds，实际：%v（stats=%+v）", v.Items, stats)
	}
	if stats.TotalSeconds != 10.13 {
		t.Fatalf("TotalSeconds = %v，want 10.13", stats.TotalSeconds)
	}
}

func TestFilmGatesLineMismatch(t *testing.T) {
	// 台词表写「你输了」，提示词 <d> 里却是「让开」：TTS 念台词表、画面按提示词，
	// 成片一定对不上。这是手册里的实测反例，必须开机前拦。
	plan := `{"characters": ["a.png"], "segments": [
      {"id": 1, "frames": 124,
       "lines": [{"speaker": "S1", "text": "你输了"}],
       "prompt": "detailed_description: [Shot 1] Wide. S1 says <d>让开</d>."}]}`
	v, _, _ := CheckFilmGates(parsePlanJSON(t, plan), false)
	if !hasGate(v.Fails(), "line_mismatch") {
		t.Fatalf("应拦 line_mismatch，实际：%v", v.Items)
	}
}

func TestFilmGatesDialogueNotInPrompt(t *testing.T) {
	plan := `{"segments": [
      {"id": 1, "frames": 124,
       "lines": [{"speaker": "S1", "text": "你输了"}],
       "prompt": "detailed_description: [Shot 1] Wide, S1 speaks."}]}`
	v, _, _ := CheckFilmGates(parsePlanJSON(t, plan), false)
	if !hasGate(v.Fails(), "dialogue_not_in_prompt") {
		t.Fatalf("应拦 dialogue_not_in_prompt，实际：%v", v.Items)
	}
}

func TestFilmGatesPictureOverflow(t *testing.T) {
	// <Picture 5> 但只有 2 张定妆图：引用不存在的输入，模型会丢身份锚
	plan := `{"characters": ["a.png", "b.png"], "segments": [
      {"id": 1, "frames": 124, "lines": [],
       "prompt": "detailed_description: [Shot 1] <Picture 5> walks in."}]}`
	v, _, _ := CheckFilmGates(parsePlanJSON(t, plan), false)
	if !hasGate(v.Fails(), "picture_overflow") {
		t.Fatalf("应拦 picture_overflow，实际：%v", v.Items)
	}
}

func TestFilmGatesStillsAndRefImages(t *testing.T) {
	plan := `{"characters": ["a.png"],
      "gates": {"identity": {"stills": 2}},
      "segments": [{"id": 1, "frames": 124, "lines": [],
                    "ref_images": false,
                    "prompt": "detailed_description: [Shot 1] Wide."}]}`
	v, _, _ := CheckFilmGates(parsePlanJSON(t, plan), false)
	if !hasGate(v.Fails(), "stills") {
		t.Fatalf("应拦 stills，实际：%v", v.Items)
	}
	if !hasGate(v.Fails(), "ref_images_segment") {
		t.Fatalf("应拦 ref_images_segment，实际：%v", v.Items)
	}
}

func TestFilmGatesMissingGatesWarnNotFail(t *testing.T) {
	// legacy 结构化分镜表没有 gates：WARN 不拦（一棍子打死会废掉所有旧工作流）；
	// 但 requireGates=true 时必须 FAIL。
	plan := `{"segments": [{"id": 1, "frames": 124, "lines": [], "prompt": "[Shot 1] Wide."}]}`
	v, _, _ := CheckFilmGates(parsePlanJSON(t, plan), false)
	if v.HasFail() {
		t.Fatalf("缺 gates 不应 FAIL：%v", v.Items)
	}
	if !hasGate(v.Warns(), "gates_missing") {
		t.Fatalf("缺 gates 应 WARN：%v", v.Items)
	}
	v2, _, _ := CheckFilmGates(parsePlanJSON(t, plan), true)
	if !hasGate(v2.Fails(), "gates_missing") {
		t.Fatalf("requireGates=true 时应 FAIL：%v", v2.Items)
	}
}

func TestFilmGatesSegmentsEmpty(t *testing.T) {
	v, _, _ := CheckFilmGates(map[string]interface{}{"gates": map[string]interface{}{}}, false)
	if !hasGate(v.Fails(), "segments_empty") {
		t.Fatalf("应拦 segments_empty，实际：%v", v.Items)
	}
}

func TestFilmGatesBudget(t *testing.T) {
	// 372 帧 → 372×0.95+3×50 = 503s，预算 400：warn 档 WARN，fail 档 FAIL
	base := `{"gates": {"budget": {"wall_seconds_max": 400, "on_exceed": "%s"}},
      "segments": [{"id": 1, "frames": 124, "lines": [], "prompt": "[Shot 1] A."},
                   {"id": 2, "frames": 124, "lines": [], "prompt": "[Shot 1] B."},
                   {"id": 3, "frames": 124, "lines": [], "prompt": "[Shot 1] C."}]}`
	v1, _, _ := CheckFilmGates(parsePlanJSON(t, strings.Replace(base, "%s", "warn", 1)), false)
	if !hasGate(v1.Warns(), "budget") || v1.HasFail() {
		t.Fatalf("warn 档应只 WARN：%v", v1.Items)
	}
	v2, _, _ := CheckFilmGates(parsePlanJSON(t, strings.Replace(base, "%s", "fail", 1)), false)
	if !hasGate(v2.Fails(), "budget") {
		t.Fatalf("fail 档应 FAIL：%v", v2.Items)
	}
}

func TestFilmGatesResolutionWarn(t *testing.T) {
	plan := `{"width": 512, "height": 288,
      "segments": [{"id": 1, "frames": 124, "lines": [], "prompt": "[Shot 1] Wide."}]}`
	v, _, _ := CheckFilmGates(parsePlanJSON(t, plan), false)
	if !hasGate(v.Warns(), "resolution") {
		t.Fatalf("低于 608×352 应 WARN resolution：%v", v.Items)
	}
	if v.HasFail() {
		t.Fatalf("resolution 只是 WARN 不许 FAIL：%v", v.Items)
	}
}

func TestDeepMergeGates(t *testing.T) {
	base := DefaultFilmGates()
	override := map[string]interface{}{
		"timing": map[string]interface{}{
			"min_shot_seconds": float64(0.8), // 覆盖
			// fps 不声明 → 继承默认 24
		},
		"budget": map[string]interface{}{
			"on_exceed": "fail", // 覆盖
		},
	}
	g := deepMergeGates(base, override)
	if got := gateFloat(gateMapGet(g, "timing", "min_shot_seconds"), -1); got != 0.8 {
		t.Fatalf("min_shot_seconds = %v，want 0.8", got)
	}
	if got := gateInt(gateMapGet(g, "timing", "fps"), -1); got != 24 {
		t.Fatalf("fps 应继承默认 24，got %v", got)
	}
	if got := gateStr(gateMapGet(g, "budget", "on_exceed")); got != "fail" {
		t.Fatalf("on_exceed = %v，want fail", got)
	}
	// identity 整段没碰 → 原样继承
	if got := gateInt(gateMapGet(g, "identity", "max_ref_images"), -1); got != 9 {
		t.Fatalf("max_ref_images 应继承 9，got %v", got)
	}
	// 合并不许写回 base（默认门是共享模板）
	if got := gateFloat(gateMapGet(base, "timing", "min_shot_seconds"), -1); got != 1.2 {
		t.Fatalf("base 被合并污染：min_shot_seconds = %v", got)
	}
}

func TestFramesInGrid(t *testing.T) {
	for _, ok := range []int{5, 22, 73, 124, 141, 158, 362} {
		if !framesInGrid(ok) {
			t.Errorf("%d 应在网格上", ok)
		}
	}
	for _, bad := range []int{0, 1, 100, 123, 125, 363} {
		if framesInGrid(bad) {
			t.Errorf("%d 不在网格上", bad)
		}
	}
}

func TestTotalSecondsGate(t *testing.T) {
	// 逐段相加，每处接缝吃掉 fade
	got := totalSecondsGate([]int{124, 124}, 0.2, 24)
	if got != 10.13 {
		t.Fatalf("total = %v，want 10.13", got)
	}
	if got := totalSecondsGate(nil, 0.2, 24); got != 0 {
		t.Fatalf("空 = %v，want 0", got)
	}
}
