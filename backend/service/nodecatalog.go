package service

import (
	"sort"
)

// 节点契约：端口类型 / 表单字段 / 成本模型。
//
// 为什么与 NodeSpecs 分两张表，而不是塞进同一个结构体：
// NodeSpecs 是**调度**元数据（显存侧别 / 是否重节点 / 超时），改错会影响显存仲裁；
// 这里全是**画布**元数据（连线类型 / 检查器表单 / 估算成本），改错只影响界面与预算。
// 两张表都以节点类型为键，覆盖性由 nodecatalog_test.go 断言（键集合必须完全相等），
// 所以「单一来源」没有被破坏：加节点必须同时登记两处，漏一处测试就红。
//
// 这张表存在的理由（v1 的真实教训）：当时 27 种节点的端口、默认值、表单
// 各写一遍在前端，加一个节点要改 4–5 处，且编译器不报错。
// 现在前端调色板与检查器全部由 GET /api/node-catalog 生成。

// PortKind 端口承载的值类型。刻意不做隐式转换：
// 隐式转换会造出「能跑但语义错了」的图，比直接拦下来更伤人。
type PortKind string

const (
	PortText  PortKind = "text"
	PortImage PortKind = "image"
	PortVideo PortKind = "video"
	PortAudio PortKind = "audio"
	PortAny   PortKind = "any"
)

// Port 一个输入/输出端口。Key 同时是连线的 sourceHandle / targetHandle 名。
//
// Required 只对输入端口有意义：它表示「这个输入没有来源就跑不了」。
// 判定见 portcheck.go 的 input_unreachable —— 那是 v1「我明明连了却没生效」
// 那个 bug 的正面修法：连线存在但类型对不上，等于没连。
type Port struct {
	Key      string   `json:"key"`
	Kind     PortKind `json:"kind"`
	Required bool     `json:"required,omitempty"`
}

// FieldKind 检查器里的表单控件类型（与 PortKind 是两件事：
// 一个是「值是什么」，一个是「怎么编辑」）。
type FieldKind string

const (
	FieldText     FieldKind = "text"
	FieldTextarea FieldKind = "textarea"
	FieldInt      FieldKind = "int"
	FieldFloat    FieldKind = "float"
	FieldBool     FieldKind = "bool"
	FieldEnum     FieldKind = "enum"
	FieldFile     FieldKind = "file"
)

// FieldSpec 一个节点字段的编辑契约，前端据此生成表单。
type FieldSpec struct {
	Key     string      `json:"key"`
	Label   string      `json:"label"`
	Kind    FieldKind   `json:"kind"`
	Default interface{} `json:"default,omitempty"`
	Min     *float64    `json:"min,omitempty"`
	Max     *float64    `json:"max,omitempty"`
	Options []string    `json:"options,omitempty"`
	Hint    string      `json:"hint,omitempty"`
}

// Cost 成本模型。单位刻意写清（Unit），因为「一帧视频」和「一张图」不是同一个量。
type Cost struct {
	GPUClass    string  `json:"gpu_class"` // diffusion | language | audio | cpu
	BaseSec     float64 `json:"base_sec"`  // 固定开销：提交 / 取回 / 拼接
	ColdLoadSec float64 `json:"cold_load_sec"`
	SecPerUnit  float64 `json:"sec_per_unit"`
	Unit        string  `json:"unit"`                // image | frame | char | call | round | none
	RefWidth    int     `json:"ref_width,omitempty"` // 单位基准分辨率；0 = 不按面积缩放
	RefHeight   int     `json:"ref_height,omitempty"`
	VramMB      int     `json:"vram_mb"`
}

// NodeContract 一个节点类型的画布契约。
type NodeContract struct {
	// Label 人类可读名（画布卡片标题）。放在这里而不是前端字典，
	// 否则加节点时前端会静默显示成英文 type —— 又一处双写。
	Label  string      `json:"label"`
	In     []Port      `json:"in"`
	Out    []Port      `json:"out"`
	Fields []FieldSpec `json:"fields"`
	Cost   Cost        `json:"cost"`
}

