package service

// 提示词诊断：分型 + 定点修复（V3 P0，TARA arXiv 2607.18724 的工程化）
//
// 现状问题：`prompt_opt` 是「扩写式重写」——把用户的提示词交给 LLM 重写一遍。
// 两个后果：
//  1. 改好改坏分不清（没有"改之前缺什么"的结论，只有"改之后变成什么"）；
//  2. 一次改动可能同时动了主体、场景、镜头、音频四件事，违反「一轮只动一个变量」。
//
// 本文件只做**诊断与算子**，不做 LLM 重写：
//   - DiagnosePrompt：判出缺哪几类要素（分型），每条给原文证据
//   - ApplyOperator：只动一处（加场景 / 换模糊词 / 裁剪过长），可回滚
//
// 刻意不做：不在这里调 LLM。诊断是确定性的，LLM 只在"某类缺陷需要创作性补齐"时被调用，
// 由调用方决定——否则又变成"不可复现的重写"。

import (
	"fmt"
	"regexp"
	"strings"
)

// 提示词缺陷类型
const (
	DefectMissingSubject = "missing_subject" // 没有明确主体（人物/物件）
	DefectMissingScene   = "missing_scene"   // 没有场景/环境
	DefectMissingAction  = "missing_action"  // 没有动作/运动
	DefectMissingCamera  = "missing_camera"  // 没有镜头语言（景别/机位/运动）
	DefectMissingAudio   = "missing_audio"   // 视频缺环境声/配乐段
	DefectAmbiguous      = "ambiguous"       // 模糊修饰（"很美""高级感"）
	DefectOverlong       = "overlong"        // 过长（超出模型有效窗口）
)

// 严重度
const (
	SeverityOK   = "ok"
	SeverityWarn = "warn"
	SeverityFail = "fail"
)

// PromptDefect 一条缺陷：类型 + 原文证据 + 一句可执行修复 + 算子名。
type PromptDefect struct {
	Type     string `json:"type"`
	Evidence string `json:"evidence"`
	Fix      string `json:"fix"`
	Operator string `json:"operator"`
	// NeedsLLM 该缺陷是否必须借助 LLM 才能补（创作性内容），否则可用纯规则算子。
	NeedsLLM bool `json:"needs_llm"`
}

// PromptDiagnosis 诊断结论。
type PromptDiagnosis struct {
	Defects  []PromptDefect `json:"defects"`
	Severity string         `json:"severity"`
	// Summary 一句话结论（人读）。
	Summary string `json:"summary"`
}

// 判据词表。刻意短而明确：宁可漏判，不要误判——
// 误判会让人去改一个本来没问题的提示词（比不提示更糟）。
var (
	cameraWords = []string{"特写", "近景", "中景", "远景", "全景", "俯拍", "仰拍", "推镜", "拉镜",
		"摇镜", "跟拍", "手持", "航拍", "close-up", "wide shot", "medium shot", "dolly", "pan", "tracking"}
	audioWords = []string{"环境声", "风声", "雨声", "海浪", "鸟鸣", "配乐", "音乐", "BGM",
		"ambience", "sound of", "music", "score"}
	actionWords = []string{"走", "跑", "转身", "抬头", "伸手", "坐下", "推开", "拿起", "挥",
		"walk", "run", "turn", "reach", "lift", "sit", "open", "swing"}
	sceneWords = []string{"在", "里", "上", "下", "房间", "街", "海边", "森林", "山", "夜", "清晨",
		"黄昏", "室内", "室外", "room", "street", "beach", "forest", "night", "morning", "indoor"}
	// 模糊修饰：这些词无法被模型执行，必须换成可观测描述
	ambiguousWords = []string{"很美", "好看", "高级感", "有氛围", "电影感", "震撼", "唯美",
		"beautiful", "cinematic feel", "amazing", "stunning", "high-end"}
)

const promptMaxRunes = 1200 // 超过这个长度，模型有效注意力会衰减（经验值，不是硬限制）

func containsAny(text string, words []string) (string, bool) {
	lower := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(lower, strings.ToLower(w)) {
			return w, true
		}
	}
	return "", false
}

