package service

import (
	"math"
	"path/filepath"
	"testing"
)

// TestMasterLoudness_BringsQuietTrackUp 低电平素材归一到目标响度。
// 用能量法测（probeLoudnessAndSilence），不依赖任何外部服务。
func TestMasterLoudness_BringsQuietTrackUp(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := makeTestClip(t, dir, "quiet.mp4", 2.0, 2.0)

	before, _ := probeLoudnessAndSilence(clip)
	out := filepath.Join(dir, "mastered.mp4")
	if err := masterLoudness(clip, out, -16, -1.5, 11); err != nil {
		t.Fatalf("响度归一失败：%v", err)
	}
	after, _ := probeLoudnessAndSilence(out)
	if math.Abs(after.mean-(-16)) > 2.5 {
		t.Fatalf("归一后均值应接近 -16dB（实测前 %.1f / 后 %.1f）", before.mean, after.mean)
	}
	if after.mean <= before.mean+3 {
		t.Fatalf("归一没有把响度提上来：前 %.1f → 后 %.1f", before.mean, after.mean)
	}
}

// TestMixBGM_KeepsDurationAndVideoCopy BGM 混音不能改时长、不能丢帧。
func TestMixBGM_KeepsDurationAndVideoCopy(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := makeTestClip(t, dir, "base.mp4", 1.0, 1.0)
	bgm := makeTestClip(t, dir, "bgm.mp4", 0.5, 0.5)

	before, _ := inspectMedia(clip)
	out := filepath.Join(dir, "mixed.mp4")
	if err := mixBGM(clip, bgm, out, -18, true); err != nil {
		t.Fatalf("BGM 混音失败：%v", err)
	}
	after, err := inspectMedia(out)
	if err != nil {
		t.Fatalf("读混音结果失败：%v", err)
	}
	if after.Frames < before.Frames {
		t.Fatalf("混音丢帧：%d → %d", before.Frames, after.Frames)
	}
	if math.Abs(after.VSeconds-before.VSeconds) > 0.06 {
		t.Fatalf("混音改了时长：%.3f → %.3f", before.VSeconds, after.VSeconds)
	}
	if d := math.Abs(after.VSeconds - after.ASeconds); d > 0.06 {
		t.Fatalf("混音引入音画漂移 %.3fs", d)
	}
}