// CatalogEntry 调色板条目 = 调度元数据 + 画布契约。
type CatalogEntry struct {
	Type  string `json:"type"`
	Side  string `json:"side"`
	Heavy bool   `json:"heavy"`
	Media bool   `json:"media"`
	NodeContract
}

func f64(v float64) *float64 { return &v }

var (
	widthField1024  = FieldSpec{Key: "width", Label: "宽", Kind: FieldInt, Default: 1024, Min: f64(384), Max: f64(1536)}
	heightField1024 = FieldSpec{Key: "height", Label: "高", Kind: FieldInt, Default: 1024, Min: f64(384), Max: f64(1536)}
	widthField608   = FieldSpec{Key: "width", Label: "宽", Kind: FieldInt, Default: 608, Min: f64(256), Max: f64(1536)}
	heightField352  = FieldSpec{Key: "height", Label: "高", Kind: FieldInt, Default: 352, Min: f64(160), Max: f64(1024)}
	stepsField      = FieldSpec{Key: "steps", Label: "采样步数", Kind: FieldInt, Default: 12, Min: f64(1), Max: f64(60)}
	framesField     = FieldSpec{Key: "frames", Label: "帧数", Kind: FieldInt, Default: 124, Min: f64(24), Max: f64(480)}
)

// nodeContracts 13 种节点的画布契约。键必须与 NodeSpecs 完全一致（测试钉住）。
var nodeContracts = map[string]NodeContract{
	"start": {
		Label: "开始",
		Out:   []Port{{Key: "text", Kind: PortText}},
		Fields: []FieldSpec{
			{Key: "prompt", Label: "提示词 / 文案", Kind: FieldTextarea, Hint: "下游文本线从这里取"},
		},
		Cost: Cost{GPUClass: "cpu", Unit: "none"},
	},
	"end": {
		Label: "结束",
		In:    []Port{{Key: "in", Kind: PortAny}},
		Cost:  Cost{GPUClass: "cpu", Unit: "none"},
	},

	// ---- 语言侧 ----
	"llm": {
		Label: "语言模型",
		In:    []Port{{Key: "text", Kind: PortText, Required: true}},
		Out:   []Port{{Key: "text", Kind: PortText}},
		Fields: []FieldSpec{
			{Key: "prompt", Label: "指令", Kind: FieldTextarea},
			{Key: "model", Label: "模型", Kind: FieldText, Hint: "留空用默认模型"},
		},
		Cost: Cost{GPUClass: "language", BaseSec: 3, SecPerUnit: 8, Unit: "call", ColdLoadSec: 25},
	},
	"storyboard": {
		Label: "分镜规划",
		In:    []Port{{Key: "text", Kind: PortText, Required: true}},
		Out:   []Port{{Key: "text", Kind: PortText}},
		Fields: []FieldSpec{
			{Key: "prompt", Label: "故事", Kind: FieldTextarea},
			{Key: "seconds", Label: "目标时长（秒）", Kind: FieldFloat, Default: 10.0, Min: f64(2), Max: f64(180)},
			{Key: "max_shots", Label: "最多镜数", Kind: FieldInt, Default: 3, Min: f64(1), Max: f64(12)},
		},
		Cost: Cost{GPUClass: "language", BaseSec: 8, SecPerUnit: 12, Unit: "call", ColdLoadSec: 25},
	},

	// ---- 扩散侧 ----
	"text2image": {
		Label: "文生图",
		In:    []Port{{Key: "text", Kind: PortText, Required: true}},
		Out:   []Port{{Key: "image", Kind: PortImage}},
		Fields: []FieldSpec{
			{Key: "prompt", Label: "画面描述（节点内）", Kind: FieldTextarea, Hint: "优先取上游文本；无连线时才用这里"},
			{Key: "prompts", Label: "批量提示词（每行一条）", Kind: FieldTextarea},
			widthField1024, heightField1024,
			{Key: "batch", Label: "张数", Kind: FieldInt, Default: 1, Min: f64(1), Max: f64(4)},
			stepsField,
			{Key: "backend", Label: "后端", Kind: FieldEnum, Default: "auto", Options: []string{"auto", "flux", "moss"}},
			{Key: "layout", Label: "MOSS 布局（填了就按 MOSS 走）", Kind: FieldTextarea},
		},
		Cost: Cost{GPUClass: "diffusion", BaseSec: 15, SecPerUnit: 30, Unit: "image",
			RefWidth: 1024, RefHeight: 1024, VramMB: 11000, ColdLoadSec: 45},
	},
	"image2image": {
		Label: "图像重绘",
		In:    []Port{{Key: "image", Kind: PortImage, Required: true}, {Key: "text", Kind: PortText}},
		Out:   []Port{{Key: "image", Kind: PortImage}},
		Fields: []FieldSpec{
			{Key: "image_path", Label: "输入图", Kind: FieldFile, Hint: "也可连一条上游图片线"},
			{Key: "prompt", Label: "重绘描述", Kind: FieldTextarea},
			{Key: "strength", Label: "重绘强度", Kind: FieldFloat, Default: 0.85, Min: f64(0.05), Max: f64(1)},
			widthField1024, heightField1024,
		},
		Cost: Cost{GPUClass: "diffusion", BaseSec: 12, SecPerUnit: 22, Unit: "image",
			RefWidth: 1024, RefHeight: 1024, VramMB: 11000, ColdLoadSec: 45},
	},
	"inpaint": {
		Label: "局部重绘",
		In:    []Port{{Key: "image", Kind: PortImage, Required: true}, {Key: "text", Kind: PortText}},
		Out:   []Port{{Key: "image", Kind: PortImage}},
		Fields: []FieldSpec{
			{Key: "image_path", Label: "原图", Kind: FieldFile},
			{Key: "mask_path", Label: "蒙版（白色为重绘区）", Kind: FieldFile},
			{Key: "prompt", Label: "重绘描述", Kind: FieldTextarea},
			widthField1024, heightField1024,
		},
		Cost: Cost{GPUClass: "diffusion", BaseSec: 12, SecPerUnit: 25, Unit: "image",
			RefWidth: 1024, RefHeight: 1024, VramMB: 11000, ColdLoadSec: 45},
	},
	"upscale": {
		Label: "超分放大",
		In:    []Port{{Key: "image", Kind: PortImage, Required: true}},
		Out:   []Port{{Key: "image", Kind: PortImage}},
		Fields: []FieldSpec{
			{Key: "image_path", Label: "输入图", Kind: FieldFile},
			{Key: "scale", Label: "放大倍数", Kind: FieldFloat, Default: 2.0, Min: f64(1.5), Max: f64(4)},
		},
		Cost: Cost{GPUClass: "diffusion", BaseSec: 8, SecPerUnit: 12, Unit: "image",
			RefWidth: 1024, RefHeight: 1024, VramMB: 9000, ColdLoadSec: 40},
	},

	// ---- 视频（H3 走语言侧调度，与 NodeSpecs 保持一致）----
	"text2video": {
		Label: "文生视频",
		In:    []Port{{Key: "text", Kind: PortText, Required: true}, {Key: "image", Kind: PortImage}},
		Out:   []Port{{Key: "video", Kind: PortVideo}},
		Fields: []FieldSpec{
			{Key: "prompt", Label: "画面描述", Kind: FieldTextarea},
			{Key: "backend", Label: "后端", Kind: FieldEnum, Default: "h3", Options: []string{"h3"}},
			widthField608, heightField352, framesField, stepsField,
			{Key: "segments", Label: "段数（续接）", Kind: FieldInt, Default: 1, Min: f64(1), Max: f64(4)},
			{Key: "prompt_opt", Label: "优化提示词", Kind: FieldBool, Default: false},
			{Key: "ref_images", Label: "参考图（多行路径）", Kind: FieldTextarea, Hint: "给了就走 Ref2VA 六段式"},
			{Key: "ref_audio", Label: "参考音频", Kind: FieldFile},
			{Key: "low_vram", Label: "低显存模式", Kind: FieldBool, Default: false},
			{Key: "pdd", Label: "PDD 8 步加速（非台词镜）", Kind: FieldBool, Default: false, Hint: "H3 PDD LoRA 8 步蒸馏，与 turbo 互斥"},
		},
		Cost: Cost{GPUClass: "language", BaseSec: 20, SecPerUnit: 0.7, Unit: "frame",
			RefWidth: 608, RefHeight: 352, VramMB: 23800, ColdLoadSec: 90},
	},
	"shotlist": {
		Label: "多镜出片",
		In:    []Port{{Key: "text", Kind: PortText}},
		Out:   []Port{{Key: "video", Kind: PortVideo}},
		Fields: []FieldSpec{
			{Key: "plan", Label: "分镜表 JSON（留空取上游）", Kind: FieldTextarea},
			widthField608, heightField352, stepsField,
			{Key: "characters", Label: "角色设定（锁身份用）", Kind: FieldTextarea},
			{Key: "interp", Label: "插帧", Kind: FieldBool, Default: false},
			{Key: "audio_master", Label: "音频母带", Kind: FieldBool, Default: true},
			{Key: "consistency_check", Label: "一致性门", Kind: FieldBool, Default: true},
			{Key: "reshoot_max", Label: "最多重拍镜数", Kind: FieldInt, Default: 0, Min: f64(0), Max: f64(6)},
		},
		Cost: Cost{GPUClass: "diffusion", BaseSec: 30, SecPerUnit: 0.75, Unit: "frame",
			RefWidth: 608, RefHeight: 352, VramMB: 23800, ColdLoadSec: 60},
	},

	// ---- 语音侧 ----
	"tts": {
		Label: "语音合成",
		In:    []Port{{Key: "text", Kind: PortText, Required: true}},
		Out:   []Port{{Key: "audio", Kind: PortAudio}},
		Fields: []FieldSpec{
			{Key: "text", Label: "台词", Kind: FieldTextarea},
			{Key: "voice", Label: "音色", Kind: FieldText, Hint: "留空用默认音色"},
			{Key: "ref_audio", Label: "参考音频（克隆）", Kind: FieldFile, Hint: "填了就克隆音色，失败不允许回退默认音色"},
		},
		Cost: Cost{GPUClass: "audio", BaseSec: 5, SecPerUnit: 0.2, Unit: "char", ColdLoadSec: 60, VramMB: 0},
	},

	// ---- 音乐侧：ACE-Step 1.5（GPU）----
	// 只列「出片会用到的」字段；repaint/cover 这类编辑任务等有真实需求再加，
	// 现在加进来只会让节点面板出现一堆没人用的输入框。
	"music": {
		Label: "音乐生成（ACE-Step）",
		In:    []Port{{Key: "text", Kind: PortText}},
		Out:   []Port{{Key: "audio", Kind: PortAudio}},
		Fields: []FieldSpec{
			{Key: "prompt", Label: "风格/氛围描述", Kind: FieldTextarea,
				Hint: "例：warm lo-fi piano, soft vinyl noise, 80 BPM, no vocals；留空取上游文本"},
			{Key: "lyrics", Label: "歌词（留空 = 纯音乐）", Kind: FieldTextarea},
			{Key: "duration", Label: "时长（秒）", Kind: FieldInt, Default: 30, Min: f64(10), Max: f64(600)},
			{Key: "model", Label: "模型", Kind: FieldEnum, Default: defaultMusicModel,
				Options: []string{"acestep-v15-xl-turbo", "acestep-v15-xl-sft", "acestep-v15-turbo"}},
			{Key: "steps", Label: "推理步数", Kind: FieldInt, Default: 8, Min: f64(1), Max: f64(64)},
			{Key: "language", Label: "歌词语言", Kind: FieldText, Hint: "zh / en / ja…；留空按歌词猜"},
			{Key: "thinking", Label: "LM 增强（更慢更稳）", Kind: FieldBool, Default: false},
			{Key: "seed", Label: "种子", Kind: FieldInt, Hint: "0/留空 = 随机"},
			{Key: "output_path", Label: "输出路径（可空）", Kind: FieldText},
		},
		// 冷加载 3–5 分钟（9GB 权重 + LM）；生成按秒算，先按 1s/秒音频给初值，
		// 真实值跑过一次后会由 EWMA 覆盖。
		Cost: Cost{GPUClass: "music", BaseSec: 15, SecPerUnit: 1.2, Unit: "second",
			VramMB: 12000, ColdLoadSec: 240},
	},

	// ---- 质量控制 ----
	"quality_loop": {
		Label: "质量循环",
		In:    []Port{{Key: "text", Kind: PortText}},
		Out:   []Port{{Key: "video", Kind: PortVideo}, {Key: "image", Kind: PortImage}},
		Fields: []FieldSpec{
			{Key: "workflow_id", Label: "被循环的图 id", Kind: FieldInt, Min: f64(1)},
			{Key: "max_rounds", Label: "最多轮数", Kind: FieldInt, Default: 3, Min: f64(1), Max: f64(8)},
			{Key: "target_score", Label: "目标分", Kind: FieldInt, Default: 85, Min: f64(0), Max: f64(100)},
		},
		// 成本取决于被循环的图，静态不可知 —— 估算时计入 Uncertain，不编数字。
		Cost: Cost{GPUClass: "cpu", Unit: "round"},
	},

	// ---- 代码渲染：headless 浏览器逐帧渲染，零显存 ----
	"code_video": {
		Label: "代码视频",
		In:    []Port{{Key: "text", Kind: PortText}},
		Out:   []Port{{Key: "video", Kind: PortVideo}},
		Fields: []FieldSpec{
			{Key: "style", Label: "风格", Kind: FieldEnum, Default: "blueprint", Options: []string{"blueprint"}},
			{Key: "title", Label: "标题", Kind: FieldText},
			{Key: "points", Label: "要点（每行一条）", Kind: FieldTextarea},
			{Key: "kicker", Label: "栏目/眉题", Kind: FieldText},
			{Key: "figure", Label: "图示", Kind: FieldEnum, Default: "none", Options: []string{"none", "bars", "flow"}},
			{Key: "labels", Label: "图示标签（逗号分隔）", Kind: FieldText},
			{Key: "values", Label: "图示数值（逗号分隔）", Kind: FieldText},
			{Key: "duration", Label: "时长（秒）", Kind: FieldFloat, Default: 6.0, Min: f64(1), Max: f64(180)},
			{Key: "width", Label: "宽", Kind: FieldInt, Default: 1920, Min: f64(640), Max: f64(3840)},
			{Key: "height", Label: "高", Kind: FieldInt, Default: 1080, Min: f64(360), Max: f64(2160)},
			{Key: "fps", Label: "帧率", Kind: FieldInt, Default: 24, Min: f64(12), Max: f64(60)},
			{Key: "workers", Label: "渲染并发", Kind: FieldInt, Default: 2, Min: f64(1), Max: f64(6)},
			{Key: "tts_text", Label: "旁白（可空）", Kind: FieldTextarea, Hint: "填了就用 FE 的 TTS 合成并合流"},
			{Key: "spec", Label: "完整 spec（JSON，可选）", Kind: FieldTextarea, Hint: "多卡片的完整规格；填了优先于上面的单卡字段"},
		},
		Cost: Cost{GPUClass: "cpu", BaseSec: 20, SecPerUnit: 0.12, Unit: "frame"},
	},

	// ---- 逃生舱：跑用户自备的 ComfyUI 图 ----
	"comfy": {
		Label: "ComfyUI 图（逃生舱）",
		// 两个输入都可选：模板用不用 {{prompt}}/{{image}} 只有用户知道，
		// 标成 Required 会把「不用占位符的图」拦死在保存时。
		// 真缺值时运行期报错（substituteComfyPlaceholders），人话指到具体占位符。
		In:  []Port{{Key: "text", Kind: PortText}, {Key: "image", Kind: PortImage}},
		Out: []Port{{Key: "out", Kind: PortAny}}, // 产物类型取决于图里的保存节点（SaveImage/SaveVideo/...）
		Fields: []FieldSpec{
			{Key: "template", Label: "模板名", Kind: FieldText,
				Hint: "data/comfy_workflows/<名字>.json；ComfyUI 里「导出 (API)」的 JSON 直接放进去"},
			{Key: "workflow_json", Label: "内联工作流 JSON", Kind: FieldTextarea,
				Hint: "ComfyUI API 格式；填了优先于模板名"},
			{Key: "prompt", Label: "提示词（替换 {{prompt}}）", Kind: FieldTextarea,
				Hint: "留空时取上游文本线"},
			{Key: "image_path", Label: "输入图（替换 {{image}}）", Kind: FieldFile,
				Hint: "留空时取上游图片线"},
			{Key: "width", Label: "宽（替换 {{width}}）", Kind: FieldInt},
			{Key: "height", Label: "高（替换 {{height}}）", Kind: FieldInt},
			{Key: "seed", Label: "种子（替换 {{seed}}）", Kind: FieldInt, Hint: "0 表示不用这个占位符"},
			{Key: "instance", Label: "实例", Kind: FieldEnum, Default: "comfyui",
				Options: []string{"comfyui", "h3", "moss"}},
			{Key: "timeout_min", Label: "超时（分钟）", Kind: FieldInt, Default: 20, Min: f64(1), Max: f64(120)},
		},
		// 图内容未知 ⇒ 耗时与显存都静态不可知，估算时计入 Uncertain，不编数字。
		Cost: Cost{GPUClass: "diffusion", Unit: "graph"},
	},
}

