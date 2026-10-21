package service

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// 真实端到端换侧验证。
//
// 目的：把「侧别感知调度到底省了多少」变成可引用的数字，而不是靠推断。
// 做法：同一张扇出图（2 个扩散 + 2 个语言，同层互不依赖）跑两遍，
// 一遍开分组调度、一遍关掉（旧行为：全部一次性起飞，顺序随机），
// 对比墙钟耗时与**真实换侧次数**。
//
// 为什么看换侧次数而不是只看耗时：墙钟受 GPU 争用、模型冷热影响，
// 抖动大；换侧次数是确定性指标——少一次就是省一次「卸载+等 CUDA 归还+冷加载」。
//
// 默认跳过（要 GPU + 两个服务在跑）。手动运行：
//
//	FLOWEDGE_REAL_BENCH=1 go test ./service/ -run TestRealSideSwitchBenchmark -v -timeout 40m
func TestRealSideSwitchBenchmark(t *testing.T) {
	if os.Getenv("FLOWEDGE_REAL_BENCH") != "1" {
		t.Skip("需要真实 ComfyUI(8188) + Ollama(11434)；设置 FLOWEDGE_REAL_BENCH=1 运行")
	}

	comfy := envOr("FLOWEDGE_COMFY", "http://127.0.0.1:8188")
	ollama := envOr("FLOWEDGE_OLLAMA", "http://127.0.0.1:11434")
	ckpt := envOr("FLOWEDGE_CKPT", "flux1-schnell-fp8.safetensors")
	llmModel := envOr("FLOWEDGE_LLM", "qwen2.5:14b")

	nodes := []map[string]interface{}{
		{"id": "s", "type": "start", "position": map[string]float64{"x": 0, "y": 0},
			"data": map[string]interface{}{"prompt": "go"}},
		{"id": "i1", "type": "text2image", "position": map[string]float64{"x": 1, "y": 0},
			"data": map[string]interface{}{"prompt": "a red cube on a plain white background", "width": 512, "height": 512}},
		{"id": "l1", "type": "llm", "position": map[string]float64{"x": 1, "y": 1},
			"data": map[string]interface{}{"prompt": "Reply with exactly: ok", "model": llmModel}},
		{"id": "i2", "type": "text2image", "position": map[string]float64{"x": 1, "y": 2},
			"data": map[string]interface{}{"prompt": "a blue sphere on a plain white background", "width": 512, "height": 512}},
		{"id": "l2", "type": "llm", "position": map[string]float64{"x": 1, "y": 3},
			"data": map[string]interface{}{"prompt": "Reply with exactly: done", "model": llmModel}},
	}
	edges := []map[string]interface{}{}
	for _, id := range []string{"i1", "l1", "i2", "l2"} {
		edges = append(edges, map[string]interface{}{"id": "e" + id, "source": "s", "target": id})
	}
	nodesJSON := marshalNodes(t, nodes)
	edgesJSON := marshalEdges(t, edges)

	type result struct {
		label    string
		wall     time.Duration
		switches int
		unloads  int
		perNode  map[string]time.Duration
		err      error
	}

	runOnce := func(label string, sideAware bool) result {
		e := NewEngineV2(
			NewLLMService(ollama, llmModel),
			NewText2ImageService(comfy, ckpt),
			nil, nil, nil, nil, nil, nil, nil,
		)
		e.SetSideAwareScheduling(sideAware)

		t.Logf("[%s] 开始（side-aware=%v）…", label, sideAware)
		start := time.Now()
		states, err := e.Execute(nodesJSON, edgesJSON)
		wall := time.Since(start)

		per := map[string]time.Duration{}
		for _, st := range states {
			if st.StartedAt != nil && st.EndedAt != nil {
				per[st.ID] = st.EndedAt.Sub(*st.StartedAt)
			}
		}
		r := result{label: label, wall: wall, switches: e.SideSwitchCount(),
			unloads: e.SideUnloadCount(), perNode: per, err: err}
		t.Logf("[%s] 墙钟 %.1fs，逻辑换侧 %d 次，真实卸载 %d 次，err=%v",
			label, wall.Seconds(), r.switches, r.unloads, err)
		return r
	}

	// 顺序颠倒开关：先跑谁会让后者「占便宜」（模型已驻留）。
	// 只跑一轮得出的耗时差不可信，正确做法是正序 + 倒序各跑一轮再看。
	plan := []struct {
		label string
		aware bool
	}{
		{"分组调度", true},
		{"旧行为(不分批)", false},
	}
	if os.Getenv("FLOWEDGE_BENCH_REVERSE") == "1" {
		plan[0], plan[1] = plan[1], plan[0]
		t.Logf("（本轮为倒序：先跑旧行为）")
	}

	var on, off result
	for _, p := range plan {
		r := runOnce(p.label, p.aware)
		if p.aware {
			on = r
		} else {
			off = r
		}
	}

	// 汇总
	report := func(r result) string {
		ids := make([]string, 0, len(r.perNode))
		for id := range r.perNode {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		parts := make([]string, 0, len(ids))
		for _, id := range ids {
			parts = append(parts, fmt.Sprintf("%s=%.1fs", id, r.perNode[id].Seconds()))
		}
		return strings.Join(parts, " ")
	}
	t.Logf("\n===== 对照结果 =====")
	t.Logf("分组调度 : 墙钟 %6.1fs  换侧 %d 次（真实卸载 %d 次）  [%s]",
		on.wall.Seconds(), on.switches, on.unloads, report(on))
	t.Logf("旧行为   : 墙钟 %6.1fs  换侧 %d 次（真实卸载 %d 次）  [%s]",
		off.wall.Seconds(), off.switches, off.unloads, report(off))
	t.Logf("说明：真实卸载才对应约 183s 的冷加载（实测冷加载 183s / 热生成 3s）。\n" +
		"      若两次的真实卸载次数相同，说明这张图本来就没触发多余的卸载，\n" +
		"      墙钟差异纯属 GPU 冷热噪声——换个更大的图才会体现差距。")
	// 结论必须如实写：换侧次数是确定性指标，墙钟不是。
	// 单次测量受 GPU 冷热、ComfyUI 模型缓存影响，±10% 以内的差异
	// 不足以宣称提速——宁可写「测不出来」，也不拿噪声当收益。
	if on.switches < off.switches {
		delta := off.wall - on.wall
		switch {
		case delta > 0 && delta.Seconds()/off.wall.Seconds() > 0.10:
			t.Logf("结论：少换侧 %d 次，墙钟快 %.1fs（%.0f%%）",
				off.switches-on.switches, delta.Seconds(), 100*delta.Seconds()/off.wall.Seconds())
		case delta < 0 && -delta.Seconds()/off.wall.Seconds() > 0.10:
			t.Logf("结论：少换侧 %d 次，但墙钟反而慢 %.1fs（%.0f%%）——说明这张图的瓶颈不在换侧",
				off.switches-on.switches, -delta.Seconds(), 100*(-delta.Seconds())/off.wall.Seconds())
		default:
			t.Logf("结论：少换侧 %d 次；墙钟差异 %.1fs 在 ±10%% 噪声内，单次测不出提速。\n"+
				"      换侧次数才是可复现的指标：同层 N 个扩散 + M 个语言时，\n"+
				"      随机顺序的期望换侧 ≈ 2NM/(N+M)，分组后恒为 1。节点越多差距越大。",
				off.switches-on.switches, delta.Seconds())
		}
	}

	// 硬断言：分组调度的换侧次数必须 ≤ 侧别数量-1（理想值是 1 次）。
	// 不做耗时断言：墙钟受 GPU 冷热影响，跑在 CI/负载机上必然不稳。
	if on.switches == 0 {
		t.Fatal("分组调度跑完却没有记录到任何换侧——计数器没接上或节点没走仲裁器")
	}
	if maxAllowed := len(sideOrder) - 1; on.switches > maxAllowed {
		t.Fatalf("分组调度换侧 %d 次，超过理论上限 %d（同侧节点没凑成批）",
			on.switches, maxAllowed)
	}
	if on.err != nil {
		t.Logf("注意：分组调度这次执行有错误（%v）——换侧数据仍有效，但耗时不代表成功路径", on.err)
	}
}

// TestRealImageLatencySplit 把「一张图的耗时」拆成「模型冷加载」与「真生成」。
//
// 动机：换侧基准里出现过 115.8s 的第一张图和紧随其后只多 3s 的第二张，
// 怀疑绝大部分时间花在加载 17GB 权重而不是扩散本身。
// 这个数字直接决定优化方向：若冷加载占绝对多数，那"少换一次侧"
// 远比"调采样参数"值钱；引擎也该优先保模型驻留而不是频繁换侧。
//
//	FLOWEDGE_REAL_BENCH=1 go test ./service/ -run TestRealImageLatencySplit -v -timeout 20m
func TestRealImageLatencySplit(t *testing.T) {
	if os.Getenv("FLOWEDGE_REAL_BENCH") != "1" {
		t.Skip("需要真实 ComfyUI(8188)；设置 FLOWEDGE_REAL_BENCH=1 运行")
	}
	comfy := envOr("FLOWEDGE_COMFY", "http://127.0.0.1:8188")
	ckpt := envOr("FLOWEDGE_CKPT", "flux1-schnell-fp8.safetensors")
	svc := NewText2ImageService(comfy, ckpt)

	// 先明确当前是否已有模型驻留，否则「第一张慢」的原因说不清
	before, _ := svc.FreeVRAMMB()
	t.Logf("开跑前 ComfyUI 可释放显存 = %d MB", before)

	var marks []float64
	for i, prompt := range []string{
		"a single small red dot on white",
		"a single small blue dot on white",
		"a single small green dot on white",
	} {
		start := time.Now()
		path, err := svc.GenerateImage(prompt, 512, 512)
		d := time.Since(start).Seconds()
		marks = append(marks, d)
		t.Logf("第 %d 张：%.1fs  err=%v  %s", i+1, d, err, path)
		if err != nil {
			t.Fatalf("出图失败：%v", err)
		}
	}
	if len(marks) >= 2 {
		first, rest := marks[0], marks[1:]
		avg := 0.0
		for _, v := range rest {
			avg += v
		}
		avg /= float64(len(rest))
		t.Logf("\n===== 拆分结果 =====")
		t.Logf("首张（含冷加载）：%.1fs", first)
		t.Logf("后续（已驻留）  ：%.1fs（均）", avg)
		if first > avg*2 {
			t.Logf("结论：冷加载约占首张耗时的 %.0f%%，权重驻留远比调采样参数值钱",
				100*(first-avg)/first)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
