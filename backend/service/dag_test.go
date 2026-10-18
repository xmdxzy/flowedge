package service

import (
	"testing"

	"flowedge/backend/model"
)

func n(id, typ string) model.NodeInstance {
	return model.NodeInstance{ID: id, Type: typ, Data: map[string]interface{}{}}
}

func e(src, tgt string) model.EdgeInstance {
	return model.EdgeInstance{ID: src + "->" + tgt, Source: src, Target: tgt}
}

func hasCode(issues []DAGIssue, code DAGIssueCode) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// TestDetectCycle_Simple 最简环 A→B→A 必须被检出
func TestDetectCycle_Simple(t *testing.T) {
	nodes := []model.NodeInstance{n("a", "start"), n("b", "llm")}
	edges := []model.EdgeInstance{e("a", "b"), e("b", "a")}

	issues := ValidateDAG(nodes, edges)
	if !hasCode(issues, IssueCycle) {
		t.Fatalf("expected cycle detected, got %+v", issues)
	}
	if !HasBlockingIssue(issues) {
		t.Fatal("cycle must be a blocking issue")
	}
}

// TestDetectCycle_DiamondIsNotCycle 菱形 DAG 不得被误判为环。
// 这是选 Kahn 而非 DFS 的核心理由：DFS 回溯边会把菱形判成环。
func TestDetectCycle_DiamondIsNotCycle(t *testing.T) {
	nodes := []model.NodeInstance{n("a", "start"), n("b", "llm"), n("c", "llm"), n("d", "end")}
	edges := []model.EdgeInstance{e("a", "b"), e("a", "c"), e("b", "d"), e("c", "d")}

	issues := ValidateDAG(nodes, edges)
	if hasCode(issues, IssueCycle) {
		t.Fatalf("diamond DAG must NOT be reported as cycle, got %+v", issues)
	}
	if HasBlockingIssue(issues) {
		t.Fatalf("diamond DAG must have no blocking issue, got %+v", issues)
	}
}

// TestDetectCycle_BranchMergeSameTarget if_else 的 true/false 双边汇合到同一节点，
// 属于合法图，不得判环
func TestDetectCycle_BranchMergeSameTarget(t *testing.T) {
	nodes := []model.NodeInstance{n("s", "start"), n("i", "if_else"), n("m", "end")}
	edges := []model.EdgeInstance{
		{ID: "e1", Source: "s", Target: "i"},
		{ID: "e2", Source: "i", Target: "m", SourceHandle: "true"},
		{ID: "e3", Source: "i", Target: "m", SourceHandle: "false"},
	}

	issues := ValidateDAG(nodes, edges)
	if HasBlockingIssue(issues) {
		t.Fatalf("branch merge must be valid, got %+v", issues)
	}
	if hasCode(issues, IssueDuplicateEdge) {
		t.Fatalf("true/false handles are distinct edges, got %+v", issues)
	}
}

// TestDetectCycle_SelfLoop 自环单独归类，不混入通用环
func TestDetectCycle_SelfLoop(t *testing.T) {
	nodes := []model.NodeInstance{n("a", "llm")}
	edges := []model.EdgeInstance{e("a", "a")}

	issues := ValidateDAG(nodes, edges)
	if !hasCode(issues, IssueSelfLoop) {
		t.Fatalf("expected self_loop, got %+v", issues)
	}
}

// TestDetectCycle_DanglingEdge 连线指向不存在的节点
func TestDetectCycle_DanglingEdge(t *testing.T) {
	nodes := []model.NodeInstance{n("a", "start")}
	edges := []model.EdgeInstance{e("a", "ghost")}

	issues := ValidateDAG(nodes, edges)
	if !hasCode(issues, IssueDanglingEdge) {
		t.Fatalf("expected dangling_edge, got %+v", issues)
	}
	if !HasBlockingIssue(issues) {
		t.Fatal("dangling edge must block execution")
	}
}

// TestDetectCycle_DuplicateEdgeIsWarn 重复连线只警告不阻断（引擎可正常执行）
func TestDetectCycle_DuplicateEdgeIsWarn(t *testing.T) {
	nodes := []model.NodeInstance{n("a", "start"), n("b", "llm")}
	edges := []model.EdgeInstance{
		{ID: "e1", Source: "a", Target: "b"},
		{ID: "e2", Source: "a", Target: "b"},
	}

	issues := ValidateDAG(nodes, edges)
	if !hasCode(issues, IssueDuplicateEdge) {
		t.Fatalf("expected duplicate_edge, got %+v", issues)
	}
	if HasBlockingIssue(issues) {
		t.Fatalf("duplicate edge must not block, got %+v", issues)
	}
}

// TestDetectCycle_OrphanAndNoStartWarn 孤立节点与缺 start 只警告
func TestDetectCycle_OrphanAndNoStartWarn(t *testing.T) {
	nodes := []model.NodeInstance{n("a", "start"), n("lonely", "llm")}
	edges := []model.EdgeInstance{}

	_ = edges
	issues := ValidateDAG(nodes, nil)
	if !hasCode(issues, IssueOrphanNode) {
		t.Fatalf("expected orphan_node warn, got %+v", issues)
	}
	if HasBlockingIssue(issues) {
		t.Fatalf("orphan must not block, got %+v", issues)
	}
}

// TestValidateDAGJSON handler 保存期校验入口
func TestValidateDAGJSON(t *testing.T) {
	nodesJSON := []byte(`[{"id":"a","type":"start","data":{}},{"id":"b","type":"llm","data":{}}]`)
	edgesJSON := []byte(`[{"id":"e1","source":"a","target":"b"},{"id":"e2","source":"b","target":"a"}]`)

	issues, err := ValidateDAGJSON(nodesJSON, edgesJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCode(issues, IssueCycle) {
		t.Fatalf("expected cycle via JSON entry, got %+v", issues)
	}

	if _, err := ValidateDAGJSON([]byte(`not-json`), nil); err == nil {
		t.Fatal("malformed nodes JSON must return error")
	}
}
