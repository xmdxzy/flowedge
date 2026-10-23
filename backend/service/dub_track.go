package service

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 台词 → 念稿音轨（Go 移植，真相源 scripts/dub-track.py）
//
// 为什么需要这一层
// ----------------
// ref2va 里写 `<Audio 1>: fully_copy` 时，这段音频会成为成片的**完整最终音轨**，
// 模型只负责对口型，不再自己造声音。所以：
//   - 台词必须提前合成（TTS），不能指望模型念
//   - 环境声必须提前混进来（模型不会往音轨里加东西）
//   - 音轨长度要与视频时长对齐，否则口型会漂
//
// 与脚本版逐步骤对齐（含 2026-10-20 实测教训）：
//  1. Kokoro 每句自带约 0.44s 前导静音——不裁掉，每句台词都比画面晚开口
//  2. 单句 loudnorm：整条轨道归一化会被长静音带偏，把台词压到 ASR 转不出字
//  3. 环境声先定电平再相加：先 amix 再 loudnorm 会让底噪占掉响度预算
//  4. 限幅：TTS 原始峰值常顶到 0dB，fully_copy 会把削波原样带进成片

const dubOutRate = 32000

// DubLineInfo 一句台词的实测起止（台词是否落在镜头切点上，靠这两个数核对）
type DubLineInfo struct {
	Speaker string  `json:"speaker"`
	Text    string  `json:"text"`
	Seconds float64 `json:"seconds"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
}

// DubTrackReport 念稿音轨合成摘要
type DubTrackReport struct {
	Out           string        `json:"out"`
	Seconds       float64       `json:"seconds"`        // 音轨总长（对齐视频时长后）
	SpokenSeconds float64       `json:"spoken_seconds"` // 纯人声时长（不含尾随静音）
	Lines         []DubLineInfo `json:"lines"`
	Backend       string        `json:"backend"` // service | sapi | sapi-fallback
	Note          string        `json:"note,omitempty"`
}

// ffmpegRun 跑一次 ffmpeg，返回合并输出（错误信息里要带 stderr 尾巴，
// 否则「滤镜语法错」这种只会在 stderr 里说原因的失败没法诊断）。
func ffmpegRun(args ...string) (string, error) {
	raw, err := exec.Command(ffmpegBin(), args...).CombinedOutput()
	return string(raw), err
}

// concatListPath 写 ffmpeg concat 反分接器的清单文件。
// 路径统一转正斜杠 + 单引号转义——Windows 反斜杠在清单文件里会被吞。
func concatListPath(dir string, parts []string) (string, error) {
	var b strings.Builder
	for _, p := range parts {
		esc := strings.ReplaceAll(p, `\`, `/`)
		esc = strings.ReplaceAll(esc, "'", `'\''`)
		b.WriteString("file '" + esc + "'\n")
	}
	lp := filepath.Join(dir, "list.txt")
	return lp, os.WriteFile(lp, []byte(b.String()), 0600)
}

// audioDuration 用 ffmpeg -i 解析时长（对标 dub-track.py 的 duration_of）。
// 注意 ffmpeg 对「只给 -i 不给输出」会返回非 0 退出码，但时长信息照常印在
// stderr——所以这里读输出、不把退出码当失败。
func audioDuration(path string) float64 {
	out, _ := ffmpegRun("-hide_banner", "-i", path)
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "Duration:") {
			continue
		}
		ts := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(ln, "Duration:"), ",", 2)[0])
		parts := strings.Split(ts, ":")
		if len(parts) != 3 {
			continue
		}
		h, _ := strconv.ParseFloat(parts[0], 64)
		m, _ := strconv.ParseFloat(parts[1], 64)
		s, _ := strconv.ParseFloat(parts[2], 64)
		return h*3600 + m*60 + s
	}
	return 0
}

// mediaSeconds 探测媒体时长（秒）。探测不到返回 0 + 错误——调用方必须
// 把「测不到」当失败，不许拿 0 当静音。
func mediaSeconds(path string) (float64, error) {
	if d := audioDuration(path); d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("探测不到时长：%s", path)
}

// polishVoice 裁静音 + 去机械感 + 加空间感 + 单句归一化。
// 失败返回 false（调用方退回原始 TTS 并明说，不中断）。
func polishVoice(src, dst string) bool {
	af := strings.Join([]string{
		// 裁首尾静音，保留 30/50ms 余量避免切到起音和收尾
		"silenceremove=start_periods=1:start_threshold=-45dB:start_silence=0.03",
		"areverse",
		"silenceremove=start_periods=1:start_threshold=-45dB:start_silence=0.05",
		"areverse",
		// 去机械感：切掉低频嗡、抬一点胸腔共鸣
		"highpass=f=85",
		"equalizer=f=180:t=q:w=1.0:g=1.5",
		// 空间感：三个抽头的早期反射（约 2.3m / 4.8m / 8.8m 的墙面反射）
		"aecho=0.85:0.85:55|115|210:0.22|0.14|0.08",
		// 混响尾不要被裁掉
		"apad=pad_dur=0.2",
		// 单句归一化（必须在单句上做，理由见文件头）
		"loudnorm=I=-16:TP=-2:LRA=9",
		fmt.Sprintf("aresample=%d", dubOutRate),
	}, ",")
	out, err := ffmpegRun("-y", "-v", "error", "-i", src, "-af", af,
		"-ar", fmt.Sprint(dubOutRate), "-ac", "2", dst)
	if err != nil {
		log.Printf("[dub] 整形失败（%v）：%.200s", err, out)
		return false
	}
	return true
}

// silenceWav 生成一段静音 wav
func silenceWav(dir, name string, seconds float64) (string, error) {
	if seconds <= 0 {
		return "", nil
	}
	p := filepath.Join(dir, name)
	out, err := ffmpegRun("-y", "-v", "error", "-f", "lavfi",
		"-i", fmt.Sprintf("anullsrc=r=%d:cl=stereo", dubOutRate),
		"-t", fmt.Sprintf("%.3f", seconds), p)
	if err != nil {
		return "", fmt.Errorf("生成静音失败：%.200s", out)
	}
	return p, nil
}

// SynthesizeDubTrack 把台词表合成一条「念稿音轨」，供 H3 ref2va 的 ref_audio 用。
//
// targetSeconds 给定则补静音/截断到该长度（对齐视频帧数）；ambienceFile 非空则
// 循环铺满并混入（先定电平再相加）。lines 为空返回错误——没有台词的镜头
// 不该走这条路（那是氛围段，交给 audioHasSpeech 抽检）。
func SynthesizeDubTrack(tts *TTSService, lines []PlanLine, outPath string,
	targetSeconds, lead, ambienceGain float64, ambienceFile string) (*DubTrackReport, error) {

	if tts == nil {
		return nil, fmt.Errorf("TTS 服务不可用（未配置），无法合成念稿音轨")
	}
	usable := make([]PlanLine, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln.Text) != "" {
			usable = append(usable, ln)
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("台词表为空")
	}

	dir, err := os.MkdirTemp("", "flowedge-dub-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return nil, err
	}

	parts := make([]string, 0, len(usable)*2+2)
	report := &DubTrackReport{Lines: []DubLineInfo{}}

	// 开场留白
	cursor := 0.0
	if lead > 0 {
		p, err := silenceWav(dir, "lead.wav", lead)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
		cursor = lead
	}

	for i, ln := range usable {
		wav := filepath.Join(dir, fmt.Sprintf("line%d.wav", i))
		// 语速：倍率 → SAPI rate（-10..10）。服务端不认 rate 也会忽略，无害。
		rate := int((ln.Speed - 1) * 10)
		if rate < -10 {
			rate = -10
		}
		if rate > 10 {
			rate = 10
		}
		backend, terr := tts.SynthesizeWith(ln.Text, wav, TTSOptions{Rate: rate, Speed: ln.Speed})
		if terr != nil {
			return nil, fmt.Errorf("第 %d 句 TTS 合成失败：%w", i+1, terr)
		}
		report.Backend = backend
		// 逐句整形：裁静音 → 去机械感 → 加空间感 → 单句归一化。
		norm := filepath.Join(dir, fmt.Sprintf("line%d-n.wav", i))
		if polishVoice(wav, norm) {
			wav = norm
		} else {
			report.Note = fmt.Sprintf("第 %d 句整形失败，使用原始 TTS", i+1)
		}
		parts = append(parts, wav)
		dur, derr := mediaSeconds(wav)
		if derr != nil {
			return nil, fmt.Errorf("第 %d 句时长探测失败：%w", i+1, derr)
		}
		report.Lines = append(report.Lines, DubLineInfo{
			Speaker: ln.Speaker, Text: ln.Text,
			Seconds: roundTo(dur, 3), Start: roundTo(cursor, 3), End: roundTo(cursor+dur, 3),
		})
		cursor += dur
		if ln.Pause > 0 {
			p, err := silenceWav(dir, fmt.Sprintf("gap%d.wav", i), ln.Pause)
			if err != nil {
				return nil, err
			}
			parts = append(parts, p)
			cursor += ln.Pause
		}
	}

	listFile, err := concatListPath(dir, parts)
	if err != nil {
		return nil, err
	}
	joined := filepath.Join(dir, "joined.wav")
	out, err := ffmpegRun("-y", "-v", "error", "-f", "concat", "-safe", "0",
		"-i", listFile, "-ar", fmt.Sprint(dubOutRate), "-ac", "2", joined)
	if err != nil {
		return nil, fmt.Errorf("音轨拼接失败：%.300s", out)
	}
	spoken, err := mediaSeconds(joined)
	if err != nil {
		return nil, fmt.Errorf("音轨时长探测失败：%w", err)
	}
	report.SpokenSeconds = roundTo(spoken, 3)

	// 目标时长对齐：不足补静音，超出必须截断。
	// 不截断会让 wav 比视频长，mux 时 -shortest 会砍掉结尾——如果台词压在末尾就丢字。
	if targetSeconds > 0 && spoken > targetSeconds+0.05 {
		cut := filepath.Join(dir, "cut.wav")
		if _, err := ffmpegRun("-y", "-v", "error", "-i", joined,
			"-t", fmt.Sprintf("%.3f", targetSeconds),
			"-ar", fmt.Sprint(dubOutRate), "-ac", "2", cut); err == nil {
			joined, spoken = cut, targetSeconds
			report.Note = fmt.Sprintf("台词总长超过目标 %.2fs，已截断（lead/pause 需要收紧）", targetSeconds)
		}
	}
	if targetSeconds > 0 && spoken < targetSeconds {
		pad, err := silenceWav(dir, "pad.wav", targetSeconds-spoken)
		if err != nil {
			return nil, err
		}
		list2, err := concatListPath(dir, []string{joined, pad})
		if err != nil {
			return nil, err
		}
		joined2 := filepath.Join(dir, "joined2.wav")
		out, err := ffmpegRun("-y", "-v", "error", "-f", "concat", "-safe", "0",
			"-i", list2, "-ar", fmt.Sprint(dubOutRate), "-ac", "2", joined2)
		if err != nil {
			return nil, fmt.Errorf("尾部补静音失败：%.300s", out)
		}
		joined = joined2
	}

	final := joined
	mixedEnv := false
	if ambienceFile != "" {
		span := spoken
		if targetSeconds > span {
			span = targetSeconds
		}
		gain := ambienceGain
		if gain <= 0 {
			gain = 0.85
		}
		mixed := filepath.Join(dir, "mixed.wav")
		// 顺序很重要：先分别定电平，再相加。先 amix 再 loudnorm 会让底噪
		// 占掉响度预算 → 人声被压到听不清（实测把第三段整段判成静音）。
		if _, err := ffmpegRun("-y", "-v", "error",
			"-i", joined, "-stream_loop", "-1", "-i", ambienceFile,
			"-filter_complex",
			fmt.Sprintf("[0:a]anull[voc];"+
				"[1:a]volume=%.2f,atrim=0:%.3f[amb];"+
				"[voc][amb]amix=inputs=2:duration=first:normalize=0,"+
				"alimiter=limit=0.95[out]", gain, span),
			"-map", "[out]", "-ar", fmt.Sprint(dubOutRate), "-ac", "2", mixed); err != nil {
			report.Note += "；环境声混入失败，仅保留人声"
		} else {
			final, mixedEnv = mixed, true
		}
	}

	// 限幅：TTS 原始峰值常顶到 0dB，fully_copy 会把这个削波原样带进成片。
	// 混过环境声的已经在上一步归一化，再 loudnorm 会把底噪重新抬起来。
	af := "alimiter=limit=0.95"
	if !mixedEnv {
		af = "loudnorm=I=-16:TP=-1.5:LRA=11"
	}
	out, err = ffmpegRun("-y", "-v", "error", "-i", final,
		"-af", af, "-ar", fmt.Sprint(dubOutRate), "-ac", "2", "-c:a", "pcm_s16le", outPath)
	if err != nil {
		return nil, fmt.Errorf("念稿音轨写出失败：%.300s", out)
	}
	total, err := mediaSeconds(outPath)
	if err != nil {
		return nil, fmt.Errorf("念稿音轨自检失败：%w", err)
	}
	report.Out = outPath
	report.Seconds = roundTo(total, 3)
	return report, nil
}

// dubTrackTempPath 台词段念稿音轨的落盘位置（跟成片同目录，便于回溯）
func dubTrackTempPath(outDir string, shotID int) string {
	return filepath.Join(outDir, fmt.Sprintf("dub-seg%d-%d.wav", shotID, time.Now().UnixNano()))
}
