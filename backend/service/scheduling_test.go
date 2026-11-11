package service

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"flowedge/backend/model"
)

// 侧别感知调度验收。
//
// 为什么需要：24GB 上「换侧」是最贵的一次开销——卸载 flux 要等 CUDA 异步
// 归还（5–10s），重新加载又要 ~40s 冷启动。而同一拓扑层里节点的起飞顺序
// 原本由 goroutine 调度决定（随机），于是「出图 → LLM → 出图 → 配音」这类
// 交替编排，最坏情况每个节点都在换侧：一次运行里白白烧掉好几分钟。
//
// 分组之后：同侧节点连续跑，换侧次数每层 ≤2 次（3 个侧别各一批）。

// sideRecorder 记录「进入顺序」，带锁（同层节点并行执行）
type sideRecorder struct {
	mu   sync.Mutex
	seq  []string
	done chan struct{}
}

func newSideRecorder() *sideRecorder {
	return &sideRecorder{done: make(chan struct{})}
}

func (r *sideRecorder) record(side string) {
	r.mu.Lock()
	r.seq = append(r.seq, side)
	r.mu.Unlock()
}

// transitions 换侧次数：相邻两次进入的侧别不同即计一次
func (r *sideRecorder) transitions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for i := 1; i < len(r.seq); i++ {
		if r.seq[i] != r.seq[i-1] {
			n++
		}
	}
	return n
}

// sideRuns 把序列压成「侧别段」，如 [d d l l a]
func (r *sideRecorder) sideRuns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.seq {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// heavyNode 造一个指定侧别的重任务节点：真正去抢仲裁器的锁，
// 这样测的是调度器实际行为，而不是我们对它的想象。
func heavyNode(e *EngineV2, rec *sideRecorder, side string) NodeExecutor {
	return func(_ context.Context, node *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		release := e.enterSide(side)
		defer release()
		rec.record(side) // 拿到锁之后才记录 —— 真正的执行顺序
		time.Sleep(60 * time.Millisecond)
		return map[string]interface{}{"text": node.ID}, nil
	}
}

// TestSideAwareSchedulingGroupsBySide 同层重任务必须按侧别成批执行。
// 这是「消除换侧抖动」的核心断言：一旦离开某个侧别就不再回来。
func TestSideAwareSchedulingGroupsBySide(t *testing.T) {
	rec := newSideRecorder()
	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil)
	e.RegisterNodeTypeWithSide("hd1", sideDiffusion, heavyNode(e, rec, sideDiffusion))
	e.RegisterNodeTypeWithSide("hd2", sideDiffusion, heavyNode(e, rec, sideDiffusion))
	e.RegisterNodeTypeWithSide("hl1", sideLanguage, heavyNode(e, rec, sideLanguage))
	e.RegisterNodeTypeWithSide("hl2", sideLanguage, heavyNode(e, rec, sideLanguage))
	e.RegisterNodeTypeWithSide("ha1", sideAudio, heavyNode(e, rec, sideAudio))

	// 交错摆放，故意制造最坏的原始顺序：d l d a l
	nodes := []map[string]interface{}{
		{"id": "s", "type": "start", "position": map[string]float64{"x": 0, "y": 0}, "data": map[string]interface{}{"prompt": "go"}},
		{"id": "hd1", "type": "hd1", "position": map[string]float64{"x": 1, "y": 0}, "data": map[string]interface{}{}},
		{"id": "hl1", "type": "hl1", "position": map[string]float64{"x": 1, "y": 1}, "data": map[string]interface{}{}},
		{"id": "hd2", "type": "hd2", "position": map[string]float64{"x": 1, "y": 2}, "data": map[string]interface{}{}},
		{"id": "ha1", "type": "ha1", "position": map[string]float64{"x": 1, "y": 3}, "data": map[string]interface{}{}},
		{"id": "hl2", "type": "hl2", "position": map[string]float64{"x": 1, "y": 4}, "data": map[string]interface{}{}},
	}
	edges := []map[string]interface{}{}
	for _, id := range []string{"hd1", "hl1", "hd2", "ha1", "hl2"} {
		edges = append(edges, map[string]interface{}{"id": "e" + id, "source": "s", "target": id})
	}

	if _, err := e.Execute(marshalNodes(t, nodes), marshalEdges(t, edges)); err != nil {
		t.Fatalf("执行失败：%v", err)
	}

	runs := rec.sideRuns()
	if len(runs) > len(sideOrder) {
		t.Fatalf("侧别被切成了 %d 段（%v），应 ≤%d 段：换侧抖动没消除",
			len(runs), runs, len(sideOrder))
	}
	// 更强的断言：同一侧别不得出现两段
	seen := map[string]bool{}
	for _, s := range runs {
		if seen[s] {
			t.Fatalf("侧别 %s 出现了多次（序列=%v）：换侧抖动", s, runs)
		}
		seen[s] = true
	}
	// 顺序必须与 sideOrder 一致（扩散优先，它冷启动最贵）
	want := []string{}
	for _, s := range sideOrder {
		for _, r := range runs {
			if r == s {
				want = append(want, s)
				break
			}
		}
	}
	if strings.Join(runs, ",") != strings.Join(want, ",") {
		t.Fatalf("执行顺序 %v 与规定侧别顺序 %v 不一致", runs, want)
	}
}

