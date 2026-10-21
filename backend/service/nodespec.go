package service

import (
	"time"

	"flowedge/backend/model"
)

// 节点元数据的**单一来源**。
//
// 拆这个文件的动因（docs/系统评估-2026-10-21.md §3 S1）：
// 节点类型原来散在 4–6 处 switch —— 超时、侧别、是否重节点、是否产出媒体，
// 每加一个节点要逐处去改，编译器不报错，漏一处就是「偶发 OOM」或「没有血缘」。
// 现在判定类元数据全部收在这张表里，执行体仍然在 engine_v2_nodes.go，
// 注册仍然走 registerBuiltinNodes（那张表不用改，它只关心执行函数）。
//
// 还有两类元数据**故意没有**在这里收口：
//   - 参数校验（nodeparams.go）：每条校验带人话修复建议，不是纯元数据；
//   - 依赖预检（preflight.go）：要区分 h3/主实例/CPU 服务，逻辑比数据多。
// 它们各自只有一处，不构成"加节点要改 N 个文件"的问题。

// NodeSpec 一个节点类型的静态属性。
type NodeSpec struct {
	Type string
	// Side 显存侧别：diffusion / language / audio，"" 表示不吃显存。
	// 必须与执行体里第一次 enterSide 的一致（scheduling_test.go 会扫源码比对）。
	Side string
	// Heavy 是否属于"重节点"：执行时需要采样显存峰值、写显存档案。
	Heavy bool
	// Media 是否真的产出媒体文件（血缘白名单）。选择器/透传节点必须为 false。
	Media bool
	// Timeout 按节点自身参数估算超时；nil 用 defaultNodeTimeout。
	Timeout func(*model.NodeInstance) time.Duration
	// MaxTimeout 该类型的硬上限；0 用 maxNodeTimeout。
	// 只有分镜执行器例外：长片本来就该跑几十分钟，30 分钟上限会杀掉合法任务。
	MaxTimeout time.Duration
}

var NodeSpecs = map[string]NodeSpec{
	// ---- 控制流：不占显存，不产媒体 ----
	"start":        {Type: "start"},
	"end":          {Type: "end"},
	"if_else":      {Type: "if_else"},
	"loop":         {Type: "loop"},
	"sub_workflow": {Type: "sub_workflow"},
	"aggregate":    {Type: "aggregate"},
	"code":         {Type: "code"},

	// ---- 数据/检索 ----
	"http_request":   {Type: "http_request"},
	"data_transform": {Type: "data_transform"},
	"rag_query":      {Type: "rag_query"},

	// ---- 语言侧：Ollama ----
	"llm":        {Type: "llm", Side: sideLanguage, Timeout: nil},
	"vision":     {Type: "vision", Side: sideLanguage},
	"prompt_opt": {Type: "prompt_opt", Side: sideLanguage},
	"scene_grid": {Type: "scene_grid", Side: sideLanguage},
	"select":     {Type: "select", Side: sideLanguage},
	"storyboard": {Type: "storyboard", Side: sideLanguage, Timeout: func(*model.NodeInstance) time.Duration {
		return 5 * time.Minute // LLM 规划 + JSON 校验
	}},
	// text2video 归语言侧：先由 LLM 优化提示词，再提交 ComfyUI（见 vram.go 旧注释）
	"text2video": {Type: "text2video", Side: sideLanguage, Heavy: true, Media: true, Timeout: text2VideoTimeout},

	// ---- 扩散侧：ComfyUI ----
	"text2image": {Type: "text2image", Side: sideDiffusion, Heavy: true, Media: true, Timeout: text2ImageTimeout},
	"image2image": {Type: "image2image", Side: sideDiffusion, Heavy: true, Media: true, Timeout: func(*model.NodeInstance) time.Duration {
		return 10 * time.Minute
	}},
	"inpaint": {Type: "inpaint", Side: sideDiffusion, Heavy: true, Media: true, Timeout: func(*model.NodeInstance) time.Duration {
		return 10 * time.Minute
	}},
	"upscale": {Type: "upscale", Side: sideDiffusion, Heavy: true, Media: true, Timeout: func(*model.NodeInstance) time.Duration {
		return 10 * time.Minute
	}},
	"shotlist": {Type: "shotlist", Side: sideDiffusion, Heavy: true, Media: true,
		Timeout: shotListTimeout, MaxTimeout: maxShotlistTimeout},
	// comfy_workflow 内部会 enterSide(sideDiffusion)，过去漏在映射表外，
	// 导致侧别感知调度把它和语言节点排在一起（真机表现为多一次 180s 冷加载）。
	"comfy_workflow": {Type: "comfy_workflow", Side: sideDiffusion, Heavy: true, Media: true},

	// ---- 语音侧：CPU，但与扩散权重抢内存带宽 ----
	"asr": {Type: "asr", Side: sideAudio, Timeout: func(*model.NodeInstance) time.Duration {
		return 15 * time.Minute // 长音频分段转写
	}},
	"tts": {Type: "tts", Side: sideAudio, Media: true},

	// ---- 后期 ----
	"video_compose": {Type: "video_compose", Media: true},
	// 字幕烧录吃 CPU（ffmpeg + libass），不占显存，产物是新的 mp4
	"subtitles":    {Type: "subtitles", Media: true},
	"quality_gate": {Type: "quality_gate"},
}

