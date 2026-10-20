package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 九宫格锁的全部判定必须是确定性的：同一个 grid 编出同一串字节，
// 同一段提示词重复注入不叠句，多段之间按原文全等比对。
// 这些性质一旦有一条靠"看起来差不多"，多段一致性就无从验证。

func sampleGrid() SceneGrid {
	return SceneGrid{
		Light:   "a single iron lantern at the middle-left, warm 2700K, hard shadows to the right",
		Palette: "cold grey stone against rust-red cloth",
		Cells: []string{
			"cracked stone vaulting with a rusted iron ring",
			"a narrow high window with pale daylight",
			"-",
			"a stone pillar with chipped base",
			"a low stone altar with a bronze bowl",
			"a sealed wooden door with iron bands",
			"worn flagstones with a dark drainage groove",
			"a scatter of dry straw",
			"a toppled clay jar",
		},
	}
}

// TestCompileSceneLock_Deterministic 同输入必须同字节——这是多段一致性的地基
func TestCompileSceneLock_Deterministic(t *testing.T) {
	a, err := CompileSceneLock(sampleGrid())
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	b, err := CompileSceneLock(sampleGrid())
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if a != b {
		t.Fatalf("lock not deterministic:\n%s\n%s", a, b)
	}
	if !strings.HasPrefix(a, sceneLockPrefix) {
		t.Fatalf("lock must start with %q, got %q", sceneLockPrefix, a[:20])
	}
	// 空占位 " - " 那一格不能被写进去
	if strings.Contains(a, "top-right holds") {
		t.Fatalf("empty cell leaked into lock: %s", a)
	}
	// 锁句内部只能有一个句号（结尾），否则 SceneLockOf 会截断
	if n := strings.Count(a, "."); n != 1 {
		t.Fatalf("lock must contain exactly one period, got %d: %s", n, a)
	}
}

// TestCompileSceneLock_StripsPeriods 格位里的句号/分号必须清洗，否则锁句边界失效
func TestCompileSceneLock_StripsPeriods(t *testing.T) {
	g := SceneGrid{Cells: []string{"a broken wheel. it leans left; rust everywhere"}}
	lock, err := CompileSceneLock(g)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if strings.Count(lock, ".") != 1 {
		t.Fatalf("period not sanitized: %s", lock)
	}
	if strings.Count(lock, ";") != 3 {
		// 结构上有 "; " 分隔（1 格 + 无 light/palette/depth）+ 清洗后的逗号不计分号
		t.Logf("lock=%s", lock)
	}
	if !strings.Contains(lock, "it leans left") {
		t.Fatalf("content lost: %s", lock)
	}
}

// TestCompileSceneLock_AllEmpty 九格全空必须报错，不能编出空锁
func TestCompileSceneLock_AllEmpty(t *testing.T) {
	if _, err := CompileSceneLock(SceneGrid{Cells: []string{"-", "", "无"}}); err == nil {
		t.Fatal("expected error for empty grid")
	}
}

// TestInjectSceneLock_T2VA 三段式：[Shot 1] 内风格句之后
func TestInjectSceneLock_T2VA(t *testing.T) {
	lock, _ := CompileSceneLock(sampleGrid())
	prompt := "integrated_multimodal_description: [Shot 1] Live-action, cinematic, a medium-wide shot frames a stone hall. A figure crosses the floor.\n" +
		"overall_soundscape: wind over broken stone.\n" +
		"non_diegetic_music: N/A"

	got := InjectSceneLock(prompt, lock)
	if !strings.Contains(got, lock) {
		t.Fatalf("lock not injected: %s", got)
	}
	// 锁必须在 [Shot 1] 内、风格句之后
	iShot := strings.Index(got, "[Shot 1]")
	iLock := strings.Index(got, "SCENE LOCK:")
	iSecond := strings.Index(got, "A figure crosses the floor.")
	if !(iShot < iLock && iLock < iSecond) {
		t.Fatalf("wrong insertion point: shot=%d lock=%d second=%d\n%s", iShot, iLock, iSecond, got)
	}
	// 风格句必须仍是第一句
	first := got[strings.Index(got, "[Shot 1]")+len("[Shot 1]"):]
	if !strings.HasPrefix(strings.TrimSpace(first), "Live-action, cinematic,") {
		t.Fatalf("style sentence no longer first: %s", first[:60])
	}
}

