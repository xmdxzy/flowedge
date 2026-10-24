package service

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// 出图流水线开 qc 后，质量循环要把产物挂在 "image" 键上。
// 旧实现里 ffprobe 把 png 报成一个 1 帧视频 → kind=video → 下游拿不到图。
func TestProbeGateMediaStillImageKind(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有 ffmpeg，跳过（本机出片环境一定有）")
	}
	dir := t.TempDir()
	png := filepath.Join(dir, "still.png")
	cmd := exec.Command(ffmpegBin(), "-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=red:s=64x64:d=1", "-frames:v", "1", png)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造图失败：%v %s", err, out)
	}

	info, err := probeGateMedia(png)
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	if info.kind != "image" {
		t.Fatalf("png 的 kind 应为 image，实际 %q（下游会按 video 找产物，拿不到图）", info.kind)
	}
	if info.width != 64 || info.height != 64 {
		t.Errorf("尺寸解析 = %dx%d，期望 64x64", info.width, info.height)
	}
}

// 真视频不能被误判成 image（按扩展名判，不该碰 mp4）。
func TestProbeGateMediaVideoKind(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有 ffmpeg，跳过")
	}
	dir := t.TempDir()
	mp4 := filepath.Join(dir, "clip.mp4")
	cmd := exec.Command(ffmpegBin(), "-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=64x64:d=1:r=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-shortest", "-pix_fmt", "yuv420p", mp4)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造视频失败：%v %s", err, out)
	}

	info, err := probeGateMedia(mp4)
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	if info.kind != "video" {
		t.Fatalf("mp4 的 kind 应为 video，实际 %q", info.kind)
	}
	if !info.hasAudio {
		t.Error("带音轨的 mp4 应识别出 hasAudio")
	}
}
