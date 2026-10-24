package service

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// 造一段纯色片，冷/暖由 colortemperature 滤镜决定。
func makeClip(t *testing.T, dir, name, filter string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	args := []string{"-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=gray:s=96x96:d=1:r=12"}
	if filter != "" {
		args = append(args, "-vf", filter)
	}
	args = append(args, "-pix_fmt", "yuv420p", out)
	if o, err := exec.Command(ffmpegBin(), args...).CombinedOutput(); err != nil {
		t.Fatalf("造片失败：%v %s", err, o)
	}
	return out
}

// 相邻两镜色温差很大时必须报 plan.color_shift（实测：一镜暖黄一镜蓝调的接缝）。
func TestAuditClipChromaDetectsShift(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有 ffmpeg，跳过")
	}
	dir := t.TempDir()
	warm := makeClip(t, dir, "warm.mp4", "colortemperature=temperature=2000")
	cool := makeClip(t, dir, "cool.mp4", "colortemperature=temperature=9000")

	issues := AuditClipChroma([]string{warm, cool})
	found := false
	for _, it := range issues {
		if it.Code == "plan.color_shift" && it.Level == "WARN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("冷暖相邻应报 plan.color_shift，实际 %+v", issues)
	}
}

// 同一段片拼两次不该误报（否则每部片都有一条无意义告警）。
func TestAuditClipChromaSameColorOK(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("没有 ffmpeg，跳过")
	}
	dir := t.TempDir()
	a := makeClip(t, dir, "a.mp4", "colortemperature=temperature=4500")
	b := makeClip(t, dir, "b.mp4", "colortemperature=temperature=4500")

	if issues := AuditClipChroma([]string{a, b}); len(issues) != 0 {
		t.Fatalf("同色温不该报漂移，实际 %+v", issues)
	}
}

// 单镜片没有"相邻"可言。
func TestAuditClipChromaSingleShot(t *testing.T) {
	if issues := AuditClipChroma([]string{"only.mp4"}); len(issues) != 0 {
		t.Fatalf("单镜不该有跨镜判定，实际 %+v", issues)
	}
}
