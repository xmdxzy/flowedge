package service

// VLM 判词协议：先证据、后等级
//
// 由来（2026-10-31 论文深读）：Q-Align (arXiv 2312.17090) 用「离散文本等级」代替连续分数，
// 显著降低同一输入多次打分的抖动；FIRM-Video (arXiv 2608.21839) 要求裁判先给可核验的证据
// 再落结论。FE 的现状是「technical 全绿但画面崩」——帧数/响度/静音/色温全过，人物却换了脸，
// 因为现有门只量**存在性**，不量**内容对不对**。
//
// 本文件只做协议与解析，不发起任何网络调用：
//   - BuildJudgePrompt：把「先证据后等级」写死进模板（模板是契约，改了要跑测试）
//   - ParseVLMVerdict：宽容解析 LLM 输出（它会乱写，解析器不能跟着乱）
//   - GateCheck：把等级映射成门结论（**解析失败一律不算通过**）
//
// 刻意不做：不把 VLM 判词塞进 runGateChecks。那里是纯媒体探测（同步、无外部依赖），
// 塞进去会让「门」变成「有时要等 30 秒 Ollama」的东西。调用方显式触发。

import (
	"fmt"
	"regexp"
	"strings"
)

// 五档等级（与 Q-Align 的离散等级对齐，不用 1-5 分数：分数会飘）
const (
	GradeExcellent = "excellent"
	GradeGood      = "good"
	GradeFair      = "fair"
	GradePoor      = "poor"
	GradeBad       = "bad"
)

// VLMVerdict 一次判词的结论。
type VLMVerdict struct {
	Grade    string   `json:"grade"` // excellent/good/fair/poor/bad；空 = 解析失败
	Evidence []string `json:"evidence"`
	Defects  []string `json:"defects"`
	Fix      string   `json:"fix"`
	Raw      string   `json:"raw,omitempty"`
	ParseOK  bool     `json:"parse_ok"`
}

// BuildJudgePrompt 生成判词提示词。
//
// 三条硬约束（都有理由，别删）：
//  1. **先列证据再给等级**：先给结论的模型会事后编证据（论文实测抖动来源之一）；
//  2. **证据必须带帧号**：没有帧号的「画面清晰」无法核验，等于没写；
//  3. **禁止「可能/似乎」**：看不清就说看不清，模糊措辞会让等级失去意义。
func BuildJudgePrompt(topic string, frameLabels []string) string {
	if strings.TrimSpace(topic) == "" {
		topic = "画面内容与分镜描述一致、人物身份稳定、无明显畸形或穿模"
	}
	frames := "（未提供帧标签）"
	if len(frameLabels) > 0 {
		frames = strings.Join(frameLabels, "、")
	}
	var b strings.Builder
	b.WriteString("你是影视质检员。只根据提供的画面帧判断，不要猜测声音、剧情或未展示的内容。\n\n")
	b.WriteString("判断目标：" + topic + "\n")
	b.WriteString("本次提供：" + frames + "\n\n")
	b.WriteString("输出格式（严格按顺序，**不要先给结论**）：\n")
	b.WriteString("1) 证据：逐帧列出你**实际看到**的内容，每行格式 `- [帧N] 事实`，至少 3 条。\n")
	b.WriteString("   只写可见事实：人物、服饰、动作、光线、背景、画面内文字。\n")
	b.WriteString("2) 缺陷：列出与目标冲突之处，每行格式 `- [帧N] 缺陷`；没有就写 `- 无`。\n")
	b.WriteString("3) 等级：从 excellent / good / fair / poor / bad 中选**一个**，单独一行写 `等级：<词>`。\n")
	b.WriteString("4) 修复：一句可直接执行的动作（改哪个参数、重拍哪一镜）；没有就写 `修复：无`。\n\n")
	b.WriteString("禁止：在列完证据前给出等级；用「可能/似乎/应该是」描述看不清的内容。\n")
	return b.String()
}

// 行首前缀：允许 markdown 列表符、引用符、编号（"3) "）、顿号等。
const linePrefix = `^[\s>*#\d.)、-]*`

var (
	gradeRe    = regexp.MustCompile(`(?im)` + linePrefix + `等\s*级\s*[:：]\s*\**\s*(excellent|good|fair|poor|bad)`)
	gradeEnRe  = regexp.MustCompile(`(?im)` + linePrefix + `grade\s*[:：]\s*\**\s*(excellent|good|fair|poor|bad)`)
	evidenceRe = regexp.MustCompile(`(?i)^\s*[-*•]?\s*\[\s*(?:帧|frame|f)\s*(\d+)\s*\]\s*(.+)$`)
	fixRe      = regexp.MustCompile(`(?im)` + linePrefix + `修\s*复\s*[:：]\s*(.+)$`)
)

