package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"flowedge/backend/model"
)

// 质检门拦截：require_audio=true 而产物无音轨 → 必须 FAIL（阻断）
func TestQualityGate_RequireAudioBlocksSilentVideo(t *testing.T) {
	e := &EngineV2{}
	node := &model.NodeInstance{
		ID: "g", Type: "quality_gate",
		Data: map[string]interface{}{"require_audio": true},
	}
	// 造一个只有视频流的 mp4（testsrc + -an）
	path := makeSilentVideo(t)
	upstream := map[string]interface{}{
		"v1": map[string]interface{}{"video": path},
	}
	out, err := e.execQualityGateNode(context.Background(), node, upstream)
	if err == nil {
		t.Fatal("无音轨视频在 require_audio 门下应 FAIL")
	}
	if !containsFold(err.Error(), "质检未通过") {
		t.Fatalf("错误信息应说明质检未通过，got %v", err)
	}
	// 判不过必须**带着逐项检查**一起失败。没有 checks，证据层只能判「没测到」，
	// 一次真实的质量不合格就被记成 unscored，评测与自进化同时失去信号。
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("判不过时应返回带 checks 的产物，got %+v", out)
	}
	if m["gate"] != "fail" {
		t.Fatalf("产物 gate 应为 fail，got %v", m["gate"])
	}
	checks, _ := m["checks"].([]gateCheck)
	if len(checks) == 0 {
		t.Fatal("产物必须带 checks（证据层用有没有 checks 判 blocked / crashed）")
	}
}

// 同一条产物在无 require_audio 规则时应放行
func TestQualityGate_PassesWithoutAudioRule(t *testing.T) {
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "quality_gate", Data: map[string]interface{}{}}
	path := makeSilentVideo(t)
	upstream := map[string]interface{}{
		"v1": map[string]interface{}{"video": path},
	}
	out, err := e.execQualityGateNode(context.Background(), node, upstream)
	if err != nil {
		t.Fatalf("无额外规则应放行，got %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok || m["gate"] != "pass" {
		t.Fatalf("输出应带 gate=pass，got %+v", out)
	}
}

// 缺输入产物必须显式失败（不能伪造通过）
func TestQualityGate_NoArtifactFails(t *testing.T) {
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "quality_gate", Data: map[string]interface{}{}}
	_, err := e.execQualityGateNode(context.Background(), node, map[string]interface{}{})
	if err == nil || !containsFold(err.Error(), "缺少输入产物") {
		t.Fatalf("缺产物应显式失败，got %v", err)
	}
}

// 静图没有时长是正常的：配了 min_width/min_height 的静图门必须放行，
// 且每项检查都带实测值。2026-10-23 真渲闭环（wf 56 still-a，MOSS 真实出图）
// 第一次抓到：probeGateMedia 曾无条件要求秒数>0，一张完好的 PNG 被判
// 「文件可能损坏」——尺寸检查在崩溃后面永远执行不到，门配了等于没配。
func TestQualityGate_StillImagePassesWithMeasuredChecks(t *testing.T) {
	path := makeStillPng(t)
	// 探测层：完好静图不得被当成坏文件
	info, err := probeGateMedia(path)
	if err != nil {
		t.Fatalf("完好静图不应被探测层拒绝：%v", err)
	}
	if info.width != 320 || info.height != 240 {
		t.Fatalf("尺寸应读到 320x240，got %dx%d", info.width, info.height)
	}
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "quality_gate",
		Data: map[string]interface{}{"min_width": float64(300), "min_height": float64(200)}}
	upstream := map[string]interface{}{
		"v1": map[string]interface{}{"image": path},
	}
	out, err := e.execQualityGateNode(context.Background(), node, upstream)
	if err != nil {
		t.Fatalf("320x240 静图过 300x200 门应放行，got %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok || m["gate"] != "pass" {
		t.Fatalf("输出应带 gate=pass，got %+v", out)
	}
	checks, _ := m["checks"].([]gateCheck)
	seen := map[string]bool{}
	for _, c := range checks {
		seen[c.Name] = true
		if !c.OK {
			t.Fatalf("所有检查应通过，got %+v", c)
		}
		if c.Detail == "" {
			t.Fatalf("每项检查必须带实测值（信封要能自解释），got %+v", c)
		}
	}
	if !seen["min_width"] || !seen["min_height"] {
		t.Fatalf("应产出 min_width/min_height 两项，got %v", seen)
	}
}

