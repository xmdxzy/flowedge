package service

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// 文生图后端：为什么需要这层抽象
//
// 改造前（2026-10-23 之前）GenerateImageWithCkpt 只有一张硬编码的 ComfyUI API 图：
// CheckpointLoaderSimple + CLIPTextEncode + EmptyLatentImage + KSampler + VAEDecode。
// 换模型 = 换 ckpt 名。这个假设对 flux/SDXL 成立，对 MOSS-Image 不成立：
//
//	MOSS 是「DiT + 独立 9B 文本编码器 + 独立 VAE」三件套，走 UNETLoader /
//	CLIPLoader(type=moss_image) / VAELoader / EmptyFlux2LatentImage。
//	它的 checkpoint 槽位里根本没有东西，换名字只会得到 400。
//
// 所以「后端」不是模型名的一个属性，而是**构图方式**的一个维度。
// 把它变成显式的一层，第三个后端（SD3.5 / Qwen-Image）就只是加一个 case，
// 不用再往 multimodal.go 里塞第三个 if。

// T2IBackend 文生图后端（决定 ComfyUI 图怎么搭、权重走哪个实例）
type T2IBackend string

const (
	BackendFlux T2IBackend = "flux" // flux/SDXL：单 checkpoint 打包
	BackendMOSS T2IBackend = "moss" // MOSS-Image：DiT + 编码器 + VAE 分离，原生文字渲染与布局控制
	BackendAuto T2IBackend = "auto" // 按提示词与布局自动选（默认）
)

// T2IBackends 所有可选后端（供前端下拉与契约 Options 复用，避免两处名单漂移）
var T2IBackends = []string{string(BackendAuto), string(BackendFlux), string(BackendMOSS)}

// MOSS 默认推理参数。
//
// 来源：OpenMOSS 官方推荐（Euler flow-matching / 50 步 / CFG 3 / shift 5）。
// shift 不用单独节点——ComfyUI 的 MOSSImage 模型定义里 sampling_settings.shift=5.0，
// 加载器会自动应用（comfy/supported_models.py:2041）。
const (
	mossDefaultSteps = 50
	mossDefaultCFG   = 3.0
	mossDefaultW     = 1024
	mossDefaultH     = 1024
)

// mossRenderHints 触发「这张图里有字」的提示词信号。
//
// 为什么是关键词表而不是让 LLM 判断：这里是构图前的同步决策，
// 一次 LLM 调用（几秒到几十秒）比省下的这点判断贵得多，而且不可复算——
// 同样的图今天 LLM 说用 moss，明天可能说用 flux。表是死的，可审计、可测。
var mossRenderHints = []string{
	// 中文
	"写着", "文字", "标题", "字幕", "海报", "招牌", "标语", "排版", "字样",
	"logo", "Logo", "LOGO", "水印文字", "封面字", "对联",
	// 英文
	"reads ", "says ", "with text", "typography", "poster", "signage", "headline", "caption text",
}

// ResolveT2IBackend 决定这次出图用哪个后端。
//
// 为什么 auto 的判据必须能说清楚：用户看到「这张图跑了 40 秒而不是 3 秒」
// 需要知道是引擎选的而不是模型坏了。所以返回 reason，前端与日志都要能看到。
func ResolveT2IBackend(explicit string, prompt string, hasLayout bool) (T2IBackend, string) {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case string(BackendFlux):
		return BackendFlux, "节点显式指定 flux"
	case string(BackendMOSS):
		return BackendMOSS, "节点显式指定 MOSS"
	}
	if hasLayout {
		return BackendMOSS, "提供了布局（bbox），只有 MOSS 支持位置控制"
	}
	low := strings.ToLower(prompt)
	for _, h := range mossRenderHints {
		if strings.Contains(low, strings.ToLower(h)) {
			return BackendMOSS, "提示词命中文字渲染信号「" + h + "」"
		}
	}
	return BackendFlux, "默认 flux（无文字/布局需求，4 步最快）"
}

// MOSSBoxSpec 一个布局元素（归一化坐标，与 ComfyUI CreateBoundingBoxes 同一口径）。
//
// bbox 为 0-1000 的 [y0, x0, y1, x1]。为什么用这个而不是像素：
// 用户在画布上框的位置与最终分辨率无关，改分辨率不用重画框。
type MOSSBoxSpec struct {
	Type string `json:"type"`           // text / object / character
	Desc string `json:"desc,omitempty"` // 该元素的描述（text 类型可省）
	Text string `json:"text,omitempty"` // type=text 时要渲染的原字
	BBox [4]int `json:"bbox"`           // 0-1000 [y0,x0,y1,x1]
}

