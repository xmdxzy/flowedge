package service

// code_video 多风格渲染的 live 冒烟：真调 lemo 渲染链，出片并核对帧数。
// 默认跳过（渲染要几十秒），用 FLOWEDGE_LIVE_CODEVIDEO=1 打开。
//
// 为什么要这条测试：风格模板是「内嵌 HTML/JS」，Go 编译期抓不到语法错误，
// 只有真跑一遍渲染才知道 render.js 有没有抛异常（页面契约没 ready = 渲染器 20s 后超时）。
// 判据是产物：mp4 存在、体积 > 0、ffprobe 能读出正数帧。

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

func liveCodeVideoCards() []CodeCard {
	return []CodeCard{
		{
			Kicker:  "PART 01 · WHY",
			Heading: "同一句旁白，为什么风格差这么多",
			Points: []string{
				"信息密度一样，观感差距来自版式与节奏",
				"观众记住的是「怎么讲」，不是「讲了什么」",
			},
			Note: "SAME SCRIPT",
		},
		{
			Kicker:  "PART 02 · SIGNATURE",
			Heading: "好看不是审美，是可复用的约束",
			Points: []string{
				"三色纪律：白 + 薄荷绿 + 紫，不出现第四种",
				"固定组件库：大字卡 / 图表卡 / 对比面板",
			},
			Figure: "bars",
			Labels: []string{"运镜", "硬切", "版式"},
			Values: []int{82, 12, 46},
			Note: "DESIGN TOKENS",
		},
		{
			Kicker:  "PART 03 · PIPELINE",
			Heading: "先定配音，再排画面",
			Points: []string{
				"音频先行 → 词级时间戳 → 分镜 → 逐帧渲染",
				"反着做，每改一句旁白都要重排一次",
			},
			Note: "AUDIO FIRST",
		},
	}
}

func TestCodeVideoStylesRender(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_CODEVIDEO") != "1" {
		t.Skip("set FLOWEDGE_LIVE_CODEVIDEO=1 to render all code_video styles")
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

	cards := liveCodeVideoCards()
	spec := CodeSpec{
		Title:       "为什么好看的科普片，都不是剪出来的",
		TitleEn:     "FLOWEDGE · STYLE LAB",
		Kicker:      "FLOWEDGE · STYLE LAB",
		FPS:         24,
		CardSeconds: secs,
		Cards:       cards,
	}
	spec = *normalizeCodeSpec(&spec)

	outDir := filepath.Join("..", "..", ".tmp", "codevideo-styles")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("建输出目录失败：%v", err)
	}

	for _, style := range codeVideoStyleNames {
		style := style
		t.Run(style, func(t *testing.T) {
			demoDir, err := os.MkdirTemp("", "flowedge-cvtest-*")
			if err != nil {
				t.Fatalf("临时目录：%v", err)
			}
			defer os.RemoveAll(demoDir)

			if err := materializeCodeTemplate(demoDir, style); err != nil {
				t.Fatalf("写入模板：%v", err)
			}
			// 每套风格都必须落到 render.js + index.html
			for _, f := range []string{"index.html", "render.js", filepath.Join("shared", "kit.js")} {
				if _, err := os.Stat(filepath.Join(demoDir, f)); err != nil {
					t.Fatalf("模板缺文件 %s：%v", f, err)
				}
			}
			b, _ := json.MarshalIndent(spec, "", "  ")
			if err := os.WriteFile(filepath.Join(demoDir, "spec.json"), b, 0o644); err != nil {
				t.Fatalf("写 spec：%v", err)
			}

			outPath, _ := filepath.Abs(filepath.Join(outDir, style+".mp4"))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			args := []string{
				filepath.Join("core", "render", "video.mjs"),
				demoDir,
				"--fps", strconv.Itoa(spec.FPS),
				"--workers", "2",
				"--out", outPath,
				"--size", "1920x1080",
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
			t.Logf("%s OK: %d 帧 / %.1fs → %s", style, frames, spec.Duration, outPath)
		})
	}
}

// probeFrameCount 用 ffprobe 读精确帧数（不看容器时长，见 AGENTS 0.12⑤）。
func probeFrameCount(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command(ffmpegBin2Probe(), "-v", "error",
		"-select_streams", "v:0",
		"-count_frames", "-show_entries", "stream=nb_read_frames",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Logf("ffprobe 失败（%v），退回 size 检查", err)
		if st, e := os.Stat(path); e == nil && st.Size() > 0 {
			return 1
		}
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// ffmpegBin2Probe 找 ffprobe：优先与 ffmpeg 同目录。
func ffmpegBin2Probe() string {
	ff := ffmpegBin()
	base := strings.TrimSuffix(filepath.Base(ff), filepath.Ext(ff))
	probe := filepath.Join(filepath.Dir(ff), strings.Replace(base, "ffmpeg", "ffprobe", 1))
	if _, err := os.Stat(probe); err == nil {
		return probe
	}
	return "ffprobe"
}

var _ = fmt.Sprintf
