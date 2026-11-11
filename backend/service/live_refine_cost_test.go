package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 二采的**耗时**对照（这才是真实代价）。
//
// 为什么要单独测：ComfyUI 有节点级输出缓存——同 seed 同 prompt 再提交一次，
// 第一遍采样会被直接复用，于是"开了二采"反而更快（实测 54s vs 152s）。
// 所以两臂必须同时换 seed 与 prompt 尾巴（nonce）才能把缓存打掉。
// 这条坑在 AGENTS 里记过一次（直连 ComfyUI 的 A/B 必须挪 seed+prompt）。
func TestLiveRefineCost(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1（会占 GPU）")
	}
	const base = "http://127.0.0.1:8189"
	dir := filepath.Join("..", "..", ".tmp", "refine-cost", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	svc := newTestT2I(base, dir)

	mk := func(seed int64, nonce string, refine bool) (string, time.Duration, error) {
		req := MediaRequest{
			Kind: "h3", Prompt: "a slow camera push over a textured wall, ambient room tone " + nonce,
			Width: 608, Height: 352, Frames: 124, Steps: 12, Seed: seed,
		}
		if refine {
			req.RefineDenoise = 0.35
			req.RefineSteps = 4
		}
		st := time.Now()
		p, _, err := svc.GenerateMedia(req)
		return p, time.Since(st), err
	}

	// 预热：把权重加载进显存，否则第一臂会被冷加载拖成"二采更快"的假象
	if _, wd, err := mk(7001, "warmup", false); err != nil {
		t.Fatalf("预热失败：%v", err)
	} else {
		t.Logf("预热 %.0fs", wd.Seconds())
	}

	a, da, err := mk(7002, "armA", false)
	if err != nil {
		t.Fatalf("一采失败：%v", err)
	}
	b, db, err := mk(7003, "armB", true)
	if err != nil {
		t.Fatalf("二采失败：%v", err)
	}

	for name, p := range map[string]string{"一采": a, "二采": b} {
		if n, ferr := countVideoFrames(p); ferr != nil || n != 124 {
			t.Fatalf("%s 应为 124 帧，实际 %d（%v）", name, n, ferr)
		}
	}
	ratio := db.Seconds() / da.Seconds()
	t.Logf("一采 %.0fs → 二采 %.0fs（%.2f×）", da.Seconds(), db.Seconds(), ratio)
	if ratio < 1.05 {
		t.Fatalf("两臂耗时几乎相同（%.2f×）：缓存没打掉或二采没进链路，这次测量无效", ratio)
	}

	summary := fmt.Sprintf("arm=A(no refine,%.1fs)/B(refine 0.35x4,%.1fs)\nratio=%.2f\nA=%s\nB=%s\n",
		da.Seconds(), db.Seconds(), ratio, a, b)
	if werr := os.WriteFile(filepath.Join(dir, "RESULT.txt"), []byte(summary), 0o644); werr != nil {
		t.Logf("写结论失败：%v", werr)
	}
}
