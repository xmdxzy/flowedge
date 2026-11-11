package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// ref2va + 段间引导能否共存。
//
// 这是整套移植里唯一"图上看着对、真机可能不成立"的地方：
// ReferenceToVideo 往 conditioning 里写 minimax_refs，AddGuide 往同一个
// conditioning 里追加 minimax_keyframes。官方实现里两者互不覆盖，
// 但**采样器是否同时消费两套条件**只能实测。
//
// 判据：带着参考图跑出来的片子，**片头 22 帧仍然要像上一镜尾巴**（PSNR > 20dB）。
// 若参考图把 keyframe 挤掉了，片头就会与尾巴无关，PSNR 掉到 10dB 附近。
func TestLiveMotionContextRef2VA(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1（会占 GPU）")
	}
	const base = "http://127.0.0.1:8189"
	dir := filepath.Join("..", "..", ".tmp", "motion-live", time.Now().Format("20060102-150405")+"-r2v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建产物目录失败：%v", err)
	}
	t.Logf("产物目录：%s", dir)

	ff := ffmpegBin()
	// 1) 上一镜素材（30 帧，带音轨）
	src := filepath.Join(dir, "prev.mp4")
	mk := []string{"-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=608x352:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=32000",
		"-frames:v", "30", "-t", "1.25",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
		"-shortest", src}
	if out, err := exec.Command(ff, mk...).CombinedOutput(); err != nil {
		t.Fatalf("造素材失败：%v %s", err, string(out))
	}

	// 2) 尾段 + 尾音轨
	tail := filepath.Join(dir, "tail.mp4")
	if err := extractTailClip(src, tail, 22, 24); err != nil {
		t.Fatalf("截尾段失败：%v", err)
	}
	tailWav := filepath.Join(dir, "tail.wav")
	if err := extractTailAudio(src, tailWav, 22, 24); err != nil {
		t.Fatalf("截尾音轨失败：%v", err)
	}

	// 3) 参考图：用上一镜的首帧（模拟"同一个人/同一个场景"）
	ref := filepath.Join(dir, "ref.png")
	if out, err := exec.Command(ff, "-y", "-v", "error", "-i", src,
		"-frames:v", "1", ref).CombinedOutput(); err != nil {
		t.Fatalf("抽参考图失败：%v %s", err, string(out))
	}

	// 4) ref2va + 段间引导同时开
	svc := newTestT2I(base, dir)
	start := time.Now()
	out, _, err := svc.GenerateMedia(MediaRequest{
		Kind: "h3_r2v", Prompt: "the same textured wall, slow camera push, ambient room tone",
		Width: 608, Height: 352, Frames: 158, Steps: 12,
		RefImages:   []string{ref},
		GuideClip:   tail,
		GuideFrames: 22,
		GuideAudio:  tailWav,
		TrimHead:    22,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ref2va+引导生成失败（%.0fs）：%v", elapsed.Seconds(), err)
	}

	n, err := countVideoFrames(out)
	if err != nil {
		t.Fatalf("数帧失败：%v", err)
	}
	if n != 158 {
		t.Fatalf("生成帧数应为 158，实际 %d", n)
	}
	info, err := probeMedia(out)
	if err != nil || !info.hasAudio {
		t.Fatalf("产物必须有音轨：%v", err)
	}

	// 5) 关键判据：带参考图时，片头是否仍然像尾巴
	psnr, perr := psnrHeadVs(t, out, tail, 22)
	if perr != nil {
		t.Fatalf("量 PSNR 失败：%v", perr)
	}
	t.Logf("ref2va+引导：%.0fs，%d 帧，片头 vs 尾段 PSNR=%.2f dB", elapsed.Seconds(), n, psnr)
	if psnr < 20 {
		t.Fatalf("片头与上一镜尾巴 PSNR 只有 %.2f dB（<20）——参考图很可能把 keyframe 挤掉了，"+
			"段间引导在 ref2va 上**不生效**", psnr)
	}

	summary := fmt.Sprintf("arm=r2v+motion\nframes=%d\nelapsed=%.1fs\npsnr_head_vs_tail=%.2f\nout=%s\n",
		n, elapsed.Seconds(), psnr, out)
	if werr := os.WriteFile(filepath.Join(dir, "RESULT.txt"), []byte(summary), 0o644); werr != nil {
		t.Logf("写结论失败：%v", werr)
	}
}

var psnrAvgRe = regexp.MustCompile(`average:([0-9.]+)`)

// psnrHeadVs 比较 a 的前 frames 帧与 b 的前 frames 帧（逐帧对齐），返回平均 PSNR。
func psnrHeadVs(t *testing.T, a, b string, frames int) (float64, error) {
	t.Helper()
	fc := fmt.Sprintf(
		"[0:v]trim=start_frame=0:end_frame=%d,setpts=PTS-STARTPTS[x];"+
			"[1:v]trim=start_frame=0:end_frame=%d,setpts=PTS-STARTPTS[y];[x][y]psnr",
		frames, frames)
	out, _ := exec.Command(ffmpegBin(), "-hide_banner", "-i", a, "-i", b,
		"-filter_complex", fc, "-f", "null", "NUL").CombinedOutput()
	m := psnrAvgRe.FindSubmatch(out)
	if m == nil {
		// 部分 ffmpeg 会把报表写进 stdout，兜底再扫一遍
		cmd := exec.Command(ffmpegBin(), "-hide_banner", "-i", a, "-i", b, "-filter_complex", fc, "-f", "null", "-")
		o2, _ := cmd.CombinedOutput()
		m = psnrAvgRe.FindSubmatch(o2)
		if m == nil {
			return 0, fmt.Errorf("解析不到 PSNR：%s", string(out))
		}
	}
	return strconv.ParseFloat(string(m[1]), 64)
}
