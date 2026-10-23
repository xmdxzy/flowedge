package service

import "flowedge/backend/model"

// 统一模块契约（Module Contract）——「万物皆可模块」的唯一接口。
//
// 为什么需要这一层
// ----------------
// 以前引擎知道「哪些是图像节点、哪些是视频节点」：侧别表、血缘白名单、
// 估算分类、前端调色板分组各写一份，加一个新模态要在四处同时知道它。
// 那是**分类**，不是**模块**。
//
// 这里把节点降维成一个统一的三元组：
//
//	吃什么样的端口（Inputs） → 要什么参数（KeyParams） → 吐什么样的端口（Outputs）
//
// 「图像 / 视频 / 音频 / 文本」只作为 **PortKind** 出现一次，是端口的类型，
// 不是节点的类别。于是：
//   - 加一个模态 = 加一个 PortKind 值 + 一个契约条目，调度/校验/任务书全部自动跟上；
//   - 加一个逻辑 = 同样一个契约条目（if_else 的 out 里就有 branch 端口）；
//   - 图和模态可以任意首尾相接：`if_else.branch`（text）能进 `text2image.prompt`（text），
//     引擎不需要"允许或禁止"任何特定组合，只看端口类型。
//
// 与 NodeSpecs 的分工（不重复、不漂移）
// ------------------------------------
//
//	NodeSpecs   = 执行侧元数据：占不占显存（Side）、要不要采峰值（Heavy）、
//	              产不产媒体文件（Media）、超时多少。**调度器读它。**
//	Modules     = 表达侧元数据：叫什么名字、吃什么吐什么、关键参数的人话标签、
//	              能承载哪几层门。**任务书与前端读它。**
//
// 两张表由 TestModuleContractCoversRegistry 强制对齐：注册了执行体却没有契约，
// 或契约指向不存在的类型，都会红。**不许出现第三张名单。**

// PortKind 端口的数据类型。**模态在这里只出现一次**，不是节点的分类维度。
type PortKind string

const (
	PortText  PortKind = "text"  // 纯文本 / 提示词 / 台词
	PortImage PortKind = "image" // 单张或多张图片
	PortVideo PortKind = "video" // 视频文件
	PortAudio PortKind = "audio" // 音频文件
	PortJSON  PortKind = "json"  // 结构化数据（分镜表、九宫格、参数包）
	PortAny   PortKind = "any"   // 透传：结构由上游决定
)

// Port 模块的一个入/出端口。
type Port struct {
	Name string   `json:"name"` // 端口名，任务书里直接展示
	Kind PortKind `json:"kind"` // 端口类型
	// Required 是否必须有来源。false = 可留空，节点会退化到自身参数或上游任意文本。
	Required bool   `json:"required"`
	Desc     string `json:"desc"`
}

// ModuleRole 模块在数据流中的形状。**不含模态**——它只回答"端口长什么样"。
type ModuleRole string

const (
	RoleSource    ModuleRole = "source"    // 无输入，产生初始数据
	RoleTransform ModuleRole = "transform" // 吃数据吐数据（文本变换、检索、转写都在这）
	RoleGenerator ModuleRole = "generator" // 产出媒体文件（图/视/音）
	RoleControl   ModuleRole = "control"   // 控制流（分支、循环、子图）
	RoleGate      ModuleRole = "gate"      // 校验并可能阻断下游
	RoleSink      ModuleRole = "sink"      // 终点
)

// Requirement 一条本机能力依赖。
type Requirement struct {
	Dep   string // ollama / vision / tts / asr / comfyui / comfy_h3
	Label string // 人话名，用于「缺什么」的提示
}

// capabilityByDep 依赖名 → 能力探测结果。预检用它取状态，不再写类型分支。
var capabilityByDep = map[string]func(Capabilities) Capability{
	"ollama":   func(c Capabilities) Capability { return c.Ollama },
	"vision":   func(c Capabilities) Capability { return c.Vision },
	"tts":      func(c Capabilities) Capability { return c.TTS },
	"asr":      func(c Capabilities) Capability { return c.ASR },
	"comfyui":  func(c Capabilities) Capability { return c.ComfyUI },
	"comfy_h3": func(c Capabilities) Capability { return c.ComfyH3 },
}

// CapabilityFor 按依赖名取能力状态；未知依赖返回零值 + false。
func CapabilityFor(dep string, caps Capabilities) (Capability, bool) {
	f, ok := capabilityByDep[dep]
	if !ok {
		return Capability{}, false
	}
	return f(caps), true
}

// KeyParam 关键参数的人话标签。
//
// **故意不带 Default**：默认值住在引擎代码与 data/models.json 里，这里再写一份就是
// 第二份真相——"涡轮 LoRA 8 步 vs 实际 12 步"就是这么漂出来的。
// 任务书只展示"节点上真实写了的值"，标签从这里取；没登记标签的字段照样输出，归入「其他参数」。
type KeyParam struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Unit    string   `json:"unit,omitempty"`
	Options []string `json:"options,omitempty"` // 非空 = 枚举，任务书会写"只能取这些值之一"
	Help    string   `json:"help,omitempty"`
}

