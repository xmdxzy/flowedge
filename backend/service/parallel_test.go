package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"flowedge/backend/model"
)

// 并发度探针：记录「同时在执行」的节点数峰值。
// 用它把「并行是真的并行」和「重任务真的串行」变成可断言的数字，
// 而不是靠看日志猜。
type overlapProbe struct {
	cur  int32
	max  int32
	mu   sync.Mutex
	seen []string
}

func (p *overlapProbe) enter(id string) {
	n := atomic.AddInt32(&p.cur, 1)
	p.mu.Lock()
	p.seen = append(p.seen, id)
	p.mu.Unlock()
	for {
		m := atomic.LoadInt32(&p.max)
		if n <= m || atomic.CompareAndSwapInt32(&p.max, m, n) {
			break
		}
	}
}

func (p *overlapProbe) leave() { atomic.AddInt32(&p.cur, -1) }

// fanoutGraph 造一张「start → N 个并列节点」的扇出图，N 个节点同层。
func fanoutGraph(t *testing.T, ids []string, nodeType string) ([]byte, []byte) {
	t.Helper()
	ns := []map[string]interface{}{
		{"id": "s", "type": "start", "position": map[string]float64{"x": 0, "y": 0},
			"data": map[string]interface{}{"prompt": "go"}},
	}
	es := []map[string]interface{}{}
	for i, id := range ids {
		ns = append(ns, map[string]interface{}{
			"id": id, "type": nodeType, "position": map[string]float64{"x": float64(i + 1), "y": 0},
			"data": map[string]interface{}{},
		})
		es = append(es, map[string]interface{}{"id": "e" + id, "source": "s", "target": id})
	}
	return marshalNodes(t, ns), marshalEdges(t, es)
}

// TestParallelBranchesOverlap 同一层互不依赖的节点必须真的同时跑。
// 这是「并行分支执行」的最低验收：不重叠就说明退化成串行了。
func TestParallelBranchesOverlap(t *testing.T) {
	probe := &overlapProbe{}
	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	e.RegisterNodeType("fan", func(_ context.Context, node *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		probe.enter(node.ID)
		defer probe.leave()
		time.Sleep(200 * time.Millisecond)
		return map[string]interface{}{"text": node.ID}, nil
	})

	ids := []string{"a", "b", "c"}
	nodes, edges := fanoutGraph(t, ids, "fan")
	if _, err := e.Execute(nodes, edges); err != nil {
		t.Fatalf("并行执行失败：%v", err)
	}
	if got := atomic.LoadInt32(&probe.max); got < 3 {
		t.Fatalf("三个同层节点应并发执行，实测并发峰值 = %d（<3 说明退化成串行）", got)
	}
}

// TestMaxParallelCapsConcurrency 设了上限就不能超。
// 这条同时保护总闸本身：并发抖到 4 说明令牌没生效。
func TestMaxParallelCapsConcurrency(t *testing.T) {
	probe := &overlapProbe{}
	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	e.SetMaxParallel(2)
	e.RegisterNodeType("fan", func(_ context.Context, node *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		probe.enter(node.ID)
		defer probe.leave()
		time.Sleep(150 * time.Millisecond)
		return map[string]interface{}{"text": node.ID}, nil
	})

	ids := []string{"a", "b", "c", "d"}
	nodes, edges := fanoutGraph(t, ids, "fan")
	if _, err := e.Execute(nodes, edges); err != nil {
		t.Fatalf("受限并发执行失败：%v", err)
	}
	if got := atomic.LoadInt32(&probe.max); got > 2 {
		t.Fatalf("SetMaxParallel(2) 后并发峰值应 ≤2，实测 = %d", got)
	}
	if got := atomic.LoadInt32(&probe.max); got < 2 {
		t.Fatalf("上限设 2 却只跑到 %d，说明总闸把并发掐死了（不是限流而是串行）", got)
	}
}

// TestHeavyNodesSerializeOnVRAMArbiter 重任务（扩散/语言/语音）绝不能并发。
// 24G 卡上两个扩散节点同时起飞必然 OOM——这里用仲裁器的 heavyMu 证明它们被串行化。
// 该测试不依赖真实 ComfyUI：enterSide 在 t2i/llm 为 nil 时同样会抢锁。
func TestHeavyNodesSerializeOnVRAMArbiter(t *testing.T) {
	probe := &overlapProbe{}
	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	e.RegisterNodeType("heavy", func(_ context.Context, node *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		release := e.enterSide(sideDiffusion)
		defer release()
		probe.enter(node.ID)
		defer probe.leave()
		time.Sleep(120 * time.Millisecond)
		return map[string]interface{}{"text": node.ID}, nil
	})

	ids := []string{"a", "b", "c"}
	nodes, edges := fanoutGraph(t, ids, "heavy")
	if _, err := e.Execute(nodes, edges); err != nil {
		t.Fatalf("重任务执行失败：%v", err)
	}
	if got := atomic.LoadInt32(&probe.max); got != 1 {
		t.Fatalf("同层三个扩散节点必须显存串行，实测并发峰值 = %d（>1 会在 24G 卡上 OOM）", got)
	}
}
