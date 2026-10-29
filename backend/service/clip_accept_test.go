package service

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// 单镜验收的用例：合格镜不得误判，缺音轨/帧数不足必须判死。
// 素材用 ffmpeg 现造，零 GPU 依赖。

func makeSilentClip(t *testing.T, dir, name string, sec float64) string {
	t.Helper()
	out := filepath.Join(dir, name)
	cmd := exec.Command(ffmpegBin(), "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=24:duration="+strconv.FormatFloat(sec, 'f', 2, 64),
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-an", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("合成无音轨素材失败：%v（%s）", err, string(b))
	}
	return out
}

func TestClipAcceptable_GoodClipPasses(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := makeTestClip(t, dir, "good.mp4", 1.0, 1.0)
	ok, issues := clipAcceptable(clip, Shot{Frames: 24}, 0.2)
	if !ok {
		t.Fatalf("合格单镜被误判：%v", issues)
	}
}

func TestClipAcceptable_NoAudioFails(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := makeSilentClip(t, dir, "silent.mp4", 1.0)
	ok, issues := clipAcceptable(clip, Shot{Frames: 24}, 0.2)
	if ok {
		t.Fatalf("没有音轨的单镜必须判不合格")
	}
	if len(issues) == 0 {
		t.Fatalf("判不合格时必须给出原因")
	}
}

func TestClipAcceptable_FrameShortfallFails(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := makeTestClip(t, dir, "short.mp4", 1.0, 1.0)   // 24 帧
	ok, _ := clipAcceptable(clip, Shot{Frames: 100}, 0.2) // 计划 100 帧、95% = 95
	if ok {
		t.Fatalf("实际 24 帧 vs 计划 100 帧，必须判不合格")
	}
}

// TestMarkNeedsReshoot_OnlyBadShot 定点重拍的核心决策：只标坏的那一镜。
func TestMarkNeedsReshoot_OnlyBadShot(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	good1 := makeTestClip(t, dir, "g1.mp4", 1.0, 1.0)
	badNoAudio := makeSilentClip(t, dir, "b.mp4", 1.0)
	good2 := makeTestClip(t, dir, "g2.mp4", 1.0, 1.0)

	clips := []string{good1, badNoAudio, good2}
	shots := []Shot{{ID: 1, Frames: 24}, {ID: 2, Frames: 24}, {ID: 3, Frames: 24}}
	bad := markNeedsReshoot(clips, shots, 0.2)
	if len(bad) != 1 || bad[0] != 1 {
		t.Fatalf("只该标第 2 镜（无音轨），实际 %v", bad)
	}
}
