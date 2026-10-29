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

// 节点超时后必须让 ComfyUI 停手。
//
// 由来（2026-10-26 实测）：run 203 的 sl-1 在 23:17:17 判超时失败，
// 但同一镜的生成一直跑到 23:45:14——引擎放弃之后 GPU 又空转了 28 分钟。
// 取消运行那条路径早就调 interruptComfy（CancelRun），超时这条漏了：
// 只丢结果，不停活。这条测试钉住「超时 = 停手 + 留痕」。
func TestNodeTimeout_InterruptsComfyUI(t *testing.T) {
	var mainHits, h3Hits atomic.Int32
	newSrv := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/interrupt" && r.Method == http.MethodPost {
				counter.Add(1)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
	}
	mainSrv := newSrv(&mainHits)
	defer mainSrv.Close()
	h3Srv := newSrv(&h3Hits)
	defer h3Srv.Close()

	e := newTestEngine()
	e.t2i = &Text2ImageService{BaseURL: mainSrv.URL, H3BaseURL: h3Srv.URL}
	// 一个永远睡下去、超出节点预算的节点
	e.RegisterNodeType("test_hang", func(ctx context.Context, _ *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return map[string]interface{}{"text": "late"}, nil
	})

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "x"}},
		{"id": "h1", "type": "test_hang", "position": map[string]float64{"x": 1, "y": 0},
			"data": map[string]interface{}{"timeout_ms": float64(120)}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 2, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "h1"},
		{"id": "b", "source": "h1", "target": "e1"},
	})

	_, err := e.Execute(nodes, edges)
	if err == nil {
		t.Fatal("超时应当让执行失败，实际成功返回")
	}

	// interruptComfy 两个实例都打：避免漏掉真正在跑的那个
	if n := mainHits.Load(); n != 1 {
		t.Fatalf("主实例应收到 1 次 /interrupt，实际 %d", n)
	}
	if n := h3Hits.Load(); n != 1 {
		t.Fatalf("H3 实例应收到 1 次 /interrupt，实际 %d", n)
	}
}

// 反过来钉住：正常结束的节点不许乱发 /interrupt（那会误杀同实例上的其它活）。
func TestNodeSuccess_DoesNotInterrupt(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/interrupt" {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e := newTestEngine()
	e.t2i = &Text2ImageService{BaseURL: srv.URL, H3BaseURL: srv.URL}

	nodes := marshalNodes(t, []map[string]interface{}{
		{"id": "s1", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "x"}},
		{"id": "e1", "type": "end", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{}},
	})
	edges := marshalEdges(t, []map[string]interface{}{
		{"id": "a", "source": "s1", "target": "e1"},
	})
	if _, err := e.Execute(nodes, edges); err != nil {
		t.Fatalf("正常图不该失败：%v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("正常结束不该发 /interrupt，实际 %d 次", n)
	}
}
