package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// 真机验证段间引导：只有设了 FLOWEDGE_LIVE_MOTION=1 才跑（要占 GPU 几分钟）。
//
// 判据（缺一不可）：
//  1. 生成的帧数 == align17(正文 124 + 上下文 22) == 158 —— 帧预算算错会直接对不上；
//  2. 裁头后正好 124 帧 —— 少裁/多裁会让成片里每镜先倒放一遍上一镜结尾；
//  3. 产物有音轨 —— H3 是一次推理出音画，没音轨说明链路接错了。
//
// 这条用例回答的是「官方 AddGuide 能不能承载 FE 的段间引导」，
// 不回答「视觉上连不连贯」（那要 A/B 抽帧，属于人工判定）。
func TestLiveMotionContext(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1（会占 GPU）")
	}
	const base = "http://127.0.0.1:8189"
	// 产物落在固定目录而不是 t.TempDir()：测试跑完 TempDir 会被删，
	// 只留一句 PASS 没法事后抽帧看连贯性（上一轮的证据就这么丢了）。
	dir := filepath.Join("..", "..", ".tmp", "motion-live", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建产物目录失败：%v", err)
	}
	t.Logf("产物目录：%s", dir)

	// 1) 造一段 30 帧的「上一镜」素材（608x352@24fps，带音轨）
	src := filepath.Join(dir, "prev.mp4")
	ff := ffmpegBin()
	args := []string{"-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=608x352:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=32000",
		"-frames:v", "30", "-t", "1.25",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
		"-shortest", src}
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("造素材失败：%v %s", err, string(out))
	}

	// 2) 按生产路径截尾段 + 尾音轨
	tail := filepath.Join(dir, "tail.mp4")
	if err := extractTailClip(src, tail, 22, 24); err != nil {
		t.Fatalf("截尾段失败：%v", err)
	}
	tailWav := filepath.Join(dir, "tail.wav")
	if err := extractTailAudio(src, tailWav, 22, 24); err != nil {
		t.Fatalf("截尾音轨失败：%v", err)
	}

	// 3) 生成 158 帧（正文 124 + 上下文 22 对齐后）
	svc := newTestT2I(base, dir)
	start := time.Now()
	out, _, err := svc.GenerateMedia(MediaRequest{
		Kind: "h3", Prompt: "a slow camera push over a textured wall, ambient room tone",
		Width: 608, Height: 352, Frames: 158, Steps: 12,
		GuideClip: tail, GuideFrames: 22, GuideAudio: tailWav, TrimHead: 22,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("生成失败（%.0fs）：%v", elapsed.Seconds(), err)
	}
	n, err := countVideoFrames(out)
	if err != nil {
		t.Fatalf("数帧失败：%v", err)
	}
	if n != 158 {
		t.Fatalf("生成帧数应为 158（124+22 对齐），实际 %d", n)
	}
	info, err := probeMedia(out)
	if err != nil || !info.hasAudio {
		t.Fatalf("产物必须有音轨（H3 一次推理出音画）：%v", err)
	}
	peak := peakVRAM()
	t.Logf("生成完成：%.0fs，%d 帧，显存峰值 %s", elapsed.Seconds(), n, peak)

	// 4) 裁头：必须正好留下正文 124 帧
	body := filepath.Join(dir, "body.mp4")
	if err := trimHeadFrames(out, body, 22, 124, 24); err != nil {
		t.Fatalf("裁头失败：%v", err)
	}
	bn, err := countVideoFrames(body)
	if err != nil {
		t.Fatalf("数帧失败：%v", err)
	}
	if bn != 124 {
		t.Fatalf("裁头后应为 124 帧，实际 %d", bn)
	}
	t.Logf("裁头后 %d 帧，成片可交付：%s", bn, body)

	// 留证据：把关键产物路径打出来，方便人工抽帧看连贯性
	fmt.Printf("LIVE_MOTION_OUT raw=%s body=%s\n", out, body)

	// 人话结论落盘，避免只靠 go test 输出窗口（上一次就是证据随 TempDir 一起没了）
	summary := fmt.Sprintf(
		"elapsed=%.1fs\nraw_frames=%d\nbody_frames=%d\npeak=%s\nraw=%s\nbody=%s\n",
		elapsed.Seconds(), n, bn, peak, out, body)
	if werr := os.WriteFile(filepath.Join(dir, "RESULT.txt"), []byte(summary), 0o644); werr != nil {
		t.Logf("写结论文件失败：%v", werr)
	}
}

// peakVRAM 读一下当前显存占用（判据用，读不到就返回 unknown，不当失败）。
func peakVRAM() string {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.used,memory.total",
		"--format=csv,noheader").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return string(out)
}
