package service

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"flowedge/backend/model"
)

// quality_gate 节点：对上游产物跑声明式质检，FAIL 阻断下游。
//
// 由来：scripts/verify-media.py 的五层验收（流列表 / 帧数 / 音画同步 /
// 静音缺口 / 响度）原本只在交付前手工跑。节点化之后，「生成→验收→装配」
// 可以画进同一张图，质检不过就地失败，而不是等成片交付出问题再回头查。
//
// 复用而非重造：帧数 / 时长 / 尺寸 / 音画差用 video_edit.go 的 inspectMedia
// （-count_frames 全解码精确计数，就是「时长正常但整段画面丢失」那类缺陷的
// 唯一可靠检测手段）；响度 / 静音缺口是本文件新增的 volumedetect +
// silencedetect 组合（一次 ffmpeg 同时拿两项）。
//
// 设计纪律（与 verify-media.py 一致）：
//   - 检查项分 fail / warn 两级：只有 fail 级会阻断（warn 只进 checks 列表）
//   - 「不可达」与「空结果」必须分开报：ffprobe 失败报 execution 错误，
//     不伪装成「质检通过」
//   - 声明式规则全部来自节点 data，没配的项不检

// gateCheck 单项检查结果
//
// Fix 是「可执行修复建议」：门只报 FAIL 码时，用户还得自己去猜改哪个参数，
// 等于把排查成本原样退回去。这里把每项失败落成一句能直接照做的动作
// （改哪个节点字段、改成多少、用哪条命令），随失败一起抛出。
type gateCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Level  string `json:"level"` // fail（阻断）/ warn（备忘）
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// execQualityGateNode 质检门执行器
func (e *EngineV2) execQualityGateNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 1. 取产物：上游任意节点输出里的 image/audio/video 路径（多个取第一个）
	path := pickArtifactPath(upstream)
	if path == "" {
		return nil, fmt.Errorf("质检门缺少输入产物：连一条来自上游的图片/音频/视频线")
	}

	info, err := probeGateMedia(path)
	if err != nil {
		// 不可达 ≠ 空结果：探测失败必须显式报，不能放行
		return nil, fmt.Errorf("质检探测失败（%s）：%w", path, err)
	}

	rules := parseGateRules(node)
	checks := runGateChecks(ctx, path, info, rules)
	annotateFixes(checks, info, rules)

	fails := make([]string, 0, len(checks))
	fixes := make([]string, 0, len(checks))
	for _, c := range checks {
		if !c.OK && c.Level == "fail" {
			detail := fmt.Sprintf("%s：%s", c.Name, c.Detail)
			if c.Fix != "" {
				detail += " → 修复：" + c.Fix
				fixes = append(fixes, fmt.Sprintf("%s → %s", c.Name, c.Fix))
			}
			fails = append(fails, detail)
		}
	}

	// 产物先建好，判不过也原样带出去。
	//
	// 这不是「失败也返回成功」：error 仍然返回，run 仍然 failed、下游仍然 skipped。
	// 带 checks 出去的**唯一**目的，是让证据层能区分「测了不合格」与「没测到」——
	// 判据就是有没有 checks（evidence.go gateFacts）。返回 nil 会让一次真正的
	// 质量不合格被记成 unscored，自进化看到的就不是「质量在漏」而是「门在崩」。
	out := map[string]interface{}{
		"gate":    map[bool]string{true: "pass", false: "fail"}[len(fails) == 0],
		"checks":  checks,
		"fixes":   fixes,
		"path":    path,
		"kind":    info.kind,
		"width":   info.width,
		"height":  info.height,
		"frames":  info.frames,
		"seconds": info.seconds,
		"text":    gateSummary(checks),
	}
	if len(fails) > 0 {
		out["failures"] = fails
		// 阻断：run 记 failed，下游级联 skipped
		return out, fmt.Errorf("质检未通过（%s）：%s", path, strings.Join(fails, "；"))
	}

	return out, nil
}

