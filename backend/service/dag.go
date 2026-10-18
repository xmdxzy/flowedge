package service

import (
	"encoding/json"
	"fmt"
	"sort"

	"flowedge/backend/model"
)

// DAGIssueCode 图结构问题的稳定错误码（API 与前端直接消费）
type DAGIssueCode string

const (
	IssueCycle         DAGIssueCode = "cycle"
	IssueSelfLoop      DAGIssueCode = "self_loop"
	IssueDanglingEdge  DAGIssueCode = "dangling_edge"
	IssueDuplicateEdge DAGIssueCode = "duplicate_edge" // 警告级：引擎可正常执行
	IssueOrphanNode    DAGIssueCode = "orphan_node"    // 警告级
	IssueNoStart       DAGIssueCode = "no_start"       // 警告级
)

// IssueCycle 等错误码对应的可读说明（前端可复用）
var issueMessages = map[DAGIssueCode]string{
	IssueCycle:         "存在循环依赖，涉及的节点永远不会执行",
	IssueSelfLoop:      "节点连到了自己",
	IssueDanglingEdge:  "连线指向了不存在的节点",
	IssueDuplicateEdge: "两个节点之间存在重复连线",
	IssueOrphanNode:    "节点没有任何连线",
	IssueNoStart:       "工作流没有 start 入口节点",
}

// DAGIssue 一条图结构问题。Level=error 会阻止保存与执行。
type DAGIssue struct {
	Code    DAGIssueCode `json:"code"`
	Level   string       `json:"level"` // error | warn
	Message string       `json:"message"`
	Nodes   []string     `json:"nodes,omitempty"`
}

func (i DAGIssue) Error() string { return i.Message }

// HasBlockingIssue 是否存在阻止执行/保存的 error 级问题
func HasBlockingIssue(issues []DAGIssue) bool {
	for _, i := range issues {
		if i.Level == "error" {
			return true
		}
	}
	return false
}

// FirstBlocking 返回第一条 error 级问题（无则返回零值）
func FirstBlocking(issues []DAGIssue) DAGIssue {
	for _, i := range issues {
		if i.Level == "error" {
			return i
		}
	}
	return DAGIssue{}
}

// ValidateDAG 校验工作流图结构。
//
// 环检测使用 **Kahn 入度法** 而非 DFS：DFS 的回溯边判定会把菱形 DAG
// （A→B、A→C、B→D、C→D）误判成环，而这类图在 FlowEdge 里非常常见
// （分支汇合 / join=any / continue_on_error），必须容忍。
func ValidateDAG(nodes []model.NodeInstance, edges []model.EdgeInstance) []DAGIssue {
	var issues []DAGIssue

	present := make(map[string]bool, len(nodes))
	hasStart := false
	for _, n := range nodes {
		present[n.ID] = true
		if n.Type == "start" {
			hasStart = true
		}
	}

	// 边规范化：剔除悬空边与自环，去重（source|target|handle）
	type edgeKey struct{ src, tgt, handle string }
	seen := make(map[edgeKey]bool)
	clean := make([]model.EdgeInstance, 0, len(edges))
	for _, eg := range edges {
		if !present[eg.Source] || !present[eg.Target] {
			issues = append(issues, DAGIssue{
				Code: IssueDanglingEdge, Level: "error",
				Message: fmt.Sprintf("%s：%s → %s", issueMessages[IssueDanglingEdge], eg.Source, eg.Target),
				Nodes:   []string{eg.Source, eg.Target},
			})
			continue
		}
		if eg.Source == eg.Target {
			issues = append(issues, DAGIssue{
				Code: IssueSelfLoop, Level: "error",
				Message: fmt.Sprintf("%s：%s", issueMessages[IssueSelfLoop], eg.Source),
				Nodes:   []string{eg.Source},
			})
			continue
		}
		k := edgeKey{eg.Source, eg.Target, eg.SourceHandle}
		if seen[k] {
			issues = append(issues, DAGIssue{
				Code: IssueDuplicateEdge, Level: "warn",
				Message: fmt.Sprintf("%s：%s → %s", issueMessages[IssueDuplicateEdge], eg.Source, eg.Target),
				Nodes:   []string{eg.Source, eg.Target},
			})
			continue
		}
		seen[k] = true
		clean = append(clean, eg)
	}

	// Kahn：入度归零即出队
	indeg := make(map[string]int, len(nodes))
	succ := make(map[string][]string, len(nodes))
	degree := make(map[string]int) // 入度+出度，用于孤立节点判定
	for _, eg := range clean {
		indeg[eg.Target]++
		succ[eg.Source] = append(succ[eg.Source], eg.Target)
		degree[eg.Source]++
		degree[eg.Target]++
	}

	queue := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if indeg[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	processed := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		processed++
		for _, t := range succ[id] {
			indeg[t]--
			if indeg[t] == 0 {
				queue = append(queue, t)
			}
		}
	}

	if processed < len(nodes) {
		cyclic := make([]string, 0)
		for _, n := range nodes {
			if indeg[n.ID] > 0 {
				cyclic = append(cyclic, n.ID)
			}
		}
		sort.Strings(cyclic)
		issues = append(issues, DAGIssue{
			Code: IssueCycle, Level: "error",
			Message: fmt.Sprintf("%s：%v", issueMessages[IssueCycle], cyclic),
			Nodes:   cyclic,
		})
	}

	// 孤立节点（多节点时才有意义）
	if len(nodes) > 1 {
		orphans := make([]string, 0)
		for _, n := range nodes {
			if degree[n.ID] == 0 {
				orphans = append(orphans, n.ID)
			}
		}
		if len(orphans) > 0 {
			sort.Strings(orphans)
			issues = append(issues, DAGIssue{
				Code: IssueOrphanNode, Level: "warn",
				Message: fmt.Sprintf("%s：%v", issueMessages[IssueOrphanNode], orphans),
				Nodes:   orphans,
			})
		}
	}

	if len(nodes) > 0 && !hasStart {
		issues = append(issues, DAGIssue{
			Code: IssueNoStart, Level: "warn",
			Message: issueMessages[IssueNoStart],
		})
	}

	return issues
}

// ValidateDAGJSON 直接校验前端提交的原始 JSON（handler 保存期校验入口）
func ValidateDAGJSON(nodesJSON, edgesJSON []byte) ([]DAGIssue, error) {
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	if len(nodesJSON) > 0 {
		if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
			return nil, fmt.Errorf("parse nodes failed: %w", err)
		}
	}
	if len(edgesJSON) > 0 {
		if err := json.Unmarshal(edgesJSON, &edges); err != nil {
			return nil, fmt.Errorf("parse edges failed: %w", err)
		}
	}
	return ValidateDAG(nodes, edges), nil
}
