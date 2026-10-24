package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flowedge/backend/model"
)

// newTestEngine 构造一个不依赖外部模型服务的引擎（用于纯 DAG 逻辑测试）
//
// 只注册测试替身节点：test_echo（原样回传 value）与 test_sleep（睡 ms 毫秒），
// 用来驱动纯编排逻辑（分支/汇合/重试/断点），不需要任何模型或网络服务。
func newTestEngine() *EngineV2 {
	e := &EngineV2{}
	e.registerBuiltinNodes()
	e.RegisterNodeType("test_echo", func(_ context.Context, n *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		v, _ := n.Data["value"].(string)
		return map[string]interface{}{"text": v}, nil
	})
	e.RegisterNodeType("test_fail", func(_ context.Context, n *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		return nil, fmt.Errorf("intentional failure")
	})
	e.RegisterNodeType("test_append", func(_ context.Context, n *model.NodeInstance, up map[string]interface{}) (interface{}, error) {
		suffix, _ := n.Data["suffix"].(string)
		return map[string]interface{}{"text": upstreamText(up) + suffix}, nil
	})
	e.RegisterNodeType("test_sleep", func(_ context.Context, n *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		ms, _ := n.Data["ms"].(float64)
		if ms <= 0 {
			ms = 50
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return map[string]interface{}{"text": "slept"}, nil
	})
	return e
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
		{"id": "m1", "type": "test_echo", "position": map[string]float64{"x": 1, "y": -1}, "data": map[string]interface{}{"value": "A"}},
		{"id": "m2", "type": "test_echo", "position": map[string]float64{"x": 1, "y": 1}, "data": map[string]interface{}{"value": "B"}},
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

// 测试节点级重试：no-url 的 http_request 节点重试 2 次（delay 120ms）后整体失败
func TestEngine_NodeRetry(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "x"}},
		{"id": "h1", "type": "test_fail", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"retry": 2, "retry_delay_ms": 120}},
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
		{"id": "m1", "type": "test_fail", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{}},
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
		{"id": "m1", "type": "test_echo", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"value": "fixed-output"}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})

	prev := []model.NodeInstance{
		{ID: "s1", Type: "start", Status: "success", Output: map[string]interface{}{"text": "hello"}},
		{ID: "m1", Type: "test_sleep", Status: "failed"},
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

// 测试 LLM 流式：token 经 WS 广播、model 覆盖传导、最终输出完整
func TestEngine_LLMStreaming(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n")
		f.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"世界\"}}]}\n\n")
		f.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n")
		f.Flush()
	}))
	defer srv.Close()

	hub := NewWSHub()
	events := hub.Subscribe()
	e := &EngineV2{
		llmStream: NewLLMStreamService(srv.URL, "default-model"),
		wsHub:     hub,
	}

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "输入"}},
		{"id": "l1", "type": "llm", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"model": "node-model"}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "l1"},
		{"id": "b", "source": "l1", "target": "e1"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("streaming execute failed: %v", err)
	}

	var tokenText strings.Builder
	for {
		select {
		case ev := <-events:
			if ev.Type == "node_token" {
				if d, ok := ev.Data["delta"].(string); ok {
					tokenText.WriteString(d)
				}
			}
		default:
			goto drained
		}
	}
drained:
	if tokenText.String() != "你好世界" {
		t.Errorf("token deltas = %q, want %q", tokenText.String(), "你好世界")
	}
	if gotModel != "node-model" {
		t.Errorf("stream request model = %q, want node-model", gotModel)
	}
	for _, n := range states {
		if n.ID == "l1" {
			if m, ok := n.Output.(map[string]interface{}); ok {
				if m["text"] != "你好世界" {
					t.Errorf("final text = %v, want 你好世界", m["text"])
				}
			} else {
				t.Fatalf("llm output missing: %v", n.Output)
			}
		}
	}
}

// loopInner 内层工作流：start → data_transform({{input}}+ ) → end
// 每轮把输入追加一个 "+"，用于验证链式迭代（上一轮输出 → 下一轮输入）
func loopInner(t *testing.T) ([]byte, []byte) {
	t.Helper()
	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "in-s", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{}},
		{"id": "in-t", "type": "test_append", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"suffix": "+"}},
		{"id": "in-e", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "ie1", "source": "in-s", "target": "in-t"},
		{"id": "ie2", "source": "in-t", "target": "in-e"},
	})
	return nodes, edges
}