// annotateFixes 给每一项「未通过」填可执行修复建议（通过的项不填，避免噪音）。
//
// 建议必须落到具体动作：改哪个字段、改成多少、或给一条能复制的命令。
// 「请检查参数」这类正确但无用的话一律不写——那只是把排查成本原样退回去。
func annotateFixes(checks []gateCheck, info *gateMediaInfo, r gateRules) {
	for i := range checks {
		c := &checks[i]
		if c.OK {
			continue
		}
		switch c.Name {
		case "has_video_stream":
			c.Fix = "合成节点必须显式映射视频流：ffmpeg ... -map 0:v:0 -map 0:a:0；少了 -map 0:v:0 就会得到纯音频 mp4"
		case "has_audio_stream":
			c.Fix = "合成时加 -map 0:a:0，或先用 TTS 节点配音再合成；同时确认音源路径真实存在"
		case "min_width":
			c.Fix = fmt.Sprintf("把上游生成节点的 width 从 %d 提到 ≥%d（注意显存：宽度每 +25%% 显存约 +30%%）",
				info.width, r.minWidth)
		case "min_height":
			c.Fix = fmt.Sprintf("把上游生成节点的 height 从 %d 提到 ≥%d", info.height, r.minHeight)
		case "min_frames":
			need := int(math.Ceil(float64(r.minFrames)))
			c.Fix = fmt.Sprintf("把文生视频节点的 frames 从 %d 提到 ≥%d（24fps 下 帧数 ≈ 秒数×24）",
				info.frames, need)
		case "duration_min":
			c.Fix = fmt.Sprintf("时长 %.2fs 短于下限 %.2fs：把 frames 提到 ≥%d（24fps）",
				info.seconds, r.durationMin, int(math.Ceil(r.durationMin*24)))
		case "duration_max":
			c.Fix = fmt.Sprintf("时长 %.2fs 超过上限 %.2fs：把 frames 降到 ≤%d（24fps），或用剪辑节点裁掉尾部",
				info.seconds, r.durationMax, int(math.Floor(r.durationMax*24)))
		case "av_drift":
			c.Fix = fmt.Sprintf("音画差 %.3fs：给合成命令加 -shortest，或把音轨裁到与视频同长",
				math.Abs(info.videoSecond-info.audioSecond))
		case "silence_gap":
			c.Fix = fmt.Sprintf("最长静音超过 %.2fs：删掉 TTS 里的空段/换行，或补一段环境音填满", r.maxSilenceGap)
		case "forbid_speech":
			c.Fix = "氛围模式不允许人声：把该段换成纯环境音轨，或去掉 TTS 节点只保留 BGM"
		case "loudness_min":
			c.Fix = fmt.Sprintf("整体响度低于 %.1fdB：ffmpeg -i 输入 -af volume=+%.0fdB 输出（先小步试，别削波）",
				r.loudnessMinDB, math.Min(12, math.Abs(r.loudnessMinDB-(-16))))
		}
	}
}

// gateRules 声明式质检规则（全部可选，未配不检）
type gateRules struct {
	minWidth      int
	minHeight     int
	minFrames     int
	durationMin   float64
	durationMax   float64
	durationTol   float64
	requireAudio  bool
	loudnessMinDB float64
	maxSilenceGap float64
	forbidSpeech  bool
	speechMaxSecs float64
}

func parseGateRules(node *model.NodeInstance) gateRules {
	r := gateRules{}
	getF := func(key string, dst *float64) {
		if v, ok := node.Data[key].(float64); ok && v > 0 {
			*dst = v
		}
	}
	getI := func(key string, dst *int) {
		if v, ok := node.Data[key].(float64); ok && v > 0 {
			*dst = int(v)
		}
	}
	if v, ok := node.Data["require_audio"].(bool); ok {
		r.requireAudio = v
	}
	if v, ok := node.Data["forbid_speech"].(bool); ok {
		r.forbidSpeech = v
	}
	getI("min_width", &r.minWidth)
	getI("min_height", &r.minHeight)
	getI("min_frames", &r.minFrames)
	getF("duration_min", &r.durationMin)
	getF("duration_max", &r.durationMax)
	getF("duration_tolerance", &r.durationTol)
	// loudness_min_db 是 **dB，天然为负**（-20 比 -14 安静）。上面 getF 的
	// `v > 0` 会把一切真实阈值静默丢弃——门看起来配了，其实永远不查。
	// 单独走有符号解析：0 仍是「没配」，负数照收。
	if v, ok := node.Data["loudness_min_db"].(float64); ok && v != 0 {
		r.loudnessMinDB = v
	}
	getF("max_silence_gap", &r.maxSilenceGap)
	getF("speech_max_seconds", &r.speechMaxSecs)
	if r.durationTol == 0 {
		r.durationTol = 0.15 // 默认 150ms（音画时长差容忍）
	}
	if r.speechMaxSecs == 0 {
		r.speechMaxSecs = 1.5 // 默认：连续非静音 >1.5s 视为「有人声」
	}
	return r
}

// gateMediaInfo 质检用媒体信息（inspectMedia + probeMedia 组合）
type gateMediaInfo struct {
	kind        string // image / audio / video
	width       int
	height      int
	frames      int
	seconds     float64
	videoSecond float64
	audioSecond float64
	hasVideo    bool
	hasAudio    bool
	fps         float64
}

