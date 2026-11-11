package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 响度归一（EBU R128，两遍法）。
//
// 为什么必须两遍（docs/系统评估-2026-10-21.md §5.2、engine-gap-audit §8）：
// 一遍法（`loudnorm=I=..:TP=..`）是**动态**归一——它边测边调增益，
// 结果是整体响度接近目标，但段内动态被压平、开头几秒还常偏离；
// 而且没有真峰限幅，BGM 叠上来容易过 0dBFS。
//
// 两遍法：第一遍只渲染混音音轨并测量（integrated / true peak / LRA / threshold），
// 第二遍把测得的数值喂回 loudnorm 开 linear=true——增益是**恒定**的，
// 动态不动，且 TP 受控。
//
// 代价是一次额外的音频渲染（`-vn`，无视频编码，秒级），相对成片编码可忽略。

// LoudnessStats 第一遍测得的响度指标
type LoudnessStats struct {
	I         float64 // integrated loudness (LUFS)
	TP        float64 // true peak (dBTP)
	LRA       float64 // loudness range (LU)
	Threshold float64
	Valid     bool
}

// loudnormJSON ffmpeg loudnorm 的 print_format=json 输出（字段都是字符串）
type loudnormJSON struct {
	InputI      string `json:"input_i"`
	InputTP     string `json:"input_tp"`
	InputLRA    string `json:"input_lra"`
	InputThresh string `json:"input_thresh"`
}

// parseLoudnormJSON 从 ffmpeg 的 stderr 里截出那段 JSON 并解析。
//
// ffmpeg 把 JSON 打在 stderr，前后混着 banner 和 stream 信息，
// 所以取**最后一个**含 input_i 的 `{...}` 块，而不是从第一个大括号开始扫。
func parseLoudnormJSON(raw string) (LoudnessStats, bool) {
	idx := strings.LastIndex(raw, `"input_i"`)
	if idx < 0 {
		return LoudnessStats{}, false
	}
	start := strings.LastIndex(raw[:idx], "{")
	end := strings.Index(raw[idx:], "}")
	if start < 0 || end < 0 {
		return LoudnessStats{}, false
	}
	blob := raw[start : idx+end+1]
	var v loudnormJSON
	if err := json.Unmarshal([]byte(blob), &v); err != nil {
		return LoudnessStats{}, false
	}
	parse := func(s string) float64 {
		f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f
	}
	st := LoudnessStats{
		I: parse(v.InputI), TP: parse(v.InputTP),
		LRA: parse(v.InputLRA), Threshold: parse(v.InputThresh),
	}
	// -inf / nan 会被 ParseFloat 变成 0；用 Valid 明确表达"测到了有效值"
	st.Valid = st.I != 0 || st.TP != 0
	return st, st.Valid
}

// loudnormFilter 第二遍的滤镜串：喂入第一遍实测值 + linear。
//
// linear=true 让 loudnorm 用**恒定增益**而不是动态压缩——这是两遍法的意义所在。
// 末尾接 alimiter 兜住真峰（loudnorm 的 TP 是软约束，AAC 编码后仍可能轻微过冲）。
func loudnormFilter(st LoudnessStats, targetLUFS, tpDB float64) string {
	if targetLUFS == 0 {
		targetLUFS = -14 // 短视频平台通用目标
	}
	if tpDB == 0 {
		tpDB = -1.5
	}
	base := fmt.Sprintf("loudnorm=I=%.1f:TP=%.1f:LRA=11", targetLUFS, tpDB)
	if st.Valid {
		base += fmt.Sprintf(":measured_I=%.2f:measured_TP=%.2f:measured_LRA=%.2f:measured_thresh=%.2f:offset=0:linear=true",
			st.I, st.TP, st.LRA, st.Threshold)
	}
	// alimiter 的 limit 是线性幅度（-1.5dBFS ≈ 0.841）
	limit := math.Pow(10, tpDB/20)
	return fmt.Sprintf("%s,alimiter=level=false:limit=%.3f", base, limit)
}

// measureLoudnessFile 对已有音频文件测一遍响度（第一遍的测量动作）。
func measureLoudnessFile(ffmpegPath, path string) (LoudnessStats, error) {
	cmd := hiddenExec(ffmpegPath, "-hide_banner", "-nostats",
		"-i", path, "-af", "loudnorm=print_format=json", "-f", "null", "-")
	out, _ := cmd.CombinedOutput()
	st, ok := parseLoudnormJSON(string(out))
	if !ok {
		return st, fmt.Errorf("响度测量失败（拿不到 loudnorm 输出）：%s", truncateRunes(string(out), 300))
	}
	return st, nil
}
