package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"flowedge/backend/model"
)

// 质量循环（Quality Loop）—— FlowEdge 的第二件核心能力。
//
// 以前「生成」和「验收」是两张图上的两件事：质检门报 FAIL，人看着修复建议
// 去改参数再点一次运行。能跑，但每一次提升都要人当调度器。
//
// 这里把它闭成一个节点：
//
//	生成 → 验收（打分）→ 诊断（哪项没过）→ 修复（改哪个字段改成多少）
//	→ 重跑 → 择优（保留得分最高的那一轮）
//
// 三条纪律（与 quality_gate 一致）：
//   - 不可达 ≠ 不合格：探测失败要显式记成 error，不能算成「这轮 0 分」；
//   - 每轮都留证据：checks / score / 应用了哪些补丁，全部进报告；
//   - 有界：max_rounds 硬上限，修不动就停，把最好的一轮交出来而不是死循环。

// repairPatch 一条机器可执行的修复动作：改哪个节点的哪个字段、改成什么。
type repairPatch struct {
	Node string      `json:"node"`
	Key  string      `json:"key"`
	To   interface{} `json:"to"`
	Why  string      `json:"why"`
}

// qualityRound 一轮的完整证据
type qualityRound struct {
	Round   int           `json:"round"`
	Score   int           `json:"score"`
	Path    string        `json:"path"`
	Checks  []gateCheck   `json:"checks"`
	Fixes   []string      `json:"fixes"`
	Patches []repairPatch `json:"patches,omitempty"`
	Error   string        `json:"error,omitempty"`
	Seconds float64       `json:"seconds"`
}