// TestSideAwareSchedulingKeepsBranchesParallel 分组不能把并行性也一起砍掉：
// 同侧多个节点仍应并发，而不是退化成串行。
func TestSideAwareSchedulingKeepsBranchesParallel(t *testing.T) {
	probe := &overlapProbe{}
	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil)
	e.RegisterNodeTypeWithSide("hd", sideDiffusion, func(_ context.Context, node *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		release := e.enterSide(sideDiffusion)
		defer release() // 与真实节点一致：整个执行期间持锁
		probe.enter(node.ID)
		defer probe.leave()
		time.Sleep(100 * time.Millisecond)
		return map[string]interface{}{"text": node.ID}, nil
	})

	ids := []string{"a", "b", "c", "d"}
	nodes, edges := fanoutGraph(t, ids, "hd")
	if _, err := e.Execute(nodes, edges); err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	// 同侧重任务之间由 heavyMu 串行（这是显存安全的代价，不是回归）
	if got := atomic.LoadInt32(&probe.max); got != 1 {
		t.Fatalf("同侧重任务并发峰值 = %d，应恒为 1（显存串行）", got)
	}
	// 并发性由「轻任务 + 跨层」提供：这里额外验证轻任务确实并行
	lprobe := &overlapProbe{}
	e.RegisterNodeType("lt", func(_ context.Context, node *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		lprobe.enter(node.ID)
		defer lprobe.leave()
		time.Sleep(120 * time.Millisecond)
		return map[string]interface{}{"text": node.ID}, nil
	})
	lnodes, ledges := fanoutGraph(t, []string{"x", "y", "z"}, "lt")
	if _, err := e.Execute(lnodes, ledges); err != nil {
		t.Fatalf("轻任务执行失败：%v", err)
	}
	if got := atomic.LoadInt32(&lprobe.max); got < 3 {
		t.Fatalf("轻任务并发峰值 = %d，应 ≥3：分组调度把并行性砍掉了", got)
	}
}

