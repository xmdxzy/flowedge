package service

// 文本拆分（text_split）：把一段长文本按「句 / 段 / 固定段数 / 每段字数」切开。
//
// 为什么需要它 —— 2026-10-23 compose 标定里唯一剩下的失败是
// 「把一段长文本拆成 3 段分别配音」：模型编了一个不存在的 `split_text` 类型，
// 改 2 轮还是在编。`data_transform` 只能改结构（JSON 取字段、拼字符串），
// 不能按语义把一段中文切成几段，所以模型没有别的选择。
//
// 为什么不做 LLM 语义切分：切分规则必须是**确定性**的（同一段文本两次切出同样的结果），
// 否则一次 run 与另一次 run 的分段不同，评测就无从比较、产物也无法复现。
// 真要按语义切（比如"按剧情转折分幕"），前面接一个 llm 节点先改写文本，再来这里机械切分。

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"flowedge/backend/model"
)

const (
	splitModeSentence  = "by_sentence"  // 按句切开
	splitModeParagraph = "by_paragraph" // 按空行/换行切开
	splitModeCount     = "by_count"     // 合并成 N 段（尽量均匀）
	splitModeLength    = "by_length"    // 每段不超过 N 字
)

// 句末标点。中文里这些符号后面就是自然的断点。
// 光看 ASCII 的 .!? 会把中文整段当成一句 —— 这是最常见的切分错误。
const (
	splitHardStops = "。！？!?…"
	splitSoftStops = "；;"
)

// execTextSplitNode 拆分上游（或参数给的）文本。
func (e *EngineV2) execTextSplitNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	raw := strOf(node.Data["text"])
	if strings.TrimSpace(raw) == "" {
		raw = upstreamText(upstream)
	}
	if strings.TrimSpace(raw) == "" {
		// 不静默返回空：下游拿到空文本会一路跑下去（TTS 合成 0 秒、字幕空），
		// 最后表现为"成片某段没声音"，而根因在这里。宁可当场失败。
		return nil, fmt.Errorf("文本拆分为空：节点参数 text 与上游都没有给出文本")
	}

	mode := strings.ToLower(strings.TrimSpace(strOf(node.Data["mode"])))
	if mode == "" {
		mode = splitModeSentence
	}

	var parts []string
	switch mode {
	case splitModeParagraph:
		parts = splitParagraphs(raw)
	case splitModeCount:
		n := dataInt(node.Data, "count", 3)
		if n <= 0 {
			n = 3
		}
		parts = splitIntoCount(sentenceParts(raw), n)
	case splitModeLength:
		maxChars := dataInt(node.Data, "max_chars", 80)
		if maxChars <= 0 {
			maxChars = 80
		}
		parts = splitByLength(sentenceParts(raw), maxChars)
	case splitModeSentence:
		parts = sentenceParts(raw)
	default:
		return nil, fmt.Errorf("文本拆分：mode=%q 不认识，只能是 %s / %s / %s / %s",
			mode, splitModeSentence, splitModeParagraph, splitModeCount, splitModeLength)
	}

	if len(parts) == 0 {
		return nil, fmt.Errorf("文本拆分没有得到任何段落（输入 %d 字，mode=%s）", utf8.RuneCountInString(raw), mode)
	}

	// 上限保护：一段 10 万字的文本按句切能出几千段，parts 会把事件日志与任务书撑爆。
	// 超出就保留前 N 段并把剩余并进最后一段（不丢内容 —— 丢内容是最坏的一种"成功"）。
	const maxParts = 200
	overflow := 0
	if len(parts) > maxParts {
		overflow = len(parts) - maxParts
		parts = append(parts[:maxParts-1], strings.Join(parts[maxParts-1:], ""))
	}

	sep := strOf(node.Data["separator"])
	if sep == "" {
		sep = "\n"
	}

	outParts := make([]interface{}, 0, len(parts))
	for _, p := range parts {
		outParts = append(outParts, p)
	}

	out := map[string]interface{}{
		"text":   strings.Join(parts, sep),
		"parts":  outParts,
		"count":  len(parts),
		"chars":  utf8.RuneCountInString(raw),
		"mode":   mode,
		"source": "text_split",
	}
	if overflow > 0 {
		out["merged_overflow"] = overflow
	}
	return out, nil
}

