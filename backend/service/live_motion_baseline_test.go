package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 段间引导的 A/B 基线臂：同样的提示词/分辨率/步数，**不给**引导，正文 124 帧。
//
// 为什么要单独跑：段间引导会多生成 34 帧（124→158）外加一次 22 帧 VAE 编码，
// 不加对照就说不清「贵了多少」。按项目纪律（AGENTS §0.4），新开关必须开/关各跑一次。
func TestLiveMotionBaseline(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1（会占 GPU）")
	}
	const base = "http://127.0.0.1:8189"
	dir := filepath.Join("..", "..", ".tmp", "motion-live", time.Now().Format("20060102-150405")+"-baseline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建产物目录失败：%v", err)
	}

	svc := newTestT2I(base, dir)
	start := time.Now()
	out, _, err := svc.GenerateMedia(MediaRequest{
		Kind: "h3", Prompt: "a slow camera push over a textured wall, ambient room tone",
		Width: 608, Height: 352, Frames: 124, Steps: 12,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("基线生成失败：%v", err)
	}
	n, err := countVideoFrames(out)
	if err != nil {
		t.Fatalf("数帧失败：%v", err)
	}
	if n != 124 {
		t.Fatalf("基线应为 124 帧，实际 %d", n)
	}
	peak := peakVRAM()
	t.Logf("基线：%.0fs，124 帧，显存 %s", elapsed.Seconds(), peak)

	summary := fmt.Sprintf("arm=baseline\nframes=124\nelapsed=%.1fs\npeak=%s\nout=%s\n",
		elapsed.Seconds(), peak, out)
	if werr := os.WriteFile(filepath.Join(dir, "RESULT.txt"), []byte(summary), 0o644); werr != nil {
		t.Logf("写结论文件失败：%v", werr)
	}
}