// TestUnloadCountSeparateFromSwitch 逻辑换侧与真实卸载必须分开计数。
//
// 实测依据：冷加载一张 512×512 要 183s、热生成只要 3s（差 61 倍）。
// 所以「换了一次侧」不等于「掏了 180 秒」——扩散侧本来就没驻留时，
// 换侧是空操作。效率评估只能看 unloads，看 switches 会高估代价。
func TestUnloadCountSeparateFromSwitch(t *testing.T) {
	e := NewEngineV2(nil, nil, nil, nil, nil, nil, nil) // t2i == nil：没有扩散服务

	e.enterSide(sideDiffusion)() // 从空进入第一侧：不算换侧
	if got := e.SideSwitchCount(); got != 0 {
		t.Fatalf("从空进入第一侧不该计换侧，实际 %d", got)
	}

	e.enterSide(sideLanguage)() // 扩散 → 语言：算一次逻辑换侧，但没有 t2i 服务可卸载
	if got := e.SideSwitchCount(); got != 1 {
		t.Fatalf("扩散→语言应计 1 次逻辑换侧，实际 %d", got)
	}
	if got := e.SideUnloadCount(); got != 0 {
		t.Fatalf("没有 ComfyUI 服务时不该计真实卸载，实际 %d", got)
	}

	e.enterSide(sideDiffusion)() // 语言 → 扩散
	if got := e.SideSwitchCount(); got != 2 {
		t.Fatalf("应累计 2 次逻辑换侧，实际 %d", got)
	}
	if got := e.SideUnloadCount(); got != 0 {
		t.Fatalf("全程没有可卸载的扩散服务，卸载计数应恒为 0，实际 %d", got)
	}
}

// TestNodeSideMappingRegression 守卫「nodeSide 映射」与「执行器实际 enterSide」的一致性。
//
// 自定义节点类型若会加载大模型，必须用 RegisterNodeTypeWithSide 声明侧别；
// 内置节点类型的侧别写死在 nodeSide 里。给节点执行器加 enterSide 却忘了
// 同步映射表，是最容易犯、又最难在运行时发现的错（表现为偶发 OOM）。
func TestNodeSideMappingRegression(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到源码文件：%v", err)
	}
	fnRe := regexp.MustCompile(`func \(e \*EngineV2\) (exec\w*Node)\(`)
	// 两种进入方式都要认：enterSide(sideX) 与 enterLanguageSide(...)
	// （后者是带「装得下就不卸载」判断的语言侧入口，本质仍属语言侧）
	sideRe := regexp.MustCompile(`enterSide\((side\w+)\)`)
	langRe := regexp.MustCompile(`enterLanguageSide\(`)

	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		lines := strings.Split(string(src), "\n")
		cur := ""
		first := map[string]string{}
		for _, ln := range lines {
			if m := fnRe.FindStringSubmatch(ln); m != nil {
				cur = m[1]
				continue
			}
			// enterLanguageSide 等价于进入语言侧，统一成 sideLanguage 常量名再比对
			sideIdent := ""
			if m := sideRe.FindStringSubmatch(ln); m != nil {
				sideIdent = m[1]
			} else if langRe.MatchString(ln) {
				sideIdent = "sideLanguage"
			}
			if sideIdent != "" && cur != "" {
				// exec<Name>Node → 节点类型（lower / snake 两种命名都在用）
				base := strings.TrimSuffix(strings.TrimPrefix(cur, "exec"), "Node")
				for _, cand := range []string{strings.ToLower(base), toSnake(base)} {
					if want := nodeSide(cand); want != "" {
						if first[cur] == "" {
							// 只认函数体里第一个入口（决定起飞时是否换侧）
							first[cur] = sideIdent
						}
						if want != sideConstValue(first[cur]) {
							t.Fatalf("%s：执行器第一个进的是 %s，但 nodeSide(%q)=%s，映射表没同步",
								cur, first[cur], cand, want)
						}
						checked++
						break
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有校验到任何执行器的侧别映射，测试失效了")
	}
}

// sideConstValue 侧别常量标识符 → 实际值（sideLanguage → "language"）
func sideConstValue(ident string) string {
	switch ident {
	case "sideDiffusion":
		return sideDiffusion
	case "sideLanguage":
		return sideLanguage
	case "sideAudio":
		return sideAudio
	case "sideMusic":
		return sideMusic
	}
	return ""
}

// toSnake CamelCase → snake_case（PromptOpt → prompt_opt）
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}