// execQualityLoopNode 生成 → 验收 → 修复 → 重跑 → 择优。
func (e *EngineV2) execQualityLoopNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if e.resolveWorkflow == nil {
		return nil, fmt.Errorf("质量循环缺少工作流解析器（引擎未装配 DB）")
	}

	wfID := uint(dataInt(node.Data, "workflow_id", 0))
	if wfID == 0 {
		return nil, fmt.Errorf("质量循环缺少 workflow_id：它要循环的是哪一条生成工作流？")
	}

	maxRounds := dataInt(node.Data, "max_rounds", 3)
	if maxRounds <= 0 {
		maxRounds = 3
	}
	if maxRounds > maxLoopIterations {
		maxRounds = maxLoopIterations
	}
	targetScore := dataInt(node.Data, "target_score", 100)
	if targetScore <= 0 || targetScore > 100 {
		targetScore = 100
	}
	repairMode, _ := node.Data["repair"].(string)
	if repairMode == "" {
		repairMode = "auto" // auto / resample / off
	}
	targetNodeOverride, _ := node.Data["target_node"].(string)
	rules := parseGateRules(node) // 与 quality_gate 同一套阈值键

	// 循环嵌套深度上限：质量循环里再套质量循环是合法的，但不能无限套。
	if d := e.depth.Add(1); d > maxSubWorkflowDepth {
		e.depth.Add(-1)
		return nil, fmt.Errorf("质量循环嵌套过深（>%d）", maxSubWorkflowDepth)
	}
	defer e.depth.Add(-1)

	seedText := upstreamText(upstream)
	if v, ok := node.Data["input"].(string); ok && v != "" {
		seedText = v
	}

	var rounds []qualityRound
	var best *qualityRound
	var patches []repairPatch // 上一轮诊断出的补丁，本轮生效

	for r := 1; r <= maxRounds; r++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("已取消（完成 %d/%d 轮）: %w", len(rounds), maxRounds, err)
		}
		started := time.Now()
		e.emit(runIDFrom(ctx), Event{
			Type: "quality_round", NodeID: node.ID,
			Data: map[string]interface{}{"round": r, "max_rounds": maxRounds, "phase": "generate", "patches": patches},
		})

		path, err := e.runInnerWorkflowPatched(ctx, wfID, seedText, patches, targetNodeOverride)
		elapsed := time.Since(started).Seconds()
		if err != nil {
			// 生成失败不是「质量不合格」，单独记；下一轮照样可以再试（换一次采样）。
			rounds = append(rounds, qualityRound{Round: r, Score: 0, Error: err.Error(), Seconds: elapsed, Patches: patches})
			log.Printf("[quality-loop] 第 %d 轮生成失败：%v", r, err)
			patches = nil // 生成失败时参数补丁未必是原因，下一轮先原样重采样
			continue
		}

		info, perr := probeGateMedia(path)
		if perr != nil {
			// 不可达 ≠ 空结果：这一轮记为「没测到」，不能算 0 分通过/不通过。
			rounds = append(rounds, qualityRound{Round: r, Path: path, Error: "探测失败：" + perr.Error(), Seconds: elapsed, Patches: patches})
			return nil, fmt.Errorf("质量循环第 %d 轮产物无法探测（%s）：%w", r, path, perr)
		}

		checks := runGateChecks(ctx, path, info, rules)
		annotateFixes(checks, info, rules)
		score, fails, fixes := scoreChecks(checks)

		rounds = append(rounds, qualityRound{Round: r, Score: score, Path: path, Checks: checks, Fixes: fixes, Patches: patches, Seconds: elapsed})
		e.emit(runIDFrom(ctx), Event{
			Type: "quality_round", NodeID: node.ID,
			Data: map[string]interface{}{"round": r, "score": score, "gate": verdictOf(fails), "path": path, "failures": fails},
		})

		if best == nil || score > best.Score {
			best = &rounds[len(rounds)-1]
		}
		if score >= targetScore {
			break
		}
		if r == maxRounds || repairMode == "off" {
			break
		}

		// 诊断 → 修复：把失败项翻译成「改哪个字段改成多少」。
		next := derivePatches(checks, rules, targetNodeOverride)
		if repairMode == "resample" || len(next) == 0 {
			// 没有可改的参数就是纯重采样：留空补丁，下一轮按原参数再跑一次。
			next = nil
			if repairMode != "resample" {
				log.Printf("[quality-loop] 第 %d 轮无参数级修复项，按重采样继续", r)
			}
		}
		patches = next
	}

	if best == nil {
		return nil, qualityLoopError(rounds)
	}

	// 择优：把最好那一轮的产物按 kind 暴露给下游（下游只认 image/video/audio 这些键）。
	out := map[string]interface{}{
		"gate":         map[bool]string{true: "pass", false: "fail"}[best.Score >= targetScore],
		"score":        best.Score,
		"target_score": targetScore,
		"rounds":       len(rounds),
		"best_round":   best.Round,
		"path":         best.Path,
		"checks":       best.Checks,
		"report":       rounds,
		"text":         qualityLoopSummary(rounds, best, targetScore),
	}
	if info, err := probeGateMedia(best.Path); err == nil {
		switch info.kind {
		case "video":
			out["video"] = best.Path
		case "audio":
			out["audio"] = best.Path
		default:
			out["image"] = best.Path
		}
	} else {
		out["video"] = best.Path // 探测不到就按路径给出去，下游仍能引用
	}

	// 没达标不报 success：下游看到的是失败 + 完整报告（「跑了但没达标」必须显式）。
	if best.Score < targetScore {
		fails := make([]string, 0, 4)
		for _, c := range best.Checks {
			if !c.OK && c.Level == "fail" {
				fails = append(fails, fmt.Sprintf("%s：%s", c.Name, c.Detail))
			}
		}
		return out, fmt.Errorf("质量循环 %d 轮未达标（最好第 %d 轮 %d 分 < 目标 %d）：%s",
			len(rounds), best.Round, best.Score, targetScore, strings.Join(fails, "；"))
	}
	return out, nil
}