// DiagnosePrompt 诊断提示词。isVideo=true 时额外要求镜头语言与音频段。
//
// 空提示词直接 fail（不是 warn）：没有提示词就不该提交。
func DiagnosePrompt(text string, isVideo bool) PromptDiagnosis {
	d := PromptDiagnosis{}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		d.Severity = SeverityFail
		d.Summary = "提示词为空"
		d.Defects = append(d.Defects, PromptDefect{
			Type:     DefectMissingSubject,
			Evidence: "",
			Fix:      "至少写清主体 + 场景 + 动作三件事，例如「蓝发少女在海边码头转身，逆光，中景跟拍」",
			Operator: "require_nonempty",
		})
		return d
	}

	// 1) 主体：没有具体名词时最难判，这里用「有动作词或有人物/物件描述」近似。
	//    宁可漏判：把「一个人」判成缺主体会让人困惑。
	if _, ok := containsAny(trimmed, actionWords); !ok {
		d.Defects = append(d.Defects, PromptDefect{
			Type:     DefectMissingAction,
			Evidence: "整段没有可执行的动作动词",
			Fix:      "补一个具体动作（转身/伸手/推门/奔跑），静止画面在视频模型里会退化成轻微抖动",
			Operator: "add_action",
			NeedsLLM: true,
		})
	}

	// 2) 场景
	if _, ok := containsAny(trimmed, sceneWords); !ok {
		d.Defects = append(d.Defects, PromptDefect{
			Type:     DefectMissingScene,
			Evidence: "没有场景/环境词",
			Fix:      "补场景（海边码头 / 雨夜街头 / 室内窗边）与时间（清晨/黄昏/夜）",
			Operator: "add_scene",
			NeedsLLM: true,
		})
	}

	// 3) 镜头语言（只对视频要求；图片没有运镜）
	if isVideo {
		if _, ok := containsAny(trimmed, cameraWords); !ok {
			d.Defects = append(d.Defects, PromptDefect{
				Type:     DefectMissingCamera,
				Evidence: "没有景别/机位/运镜词",
				Fix:      "补一个镜头语言（中景跟拍 / 特写 / 俯拍推镜），否则模型会自己选机位，多镜之间容易跳轴",
				Operator: "add_camera",
			})
		}
		// 4) 音频段：H3 是音视频联合生成，缺音频描述会得到随机环境声
		if _, ok := containsAny(trimmed, audioWords); !ok {
			d.Defects = append(d.Defects, PromptDefect{
				Type:     DefectMissingAudio,
				Evidence: "没有环境声/配乐描述",
				Fix:      "补一句环境声（海浪拍岸、远处人声）与是否要配乐；H3 会按这段生成音轨",
				Operator: "add_audio",
			})
		}
	}

	// 5) 模糊修饰
	for _, w := range ambiguousWords {
		if strings.Contains(strings.ToLower(trimmed), strings.ToLower(w)) {
			d.Defects = append(d.Defects, PromptDefect{
				Type:     DefectAmbiguous,
				Evidence: w,
				Fix:      fmt.Sprintf("把「%s」换成可观测描述（光线方向、色温、材质、构图）——模型无法执行主观形容词", w),
				Operator: "replace_ambiguous",
			})
			break // 只报一次：同类问题报 5 条会淹没真正的缺口
		}
	}

	// 6) 过长
	if n := len([]rune(trimmed)); n > promptMaxRunes {
		d.Defects = append(d.Defects, PromptDefect{
			Type:     DefectOverlong,
			Evidence: fmt.Sprintf("%d 字，超过建议上限 %d", n, promptMaxRunes),
			Fix:      "拆成分镜表逐镜描述；单条提示词过长会让模型丢掉后半段的约束",
			Operator: "trim",
		})
	}

	d.Severity = severityOfDefects(d.Defects)
	d.Summary = summarizeDiagnosis(d.Defects)
	return d
}

func severityOfDefects(defects []PromptDefect) string {
	if len(defects) == 0 {
		return SeverityOK
	}
	for _, x := range defects {
		switch x.Type {
		case DefectMissingSubject, DefectMissingScene, DefectMissingAction:
			return SeverityFail
		}
	}
	return SeverityWarn
}