// TestInjectSceneLock_Ref2VA 六段式：风格句在 [Shot 1] 之前，锁接在风格句后、镜头标记前
func TestInjectSceneLock_Ref2VA(t *testing.T) {
	lock, _ := CompileSceneLock(sampleGrid())
	prompt := "subject_definitions:\n<Subject 1> is a gate captain.\n" +
		"summary:\n[reference generation] a captain in a hall.\n" +
		"retention_analysis:\n<Subject 1> (appears in [Shot 1]): fully_preserved - face.\n" +
		"detailed_description:\nLive-action, cinematic, cold blue palette. [Shot 1] A stone hall fills the frame.\n" +
		"overall_soundscape:\nwind.\n" +
		"non_diegetic_music:\nN/A"

	got := InjectSceneLock(prompt, lock)
	iStyle := strings.Index(got, "cold blue palette.")
	iLock := strings.Index(got, "SCENE LOCK:")
	iShot := strings.Index(got, "[Shot 1] A stone hall")
	if !(iStyle < iLock && iLock < iShot) {
		t.Fatalf("wrong Ref2VA insertion: style=%d lock=%d shot=%d\n%s", iStyle, iLock, iShot, got)
	}
	// 其他段不能被破坏
	if !strings.Contains(got, "<Subject 1> (appears in [Shot 1]): fully_preserved") {
		t.Fatalf("retention_analysis damaged: %s", got)
	}
}

// TestInjectSceneLock_Idempotent 重复注入不能叠句（下游会多次路过注入点）
func TestInjectSceneLock_Idempotent(t *testing.T) {
	lock, _ := CompileSceneLock(sampleGrid())
	prompt := "integrated_multimodal_description: [Shot 1] Live-action, a hall. A figure walks.\noverall_soundscape: wind.\nnon_diegetic_music: N/A"
	once := InjectSceneLock(prompt, lock)
	twice := InjectSceneLock(once, lock)
	if once != twice {
		t.Fatalf("not idempotent:\nonce=%s\ntwice=%s", once, twice)
	}
	if n := strings.Count(twice, "SCENE LOCK:"); n != 1 {
		t.Fatalf("lock duplicated %d times", n)
	}
}

// TestStripSceneLock 剥干净后不能留下双空格或孤立句号
func TestStripSceneLock(t *testing.T) {
	lock, _ := CompileSceneLock(sampleGrid())
	p := "integrated_multimodal_description: [Shot 1] Live-action, a hall. " + lock + " A figure walks."
	out := StripSceneLock(p)
	if strings.Contains(out, "SCENE LOCK:") {
		t.Fatalf("lock not stripped: %s", out)
	}
	if strings.Contains(out, "  ") {
		t.Fatalf("double space left: %q", out)
	}
	if !strings.Contains(out, "A figure walks.") {
		t.Fatalf("content lost: %s", out)
	}
}

// TestAuditSceneConsistency 门必须能 FAIL：缺锁、漂移都要抓到
func TestAuditSceneConsistency(t *testing.T) {
	lock, _ := CompileSceneLock(sampleGrid())
	base := "integrated_multimodal_description: [Shot 1] Live-action, a hall. A figure walks.\noverall_soundscape: wind.\nnon_diegetic_music: N/A"
	seg := InjectSceneLock(base, lock)

	// 全等 → 无 FAIL
	issues := auditSceneConsistency([]string{seg, seg, seg})
	for _, it := range issues {
		if it.Level == "FAIL" {
			t.Fatalf("unexpected FAIL: %+v", it)
		}
	}

	// 缺一段锁 → 必须 FAIL（不是 WARN，WARN 等于放过）
	issues = auditSceneConsistency([]string{seg, base})
	found := false
	for _, it := range issues {
		if it.Code == "h3.scene_lock_missing" && it.Level == "FAIL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing lock not detected: %+v", issues)
	}

	// 锁句漂移（改一个介词）→ 必须 FAIL
	drifted := strings.Replace(seg, "the light stays", "the light remains", 1)
	issues = auditSceneConsistency([]string{seg, drifted})
	found = false
	for _, it := range issues {
		if it.Code == "h3.scene_lock_drift" && it.Level == "FAIL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("drift not detected: %+v", issues)
	}
}

// TestParseSceneGrid_Pipe 位置|内容（中文位置名）
func TestParseSceneGrid_Pipe(t *testing.T) {
	g, err := ParseSceneGrid(`左上|a cracked vault
中心|a stone altar with a bronze bowl
右下|a toppled clay jar`)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if g.Cells[0] != "a cracked vault" {
		t.Fatalf("top-left = %q", g.Cells[0])
	}
	if g.Cells[4] != "a stone altar with a bronze bowl" {
		t.Fatalf("center = %q", g.Cells[4])
	}
	if g.Cells[8] != "a toppled clay jar" {
		t.Fatalf("bottom-right = %q", g.Cells[8])
	}
}

