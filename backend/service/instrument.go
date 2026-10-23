package service

import (
	"fmt"
	"math"
	"sort"

	"flowedge/backend/model"
)

// 自动接尺子：给一张没有门的图插入质检门。
//
// 为什么这件事必须由代码做，不能靠文档提醒
//
//	12 条生产工作流零门覆盖（2026-10-23 实测），不是没人知道要接门，而是
//	「接门」这件事本身有坑：接错位置会切断产物传递、阈值凭空给会制造假失败。
//	把它写成函数，坑就只踩一次；写成建议，每张图都要再踩一遍。
//
// 两条硬约束（读执行器代码得出的，不是偏好）
//
//	① 门只能接在**终点前**，不能插在中间。
//	   引擎的 upstream 只来自直接前驱（engine_v2.go nodeInputs）：把门插在
//	   生成节点和消费节点之间，下游拿到的就是门的输出 {gate, checks, path}，
//	   而不是 {video: "..."} —— 下游会报「缺产物」，整条图跑不起来。
//	   接在终点前则完全无损：end 不消费产物，门失败整条 run 记 failed。
//
//	② 阈值从图里**已有的生成参数反推**，不凭空给。
//	   凭空给 min_frames=124 而生成节点 frames=96，门就永远不过 —— 那不是
//	   「验收」，那是把一条能跑的图改死。反推 + 5% 容差才是可执行的默认值，
//	   剩下的（响度/静音/一致性）需要按片子定，不在这里猜。

// InstrumentOptions 接门的选项
type InstrumentOptions struct {
	// GateIDPrefix 门节点 id 前缀（默认 evo-gate）
	GateIDPrefix string
	// Tolerance 阈值容差（默认 0.05，即 95% 的生成值仍算过）
	Tolerance float64
}

// InstrumentPlan 接门方案。不落库：调用方先看方案，再决定要不要写。
type InstrumentPlan struct {
	Applied bool `json:"applied"`
	// Gates 新插入的门节点 id
	Gates []string `json:"gates,omitempty"`
	// Derived 每个阈值是从哪个节点的哪个参数反推来的（可复核，不许魔术数字）
	Derived []string `json:"derived,omitempty"`
	// Skipped 为什么没接（非空即 Applied=false）
	Skipped []string             `json:"skipped,omitempty"`
	Nodes   []model.NodeInstance `json:"-"`
	Edges   []model.EdgeInstance `json:"-"`
}

