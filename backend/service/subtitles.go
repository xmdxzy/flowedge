package service

import (
	"fmt"
	"math"
	"os"
	"strings"
)

// 字幕产物（2026-10-30）：把配音逐行时间戳排到全片时间轴，写成 SRT。
//
// 时间戳来源是 SynthesizeDubTrack 的**实测**（每句合成后 ffprobe 的真实时长，
// DubLineInfo.Start/End）——不是计划值：TTS 计划前导与实际语音起点实测差过
// 0.44s，按计划值排轴必漂（AGENTS §0.12 ②）。
//
// 接缝重叠值与 assembleShots **同源**（transitionOverlapD）：装配器每条接缝
// 吃掉 d 秒，全片时间轴就必须同步减掉，否则每过一条接缝字幕整体漂一截。
// 烧不烧由 produce 层 subtitles/overlay_srt 参数决定（text_overlay.go 的
// SRT 模式）；.srt 文件本身总是落盘——剪映/PR 导入直接用。

// srtLine 一条字幕（全片时间轴，秒）
type srtLine struct {
	Start   float64
	End     float64
	Speaker string
	Text    string
}

// transitionOverlapD 装配器在每条接缝上吃掉的时长（秒）。
// 单独提出来给 SRT 排轴共用：改装配窗口值必须只改这里，
// 两处各写一份迟早漂移（AGENTS §0.12④ 的镜像教训）。
func transitionOverlapD(trans string) float64 {
	switch trans {
	case "dissolve":
		return 0.30
	case "continue":
		return 0.12
	}
	return 0.08 // cut：2 帧融合，视觉上仍是硬切
}

// filmSubtitleLines 把逐镜 dub 行换算成全片时间轴。
// dubs[i] 是第 i 镜的念稿音轨报告（nil = 该镜无台词）；durations[i] 是第 i 镜
// 成片的探测时长；transitions[i] 是第 i 镜与 i+1 镜之间的衔接。
// 镜起点 = Σ前面镜时长 − Σ前面接缝重叠（与 assembleShots 的 total 累计一致）。
func filmSubtitleLines(dubs []*DubTrackReport, durations []float64, transitions []string) []srtLine {
	var out []srtLine
	shotStart := 0.0
	for i := range durations {
		dur := durations[i]
		if rep := dubs[i]; rep != nil {
			shotEnd := shotStart + dur
			for _, ln := range rep.Lines {
				if strings.TrimSpace(ln.Text) == "" {
					continue
				}
				start, end := shotStart+ln.Start, shotStart+ln.End
				if start >= shotEnd {
					continue // 整句被截到镜外（超长截断的残句）：不排
				}
				if end > shotEnd-0.05 {
					end = shotEnd - 0.05 // 台词尾部压线：夹回本镜，不许越到下一镜
				}
				if end-start < 0.2 {
					continue // 夹完太短不可读
				}
				out = append(out, srtLine{Start: start, End: end, Speaker: ln.Speaker, Text: ln.Text})
			}
		}
		if i < len(transitions) {
			d := transitionOverlapD(transitions[i])
			// 与装配器同一条护栏：融合窗口装不下短镜就退回 cut 档
			if dur > 0 && d >= dur-0.1 {
				d = 0.08
			}
			shotStart += dur - d
		}
	}
	return out
}

// srtTimestamp SRT 时间戳：00:00:01,080（毫秒用逗号，这是 SRT 规范，不是打错）
func srtTimestamp(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(math.Round(sec * 1000))
	h := ms / 3600000
	ms %= 3600000
	m := ms / 60000
	ms %= 60000
	s := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// writeShotSRT 按 SRT 规范落盘：序号从 1 起、时间轴行、文本行、空行分隔。
// 有说话人时带「名字：」前缀——多角色片的字幕惯例，观众靠它分辨谁在说。
func writeShotSRT(path string, lines []srtLine) error {
	var b strings.Builder
	for i, ln := range lines {
		text := strings.TrimSpace(ln.Text)
		if ln.Speaker != "" {
			text = ln.Speaker + "：" + text
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTimestamp(ln.Start), srtTimestamp(ln.End), text)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
