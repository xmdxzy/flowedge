package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"flowedge/backend/model"
)

// 择优节点（select）：候选图 → VLM 逐个打分 → 留最高分那张。
//
// 背景（2026-10-19 审计 D 类）：引擎早就能一次出 K 张图（text2image 的 batch），
// 但下游固定取 images[0] —— 等于抽了卡却不看牌。商业产品把「抽 3 挑 1」当卖点
// 因为每张都要花钱；本地抽卡只花时间，所以这应该是默认能力，而不是人工比对。
//
// 设计取舍：
//   - 只做「打分 + 挑」，不做「重抽」：重抽是 loop 节点的事，这里保持单一职责。
//   - 只有一个候选时**不打分**：为省一次扩散↔语言换侧（实测 5–10s），单张没得选。
//   - 模型给的 pass 只作参考，最终 pass 一律按 threshold 重算，避免阈值和模型判断打架。

const judgeInstruction = `你是出图质检员。严格按下面的「参考主题」判断这张图是否符合要求，并打分。
参考主题：%s
%s
只输出一个 JSON 对象，不要任何解释、不要 markdown 代码块：
{"score": 0-100 的整数, "reason": "一句话说明扣分点或亮点"}
评分口径：主体与参考主题一致 40 分、构图完整不被裁切 20 分、无乱码文字或水印 20 分、无肢体穿模或模糊 20 分。`

// buildJudgePrompt 组装打分提示词。
// reference 为空时必须显式说明——否则模型会「按画面自评」，结论和主题无关（AGENTS §6 的老坑）。
func buildJudgePrompt(reference, criteria string) string {
	ref := strings.TrimSpace(reference)
	if ref == "" {
		ref = "（未提供参考主题：只按画面完成度打分，并在 reason 里注明「未提供参考」）"
	}
	extra := ""
	if c := strings.TrimSpace(criteria); c != "" {
		extra = "额外要求：" + c
	}
	return fmt.Sprintf(judgeInstruction, ref, extra)
}

var judgeNumRe = regexp.MustCompile(`\b(\d{1,3})\b`)

// parseJudge 从模型输出里抠出分数与理由。
//
// 小模型经常带 ```json 包裹、前后闲话，甚至直接写「85 分，主体不错」，
// 所以：先试第一个 {...} 的 JSON，再退回「抓第一个 0–100 的数字」。
// 两种都失败才报 ok=false —— 让调用方决定是放弃还是当作 0 分。
func parseJudge(text string) (score int, reason string, ok bool) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		var raw struct {
			Score  float64 `json:"score"`
			Reason string  `json:"reason"`
		}
		if err := json.Unmarshal([]byte(text[start:end+1]), &raw); err == nil {
			s := int(raw.Score)
			if s < 0 {
				s = 0
			}
			if s > 100 {
				s = 100
			}
			return s, strings.TrimSpace(raw.Reason), true
		}
	}
	if m := judgeNumRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 0 && n <= 100 {
			return n, strings.TrimSpace(text), true
		}
	}
	return 0, strings.TrimSpace(text), false
}

