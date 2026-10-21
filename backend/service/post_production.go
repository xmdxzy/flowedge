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
	// LoudnessTarget 目标响度（LUFS）；0 表示不做响度归一。
	// 做的时候走两遍法：先渲一条混音音轨测，再带实测值归一（见 loudness.go）。
	LoudnessTarget float64
	// TruePeak 真峰上限（dBTP），默认 -1.5
	TruePeak float64
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
	aparts, a := buildAudioMix(spec, hasAudio)
	parts = append(parts, aparts...)

	return strings.Join(parts, ";"), v, a
}

// PostProcess 执行后期包装。out 是产物路径，in 是源片。
func (s *VideoService) PostProcess(in, out string, spec PostSpec) error {
	info, err := probeMedia(in)
	if err != nil {
		return err
	}
	filter, vLabel, aLabel := buildPostFilter(spec, info.hasAudio)

	tmp, err := os.MkdirTemp("", "flowedge-post-*")
	if err != nil {
		return fmt.Errorf("建临时目录失败：%w", err)
	}
	defer os.RemoveAll(tmp)

	// 响度两遍法：先单渲一条混音音轨测响度，再把实测值喂回第二遍。
	// 只在有音轨且显式要求时才做——纯画面任务不该多付一次渲染。
	if spec.LoudnessTarget != 0 && info.hasAudio {
		mixOut := filepath.Join(tmp, "mix.wav")
		if err := s.renderMixAudio(in, spec, info.hasAudio, mixOut, info.dur); err != nil {
			return err
		}
		st, merr := measureLoudnessFile(s.FFmpegPath, mixOut)
		if merr != nil {
			return merr
		}
		src := aLabel
		if src == "" {
			src = "0:a" // 没有混音链，直接对原音轨归一
		}
		seg := fmt.Sprintf("[%s]%s[anorm]", src, loudnormFilter(st, spec.LoudnessTarget, spec.TruePeak))
		if filter == "" {
			filter = seg
		} else {
			filter += ";" + seg
		}
		aLabel = "anorm"
	}

	if filter == "" {
		// 什么都没配：不重编码，直接复制——"无操作"不该付出一次画质损失
		return copyFile(in, out)
	}

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

	// 标签可能有两种形态：滤镜输出（vsub / anorm）要写成 [label]；
	// 没有滤镜时是原始流（0:v / 0:a），**不能**加方括号——否则 ffmpeg 报
	// "Invalid file index"（EINVAL）。以前只在"只配了 BGM、没改画面"时才会踩到。
	mapArg := func(l string) string {
		if strings.Contains(l, ":") {
			return l
		}
		return "[" + l + "]"
	}

	args = append(args, "-filter_complex", filter, "-map", mapArg(vLabel))
	if aLabel != "" {
		args = append(args, "-map", mapArg(aLabel), "-c:a", "aac", "-b:a", "192k")
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

// buildAudioMix 只生成"混音"那段链（音频部分），返回片段与输出标签。
//
// 单独抽出来是为了响度两遍法：第一遍只需要这条链（-vn 渲一条 wav），
// 第二遍把它和视频链拼在一起。两处各写一份必然漂移。
func buildAudioMix(spec PostSpec, hasAudio bool) ([]string, string) {
	var parts []string
	a := ""
	switch {
	case spec.BGM != "" && hasAudio:
		gain := spec.BGMGainDB
		if gain == 0 {
			gain = -12
		}
		if spec.Duck {
			parts = append(parts,
				"[0:a]asplit=2[voice][sc]",
				fmt.Sprintf("[1:a]volume=%.1fdB[bgm]", gain),
				"[bgm][sc]sidechaincompress=threshold=0.05:ratio=8:attack=10:release=250[bgmduck]",
				"[voice][bgmduck]amix=inputs=2:duration=first:dropout_transition=0:normalize=0[aout]",
			)
		} else {
			parts = append(parts,
				fmt.Sprintf("[1:a]volume=%.1fdB[bgm]", gain),
				"[0:a][bgm]amix=inputs=2:duration=first:dropout_transition=0:normalize=0[aout]",
			)
		}
		a = "aout"
	case spec.BGM != "" && !hasAudio:
		gain := spec.BGMGainDB
		if gain == 0 {
			gain = -6
		}
		parts = append(parts, fmt.Sprintf("[1:a]volume=%.1fdB[aout]", gain))
		a = "aout"
	}
	return parts, a
}

// renderMixAudio 只渲染混音音轨（-vn），供响度第一遍测量。
// 秒级开销：没有视频编码，只是把 voice+BGM 混成一条 pcm wav。
func (s *VideoService) renderMixAudio(in string, spec PostSpec, hasAudio bool, out string, dur float64) error {
	parts, label := buildAudioMix(spec, hasAudio)
	// 没配 BGM 时没有混音链，直接拿原音轨测——这是最常见的情况（只做归一）。
	if label == "" && !hasAudio {
		return fmt.Errorf("原片没有音轨，无法做响度归一")
	}
	absIn, _ := filepath.Abs(in)
	args := []string{"-y", "-v", "error", "-i", absIn}
	if spec.BGM != "" {
		absBGM, _ := filepath.Abs(spec.BGM)
		args = append(args, "-stream_loop", "-1", "-i", absBGM)
	}
	if label != "" {
		args = append(args, "-filter_complex", strings.Join(parts, ";"), "-map", "["+label+"]")
	} else {
		args = append(args, "-map", "0:a")
	}
	args = append(args, "-vn", "-c:a", "pcm_s16le", "-t", fmt.Sprintf("%.3f", dur), out)
	cmd := exec.Command(s.FFmpegPath, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("渲染混音音轨失败：%w，输出：%s", err, truncateRunes(string(output), 400))
	}
	return nil
}
