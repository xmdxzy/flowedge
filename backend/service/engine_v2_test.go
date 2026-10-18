package service

import (
	"encoding/json"
	"testing"
)

// newTestEngine 构造一个不依赖外部模型服务的引擎（用于纯 DAG 逻辑测试）
func newTestEngine() *EngineV2 {
	return &EngineV2{
		llm: nil, t2i: nil, asr: nil, tts: nil, video: nil,
		http: nil, rag: nil, gpuSched: nil, wsHub: nil,
	}
}

func marshalNodes(t *testing.T, nodes []map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(nodes)
	if err != nil {
		t.Fatalf("marshal nodes: %v", err)
	}
	return b
}

func marshalEdges(t *testing.T, edges []map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(edges)
	if err != nil {
		t.Fatalf("marshal edges: %v", err)
	}
	return b
}

// 测试线性 DAG：start -> end
func TestEngine_LinearDAG(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "hello"}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "edge-1", "source": "s1", "target": "e1"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	status := map[string]string{}
	for _, n := range states {
		status[n.ID] = n.Status
	}
	if status["s1"] != "success" {
		t.Errorf("start node status = %s, want success", status["s1"])
	}
	if status["e1"] != "success" {
		t.Errorf("end node status = %s, want success", status["e1"])
	}
}

// 测试条件分支：非空输入应走 true 分支
func TestEngine_IfElseTrueBranch(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "non-empty"}},
		{"id": "if1", "type": "if_else", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"condition_type": "not_empty"}},
		{"id": "et", "type": "end", "position": map[string]float64{"x": 2, "y": -1}, "data": map[string]interface{}{}},
		{"id": "ef", "type": "end", "position": map[string]float64{"x": 2, "y": 1}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "e1", "source": "s1", "target": "if1"},
		{"id": "e2t", "source": "if1", "target": "et", "sourceHandle": "true"},
		{"id": "e2f", "source": "if1", "target": "ef", "sourceHandle": "false"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	status := map[string]string{}
	for _, n := range states {
		status[n.ID] = n.Status
	}
	if status["et"] != "success" {
		t.Errorf("true branch = %s, want success", status["et"])
	}
	if status["ef"] != "skipped" {
		t.Errorf("false branch = %s, want skipped", status["ef"])
	}
}

// 测试条件分支：空输入应走 false 分支
func TestEngine_IfElseFalseBranch(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": ""}},
		{"id": "if1", "type": "if_else", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"condition_type": "not_empty"}},
		{"id": "et", "type": "end", "position": map[string]float64{"x": 2, "y": -1}, "data": map[string]interface{}{}},
		{"id": "ef", "type": "end", "position": map[string]float64{"x": 2, "y": 1}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "e1", "source": "s1", "target": "if1"},
		{"id": "e2t", "source": "if1", "target": "et", "sourceHandle": "true"},
		{"id": "e2f", "source": "if1", "target": "ef", "sourceHandle": "false"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	status := map[string]string{}
	for _, n := range states {
		status[n.ID] = n.Status
	}
	if status["et"] != "skipped" {
		t.Errorf("true branch = %s, want skipped", status["et"])
	}
	if status["ef"] != "success" {
		t.Errorf("false branch = %s, want success", status["ef"])
	}
}

// 测试菱形 DAG（分支后汇合）
func TestEngine_DiamondDAG(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "x"}},
		{"id": "m1", "type": "data_transform", "position": map[string]float64{"x": 1, "y": -1}, "data": map[string]interface{}{"template": "A"}},
		{"id": "m2", "type": "data_transform", "position": map[string]float64{"x": 1, "y": 1}, "data": map[string]interface{}{"template": "B"}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "m1"},
		{"id": "b", "source": "s1", "target": "m2"},
		{"id": "c", "source": "m1", "target": "e1"},
		{"id": "d", "source": "m2", "target": "e1"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	status := map[string]string{}
	for _, n := range states {
		status[n.ID] = n.Status
	}
	for _, id := range []string{"s1", "m1", "m2", "e1"} {
		if status[id] != "success" {
			t.Errorf("node %s = %s, want success", id, status[id])
		}
	}
}

// 测试 GPU 调度器水位线配置
func TestGPUScheduler_Thresholds(t *testing.T) {
	s := NewGPUScheduler(24576)
	cfg := s.config

	if cfg.SafeThresholdMB != 17203 {
		t.Errorf("safe threshold = %d, want 17203", cfg.SafeThresholdMB)
	}
	if cfg.WarnThresholdMB != 20889 {
		t.Errorf("warn threshold = %d, want 20889", cfg.WarnThresholdMB)
	}
	if cfg.RedThresholdMB != 22609 {
		t.Errorf("red threshold = %d, want 22609", cfg.RedThresholdMB)
	}
}

// 测试向量存储：余弦相似度
func TestVectorStore_Similarity(t *testing.T) {
	vs := NewVectorStore(t.TempDir())

	id1, err := vs.Add(VectorRecord{
		Content:   "人工智能的发展",
		Embedding: SimpleEmbed("人工智能的发展"),
	})
	if err != nil || id1 == "" {
		t.Fatalf("add record 1 failed: %v", err)
	}

	_, err = vs.Add(VectorRecord{
		Content:   "完全无关的内容xyz",
		Embedding: SimpleEmbed("完全无关的内容xyz"),
	})
	if err != nil {
		t.Fatalf("add record 2 failed: %v", err)
	}

	if vs.Count() != 2 {
		t.Errorf("count = %d, want 2", vs.Count())
	}

	results, err := vs.SearchByText("人工智能", 1)
	if err != nil || len(results) != 1 {
		t.Fatalf("search failed: %v, results=%d", err, len(results))
	}
	if results[0].Record.Content != "人工智能的发展" {
		t.Errorf("top result = %s, want 人工智能的发展", results[0].Record.Content)
	}
}