// probeGateMedia 组合现有探测：inspectMedia（精确帧数/尺寸/音画差）+
// probeMedia（ffmpeg -i 快速判音轨与时长，兜底 inspectMedia 失败场景）
func probeGateMedia(path string) (*gateMediaInfo, error) {
	info := &gateMediaInfo{kind: "image"}
	chk, cerr := inspectMedia(path)
	if cerr != nil {
		return nil, cerr
	}
	if chk.Size != "" {
		info.hasVideo = true
		info.kind = "video"
		info.width, info.height = parseSize(chk.Size)
	}
	info.frames = chk.Frames
	info.fps = chk.FPS
	info.videoSecond = chk.VSeconds
	info.audioSecond = chk.ASeconds
	info.hasAudio = chk.HasAudio
	if info.hasAudio && !info.hasVideo {
		info.kind = "audio"
	}
	info.seconds = info.videoSecond
	if info.audioSecond > info.seconds {
		info.seconds = info.audioSecond
	}
	// inspectMedia 在 ffprobe 缺失时静默降级（Note 标记），此时用 probeMedia 兜底
	if chk.Size == "" && chk.Note != "" {
		if pm, perr := probeMedia(path); perr == nil {
			info.hasAudio = pm.hasAudio
			info.seconds = pm.dur
			if info.hasAudio && !info.hasVideo {
				info.kind = "audio"
			}
		}
	}
	// 静图（PNG/JPEG 等单帧产物）天生没有时长：ffprobe 报 0s 是正常，不是损坏。
	// 2026-10-23 真渲闭环第一次抓到这个：wf 56/57/58 三个静图流的门配的是
	// min_width/min_height（静图本来就量尺寸），却在探测阶段被这句「探测不到
	// 时长」硬错弹回——尺寸检查在崩溃后面永远执行不到，门配了等于没配。
	// 现在只有「什么都读不到」才判损坏：无尺寸、无音轨、无时长。那种情况若放行
	// 就是假通过（空规则 + 坏文件 → 零检查过门），比崩溃更糟。
	if info.seconds == 0 && info.width == 0 && info.height == 0 && !info.hasAudio {
		return nil, fmt.Errorf("探测不到时长（文件可能损坏或格式不支持）")
	}
	return info, nil
}

func parseSize(s string) (int, int) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	return w, h
}

// pickArtifactPath 从上游输出里找产物路径
func pickArtifactPath(upstream map[string]interface{}) string {
	for _, key := range []string{"video", "audio", "image"} {
		for _, out := range upstream {
			m, ok := out.(map[string]interface{})
			if !ok {
				continue
			}
			if p, ok := m[key].(string); ok && strings.TrimSpace(p) != "" {
				return strings.TrimSpace(p)
			}
		}
	}
	return ""
}

// runGateChecks 按规则跑检查（流列表 / 尺寸 / 帧数 / 时长 / 音轨 / 音画同步 / 响度 / 静音）
func runGateChecks(ctx context.Context, path string, info *gateMediaInfo, r gateRules) []gateCheck {
	checks := make([]gateCheck, 0, 8)
	add := func(name string, ok bool, level, format string, args ...interface{}) {
		checks = append(checks, gateCheck{Name: name, OK: ok, Level: level, Detail: fmt.Sprintf(format, args...)})
	}

	// 流列表（verify-media 的头号 FAIL：-map 漏写导致纯音频 mp4）
	if info.kind == "video" {
		add("has_video_stream", info.hasVideo, "fail", "视频流存在（%d×%d）", info.width, info.height)
	}
	if r.requireAudio {
		add("has_audio_stream", info.hasAudio, "fail", "音频流存在（时长 %.2fs）", info.audioSecond)
	}

	// 尺寸下限（图片/视频）
	if info.kind != "audio" {
		if r.minWidth > 0 {
			add("min_width", info.width >= r.minWidth, "fail", "宽 %d ≥ %d", info.width, r.minWidth)
		}
		if r.minHeight > 0 {
			add("min_height", info.height >= r.minHeight, "fail", "高 %d ≥ %d", info.height, r.minHeight)
		}
	}

	// 帧数下限（视频）：inspectMedia 已用 -count_frames 精确计数
	if info.kind == "video" && r.minFrames > 0 {
		add("min_frames", info.frames >= r.minFrames, "fail", "帧数 %d ≥ %d", info.frames, r.minFrames)
	}

	// 时长区间
	if r.durationMin > 0 {
		add("duration_min", info.seconds >= r.durationMin, "fail", "时长 %.2fs ≥ %.2fs", info.seconds, r.durationMin)
	}
	if r.durationMax > 0 {
		add("duration_max", info.seconds <= r.durationMax, "fail", "时长 %.2fs ≤ %.2fs", info.seconds, r.durationMax)
	}

	// 音画时长差（容器 sync 之外用户真能感知的项）
	if info.hasAudio && info.hasVideo {
		drift := math.Abs(info.videoSecond - info.audioSecond)
		add("av_drift", drift <= r.durationTol, "fail", "音画时长差 %.3fs ≤ %.3fs", drift, r.durationTol)
	}

	// 响度 / 静音（仅视频/音频）
	if info.kind != "image" && (r.loudnessMinDB != 0 || r.maxSilenceGap > 0 || r.forbidSpeech) {
		loud, silences := probeLoudnessAndSilence(path)
		if r.loudnessMinDB != 0 {
			add("loudness_min", loud.mean >= r.loudnessMinDB, "warn", "均值 %.1fdB（门限 %.1fdB）", loud.mean, r.loudnessMinDB)
		}
		if r.maxSilenceGap > 0 {
			worst := 0.0
			for _, s := range silences {
				if s[1]-s[0] > worst {
					worst = s[1] - s[0]
				}
			}
			add("silence_gap", worst <= r.maxSilenceGap, "fail", "最长静音 %.2fs ≤ %.2fs", worst, r.maxSilenceGap)
		}
		if r.forbidSpeech {
			// 能量法判「有人声」：连续非静音超过 speechMaxSecs 即判 FAIL
			// （ASR 级判读需要 asr 服务，能耗太大；能量法是够用的门槛）
			worst := longestLoudRun(silences, info.seconds)
			add("forbid_speech", worst <= r.speechMaxSecs, "fail",
				"最长连续非静音 %.2fs ≤ %.2fs（氛围模式：疑似有人声）", worst, r.speechMaxSecs)
		}
	}
	return checks
}