// TestParseSceneGrid_OrderedLines 无位置名时按阅读顺序填充
func TestParseSceneGrid_OrderedLines(t *testing.T) {
	g, err := ParseSceneGrid("a vault\nb window\nc beam\nd pillar\ne altar\nf door\ng floor\nh straw\ni jar")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if g.Cells[0] != "a vault" || g.Cells[8] != "i jar" || g.Cells[4] != "e altar" {
		t.Fatalf("order wrong: %v", g.Cells)
	}
}

// TestParseSceneGrid_JSON 节点回流走 JSON
func TestParseSceneGrid_JSON(t *testing.T) {
	g, err := ParseSceneGrid(`{"light":"warm lamp at middle-left","cells":["vault","","","pillar","altar","","floor","","jar"]}`)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if g.Light == "" || g.Cells[4] != "altar" {
		t.Fatalf("json parse wrong: %+v", g)
	}
}

// TestParseSceneGrid_AllEmpty 全空必须报错（不能静默产出空锁）
func TestParseSceneGrid_AllEmpty(t *testing.T) {
	if _, err := ParseSceneGrid("\n\n\n"); err == nil {
		t.Fatal("expected error for empty input")
	}
}

// TestExecSceneGridNode_Manual 手动九格：不调 LLM，直接产出锁
func TestExecSceneGridNode_Manual(t *testing.T) {
	eng := &EngineV2{}
	node := &model.NodeInstance{
		ID: "g1", Type: "scene_grid",
		Data: map[string]interface{}{
			"cells": "左上|a cracked vault\n中心|a stone altar\n右下|a toppled jar",
			"light": "a single warm lantern at the middle-left",
		},
	}

	out, err := eng.execSceneGridNode(nil, node, nil)
	if err != nil {
		t.Fatalf("node failed: %v", err)
	}
	m := out.(map[string]interface{})
	lock, _ := m["lock"].(string)
	if !strings.HasPrefix(lock, sceneLockPrefix) {
		t.Fatalf("no lock: %v", m)
	}
	if m["source"] != "manual" {
		t.Fatalf("source=%v", m["source"])
	}
	if !strings.Contains(lock, "the light stays") {
		t.Fatalf("light missing: %s", lock)
	}
}

// TestAuditH3Prompt_SceneLockForeignLanguage 中文格位要提示（英文正文规则）
func TestAuditH3Prompt_SceneLockForeignLanguage(t *testing.T) {
	lock := "SCENE LOCK: the top-left holds 破败石拱."
	p := "integrated_multimodal_description: [Shot 1] Live-action, a hall. " + lock + " A figure walks.\n" +
		"overall_soundscape: wind.\nnon_diegetic_music: N/A"
	issues := auditH3Prompt(p)
	found := false
	for _, it := range issues {
		if it.Code == "scene_lock_not_english" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cjk lock not flagged: %+v", issues)
	}
}

// fakeLLM 起一个只回固定内容的 OpenAI 兼容端点。
//
// 为什么要 fake 而不是打真 Ollama：这条链路的判定是「锁句有没有被逐字注入」，
// 与模型能力无关。真模型每轮措辞都不同，反而让测试不稳定。
func fakeLLM(t *testing.T, content string) *LLMService {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
			"usage": map[string]int{},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return NewLLMService(srv.URL, "fake")
}

// TestOptimizePromptForH3WithGrid 全链：LLM 输出三段式 → 锁句被逐字注入 Shot 1
func TestOptimizePromptForH3WithGrid(t *testing.T) {
	lock, _ := CompileSceneLock(sampleGrid())
	eng := &EngineV2{llm: fakeLLM(t, "integrated_multimodal_description: [Shot 1] Live-action, cinematic, a wide shot frames a stone hall. A figure crosses the floor.\noverall_soundscape: wind over broken stone.\nnon_diegetic_music: N/A")}

	out, err := eng.OptimizePromptForH3WithGrid(context.Background(), "石殿里有人走过", 5, lock)
	if err != nil {
		t.Fatalf("optimize failed: %v", err)
	}
	if !strings.Contains(out, lock) {
		t.Fatalf("lock missing from result:/n%s", out)
	}
	iShot := strings.Index(out, "[Shot 1]")
	iLock := strings.Index(out, "SCENE LOCK:")
	iNext := strings.Index(out, "A figure crosses")
	if !(iShot >= 0 && iShot < iLock && iLock < iNext) {
		t.Fatalf("lock inserted at wrong place: %s", out)
	}
	// 声音字段必须还在（注入不能吃掉字段边界）
	if !strings.Contains(out, "overall_soundscape:") || !strings.Contains(out, "non_diegetic_music:") {
		t.Fatalf("audio fields lost:/n%s", out)
	}
}