// sentenceParts 按句切，**保留句末标点**。
//
// 保留标点不是洁癖：TTS 靠标点判断停顿与语调，切完把句号扔掉会让
// 每段听起来都像"半句话被突然掐断"。
func sentenceParts(s string) []string {
	var out []string
	var cur strings.Builder

	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		cur.WriteRune(r)

		switch {
		case strings.ContainsRune(splitHardStops, r):
			// 连续标点（"…！"、"？!"）算同一个句尾，吞掉后面紧跟的标点
			for i+1 < len(runes) && strings.ContainsRune(splitHardStops+splitSoftStops, runes[i+1]) {
				i++
				cur.WriteRune(runes[i])
			}
			// 标点后紧跟的右引号/右括号也要留在本句
			for i+1 < len(runes) && strings.ContainsRune("”’）)」』】》", runes[i+1]) {
				i++
				cur.WriteRune(runes[i])
			}
			flush()
		case r == '\n':
			flush()
		}
	}
	flush()
	return out
}

// splitParagraphs 按空行切。没有空行时退化为按行切。
//
// 为什么先找空行：中文长文的段落通常用空行分隔，而纯按换行切会把
// "一句话一行"的排版切成几十个碎段（那种排版在剧本里很常见）。
func splitParagraphs(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	blocks := strings.Split(s, "\n\n")
	var out []string
	for _, b := range blocks {
		if t := strings.TrimSpace(b); t != "" {
			out = append(out, t)
		}
	}
	if len(out) > 1 {
		return out
	}
	// 没有空行 → 按单换行切
	out = out[:0]
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	if len(out) > 0 {
		return out
	}
	// 连换行都没有 → 只能按句
	return sentenceParts(s)
}

// splitIntoCount 把句子合并成 n 段，按字数尽量均匀。
//
// 为什么不是"按字数硬切"：硬切会从句子中间切下去，导致某段以"他说"开头、
// 另一段以"我不同意"开头 —— 配音时听感是断的。先按句再合并，切点永远在句末。
func splitIntoCount(sents []string, n int) []string {
	if len(sents) == 0 {
		return nil
	}
	if n >= len(sents) {
		return sents
	}
	total := 0
	for _, s := range sents {
		total += utf8.RuneCountInString(s)
	}
	target := total / n
	if target <= 0 {
		target = 1
	}

	var out []string
	var cur strings.Builder
	curLen := 0
	for i, s := range sents {
		cur.WriteString(s)
		curLen += utf8.RuneCountInString(s)

		// 还差几段没分（含当前这段），每段应至少分到 remaining/target
		remainingParts := n - len(out) - 1
		remainingSents := len(sents) - i - 1

		// 到了目标字数就断；但必须给后面留够句子，否则最后一段会被塞爆
		if curLen >= target && remainingParts > 0 && remainingSents >= remainingParts {
			out = append(out, cur.String())
			cur.Reset()
			curLen = 0
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// splitByLength 每段不超过 maxChars，切点仍在句末。
// 单句本身就超长时（没有标点的长段落）不硬切 —— 硬切会把词切开。
func splitByLength(sents []string, maxChars int) []string {
	var out []string
	var cur strings.Builder
	curLen := 0
	for _, s := range sents {
		l := utf8.RuneCountInString(s)
		if curLen > 0 && curLen+l > maxChars {
			out = append(out, cur.String())
			cur.Reset()
			curLen = 0
		}
		cur.WriteString(s)
		curLen += l
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
