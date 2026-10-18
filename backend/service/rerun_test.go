package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"encoding/json"

	"flowedge/backend/model"
)

// newCountingEngine 造一个只带 HTTP 服务的引擎：每个 http 节点按 URL path 计数，
// 用来精确验证「哪些节点真的被执行了」。
func newCountingEngine(t *testing.T, url string) (*EngineV2, *map[string]int) {
	t.Helper()
	var mu sync.Mutex
	hits := map[string]int{}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	_ = url
	return eng, &hits
}

func httpNode(id, url string) model.NodeInstance {
	return model.NodeInstance{ID: id, Type: "http_request", Data: map[string]interface{}{"url": url, "method": "GET"}}
}

// startHTTPBHTTPEnd 构造 start → a → b → end 的线性图
func linearHTTPGraph(srvURL string) ([]byte, []byte) {
	nodes := []model.NodeInstance{
		{ID: "s", Type: "start", Data: map[string]interface{}{"prompt": "go"}},
		httpNode("a", srvURL+"/a"),
		httpNode("b", srvURL+"/b"),
		{ID: "e", Type: "end", Data: map[string]interface{}{}},
	}
	edges := []model.EdgeInstance{
		{ID: "e1", Source: "s", Target: "a"},
		{ID: "e2", Source: "a", Target: "b"},
		{ID: "e3", Source: "b", Target: "e"},
	}
	nb, _ := json.Marshal(nodes)
	eb, _ := json.Marshal(edges)
	return nb, eb
}

// TestEngine_ResumeFromNode_RerunsOnlyDownstream
// 从中间节点重跑：该节点及其下游必须重算，成功的上游不得再执行。
func TestEngine_ResumeFromNode_RerunsOnlyDownstream(t *testing.T) {
	mux := http.NewServeMux()
	var mu sync.Mutex
	hits := map[string]int{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	nodesJSON, edgesJSON := linearHTTPGraph(srv.URL)
	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)

	// 首次执行：a、b 各执行一次
	states, err := eng.ExecutePersistent(nodesJSON, edgesJSON, nil)
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	if err := checkAllSuccess(states); err != nil {
		t.Fatal(err)
	}
	if hits["/a"] != 1 || hits["/b"] != 1 {
		t.Fatalf("first run: got a=%d b=%d, want 1/1", hits["/a"], hits["/b"])
	}

	// 整体续跑：全部 success → 不应再执行任何节点
	states2, err := eng.Resume(nodesJSON, edgesJSON, states, nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if hits["/a"] != 1 || hits["/b"] != 1 {
		t.Fatalf("full resume should not re-execute: a=%d b=%d", hits["/a"], hits["/b"])
	}

	// 从 b 重跑：b 重算，a 保持原样
	states3, err := eng.ResumeFrom(nodesJSON, edgesJSON, states2, "b", nil)
	if err != nil {
		t.Fatalf("resumefrom(b) failed: %v", err)
	}
	if err := checkAllSuccess(states3); err != nil {
		t.Fatal(err)
	}
	if hits["/a"] != 1 {
		t.Fatalf("a must NOT re-execute when rerun from b, got %d", hits["/a"])
	}
	if hits["/b"] != 2 {
		t.Fatalf("b must re-execute exactly once, got %d", hits["/b"])
	}

	// 从 a 重跑：a、b 都必须重算
	_, err = eng.ResumeFrom(nodesJSON, edgesJSON, states3, "a", nil)
	if err != nil {
		t.Fatalf("resumefrom(a) failed: %v", err)
	}
	if hits["/a"] != 2 || hits["/b"] != 3 {
		t.Fatalf("rerun from a: got a=%d b=%d, want 2/3", hits["/a"], hits["/b"])
	}
}

// TestEngine_ResumeFromNode_PreservesUpstreamOutput
// 重跑后上游输出不得变化（断点复用生效）
func TestEngine_ResumeFromNode_PreservesUpstreamOutput(t *testing.T) {
	nodesJSON, edgesJSON := linearHTTPGraph("http://127.0.0.1:1/unused") // 不会真的请求：从 a 重跑时 a 会失败
	_ = nodesJSON
	_ = edgesJSON

	// 用自定义 transform 节点避免真实网络
	nodes := []model.NodeInstance{
		{ID: "s", Type: "start", Data: map[string]interface{}{"prompt": "hello"}},
		{ID: "t1", Type: "data_transform", Data: map[string]interface{}{"template": "{{input}}-one"}},
		{ID: "t2", Type: "data_transform", Data: map[string]interface{}{"template": "{{t1.text}}-two"}},
		{ID: "e", Type: "end", Data: map[string]interface{}{}},
	}
	edges := []model.EdgeInstance{
		{ID: "e1", Source: "s", Target: "t1"},
		{ID: "e2", Source: "t1", Target: "t2"},
		{ID: "e3", Source: "t2", Target: "e"},
	}
	nb, _ := json.Marshal(nodes)
	eb, _ := json.Marshal(edges)

	eng := NewEngineV2(nil, nil, nil, nil, nil, NewHTTPService(), nil, nil, nil)
	states, err := eng.ExecutePersistent(nb, eb, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	out1 := ""
	for _, n := range states {
		if n.ID == "t1" {
			if m, ok := n.Output.(map[string]interface{}); ok {
				out1, _ = m["text"].(string)
			}
		}
	}
	if out1 == "" {
		t.Fatal("t1 produced no output")
	}

	// 从 t2 重跑：t1 的输出对象必须原样保留
	states2, err := eng.ResumeFrom(nb, eb, states, "t2", nil)
	if err != nil {
		t.Fatalf("resumefrom failed: %v", err)
	}
	out2 := ""
	for _, n := range states2 {
		if n.ID == "t1" {
			if m, ok := n.Output.(map[string]interface{}); ok {
				out2, _ = m["text"].(string)
			}
		}
	}
	if out2 != out1 {
		t.Fatalf("upstream output changed: %q -> %q", out1, out2)
	}

	outT2 := ""
	for _, n := range states2 {
		if n.ID == "t2" {
			if m, ok := n.Output.(map[string]interface{}); ok {
				outT2, _ = m["text"].(string)
			}
		}
	}
	if outT2 == "" {
		t.Fatal("t2 produced no output after rerun")
	}
}

// TestDownstreamClosure 闭包必须包含自身与全部下游
func TestDownstreamClosure(t *testing.T) {
	edges := []model.EdgeInstance{
		{Source: "a", Target: "b"},
		{Source: "b", Target: "c"},
		{Source: "x", Target: "c"},
		{Source: "c", Target: "d"},
	}
	set := downstreamClosure(edges, "b")
	for _, want := range []string{"b", "c", "d"} {
		if !set[want] {
			t.Fatalf("closure should contain %q, got %v", want, set)
		}
	}
	for _, notWant := range []string{"a", "x"} {
		if set[notWant] {
			t.Fatalf("closure should NOT contain %q, got %v", notWant, set)
		}
	}
	if downstreamClosure(edges, "") != nil {
		t.Fatal("empty fromNode must yield nil closure")
	}
}

func checkAllSuccess(states []model.NodeInstance) error {
	for _, n := range states {
		if n.Status != "success" {
			return &nodeExecError{Code: "test", Err: errTestStatus(n.ID, n.Status, n.Error)}
		}
	}
	return nil
}

func errTestStatus(id, status, errMsg string) error {
	return &simpleErr{msg: "node " + id + " status=" + status + " err=" + errMsg}
}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

var _ = newCountingEngine
