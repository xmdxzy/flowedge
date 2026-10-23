package service

import (
	"encoding/json"
	"net/http"
	"testing"

	"flowedge/backend/model"
)

// 任务书回写是**写操作**，出错代价比读大一个量级：
// 改错了图可能直接产出废片，而"半批生效"会让图停在一个谁都没设计过的状态。
// 所以测试的重心不是"能改对"，是"该拒的都拒了"和"整批原子"。

func nodesForApply() []model.NodeInstance {
	return []model.NodeInstance{
		{ID: "n1", Type: "text2image", Data: map[string]interface{}{
			"prompt": "a cat", "steps": float64(4), "width": float64(512),
		}},
		{ID: "g1", Type: "quality_gate", Data: map[string]interface{}{
			"require_audio": true, "min_frames": float64(124),
		}},
	}
}

// 改普通参数：生效，且回执带上旧值与新值
func TestApply_PlainParamApplied(t *testing.T) {
	next, applied, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "n1", Path: "data.steps", From: 4, To: 12},
	}, ApplyOptions{})

	if len(probs) != 0 {
		t.Fatalf("不该有问题：%v", probs)
	}
	if len(applied) != 1 || applied[0].Path != "data.steps" {
		t.Fatalf("回执不对：%+v", applied)
	}
	// from 是 int、现场是 float64，JSON 语义比较必须认得出相等
	if got := next[0].Data["steps"]; jsonText(got) != "12" {
		t.Fatalf("steps 应改为 12，实际 %v", got)
	}
}

// 节点不存在 → 404
func TestApply_NodeNotFound(t *testing.T) {
	_, _, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "nope", Path: "data.steps", From: 4, To: 12},
	}, ApplyOptions{})
	if len(probs) != 1 || probs[0].Code != EditNodeNotFound || probs[0].Status != http.StatusNotFound {
		t.Fatalf("应为 node_not_found/404，实际 %+v", probs)
	}
}

// 改图结构（type / position / id）→ 403。那不是调参，是换一张图。
func TestApply_StructuralPathRejected(t *testing.T) {
	for _, p := range []string{"type", "position.x", "id"} {
		_, _, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
			{NodeID: "n1", Path: p, From: "text2image", To: "llm"},
		}, ApplyOptions{})
		if len(probs) != 1 || probs[0].Code != EditPathNotAllowed || probs[0].Status != http.StatusForbidden {
			t.Fatalf("path=%s 应被 403 拒绝，实际 %+v", p, probs)
		}
	}
}

// 门层参数默认锁死：放宽验收必须显式授权
func TestApply_GateParamLockedByDefault(t *testing.T) {
	_, _, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "g1", Path: "data.min_frames", From: 124, To: 96, Reason: "想少等一会儿"},
	}, ApplyOptions{AllowGateChange: false})

	if len(probs) != 1 || probs[0].Code != EditGateLocked || probs[0].Status != http.StatusForbidden {
		t.Fatalf("门层参数应被锁，实际 %+v", probs)
	}
}

// 授权了但不写理由 → 400：审计上必须能回答"凭什么放宽了门"
func TestApply_GateParamRequiresReason(t *testing.T) {
	_, _, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "g1", Path: "data.min_frames", From: 124, To: 96},
	}, ApplyOptions{AllowGateChange: true})

	if len(probs) != 1 || probs[0].Code != EditBadValue || probs[0].Status != http.StatusBadRequest {
		t.Fatalf("应要求 reason，实际 %+v", probs)
	}
}

// 授权 + 理由齐全 → 允许（不堵死正常需求，只留痕）
func TestApply_GateParamAllowedWithReason(t *testing.T) {
	next, applied, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "g1", Path: "data.min_frames", From: 124, To: 186, Reason: "目标时长从 5s 提到 7.5s，帧数必须同步"},
	}, ApplyOptions{AllowGateChange: true})

	if len(probs) != 0 {
		t.Fatalf("应允许，实际 %+v", probs)
	}
	if len(applied) != 1 || jsonText(next[1].Data["min_frames"]) != "186" {
		t.Fatalf("未生效：%+v", next[1].Data)
	}
}

