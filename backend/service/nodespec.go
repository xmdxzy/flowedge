package service

import (
	"strings"
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
	// ---- 控制流 ----
	"start": {Type: "start"},
	"end":   {Type: "end"},
	// 质量循环：每一轮都是完整生成，天然比单次生成贵 max_rounds 倍。
	// 上限 12 小时：默认 30 分钟会把合法的多轮生成循环误杀。
	"quality_loop": {Type: "quality_loop", Timeout: func(n *model.NodeInstance) time.Duration {
		rounds := 3
		if v, ok := n.Data["max_rounds"].(float64); ok && v > 0 {
			rounds = int(v)
		}
		return time.Duration(rounds) * 2 * time.Hour
	}, MaxTimeout: 12 * time.Hour},

	// ---- 语言侧：Ollama（规划与提示词优化，不产媒体）----
	"llm": {Type: "llm", Side: sideLanguage},
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
	// comfy 逃生舱：跑用户自备的 ComfyUI 图。图内容未知 ⇒ 按重节点处理
	//（采样显存峰值），超时默认 20 分钟、上限 2 小时（comfyNodeTimeout 内部夹紧）。
	"comfy": {Type: "comfy", Side: sideDiffusion, Heavy: true, Media: true,
		Timeout: comfyNodeTimeout, MaxTimeout: comfyMaxTimeout},

	// ---- 代码渲染：headless 浏览器逐帧截图，纯 CPU、零显存 ----
	// 不吃显存 ⇒ Side 为空、Heavy=false，队列按 CPU 道并行（见 queue.go 白名单）。
	// Media=true：它真的产出视频文件，血缘与产物清单要认。
	"code_video": {Type: "code_video", Media: true, Timeout: codeVideoTimeout, MaxTimeout: 30 * time.Minute},

	// ---- 语音侧：CPU，但与扩散权重抢内存带宽 ----
	"tts": {Type: "tts", Side: sideAudio, Media: true, Timeout: ttsTimeout},
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
//
// MOSS 后端另算（见 mossT2ITimeout）：它是 50 步模型，且可能要先把 8190 实例拉起来，
// 沿用 flux 的 4 分钟会把一张正常的 MOSS 图判成超时。
func text2ImageTimeout(n *model.NodeInstance) time.Duration {
	if isMOSST2INode(n) {
		return mossT2ITimeout(n)
	}
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

// isMOSST2INode 这个文生图节点会不会走 MOSS。
//
// 判据必须与 ResolveT2IBackend 一致（显式 moss，或 auto 且填了布局），
// 但这里**不看提示词关键词**：超时在拿到上游文本之前就要算，
// 而且按关键词猜会让"差一个词"就差 5 分钟超时，说不清。
// 命中关键词但没猜到的情况由 MaxTimeout 兜底，不会被误杀。
func isMOSST2INode(n *model.NodeInstance) bool {
	if n == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(fieldString(n, "backend"))) {
	case "moss":
		return true
	case "flux":
		return false
	}
	return strings.TrimSpace(fieldString(n, "layout")) != ""
}

// mossT2ITimeout MOSS 出图超时：首张 10 分钟，此后每张 4 分钟。
//
// 组成（都是实测或可外推的固定成本，不是拍脑袋）：
//
//	8190 实例冷启        ~130s（实测：988 节点注册 + torch 2.14 导入）
//	11GB 权重载入        ~60s
//	50 步 @1024² 采样    60–150s（官方口径，本机未实测，取上界）
//
// 首张 600s 覆盖「冷启 + 载入 + 采样」的最坏组合还留了一倍余量；
// 之后实例是热的，每张只算采样，240s 足够。
func mossT2ITimeout(n *model.NodeInstance) time.Duration {
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
	return time.Duration(600+(n2-1)*240) * time.Second
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
	// 预算按「帧 × 分辨率 × 步数」算，不是固定每镜 N 分钟：
	// 768×448@20 步的成本是 608×352@12 步的 2.5 倍，只按帧数会给少一半预算。
	return ShotListBudget(shots, totalFrames,
		int(fieldFloat(n, "width", 608)),
		int(fieldFloat(n, "height", 352)),
		int(fieldFloat(n, "steps", 12)))
}

// ttsTimeout 语音合成超时。
//
// 这个节点原先没有 Timeout，吃的是 defaultNodeTimeout = 180s——而 Breeze TTS 2
// （3.47B BF16）的**冷加载实测正好 180s**（2026-10-27 现场用 GPU Process Memory
// 逐 10 秒采样：3.0GB → 11.5GB → 19.9GB，loaded=true 用了 180s）。两数打平 ⇒
// 「服务被卸载后第一次请求」必然超时失败，报 node timeout after 3m0s（run #288 实发生）。
//
// 热合成很快（实测 5s/句），所以预算主要是买「模型不在显存里」这件事：
//
//	5 分钟基础  覆盖 180s 冷加载 + 服务进程拉起/就绪 + 首句
//	每字 200ms  长台词的自回归成本（Breeze 自回归，长文本容易跑飞）
//
// 上限由 maxNodeTimeout（30 分钟）兜底，长文本不会被误杀。
func ttsTimeout(n *model.NodeInstance) time.Duration {
	text := fieldString(n, "text")
	if text == "" {
		text = fieldString(n, "prompt")
	}
	return 5*time.Minute + time.Duration(len([]rune(text)))*200*time.Millisecond
}

// fieldString 读节点字符串字段（取不到返回空串，让调用方自己判空）。
func fieldString(n *model.NodeInstance, key string) string {
	if n == nil {
		return ""
	}
	if s, ok := n.Data[key].(string); ok {
		return s
	}
	return ""
}