// MOSSLayout 官方 layout JSON 结构。
//
// 字段与 comfy_extras/nodes_moss_image.py:46 的输出逐字一致：
// bbox_coord_space=abs_centralized（像素制、原点在画布中心）、bbox_format=yxyx。
type MOSSLayout struct {
	Summary struct {
		Caption string `json:"caption"`
	} `json:"summary"`
	Items           []mossLayoutItem `json:"items"`
	BBoxCoordSpace  string           `json:"bbox_coord_space"`
	BBoxFormat      string           `json:"bbox_format"`
	SerializedCache string           `json:"-"`
}

type mossLayoutItem struct {
	Label   string `json:"label"`
	Caption string `json:"caption"`
	Text    string `json:"text,omitempty"`
	BBox    [4]int `json:"bbox"`
}

// BuildMOSSLayoutJSON 把归一化框编译成喂给 CLIPTextEncode 的 layout JSON。
//
// 为什么不连 MOSSImageLayoutPrompt 节点：那个节点只是把 Create Bounding Boxes
// 的数组转成同样的 JSON 字符串。引擎侧自己拼省掉两个节点，也让 layout 可以
// 从上游 JSON 端口直接进来（图里能算出来，不必手填）。
//
// 坐标换算逐字照抄官方实现（含 floor/ceil 的方向），换错会整体偏移半格。
func BuildMOSSLayoutJSON(summary string, boxes []MOSSBoxSpec, width, height int) (string, error) {
	if width <= 0 || height <= 0 {
		width, height = mossDefaultW, mossDefaultH
	}
	items := make([]mossLayoutItem, 0, len(boxes))
	for _, b := range boxes {
		label := strings.ToLower(strings.TrimSpace(b.Type))
		switch label {
		case "text":
			label = "text"
		case "character", "person":
			label = "character"
		case "", "object":
			label = "object"
		default:
			label = "object"
		}
		it := mossLayoutItem{Label: label, Caption: b.Desc}
		if label == "text" {
			it.Text = b.Text
			if it.Caption == "" {
				it.Caption = b.Text
			}
		}
		y0, x0, y1, x1 := float64(b.BBox[0]), float64(b.BBox[1]), float64(b.BBox[2]), float64(b.BBox[3])
		it.BBox = [4]int{
			int(math.Floor(round9(y0*float64(height)/1000))) - height/2,
			int(math.Floor(round9(x0*float64(width)/1000))) - width/2,
			int(math.Ceil(round9(y1*float64(height)/1000))) - height/2,
			int(math.Ceil(round9(x1*float64(width)/1000))) - width/2,
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return "", nil
	}
	l := MOSSLayout{BBoxCoordSpace: "abs_centralized", BBoxFormat: "yxyx", Items: items}
	l.Summary.Caption = strings.TrimSpace(summary)
	raw, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// round9 对齐官方的 round(x, 9)：干掉浮点尾数导致的 ±1 像素漂移
func round9(v float64) float64 {
	return math.Round(v*1e9) / 1e9
}

// BuildFluxGraph flux / SDXL：单 checkpoint 打包的标准 txt2img API 图。
//
// 这张图与 2026-10-23 改造前的实现逐节点一致（同样的节点 id 与连线），
// 只是把字面量换成入参——升级不能悄悄改变既有出片结果。
func BuildFluxGraph(prompt, negative, ckpt string, width, height, steps int, cfg float64, seed int64) map[string]interface{} {
	link := func(id string, slot int) []interface{} { return []interface{}{id, slot} }
	return map[string]interface{}{
		"4": map[string]interface{}{
			"class_type": "CheckpointLoaderSimple",
			"inputs":     map[string]interface{}{"ckpt_name": ckpt},
		},
		"6": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs":     map[string]interface{}{"text": prompt, "clip": link("4", 1)},
		},
		"33": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs":     map[string]interface{}{"text": negative, "clip": link("4", 1)},
		},
		"5": map[string]interface{}{
			"class_type": "EmptyLatentImage",
			"inputs":     map[string]interface{}{"width": width, "height": height, "batch_size": 1},
		},
		"3": map[string]interface{}{
			"class_type": "KSampler",
			"inputs": map[string]interface{}{
				"seed": seed, "steps": steps, "cfg": cfg,
				"sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
				"model": link("4", 0), "positive": link("6", 0), "negative": link("33", 0),
				"latent_image": link("5", 0),
			},
		},
		"8": map[string]interface{}{
			"class_type": "VAEDecode",
			"inputs":     map[string]interface{}{"samples": link("3", 0), "vae": link("4", 2)},
		},
		"9": map[string]interface{}{
			"class_type": "SaveImage",
			"inputs":     map[string]interface{}{"images": link("8", 0), "filename_prefix": "flowedge"},
		},
	}
}