// InstrumentGates 给图插入质检门。幂等：已有门的图原样返回并说明原因。
func InstrumentGates(nodes []model.NodeInstance, edges []model.EdgeInstance, opts InstrumentOptions) InstrumentPlan {
	if opts.GateIDPrefix == "" {
		opts.GateIDPrefix = "evo-gate"
	}
	if opts.Tolerance <= 0 || opts.Tolerance >= 1 {
		opts.Tolerance = 0.05
	}
	plan := InstrumentPlan{Nodes: nodes, Edges: edges}

	if len(nodes) == 0 {
		plan.Skipped = append(plan.Skipped, "图里没有节点")
		return plan
	}

	// 已有门就不动：门是判据，重复接等于改判据范围，必须由人显式决定。
	var existing []string
	for i := range nodes {
		if mod, ok := ModuleContractOf(nodes[i].Type); ok && mod.Role == RoleGate {
			existing = append(existing, nodes[i].ID)
		}
	}
	if len(existing) > 0 {
		sort.Strings(existing)
		plan.Skipped = append(plan.Skipped,
			fmt.Sprintf("已有 %d 个门（%v）：不重复接。要换门请手工改图，别让工具替你决定判据范围",
				len(existing), existing))
		return plan
	}

	byID := make(map[string]*model.NodeInstance, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	incoming := map[string][]model.EdgeInstance{}
	for _, e := range edges {
		incoming[e.Target] = append(incoming[e.Target], e)
	}

	var ends []string
	for i := range nodes {
		if nodes[i].Type == "end" {
			ends = append(ends, nodes[i].ID)
		}
	}
	if len(ends) == 0 {
		plan.Skipped = append(plan.Skipped,
			"没有终点节点（end）：门要接在终点前，找不到落点。加一个 end 节点后再接")
		return plan
	}
	sort.Strings(ends)

	newNodes := append([]model.NodeInstance(nil), nodes...)
	newEdges := append([]model.EdgeInstance(nil), edges...)
	usedIDs := map[string]bool{}
	for i := range nodes {
		usedIDs[nodes[i].ID] = true
	}

	for _, endID := range ends {
		ins := incoming[endID]
		if len(ins) == 0 {
			plan.Skipped = append(plan.Skipped,
				fmt.Sprintf("终点 %s 没有任何入边：门接上去也收不到产物", endID))
			continue
		}

		gateID := uniqueID(opts.GateIDPrefix+"-"+endID, usedIDs)
		usedIDs[gateID] = true

		// 阈值反推：只看这条终点链上的祖先（别的分支的参数与这条产物无关）
		ancestors := ancestorIDs(endID, incoming, byID)
		data, derived, why := deriveGateData(ancestors, byID, opts.Tolerance)
		if len(data) == 0 {
			// 没有任何可反推的生成参数：接上去也是一条永远不查的门。
			// 明确跳过并说清缺什么，比接一条空门更诚实（空门只会让证据多一行 gateNodes）。
			plan.Skipped = append(plan.Skipped, fmt.Sprintf(
				"终点 %s 的上游没有可反推的生成参数（%s）：接空门等于没有门，需手工给阈值", endID, why))
			continue
		}

		pos := map[string]float64{"x": 0, "y": 0}
		if end := byID[endID]; end != nil && end.Position != nil {
			pos = map[string]float64{"x": end.Position["x"] - 260, "y": end.Position["y"]}
		}
		newNodes = append(newNodes, model.NodeInstance{
			ID: gateID, Type: "quality_gate", Status: "pending",
			Position: pos, Data: data,
		})

		// 改接：每条入边改为指向门，门再指向终点。
		// TargetHandle 清空：门的输入只有一个 any 口，保留旧 handle 会指向不存在的端口。
		var keptHandle string
		for i := range newEdges {
			if newEdges[i].Target != endID {
				continue
			}
			if keptHandle == "" {
				keptHandle = newEdges[i].TargetHandle
			}
			newEdges[i].Target = gateID
			newEdges[i].TargetHandle = ""
			newEdges[i].ID = newEdges[i].ID + "-via-" + gateID
		}
		newEdges = append(newEdges, model.EdgeInstance{
			ID:     "edge-" + gateID + "-" + endID,
			Source: gateID, Target: endID, TargetHandle: keptHandle,
		})

		plan.Gates = append(plan.Gates, gateID)
		plan.Derived = append(plan.Derived, derived...)
	}

	if len(plan.Gates) == 0 {
		return plan
	}
	plan.Applied = true
	plan.Nodes = newNodes
	plan.Edges = newEdges
	return plan
}

// uniqueID 生成一个没被占用的节点 id
func uniqueID(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !used[cand] {
			return cand
		}
	}
}

// ancestorIDs 沿入边向上收集祖先（含直接前驱，不含自身）
func ancestorIDs(id string, incoming map[string][]model.EdgeInstance, byID map[string]*model.NodeInstance) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(cur string)
	walk = func(cur string) {
		for _, e := range incoming[cur] {
			if seen[e.Source] {
				continue
			}
			seen[e.Source] = true
			if byID[e.Source] == nil {
				continue
			}
			out = append(out, e.Source)
			walk(e.Source)
		}
	}
	walk(id)
	sort.Strings(out)
	return out
}