func summarizeDiagnosis(defects []PromptDefect) string {
	if len(defects) == 0 {
		return "提示词要素完整"
	}
	names := make([]string, 0, len(defects))
	for _, d := range defects {
		names = append(names, d.Type)
	}
	return fmt.Sprintf("%d 项待补：%s", len(defects), strings.Join(names, "、"))
}

var multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)

// ApplyOperator 应用一个定点修复算子，返回修复后的文本与说明。
//
// 只做**纯规则**算子；创作性算子（add_scene/add_action，NeedsLLM=true）返回错误，
// 由调用方决定是否交给 LLM——这样"哪一步动了什么"永远可追溯。
func ApplyOperator(text, operator, arg string) (string, string, error) {
	switch operator {
	case "trim":
		limit := promptMaxRunes
		if arg != "" {
			if n, err := parseIntSafe(arg); err == nil && n > 0 {
				limit = n
			}
		}
		r := []rune(text)
		if len(r) <= limit {
			return text, "未超长，无需裁剪", nil
		}
		// 按句子边界裁，避免把一句话砍半
		cut := string(r[:limit])
		if idx := strings.LastIndexAny(cut, "。！？.!?\n"); idx > limit/2 {
			cut = cut[:idx+1]
		}
		return strings.TrimSpace(cut), fmt.Sprintf("已裁到 %d 字（句子边界）", len([]rune(cut))), nil

	case "replace_ambiguous":
		if strings.TrimSpace(arg) == "" {
			return text, "", fmt.Errorf("replace_ambiguous 需要替换文本（arg 为空）")
		}
		out := text
		changed := 0
		for _, w := range ambiguousWords {
			if strings.Contains(strings.ToLower(out), strings.ToLower(w)) {
				out = strings.ReplaceAll(out, w, arg)
				changed++
			}
		}
		if changed == 0 {
			return text, "没有找到模糊修饰，未改动", nil
		}
		return out, fmt.Sprintf("替换了 %d 处模糊修饰", changed), nil

	case "normalize_space":
		out := multiSpaceRe.ReplaceAllString(text, " ")
		return out, "已合并多余空格", nil

	case "add_scene", "add_action", "add_camera", "add_audio":
		return text, "", fmt.Errorf("算子 %s 需要创作性内容（NeedsLLM），应由调用方交给 LLM 生成后再拼接", operator)

	case "require_nonempty":
		return text, "", fmt.Errorf("提示词为空：没有可修复的对象，必须由人重新填写")
	}
	return text, "", fmt.Errorf("未知算子 %q", operator)
}

func parseIntSafe(s string) (int, error) {
	n := 0
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("空数字")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("非数字 %q", s)
		}
		n = n*10 + int(c-'0')
		if n > 1<<20 {
			return 0, fmt.Errorf("数字过大")
		}
	}
	return n, nil
}

// DiagnoseGuide 把诊断结论翻译成「本次只允许修这几处」的约束文本，注入改写提示词。
//
// 为什么这样设计（而不是让 LLM 自由重写）：
//   - 诊断 ok（无缺陷）时返回空串 ⇒ 调用方不注入 ⇒ **旧行为完全不变**，
//     只有本来就有问题的提示词才会被约束改写；
//   - 明确列出"已存在的内容必须逐字保留" ⇒ 压住 LLM 顺手改人物/台词的冲动，
//     这也是多段片里台词被改写、身份漂移的一个来源。
func DiagnoseGuide(d PromptDiagnosis) string {
	if len(d.Defects) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【本次必须处理的问题（只改这些，不要重写已经写好的部分）】\n")
	for _, x := range d.Defects {
		// 带上算子名：出问题时能一眼看出「这一轮动的是哪个算子」，
		// 也是「一轮只动一个变量」纪律在日志里的落点。
		b.WriteString(fmt.Sprintf("- %s（算子 %s）：%s\n", x.Type, x.Operator, x.Fix))
	}
	b.WriteString("已存在的人物、场景、光线、台词、画面内文字必须逐字保留；不要新增未要求的元素。")
	return b.String()
}