// runInnerWorkflowPatched 跑一次内层生成工作流，可附带节点参数补丁。
func (e *EngineV2) runInnerWorkflowPatched(ctx context.Context, wfID uint, injectText string, patches []repairPatch, targetOverride string) (string, error) {
	nodesJSON, edgesJSON, err := e.resolveWorkflow(wfID)
	if err != nil {
		return "", fmt.Errorf("读取工作流 #%d 失败：%w", wfID, err)
	}
	var sub []model.NodeInstance
	if err := json.Unmarshal(nodesJSON, &sub); err != nil {
		return "", fmt.Errorf("解析工作流 #%d 失败：%w", wfID, err)
	}
	if injectText != "" && len(sub) > 0 && sub[0].Type == "start" {
		if cur, _ := sub[0].Data["prompt"].(string); cur == "" {
			sub[0].Data["prompt"] = injectText
		}
	}
	applyPatches(sub, patches, targetOverride)

	subJSON, err := json.Marshal(sub)
	if err != nil {
		return "", err
	}
	states, err := e.runWithContext(ctx, subJSON, edgesJSON, nil, "", nil)
	if err != nil {
		return "", err
	}
	path := pickArtifactPathFromStates(states)
	if path == "" {
		return "", fmt.Errorf("工作流 #%d 跑完了但没有产物（检查是否连了 text2image/text2video/tts 这类生成节点）", wfID)
	}
	return path, nil
}

// applyPatches 把补丁写进节点 data；targetOverride 为空时自动挑「生成节点」。
func applyPatches(nodes []model.NodeInstance, patches []repairPatch, targetOverride string) {
	if len(patches) == 0 {
		return
	}
	for _, p := range patches {
		id := p.Node
		if id == "" {
			id = targetOverride
		}
		if id == "" {
			id = autoTargetNode(nodes)
		}
		if id == "" {
			continue
		}
		for i := range nodes {
			if nodes[i].ID != id {
				continue
			}
			if nodes[i].Data == nil {
				nodes[i].Data = map[string]interface{}{}
			}
			// prompt_suffix 不是真实字段：它是要追加到提示词末尾的一段约束
			// （例如「全程无对白」），写进 data 不会有任何节点读它。
			if p.Key == "prompt_suffix" {
				if sfx, ok := p.To.(string); ok && sfx != "" {
					cur, _ := nodes[i].Data["prompt"].(string)
					if !strings.Contains(cur, sfx) {
						nodes[i].Data["prompt"] = strings.TrimSpace(cur + " " + sfx)
					}
				}
				continue
			}
			nodes[i].Data[p.Key] = p.To
		}
	}
}

// autoTargetNode 自动挑生成节点：媒体产物的源头。
func autoTargetNode(nodes []model.NodeInstance) string {
	priority := []string{"shotlist", "text2video", "comfy_workflow", "text2image", "image2image", "upscale"}
	for _, t := range priority {
		for _, n := range nodes {
			if n.Type == t {
				return n.ID
			}
		}
	}
	// 兜底：没有已知生成节点时，挑「带生成参数」的节点（frames/width/prompt），
	// 跳过 start/end/门这类控制节点。自定义节点因此也能被修补。
	for _, n := range nodes {
		if n.Type == "start" || n.Type == "end" || n.Type == "if_else" || n.Type == "quality_gate" {
			continue
		}
		if _, ok := n.Data["frames"]; ok {
			return n.ID
		}
		if _, ok := n.Data["width"]; ok {
			return n.ID
		}
	}
	return ""
}

// derivePatches 把「哪项没过」翻译成「改哪个字段改成多少」。
//
// 只做有确定解的修复：尺寸/帧数/时长/响度这类数值项。
// 采样噪声、语义类问题（乱语、画面不对）交给下一轮重采样——
// 硬编一个「改提示词」的规则，只会把问题变成随机。
func derivePatches(checks []gateCheck, r gateRules, targetNode string) []repairPatch {
	var out []repairPatch
	add := func(key string, to interface{}, why string) {
		for _, p := range out {
			if p.Key == key {
				if n, ok := to.(int); ok {
					if cur, ok2 := p.To.(int); ok2 && n <= cur {
						return
					}
				}
			}
		}
		out = append(out, repairPatch{Node: targetNode, Key: key, To: to, Why: why})
	}
	for _, c := range checks {
		if c.OK || c.Level != "fail" {
			continue
		}
		switch c.Name {
		case "min_frames":
			add("frames", int(math.Ceil(float64(r.minFrames))), c.Fix)
		case "min_width":
			add("width", snap16(r.minWidth), c.Fix)
		case "min_height":
			add("height", snap16(r.minHeight), c.Fix)
		case "duration_min":
			add("frames", int(math.Ceil(r.durationMin*24)), c.Fix)
		case "duration_max":
			// 上限类不自动改：降时长会连带改内容，交给人。
		case "has_audio_stream":
			add("audio", true, c.Fix)
		case "forbid_speech":
			add("prompt_suffix", "（纯环境音，无人声对白，无旁白）", c.Fix)
		}
	}
	// 帧数类补丁统一按 4 的倍数对齐：多数视频模型要求 frames = 4n+1 或 4n。
	for i := range out {
		if out[i].Key == "frames" {
			if n, ok := out[i].To.(int); ok && n < 5 {
				out[i].To = 5
			}
		}
	}
	return out
}