// ModuleContract 一个模块对外宣称的全部能力。
type ModuleContract struct {
	Type      string
	Title     string // 人话名。前端卡片标题、任务书标题都用它，**不许另外硬编码**
	Role      ModuleRole
	Inputs    []Port
	Outputs   []Port
	KeyParams []KeyParam
	// Requires 跑起来需要哪些本机能力（ollama / vision / tts / asr / comfyui / comfy_h3）。
	// 依赖预检读这里，**不再按节点类型 switch**：加一个模态只要在契约里写清楚它要什么服务，
	// 预检、任务书、模块目录自动跟上。
	Requires []Requirement
	// RequiresFor 动态覆盖 Requires（少数模块按参数切换依赖，例如视频后端 wan/h3）。
	// 非 nil 时以它为准；返回空切片表示"该配置下不需要任何外部服务"。
	RequiresFor func(*model.NodeInstance) []Requirement
	// GateLayers 该模块能承载/产出的门层（timing/identity/audio/technical/budget）。
	// 空 = 不参与门。任务书的「验收门」段完全由它派生，不写第二份清单。
	GateLayers []string
	// Agent 一句话给智能体：吃X吐Y、什么时候用、什么情况下别用。
	Agent string
	// Nondeterministic 该模块不承诺「同输入同产出」。
	//
	// 为什么必须在契约里而不是写在注释里：评测协议要求「可复现」才允许配对比较
	// （§5.5）。一旦图里混进不承诺复现的节点，整张图的 CR/GPR 就不能与确定性图
	// 直接比 —— 任务书要标出来，评测器要据此分组。靠人记记不住。
	Nondeterministic bool
}