// loopState 启动一个外层 start → loop → end 并返回 loop 节点输出
func runLoopCase(t *testing.T, loopData map[string]interface{}, prompt string) (map[string]interface{}, *EngineV2) {
	t.Helper()
	e := newTestEngine()
	innerNodes, innerEdges := loopInner(t)
	e.SetResolver(func(id uint) ([]byte, []byte, error) {
		if id == 88 {
			return innerNodes, innerEdges, nil
		}
		return nil, nil, fmt.Errorf("workflow %d not found", id)
	})

	loopData["workflow_id"] = 88
	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": prompt}},
		{"id": "lp", "type": "loop", "position": map[string]float64{"x": 1, "y": 0}, "data": loopData},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "lp"},
		{"id": "b", "source": "lp", "target": "e1"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("loop execute failed: %v", err)
	}
	for _, n := range states {
		if n.ID == "lp" {
			m, ok := n.Output.(map[string]interface{})
			if !ok {
				t.Fatalf("loop output is not a map: %v", n.Output)
			}
			return m, e
		}
	}
	t.Fatalf("loop node state missing")
	return nil, nil
}

// count 模式：固定 3 轮，每轮以上一轮输出为输入（链式精炼）
func TestEngine_LoopCount(t *testing.T) {
	out, _ := runLoopCase(t, map[string]interface{}{
		"mode":       "count",
		"iterations": 3,
	}, "a")

	if got := out["text"]; got != "a+++" {
		t.Errorf("loop text = %v, want a+++", got)
	}
	if got := out["iterations"]; got != 3 {
		t.Errorf("iterations = %v, want 3", got)
	}
	if got := out["stopped_by"]; got != "completed" {
		t.Errorf("stopped_by = %v, want completed", got)
	}

	items, ok := out["items"].([]interface{})
	if !ok || len(items) != 3 {
		t.Fatalf("items = %v, want 3 records", out["items"])
	}
	wantInputs := []string{"a", "a+", "a++"}
	for i, it := range items {
		rec := it.(map[string]interface{})
		if rec["input"] != wantInputs[i] {
			t.Errorf("iteration %d input = %v, want %s (not chained?)", i+1, rec["input"], wantInputs[i])
		}
	}
}

// foreach 模式：按换行切分上游文本，逐项输入，迭代次数 = 条目数
func TestEngine_LoopForeach(t *testing.T) {
	out, _ := runLoopCase(t, map[string]interface{}{
		"mode": "foreach",
	}, "x\ny\nz")

	if got := out["iterations"]; got != 3 {
		t.Errorf("iterations = %v, want 3", got)
	}
	if got := out["stopped_by"]; got != "exhausted" {
		t.Errorf("stopped_by = %v, want exhausted", got)
	}
	if got := out["text"]; got != "z+" {
		t.Errorf("text = %v, want z+ (last item output)", got)
	}

	items := out["items"].([]interface{})
	want := []string{"x", "y", "z"}
	for i, it := range items {
		if rec := it.(map[string]interface{}); rec["input"] != want[i] {
			t.Errorf("iteration %d input = %v, want %s", i+1, rec["input"], want[i])
		}
	}
}

// until 模式：条件命中即停（本例跑到第三轮出现 "+++"）
func TestEngine_LoopUntil(t *testing.T) {
	out, _ := runLoopCase(t, map[string]interface{}{
		"mode":           "until",
		"condition_type": "contains",
		"compare_value":  "+++",
		"max_iterations": 10,
	}, "a")

	if got := out["iterations"]; got != 3 {
		t.Errorf("iterations = %v, want 3 (stop at a+++)", got)
	}
	if got := out["stopped_by"]; got != "condition" {
		t.Errorf("stopped_by = %v, want condition", got)
	}
	if got := out["text"]; got != "a+++" {
		t.Errorf("text = %v, want a+++", got)
	}
}

// 安全阀：条件永不满足时必须被 max_iterations 截断，不能无限跑
func TestEngine_LoopMaxIterations(t *testing.T) {
	out, _ := runLoopCase(t, map[string]interface{}{
		"mode":           "until",
		"condition_type": "contains",
		"compare_value":  "never-appears",
		"max_iterations": 4,
	}, "a")

	if got := out["iterations"]; got != 4 {
		t.Errorf("iterations = %v, want 4", got)
	}
	if got := out["stopped_by"]; got != "max_iterations" {
		t.Errorf("stopped_by = %v, want max_iterations", got)
	}
	if got := out["text"]; got != "a++++" {
		t.Errorf("text = %v, want a++++", got)
	}
}