// BuildMOSSGraph MOSS-Image：DiT / 文本编码器 / VAE 三件套分离加载。
//
// 三个关键差异（照抄 ComfyUI 官方模板，改错任何一个都是 400 或糊图）：
//  1. 用 EmptyFlux2LatentImage 而不是 EmptyLatentImage——MOSS 的 latent_format 是 Flux2，
//     通道数与老 latent 不同，用错节点报 size mismatch。
//  2. CLIPLoader 的 type 必须是 "moss_image"（0.41 才有的枚举），
//     漏了会得到「检测不到模型类型」。
//  3. 不插 ModelSamplingShift：MOSSImage 模型定义自带 sampling_settings.shift=5.0，
//     再手动设一遍等于 shift 叠乘。
func BuildMOSSGraph(prompt, negative, unet, clip, vae string, width, height, steps int, cfg float64, seed int64) map[string]interface{} {
	link := func(id string, slot int) []interface{} { return []interface{}{id, slot} }
	return map[string]interface{}{
		"1": map[string]interface{}{
			"class_type": "UNETLoader",
			"inputs":     map[string]interface{}{"unet_name": unet, "weight_dtype": "default"},
		},
		"2": map[string]interface{}{
			"class_type": "CLIPLoader",
			"inputs":     map[string]interface{}{"clip_name": clip, "type": "moss_image"},
		},
		"3": map[string]interface{}{
			"class_type": "VAELoader",
			"inputs":     map[string]interface{}{"vae_name": vae},
		},
		"4": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs":     map[string]interface{}{"text": prompt, "clip": link("2", 0)},
		},
		"5": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs":     map[string]interface{}{"text": negative, "clip": link("2", 0)},
		},
		"6": map[string]interface{}{
			"class_type": "EmptyFlux2LatentImage",
			"inputs":     map[string]interface{}{"width": width, "height": height, "batch_size": 1},
		},
		"7": map[string]interface{}{
			"class_type": "KSampler",
			"inputs": map[string]interface{}{
				"seed": seed, "steps": steps, "cfg": cfg,
				"sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
				"model": link("1", 0), "positive": link("4", 0), "negative": link("5", 0),
				"latent_image": link("6", 0),
			},
		},
		"8": map[string]interface{}{
			"class_type": "VAEDecode",
			"inputs":     map[string]interface{}{"samples": link("7", 0), "vae": link("3", 0)},
		},
		"9": map[string]interface{}{
			"class_type": "SaveImage",
			"inputs":     map[string]interface{}{"images": link("8", 0), "filename_prefix": "flowedge"},
		},
	}
}

// ParseMOSSBoxes 从任意 JSON 片段解析布局元素（上游端口 / 节点参数都走这里）。
//
// 宽容的原因：上游可能是 LLM 输出的数组、可能是手填的字符串。
// 解析不出来就返回空——空布局会退化成普通提示词，不会让整张图失败。
func ParseMOSSBoxes(raw interface{}) []MOSSBoxSpec {
	if raw == nil {
		return nil
	}
	var text string
	switch v := raw.(type) {
	case string:
		text = strings.TrimSpace(v)
	case []byte:
		text = strings.TrimSpace(string(v))
	case []interface{}, map[string]interface{}:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		text = string(b)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		text = string(b)
	}
	if text == "" {
		return nil
	}
	// 直接是数组
	var arr []map[string]interface{}
	if err := json.Unmarshal([]byte(text), &arr); err == nil {
		return boxesFromMaps(arr)
	}
	// 包了一层 {"items":[...]} 或 {"layout": {...}}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return nil
	}
	for _, key := range []string{"items", "boxes", "layout"} {
		if sub, ok := obj[key]; ok {
			if s, ok := sub.(string); ok {
				return ParseMOSSBoxes(s)
			}
			if a, ok := sub.([]interface{}); ok {
				ms := make([]map[string]interface{}, 0, len(a))
				for _, e := range a {
					if m, ok := e.(map[string]interface{}); ok {
						ms = append(ms, m)
					}
				}
				return boxesFromMaps(ms)
			}
		}
	}
	return nil
}

func boxesFromMaps(arr []map[string]interface{}) []MOSSBoxSpec {
	out := make([]MOSSBoxSpec, 0, len(arr))
	for _, m := range arr {
		var b MOSSBoxSpec
		if v, ok := m["type"].(string); ok {
			b.Type = v
		}
		if v, ok := m["desc"].(string); ok {
			b.Desc = v
		}
		if v, ok := m["text"].(string); ok {
			b.Text = v
		}
		switch bb := m["bbox"].(type) {
		case []interface{}:
			for i := 0; i < 4 && i < len(bb); i++ {
				switch n := bb[i].(type) {
				case float64:
					b.BBox[i] = int(n)
				case string:
					if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
						b.BBox[i] = int(f)
					}
				}
			}
		case []float64:
			for i := 0; i < 4 && i < len(bb); i++ {
				b.BBox[i] = int(bb[i])
			}
		}
		out = append(out, b)
	}
	return out
}
