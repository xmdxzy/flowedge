package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"flowedge/backend/model"
)

// 节点超时后，那条执行协程必须收到取消。
//
// 由来（2026-10-26 实测 run 211）：sl-1 在 02:20:51 判超时失败，
// 而它的逐镜事件在 **02:45:25** 才写出「shot=1 failed 2701.5s」——
// 引擎宣布失败之后，那条协程又跑了 25 分钟，还在发事件、调 ASR、跑 ffmpeg。
// 若不取消，它还会继续提交后面的镜次（每镜又是一次十几分钟起步的生成）。
func TestNodeTimeout_CancelsExecution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e := newTestEngine()
	e.t2i = &Text2ImageService{BaseURL: srv.URL, H3BaseURL: srv.URL}

	var sawCancel atomic.Bool
	e.RegisterNodeType("test_wait_cancel", func(ctx context.Context, _ *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		// 模拟「正在跑的长任务」：超时之后它得能看到 ctx 被取消
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return map[string]interface{}{"text": "too late"}, nil
		}
	})

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "x"}},
		{"id": "w1", "type": "test_wait_cancel", "position": map[string]float64{"x": 1, "y": 0},
			"data": map[string]interface{}{"timeout_ms": float64(120)}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "w1"},
		{"id": "b", "source": "w1", "target": "e1"},
	})

	if _, err := e.Execute(nodes, edges); err == nil {
		t.Fatal("超时应当让执行失败")
	}

	// 给协程一点时间看到取消并记录
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sawCancel.Load() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("超时后执行协程没有收到 ctx 取消——它会带着已完成/未完成的活继续跑下去")
}
