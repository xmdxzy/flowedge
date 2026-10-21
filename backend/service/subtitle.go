package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 字幕：把「分镜台词」或「逐行文本」变成 .srt，再烧进成片。
//
// 为什么补这个（docs/系统评估-2026-10-21.md §5.2）：
// 字幕 / 竖屏 / BGM 是短视频交付的三道准入门槛，之前一道都没有。
// AddSubtitles 早就写好了（multimodal.go），但**零调用**——属于"看着有，实际是死代码"。
// 这里把它接上，并且补上真正缺的部分：字幕文件从哪来。

// SubtitleLine 一条字幕
type SubtitleLine struct {
	Start float64 // 秒
	End   float64
	Text  string
}

// FormatSRTTime 秒 → SRT 时间戳（00:00:01,500）
func FormatSRTTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec)
	ms := int((sec - float64(total)) * 1000)
	if ms > 999 {
		ms = 999
	}
	return fmt.Sprintf("%02d:%02d:%02d,%03d", total/3600, (total%3600)/60, total%60, ms)
}

// BuildSRT 生成 srt 文本。
//
// 做三件规范化：去掉空行、保证每条至少 0.6s（太短会闪一帧就没了）、
// 相邻条不重叠（重叠会让播放器只显示其中一条，等于丢字幕）。
func BuildSRT(lines []SubtitleLine) string {
	const minDur = 0.6
	var b strings.Builder
	idx := 0
	prevEnd := 0.0
	for _, ln := range lines {
		text := strings.TrimSpace(strings.ReplaceAll(ln.Text, "\n", " "))
		if text == "" {
			continue
		}
		start := ln.Start
		if start < prevEnd {
			start = prevEnd
		}
		end := ln.End
		if end < start+minDur {
			end = start + minDur
		}
		idx++
		b.WriteString(fmt.Sprintf("%d\n%s --> %s\n%s\n\n", idx, FormatSRTTime(start), FormatSRTTime(end), text))
		prevEnd = end
	}
	return b.String()
}

// cleanDialogue 分镜里的台词写法是「身份：原话」。
// 字幕只该显示原话——把身份念出来是错的。
func cleanDialogue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 「无对白」「静默」这类占位不是台词
	switch strings.Trim(s, "「」 ") {
	case "无对白", "无台词", "静默", "无":
		return ""
	}
	if i := strings.Index(s, "："); i >= 0 {
		s = s[i+len("："):]
	} else if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	return strings.Trim(strings.TrimSpace(s), "「」\"'")
}

// SubtitlesFromPlan 按分镜表的时间轴排字幕。
//
// 用 Shot.Seconds 累加而不是实际成片时长：分镜的实际帧数会吸附到 17k+5 网格，
// 与期望秒数有百毫秒级偏差；这里取期望值，偏差交给 BuildSRT 的 minDur 兜。
// 要精确对齐得读成片音轨，那是下一轮的事（ASR 对齐）。
func SubtitlesFromPlan(plan ShotPlan) []SubtitleLine {
	var out []SubtitleLine
	cursor := 0.0
	for _, sh := range plan.Shots {
		dur := sh.Seconds
		if dur <= 0 && sh.Frames > 0 {
			dur = float64(sh.Frames) / 24.0 // H3 默认 24fps
		}
		if dur <= 0 {
			continue
		}
		if txt := cleanDialogue(sh.Dialogue); txt != "" {
			out = append(out, SubtitleLine{Start: cursor + 0.15, End: cursor + dur - 0.15, Text: txt})
		}
		cursor += dur
	}
	return out
}

// SubtitlesFromLines 把逐行文本均分到整条片子上。
// 没有分镜表时的兜底：至少让字幕存在，时间轴粗糙但可用。
func SubtitlesFromLines(texts []string, totalSeconds float64) []SubtitleLine {
	var clean []string
	for _, t := range texts {
		if v := strings.TrimSpace(t); v != "" {
			clean = append(clean, v)
		}
	}
	if len(clean) == 0 || totalSeconds <= 0 {
		return nil
	}
	per := totalSeconds / float64(len(clean))
	out := make([]SubtitleLine, 0, len(clean))
	for i, t := range clean {
		out = append(out, SubtitleLine{
			Start: float64(i)*per + 0.1,
			End:   float64(i+1)*per - 0.1,
			Text:  t,
		})
	}
	return out
}

// SubtitleStyle 烧录样式（只暴露真正会被用到的几项）
type SubtitleStyle struct {
	FontSize int // 0 = 用 ffmpeg 默认
	MarginV  int // 距底部像素
}

// BurnSubtitles 把 srt 烧进画面（重编码视频流、音频流直接 copy）。
//
// 路径坑：`subtitles=` 是**滤镜参数**，不是输入路径。Windows 的 `C:\` 里那个冒号
// 会被 ffmpeg 当成选项分隔符，中文路径还会卡编码。所以这里不拼路径——
// 把 srt 复制成临时目录里的 `sub.srt`，用 `cmd.Dir` 切过去，滤镜里只写文件名。
// 这样任何盘符、任何中文目录都不会炸。
func (s *VideoService) BurnSubtitles(video, srt, out string, style SubtitleStyle) error {
	if _, err := os.Stat(srt); err != nil {
		return fmt.Errorf("字幕文件不存在：%s", srt)
	}
	tmp, err := os.MkdirTemp("", "flowedge-sub-*")
	if err != nil {
		return fmt.Errorf("建临时目录失败：%w", err)
	}
	defer os.RemoveAll(tmp)

	local := filepath.Join(tmp, "sub.srt")
	if err := copyFile(srt, local); err != nil {
		return fmt.Errorf("复制字幕失败：%w", err)
	}

	filter := "subtitles=sub.srt"
	if style.FontSize > 0 || style.MarginV > 0 {
		var parts []string
		if style.FontSize > 0 {
			parts = append(parts, fmt.Sprintf("FontSize=%d", style.FontSize))
		}
		if style.MarginV > 0 {
			parts = append(parts, fmt.Sprintf("MarginV=%d", style.MarginV))
		}
		parts = append(parts, "Outline=2", "Shadow=0")
		filter += fmt.Sprintf(":force_style='%s'", strings.Join(parts, ","))
	}

	absVideo, _ := filepath.Abs(video)
	absOut, _ := filepath.Abs(out)
	cmd := exec.Command(s.FFmpegPath,
		"-y", "-i", absVideo,
		"-vf", filter,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
		"-c:a", "copy",
		absOut,
	)
	cmd.Dir = tmp // 关键：让滤镜里的 sub.srt 是个裸文件名
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(output)
		if strings.Contains(msg, "No such filter") || strings.Contains(msg, "Error initializing filter") {
			return fmt.Errorf("当前 ffmpeg 未编译 libass，无法烧字幕（改用带 libass 的 full 版）：%s",
				truncateRunes(msg, 300))
		}
		return fmt.Errorf("烧字幕失败：%w，输出：%s", err, truncateRunes(msg, 600))
	}
	if _, statErr := os.Stat(absOut); statErr != nil {
		return fmt.Errorf("ffmpeg 返回成功但产物不存在：%s", absOut)
	}
	return nil
}
