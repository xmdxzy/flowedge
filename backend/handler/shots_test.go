package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

const shotlistNodesJSON = `[
  {"id":"start1","type":"start","position":{"x":0,"y":0},"data":{}},
  {"id":"sl1","type":"shotlist","position":{"x":200,"y":0},
   "data":{"shots":[{"id":1},{"id":2},{"id":3}],"width":608,"height":352}},
  {"id":"end1","type":"end","position":{"x":400,"y":0},"data":{}}
]`

// TestInjectReroll_SetsFieldAndKeepsEverythingElse 注入 reroll 不能丢前端字段
// （position / style / measured 丢了，续跑返回的图在前端会变形）
func TestInjectReroll_SetsFieldAndKeepsEverythingElse(t *testing.T) {
	out, nodeID, count, err := injectReroll([]byte(shotlistNodesJSON), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if nodeID != "sl1" {
		t.Errorf("nodeID = %q", nodeID)
	}
	if count != 3 {
		t.Errorf("shot count = %d, want 3", count)
	}
	var nodes []map[string]interface{}
	if err := json.Unmarshal(out, &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("node count changed: %d", len(nodes))
	}
	for _, n := range nodes {
		if _, ok := n["position"]; !ok {
			t.Errorf("position lost on node %v", n["id"])
		}
	}
	data := nodes[1]["data"].(map[string]interface{})
	if data["reroll"] != "2" {
		t.Errorf("reroll = %v, want \"2\"", data["reroll"])
	}
	if _, ok := data["shots"]; !ok {
		t.Error("shots config was dropped")
	}
}

// TestInjectReroll_MergesExistingReroll 用户已经手写了 reroll，再点重拍要合并
func TestInjectReroll_MergesExistingReroll(t *testing.T) {
	raw := `[{"id":"sl1","type":"shotlist","data":{"reroll":"1,3","shots":[{},{},{}]}}]`
	out, _, _, err := injectReroll([]byte(raw), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []map[string]interface{}
	_ = json.Unmarshal(out, &nodes)
	got := nodes[0]["data"].(map[string]interface{})["reroll"]
	if !strings.Contains(got.(string), "1,3") || !strings.Contains(got.(string), "2") {
		t.Errorf("reroll should merge: %v", got)
	}
}

// TestInjectReroll_RejectsOutOfRange 镜号超范围要在入口拦住，
// 而不是让它跑到引擎里变成一个没人懂的失败
func TestInjectReroll_RejectsOutOfRange(t *testing.T) {
	_, _, count, err := injectReroll([]byte(shotlistNodesJSON), "", 9)
	if err == nil {
		t.Fatal("expected range error")
	}
	if !strings.Contains(err.Error(), "3 镜") {
		t.Errorf("error should state the actual shot count: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d", count)
	}
}

// TestInjectReroll_RejectsNonShotlistNode 指定错节点要报清楚
func TestInjectReroll_RejectsNonShotlistNode(t *testing.T) {
	_, _, _, err := injectReroll([]byte(shotlistNodesJSON), "start1", 1)
	if err == nil || !strings.Contains(err.Error(), "shotlist") {
		t.Errorf("expected shotlist error, got %v", err)
	}
	_, _, _, err = injectReroll([]byte(shotlistNodesJSON), "nope", 1)
	if err == nil {
		t.Error("unknown node id must error")
	}
}

// TestInjectReroll_NoShotlistNode 没有分镜节点时给能看懂的解释
func TestInjectReroll_NoShotlistNode(t *testing.T) {
	raw := `[{"id":"a","type":"text2image","data":{}}]`
	_, _, _, err := injectReroll([]byte(raw), "", 1)
	if err == nil || !strings.Contains(err.Error(), "分镜") {
		t.Errorf("expected explanation, got %v", err)
	}
}

// TestInjectReroll_BrokenJSON 坏数据不能静默通过
func TestInjectReroll_BrokenJSON(t *testing.T) {
	if _, _, _, err := injectReroll([]byte(`{`), "", 1); err == nil {
		t.Error("broken json must error")
	}
	if _, _, _, err := injectReroll([]byte(`[]`), "", 1); err == nil {
		t.Error("empty node list must error")
	}
}

// TestCountShots_PlanString 分镜表以 plan JSON 字符串保存时也要能数镜数
func TestCountShots_PlanString(t *testing.T) {
	data := map[string]interface{}{
		"plan": "```json\n{\"shots\":[{\"id\":1},{\"id\":2}]}\n```",
	}
	if got := countShots(data); got != 2 {
		t.Errorf("countShots = %d, want 2", got)
	}
	if got := countShots(map[string]interface{}{}); got != 0 {
		t.Errorf("empty data should yield 0, got %d", got)
	}
}
