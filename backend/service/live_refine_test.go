package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 二采的真机 A/B：同一 seed 跑两臂（关 / 开），判据三条：
//  1. 两臂都出 124 帧 + 音轨（二采没把片子搞坏）；
//  2. 两臂画面**不一样**（PSNR < 45）——一样就说明第二遍白算了；
//  3. 画面**不是一个东西**（PSNR > 15）——太低说明二采把一采重画了，等于没保住构图。
//
// 耗时差就是二采的代价（第二遍 4 步 @ denoise 0.35）。
func TestLiveRefineAB(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1（会占 GPU）")
	}
	const base = "http://127.0.0.1:8189"
	dir := filepath.Join("..", "..", ".tmp", "refine-live", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	svc := newTestT2I(base, dir)

	req := MediaRequest{
		Kind: "h3", Prompt: "a slow camera push over a textured wall, ambient room tone",
		Width: 608, Height: 352, Frames: 124, Steps: 12, Seed: 20261101,
	}

	// 臂 A：一采到底
	startA := time.Now()
	a, _, err := svc.GenerateMedia(req)
	elapsedA := time.Since(startA)
	if err != nil {
		t.Fatalf("一采失败：%v", err)
	}

	// 臂 B：同 seed + 二采
	reqRefine := req
	reqRefine.RefineDenoise = 0.35
	reqRefine.RefineSteps = 4
	startB := time.Now()
	b, _, err := svc.GenerateMedia(reqRefine)
	elapsedB := time.Since(startB)
	if err != nil {
		t.Fatalf("二采失败：%v", err)
	}

	for name, p := range map[string]string{"一采": a, "二采": b} {
		n, ferr := countVideoFrames(p)
		if ferr != nil || n != 124 {
			t.Fatalf("%s 应为 124 帧，实际 %d（%v）", name, n, ferr)
		}
		info, perr := probeMedia(p)
		if perr != nil || !info.hasAudio {
			t.Fatalf("%s 必须有音轨：%v", name, perr)
		}
	}

	psnr, perr := psnrHeadVs(t, a, b, 124)
	if perr != nil {
		t.Fatalf("量 PSNR 失败：%v", perr)
	}
	t.Logf("一采 %.0fs / 二采 %.0fs（+%.0f%%），两臂 PSNR=%.2f dB",
		elapsedA.Seconds(), elapsedB.Seconds(),
		(elapsedB.Seconds()/elapsedA.Seconds()-1)*100, psnr)

	if psnr >= 45 {
		t.Fatalf("两臂 PSNR=%.2f dB，画面几乎一样——二采没进采样链路（白算一遍）", psnr)
	}
	if psnr < 15 {
		t.Fatalf("两臂 PSNR=%.2f dB，二采把一采重画了——精修没保住构图", psnr)
	}

	summary := fmt.Sprintf("arm=A/B (seed=20261101)\nbaseline=%.1fs\nrefine=%.1fs\npsnr=%.2f\nA=%s\nB=%s\n",
		elapsedA.Seconds(), elapsedB.Seconds(), psnr, a, b)
	if werr := os.WriteFile(filepath.Join(dir, "RESULT.txt"), []byte(summary), 0o644); werr != nil {
		t.Logf("写结论失败：%v", werr)
	}
}