// Modules 全部已注册节点的契约。新增节点必须在此登记，
// 否则 TestModuleContractCoversRegistry 会红——因为"没登记"不会报错，只会让任务书缺一段。
var Modules = map[string]ModuleContract{
	// ---------------- 入口 / 出口 ----------------

	"start": {
		Type: "start", Title: "开始", Role: RoleSource,
		Outputs: []Port{{"text", PortText, true, "初始输入（用户填的变量或触发器的 payload）"}},
		KeyParams: []KeyParam{
			{Key: "prompt", Label: "初始文本"},
		},
		Agent: "整条流水线的入口。它的 text 就是「用户这次要的内容」，下游任何吃文本的模块都能接。",
	},

	// 文件源：外部已有文件的入口。
	//
	// 四个输出端口是**互斥**的：运行时只填 kind 对应的那一个。
	// 为什么不合成一个 any 端口：any 会让下游的类型检查全部失效 ——
	// 一个 mp4 的路径接到吃 audio 的节点上，静态检查放行，ffmpeg 跑到一半才炸。
	"input_source": {
		Type: "input_source", Title: "文件源", Role: RoleSource,
		Outputs: []Port{
			{"text", PortText, false, "kind=text 时：文本内容"},
			{"image", PortImage, false, "kind=image 时：图片的绝对路径"},
			{"audio", PortAudio, false, "kind=audio 时：音频的绝对路径"},
			{"video", PortVideo, false, "kind=video 时：视频的绝对路径"},
		},
		KeyParams: []KeyParam{
			{Key: "kind", Label: "文件类型", Options: []string{"image", "audio", "video", "text"},
				Help: "留空则按扩展名推断。四个输出端口互斥，实际只输出这一类"},
			{Key: "path", Label: "文件路径", Help: "本地绝对路径；kind=text 时可留空改填「文本内容」"},
			{Key: "text", Label: "文本内容", Help: "仅 kind=text 时使用"},
		},
		Agent: "把已经存在的本地文件接进图里。目标里带「转写这段录音」「把这张图放大」「把这段视频改竖屏」时，" +
			"必须用它接在 asr / upscale / post_production / vision 前面 —— start 只输出文本，接不到媒体端口。" +
			"还不知道具体文件时把 path 写成「待提供」，图可以先搭起来，运行时它会明确报「还没给文件」。",
	},

	// 文本拆分：长文案切成多段（分别配音 / 分别配图 / 分批处理）。
	//
	// 为什么必须确定性：同一段文本两次切出同样的结果，产物才可复现。
	// 需要"按语义分幕"就先接一个 llm 节点改写文本，再来这里机械切分。
	"text_split": {
		Type: "text_split", Title: "文本拆分", Role: RoleTransform,
		Inputs:  []Port{{"text", PortText, true, "待拆分的文本（参数留空时取上游）"}},
		Outputs: []Port{
			{"text", PortText, true, "按分隔符拼回全文（下游可整体消费）"},
			{"parts", PortJSON, true, "分段数组（纯文本，可直接给 loop 的 items）"},
			{"count", PortJSON, true, "段数"},
		},
		KeyParams: []KeyParam{
			{Key: "mode", Label: "拆分方式", Options: []string{"by_sentence", "by_paragraph", "by_count", "by_length"},
				Help: "by_sentence 按句 / by_paragraph 按空行 / by_count 合并成 N 段 / by_length 每段不超过 N 字"},
			{Key: "count", Label: "段数", Help: "mode=by_count 时生效，默认 3"},
			{Key: "max_chars", Label: "每段字数上限", Help: "mode=by_length 时生效，默认 80"},
			{Key: "separator", Label: "拼接符", Help: "text 端口用这个把各段拼回去，默认换行"},
			{Key: "text", Label: "文本内容", Help: "留空则取上游文本"},
		},
		Agent: "把长文本切成几段。目标里出现「拆成 N 段」「分段配音」「每段不超过 X 字」「按段落分别处理」时用它。" +
			"切点永远落在句末（保留句号），不会从句中间断开。输出 parts 数组可直接接 loop 的 items 做逐段处理；" +
			"只想整体念一遍就接 text 端口。",
	},

	"end": {
		Type: "end", Title: "结束", Role: RoleSink,
		Inputs: []Port{{"any", PortAny, false, "任意上游产物，仅用于标记终点与汇总"}},
		Agent:  "显式终点。没有它引擎也会在末端节点结束后收尾，但加了之后任务书能写出「交付到这里」。",
	},

	// ---------------- 控制流（逻辑模块，与模态模块同一套端口规则） ----------------

	"if_else": {
		Type: "if_else", Title: "条件分支", Role: RoleControl,
		Inputs:  []Port{{"text", PortText, true, "被判断的文本"}},
		Outputs: []Port{{"text", PortText, true, "原样透传"}, {"branch", PortText, true, "走的分支名（true/false）"}},
		KeyParams: []KeyParam{
			{Key: "condition_type", Label: "判断方式", Options: []string{"contains", "equals", "length_gt", "regex"}},
			{Key: "compare_value", Label: "比较值"},
		},
		Agent: "文本条件分流。两个出口都是文本，所以真假两支可以接任意模态模块。",
	},

	"loop": {
		Type: "loop", Title: "循环", Role: RoleControl,
		Inputs:  []Port{{"text", PortText, false, "循环体输入"}},
		Outputs: []Port{{"text", PortText, true, "末次结果"}, {"all", PortJSON, true, "全部迭代结果"}, {"iterations", PortJSON, true, "迭代次数"}},
		KeyParams: []KeyParam{
			// workflow_id 以前漏声明：执行器必须读它，任务书却没提，
			// 于是面板上填不了、compose 也不知道它需要 —— 画出来的 loop 必然跑不起来。
			{Key: "workflow_id", Label: "要循环的工作流 id",
				Help: "必填。必须是已保存的工作流；构图阶段还没有这个 id，所以那时不能用 loop"},
			{Key: "mode", Label: "循环方式", Options: []string{"count", "while", "foreach"}},
			{Key: "iterations", Label: "次数"},
			{Key: "max_iterations", Label: "最大次数（防死循环）"},
			{Key: "separator", Label: "拼接分隔符"},
		},
		Agent: "对同一段子图重复执行。all 端口给的是数组，下游可以用 aggregate 收成一段文本。" +
			"**必须先在 workflow_id 里指定一条已存的工作流** —— 它引用子流程，不是内联子图。" +
			"所以构图（生成新图）阶段用不了它：那时还没有可引用的 id，画出来会成环。" +
			"要「逐段处理」请用 text_split 的 parts 接下游，或在节点数上限内顺序展开。",
	},

	"sub_workflow": {
		Type: "sub_workflow", Title: "子工作流", Role: RoleControl,
		Inputs:  []Port{{"any", PortAny, false, "传给子工作流的 vars"}},
		Outputs: []Port{{"any", PortAny, true, "子工作流的最终输出"}},
		KeyParams: []KeyParam{
			{Key: "workflow_id", Label: "引用的工作流 id"},
			{Key: "workflow_name", Label: "引用的工作流名"},
		},
		Agent: "把另一条已保存的工作流当模块调用。当前是黑盒引用：内部节点不出现在本任务书里，需要单独取它的 spec。",
	},

	// ---------------- 数据 / 检索 ----------------

	"http_request": {
		Type: "http_request", Title: "HTTP 请求", Role: RoleTransform,
		Inputs:  []Port{{"text", PortText, false, "请求体文本（也可用 body 参数）"}},
		Outputs: []Port{{"text", PortText, true, "响应文本"}, {"json", PortJSON, true, "响应 JSON"}, {"status_code", PortJSON, true, "状态码"}},
		KeyParams: []KeyParam{
			{Key: "method", Label: "方法", Options: []string{"GET", "POST", "PUT", "DELETE"}},
			{Key: "url", Label: "地址"},
			{Key: "credential", Label: "凭据名"},
			{Key: "body", Label: "请求体"},
		},
		Agent: "调外部服务。离线优先的定位下只用于本机服务或内网接口，不要指望它替代 SaaS 连接器。",
	},

	"data_transform": {
		Type: "data_transform", Title: "数据转换", Role: RoleTransform,
		Inputs:  []Port{{"text", PortText, true, "待转换文本"}},
		Outputs: []Port{{"text", PortText, true, "转换结果"}},
		KeyParams: []KeyParam{
			{Key: "mode", Label: "转换方式"},
		},
		Agent: "纯文本变换（截断/替换/格式化），不调模型、不吃显存。能用它解决的不要动 LLM。",
	},

	"rag_query": {
		Type: "rag_query", Title: "知识库检索", Role: RoleTransform,
		Inputs:  []Port{{"text", PortText, true, "查询语句"}},
		Outputs: []Port{{"text", PortText, true, "拼接后的上下文"}, {"json", PortJSON, true, "命中片段与来源"}, {"hits", PortJSON, true, "命中数"}},
		KeyParams: []KeyParam{
			{Key: "query", Label: "查询（留空用上游文本）"},
			{Key: "top_k", Label: "召回条数"},
		},
		Agent: "从本地知识库捞片段拼进上下文。它只负责「找材料」，不负责「写答案」。",
	},

	"aggregate": {
		Type: "aggregate", Title: "变量聚合", Role: RoleTransform,
		Inputs:  []Port{{"any", PortAny, true, "任意数量的上游产物"}},
		Outputs: []Port{{"text", PortText, true, "拼接后的文本"}, {"json", PortJSON, true, "原始数组"}, {"count", PortJSON, true, "聚合条数"}},
		KeyParams: []KeyParam{
			{Key: "mode", Label: "聚合方式"},
			{Key: "field", Label: "取哪个字段"},
			{Key: "inputs", Label: "显式指定的输入"},
			{Key: "separator", Label: "拼接分隔符"},
			{Key: "include_empty", Label: "包含空值"},
		},
		Agent: "把多条并发分支收成一段。它是「并行跑完再合流」的标准做法，不要用循环硬凑。",
	},

	// ---------------- 语言侧（端口全是 text，与模态无关） ----------------

	"llm": {
		Type: "llm", Title: "大语言模型", Role: RoleTransform,
		Requires: []Requirement{{Dep: "ollama", Label: "LLM（Ollama）"}},
		Inputs:   []Port{{"text", PortText, true, "提示词或上游文本"}},
		Outputs:  []Port{{"text", PortText, true, "模型输出"}},
		KeyParams: []KeyParam{
			{Key: "model", Label: "模型"},
			{Key: "llm_profile", Label: "自定义模型端点"},
			{Key: "task_preset", Label: "任务预设", Options: []string{"image", "translate", "storyboard", "summary", "custom"}},
			{Key: "prompt", Label: "系统提示词（自定义任务时）"},
			{Key: "tools", Label: "可调用的工具"},
			{Key: "max_tool_rounds", Label: "工具最多轮数"},
			{Key: "tool_strict", Label: "工具失败即中止"},
			{Key: "binding", Label: "能力卡片"},
		},
		Agent: "通用文本变换。纯转换（翻译/优化/拆解）必须 temperature=0 才可复现；创作类才给多样性。",
	},

	"vision": {
		Type: "vision", Title: "视觉理解", Role: RoleTransform,
		Requires: []Requirement{{Dep: "vision", Label: "视觉模型"}},
		Inputs:   []Port{{"image", PortImage, true, "要看的图"}, {"text", PortText, false, "提问"}},
		Outputs:  []Port{{"text", PortText, true, "看图结论"}},
		KeyParams: []KeyParam{
			{Key: "prompt", Label: "提问"},
			{Key: "model", Label: "视觉模型"},
			{Key: "image_path", Label: "图片路径（留空取上游）"},
		},
		Agent: "看图说话。产出是文本，所以可以直接接 llm 做二次判断，或接 if_else 做质量分流。",
	},

	"prompt_opt": {
		Type: "prompt_opt", Title: "提示词优化", Role: RoleTransform,
		Inputs:  []Port{{"text", PortText, true, "原始描述"}},
		Outputs: []Port{{"text", PortText, true, "优化后的提示词"}, {"original", PortText, true, "原文"}},
		KeyParams: []KeyParam{
			{Key: "mode", Label: "优化方向", Options: []string{"image", "video", "translate"}},
			{Key: "seconds", Label: "目标时长（视频模式）", Unit: "s"},
		},
		Agent: "把中文想法改写成模型能吃的英文提示词。必须确定性（temperature=0），否则同一张图两次结果不一致。",
	},

	"scene_grid": {
		Type: "scene_grid", Title: "场景九宫格", Role: RoleTransform,
		Inputs:  []Port{{"text", PortText, true, "场景描述"}},
		Outputs: []Port{{"text", PortText, true, "SCENE LOCK 句"}, {"lock", PortText, true, "锁句（与 text 相同，供下游判定）"}},
		KeyParams: []KeyParam{
			{Key: "cells", Label: "九宫格逐格事实"},
			{Key: "idea", Label: "一句话概念"},
			{Key: "light", Label: "光线"},
			{Key: "palette", Label: "色调"},
			{Key: "depth", Label: "景深"},
		},
		Agent: "把场景拆成 3×3 物理事实，编译成一句 SCENE LOCK。**下游必须逐字注入，改一个字节接缝就换场地。**",
	},

	"storyboard": {
		Type: "storyboard", Title: "分镜规划", Role: RoleTransform,
		Requires: []Requirement{{Dep: "ollama", Label: "LLM（Ollama）"}},
		Inputs:   []Port{{"text", PortText, true, "剧本 / 创意"}},
		Outputs:  []Port{{"plan", PortJSON, true, "分镜表 JSON"}, {"shots", PortJSON, true, "镜头数组"}},
		KeyParams: []KeyParam{
			{Key: "brief", Label: "创作要求"},
			{Key: "seconds", Label: "目标总时长", Unit: "s"},
			{Key: "max_shots", Label: "最大镜数"},
		},
		Agent: "LLM 拆镜，产出 plan JSON。它只出计划，不出画面；画面由 shotlist 执行。",
	},

	"select": {
		Type: "select", Title: "择优挑选", Role: RoleTransform,
		Inputs:  []Port{{"image", PortImage, true, "候选图"}},
		Outputs: []Port{{"image", PortImage, true, "选中图"}, {"json", PortJSON, true, "候选与得分"}},
		KeyParams: []KeyParam{
			{Key: "reference", Label: "参考图"},
			{Key: "criteria", Label: "评分标准"},
			{Key: "threshold", Label: "通过阈值"},
			{Key: "model", Label: "评审模型"},
		},
		Agent: "多张候选里挑一张，并把分数留下。产物文件是上游的，**它不是产物生产者**（血缘会指向上游）。",
	},

	// ---------------- 扩散侧（生成媒体，端口声明 kind） ----------------

	"text2image": {
		Type: "text2image", Title: "文生图", Role: RoleGenerator,
		Requires: []Requirement{{Dep: "comfyui", Label: "ComfyUI（图像）"}},
		Inputs:   []Port{{"text", PortText, false, "提示词（可来自上游或 prompts 批量）"}},
		Outputs:  []Port{{"image", PortImage, true, "图片"}, {"images", PortJSON, true, "多图数组"}, {"text", PortText, true, "实际使用的提示词"}},
		KeyParams: []KeyParam{
			{Key: "backend", Label: "出图后端", Options: T2IBackends,
				Help: "auto=按提示词与布局自动选（推荐）；flux=4 步最快，不擅长图内文字；moss=MOSS-Image，能渲染图内文字并按 bbox 摆位，代价是 50 步（慢一个数量级）"},
			{Key: "layout", Label: "布局（仅 MOSS）",
				Help: "JSON 数组：[{type:\"text\",text:\"要渲染的字\",bbox:[y0,x0,y1,x1]},{type:\"object\",desc:\"描述\",bbox:[...]}]，坐标 0-1000，原点左上。也可由上游 JSON 端口喂进来"},
			{Key: "negative", Label: "负面提示词"},
			{Key: "steps", Label: "步数", Help: "留空=按后端默认（flux 4 / MOSS 50）"},
			{Key: "cfg", Label: "引导系数", Help: "留空=按后端默认（flux 1.0 / MOSS 3.0）"},
			{Key: "split_lines", Label: "上游文本按行拆成多条"},
			{Key: "translate_mode", Label: "中文翻译", Options: []string{"off", "auto", "force"},
				Help: "auto 只在语言模型已驻留时翻，避免为翻译多付一次换侧。MOSS 原生吃中文，一般不用翻"},
			{Key: "width", Label: "宽", Unit: "px"},
			{Key: "height", Label: "高", Unit: "px"},
			{Key: "batch", Label: "每条约几张"},
			{Key: "binding", Label: "能力卡片"},
		},
		Agent: "批量提示词在同一次模型加载里跑完，不要写成循环——循环会让每张都重新加载 17GB 权重。" +
			"图里要有可读的文字（海报、招牌、字幕板、封面标题）时把 backend 设为 moss：flux 画不出字，只会画出像字的笔画。",
	},

	"image2image": {
		Type: "image2image", Title: "图生图", Role: RoleGenerator,
		Requires: []Requirement{{Dep: "comfyui", Label: "ComfyUI（图像）"}},
		Inputs:   []Port{{"image", PortImage, true, "原图"}},
		Outputs:  []Port{{"image", PortImage, true, "结果图"}},
		KeyParams: []KeyParam{
			{Key: "image_path", Label: "原图路径（留空取上游）"},
			{Key: "strength", Label: "改动幅度", Help: "0=不变，1=完全重画"},
			{Key: "prompt", Label: "改写成什么"},
		},
		Agent: "保留构图改风格/细节。改幅度越大越容易丢人物一致性。",
	},

	"inpaint": {
		Type: "inpaint", Title: "局部重绘", Role: RoleGenerator,
		Requires: []Requirement{{Dep: "comfyui", Label: "ComfyUI（图像）"}},
		Inputs:   []Port{{"image", PortImage, true, "原图"}, {"image", PortImage, false, "蒙版（也可填 mask_path）"}},
		Outputs:  []Port{{"image", PortImage, true, "结果图"}},
		KeyParams: []KeyParam{
			{Key: "image_path", Label: "原图路径（留空取上游）"},
			{Key: "mask_path", Label: "蒙版路径", Help: "白色区域被重画"},
			{Key: "model", Label: "模型"},
		},
		Agent: "只改蒙版区域。没有蒙版必失败（保存期就拦）。",
	},

	"upscale": {
		Type: "upscale", Title: "超分放大", Role: RoleGenerator,
		Requires: []Requirement{{Dep: "comfyui", Label: "ComfyUI（图像）"}},
		Inputs:   []Port{{"image", PortImage, true, "原图"}},
		Outputs:  []Port{{"image", PortImage, true, "放大图"}, {"width", PortJSON, true, "输出宽"}, {"height", PortJSON, true, "输出高"}},
		KeyParams: []KeyParam{
			{Key: "image_path", Label: "原图路径（留空取上游）"},
			{Key: "model", Label: "超分模型"},
		},
		Agent: "放大不重画。它不理解画面内容，别指望修细节语义错误。",
	},

	"text2video": {
		Type: "text2video", Title: "文生视频", Role: RoleGenerator,
		// 后端决定依赖：h3 必须走 8189 独立实例，其余走主实例。
		// 这是「一个模块两种服务」的唯一特例，所以用函数而不是静态列表——
		// 但判定仍写在契约里，预检不需要知道 text2video 是什么。
		RequiresFor: func(n *model.NodeInstance) []Requirement {
			if backend, _ := n.Data["backend"].(string); backend == "h3" {
				return []Requirement{{Dep: "comfy_h3", Label: "ComfyUI-H3（音视频联合生成）"}}
			}
			return []Requirement{{Dep: "comfyui", Label: "ComfyUI（视频）"}}
		},
		Inputs:  []Port{{"image", PortImage, false, "首帧（有则图生视频）"}, {"text", PortText, false, "镜头描述"}},
		Outputs: []Port{{"video", PortVideo, true, "视频"}, {"text", PortText, true, "实际提示词"}, {"audio_muxed", PortAudio, false, "是否已混音轨"}},
		KeyParams: []KeyParam{
			{Key: "backend", Label: "后端", Options: []string{"wan", "h3"}},
			{Key: "width", Label: "宽", Unit: "px"},
			{Key: "height", Label: "高", Unit: "px"},
			{Key: "frames", Label: "帧数"},
			{Key: "fps", Label: "帧率", Unit: "fps"},
			{Key: "steps", Label: "步数", Help: "低于 12 H3 音频会乱语"},
			{Key: "segments", Label: "分段数"},
			{Key: "turbo", Label: "涡轮 LoRA", Help: "20 步 → 12 步"},
			{Key: "low_vram", Label: "低显存模式", Help: "逃生开关，不降峰值，别当常规优化"},
			{Key: "prompt_opt", Label: "先做提示词优化"},
			{Key: "scene_grid", Label: "先做九宫格锁"},
			{Key: "ref_images", Label: "参考图"},
			{Key: "ref_audio", Label: "参考音频"},
		},
		Agent: "H3 必须走 8189 独立实例；主实例（torch 2.6）跑同一张图必 OOM。段数与帧数直接决定墙钟。",
	},

	"comfy_workflow": {
		Type: "comfy_workflow", Title: "ComfyUI 工作流", Role: RoleGenerator,
		Inputs:  []Port{{"any", PortAny, false, "绑定变量（喂给 workflow 里的 placeholders）"}},
		Outputs: []Port{{"image", PortImage, false, "图（若该图产出图）"}, {"video", PortVideo, false, "视频（若该图产出视频）"}},
		KeyParams: []KeyParam{
			{Key: "workflow_file", Label: "工作流文件"},
			{Key: "workflow", Label: "内联 workflow JSON"},
			{Key: "bindings", Label: "变量绑定"},
			{Key: "timeout_sec", Label: "超时", Unit: "s"},
		},
		Agent: "执行 ComfyUI「导出(API)」格式的 JSON。UI 格式会被明确拒绝——先让对方按 API 格式导出。",
	},

	"shotlist": {
		Type: "shotlist", Title: "分镜执行", Role: RoleGenerator,
		Requires: []Requirement{{Dep: "comfy_h3", Label: "ComfyUI-H3（音视频联合生成）"}},
		Inputs: []Port{
			{"plan", PortJSON, false, "分镜表（留空取上游 storyboard）"},
			{"image", PortImage, false, "定妆图 / 参考图"},
			{"audio", PortAudio, false, "参考音频"},
		},
		Outputs: []Port{{"video", PortVideo, true, "成片"}, {"shots", PortJSON, true, "每镜实际结果"}, {"seconds", PortJSON, true, "实际时长"}, {"parts", PortVideo, false, "逐镜产物（有序，一致性门按序比对）"}, {"plan", PortJSON, false, "分镜表回传（字幕排轴用）"}, {"gate_report", PortJSON, false, "五层门检报告"}},
		KeyParams: []KeyParam{
			{Key: "plan", Label: "分镜表 JSON"},
			{Key: "ref_images", Label: "定妆图 / 参考图", Help: "覆盖 plan.characters；身份由图像锁，每镜走 ref2va"},
			{Key: "turbo", Label: "涡轮 LoRA", Help: "20 步 → 12 步；低于 12 音频会乱语"},
			{Key: "audio_check", Label: "氛围段人声抽检（有人声就中置相消）", Help: "台词段不走这条——那会消掉台词本身；台词段改为 ASR 验词 + 按门重试"},
			{Key: "ambience_gain", Label: "环境声混入增益", Help: "念稿音轨里底噪的电平，默认 0.85"},
			{Key: "require_gates", Label: "强制要求 gates", Help: "开：缺 gates 直接拦；关（默认）：缺 gates 只告警，兼容旧结构化分镜表"},
			{Key: "interp", Label: "成片插帧到 48fps"},
		},
		GateLayers: []string{"timing", "identity", "audio", "technical", "budget"},
		Agent:      "一条片子的主执行器。**它承载全部五层门**，所以任务书里必须把 plan.gates 原样带上，不许重编。",
	},

	// ---------------- 语音侧 ----------------

	"asr": {
		Type: "asr", Title: "语音转写", Role: RoleTransform,
		Requires: []Requirement{{Dep: "asr", Label: "ASR（faster-whisper）"}},
		Inputs:   []Port{{"audio", PortAudio, true, "要转写的音频"}},
		Outputs:  []Port{{"text", PortText, true, "转写文本"}, {"model", PortText, true, "实际模型"}},
		KeyParams: []KeyParam{
			{Key: "audio_path", Label: "音频路径（留空取上游）"},
			{Key: "model", Label: "模型"},
			{Key: "device", Label: "设备", Options: []string{"cpu", "cuda"}},
		},
		Agent: "默认 CPU。**服务不可达时返回空文本，看起来像「没人说话」**——验收时必须区分「不可达」与「空结果」。",
	},

	"tts": {
		Type: "tts", Title: "语音合成", Role: RoleGenerator,
		Requires: []Requirement{{Dep: "tts", Label: "TTS（Kokoro / Audio8）"}},
		Inputs:   []Port{{"text", PortText, true, "台词 / 旁白"}},
		Outputs:  []Port{{"audio", PortAudio, true, "音频"}, {"text", PortText, true, "实际配音文本"}},
		KeyParams: []KeyParam{
			{Key: "text", Label: "文本（留空取上游）"},
			{Key: "voice", Label: "音色（Kokoro 音色名 / Audio8 已注册音色）"},
			{Key: "rate", Label: "语速（仅 Kokoro 生效）"},
			{Key: "device", Label: "设备", Options: []string{"cpu", "cuda"}},
		},
		Agent: "后端由 TTS_BASE_URL 决定：Kokoro（默认 :8102，固定音色 + 语速）或 Audio8-TTS（:8103，零样本克隆 + 44.1kHz，无语速）。" +
			"Audio8 可用后端有两个：0.1B ONNX（纯 CPU，本机实测产出无效语音，勿用）与 0.6B PyTorch（CUDA，实测音色克隆 MFCC 相似度 0.993）。" +
			"克隆用法：先 POST /voices/register 注册音色，再把 voice 填成该音色名。" +
			"两者都会在每句开头带静音——排轴要按「语音实际起点」核，不能只看容器时长。",
	},

	// ---------------- 后期（CPU，一次重编码） ----------------

	"video_compose": {
		Type: "video_compose", Title: "视频合成", Role: RoleTransform,
		Inputs:  []Port{{"image", PortImage, true, "画面（图序列）"}, {"audio", PortAudio, false, "音轨"}},
		Outputs: []Port{{"video", PortVideo, true, "合成视频"}},
		Agent:   "只做容器级合成（图 + 音 → mp4）。竖屏/BGM/字幕请走 post_production，别在这里叠三层。",
	},

	"subtitles": {
		Type: "subtitles", Title: "字幕烧录", Role: RoleTransform,
		Inputs:  []Port{{"video", PortVideo, true, "成片"}, {"text", PortText, false, "台词（逐行）"}},
		Outputs: []Port{{"video", PortVideo, true, "带硬字幕的成片"}},
		KeyParams: []KeyParam{
			{Key: "video_path", Label: "视频路径（留空取上游）"},
			{Key: "srt_path", Label: "现成 srt 路径"},
			{Key: "plan", Label: "分镜表（按台词排轴）"},
			{Key: "dialogues", Label: "逐行台词"},
		},
		Agent: "仍是「按期望秒数排轴」，没有 ASR 强制对齐。台词与画面对不上时先怀疑时间轴，不是字体。",
	},

	"post_production": {
		Type: "post_production", Title: "后期包装", Role: RoleTransform,
		Inputs:  []Port{{"video", PortVideo, true, "成片"}, {"audio", PortAudio, false, "BGM"}},
		Outputs: []Port{{"video", PortVideo, true, "包装后成片"}},
		KeyParams: []KeyParam{
			{Key: "aspect", Label: "画幅", Options: []string{"keep", "9:16", "16:9", "1:1"}},
			{Key: "fit", Label: "适配方式", Options: []string{"blur", "crop", "pad"}},
			{Key: "bgm", Label: "BGM 路径"},
			{Key: "bgm_gain", Label: "BGM 增益"},
			{Key: "duck", Label: "侧链闪避"},
			{Key: "loudness", Label: "响度归一"},
		},
		// audio 层：loudness 两遍法归一是它承载的音频门动作（vocabulary 里
		// audio.loudness 说的就是它）。只写 technical 会让任务书把它判成裸门。
		GateLayers: []string{"technical", "audio"},
		Agent:      "竖屏 + BGM + 字幕必须一次做完（一条 filter_complex）。拆成三个节点 = 三次 libx264，画质掉三轮。",
	},

	// ---------------- 外部智能体 ----------------

	"agent_task": {
		Type: "agent_task", Title: "外部智能体", Role: RoleTransform,
		Nondeterministic: true,
		Inputs:           []Port{{"text", PortText, false, "任务描述（节点没写 prompt 时用它）"}},
		Outputs: []Port{
			{"text", PortText, true, "智能体的产出"},
			{"info", PortJSON, true, "agent id / 退出码 / 耗时 / 是否截断"},
		},
		KeyParams: []KeyParam{
			{Key: "agent_id", Label: "智能体", Help: "来自 data/agents.json 的名册；节点只能选 id，命令行只存在本机文件里"},
			{Key: "prompt", Label: "任务描述", Help: "留空则用上游文本。CLI 会以参数或 stdin 传入"},
			{Key: "skill_refs", Label: "技能包参考", Help: "逗号分隔的包名（如 ue5-ops,kaggle-ml-ops）。执行前把 SKILL.md 与参考文献的**路径**写进任务描述，让智能体自己去读——不复制内容，所以不会出现两份说法"},
			{Key: "timeout_ms", Label: "超时", Unit: "ms"},
			{Key: "workdir", Label: "工作目录", Help: "留空则每次执行用独立临时目录，避免并发互相覆盖文件"},
			{Key: "allow_failure", Label: "失败也往下传", Help: "打开后非零退出不阻断，但 info.exit_code 会如实记录"},
		},
		Agent: "把需要判断的活儿（写文案/改代码/查资料/审片意见）交给外部智能体。" +
			"**不承诺可复现**：同一输入两次结果可能不同，评测时须单独分组。" +
			"出图/转写/合成/验收不要用它 —— 那些有确定性模块，更便宜也更稳。",
	},

	// ---------------- 外部工具（MCP） ----------------

	// mcp_call：把外部 MCP 服务器的一个工具当成节点。
	//
	// 为什么也标非确定性：工具结果由外部系统决定（网页今天长什么样、库里有什么行），
	// 同输入不同产出是常态，不该被评测当成可复现模块做配对比较。
	"mcp_call": {
		Type: "mcp_call", Title: "MCP 工具", Role: RoleTransform,
		Nondeterministic: true,
		Inputs:           []Port{{"text", PortText, false, "上游文本（args 里用 {{text}} 引用）"}},
		Outputs: []Port{
			{"text", PortText, true, "工具返回的文本（多个 content 文本块拼成一段）"},
			{"json", PortJSON, false, "工具的原始返回值"},
			{"info", PortJSON, true, "服务器 / 工具名 / is_error / 耗时"},
		},
		KeyParams: []KeyParam{
			{Key: "server", Label: "MCP 服务器", Help: "来自工具台扫描到的 mcp.json（引擎自有 + WorkBuddy/Cursor/Claude 等本机配置）"},
			{Key: "tool", Label: "工具", Help: "该服务器 tools/list 里的 name；在工具台展开服务器能看到全部工具与入参 schema"},
			{Key: "args", Label: "参数 JSON", Help: "对象形式的 arguments。可写 {{text}} 占位替换上游文本；留空则把上游文本包成 {\"text\": \"…\"}"},
			{Key: "timeout_ms", Label: "超时", Unit: "ms", Help: "默认 180 秒，上限 30 分钟"},
			{Key: "allow_failure", Label: "失败也往下传", Help: "打开后 is_error 不阻断，但 info.is_error 如实记录"},
		},
		Agent: "外部 MCP 服务器提供的工具（浏览器抓取、数据库查询、平台 API…）。" +
			"先在「工具台」确认服务器能通、工具名对得上，再写进图里。" +
			"**不承诺可复现**：外部世界会变，评测时单独分组。",
	},

	// ---------------- 门 ----------------

	"quality_gate": {
		Type: "quality_gate", Title: "质检门", Role: RoleGate,
		Inputs:  []Port{{"any", PortAny, true, "被检查的产物"}},
		Outputs: []Port{{"any", PortAny, true, "通过的产物原样透传"}, {"json", PortJSON, true, "逐项检查结果"}},
		KeyParams: []KeyParam{
			{Key: "min_frames", Label: "最少帧数"},
			{Key: "duration_min", Label: "最短时长", Unit: "s"},
			{Key: "duration_max", Label: "最长时长", Unit: "s"},
			{Key: "min_width", Label: "最小宽", Unit: "px"},
			{Key: "min_height", Label: "最小高", Unit: "px"},
			{Key: "duration_tolerance", Label: "音画容差", Unit: "s"},
			{Key: "max_silence_gap", Label: "最长静音缺口", Unit: "s"},
			{Key: "loudness_min_db", Label: "响度下限", Unit: "dB"},
			{Key: "require_audio", Label: "必须有音轨"},
			{Key: "forbid_speech", Label: "氛围模式（有人声即 FAIL）"},
		},
		GateLayers: []string{"technical", "audio"},
		Agent:      "任一项 FAIL 即阻断下游。它只查「能算的」（帧数/时长/分辨率/响度），不判「好不好看」。",
	},

	"consistency_gate": {
		Type: "consistency_gate", Title: "一致性门", Role: RoleGate,
		Inputs:  []Port{{"video", PortVideo, false, "待检段（多段按上游 parts 顺序）"}, {"image", PortImage, false, "待检图 / 锚图"}},
		Outputs: []Port{{"json", PortJSON, true, "逐接缝与漂移度量"}, {"text", PortText, true, "一句话结论"}},
		KeyParams: []KeyParam{
			{Key: "inputs", Label: "待检路径", Help: "一行一条，按生成顺序。留空则用上游 parts（有序）"},
			{Key: "anchor", Label: "锚图", Help: "可选：定妆图/首段关键帧，每段都额外与它比一次"},
			{Key: "layout_min", Label: "布局相似度下限", Unit: "0-1"},
			{Key: "palette_min", Label: "配色相似度下限", Unit: "0-1"},
			{Key: "luma_max", Label: "亮度差上限", Unit: "0-1"},
			{Key: "drift_layout_min", Label: "相对首段布局下限", Unit: "0-1"},
			{Key: "drift_palette_min", Label: "相对首段配色下限", Unit: "0-1"},
			{Key: "check_drift", Label: "查累积漂移"},
			{Key: "warn_only", Label: "只报不拦"},
		},
		GateLayers: []string{"identity", "technical"},
		Agent: "**像素侧**的一致性门，与场景九宫格（文本侧）互补：九宫格保证各段收到同一句锁句，" +
			"它保证各段**画出来**是同一个场景。三项独立判：布局（dHash，陈设/构图）、" +
			"配色（HSV 直方图巴氏系数）、光比（平均亮度差）；另查各段相对首段的累积漂移。" +
			"报错会指名**哪个格位**漂了（九宫格同名格位），直接对应要改的那一格。" +
			"注意：它只证明「画面像不像」，不证明「好不好看」，也不替代质检门（帧数/时长/响度）。",
	},

	// ---------------- 代码 ----------------

	"code": {
		Type: "code", Title: "代码执行", Role: RoleTransform,
		Inputs:  []Port{{"text", PortText, false, "输入文本"}, {"json", PortJSON, false, "输入数据"}},
		Outputs: []Port{{"text", PortText, true, "标准输出"}, {"code_out", PortJSON, true, "结构化结果"}},
		KeyParams: []KeyParam{
			{Key: "code", Label: "代码"},
			{Key: "timeout_ms", Label: "超时", Unit: "ms"},
		},
		Agent: "在本机跑脚本。它是「任意逻辑」的逃生口——但跑不进任务书的门，出了错也不会自动重试。",
	},
	"command": {
		Type: "command", Title: "本地命令", Role: RoleTransform,
		Inputs:  []Port{{"text", PortText, false, "作为 {{input}} 注入参数的上游文本"}},
		Outputs: []Port{{"text", PortText, true, "标准输出"}, {"json", PortJSON, true, "退出码 / stderr / 耗时"}, {"status_code", PortJSON, true, "退出码（0 = 成功）"}},
		KeyParams: []KeyParam{
			{Key: "command", Label: "命令", Help: "可执行文件名或绝对路径；不经 shell，argv 逐个传参"},
			{Key: "args", Label: "参数", Help: "JSON 数组优先，否则按空格分隔；支持 {{input}} 占位"},
			{Key: "workdir", Label: "工作目录"},
			{Key: "timeout_ms", Label: "超时", Unit: "ms"},
			{Key: "fail_on_nonzero", Label: "非零退出即失败", Help: "关掉后非零退出仍往下传，但 exit_code 如实记录"},
		},
		Agent: "agent_task 的确定性对偶：把需要判断的活儿交给智能体（不可复现），把验收交给本地命令（必然可复现）。跑测试、跑构建、调校验脚本用它，不要用 agent 代跑——那会把确定性结论变成一次抽卡。",
	},
}

// ModuleContractOf 查契约。未登记时返回一个**显式标记为未声明**的兜底契约，
// 而不是零值：任务书必须能写出"这个模块没有契约"，不能静默当成普通变换。
func ModuleContractOf(typ string) (ModuleContract, bool) {
	c, ok := Modules[typ]
	return c, ok
}

// PortKindAccepts 判断上游端口类型能否连到下游端口。
//
// 规则只有一条：any 与任何类型互通，其余必须完全一致。
// 引擎**不写"哪些节点能接哪些节点"的白名单**——那就是分类，会把新模态挡在门外。
func PortKindAccepts(from, to PortKind) bool {
	if from == PortAny || to == PortAny {
		return true
	}
	return from == to
}

// ModuleTypes 已声明契约的类型（供一致性测试与 /api/modules 使用）。
func ModuleTypes() []string {
	out := make([]string, 0, len(Modules))
	for k := range Modules {
		out = append(out, k)
	}
	return out
}

// DefaultKeyParams 该类型的参数标签表（任务书用；没有则空，字段仍会输出到「其他参数」）。
func DefaultKeyParams(typ string) []KeyParam {
	if c, ok := Modules[typ]; ok {
		return c.KeyParams
	}
	return nil
}