// ParseVLMVerdict 解析判词。
//
// 宽容策略：LLM 会加粗、换冒号、写英文、在等级后面补理由 —— 都能认。
// 但**认不出等级就 ParseOK=false**，绝不猜一个默认值：猜成 good 等于把门关掉。
func ParseVLMVerdict(raw string) VLMVerdict {
	v := VLMVerdict{Raw: raw}
	if strings.TrimSpace(raw) == "" {
		return v
	}
	text := strings.ReplaceAll(raw, "\r\n", "\n")

	if m := gradeRe.FindStringSubmatch(text); len(m) == 2 {
		v.Grade = strings.ToLower(m[1])
	} else if m := gradeEnRe.FindStringSubmatch(text); len(m) == 2 {
		v.Grade = strings.ToLower(m[1])
	}
	v.ParseOK = v.Grade != ""

	// 证据与缺陷用同一种行格式，靠位置归类：
	// 「缺陷」小节之后、等级行之前的带帧号条目算缺陷；等级行之前的其余条目算证据。
	gradeIdx := -1
	if loc := gradeRe.FindStringIndex(text); loc != nil {
		gradeIdx = loc[0]
	} else if loc := gradeEnRe.FindStringIndex(text); loc != nil {
		gradeIdx = loc[0]
	}
	defectIdx := strings.Index(text, "缺陷")

	offset := 0
	for _, line := range strings.Split(text, "\n") {
		lineStart := offset
		offset += len(line) + 1
		m := evidenceRe.FindStringSubmatch(line)
		if len(m) != 3 {
			continue
		}
		body := strings.TrimSpace(m[2])
		item := fmt.Sprintf("[帧%s] %s", m[1], body)
		if defectIdx >= 0 && lineStart > defectIdx && (gradeIdx < 0 || lineStart < gradeIdx) {
			if body == "无" || body == "none" || body == "None" {
				continue
			}
			v.Defects = append(v.Defects, item)
			continue
		}
		if gradeIdx >= 0 && lineStart > gradeIdx {
			continue // 等级之后的内容不算证据（可能是修复建议里的帧号）
		}
		v.Evidence = append(v.Evidence, item)
	}

	if m := fixRe.FindStringSubmatch(text); len(m) == 2 {
		fix := strings.TrimSpace(strings.Trim(m[1], "*` "))
		if fix != "" && fix != "无" && fix != "none" && fix != "None" {
			v.Fix = fix
		}
	}
	return v
}

// GateCheck 把判词映射成门结论。
//
// 映射规则（与「不可达 ≠ 空结果」同一条纪律）：
//   - excellent/good → 通过
//   - fair           → 不通过但只 warn（可以交付，但要人看一眼）
//   - poor/bad       → 不通过且 fail（拦住）
//   - 解析失败       → 不通过且 warn，**并明说"判词不可解析"**
//
// 最后一条是关键：解析失败绝不能被当成通过，否则一次模板改动就能让门静默失效。
func (v VLMVerdict) GateCheck(name string) gateCheck {
	if name == "" {
		name = "vlm_content"
	}
	c := gateCheck{Name: name}
	switch v.Grade {
	case GradeExcellent, GradeGood:
		c.OK = true
		c.Level = "warn"
		c.Detail = fmt.Sprintf("VLM 等级 %s（证据 %d 条）", v.Grade, len(v.Evidence))
		if v.Fix != "" {
			c.Fix = v.Fix
		}
	case GradeFair:
		c.OK = false
		c.Level = "warn"
		c.Detail = fmt.Sprintf("VLM 等级 fair：可交付但建议人看。证据 %d 条，缺陷 %d 条",
			len(v.Evidence), len(v.Defects))
		c.Fix = firstNonEmptyStr(v.Fix, "按缺陷行逐条核对；要重拍只重跑对应镜（shotlist 定点重拍）")
	case GradePoor, GradeBad:
		c.OK = false
		c.Level = "fail"
		c.Detail = fmt.Sprintf("VLM 等级 %s：不应交付。缺陷：%s", v.Grade, strings.Join(v.Defects, "；"))
		c.Fix = firstNonEmptyStr(v.Fix, "按缺陷行定位镜次后定点重拍；若证据指向参考图不足，先补 ref_images")
	default:
		c.OK = false
		c.Level = "warn"
		c.Detail = "VLM 判词不可解析（没有出现「等级：excellent|good|fair|poor|bad」）——按未通过处理，不按通过处理"
		c.Fix = "检查判词模板是否被改动（BuildJudgePrompt 的格式契约有测试钉住），或换一个更听话的本地 VLM"
	}
	return c
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// VLMGradeRank 把等级映射成可比较的序号（越大越好），便于跨版本比较与聚合。
// 解析失败返回 -1：聚合时**排除**而不是当 0（0 会被误读成"最差"，与"没测"混淆）。
func VLMGradeRank(grade string) int {
	switch strings.ToLower(strings.TrimSpace(grade)) {
	case GradeBad:
		return 0
	case GradePoor:
		return 1
	case GradeFair:
		return 2
	case GradeGood:
		return 3
	case GradeExcellent:
		return 4
	}
	return -1
}