// 分支汇合：if_else 命中 false 后，true 分支被裁剪、汇合节点仍必须执行
// （此前的缺陷：被跳过的前驱不会递减前驱计数 → 汇合节点永远等不到 remaining==0）
func TestEngine_BranchMerge(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "no-match"}},
		{"id": "c1", "type": "if_else", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"condition_type": "contains", "compare_value": "HIT"}},
		{"id": "t1", "type": "test_echo", "position": map[string]float64{"x": 2, "y": -1}, "data": map[string]interface{}{"value": "true-branch"}},
		{"id": "f1", "type": "test_echo", "position": map[string]float64{"x": 2, "y": 1}, "data": map[string]interface{}{"value": "false-branch"}},
		{"id": "m1", "type": "test_echo", "position": map[string]float64{"x": 3, "y": 0}, "data": map[string]interface{}{"value": "merged:{{input}}"}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 4, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "c1"},
		{"id": "b", "source": "c1", "target": "t1", "sourceHandle": "true"},
		{"id": "c", "source": "c1", "target": "f1", "sourceHandle": "false"},
		{"id": "d", "source": "t1", "target": "m1"},
		{"id": "f", "source": "f1", "target": "m1"},
		{"id": "g", "source": "m1", "target": "e1"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("branch merge execute failed: %v", err)
	}
	st := map[string]string{}
	for _, n := range states {
		st[n.ID] = n.Status
	}
	if st["t1"] != "skipped" {
		t.Errorf("true branch = %s, want skipped", st["t1"])
	}
	if st["m1"] != "success" {
		t.Errorf("merge node = %s, want success (skipped predecessor must release the count)", st["m1"])
	}
	if st["e1"] != "success" {
		t.Errorf("end = %s, want success", st["e1"])
	}
}

// join=any：深分支不必等另一条链的后续层，汇合节点可与其并行
func TestEngine_JoinAny(t *testing.T) {

	run := func(join string) (jStart time.Time, bEnd time.Time) {
		e := newTestEngine()

		data := map[string]interface{}{"value": "{{input}}"}
		if join != "" {
			data["join"] = join
		}

		nodes := marshalNodes(t, []map[string]interface{}{
			{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "seed"}},
			{"id": "x1", "type": "test_echo", "position": map[string]float64{"x": 1, "y": -1}, "data": map[string]interface{}{"value": "fast"}},
			{"id": "a1", "type": "test_echo", "position": map[string]float64{"x": 1, "y": 1}, "data": map[string]interface{}{"value": "slow-seed"}},
			{"id": "b1", "type": "test_sleep", "position": map[string]float64{"x": 2, "y": 1}, "data": map[string]interface{}{"ms": 250}},
			{"id": "j1", "type": "test_echo", "position": map[string]float64{"x": 3, "y": 0}, "data": data},
			{"id": "e1", "type": "end", "position": map[string]float64{"x": 4, "y": 0}, "data": map[string]interface{}{}},
		})
		edges := marshalEdges(t, []map[string]interface{}{
			{"id": "e1", "source": "s1", "target": "x1"},
			{"id": "e2", "source": "s1", "target": "a1"},
			{"id": "e3", "source": "a1", "target": "b1"},
			{"id": "e4", "source": "x1", "target": "j1"},
			{"id": "e5", "source": "b1", "target": "j1"},
			{"id": "e6", "source": "j1", "target": "e1"},
		})

		states, err := e.Execute(nodes, edges)
		if err != nil {
			t.Fatalf("join=%q execute failed: %v", join, err)
		}
		for _, n := range states {
			switch n.ID {
			case "j1":
				if n.Status != "success" || n.StartedAt == nil || n.EndedAt == nil {
					t.Fatalf("join=%q merge node status=%s", join, n.Status)
				}
				jStart = *n.StartedAt
			case "b1":
				if n.Status != "success" || n.EndedAt == nil {
					t.Fatalf("join=%q slow node status=%s err=%s", join, n.Status, n.Error)
				}
				bEnd = *n.EndedAt
			}
		}
		return
	}

	allJ, allB := run("all")
	anyJ, anyB := run("any")

	if allJ.Before(allB) {
		t.Errorf("join=all must not start before every predecessor finished: j1 started %v, b1 ended %v", allJ, allB)
	}
	if !anyJ.Before(anyB) {
		t.Errorf("join=any should run without waiting: j1 started %v, b1 ended %v", anyJ, anyB)
	}
}