// snap16 宽高对齐到 16 的倍数（扩散模型的分块要求）。
func snap16(v int) int {
	if v <= 0 {
		return 0
	}
	return int(math.Ceil(float64(v)/16.0)) * 16
}

// scoreChecks 打分：fail 级检查的通过率（warn 不计分，只进列表）。
func scoreChecks(checks []gateCheck) (int, []string, []string) {
	var fails, fixes []string
	total, passed := 0, 0
	for _, c := range checks {
		if c.Level != "fail" {
			continue
		}
		total++
		if c.OK {
			passed++
			continue
		}
		fails = append(fails, fmt.Sprintf("%s：%s", c.Name, c.Detail))
		if c.Fix != "" {
			fixes = append(fixes, c.Fix)
		}
	}
	if total == 0 {
		return 100, nil, nil // 没配 fail 级规则 = 没有可阻断项
	}
	return int(math.Round(float64(passed) * 100 / float64(total))), fails, fixes
}

func verdictOf(fails []string) string {
	if len(fails) == 0 {
		return "pass"
	}
	// 门的判据只看 fail 列表，与 quality_gate 的 verdict 口径一致。
	return "fail"
}

// pickArtifactPathFromStates 从节点状态里找产物路径（优先 end 节点的显式输出）。
func pickArtifactPathFromStates(states []model.NodeInstance) string {
	// 先看 end：用户显式连到 end 的才是"交付物"
	for i := len(states) - 1; i >= 0; i-- {
		if states[i].Type != "end" || states[i].Status != "success" {
			continue
		}
		if p := artifactPathOf(states[i].Output); p != "" {
			return p
		}
	}
	for i := len(states) - 1; i >= 0; i-- {
		if states[i].Status != "success" {
			continue
		}
		if p := artifactPathOf(states[i].Output); p != "" {
			return p
		}
	}
	return ""
}

func artifactPathOf(out interface{}) string {
	m, ok := out.(map[string]interface{})
	if !ok {
		return ""
	}
	for _, k := range []string{"video", "image", "audio", "path", "file"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func qualityLoopError(rounds []qualityRound) error {
	if len(rounds) == 0 {
		return fmt.Errorf("质量循环没有跑出任何一轮")
	}
	last := rounds[len(rounds)-1]
	return fmt.Errorf("质量循环全部 %d 轮失败，最后一轮：%s", len(rounds), last.Error)
}

// qualityLoopSummary 一句话结论：几轮、最好第几轮、多少分、还差什么。
func qualityLoopSummary(rounds []qualityRound, best *qualityRound, target int) string {
	var scores []string
	for _, r := range rounds {
		if r.Error != "" {
			scores = append(scores, fmt.Sprintf("R%d=失败", r.Round))
			continue
		}
		scores = append(scores, fmt.Sprintf("R%d=%d", r.Round, r.Score))
	}
	state := "达标"
	if best.Score < target {
		state = "未达标"
	}
	lines := []string{
		fmt.Sprintf("质量循环 %d 轮（%s），取第 %d 轮（%d 分，目标 %d）：%s",
			len(rounds), strings.Join(scores, " "), best.Round, best.Score, target, state),
	}
	if len(best.Fixes) > 0 {
		lines = append(lines, "仍待修复："+strings.Join(dedupStrings(best.Fixes), "；"))
	}
	return strings.Join(lines, "\n")
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