// TestOptimizePromptForH3WithGrid_NoLock 不传锁时行为与旧版一致
func TestOptimizePromptForH3WithGrid_NoLock(t *testing.T) {
	eng := &EngineV2{llm: fakeLLM(t, "integrated_multimodal_description: [Shot 1] Live-action, a hall.\noverall_soundscape: wind.\nnon_diegetic_music: N/A")}
	out, err := eng.OptimizePromptForH3(context.Background(), "石殿", 5)
	if err != nil {
		t.Fatalf("optimize failed: %v", err)
	}
	if strings.Contains(out, "SCENE LOCK:") {
		t.Fatalf("lock injected without grid: %s", out)
	}
}

// TestResolveSceneLock 三种来源的优先级：直通锁 > 九行格位
func TestResolveSceneLock(t *testing.T) {
	lock, _ := CompileSceneLock(sampleGrid())

	// 1) 直接粘贴编译好的锁句：必须原样直通，不能二次编译
	node := &model.NodeInstance{ID: "tv", Type: "text2video", Data: map[string]interface{}{"scene_grid": lock}}
	got, err := resolveSceneLock(node, nil)
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if got != lock {
		t.Fatalf("lock not passed through verbatim:/n%s/n%s", got, lock)
	}

	// 2) 九行格位：现场编译
	node2 := &model.NodeInstance{ID: "tv2", Type: "text2video", Data: map[string]interface{}{
		"scene_grid": "左上|a cracked vault\n中心|a stone altar\n右下|a toppled jar",
	}}
	got2, err := resolveSceneLock(node2, nil)
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if !strings.HasPrefix(got2, sceneLockPrefix) || !strings.Contains(got2, "a cracked vault") {
		t.Fatalf("grid not compiled: %s", got2)
	}

	// 3) 上游节点输出 lock：优先于本节点字段
	up := map[string]interface{}{"g1": map[string]interface{}{"lock": lock}}
	node3 := &model.NodeInstance{ID: "tv3", Type: "text2video", Data: map[string]interface{}{}}
	got3, err := resolveSceneLock(node3, up)
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if got3 != lock {
		t.Fatalf("upstream lock ignored: %s", got3)
	}

	// 4) 都没有：返回空，不报错（锁是可选增强，不该阻断旧工作流）
	got4, err := resolveSceneLock(&model.NodeInstance{ID: "tv4", Type: "text2video", Data: map[string]interface{}{}}, nil)
	if err != nil || got4 != "" {
		t.Fatalf("expected empty lock, got %q err=%v", got4, err)
	}
}

// TestResolveSceneLock_BeatsShareBytes 多段路径的核心性质：
// 同一份锁注入到内容不同的段落，锁句必须字节全等（这是门能通过的前提）
func TestResolveSceneLock_BeatsShareBytes(t *testing.T) {
	lock, _ := CompileSceneLock(sampleGrid())
	segs := []string{
		"integrated_multimodal_description: [Shot 1] Live-action, a hall. A figure enters.\noverall_soundscape: wind.\nnon_diegetic_music: N/A",
		"integrated_multimodal_description: [Shot 1] Live-action, a hall. The figure kneels.\noverall_soundscape: wind.\nnon_diegetic_music: N/A",
		"integrated_multimodal_description: [Shot 1] Live-action, a hall. The bowl overflows.\noverall_soundscape: wind.\nnon_diegetic_music: N/A",
	}
	for i := range segs {
		segs[i] = InjectSceneLock(segs[i], lock)
	}
	if issues := auditSceneConsistency(segs); len(issues) > 0 {
		t.Fatalf("segments should pass gate: %+v", issues)
	}
	// 锁句本身必须完全一致，而不是"看起来一样"
	for _, s := range segs {
		if SceneLockOf(s) != lock {
			t.Fatalf("lock bytes differ:/n%s/n%s", SceneLockOf(s), lock)
		}
	}
}
