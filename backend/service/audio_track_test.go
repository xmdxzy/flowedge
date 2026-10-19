package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 音轨替换必须「1:1 覆盖 + 一帧不丢」。
//
// 这条测试故意让**音频比视频短**（0.5s 音频 / 1s 视频）：这正是实测踩到的坑——
// 用 -shortest 会因为音频短而砍掉视频尾帧，总时长只差几毫秒，
// 只有数帧才发现（362 -> 361）。replaceAudioTrack 因此改用 apad + -t。
func TestReplaceAudioTrackKeepsEveryFrame(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不可用，跳过")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "v.mp4")
	audio := filepath.Join(dir, "a.wav")

	// 1 秒 / 24fps 测试图 + 0.5 秒正弦（故意短于视频）
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=24:duration=1",
		"-pix_fmt", "yuv420p", video).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败：%v（%s）", err, b)
	}
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=0.5",
		"-c:a", "pcm_s16le", audio).CombinedOutput(); err != nil {
		t.Fatalf("生成测试音频失败：%v（%s）", err, b)
	}

	before, err := inspectMedia(video)
	if err != nil {
		t.Fatalf("inspect 原始视频失败：%v", err)
	}
	if before.Frames == 0 {
		t.Skip("环境里数不到帧（缺 ffprobe），跳过")
	}

	out, err := replaceAudioTrack(video, audio)
	if err != nil {
		t.Fatalf("替换音轨失败：%v", err)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Fatalf("输出文件不存在：%v", statErr)
	}

	after, err := inspectMedia(out)
	if err != nil {
		t.Fatalf("inspect 输出失败：%v", err)
	}
	if after.Frames != before.Frames {
		t.Fatalf("帧必须一帧不少：before=%d after=%d", before.Frames, after.Frames)
	}
	// 音频被 apad 补满到视频长度（0.5s -> ~1.0s），不能还是 0.5s
	if after.ASeconds < before.VSeconds*0.9 {
		t.Fatalf("音轨应被补满到视频长度：audio=%.3f video=%.3f", after.ASeconds, before.VSeconds)
	}
	if after.ASeconds <= 0 {
		t.Fatal("输出必须带音轨")
	}
}