// deriveGateData 从祖先节点的生成参数反推门阈值。
//
// 只推**能算出来**的四类：帧数 / 尺寸 / 时长 / 是否需要音轨。
// 响度、静音缺口、一致性阈值一律不推 —— 那要按片子定，猜一个数字
// 等于给整库塞一条假的验收标准。
func deriveGateData(ancestors []string, byID map[string]*model.NodeInstance, tol float64) (map[string]interface{}, []string, string) {
	data := map[string]interface{}{}
	var derived []string
	var missing []string

	num := func(n *model.NodeInstance, key string) (float64, bool) {
		if n == nil {
			return 0, false
		}
		v, ok := n.Data[key].(float64)
		return v, ok
	}

	var maxFrames, maxW, maxH float64
	var fps float64
	framesFrom, sizeFrom := "", ""
	framesSegs := 1.0
	hasAudioSource := false

	for _, id := range ancestors {
		n := byID[id]
		if n == nil {
			continue
		}
		// frames 是**单段**帧数，segments 是段数。成片帧数 = frames × segments。
		//
		// 漏乘 segments 的后果不是「阈值偏松」，而是把一条能跑的图改死：
		// 修士斗法 v.frames=124、segments=6，成片实测 31.05s/1484 帧；
		// 只按单段反推得 duration_max=5.43s，门第一次跑就把真片判成超标。
		// 这类参数错配比「没接门」更糟 —— 它让门看起来在工作，实际在乱杀。
		if v, ok := num(n, "frames"); ok && v > 0 {
			segs := 1.0
			if s, ok2 := num(n, "segments"); ok2 && s > 1 {
				segs = s
			}
			if total := v * segs; total > maxFrames {
				maxFrames, framesFrom, framesSegs = total, id, segs
			}
		}
		if v, ok := num(n, "width"); ok && v > maxW {
			maxW, sizeFrom = v, id
		}
		if v, ok := num(n, "height"); ok && v > maxH {
			maxH = v
		}
		if v, ok := num(n, "fps"); ok && v > 0 {
			fps = v
		}
		// 音轨来源：任一声源节点都算（tts / audio / 后期混音开启）
		switch n.Type {
		case "tts", "voiceover", "audio_track", "post_production", "dub_track", "audio_mix":
			hasAudioSource = true
		}
		if hasAudioSource {
			continue
		}
		if _, ok := n.Data["audio"]; ok {
			hasAudioSource = true
		}
	}

	floorTo := func(v float64) int { return int(math.Floor(v)) }

	if maxFrames > 0 {
		data["min_frames"] = floorTo(maxFrames * (1 - tol))
		if framesSegs > 1 {
			derived = append(derived, fmt.Sprintf("min_frames=%d（%s.frames=%.0f × %.0f 段 = %.0f 帧，留 %.0f%% 容差）",
				floorTo(maxFrames*(1-tol)), framesFrom, maxFrames/framesSegs, framesSegs, maxFrames, tol*100))
		} else {
			derived = append(derived, fmt.Sprintf("min_frames=%d（%s.frames=%.0f，留 %.0f%% 容差）",
				floorTo(maxFrames*(1-tol)), framesFrom, maxFrames, tol*100))
		}
	} else {
		missing = append(missing, "frames")
	}
	if maxW > 0 {
		data["min_width"] = floorTo(maxW)
		derived = append(derived, fmt.Sprintf("min_width=%d（%s.width=%.0f，尺寸是生成死的，不留容差）",
			floorTo(maxW), sizeFrom, maxW))
	}
	if maxH > 0 {
		data["min_height"] = floorTo(maxH)
		derived = append(derived, fmt.Sprintf("min_height=%d（%s 链上最大 height=%.0f）", floorTo(maxH), sizeFrom, maxH))
	}
	if maxFrames > 0 && fps > 0 {
		secs := maxFrames / fps
		data["duration_min"] = round2(secs * (1 - tol))
		data["duration_max"] = round2(secs * (1 + tol))
		// 时长是比帧数更强的判据：帧数会因插帧翻倍（interp=true 时实测≈2×），
		// 但时长必须等于 声明帧数 ÷ fps —— 插帧不改时长。
		if framesSegs > 1 {
			derived = append(derived,
				fmt.Sprintf("duration_min=%.2fs / duration_max=%.2fs（%.0f 帧 × %.0f 段 ÷ %.1f fps = %.2fs，±%.0f%%）",
					round2(secs*(1-tol)), round2(secs*(1+tol)), maxFrames/framesSegs, framesSegs, fps, secs, tol*100))
		} else {
			derived = append(derived,
				fmt.Sprintf("duration_min=%.2fs / duration_max=%.2fs（%.0f 帧 ÷ %.1f fps = %.2fs，±%.0f%%）",
					round2(secs*(1-tol)), round2(secs*(1+tol)), maxFrames, fps, secs, tol*100))
		}
	}
	if hasAudioSource {
		data["require_audio"] = true
		derived = append(derived, "require_audio=true（上游有声源/混音节点，成片必须有音轨）")
	}
	if len(missing) > 0 {
		derived = append(derived, "未推："+fmt.Sprint(missing)+"（上游没写这几项，门不查它们）")
	}
	return data, derived, fmt.Sprint(missing)
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
