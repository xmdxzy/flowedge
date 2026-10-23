package service

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// 五层控制门的运行期校验（Go 移植）
//
// 真相源是 scripts/film-gates.py 的 check_plan —— 这里是**逐条忠实移植**，
// 不是重写。为什么引擎里必须有一份：门如果只活在脚本侧，经引擎跑的工作流就
// 「声明承载五层门」（nodecontract 里 shotlist 的 GateLayers 写着五层）却在
// 运行期什么都不拦——契约说谎比没有契约更糟。
//
// 门的五个层（每层都是「规则 + 阈值 + 违约动作」）：
//
//	timing     节奏：总时长、网格吸附、每镜最短/最长停留、各段权重
//	identity   角色：定妆图数量、每段必须带参考图、prompt 里 <Picture N> 不得越界
//	audio      声音：台词段必须转出原话、氛围段必须无人声、响度区间、静音缺口
//	technical  技术：分辨率、帧率、音画漂移、接缝时长
//	budget     成本：总墙钟上限、单段上限、超了是告警还是拦截
//
// 合并顺序：DEFAULT < PROFILE < 本片 plan.gates（与 Python 侧一致）。
// plan 缺 gates 时按默认门执行并记 WARN（legacy 结构化分镜表没有 gates，
// 不能一棍子打死）；**gates 一旦声明，就必须完整自洽**——违约项 FAIL，
// 开机前拦截，不等跑完 20 分钟才发现。

const (
	gateFPS         = 24
	gateGridStep    = 17
	gateGridOffset  = 5
	gateDefaultFade = 0.2
	// 单帧出片成本的保守估计（秒/帧，608×352 / 12 步 / 2 参考图实测 0.85–0.93）
	gateSecPerFrame = 0.95
)

// DefaultFilmGates 默认门：只写「通用规则」，不写「这类片子该多少秒」。
// 每个字段都可以被 plan.gates 覆盖——这正是「每次任务前先设计门」的入口。
// 每次调用返回全新 map（调用方可能合并/改写，不许共享底层引用）。
func DefaultFilmGates() map[string]interface{} {
	return map[string]interface{}{
		"version": float64(1),
		"profile": "default",
		"timing": map[string]interface{}{
			"fps":               float64(gateFPS),
			"grid":              []interface{}{float64(gateGridOffset), float64(gateGridStep)},
			"target_seconds":    float64(0), // 0 = 不强制总长
			"tolerance_seconds": float64(0.75),
			"min_shot_seconds":  float64(1.2), // 单镜最短停留：低于这个数观众读不到信息
			"max_shot_seconds":  float64(0),   // 0 = 不限制
			"beat_weights":      map[string]interface{}{},
			"require_beats":     true,
		},
		"identity": map[string]interface{}{
			"stills":                           float64(0), // 0 = 本片无重复角色
			"require_ref_images_every_segment": true,
			"max_ref_images":                   float64(9),
			"require_picture_tags":             true,
		},
		"audio": map[string]interface{}{
			"dialogue": map[string]interface{}{
				"must_transcribe": true,
				"min_chars":       float64(2),
			},
			"ambience": map[string]interface{}{
				"must_be_silent": true,
				"max_chars":      float64(0),
			},
			"loudness_db":     []interface{}{float64(-22), float64(-11)},
			"max_silence_gap": float64(1.0),
			// 段级失败策略：一段不合门时先重跑（采样有随机性），仍不过才放弃整片
			"segment_retry":   float64(1),
			"on_segment_fail": "retry", // retry | fail
		},
		"technical": map[string]interface{}{
			"min_width":       float64(608),
			"min_height":      float64(352),
			"must_have_audio": true,
			"av_drift_max":    float64(0.2),
			"fade":            float64(gateDefaultFade),
		},
		"budget": map[string]interface{}{
			"wall_seconds_max":     float64(0), // 0 = 不限制
			"per_segment_wall_max": float64(0),
			"on_exceed":            "warn", // warn | fail
		},
	}
}

