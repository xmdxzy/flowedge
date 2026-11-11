package service

import (
	"flowedge/backend/model"
)

// 图级体检：把「结构 / 类型 / 参数」三层校验合成一份报告。
//
// 这是画布唯一的守门入口（POST /api/graph/validate），也是保存期的守门入口。
// 三层的分级刻意不同：
//   - 结构（DAG）：cycle / dangling / self_loop 是 error，孤立节点与重复边是 warn；
//   - 类型（端口）：类型不匹配是 error，端口名不认识是 warn；
//   - 参数：静态可判定的必填缺失是 error（有入边即放行，运行期才可能从上游拿到）。
//
// 报告结构前后端共用，字段名即协议。

// GraphReport 一次图体检的完整结果。
type GraphReport struct {
	OK       bool `json:"ok"`
	NodeCnt  int  `json:"node_count"`
	EdgeCnt  int  `json:"edge_count"`
	Blocking int  `json:"blocking"`
	Warnings int  `json:"warnings"`

	DAG    []DAGIssue       `json:"dag"`
	Ports  []PortIssue      `json:"ports"`
	Params []NodeParamIssue `json:"params"`

	// BlockingCodes 所有 error 级问题的 code（去重，按出现顺序），
	// 前端据此做「一键定位到节点」，测试据此断言不会悄悄放宽规则。
	BlockingCodes []string `json:"blocking_codes,omitempty"`
}

// ValidateGraph 三层校验一次跑完。
//
// hasInbound 由边集合推导：有入边的节点，其输入可能在运行期由上游提供，
// 保存期不能因为它「暂时没填」就判死（否则模板图全都存不进去）。
func ValidateGraph(nodes []model.NodeInstance, edges []model.EdgeInstance) GraphReport {
	rep := GraphReport{
		NodeCnt: len(nodes),
		EdgeCnt: len(edges),
		DAG:     ValidateDAG(nodes, edges),
		Ports:   ValidatePorts(nodes, edges),
		Params:  ValidateNodeParams(nodes, InboundSet(edges)),
	}

	seen := make(map[string]bool, 8)
	for _, i := range rep.DAG {
		if i.Level == "error" {
			rep.Blocking++
			if !seen[string(i.Code)] {
				seen[string(i.Code)] = true
				rep.BlockingCodes = append(rep.BlockingCodes, string(i.Code))
			}
		} else {
			rep.Warnings++
		}
	}
	for _, i := range rep.Ports {
		if i.Level == "error" {
			rep.Blocking++
			if !seen[i.Code] {
				seen[i.Code] = true
				rep.BlockingCodes = append(rep.BlockingCodes, i.Code)
			}
		} else {
			rep.Warnings++
		}
	}
	// 参数问题没有 level 字段：按约定全是保存期硬错误。
	rep.Blocking += len(rep.Params)
	if len(rep.Params) > 0 {
		rep.BlockingCodes = append(rep.BlockingCodes, "missing_param")
	}
	rep.OK = rep.Blocking == 0
	return rep
}

// LayoutNode 自动布局的落位结果（只在 Position 缺失或全为 0 时写入）。
const (
	layoutColGap = 260.0
	layoutRowGap = 150.0
	layoutPadX   = 80.0
	layoutPadY   = 80.0
)

// AutoLayout 给缺坐标的节点算一套分层布局。
//
// 为什么必须做：从 pipelines.json 反向生成的模板图**没有任何坐标**
// （`PipelineStore.Build` 只产出 id/type/data）。没有这一步，模板在画布上
// 会全部堆在原点，等于又回到「一片空白/一坨乱麻」——v1 的头号败因。
//
// 规则：按最长路径分层（Kahn），同层按 id 排序后纵向排开；
// 环里的节点（正常图不该有）统一放到最后一层，保证它们仍然可见可拖。
func AutoLayout(nodes []model.NodeInstance, edges []model.EdgeInstance) {
	if len(nodes) == 0 {
		return
	}
	present := make(map[string]bool, len(nodes))
	need := false
	for _, n := range nodes {
		present[n.ID] = true
		if n.Position == nil || (n.Position["x"] == 0 && n.Position["y"] == 0) {
			need = true
		}
	}
	if !need {
		return // 用户已经摆好了，不覆盖他的布局
	}

	indeg := make(map[string]int, len(nodes))
	succ := make(map[string][]string, len(nodes))
	for _, e := range edges {
		if !present[e.Source] || !present[e.Target] || e.Source == e.Target {
			continue
		}
		indeg[e.Target]++
		succ[e.Source] = append(succ[e.Source], e.Target)
	}

	depth := make(map[string]int, len(nodes))
	queue := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if indeg[n.ID] == 0 {
			queue = append(queue, n.ID)
			depth[n.ID] = 0
		}
	}
	order := make([]string, 0, len(nodes))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, t := range succ[id] {
			if d := depth[id] + 1; d > depth[t] {
				depth[t] = d
			}
			indeg[t]--
			if indeg[t] == 0 {
				queue = append(queue, t)
			}
		}
	}
	// 环里的节点：放到末层，避免它们因为没被 Kahn 走到而停在原点
	maxDepth := 0
	for _, d := range depth {
		if d > maxDepth {
			maxDepth = d
		}
	}
	for _, n := range nodes {
		if _, ok := depth[n.ID]; !ok {
			depth[n.ID] = maxDepth + 1
		}
	}

	// 同层内按 id 稳定排序，保证同一张图每次得到同样的布局（可 diff）
	layerIdx := make(map[string]int, len(nodes))
	for _, n := range nodes {
		d := depth[n.ID]
		layerIdx[n.ID] = d
	}
	byLayer := make(map[int][]string, maxDepth+2)
	for _, n := range nodes {
		d := layerIdx[n.ID]
		byLayer[d] = append(byLayer[d], n.ID)
	}
	for d := range byLayer {
		ids := byLayer[d]
		for i := 1; i < len(ids); i++ {
			for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
				ids[j], ids[j-1] = ids[j-1], ids[j]
			}
		}
		byLayer[d] = ids
	}

	for d, ids := range byLayer {
		for row, id := range ids {
			for i := range nodes {
				if nodes[i].ID != id {
					continue
				}
				if nodes[i].Position != nil && (nodes[i].Position["x"] != 0 || nodes[i].Position["y"] != 0) {
					continue
				}
				nodes[i].Position = map[string]float64{
					"x": layoutPadX + float64(d)*layoutColGap,
					"y": layoutPadY + float64(row)*layoutRowGap,
				}
			}
		}
	}
}
