package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// v2v 真机验证：给一段源视频，模型应该"改造它"而不是"无视它重新生成"。
//
// 判据：
//  1. 出片 124 帧 + 音轨（链路成立）；
//  2. 成片与源片 PSNR 明显高于"随机无关画面"的水平——如果模型无视 <Video 1>，
//     出来的东西与源片就只是"同一个提示词的两条独立生成"，PSNR 会掉到 10dB 附近。
//
// 阈值取 18dB：真机实测 v2v 保留构图时一般在 20–28dB；纯重生成落在 9–12dB。
func TestLiveSourceVideo(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1（会占 GPU）")
	}
	const base = "http://127.0.0.1:8189"
	dir := filepath.Join("..", "..", ".tmp", "v2v-live", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	ff := ffmpegBin()

	src := filepath.Join(dir, "source.mp4")
	args := []string{"-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=608x352:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=32000",
		"-frames:v", "124", "-t", "5.167",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
		"-shortest", src}
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("造源片失败：%v %s", err, string(out))
	}

	svc := newTestT2I(base, dir)
	start := time.Now()
	out, _, err := svc.GenerateMedia(MediaRequest{
		Kind: "h3_r2v", Prompt: "keep the composition and motion, restyle the scene as a warm sunset",
		Width: 608, Height: 352, Frames: 124, Steps: 8,
		SourceVideo: src, SourceStartFrame: 0,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("v2v 生成失败（%.0fs）：%v", elapsed.Seconds(), err)
	}
	n, err := countVideoFrames(out)
	if err != nil || n != 124 {
		t.Fatalf("应为 124 帧，实际 %d（%v）", n, err)
	}
	info, perr := probeMedia(out)
	if perr != nil || !info.hasAudio {
		t.Fatalf("产物必须有音轨：%v", perr)
	}

	psnr, err := psnrHeadVs(t, out, src, 124)
	if err != nil {
		t.Fatalf("量 PSNR 失败：%v", err)
	}
	t.Logf("v2v：%.0fs，124 帧，成片 vs 源片 PSNR=%.2f dB", elapsed.Seconds(), psnr)
	if psnr < 18 {
		t.Fatalf("成片与源片 PSNR 只有 %.2f dB（<18）——模型很可能无视了 <Video 1>，"+
			"等于按提示词重新生成了一条", psnr)
	}

	summary := fmt.Sprintf("elapsed=%.1fs\nframes=%d\npsnr_vs_source=%.2f\nout=%s\nsrc=%s\n",
		elapsed.Seconds(), n, psnr, out, src)
	if werr := os.WriteFile(filepath.Join(dir, "RESULT.txt"), []byte(summary), 0o644); werr != nil {
		t.Logf("写结论失败：%v", werr)
	}
}