// continue_on_error：节点失败不中断工作流，错误对象向下游传播
func TestEngine_ContinueOnError(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "x"}},
		{"id": "f1", "type": "test_fail", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{"continue_on_error": true}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "f1"},
		{"id": "b", "source": "f1", "target": "e1"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("workflow must not fail when continue_on_error is set: %v", err)
	}
	st := map[string]string{}
	var failOut interface{}
	for _, n := range states {
		st[n.ID] = n.Status
		if n.ID == "f1" {
			failOut = n.Output
		}
	}
	if st["f1"] != "failed" {
		t.Errorf("failing node status = %s, want failed (still recorded honestly)", st["f1"])
	}
	if st["e1"] != "success" {
		t.Errorf("downstream = %s, want success", st["e1"])
	}
	if m, ok := failOut.(map[string]interface{}); !ok || m["error"] == nil {
		t.Errorf("failed node should propagate an error object downstream, got %v", failOut)
	}
}

// 变量替换的基本行为：已知节点替换、未知节点/字段保留原文、{{input}} 不被动
func TestResolveDataVariables(t *testing.T) {
	ctx := map[string]interface{}{
		"llm-1":  map[string]interface{}{"text": "摘要内容"},
		"http-2": map[string]interface{}{"text": "ok", "status_code": float64(200)},
		"meta-3": map[string]interface{}{"tags": []interface{}{"a", "b"}},
		"loop-4": map[string]interface{}{"iterations": float64(3), "text": "done"},
	}

	cases := []struct {
		in   string
		want string
	}{
		{"前缀 {{llm-1.text}} 后缀", "前缀 摘要内容 后缀"},
		{"状态 {{http-2.status_code}}", "状态 200"},
		{"{{ loop-4.iterations }} 轮", "3 轮"},
		{"{{meta-3.tags}}", `["a","b"]`},
		{"{{missing.text}} 未完成", "{{missing.text}} 未完成"},
		{"{{llm-1.nosuch}}", "{{llm-1.nosuch}}"},
		{"保留 {{input}} 给模板节点", "保留 {{input}} 给模板节点"},
		{"无变量", "无变量"},
	}
	for _, c := range cases {
		if got := resolveString(c.in, ctx); got != c.want {
			t.Errorf("resolveString(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// 深拷贝：不能修改原始数据
	orig := map[string]interface{}{"template": "{{llm-1.text}}", "nested": map[string]interface{}{"v": "{{llm-1.text}}"}}
	out := resolveData(orig, ctx)
	if orig["template"] != "{{llm-1.text}}" {
		t.Error("resolveData mutated the source map")
	}
	if out["template"] != "摘要内容" {
		t.Errorf("resolved template = %v", out["template"])
	}
	if n := out["nested"].(map[string]interface{}); n["v"] != "摘要内容" {
		t.Errorf("nested value not resolved: %v", n["v"])
	}
}

// 端到端：两条并行分支各自产出，汇合节点用变量把它们拼起来
func TestEngine_VariablesAcrossBranches(t *testing.T) {
	e := newTestEngine()

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "seed"}},
		{"id": "t1", "type": "test_echo", "position": map[string]float64{"x": 1, "y": -1}, "data": map[string]interface{}{"value": "A"}},
		{"id": "t2", "type": "test_echo", "position": map[string]float64{"x": 1, "y": 1}, "data": map[string]interface{}{"value": "B"}},
		{"id": "m1", "type": "test_echo", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{"value": "{{t1.text}}-{{t2.text}}"}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 3, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "t1"},
		{"id": "b", "source": "s1", "target": "t2"},
		{"id": "c", "source": "t1", "target": "m1"},
		{"id": "d", "source": "t2", "target": "m1"},
		{"id": "e", "source": "m1", "target": "e1"},
	})

	states, err := e.Execute(nodes, edges)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	for _, n := range states {
		if n.ID == "m1" {
			m := n.Output.(map[string]interface{})
			if m["text"] != "A-B" {
				t.Errorf("merge output = %v, want A-B (variable refs across parallel branches)", m["text"])
			}
			return
		}
	}
	t.Fatal("merge node state missing")
}
