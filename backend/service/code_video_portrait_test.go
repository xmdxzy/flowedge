package service

// 竖屏渲染的 live 冒烟：同一份 spec 用 9:16 / 3:4 各出一次，核对帧数与尺寸。
// 默认跳过，FLOWEDGE_LIVE_CODEVIDEO=1 打开。
//
// 为什么单独一条：竖屏不是「横屏挤扁」，四套风格各有独立布局（安全区、断行、字号表），
// 布局写错在横屏样片里完全看不出来，只有真按 1080×1920 渲一遍才露。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCodeVideoPortraitRender(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_CODEVIDEO") != "1" {
		t.Skip("set FLOWEDGE_LIVE_CODEVIDEO=1 to render portrait variants")
	}
	root := codeVideoRoot()
	if root == "" {
		t.Skip("lemo-opuscar 渲染库不在，跳过")
	}
	secs := 4.0
	if v := os.Getenv("FLOWEDGE_CODEVIDEO_SECS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			secs = f
		}
	}

	cases := []struct {
		style  string
		w, h   int
		suffix string
	}{
		{"darkcard", 1080, 1920, "9x16"},
		{"studio", 1080, 1920, "9x16"},
		{"atlas", 1080, 1920, "9x16"},
		{"darkcard", 1080, 1440, "3x4"},
		{"atlas", 1080, 1440, "3x4"},
		{"blueprint", 1080, 1920, "9x16"},
	}

	spec := CodeSpec{
		Title:       "竖屏也要能看",
		TitleEn:     "FLOWEDGE · PORTRAIT",
		Kicker:      "FLOWEDGE · PORTRAIT",
		FPS:         24,
		CardSeconds: secs,
		Cards:       liveCodeVideoCards(),
	}
	spec = *normalizeCodeSpec(&spec)

	outDir := filepath.Join("..", "..", ".tmp", "codevideo-styles")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("建输出目录失败：%v", err)
	}

	for _, c := range cases {
		c := c
		t.Run(c.style+"-"+c.suffix, func(t *testing.T) {
			demoDir, err := os.MkdirTemp("", "flowedge-cvp-*")
			if err != nil {
				t.Fatalf("临时目录：%v", err)
			}
			defer os.RemoveAll(demoDir)
			if err := materializeCodeTemplate(demoDir, c.style); err != nil {
				t.Fatalf("写入模板：%v", err)
			}
			b, _ := json.MarshalIndent(spec, "", "  ")
			if err := os.WriteFile(filepath.Join(demoDir, "spec.json"), b, 0o644); err != nil {
				t.Fatalf("写 spec：%v", err)
			}

			outPath, _ := filepath.Abs(filepath.Join(outDir, fmt.Sprintf("%s-p%s.mp4", c.style, c.suffix)))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			args := []string{
				filepath.Join("core", "render", "video.mjs"), demoDir,
				"--fps", strconv.Itoa(spec.FPS),
				"--workers", "2",
				"--out", outPath,
				"--size", fmt.Sprintf("%dx%d", c.w, c.h),
			}
			nodeBin := strings.TrimSpace(os.Getenv("FLOWEDGE_NODE"))
			if nodeBin == "" {
				nodeBin = "node"
			}
			cmd := exec.CommandContext(ctx, nodeBin, args...)
			cmd.Dir = root
			output, runErr := cmd.CombinedOutput()
			if runErr != nil {
				tail := string(output)
				if len(tail) > 1500 {
					tail = tail[len(tail)-1500:]
				}
				t.Fatalf("渲染失败（%v）：%s", runErr, tail)
			}

			frames := probeFrameCount(t, outPath)
			want := int(spec.Duration * float64(spec.FPS))
			if frames <= 0 {
				t.Fatalf("产物帧数为 0：%s", outPath)
			}
			if diff := want - frames; diff > 2 || diff < -2 {
				t.Fatalf("帧数不符：want≈%d got=%d", want, frames)
			}
			// 尺寸也要核：--size 传错会让布局按错误视口算，帧数却完全正常
			if dw, dh := probeDims(t, outPath); dw != c.w || dh != c.h {
				t.Fatalf("尺寸不符：want %dx%d got %dx%d", c.w, c.h, dw, dh)
			}
			t.Logf("%s %s OK: %d 帧 %dx%d → %s", c.style, c.suffix, frames, c.w, c.h, outPath)
		})
	}
}

func probeDims(t *testing.T, path string) (int, int) {
	t.Helper()
	out, err := exec.Command(ffmpegBin2Probe(), "-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0:s=x", path).Output()
	if err != nil {
		t.Logf("ffprobe 取尺寸失败：%v", err)
		return 0, 0
	}
	var w, h int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%dx%d", &w, &h)
	return w, h
}