// 坏文件仍然必须硬错：修静图不能顺手把「假通过」的门打开
// （空规则 + 坏文件 → 零检查放行，比崩溃更糟）。
func TestProbeGateMedia_CorruptFileStillErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.mp4")
	if err := os.WriteFile(path, []byte("this is not a media file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := probeGateMedia(path); err == nil {
		t.Fatal("读不到任何媒体事实的坏文件必须报错，不许放行")
	}
}

// makeStillPng 造一张 320x240 测试静图（与视频用例同一模式：ffmpeg 不可用则跳过）
func makeStillPng(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg 不可用，跳过")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "still.png")
	cmd := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1",
		"-frames:v", "1", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("造测试静图失败：%v %s", err, out)
	}
	return path
}

// makeSilentVideo 造一条 2 秒无音轨测试视频
func makeSilentVideo(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg 不可用，跳过")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "silent.mp4")
	cmd := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=16",
		"-an", "-pix_fmt", "yuv420p", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("造测试视频失败：%v %s", err, out)
	}
	return path
}

// 音画同步检查：0.1s drift 应在 0.15s 容差内（fail 级）
func TestGateCheck_AVDriftWithinTolerance(t *testing.T) {
	info := &gateMediaInfo{hasVideo: true, hasAudio: true, kind: "video", videoSecond: 5.0, audioSecond: 5.1}
	r := gateRules{durationTol: 0.15}
	checks := runGateChecks(context.Background(), "", info, r)
	found := false
	for _, c := range checks {
		if c.Name == "av_drift" {
			found = true
			if !c.OK {
				t.Fatalf("0.1s drift 应在 0.15s 容差内，got %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("应产出 av_drift 检查项")
	}
}

// 超差必须 FAIL 级阻断
func TestGateCheck_AVDriftExceedsTolerance(t *testing.T) {
	info := &gateMediaInfo{hasVideo: true, hasAudio: true, kind: "video", videoSecond: 5.0, audioSecond: 5.5}
	r := gateRules{durationTol: 0.15}
	checks := runGateChecks(context.Background(), "", info, r)
	for _, c := range checks {
		if c.Name == "av_drift" && (c.OK || c.Level != "fail") {
			t.Fatalf("0.5s drift 应 fail 级不过，got %+v", c)
		}
	}
}

// loudness_min_db 是**负的 dB 阈值**（-20 比 -14 安静）。解析器必须收负数：
// 曾经用 v > 0 的通用浮点解析，把一切真实阈值静默丢弃——门看起来配了，
// 其实永远不查。这条盯着不许退回去。
func TestParseGateRules_LoudnessMinDBSigned(t *testing.T) {
	node := &model.NodeInstance{ID: "g", Type: "quality_gate",
		Data: map[string]interface{}{"loudness_min_db": float64(-20)}}
	if r := parseGateRules(node); r.loudnessMinDB != -20 {
		t.Fatalf("负阈值必须照收，got %v", r.loudnessMinDB)
	}
	// 0 = 没配：不能变成「按 0dB 查一遍」制造假 FAIL
	node2 := &model.NodeInstance{ID: "g", Type: "quality_gate",
		Data: map[string]interface{}{"loudness_min_db": float64(0)}}
	if r2 := parseGateRules(node2); r2.loudnessMinDB != 0 {
		t.Fatalf("0 应视为未配置，got %v", r2.loudnessMinDB)
	}
}

// 阈值要真的落到检查项里（不是只存进规则对象）
func TestGateCheck_LoudnessMinUsesNegativeThreshold(t *testing.T) {
	path := makeQuietVideo(t)
	info, err := probeGateMedia(path)
	if err != nil {
		t.Skipf("探测失败：%v", err)
	}
	r := gateRules{loudnessMinDB: -20}
	checks := runGateChecks(context.Background(), path, info, r)
	found := false
	for _, c := range checks {
		if c.Name == "loudness_min" {
			found = true
			if !containsFold(c.Detail, "-20") {
				t.Fatalf("检查项应带上负阈值 -20，got %+v", c)
			}
			if c.OK {
				t.Fatalf("-45dB 的暗嗓应低于 -20dB 门限，got %+v", c)
			}
			if c.Level != "warn" {
				t.Fatalf("响度是预警级（真 enforcement 在后期两遍法），got %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("配了 loudness_min_db 就必须产出 loudness_min 检查项")
	}
}

// makeQuietVideo 造一条带 -45dB 暗噪音频的 2 秒测试视频
func makeQuietVideo(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg 不可用，跳过")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "quiet.mp4")
	cmd := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=16",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-af", "volume=-45dB",
		"-shortest", "-pix_fmt", "yuv420p", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("造测试视频失败：%v %s", err, out)
	}
	return path
}
