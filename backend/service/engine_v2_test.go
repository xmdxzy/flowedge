package service

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"flowedge/backend/model"
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

// 测试节点级重试：no-url 的 http_request 节点重试 2 次（delay 120ms）后整体失败
func TestEngine_NodeRetry(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "x"}},
		{"id": "h1", "type": "http_request", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"retry": 2, "retry_delay_ms": 120}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "e1", "source": "s1", "target": "h1"},
	})

	states, err := e.Execute(nodes, edges)
	if err == nil {
		t.Fatalf("expected workflow failure after retries exhausted")
	}

	for _, n := range states {
		if n.ID == "h1" {
			if n.Status != "failed" {
				t.Errorf("http node status = %s, want failed", n.Status)
			}
			if n.StartedAt == nil || n.EndedAt == nil {
				t.Fatalf("http node missing timestamps")
			}
			spent := n.EndedAt.Sub(*n.StartedAt)
			// 1 次原始尝试 + 2 次重试，每次间隔 120ms → 至少 240ms
			if spent < 240*time.Millisecond {
				t.Errorf("retry elapsed = %v, want >= 240ms", spent)
			}
		}
	}
}

// 测试断点续跑：首跑失败 → 修复节点定义 → Resume 复用成功节点输出
func TestEngine_Resume(t *testing.T) {
	e := newTestEngine()

	// 第一跑：中间 http_request 节点没有 url → 失败
	badNodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "hello"}},
		{"id": "m1", "type": "http_request", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	badEdges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "m1"},
		{"id": "b", "source": "m1", "target": "e1"},
	})

	var persistCalls int
	_, err := e.ExecutePersistent(badNodes, badEdges, func(states []model.NodeInstance) {
		persistCalls++
	})
	if err == nil {
		t.Fatalf("first run should fail")
	}

	// 第二跑：修复中间节点（换成 data_transform），从断点恢复
	fixedNodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "hello"}},
		{"id": "m1", "type": "data_transform", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"template": "fixed-output"}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})

	prev := []model.NodeInstance{
		{ID: "s1", Type: "start", Status: "success", Output: map[string]interface{}{"text": "hello"}},
		{ID: "m1", Type: "http_request", Status: "failed"},
		{ID: "e1", Type: "end", Status: "pending"},
	}

	states, err := e.Resume(fixedNodes, badEdges, prev, nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	status := map[string]string{}
	output := map[string]interface{}{}
	for _, n := range states {
		status[n.ID] = n.Status
		output[n.ID] = n.Output
	}
	if status["s1"] != "success" {
		t.Errorf("reused start = %s, want success", status["s1"])
	}
	if status["m1"] != "success" {
		t.Errorf("re-executed middle = %s, want success", status["m1"])
	}
	if status["e1"] != "success" {
		t.Errorf("end = %s, want success", status["e1"])
	}
	if out := output["m1"]; out == nil {
		t.Errorf("middle output missing")
	}
	if persistCalls < 2 {
		t.Errorf("persist called %d times, want >= 2 (initial + layers)", persistCalls)
	}
}

// 测试子工作流：外层文本注入内层 start，内层 end 输出回传外层
func TestEngine_SubWorkflow(t *testing.T) {
	e := newTestEngine()

	innerNodes := marshalNodes(t, []map[string]interface{}{
		{"id": "in-s", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{}},
		{"id": "in-e", "type": "end", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{}},
	})
	innerEdges := marshalEdges(t, []map[string]interface{}{
		{"id": "ie1", "source": "in-s", "target": "in-e"},
	})
	e.SetResolver(func(id uint) ([]byte, []byte, error) {
		if id == 77 {
			return innerNodes, innerEdges, nil
		}
		return nil, nil, fmt.Errorf("workflow %d not found", id)
	})

	outerNodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "outer-text"}},
		{"id": "sub1", "type": "sub_workflow", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"workflow_id": 77}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	outerEdges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "sub1"},
		{"id": "b", "source": "sub1", "target": "e1"},
	})

	states, err := e.Execute(outerNodes, outerEdges)
	if err != nil {
		t.Fatalf("sub workflow execution failed: %v", err)
	}

	for _, n := range states {
		if n.ID == "e1" && n.Status == "success" {
			if m, ok := n.Output.(map[string]interface{}); ok {
				if m["text"] != "outer-text" {
					t.Errorf("sub workflow output text = %v, want outer-text", m["text"])
				}
				return
			}
			t.Fatalf("end output is not a map: %v", n.Output)
		}
	}
	t.Fatalf("outer end node did not succeed")
}
