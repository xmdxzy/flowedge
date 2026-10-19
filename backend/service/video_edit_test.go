package service

import (
	"regexp"
	"strconv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 装配是整条长片流水线的最后一环，出错会白烧十几分钟显卡。
// 这里直接用 ffmpeg 造三段极短素材，跑真实的 assembleShots，锁住：
//   1. 滤镜图能解析（历史上踩过 "[afin]" 多给输入、空滤镜两处错）
//   2. 输出时长 ≈ 各段之和 − 重叠
//   3. 音轨存在
func countVideoFrames(t *testing.T, path string) int {
	t.Helper()
	cmd := exec.Command("ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", path)
	b, err := cmd.Output()
	if err != nil {
		t.Skip("没有 ffprobe，跳过帧数校验")
	}
	n := 0
	fmt.Sscanf(string(b), "%d", &n)
	return n
}

func TestAssembleShotsWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath(ffmpegBin()); err != nil {
		t.Skip("没有 ffmpeg，跳过装配测试")
	}
	dir := t.TempDir()
	clips := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		p := filepath.Join(dir, fmt.Sprintf("c%d.mp4", i))
		// 1 秒纯色 + 正弦音；不同颜色便于肉眼确认
		cmd := exec.Command(ffmpegBin(), "-y", "-v", "error",
			"-f", "lavfi", "-i", fmt.Sprintf("color=c=red@1.0:s=160x96:d=1:r=24"),
			"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", p)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("造素材失败：%v %s", err, out)
		}
		clips = append(clips, p)
	}

	out := filepath.Join(dir, "film.mp4")
	if err := assembleShots(clips, []string{"cut", "dissolve"}, out); err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() < 4096 {
		t.Fatalf("产出无效：%v", err)
	}

	info, err := probeMedia(out)
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	// 1+1+1 − 0.04 − 0.30 ≈ 2.66s，允许 ±0.2s（帧对齐）
	if info.dur < 2.4 || info.dur > 2.9 {
		t.Fatalf("时长异常：%.2fs（期望约 2.66s）", info.dur)
	}
	if !info.hasAudio {
		t.Fatal("成片没有音轨")
	}
	// 关键回归：视频帧数必须覆盖整条时长。
	// 曾经因为 cut 的淡化窗口比一帧还短，第二段画面被整段丢掉——
	// 时长（由音轨决定）看着正常，画面却只有第一镜。
	frames := countVideoFrames(t, out)
	want := int(info.dur * 24)
	if frames < want-4 {
		t.Fatalf("视频帧数不足：%d 帧（时长 %.2fs 应有约 %d 帧）——段被吞了", frames, info.dur, want)
	}
}

// 自检必须能抓住「视频流被截断、音频却是全长」——这正是 xfade 淡化短于一帧时
// 发生的真实事故：总时长正常、能播放、但中间整段画面没有。
func TestInspectMediaDetectsShortVideo(t *testing.T) {
	if _, err := exec.LookPath(ffmpegBin()); err != nil {
		t.Skip("没有 ffmpeg，跳过自检测试")
	}
	if ffprobeBin() == "" {
		t.Skip("没有 ffprobe，跳过自检测试")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.mp4")
	cmd := exec.Command(ffmpegBin(), "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x96:rate=24:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", good)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造素材失败：%v %s", err, out)
	}

	// 好素材必须通过
	if chk, err := inspectMedia(good); err != nil || !chk.OK {
		t.Fatalf("正常成片被判失败：%+v err=%v", chk, err)
	}

	// 把视频截到 1.2s、音频保留 3s，模拟丢段
	broken := filepath.Join(dir, "broken.mp4")
	cmd = exec.Command(ffmpegBin(), "-y", "-v", "error", "-i", good,
		"-filter_complex", "[0:v]trim=0:1.2,setpts=PTS-STARTPTS[v]",
		"-map", "[v]", "-map", "0:a", "-c:v", "libx264", "-c:a", "copy", broken)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造坏样本失败：%v %s", err, out)
	}

	chk, err := inspectMedia(broken)
	if err != nil {
		t.Fatalf("自检报错：%v", err)
	}
	if chk.OK {
		t.Fatalf("丢段成片没被抓出来：%+v", chk)
	}
	if len(chk.Issues) == 0 {
		t.Fatalf("失败但没有给出原因")
	}
	if chk.Frames <= 0 || chk.ASeconds <= chk.VSeconds {
		t.Fatalf("帧数或流时长解析异常：%+v", chk)
	}
}

// 中置相消是「人声乱语」的第三道防线：把居中人声从信号里减掉，而不是调小音量。
// 这里造一条「居中正弦 + 左右不同的宽声场」音频，验证：
//   1. 居中成分确实被抵消（响度显著下降）
//   2. 视频帧不受影响（-c:v copy，不能二次编码丢帧）
func TestStripSpeechCenter(t *testing.T) {
	if _, err := exec.LookPath(ffmpegBin()); err != nil {
		t.Skip("没有 ffmpeg")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	// 左声道 = 居中 440Hz + 宽带噪声；右声道 = 居中 440Hz + 不同的噪声
	// → 相减后 440Hz 抵消，剩下的噪声不完全抵消
	cmd := exec.Command(ffmpegBin(), "-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=gray:s=160x96:d=2:r=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "lavfi", "-i", "anoisesrc=d=2:a=0.05:seed=11",
		"-f", "lavfi", "-i", "anoisesrc=d=2:a=0.05:seed=22",
		"-filter_complex",
		"[2:a][3:a]amerge=inputs=2[wide];"+ // 宽声场：左右不同噪声
			"[1:a]pan=stereo|c0=c0|c1=c0[center];"+ // 居中人声替身：左右相同
			"[wide][center]amix=inputs=2:duration=shortest,alimiter=limit=0.9[a]",
		"-map", "0:v", "-map", "[a]", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("造素材失败（跳过）：%v %s", err, out)
	}

	before := countVideoFrames(t, src)
	if before <= 0 {
		t.Skip("无法计数帧")
	}
	meanBefore := meanVolume(t, src)

	if err := stripSpeechCenter(src); err != nil {
		t.Fatalf("中置相消失败：%v", err)
	}
	if after := countVideoFrames(t, src); after != before {
		t.Fatalf("视频帧被改动：%d → %d（应当 -c:v copy 原样保留）", before, after)
	}
	meanAfter := meanVolume(t, src)
	if meanAfter >= meanBefore-1.0 {
		t.Fatalf("居中成分没有被抵消：%.1fdB → %.1fdB", meanBefore, meanAfter)
	}
}

// meanVolume 取整条音轨的平均响度（dB）
func meanVolume(t *testing.T, path string) float64 {
	t.Helper()
	raw, _ := exec.Command(ffmpegBin(), "-hide_banner", "-i", path,
		"-af", "volumedetect", "-f", "null", os.DevNull).CombinedOutput()
	m := regexp.MustCompile(`mean_volume:\s*(-?[0-9.]+)`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("读不到 mean_volume：%s", raw)
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	return v
}
