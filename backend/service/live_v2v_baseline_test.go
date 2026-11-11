package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// v2v 的决定性对照：同一个提示词，**不给**源视频跑一次。
//
// 为什么要这条：PSNR/SSIM 的绝对值不能说明"用没用上源片"——
// 提示词里带"换风格"时，即便模型牢牢跟随源片，像素也会大变。
// 唯一干净的判据是**相对**的：给了源片的那条，与源片的结构相似度必须明显高于没给源片的那条。
func TestLiveSourceVideoBaseline(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_MOTION") != "1" {
		t.Skip("需要 FLOWEDGE_LIVE_MOTION=1（会占 GPU）")
	}
	src := os.Getenv("FLOWEDGE_V2V_SRC")
	if src == "" {
		t.Skip("需要 FLOWEDGE_V2V_SRC 指向上一轮 v2v 用的源片")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("源片不存在：%s", src)
	}

	const base = "http://127.0.0.1:8189"
	dir := filepath.Join("..", "..", ".tmp", "v2v-baseline", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	svc := newTestT2I(base, dir)

	start := time.Now()
	out, _, err := svc.GenerateMedia(MediaRequest{
		Kind: "h3_r2v", Prompt: "keep the composition and motion, restyle the scene as a warm sunset",
		Width: 608, Height: 352, Frames: 124, Steps: 8,
		RefImages: []string{firstFramePNG(t, dir, src)}, // 只给首帧，够让 r2v 成立但不给整段视频
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("无源片对照生成失败：%v", err)
	}
	ssim, err := ssimPair(t, out, src, 124)
	if err != nil {
		t.Fatalf("量 SSIM 失败：%v", err)
	}
	t.Logf("无源片对照：%.0fs，SSIM vs 源片=%.4f", elapsed.Seconds(), ssim)
	fmt.Printf("BASELINE_OUT=%s SSIM=%.4f\n", out, ssim)
}

// firstFramePNG 抽源片首帧当参考图（让 r2v 链路成立，但不给整段视频）。
func firstFramePNG(t *testing.T, dir, src string) string {
	t.Helper()
	out := filepath.Join(dir, "first.png")
	if b, err := exec.Command(ffmpegBin(), "-y", "-v", "error", "-i", src, "-frames:v", "1", out).CombinedOutput(); err != nil {
		t.Fatalf("抽首帧失败：%v %s", err, string(b))
	}
	return out
}

var ssimRe = regexp.MustCompile(`All:([0-9.]+)`)

// ssimPair 比较两段视频前 frames 帧的结构相似度（分越高越像）。
func ssimPair(t *testing.T, a, b string, frames int) (float64, error) {
	t.Helper()
	fc := fmt.Sprintf(
		"[0:v]trim=start_frame=0:end_frame=%d,setpts=PTS-STARTPTS[x];"+
			"[1:v]trim=start_frame=0:end_frame=%d,setpts=PTS-STARTPTS[y];[x][y]ssim",
		frames, frames)
	out, _ := exec.Command(ffmpegBin(), "-hide_banner", "-i", a, "-i", b,
		"-filter_complex", fc, "-f", "null", "NUL").CombinedOutput()
	m := ssimRe.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("解析不到 SSIM：%.300s", string(out))
	}
	var v float64
	if _, err := fmt.Sscanf(string(m[1]), "%f", &v); err != nil {
		return 0, err
	}
	return v, nil
}
