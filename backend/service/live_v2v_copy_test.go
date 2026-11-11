package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 判别实验：让模型"原样复刻源片"，不做任何风格改动。
//
// 逻辑很简单：如果 ref_videos 真的进了条件，输出会非常接近源片（SSIM 高）；
// 如果这个通道根本没生效，模型只能按文本瞎生成，SSIM 会停在"两条无关视频"的水平（~0.5）。
// 这比"换成夕阳"那种提示词干净——那种情况下即使完美跟随，像素也会大变，判不出来。
func TestLiveV2VCopy(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1")
	}
	src := os.Getenv("FLOWEDGE_V2V_SRC")
	if src == "" {
		t.Skip("需要 FLOWEDGE_V2V_SRC")
	}
	const base = "http://127.0.0.1:8189"
	dir := filepath.Join("..", "..", ".tmp", "v2v-copy", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	svc := newTestT2I(base, dir)

	start := time.Now()
	steps := 8
	if v := os.Getenv("FLOWEDGE_V2V_STEPS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			steps = n
		}
	}
	out, _, err := svc.GenerateMedia(MediaRequest{
		Kind: "h3_r2v",
		// 按官方 Ref2VA 语法：参考必须在 subject_definitions 里被定义，
		// retention_analysis 只做"保留程度"的声明。前几轮只写进后者，可能没绑上。
		Prompt: "subject_definitions:\n" +
			"<Video 1> is the source clip to reproduce: its content, composition and motion are preserved exactly.\n" +
			"summary: [video reference] reproduce <Video 1> without changes.\n" +
			"retention_analysis:\n" +
			"<Video 1>: fully_preserved.\n" +
			"detailed_description: same as <Video 1>: same composition, same colors, same motion, no added elements.\n" +
			"overall_soundscape: quiet room tone.\n" +
			"non_diegetic_music: none.",
		Width: 608, Height: 352, Frames: 124, Steps: steps,
		SourceVideo: src, SourceStartFrame: 0,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	ssim, err := ssimPair(t, out, src, 124)
	if err != nil {
		t.Fatalf("量 SSIM 失败：%v", err)
	}
	t.Logf("复刻实验：%.0fs，SSIM vs 源片=%.4f（连通应 >0.70，未生效约 0.50）", elapsed.Seconds(), ssim)
	fmt.Printf("COPY_OUT=%s SSIM=%.4f\n", out, ssim)
}