// NodeContractOf 查画布契约。未登记返回零值 + false。
func NodeContractOf(typ string) (NodeContract, bool) {
	c, ok := nodeContracts[typ]
	return c, ok
}

// NodeCatalog 调色板的唯一数据源（按类型名排序，输出稳定便于前端做 diff）。
func NodeCatalog() []CatalogEntry {
	out := make([]CatalogEntry, 0, len(NodeSpecs))
	for typ, spec := range NodeSpecs {
		c := nodeContracts[typ]
		out = append(out, CatalogEntry{
			Type: typ, Side: spec.Side, Heavy: spec.Heavy, Media: spec.Media,
			NodeContract: c,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// PortKindsOf 取节点某方向的端口类型集合；未知类型返回 false。
func PortKindsOf(typ string, outbound bool) ([]PortKind, bool) {
	c, ok := nodeContracts[typ]
	if !ok {
		return nil, false
	}
	ports := c.In
	if outbound {
		ports = c.Out
	}
	kinds := make([]PortKind, 0, len(ports))
	for _, p := range ports {
		kinds = append(kinds, p.Kind)
	}
	return kinds, true
}

// PortDeclared 该节点是否声明了指定方向上的这个端口名。
// 只用于校验 handle，不作为阻断依据（handle 是 UI 细节）。
func PortDeclared(typ string, outbound bool, key string) bool {
	c, ok := nodeContracts[typ]
	if !ok || key == "" {
		return false
	}
	ports := c.In
	if outbound {
		ports = c.Out
	}
	for _, p := range ports {
		if p.Key == key {
			return true
		}
	}
	return false
}
