package service

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// 出片后的跨镜色温一致性检查。
//
// 为什么 plan 层门不够：分镜表可以两镜都写「冷色、低对比」，但生成模型仍可能
// 一镜偏暖一镜偏冷（实测：10 秒两镜片，第 1 镜暖黄、第 2 镜蓝调，接缝处观感跳变）。
// 那是**生成侧**的漂移，只能量产物本身。
//
// 怎么量（零新依赖）：ffmpeg 的 signalstats 给出每帧 YUV 的 UAVG / VAVG。
//   - VAVG > 128 → 偏红（暖）；< 128 → 偏绿
//   - UAVG < 128 → 偏蓝（冷）；> 128 → 偏黄
// 这里只用它做一个「相对漂移」判定：相邻两镜的 U/V 均值差超过阈值就提示，
// 不做绝对色温换算（那需要色彩管理，超出本工具该承担的范围）。

// chromaDriftThreshold 相邻镜头 U/V 均值的容差（YUV 8bit，128 为中性）。
// 实测同场景两镜正常波动 <4；冷暖切换一般 >8。
const chromaDriftThreshold = 6.0

// clipChroma 取一段视频的平均 U/V（采样若干帧）。
//
// 用 ffmpeg 的 metadata=print 而不是 ffprobe -show_entries：后者要 lavfi 输入
// 加 movie= 转义（Windows 路径里的冒号/反斜杠），错一步就静默拿不到值。
func clipChroma(path string) (u, v float64, ok bool) {
	args := []string{
		"-hide_banner", "-nostats",
		"-i", path,
		// 每秒 2 帧足够代表色温；signalstats 算 U/V 均值，metadata=print 打到 stderr
		"-vf", "fps=2,signalstats,metadata=print",
		"-f", "null", "-",
	}
	out, _ := exec.Command(ffmpegBin(), args...).CombinedOutput()
	if len(out) == 0 {
		return 0, 0, false
	}
	var su, sv float64
	var nu, nv int
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.Contains(line, "lavfi.signalstats.UAVG="):
			if val, err := parseStatValue(line); err == nil {
				su += val
				nu++
			}
		case strings.Contains(line, "lavfi.signalstats.VAVG="):
			if val, err := parseStatValue(line); err == nil {
				sv += val
				nv++
			}
		}
	}
	if nu == 0 || nv == 0 {
		return 0, 0, false
	}
	return su / float64(nu), sv / float64(nv), true
}

// parseStatValue 从 "...lavfi.signalstats.UAVG=128" 取数值。
func parseStatValue(line string) (float64, error) {
	i := strings.LastIndex(line, "=")
	if i < 0 || i+1 >= len(line) {
		return 0, fmt.Errorf("no value in %q", line)
	}
	return strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
}

// AuditClipChroma 逐对比较相邻镜头的色度均值，漂移超阈值就报一条 WARN。
//
// 只做相邻比较：整片都偏色是风格，相邻跳变才是接缝问题。
func AuditClipChroma(clips []string) []ShotConsistencyIssue {
	var issues []ShotConsistencyIssue
	if len(clips) < 2 {
		return issues
	}
	type chroma struct {
		u, v float64
		ok   bool
	}
	vals := make([]chroma, len(clips))
	for i, c := range clips {
		u, v, ok := clipChroma(c)
		vals[i] = chroma{u, v, ok}
	}
	for i := 0; i+1 < len(vals); i++ {
		a, b := vals[i], vals[i+1]
		if !a.ok || !b.ok {
			continue // 量不到就不判（不可达 ≠ 没漂移）
		}
		du, dv := b.u-a.u, b.v-a.v
		if absFloat(du) > chromaDriftThreshold || absFloat(dv) > chromaDriftThreshold {
			dir := "偏冷"
			if dv > 0 || du < 0 {
				dir = "偏暖"
			}
			issues = append(issues, ShotConsistencyIssue{
				Level:  "WARN",
				Code:   "plan.color_shift",
				ShotID: i + 2,
				Msg: fmt.Sprintf("第 %d 镜相对上一镜色温漂移 %.1f/%.1f（U/V，阈值 %.0f）→ 观感%s；"+
					"可在分镜表把两镜的 light 写成同一句，或提高一致性要求后重跑",
					i+2, du, dv, chromaDriftThreshold, dir),
			})
		}
	}
	return issues
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