// from 与现场值不一致 → 409。这是乐观锁：期间别人改过，不能覆盖。
func TestApply_FromMismatch(t *testing.T) {
	_, _, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "n1", Path: "data.steps", From: 8, To: 12}, // 现场是 4
	}, ApplyOptions{})

	if len(probs) != 1 || probs[0].Code != EditFromMismatch || probs[0].Status != http.StatusConflict {
		t.Fatalf("应为 from_mismatch/409，实际 %+v", probs)
	}
}

// from 缺失 → 400：没有现场值就无法判断是否在覆盖别人的改动
func TestApply_FromRequired(t *testing.T) {
	_, _, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "n1", Path: "data.steps", To: 12},
	}, ApplyOptions{})

	if len(probs) != 1 || probs[0].Code != EditBadValue || probs[0].Status != http.StatusBadRequest {
		t.Fatalf("应要求 from，实际 %+v", probs)
	}
}

// 路径不存在 → 409（能核对但核对不了 from）
func TestApply_PathMissing(t *testing.T) {
	_, _, probs := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "n1", Path: "data.no_such_key", From: 1, To: 2},
	}, ApplyOptions{})

	if len(probs) != 1 || probs[0].Status != http.StatusConflict {
		t.Fatalf("应为冲突类错误，实际 %+v", probs)
	}
}

// edits 为空 → 400（不是"什么也没做"就算成功）
func TestApply_EmptyEdits(t *testing.T) {
	_, _, probs := ApplySpecEdits(nodesForApply(), nil, ApplyOptions{})
	if len(probs) != 1 || probs[0].Code != EditBadValue {
		t.Fatalf("空 edits 应报错，实际 %+v", probs)
	}
}

// 原子性：一批里有一条不过，整批作废 —— 包括那条本来能通过的不许落地。
//
// 半批生效是最危险的失败形态：每一条单独看都"成功"了，
// 但图会停在一个谁都没设计过的状态，且没人知道停在哪。
func TestApply_AtomicOnAnyFailure(t *testing.T) {
	orig := nodesForApply()
	next, applied, probs := ApplySpecEdits(orig, []SpecEdit{
		{NodeID: "n1", Path: "data.steps", From: 4, To: 12}, // 这条本来能过
		{NodeID: "nope", Path: "data.x", From: 1, To: 2},    // 这条不存在
	}, ApplyOptions{})

	if len(probs) != 1 {
		t.Fatalf("应报 1 个问题，实际 %d", len(probs))
	}
	if len(applied) != 0 {
		t.Fatalf("整批失败时不该有任何生效项，实际 %+v", applied)
	}
	if jsonText(next[0].Data["steps"]) != "4" {
		t.Fatalf("整批失败时节点必须原样返回，steps 变成了 %v", next[0].Data["steps"])
	}
}

// 嵌套路径要能下钻，且不破坏原对象的其它键
func TestApply_NestedPath(t *testing.T) {
	nodes := []model.NodeInstance{{ID: "n1", Type: "llm", Data: map[string]interface{}{
		"params": map[string]interface{}{"temperature": 0.7, "top_p": 0.9},
	}}}
	next, _, probs := ApplySpecEdits(nodes, []SpecEdit{
		{NodeID: "n1", Path: "data.params.temperature", From: 0.7, To: 0.3},
	}, ApplyOptions{})

	if len(probs) != 0 {
		t.Fatalf("不该有问题：%+v", probs)
	}
	p := next[0].Data["params"].(map[string]interface{})
	if jsonText(p["temperature"]) != "0.3" || jsonText(p["top_p"]) != "0.9" {
		t.Fatalf("嵌套改写出错：%v", p)
	}
}

// 回执必须能被 JSON 序列化（前端/MCP 直接吃），且 old/new 原样保留
func TestApply_ResultSerializable(t *testing.T) {
	_, applied, _ := ApplySpecEdits(nodesForApply(), []SpecEdit{
		{NodeID: "n1", Path: "data.prompt", From: "a cat", To: "a black cat"},
	}, ApplyOptions{})
	raw, err := json.Marshal(applied)
	if err != nil {
		t.Fatalf("回执不可序列化：%v", err)
	}
	if string(raw) == "[]" || string(raw) == "null" {
		t.Fatalf("回执为空：%s", raw)
	}
}