// collectCandidates 收集候选图路径（保持顺序、去重、只认图片后缀）。
//
// 上游来源很多：text2image 的 image / images（batch 数组）、聚合节点的 values、
// 以及节点自己填的多行路径。全部收拢到一处，免得每接一种上游就改一次。
func collectCandidates(node *model.NodeInstance, upstream map[string]interface{}) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(strings.Trim(p, `"`))
		if p == "" || seen[p] || !looksLikeImagePath(p) {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	if node != nil {
		if s, ok := node.Data["images"].(string); ok {
			for _, line := range strings.Split(s, "\n") {
				add(line)
			}
		}
	}

	// upstream 是 map，直接 range 会让候选顺序随 Go 的 map 迭代随机变化——
	// 同一份图输入两次跑出两种顺序，择优结果不可复现，测试也会间歇性失败。
	// 排序规则不是纯字典序：**直接产出图的上游（image/images）优先于聚合节点
	// （values）**，同类内再按 key 排，保证顺序既确定又符合优先级语义。
	keys := make([]string, 0, len(upstream))
	for k := range upstream {
		keys = append(keys, k)
	}
	rankOf := func(m map[string]interface{}) int {
		if _, ok := m["image"]; ok {
			return 0
		}
		if _, ok := m["images"]; ok {
			return 0
		}
		return 1
	}
	sort.SliceStable(keys, func(i, j int) bool {
		mi, _ := upstream[keys[i]].(map[string]interface{})
		mj, _ := upstream[keys[j]].(map[string]interface{})
		if ri, rj := rankOf(mi), rankOf(mj); ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		up := upstream[k]
		m, ok := up.(map[string]interface{})
		if !ok {
			continue
		}
		if s, ok := m["image"].(string); ok {
			add(s)
		}
		addAny := func(v interface{}) {
			switch t := v.(type) {
			case string:
				add(t)
			case []string:
				for _, s := range t {
					add(s)
				}
			case []interface{}:
				for _, x := range t {
					if s, ok := x.(string); ok {
						add(s)
					}
				}
			}
		}
		for _, k := range []string{"images", "values", "files", "parts"} {
			if v, ok := m[k]; ok {
				addAny(v)
			}
		}
	}
	return out
}

// pickBest 选出最高分那条（纯函数，便于测试）。同分取靠前的——顺序即生成顺序。
func pickBest(scores []int) (idx, best int) {
	idx, best = 0, -1
	for i, s := range scores {
		if s > best {
			idx, best = i, s
		}
	}
	return idx, best
}

// referenceFrom 取「这张图本来该画什么」：节点配置优先，其次上游的 prompt / original_prompt / text。
func referenceFrom(node *model.NodeInstance, upstream map[string]interface{}) string {
	if node != nil {
		if s, ok := node.Data["reference"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	for _, up := range upstream {
		m, ok := up.(map[string]interface{})
		if !ok {
			continue
		}
		for _, k := range []string{"original_prompt", "prompt", "text"} {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				// text 可能是正文而不是提示词，只在看起来像提示词的短文本时才用
				if k == "text" && len([]rune(strings.TrimSpace(s))) > 400 {
					continue
				}
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

type judgeItem struct {
	Path   string `json:"path"`
	Score  int    `json:"score"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// execSelectNode 择优：K 个候选 → 逐个让 VLM 打分 → 输出最高分。
func (e *EngineV2) execSelectNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	cands := collectCandidates(node, upstream)
	if len(cands) == 0 {
		return nil, fmt.Errorf("择优节点没有候选图：接一个「文生图」（batch>1）或聚合节点，或在 images 里一行填一条路径")
	}
	threshold := int(fieldFloat(node, "threshold", 60))
	reference := referenceFrom(node, upstream)
	criteria, _ := node.Data["criteria"].(string)

	// 单候选不打分：不为此付一次换侧代价（5–10s），也没有"挑"的意义
	if len(cands) == 1 {
		return map[string]interface{}{
			"image":      cands[0],
			"score":      0,
			"passed":     true,
			"scored":     0,
			"candidates": toJSON([]judgeItem{{Path: cands[0], Reason: "唯一候选，未打分"}}),
			"reference":  reference,
			"note":       "只有一个候选，跳过打分（省一次语言侧换侧）",
		}, nil
	}

	if e.llm == nil {
		return nil, fmt.Errorf("择优需要视觉模型，但 LLM 服务未初始化")
	}
	modelName, merr := e.resolveVisionModel(node)
	if merr != nil {
		return nil, merr
	}

	prompt := buildJudgePrompt(reference, criteria)
	items := make([]judgeItem, 0, len(cands))
	scores := make([]int, 0, len(cands))

	// 打分是语言侧：先把扩散侧让出去，否则 flux 17GB + VLM 6GB 会贴着 24GB 打转
	release := e.enterSide(sideLanguage)
	defer release()

	for _, p := range cands {
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("已取消（已评 %d/%d 张）：%w", len(items), len(cands), cerr)
		}
		text, _, err := e.llm.VisionChat(prompt, []string{p}, modelName)
		if err != nil {
			items = append(items, judgeItem{Path: p, Score: 0, Error: err.Error()})
			scores = append(scores, 0)
			continue
		}
		score, reason, ok := parseJudge(text)
		if !ok {
			items = append(items, judgeItem{Path: p, Score: 0, Reason: reason, Error: "打分输出无法解析"})
			scores = append(scores, 0)
			continue
		}
		items = append(items, judgeItem{Path: p, Score: score, Reason: reason})
		scores = append(scores, score)
	}

	idx, best := pickBest(scores)
	return map[string]interface{}{
		"image":      items[idx].Path,
		"score":      best,
		"passed":     best >= threshold,
		"threshold":  threshold,
		"picked":     idx,
		"scored":     len(cands),
		"candidates": toJSON(items),
		"reference":  reference,
		"model":      modelName,
		"note":       noteForPick(best, threshold, idx),
	}, nil
}

// noteForPick 把结论说人话，免得用户只看数字不知道自己该干嘛
func noteForPick(best, threshold, idx int) string {
	if best >= threshold {
		return fmt.Sprintf("最高 %d 分（阈值 %d）→ 采用第 %d 张", best, threshold, idx+1)
	}
	return fmt.Sprintf("全部低于阈值 %d（最高 %d 分）→ 建议改提示词或加候选后重跑", threshold, best)
}
