package service

import (
	"testing"
	"time"

	"flowedge/backend/model"
)

// NodeSpecs 是节点元数据的单一来源，所以它必须与注册表**完全一致**：
// 注册了却没有元数据 → 调度器当它不吃显存（偶发 OOM）；
// 有元数据却没注册 → 用户能选中一个跑不起来的节点（`unknown node type`）。
// 这条测试就是防这两类漂移。
func TestNodeSpecsCoverRegisteredTypes(t *testing.T) {
	e := &EngineV2{}
	e.registerBuiltinNodes()

	for typ := range e.nodes {
		if _, ok := NodeSpecs[typ]; !ok {
			t.Errorf("节点 %q 已注册执行体，但 NodeSpecs 里没有元数据（侧别/重节点/超时都会走兜底）", typ)
		}
	}
	for typ := range NodeSpecs {
		if _, ok := e.nodes[typ]; !ok {
			t.Errorf("NodeSpecs 声明了 %q，但没有注册执行体——前端会显示一个跑不起来的节点", typ)
		}
	}
}

// 超时算法从 switch 搬进 NodeSpecs 后，逐类核对一次，防止搬运时改错系数。
func TestNodeTimeoutBudgetPerType(t *testing.T) {
	mk := func(typ string, data map[string]interface{}) *model.NodeInstance {
		return &model.NodeInstance{ID: "n", Type: typ, Data: data}
	}

	// 文生图：240s + 每张 75s，取 prompts 条数与 batch 的较大者
	if got, want := nodeTimeoutFor(mk("text2image", map[string]interface{}{
		"prompts": "a\nb\nc", "batch": float64(2),
	})), time.Duration(240+3*75)*time.Second; got != want {
		t.Fatalf("text2image 批量预算 %v，期望 %v", got, want)
	}
	// H3 按段数 × 12 分钟
	if got, want := nodeTimeoutFor(mk("text2video", map[string]interface{}{
		"backend": "h3", "segments": float64(3),
	})), 36*time.Minute; got != want {
		t.Fatalf("H3 3 段预算 %v，期望 %v", got, want)
	}
	// 其余视频后端固定 25 分钟
	if got, want := nodeTimeoutFor(mk("text2video", nil)), 25*time.Minute; got != want {
		t.Fatalf("Wan 预算 %v，期望 %v", got, want)
	}
	// 分镜：12 镜 300 帧的预算必须能突破全局 30 分钟上限（否则长片必然被杀）
	p := ShotPlan{}
	for i := 0; i < 12; i++ {
		p.Shots = append(p.Shots, Shot{Frames: 25})
	}
	budget := nodeTimeoutFor(mk("shotlist", map[string]interface{}{"plan": toJSON(p)}))
	if budget <= maxNodeTimeout {
		t.Fatalf("12 镜预算 %v 不该低于全局上限 %v", budget, maxNodeTimeout)
	}
	if nodeMaxTimeoutFor("shotlist") != maxShotlistTimeout {
		t.Fatalf("分镜硬上限应为 %v，实际 %v", maxShotlistTimeout, nodeMaxTimeoutFor("shotlist"))
	}
	// 没有 plan（来自上游 storyboard）时给类型上限，运行期再细分
	if got := nodeTimeoutFor(mk("shotlist", nil)); got != maxShotlistTimeout {
		t.Fatalf("无 plan 时应给类型上限 %v，实际 %v", maxShotlistTimeout, got)
	}
	// 未登记类型走默认 180s
	if got := nodeTimeoutFor(mk("unknown_type", nil)); got != defaultNodeTimeout {
		t.Fatalf("未登记类型应给默认 %v，实际 %v", defaultNodeTimeout, got)
	}
}

// 血缘白名单现在由 NodeSpecs.Media 派生，这条锁住派生结果没漏掉视频管线主产物。
func TestMediaProducerDerivedFromSpecs(t *testing.T) {
	for _, typ := range []string{"text2image", "tts", "shotlist", "text2video", "upscale", "inpaint", "image2image"} {
		if !mediaProducerTypes[typ] {
			t.Errorf("%s 应标记 Media=true（否则产物没有血缘）", typ)
		}
	}
	for _, typ := range []string{"end", "start", "llm", "prompt_opt", "storyboard", "quality_loop"} {
		if mediaProducerTypes[typ] {
			t.Errorf("%s 不产出媒体文件，不该进血缘白名单", typ)
		}
	}
}
