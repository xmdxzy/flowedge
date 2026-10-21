package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 后期包装：竖屏重构 + BGM 混音（侧链闪避）+ 可选字幕，**一次重编码**完成。
//
// 为什么要合在一个节点里（docs/系统评估-2026-10-21.md §5.2）：
// 竖屏、BGM、字幕是短视频交付的三道门槛。但它们如果各做一个节点、各跑一次 ffmpeg，
// 就是三次 libx264 重编码——画质掉三轮，耗时也翻三倍。
// 这里用一条 filter_complex 一次做完；单独只需要字幕时仍可用更简单的 subtitles 节点。
//
// 对外只暴露真正会被用到的参数，不做成 ffmpeg 参数翻译器。

// PostSpec 后期参数
type PostSpec struct {
	Width, Height int     // 目标分辨率；0 表示不改构图
	Fit           string  // crop（中心裁切，默认）| blur（模糊垫底，保内容）
	BGM           string  // 背景音乐路径；空表示不混
	BGMGainDB     float64 // 相对原曲的增益（dB），负数=压低
	Duck          bool    // 是否用侧链压缩让人声出现时自动压低 BGM
	SRT           string  // 已就绪的字幕文件（烧录用）
	FontSize      int
	MarginV       int
}

// buildPostFilter 生成 filter_complex 与输入/输出标签（纯函数，便于单测）。
//
// 返回值：filter、视频输出标签、音频输出标签（无音频时为空）。
func buildPostFilter(spec PostSpec, hasAudio bool) (string, string, string) {
	var parts []string
	v := "0:v"

	// ---- 画面：重构图 ----
	if spec.Width > 0 && spec.Height > 0 {
		w, h := spec.Width, spec.Height
		if spec.Fit == "blur" {
			// 模糊垫底：原画面完整保留，两侧用放大模糊填满——横屏改竖屏不丢内容
			parts = append(parts,
				fmt.Sprintf("[%s]split=2[bga][fga]", v),
				fmt.Sprintf("[bga]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,boxblur=24:2[bg]", w, h, w, h),
				fmt.Sprintf("[fga]scale=%d:%d:force_original_aspect_ratio=decrease[fg]", w, h),
				"[bg][fg]overlay=(W-w)/2:(H-h)/2[vfit]",
			)
		} else {
			// 中心裁切：先等比放大到铺满，再裁中间——最常用的竖屏改法
			parts = append(parts,
				fmt.Sprintf("[%s]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d[vfit]", v, w, h, w, h),
			)
		}
		v = "vfit"
	}

	// ---- 字幕：滤镜里只写裸文件名 sub.srt（调用方会把工作目录切过去） ----
	if spec.SRT != "" {
		f := "subtitles=sub.srt"
		if spec.FontSize > 0 || spec.MarginV > 0 {
			var st []string
			if spec.FontSize > 0 {
				st = append(st, fmt.Sprintf("FontSize=%d", spec.FontSize))
			}
			if spec.MarginV > 0 {
				st = append(st, fmt.Sprintf("MarginV=%d", spec.MarginV))
			}
			st = append(st, "Outline=2", "Shadow=0")
			f += fmt.Sprintf(":force_style='%s'", strings.Join(st, ","))
		}
		parts = append(parts, fmt.Sprintf("[%s]%s[vsub]", v, f))
		v = "vsub"
	}

	// ---- 声音：BGM 混音 + 侧链闪避 ----
	//
	// amix 必须 normalize=0：默认 normalize=1 会按输入数把每路压低（约 -6dB），
	// 叠上侧链压缩后 BGM 只剩 -34dB —— 实测「660Hz 频段能量比母片还低」，
	// 也就是"混了但听不见"。这条是 A/B 测出来的，不是推的。
	a := ""
	switch {
	case spec.BGM != "" && hasAudio:
		gain := spec.BGMGainDB
		if gain == 0 {
			gain = -12
		}
		if spec.Duck {
			// 人声分两路：一路参与混音，一路只当侧链信号。
			// sidechaincompress 第一个输入是被压的对象（BGM），第二个是参照（人声）：
			// 人声一响，BGM 自动退下去，不用手写关键帧。
			parts = append(parts,
				"[0:a]asplit=2[voice][sc]",
				fmt.Sprintf("[1:a]volume=%.1fdB[bgm]", gain),
				// 参数是实测扫出来的（660Hz 窄带测 BGM 电平）：
				//   threshold 0.05 / ratio 8 → 有人声时压 5.9dB，静音时 0dB（不误压）
				//   threshold 0.08 / ratio 4 → 只压 2.8dB，等于没闪避
				//   threshold 0.03 / ratio 6 → 压 8.4dB，但 AAC 底噪也会被压（BGM 整体矮 3dB）
				"[bgm][sc]sidechaincompress=threshold=0.05:ratio=8:attack=10:release=250[bgmduck]",
				"[voice][bgmduck]amix=inputs=2:duration=first:dropout_transition=0:normalize=0[aout]",
			)
		} else {
			// 不闪避时**不能**保留 asplit：旁路输出 [sc] 没人消费，
			// ffmpeg 直接报 "Filter asplit has an unconnected output"（EINVAL）。
			parts = append(parts,
				fmt.Sprintf("[1:a]volume=%.1fdB[bgm]", gain),
				"[0:a][bgm]amix=inputs=2:duration=first:dropout_transition=0:normalize=0[aout]",
			)
		}
		a = "aout"
	case spec.BGM != "" && !hasAudio:
		// 原片没音轨：BGM 直接当主音轨（长度由末尾 -t 兜住）
		gain := spec.BGMGainDB
		if gain == 0 {
			gain = -6
		}
		parts = append(parts, fmt.Sprintf("[1:a]volume=%.1fdB[aout]", gain))
		a = "aout"
	}

	return strings.Join(parts, ";"), v, a
}

