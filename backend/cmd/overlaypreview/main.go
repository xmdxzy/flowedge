// overlaypreview 是贴字的"先看效果"工具：把同一段文字按各预设各渲一版，
// 直接看静帧再决定出片参数。零 GPU，几秒钟出结果。
//
//	go run ./backend/cmd/overlaypreview -src clip.mp4 -text "山海茶肆|今夜开张" -out .tmp/preview
//
// 为什么要有它：贴字的观感（字重/描边/位置/淡入）在 352p 和 720p 上差别很大，
// 而重新出一次片要几分钟。先用现成成片迭代排版，定了再演进流水线。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"flowedge/backend/service"
)

func main() {
	src := flag.String("src", "", "输入视频")
	text := flag.String("text", "山海茶肆", "要贴的文字，换行用 | 分隔")
	srt := flag.String("srt", "", "字幕文件（.srt/.ass）；给了按时间轴烧，忽略 -text")
	outDir := flag.String("out", ".tmp/overlay-preview", "输出目录")
	scale := flag.Int("scale", 0, "上采样目标高度（0=保持）")
	from := flag.Float64("from", 0.5, "出现时间")
	to := flag.Float64("to", 0, "消失时间（0=到片尾）")
	flag.Parse()

	if *src == "" {
		fmt.Fprintln(os.Stderr, "缺少 -src")
		os.Exit(2)
	}
	if _, err := os.Stat(*src); err != nil {
		fmt.Fprintf(os.Stderr, "输入不存在：%v\n", err)
		os.Exit(2)
	}
	if *srt != "" {
		if _, err := os.Stat(*srt); err != nil {
			fmt.Fprintf(os.Stderr, "字幕文件不存在：%v\n", err)
			os.Exit(2)
		}
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "建目录失败：%v\n", err)
		os.Exit(2)
	}

	body := strings.ReplaceAll(*text, "|", "\n")
	styles := []string{service.StyleCaption, service.StyleTitle, service.StylePlate, service.StyleLowerThird}
	for _, style := range styles {
		for _, sc := range scaleList(*scale) {
			opt := service.OverlayOptions{
				Text:  body,
				SRT:   *srt,
				Style: style,
				Scale: sc,
				From:  *from,
				To:    *to,
			}
			// 复制一份输入到输出目录：ApplyTextOverlay 会把结果写在同目录，
			// 不污染产物目录。
			base := strings.TrimSuffix(filepath.Base(*src), filepath.Ext(*src))
			work := filepath.Join(*outDir, fmt.Sprintf("%s-%s-%d.mp4", base, style, sc))
			if err := copyFile(*src, work); err != nil {
				fmt.Fprintf(os.Stderr, "复制失败：%v\n", err)
				os.Exit(1)
			}
			dst, err := service.PreviewOverlay(work, opt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s scale=%d] 失败：%v\n", style, sc, err)
				continue
			}
			fmt.Printf("[%s scale=%d] -> %s\n", style, sc, dst)
		}
	}
}

func scaleList(s int) []int {
	if s > 0 {
		return []int{s}
	}
	// 默认同时给「原分辨率」和「720p」两版，好判断上采样值不值。
	return []int{0, 720}
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
