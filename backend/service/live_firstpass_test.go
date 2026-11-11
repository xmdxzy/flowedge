package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 一采确认的两段式：靠"同 seed → 同一次一采采样 → 命中 ComfyUI 节点缓存"实现。
//
// 判据：第二遍（关 confirm、开二采）必须明显快于第一遍（只跑一采）——
// 如果没命中缓存，第二遍会把一采重采一遍再补二采，时间是第一遍的 ~1.3 倍而不是更低。
func TestLiveFirstPassConfirm(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1（会占 GPU）")
	}
	const base = "http://127.0.0.1:8189"
	dir := filepath.Join("..", "..", ".tmp", "firstpass-live", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	svc := newTestT2I(base, dir)

	const seed = 7777
	prompt := "a slow camera push over a textured concrete wall, soft daylight, ambient room tone"

	gen := func(refine float64) (string, time.Duration, error) {
		st := time.Now()
		p, _, err := svc.GenerateMedia(MediaRequest{
			Kind: "h3", Prompt: prompt, Width: 608, Height: 352, Frames: 124, Steps: 8,
			Seed: seed, RefineDenoise: refine, RefineSteps: 4,
		})
		return p, time.Since(st), err
	}

	// 第 1 遍：只跑一采（confirm 开时 shotlist 会把 refine 压成 0，这里直接模拟）
	a, ta, err := gen(0)
	if err != nil {
		t.Fatalf("一采失败：%v", err)
	}
	// 第 2 遍：同 seed 开二采
	b, tb, err := gen(0.35)
	if err != nil {
		t.Fatalf("二采失败：%v", err)
	}

	for name, p := range map[string]string{"一采": a, "二采": b} {
		if n, e := countVideoFrames(p); e != nil || n != 124 {
			t.Fatalf("%s 应为 124 帧，实际 %d（%v）", name, n, e)
		}
	}

	ratio := tb.Seconds() / ta.Seconds()
	t.Logf("一采 %.0fs → 同 seed 补二采 %.0fs（%.2f×）", ta.Seconds(), tb.Seconds(), ratio)
	if ratio >= 0.8 {
		t.Fatalf("补二采用了 %.2f× 的时间——一采没命中缓存，两段式不成立", ratio)
	}

	summary := fmt.Sprintf("first_pass=%.1fs\nsecond_pass_same_seed=%.1fs\nratio=%.2f\nA=%s\nB=%s\n",
		ta.Seconds(), tb.Seconds(), ratio, a, b)
	if werr := os.WriteFile(filepath.Join(dir, "RESULT.txt"), []byte(summary), 0o644); werr != nil {
		t.Logf("写结论失败：%v", werr)
	}
}