// PostProcess 执行后期包装。out 是产物路径，in 是源片。
func (s *VideoService) PostProcess(in, out string, spec PostSpec) error {
	info, err := probeMedia(in)
	if err != nil {
		return err
	}
	filter, vLabel, aLabel := buildPostFilter(spec, info.hasAudio)
	if filter == "" {
		// 什么都没配：不重编码，直接复制——"无操作"不该付出一次画质损失
		return copyFile(in, out)
	}

	tmp, err := os.MkdirTemp("", "flowedge-post-*")
	if err != nil {
		return fmt.Errorf("建临时目录失败：%w", err)
	}
	defer os.RemoveAll(tmp)

	absIn, _ := filepath.Abs(in)
	absOut, _ := filepath.Abs(out)
	args := []string{"-y", "-i", absIn}

	if spec.BGM != "" {
		absBGM, _ := filepath.Abs(spec.BGM)
		if _, statErr := os.Stat(absBGM); statErr != nil {
			return fmt.Errorf("BGM 文件不存在：%s", spec.BGM)
		}
		// BGM 通常比成片短：用 -stream_loop 铺满整条，长度由末尾的 -t 兜住
		args = append(args, "-stream_loop", "-1", "-i", absBGM)
	}
	if spec.SRT != "" {
		if _, statErr := os.Stat(spec.SRT); statErr != nil {
			return fmt.Errorf("字幕文件不存在：%s", spec.SRT)
		}
		if err := copyFile(spec.SRT, filepath.Join(tmp, "sub.srt")); err != nil {
			return fmt.Errorf("复制字幕失败：%w", err)
		}
	}

	args = append(args, "-filter_complex", filter, "-map", "["+vLabel+"]")
	if aLabel != "" {
		args = append(args, "-map", "["+aLabel+"]", "-c:a", "aac", "-b:a", "192k")
	} else if info.hasAudio {
		args = append(args, "-map", "0:a?", "-c:a", "copy")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
		"-t", fmt.Sprintf("%.3f", info.dur),
		absOut,
	)

	cmd := exec.Command(s.FFmpegPath, args...)
	cmd.Dir = tmp // 让滤镜里的 sub.srt 是裸文件名，绕开 Windows 盘符冒号
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(output)
		if spec.SRT != "" && (strings.Contains(msg, "No such filter") || strings.Contains(msg, "Error initializing filter")) {
			return fmt.Errorf("当前 ffmpeg 未编译 libass，无法烧字幕（改用带 libass 的 full 版）：%s",
				truncateRunes(msg, 300))
		}
		return fmt.Errorf("后期处理失败：%w，输出：%s", err, truncateRunes(msg, 600))
	}
	if _, statErr := os.Stat(absOut); statErr != nil {
		return fmt.Errorf("ffmpeg 返回成功但产物不存在：%s", absOut)
	}
	return nil
}