// loudnessResult 响度探测结果
type loudnessResult struct {
	mean float64
	max  float64
}

// probeLoudnessAndSilence 一次 ffmpeg 同时拿 volumedetect + silencedetect
func probeLoudnessAndSilence(path string) (loudnessResult, [][2]float64) {
	bin := ffmpegBin()
	if bin == "" {
		return loudnessResult{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin,
		"-hide_banner", "-nostats", "-i", path,
		"-af", "volumedetect,silencedetect=noise=-35dB:d=0.4",
		"-f", "null", "-").CombinedOutput()
	text := string(out)
	lr := loudnessResult{}
	silences := make([][2]float64, 0, 4)
	var silenceStart float64
	inSilence := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "mean_volume:"):
			lr.mean = parseDB(line)
		case strings.Contains(line, "max_volume:"):
			lr.max = parseDB(line)
		case strings.Contains(line, "silence_start:"):
			silenceStart = parseSilenceField(line)
			inSilence = true
		case strings.Contains(line, "silence_end:"):
			if inSilence {
				silences = append(silences, [2]float64{silenceStart, parseSilenceField(line)})
				inSilence = false
			}
		}
	}
	// 结尾仍静音（没有 silence_end）时按探测到的 start 记账
	if inSilence {
		silences = append(silences, [2]float64{silenceStart, silenceStart})
	}
	return lr, silences
}

func parseDB(line string) float64 {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return -999
	}
	field := strings.Fields(strings.TrimSpace(line[idx+1:]))
	if len(field) == 0 {
		return -999
	}
	return parseFloatStr(strings.TrimSuffix(field[0], "dB"))
}

func parseSilenceField(line string) float64 {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return 0
	}
	field := strings.Fields(strings.TrimSpace(line[idx+1:]))
	if len(field) == 0 {
		return 0
	}
	return parseFloatStr(field[0])
}

func parseFloatStr(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// longestLoudRun 由静音段反推最长连续非静音时长
func longestLoudRun(silences [][2]float64, total float64) float64 {
	if total <= 0 {
		return 0
	}
	sorted := append([][2]float64(nil), silences...)
	// 简单插入排序（段数极少）
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j][0] < sorted[j-1][0]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	worst, cursor := 0.0, 0.0
	for _, s := range sorted {
		if s[0] > cursor {
			if s[0]-cursor > worst {
				worst = s[0] - cursor
			}
		}
		if s[1] > cursor {
			cursor = s[1]
		}
	}
	if total-cursor > worst {
		worst = total - cursor
	}
	return worst
}

// gateSummary 汇总文本（供变量引用 {{gate-1.text}}）
func gateSummary(checks []gateCheck) string {
	pass, fail, warn := 0, 0, 0
	for _, c := range checks {
		switch {
		case !c.OK && c.Level == "fail":
			fail++
		case !c.OK:
			warn++
		default:
			pass++
		}
	}
	return fmt.Sprintf("质检 %d 项：通过 %d，失败 %d，警告 %d", len(checks), pass, fail, warn)
}