// ---------------------------------------------------------------- 工具函数

func framesInGrid(n int) bool {
	return n > 0 && (n-gateGridOffset)%gateGridStep == 0
}

func segSeconds(frames int, fps int) float64 {
	if fps <= 0 {
		fps = gateFPS
	}
	return roundTo(float64(frames)/float64(fps), 3)
}

// totalSecondsGate 装配后总长：逐段相加，每处接缝吃掉 fade。
func totalSecondsGate(frames []int, fade float64, fps int) float64 {
	if len(frames) == 0 {
		return 0
	}
	if fps <= 0 {
		fps = gateFPS
	}
	n := len(frames) - 1
	if n < 0 {
		n = 0
	}
	return roundTo(float64(sumInt(frames))/float64(fps)-fade*float64(n), 2)
}

func sumInt(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

func roundTo(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// countShots 数「镜头起点」标记。
//
// 不能简单数 [Shot N] 出现次数：retention_analysis 里会写
// `<Picture 1> (appears in [Shot 1], [Shot 2])`，那是「出现于哪几镜」的引用，
// 不是新镜头。区别在于镜头起点后面跟的是内容，引用后面跟的是逗号或右括号。
//
// RE2 没有负向预查，Python 的 (?!\s*[,)]) 在这里是手工后瞻替身——
// 改这段前先跑 TestCountShotsSkipsReferences。
func countShots(prompt string) []int {
	locs := shotTagRe.FindAllStringSubmatchIndex(prompt, -1)
	out := make([]int, 0, len(locs))
	for _, m := range locs {
		j := m[1]
		for j < len(prompt) && isGateSpace(prompt[j]) {
			j++
		}
		if j < len(prompt) && (prompt[j] == ',' || prompt[j] == ')') {
			continue // 引用（后面跟逗号/右括号），不是镜头起点
		}
		n, err := strconv.Atoi(prompt[m[2]:m[3]])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

var shotTagRe = regexp.MustCompile(`(?i)\[\s*Shot\s+(\d+)\s*\]`)

func isGateSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// shotTimestamps 取 [Shot k] 之前声明的时间戳（形如 At 00:04.100）。
var shotStampRe = regexp.MustCompile(`(?i)At\s+(\d{1,2}):(\d{2})\.(\d{3})`)

func shotTimestamps(prompt string) []float64 {
	ms := shotStampRe.FindAllStringSubmatch(prompt, -1)
	out := make([]float64, 0, len(ms))
	for _, m := range ms {
		mm, _ := strconv.Atoi(m[1])
		ss, _ := strconv.Atoi(m[2])
		fff, _ := strconv.Atoi(m[3])
		out = append(out, float64(mm)*60+float64(ss)+float64(fff)/1000.0)
	}
	return out
}

var pictureTagRe = regexp.MustCompile(`(?i)<\s*Picture\s+(\d+)\s*>`)

func pictureTags(prompt string) []int {
	ms := pictureTagRe.FindAllStringSubmatch(prompt, -1)
	out := make([]int, 0, len(ms))
	for _, m := range ms {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

var dialogueBlockRe = regexp.MustCompile(`(?is)<d>(.*?)</d>`)

func dialogueBlocks(prompt string) []string {
	ms := dialogueBlockRe.FindAllStringSubmatch(prompt, -1)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// stripAllSpace 去掉全部空白（对标 Python re.sub(r"\s+", "", s)）：
// 台词核对必须忽略 TTS/提示词两边的空格差异，只比字。
func stripAllSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\u3000' {
			return -1
		}
		return r
	}, s)
}

// deepMergeGates 门的合并：任务级 gates 覆盖默认门，未声明的继承默认。
func deepMergeGates(base, override map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		if bv, ok := out[k]; ok {
			bm, bok := bv.(map[string]interface{})
			om, ook := v.(map[string]interface{})
			if bok && ook {
				out[k] = deepMergeGates(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------- 泛型取值（JSON 解码后的形态）

func gateNum(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func gateFloat(v interface{}, def float64) float64 {
	if f, ok := gateNum(v); ok {
		return f
	}
	return def
}

func gateInt(v interface{}, def int) int {
	if f, ok := gateNum(v); ok {
		return int(f)
	}
	return def
}

func gateStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func gateBool(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	}
	return false
}

func gateMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func gateSlice(v interface{}) []interface{} {
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

// gateMapGet 逐层取嵌套 map 的标量值（g["timing"]["fps"] 的 Go 形态）。
// 任一层不是 map 或键不存在 → 返回 def，不 panic。
func gateMapGet(m map[string]interface{}, path ...string) interface{} {
	cur := m
	for i, k := range path {
		v, ok := cur[k]
		if !ok {
			return nil
		}
		if i == len(path)-1 {
			return v
		}
		next, ok := v.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = next
	}
	return nil
}

// ---------------------------------------------------------------- 判定

// GateItem 一条门检记录。OK 级是「过了」的显式记录（不是噪声）——
// 证据信封的判据是「门有没有留下 checks」，OK 项同样是 checks。
type GateItem struct {
	Level string `json:"level"` // OK | WARN | FAIL
	Gate  string `json:"gate"`
	Msg   string `json:"msg"`
}

// GateVerdict 门检结论。三态语义与 evidence.go 对齐：
// FAIL = 门跑完判不过；WARN = 能跑但要知道；OK = 显式通过。
type GateVerdict struct {
	Items []GateItem `json:"items"`
}

func (v *GateVerdict) Add(level, gate, msg string) {
	v.Items = append(v.Items, GateItem{Level: level, Gate: gate, Msg: msg})
}

// HasFail 有没有硬违约（调用方据此决定是否开机前拦截）。
func (v *GateVerdict) HasFail() bool {
	for _, it := range v.Items {
		if it.Level == "FAIL" {
			return true
		}
	}
	return false
}

// Worst 最重级别：FAIL > WARN > OK。
func (v *GateVerdict) Worst() string {
	w := "OK"
	for _, it := range v.Items {
		if it.Level == "FAIL" {
			return "FAIL"
		}
		if it.Level == "WARN" {
			w = "WARN"
		}
	}
	return w
}

// Fails 全部硬违约（错误信息里列给用户看）。
func (v *GateVerdict) Fails() []GateItem {
	out := []GateItem{}
	for _, it := range v.Items {
		if it.Level == "FAIL" {
			out = append(out, it)
		}
	}
	return out
}

// Warns 全部告警。
func (v *GateVerdict) Warns() []GateItem {
	out := []GateItem{}
	for _, it := range v.Items {
		if it.Level == "WARN" {
			out = append(out, it)
		}
	}
	return out
}

// GateStats 门检统计（stats 摘要，随 gate_report 落输出）。
type GateStats struct {
	Frames       []int   `json:"frames"`
	TotalSeconds float64 `json:"total_seconds"` // 0 = 有段未声明 frames，算不出
	Shots        []int   `json:"shots"`         // 每段镜头起点数
	Lines        int     `json:"lines"`         // 台词总句数
	Stills       int     `json:"stills"`        // 定妆图张数
}

// CheckFilmGates 静态自洽校验：能算的错，开机前就拦掉。
//
// plan 是**归一化后的分镜表 map**（segments[] 带 id/frames/prompt/lines，
// 顶层 characters/width/height/gates）。requireGates=false 时缺 gates 只记
// WARN（legacy 结构化分镜表没有 gates，不能一棍子打死）；true 时缺 gates 记 FAIL。
//
// 返回：判定记录、合并后的门（DEFAULT < 本片覆盖）、统计。
func CheckFilmGates(plan map[string]interface{}, requireGates bool) (*GateVerdict, map[string]interface{}, GateStats) {
	v := &GateVerdict{}
	rawGates := gateMap(plan["gates"])

	if len(rawGates) == 0 {
		if requireGates {
			v.Add("FAIL", "gates_missing",
				"plan 没有 gates：本片子的节奏/一致性/验收标准还没设计。"+
					"先按 film-gates.py plan 生成草案并改写")
		} else {
			v.Add("WARN", "gates_missing", "未声明 gates，全部按默认门执行")
		}
		rawGates = map[string]interface{}{}
	}

	g := deepMergeGates(DefaultFilmGates(), rawGates)
	fps := gateInt(gateMapGet(g, "timing", "fps"), gateFPS)
	if fps <= 0 {
		fps = gateFPS
	}
	fade := gateFloat(gateMapGet(g, "technical", "fade"), gateDefaultFade)

	segments := gateSlice(plan["segments"])
	if len(segments) == 0 {
		v.Add("FAIL", "segments_empty", "plan.segments 为空")
		return v, g, GateStats{}
	}

	// ---------- timing：网格 / 每镜停留 / 总长 ----------
	frames := make([]int, 0, len(segments))
	allFrames := true
	for i, segv := range segments {
		seg := gateMap(segv)
		sid := segID(seg, i)
		fv := seg["frames"]
		if fv == nil {
			v.Add("WARN", "frames_missing", fmt.Sprintf("seg%d 未声明 frames，将由权重推导", sid))
			frames = append(frames, 0)
			allFrames = false
			continue
		}
		f := gateInt(fv, 0)
		if !framesInGrid(f) {
			v.Add("FAIL", "grid", fmt.Sprintf("seg%d frames=%d 不在 17k+5 网格", sid, f))
		}
		frames = append(frames, f)

		secs := segSeconds(f, fps)
		prompt := gateStr(seg["prompt"])
		shots := countShots(prompt)
		minShot := gateFloat(gateMapGet(g, "timing", "min_shot_seconds"), 0)
		maxShot := gateFloat(gateMapGet(g, "timing", "max_shot_seconds"), 0)

		if len(shots) > 0 {
			stamps := shotTimestamps(prompt)
			// 第 k 镜的起点：第 1 镜是 0，其余取声明的时间戳
			starts := append([]float64{0.0}, stamps...)
			if len(starts) > len(shots) {
				starts = starts[:len(shots)]
			}
			if len(stamps) >= len(shots)-1 {
				for k := 0; k < len(shots); k++ {
					end := secs
					if k+1 < len(starts) {
						end = starts[k+1]
					}
					span := roundTo(end-starts[k], 3)
					if span < minShot {
						v.Add("FAIL", "shot_too_short",
							fmt.Sprintf("seg%d 第 %d 镜只有 %.3fs（门要求 ≥%.1fs）——切点要前移或减少镜头数",
								sid, k+1, span, minShot))
					}
					if maxShot > 0 && span > maxShot {
						v.Add("WARN", "shot_too_long",
							fmt.Sprintf("seg%d 第 %d 镜 %.3fs 超过 %.1fs", sid, k+1, span, maxShot))
					}
				}
				if len(stamps) > 0 {
					sorted := true
					for k := 1; k < len(stamps); k++ {
						if stamps[k] < stamps[k-1] {
							sorted = false
							break
						}
					}
					if !sorted {
						v.Add("FAIL", "shot_order", fmt.Sprintf("seg%d 镜头时间戳不递增", sid))
					}
					last := stamps[len(stamps)-1]
					if last >= secs-minShot {
						v.Add("FAIL", "shot_tail",
							fmt.Sprintf("seg%d 最后一个切点 %.3fs 距片尾 %.3fs 不足 %.1fs", sid, last, secs, minShot))
					}
				}
			} else {
				v.Add("WARN", "shot_stamps",
					fmt.Sprintf("seg%d 有 %d 个镜头但只声明了 %d 个时间戳，无法校验每镜停留时长",
						sid, len(shots), len(stamps)))
			}
		} else if gateBool(gateMapGet(g, "timing", "require_beats")) {
			v.Add("WARN", "no_shot_tags", fmt.Sprintf("seg%d 提示词里没有 [Shot N] 标记，镜内节奏不可控", sid))
		}
	}

	var stats GateStats
	stats.Frames = frames
	if allFrames && len(frames) > 0 {
		stats.TotalSeconds = totalSecondsGate(frames, fade, fps)
		got := stats.TotalSeconds
		target := gateFloat(gateMapGet(g, "timing", "target_seconds"), 0)
		tol := gateFloat(gateMapGet(g, "timing", "tolerance_seconds"), 0.75)
		if target > 0 && math.Abs(got-target) > tol {
			v.Add("FAIL", "total_seconds",
				fmt.Sprintf("装配后 %.2fs 与目标 %.1fs 相差超过容差 %.2fs（段帧数 %v）",
					got, target, tol, frames))
		} else if target > 0 {
			v.Add("OK", "total_seconds", fmt.Sprintf("%.2fs ↔ 目标 %.1fs（容差 %.2fs）", got, target, tol))
		}
	}

	// ---------- identity：定妆图 / Picture 标签 ----------
	chars := gateSlice(plan["characters"])
	need := gateInt(gateMapGet(g, "identity", "stills"), 0)
	if need > 0 && len(chars) < need {
		v.Add("FAIL", "stills", fmt.Sprintf("门要求 %d 张定妆图，plan 只给了 %d 张", need, len(chars)))
	}
	maxRef := gateInt(gateMapGet(g, "identity", "max_ref_images"), 9)
	if len(chars) > maxRef {
		v.Add("FAIL", "ref_images", fmt.Sprintf("参考图 %d 张超过上限 %d", len(chars), maxRef))
	}

	for i, segv := range segments {
		seg := gateMap(segv)
		sid := segID(seg, i)
		prompt := gateStr(seg["prompt"])
		tags := pictureTags(prompt)
		if len(tags) > 0 {
			over := -1
			for _, t := range tags {
				if t > len(chars) && t > over {
					over = t
				}
			}
			if over > 0 {
				v.Add("FAIL", "picture_overflow",
					fmt.Sprintf("seg%d 引用了 <Picture %d> 但只有 %d 张定妆图", sid, over, len(chars)))
			}
		} else if len(chars) > 0 && gateBool(gateMapGet(g, "identity", "require_picture_tags")) {
			v.Add("WARN", "picture_tags",
				fmt.Sprintf("seg%d 有定妆图却没用 <Picture N> 指认角色，一致性只能靠文字描述", sid))
		}

		if len(chars) > 0 && gateBool(gateMapGet(g, "identity", "require_ref_images_every_segment")) {
			// 只有显式写了 "ref_images": false 才算关（JSON 缺省/ null 都不算）
			if rv, ok := seg["ref_images"]; ok && rv != nil && !gateBool(rv) {
				v.Add("FAIL", "ref_images_segment",
					fmt.Sprintf("seg%d 显式关闭了参考图：这一段会换脸", sid))
			}
		}
	}

	// ---------- audio：台词表 ↔ 提示词 ----------
	totalLines := 0
	for i, segv := range segments {
		seg := gateMap(segv)
		sid := segID(seg, i)
		lines := nonEmptyLines(seg["lines"])
		totalLines += len(lines)
		blocks := dialogueBlocks(gateStr(seg["prompt"]))

		if len(lines) > 0 {
			if len(blocks) == 0 {
				v.Add("FAIL", "dialogue_not_in_prompt",
					fmt.Sprintf("seg%d 有 %d 句台词，但提示词里没有 <d>…</d> 标签，H3 不会照念", sid, len(lines)))
			}
			for _, ln := range lines {
				text := stripAllSpace(gateStr(ln["text"]))
				if text == "" {
					continue
				}
				hit := false
				for _, b := range blocks {
					if strings.Contains(stripAllSpace(b), text) {
						hit = true
						break
					}
				}
				if !hit {
					// 这是硬错误：TTS 念的是台词表，提示词里写的是另一句，
					// 成片一定念错。必须在开机前拦，不能等交付时才发现。
					v.Add("FAIL", "line_mismatch",
						fmt.Sprintf("seg%d 台词「%s」没出现在 <d> 块里：TTS 会念台词表，画面按提示词走，两者对不上",
							sid, gateStr(ln["text"])))
				}
			}
			if gateBool(gateMapGet(g, "audio", "dialogue", "must_transcribe")) {
				v.Add("OK", "dialogue_policy", fmt.Sprintf("seg%d 台词段：出片后必须 ASR 转出原话", sid))
			}
		} else {
			if gateBool(gateMapGet(g, "audio", "ambience", "must_be_silent")) {
				v.Add("OK", "ambience_policy", fmt.Sprintf("seg%d 氛围段：出片后必须 ASR 无人声", sid))
			}
		}
	}
	stats.Lines = totalLines

	if len(chars) > 0 && totalLines > 0 {
		anyPic := false
		for _, segv := range segments {
			if len(pictureTags(gateStr(gateMap(segv)["prompt"]))) > 0 {
				anyPic = true
				break
			}
		}
		if !anyPic {
			v.Add("WARN", "no_picture_with_dialogue", "有台词又有定妆图，却没有 <Picture N> 指认")
		}
	}

	// ---------- technical ----------
	w := gateInt(plan["width"], 608)
	h := gateInt(plan["height"], 352)
	if w < gateInt(gateMapGet(g, "technical", "min_width"), 0) ||
		h < gateInt(gateMapGet(g, "technical", "min_height"), 0) {
		v.Add("WARN", "resolution",
			fmt.Sprintf("%d×%d 低于门要求 %d×%d", w, h,
				gateInt(gateMapGet(g, "technical", "min_width"), 0),
				gateInt(gateMapGet(g, "technical", "min_height"), 0)))
	}
	if gateBool(gateMapGet(g, "technical", "must_have_audio")) {
		for i, segv := range segments {
			seg := gateMap(segv)
			if rv, ok := seg["mute"]; ok && rv != nil && gateBool(rv) {
				v.Add("FAIL", "audio_mute", fmt.Sprintf("seg%d 被设为静音", segID(seg, i)))
			}
		}
	}

	// ---------- budget ----------
	if allFrames && len(frames) > 0 {
		est := roundTo(float64(sumInt(frames))*gateSecPerFrame+50*float64(len(frames)), 0)
		cap := gateInt(gateMapGet(g, "budget", "wall_seconds_max"), 0)
		if cap > 0 && est > float64(cap) {
			level := "WARN"
			if gateStr(gateMapGet(g, "budget", "on_exceed")) == "fail" {
				level = "FAIL"
			}
			v.Add(level, "budget",
				fmt.Sprintf("预计 %.0fs 超过预算 %ds（%d 帧）；要么加预算，要么压帧数/分段数",
					est, cap, sumInt(frames)))
		}
	}

	for _, segv := range segments {
		stats.Shots = append(stats.Shots, len(countShots(gateStr(gateMap(segv)["prompt"]))))
	}
	stats.Stills = len(chars)
	return v, g, stats
}

// segID 段号：seg.id 优先，缺省用 1-based 下标（对标 Python 的 seg.get("id") or (i+1)）。
// 注意 Python 的 or 对 0/空串也生效，Go 里显式判零值/空。
func segID(seg map[string]interface{}, i int) int {
	if seg == nil {
		return i + 1
	}
	if n, ok := gateNum(seg["id"]); ok && n != 0 {
		return int(n)
	}
	return i + 1
}

// nonEmptyLines 过滤掉无文本的台词条目（对标 Python 的 lines 过滤）。
func nonEmptyLines(v interface{}) []map[string]interface{} {
	raw := gateSlice(v)
	out := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		m := gateMap(item)
		if m == nil {
			continue
		}
		if strings.TrimSpace(gateStr(m["text"])) != "" {
			out = append(out, m)
		}
	}
	return out
}
