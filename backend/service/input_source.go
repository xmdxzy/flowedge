package service

// 文件源（input_source）：把「已有的外部文件」变成图里的一等公民。
//
// 为什么必须有这个模块 —— 不是"再加一个便利节点"，而是补一个结构性缺口：
//
//	start 只输出 text。而 asr 吃 audio、upscale 吃 image、post_production 吃 video。
//	于是「转写一段录音」这类目标，在图上根本**无法表达** —— 除非让 asr 孤立地站着，
//	把路径塞进它的 data.audio_path。
//
// 这在引擎里是合法的（inputCoveredByParam 认这个），但有两个代价：
//  1. 模型不知道"孤立节点合法"，于是反复硬连 start(text)→asr(audio)，被校验器拒绝，
//     修 2 轮还是拒绝 —— 2026-10-23 标定里 3/10 条目标就是这么失败的。
//  2. 人也一样。画布上拖一个 asr 节点，看到它有必填 audio 输入却没有上游，
//     第一反应是"这图画错了"，而不是"它靠参数自给"。
//
// 有了 input_source，"外部文件"变成一条正常的边：input_source → asr。
// 图的结构与直觉一致，模型与人都不用再记一条例外规则。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"flowedge/backend/model"
)

// 允许的 kind 取值。故意只用四种，不引入 "file" 这种万能类型 ——
// 万能类型的下游只能接 PortAny，等于把类型检查让掉。
const (
	sourceKindText  = "text"
	sourceKindImage = "image"
	sourceKindAudio = "audio"
	sourceKindVideo = "video"
)

// 占位值：构图或人先搭骨架、文件后补，是正常用法。
// 这些值不该被当成"路径写错了"，而该被当成"还没给文件"。
var sourcePlaceholders = []string{"待提供", "待填写", "todo", "tbd", "placeholder", "none", "null", "-"}

// 扩展名 → kind。用于 path 给了但 kind 没给时自动判定。
//
// 为什么不要求必须写 kind：人填一个 .wav 路径时，"这是音频"是显然的；
// 让他再选一次类型只是多一个可以填错的地方。
var sourceExtKinds = map[string]string{
	".png": sourceKindImage, ".jpg": sourceKindImage, ".jpeg": sourceKindImage,
	".webp": sourceKindImage, ".bmp": sourceKindImage, ".gif": sourceKindImage,
	".mp3": sourceKindAudio, ".wav": sourceKindAudio, ".flac": sourceKindAudio,
	".m4a": sourceKindAudio, ".aac": sourceKindAudio, ".ogg": sourceKindAudio,
	".mp4": sourceKindVideo, ".mov": sourceKindVideo, ".mkv": sourceKindVideo,
	".webm": sourceKindVideo, ".avi": sourceKindVideo,
	".txt": sourceKindText, ".md": sourceKindText, ".srt": sourceKindText, ".json": sourceKindText,
}

// SourceKindOf 判定节点实际输出的类型。导出给前端/校验用，避免两端各写一套推断。
func SourceKindOf(node *model.NodeInstance) string {
	if node == nil {
		return ""
	}
	kind := strings.ToLower(strings.TrimSpace(strOf(node.Data["kind"])))
	if isSourceKind(kind) {
		return kind
	}
	// kind 没写或写错 → 按扩展名推断（推断不出就是 text，因为 text 可以来自 text 参数）
	if p := strings.TrimSpace(strOf(node.Data["path"])); p != "" {
		if k, ok := sourceExtKinds[strings.ToLower(filepath.Ext(p))]; ok {
			return k
		}
	}
	return sourceKindText
}

func isSourceKind(k string) bool {
	switch k {
	case sourceKindText, sourceKindImage, sourceKindAudio, sourceKindVideo:
		return true
	}
	return false
}

// execInputSourceNode 把外部文件读成下游能吃的端口。
//
// 输出**只含 kind 对应的那一个键**（加元信息），不把四个端口全填上。
// 理由：下游取不到自己的键时会报"没有 X 输入"，这个错误是准确的；
// 如果四个键都填同一个路径，audio 节点会拿到一个 mp4 的路径，然后 ffmpeg 报一句
// 与真实原因无关的错 —— 排查方向直接跑偏。
func (e *EngineV2) execInputSourceNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	kind := SourceKindOf(node)
	out := map[string]interface{}{
		"kind":   kind,
		"source": "input_source",
	}

	// text：可以来自 text 参数，也可以来自上游（虽然它自己就是 source，但人可能接一条边）
	if kind == sourceKindText {
		text := strOf(node.Data["text"])
		if text == "" {
			text = strOf(node.Data["path"]) // 允许 path 直接写一段文字（老习惯）
		}
		if text == "" {
			for _, up := range upstream {
				if m, ok := up.(map[string]interface{}); ok {
					if t, ok := m["text"].(string); ok && t != "" {
						text = t
						break
					}
				}
			}
		}
		if text == "" {
			return nil, fmt.Errorf("文件源（text）没有内容：在节点里填「文本内容」，或从上游接一条文本边")
		}
		out["text"] = text
		return out, nil
	}

	// 媒体类：必须有真实文件
	path := strings.TrimSpace(strOf(node.Data["path"]))
	if path == "" {
		return nil, fmt.Errorf("文件源（%s）没有填文件路径", kind)
	}
	if isPlaceholderPath(path) {
		// 这不是"配置错了"，是"还没给文件"。措辞要让人知道下一步做什么，
		// 而不是去查路径拼写。
		return nil, fmt.Errorf("文件源（%s）还是占位值 %q —— 还没有指定实际文件。先填好路径再运行", kind, path)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("文件源的路径无法解析：%s", path)
	}
	st, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("文件源指定的文件不存在：%s", abs)
		}
		return nil, fmt.Errorf("文件源无法读取 %s：%v", abs, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("文件源要的是文件路径，给的是目录：%s", abs)
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("文件源指定的文件是空的（0 字节）：%s", abs)
	}

	// 声明的 kind 与扩展名不符 → 明确报错，不猜。
	// 猜错的代价是下游跑到一半才炸，而那时错误信息已经离原因很远。
	if k, ok := sourceExtKinds[strings.ToLower(filepath.Ext(abs))]; ok && k != kind {
		return nil, fmt.Errorf("文件源声明为 %s，但 %s 看起来是 %s。改 kind 或换文件",
			kind, filepath.Base(abs), k)
	}

	out[kind] = abs
	// path 也带上：任务书与产物血缘要靠它写"用了哪个源文件"
	out["path"] = abs
	out["bytes"] = st.Size()
	return out, nil
}

func isPlaceholderPath(p string) bool {
	low := strings.ToLower(strings.TrimSpace(p))
	for _, ph := range sourcePlaceholders {
		if low == ph {
			return true
		}
	}
	// "<...>" / "{{...}}" 也是占位写法
	return strings.HasPrefix(low, "<") || strings.HasPrefix(low, "{{")
}

// strOf 取字符串参数（data 里可能是 nil / 数字 / bool，类型断言会 panic）
func strOf(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		// JSON 数字 → 去掉多余的 .0
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