// NodeSpecOf 查节点元数据。未登记的类型返回零值 + false，
// 调用方应退回保守行为（当重节点处理、按默认超时）。
func NodeSpecOf(typ string) (NodeSpec, bool) {
	s, ok := NodeSpecs[typ]
	return s, ok
}

// nodeTimeoutFor 按节点类型与工作量估算超时。
//
// 固定 180s 在「批量出图」和「分段视频」上会误杀合法任务（实测 5 条提示词
// 需要 ~3 分钟、2 段 H3 视频需要 ~5 分钟）。每类节点的算法放在 NodeSpecs.Timeout 里，
// 这里只负责兜底。data.timeout_ms 仍可覆盖（见 ExecuteWithControl）。
func nodeTimeoutFor(n *model.NodeInstance) time.Duration {
	if s, ok := NodeSpecs[n.Type]; ok && s.Timeout != nil {
		return s.Timeout(n)
	}
	return defaultNodeTimeout
}

// nodeMaxTimeoutFor 该类型的硬上限；未声明则是全局 30 分钟。
func nodeMaxTimeoutFor(typ string) time.Duration {
	if s, ok := NodeSpecs[typ]; ok && s.MaxTimeout > 0 {
		return s.MaxTimeout
	}
	return maxNodeTimeout
}

// NodeSpecTypes 已登记元数据的类型（供能力自检 / 一致性测试比对注册表）。
func NodeSpecTypes() []string {
	out := make([]string, 0, len(NodeSpecs))
	for k := range NodeSpecs {
		out = append(out, k)
	}
	return out
}

// mediaProducerTypes 由 NodeSpecs.Media 派生，不再手写第二份名单。
// select 不在此列：它只从候选里挑一个已存在的文件，文件是上游产出的；
// 把它算成生产者会让同一件产物被后写的记录覆盖，"谁做的"指向错误节点。
var mediaProducerTypes = func() map[string]bool {
	m := make(map[string]bool, len(NodeSpecs))
	for k, s := range NodeSpecs {
		if s.Media {
			m[k] = true
		}
	}
	return m
}()

// text2ImageTimeout 首张含模型加载给 4 分钟，此后每张按 75 秒累计。
func text2ImageTimeout(n *model.NodeInstance) time.Duration {
	prompts := len(splitPromptLines(n.Data["prompts"]))
	batch := int(fieldFloat(n, "batch", 1))
	if batch < 1 {
		batch = 1
	}
	n2 := prompts
	if n2 < batch {
		n2 = batch
	}
	if n2 < 1 {
		n2 = 1
	}
	return time.Duration(240+n2*75) * time.Second
}

// text2VideoTimeout H3 按段数给（每段约 2–3 分钟），其余后端给 25 分钟。
func text2VideoTimeout(n *model.NodeInstance) time.Duration {
	if backend, _ := n.Data["backend"].(string); backend == "h3" {
		segs := int(fieldFloat(n, "segments", 1))
		if segs < 1 {
			segs = 1
		}
		return time.Duration(segs) * 12 * time.Minute
	}
	return 25 * time.Minute
}

// shotListTimeout 预算必须按「镜数 × 帧数」给，不能按固定每镜 N 分钟。
// 基准是 AGENTS §5.10 实测 0.85–0.93 秒/帧，且出现过单段 556s 的异常争用，
// 所以留余量而不是卡着平均数走。
func shotListTimeout(n *model.NodeInstance) time.Duration {
	shots, totalFrames := 0, 0
	if raw, ok := n.Data["plan"].(string); ok && raw != "" {
		if p, err := ParseShotPlan(raw); err == nil {
			shots = len(p.Shots)
			for _, sh := range p.Shots {
				totalFrames += sh.Frames
			}
		}
	}
	if shots == 0 {
		// 分镜表来自上游节点（storyboard），编译期读不到 → 给类型上限，运行期再细分
		return maxShotlistTimeout
	}
	return ShotListBudget(shots, totalFrames)
}
