package service

import (
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// 本文件只用 ffmpeg 合成素材，不需要 GPU、不需要 ComfyUI，
// 目的是把「装配后处理」这条链路钉在 CI 里：音画对齐、响度归一都必须可复现。

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if _, err2 := exec.LookPath("ffprobe"); err2 != nil {
			t.Skip("ffmpeg/ffprobe 不可用，跳过装配后处理用例")
		}
	}
}

func makeTestClip(t *testing.T, dir, name string, vSec, aSec float64) string {
	t.Helper()
	out := filepath.Join(dir, name)
	cmd := exec.Command(ffmpegBin(), "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=24:duration="+strconv.FormatFloat(vSec, 'f', 2, 64),
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=32000:duration="+strconv.FormatFloat(aSec, 'f', 2, 64),
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("合成素材失败：%v（%s）", err, string(b))
	}
	return out
}

// TestAlignAVToVideo_FixesDriftWithoutDroppingFrames 音轨长于视频流时必须对齐且不丢帧。
func TestAlignAVToVideo_FixesDriftWithoutDroppingFrames(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := makeTestClip(t, dir, "drift.mp4", 1.0, 1.3)

	before, err := inspectMedia(clip)
	if err != nil {
		t.Fatalf("读素材失败：%v", err)
	}
	if before.Frames == 0 {
		t.Fatalf("素材没有帧，无法验证丢帧")
	}

	driftBefore, fixed, err := alignAVToVideo(clip, 0.05)
	if err != nil {
		t.Fatalf("对齐失败：%v", err)
	}
	if !fixed {
		t.Fatalf("音轨比视频长 0.3s，应该执行对齐（driftBefore=%.3f）", driftBefore)
	}
	if driftBefore < 0.2 {
		t.Fatalf("对齐前漂移应约 0.3s，实际 %.3f", driftBefore)
	}

	after, err := inspectMedia(clip)
	if err != nil {
		t.Fatalf("读对齐后素材失败：%v", err)
	}
	if d := math.Abs(after.VSeconds - after.ASeconds); d > 0.05 {
		t.Fatalf("对齐后仍有 %.3f s 漂移（视频 %.3f / 音频 %.3f）", d, after.VSeconds, after.ASeconds)
	}
	if after.Frames < before.Frames {
		t.Fatalf("对齐丢了帧：%d → %d", before.Frames, after.Frames)
	}
}

// TestAlignAVToVideo_NoopWhenAligned 已对齐的成片不该被重封（避免无谓重编码）。
func TestAlignAVToVideo_NoopWhenAligned(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := makeTestClip(t, dir, "ok.mp4", 1.0, 1.0)

	_, fixed, err := alignAVToVideo(clip, 0.05)
	if err != nil {
		t.Fatalf("对齐失败：%v", err)
	}
	if fixed {
		t.Fatalf("已对齐的片子不该被重封")
	}
}
