package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"flowedge/backend/model"
)

// 图级成本估算。
//
// 口径（写死在实现里，不接受"拍脑袋"）：
//  1. **不报单值**。任何"这条要 40 分钟"的说法都在骗人——冷/热、命中缓存、
//     分辨率、步数任一变化就换了一条曲线。返回的是 热-乐观 / P50 / P90 / 硬上限 四档。
//  2. **不跨参数外推**。分单位算：一张 1024² 图 ≠ 一帧 608×352 ≠ 一个字，
//     各自有基准；分辨率只按面积做线性缩放，且夹在 [0.25, 6] 倍之间防止离谱。
//  3. **冷加载单独记**。显存是单侧驻留的（换侧要卸载），所以冷加载按"用到的
//     显存侧别"累加，只在 P90 档计入——P50 假设实例已经热的。
//  4. **算不出来的就说算不出来**。quality_loop 的成本取决于被循环的图，
//     静态不可知，进 Uncertain 列表，不编数字。

// EstimateLine 单个节点的估算明细（前端画"谁最贵"用）。
type EstimateLine struct {
	NodeID   string  `json:"node_id"`
	NodeType string  `json:"node_type"`
	Unit     string  `json:"unit"`
	Count    float64 `json:"count"`
	AreaMul  float64 `json:"area_mul"`
	Seconds  float64 `json:"seconds"`
	Note     string  `json:"note,omitempty"`
}

// GraphEstimate 图级估算结果。刻意没有单一 seconds 字段。
type GraphEstimate struct {
	OptimisticSec float64 `json:"optimistic_sec"` // 最乐观（热实例、无重试）
	P50Sec        float64 `json:"p50_sec"`
	P90Sec        float64 `json:"p90_sec"`        // 含冷加载
	WorstSec      float64 `json:"worst_sec"`      // 硬上限：各节点超时预算之和
	ColdLoadSec   float64 `json:"cold_load_sec"`  // P90 里含的冷加载部分
	VramPeakMB    int     `json:"vram_peak_mb"`   // 峰值取最大值，不是求和（单侧驻留）

	Lines     []EstimateLine `json:"lines"`
	Uncertain []string       `json:"uncertain,omitempty"`
	Caveat    string         `json:"caveat"`
}

const estimateCaveat = "区间估算：同参数线性外推，分辨率按面积缩放。跨后端/跨步数/首次运行请以实测为准；worst 是各节点超时预算之和，不是期望值。"

