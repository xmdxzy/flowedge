package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// 段间引导（motion context）的纯函数约定。
//
// 这两条规矩不是审美问题：H3 只吃 17k+5 网格，而官方 AddGuide 对不合法的
// 引导长度会**自己往下削**（while n%17 != 5: n--），于是一个算错的帧数
// 不会报错，只会让「续接 22 帧」悄悄变成「续接 5 帧」。
func TestAlignH3Frames(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 5}, {1, 5}, {5, 5},
		{124, 124}, {125, 141}, {141, 141}, {142, 158},
		{146, 158}, // 124 正文 + 22 上下文 → 158
		{158, 158}, {159, 175},
	}
	for _, c := range cases {
		if got := alignH3Frames(c.in); got != c.want {
			t.Fatalf("alignH3Frames(%d) = %d，期望 %d", c.in, got, c.want)
		}
		if got := alignH3Frames(c.in); (got-5)%17 != 0 {
			t.Fatalf("alignH3Frames(%d) = %d 不在 17k+5 网格上", c.in, got)
		}
	}
}

func TestSnapContextFrames(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 5}, {5, 5}, {6, 5}, {21, 5}, {22, 22}, {30, 22},
		{39, 39}, {40, 39}, {56, 56}, {120, 56},
	}
	for _, c := range cases {
		if got := snapContextFrames(c.in); got != c.want {
			t.Fatalf("snapContextFrames(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

func TestNodeIntLoose(t *testing.T) {
	if v, ok := nodeIntLoose(float64(22)); !ok || v != 22 {
		t.Fatalf("float64 解析失败：%v %v", v, ok)
	}
	if v, ok := nodeIntLoose("39"); !ok || v != 39 {
		t.Fatalf("字符串解析失败：%v %v", v, ok)
	}
	if _, ok := nodeIntLoose(nil); ok {
		t.Fatal("nil 不该被当成 0（会把「没配」当成「关」）")
	}
	if _, ok := nodeIntLoose("abc"); ok {
		t.Fatal("非法字符串不该被接受")
	}
}

// makeContinuityTestClip 造一段带音轨的测试片（只在装了 ffmpeg 的环境跑）。
func makeContinuityTestClip(t *testing.T, path string, frames int) {
	t.Helper()
	ff := ffmpegBin()
	if _, err := exec.LookPath(ff); err != nil {
		t.Skip("未安装 ffmpeg，跳过")
	}
	args := []string{
		"-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=32000",
		"-frames:v", strconv.Itoa(frames),
		"-t", fmt.Sprintf("%.3f", float64(frames)/24.0),
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast",
		"-c:a", "aac", "-shortest", path,
	}
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Skipf("造测试片失败（环境缺编码器）：%v %s", err, string(out))
	}
	n, err := countVideoFrames(path)
	if err != nil {
		t.Fatalf("数帧失败：%v", err)
	}
	if n != frames {
		t.Fatalf("测试片应 %d 帧，实际 %d", frames, n)
	}
}

// 段间引导的帧数必须**精确**：尾部片段少一帧，AddGuide 会把 21 削到 5；
// 裁头少裁一帧，成片每一镜都会先倒放一遍上一镜的结尾。
func TestContinuityClipExactFrames(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	makeContinuityTestClip(t, src, 30)

	tail := filepath.Join(dir, "tail.mp4")
	if err := extractTailClip(src, tail, 22, 24); err != nil {
		t.Fatalf("截尾部片段失败：%v", err)
	}
	if n, err := countVideoFrames(tail); err != nil || n != 22 {
		t.Fatalf("尾部片段应正好 22 帧，实际 %d（%v）", n, err)
	}
	// 必须是**最后** 22 帧：源片末帧与尾部片段末帧应一致（比字节不可靠，
	// 这里比「从第 8 帧开始」这一事实——源 30 帧取 22 帧，起点就是 8）。
	body := filepath.Join(dir, "body.mp4")
	if err := trimHeadFrames(tail, body, 5, 15, 24); err != nil {
		t.Fatalf("裁头失败：%v", err)
	}
	if n, err := countVideoFrames(body); err != nil || n != 15 {
		t.Fatalf("裁头后应正好 15 帧，实际 %d（%v）", n, err)
	}

	wav := filepath.Join(dir, "tail.wav")
	if err := extractTailAudio(src, wav, 22, 24); err != nil {
		t.Fatalf("截尾音轨失败：%v", err)
	}
	st, err := os.Stat(wav)
	if err != nil || st.Size() < 1000 {
		t.Fatalf("尾音轨文件无效：%v", err)
	}
}

// 上一镜比要取的窗口还短时必须报错，不能给出「补帧」的假片段：
// 复制末帧补齐会让模型生成可见的停顿/重复帧（AIMixer 的 prepare_segment_clip
// 也明写「只裁不补」）。
func TestExtractTailClipTooShort(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "short.mp4")
	makeContinuityTestClip(t, src, 10)
	err := extractTailClip(src, filepath.Join(dir, "o.mp4"), 22, 24)
	if err == nil {
		t.Fatal("源片只有 10 帧却要取 22 帧，必须报错而不是补帧")
	}
}