// estimateFloat 读数值字段，兼容 JSON 解出来的 float64 与手写/配置给的整数。
//
// 为什么不用 fieldFloat：它把 0 当作"没给"（`v != 0`），而估算里 0 是合法值
// （例如 steps=0 的透传节点），并且它只认 float64。估算是纯读出，不该被这两条改变。
func estimateFloat(n *model.NodeInstance, key string, def float64) float64 {
	if n == nil {
		return def
	}
	v, ok := n.Data[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return def
}
// clampArea 面积缩放系数，夹在 [0.25, 6]，防止 4K/320 这类极端把估算拉爆。
func clampArea(v float64) float64 {
	if v <= 0 || math.IsNaN(v) {
		return 1
	}
	if v < 0.25 {
		return 0.25
	}
	if v > 6 {
		return 6
	}
	return v
}

// estimateCount 按 Cost.Unit 数出"要跑多少个单位"。
func estimateCount(n *model.NodeInstance, c Cost) (float64, string) {
	switch c.Unit {
	case "image":
		batch := estimateFloat(n, "batch", 1)
		if batch < 1 {
			batch = 1
		}
		lines := float64(len(splitPromptLines(n.Data["prompts"])))
		if lines < 1 {
			lines = 1
		}
		count := math.Max(batch, lines)
		note := ""
		if count > 1 {
			note = fmt.Sprintf("%.0f 张", count)
		}
		return count, note
	case "frame":
		frames := estimateFloat(n, "frames", 0)
		if frames <= 0 {
			frames = 124 // H3 单镜默认帧数
		}
		segs := estimateFloat(n, "segments", 1)
		if segs < 1 {
			segs = 1
		}
		return frames * segs, fmt.Sprintf("%.0f 帧 × %.0f 段", frames, segs)
	case "char":
		text := fieldString(n, "text")
		if text == "" {
			text = fieldString(n, "prompt")
		}
		cnt := float64(len([]rune(text)))
		if cnt < 1 {
			cnt = 1
		}
		return cnt, fmt.Sprintf("%.0f 字", cnt)
	case "call":
		return 1, ""
	case "round":
		rounds := estimateFloat(n, "max_rounds", 3)
		if rounds < 1 {
			rounds = 3
		}
		return rounds, fmt.Sprintf("最多 %.0f 轮（内层成本未知）", rounds)
	}
	return 0, ""
}

// EstimateGraph 估算整张图的耗时与显存峰值。
func EstimateGraph(nodes []model.NodeInstance, edges []model.EdgeInstance) GraphEstimate {
	est := GraphEstimate{Caveat: estimateCaveat, Lines: make([]EstimateLine, 0, len(nodes))}
	_ = edges // 目前估算与连接关系无关；保留参数是为了将来做"跳过未连分支"

	coldBySide := make(map[string]float64)
	for i := range nodes {
		n := &nodes[i]
		contract, ok := nodeContracts[n.Type]
		if !ok {
			est.Uncertain = append(est.Uncertain,
				fmt.Sprintf("节点 %s 的类型 %q 没有成本模型，未计入耗时", n.ID, n.Type))
			continue
		}
		c := contract.Cost
		if c.Unit == "round" {
			est.Uncertain = append(est.Uncertain,
				fmt.Sprintf("节点 %s（%s）的成本取决于被循环的图，静态不可知，未计入耗时", n.ID, n.Type))
			continue
		}
		if c.Unit == "graph" {
			// comfy 逃生舱：图里是 fifty 步的 1024² 还是双阶段放大，只有用户知道。
			// 编一个"默认 20 分钟"等于把估算变成了猜测；进 Uncertain 让前端亮黄字。
			est.Uncertain = append(est.Uncertain,
				fmt.Sprintf("节点 %s（%s）跑的是用户自备的 ComfyUI 图，耗时与显存取决于图内容，静态不可知，未计入", n.ID, n.Type))
			continue
		}
		if n.Type == "shotlist" {
			// 分镜表若已内联，可以按真实镜数/帧数估；否则只能按单镜默认值，必须声明
			if raw, ok := n.Data["plan"].(string); ok && raw != "" {
				if plan, err := ParseShotPlan(raw); err == nil && len(plan.Shots) > 0 {
					total := 0
					for _, sh := range plan.Shots {
						total += sh.Frames
					}
					base := c.BaseSec + c.SecPerUnit*float64(total)
					est.Lines = append(est.Lines, EstimateLine{
						NodeID: n.ID, NodeType: n.Type, Unit: "frame",
						Count: float64(total), AreaMul: 1, Seconds: base,
						Note: fmt.Sprintf("%d 镜 / %d 帧", len(plan.Shots), total),
					})
					if c.VramMB > est.VramPeakMB {
						est.VramPeakMB = c.VramMB
					}
					if c.ColdLoadSec > 0 && c.ColdLoadSec > coldBySide[c.GPUClass] {
						coldBySide[c.GPUClass] = c.ColdLoadSec
					}
					continue
				}
			}
			est.Uncertain = append(est.Uncertain,
				fmt.Sprintf("节点 %s 的分镜表来自上游（未内联），镜数与帧数未知，按单镜 124 帧估", n.ID))
		}

		count, note := estimateCount(n, c)
		areaMul := 1.0
		if c.RefWidth > 0 && c.RefHeight > 0 {
			w := estimateFloat(n, "width", float64(c.RefWidth))
			h := estimateFloat(n, "height", float64(c.RefHeight))
			areaMul = clampArea((w * h) / float64(c.RefWidth*c.RefHeight))
			if math.Abs(areaMul-1) > 0.01 {
				if note != "" {
					note += "，"
				}
				note += fmt.Sprintf("面积 ×%.2f", areaMul)
			}
		}
		sec := c.BaseSec + c.SecPerUnit*count*areaMul
		est.Lines = append(est.Lines, EstimateLine{
			NodeID: n.ID, NodeType: n.Type, Unit: c.Unit,
			Count: count, AreaMul: areaMul, Seconds: sec, Note: note,
		})
		if c.VramMB > est.VramPeakMB {
			est.VramPeakMB = c.VramMB
		}
		if c.ColdLoadSec > 0 && c.ColdLoadSec > coldBySide[c.GPUClass] {
			coldBySide[c.GPUClass] = c.ColdLoadSec
		}
	}

	p50 := 0.0
	worst := 0.0
	for _, l := range est.Lines {
		p50 += l.Seconds
	}
	for i := range nodes {
		worst += nodeTimeoutFor(&nodes[i]).Seconds()
	}
	cold := 0.0
	for _, v := range coldBySide {
		cold += v
	}

	est.P50Sec = math.Round(p50*10) / 10
	est.OptimisticSec = math.Round(p50*0.6*10) / 10
	est.ColdLoadSec = math.Round(cold*10) / 10
	est.P90Sec = math.Round((p50*1.4+cold)*10) / 10
	est.WorstSec = math.Round(worst*10) / 10
	return est
}
