package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"flowedge/backend/model"
)

// 文本梯度：用一次模型调用，把「哪条检查在漏」变成「这一版该怎么改」。
//
// 为什么不是 RL 梯度（借 Agent Lightning 时做的取舍，见 docs/自进化-*）
//
//	本地没有可训练权重：24G 卡跑不动 rollout 级别的参数更新；
//	FlowEdge 的"策略"是**图与提示词**，不是权重。文本梯度（DSPy/GEPA 路线）
//	才是对得上的优化器：把反馈写成文字，让模型提出下一个版本，再用配对证据选择。
//
// 与规则表的分工（candidate.go candidateRules）
//
//	规则只会提**有行业标准值**的改动（-14 LUFS / 1.0s 静音缺口）。
//	覆盖不到的部分（提示词怎么写、九宫格锁句怎么排、哪个模型档位）没有标准答案，
//	只能靠模型提假设 —— 但必须过同一套审计，且**永不自动晋升**。

// VariantEdit 模型提出的一处改动。
//
// from 不由模型给：模型经常把当前值抄错（"我把 0.18 改成 0.12"而实际是 0.3），
// 让模型决定 from 就会得到一批"看着合理、一校验就全废"的候选。
// from 由引擎从图上读，模型的 to 才是它真正的主张。
type VariantEdit struct {
	NodeID string      `json:"nodeId"`
	Key    string      `json:"key"`
	From   interface{} `json:"from"`
	To     interface{} `json:"to"`
	Why    string      `json:"why"`
}

// TextualVariant 一个文本梯度变体
type TextualVariant struct {
	Title      string        `json:"title"`
	Hypothesis string        `json:"hypothesis"`
	Expected   string        `json:"expected,omitempty"`
	Edits      []VariantEdit `json:"edits"`
	// EditsAsCandidate 是可直接喂给 /evidence/trial 的形态（path 已拼好、from 已校正）
	EditsAsCandidate []CandidateEdit `json:"editsAsCandidate,omitempty"`

	// Verdict accepted / rejected / stripped
	Verdict    string   `json:"verdict"`
	Reason     string   `json:"reason,omitempty"`
	Hacks      []string `json:"hacks,omitempty"`
	Stripped   []string `json:"stripped,omitempty"`
	Projection string   `json:"projection,omitempty"`
}

// VariantRequest 生成变体的输入
type VariantRequest struct {
	Nodes  []model.NodeInstance
	Attrs  []Attribution
	Max    int
	Prompt string // 可选：额外目标（例如"这次只调后期不动生成"）
}

// BuildVariantPrompt 组装提示词。
//
// 提示词本身就是"门"：把可调面清单、失分归因、禁止项写死在里面，
// 比事后过滤更省一次往返 —— 但事后过滤仍然要做（模型不遵守时不能放行）。
func BuildVariantPrompt(req VariantRequest) string {
	max := req.Max
	if max <= 0 || max > 5 {
		max = 3
	}
	var b strings.Builder
	b.WriteString("你是 FlowEdge 工作流引擎的优化器。你的任务是根据质检门的失分归因，提出下一版工作流的参数改动。\n\n")

	b.WriteString("## 硬规则（违反任何一条，该变体会被机器直接丢弃）\n")
	b.WriteString("1. 只能改下面「可调面」清单里列出的参数，路径必须是 节点ID.参数名。\n")
	b.WriteString("2. 严禁改动任何质检门上的判据（例如 loudness_min_db、min_frames、require_audio）。改判据不是改进，是改评分标准。\n")
	b.WriteString("3. 严禁删除节点、严禁减少步骤。\n")
	b.WriteString("4. 不要输出坐标、不要输出连线、不要输出解释性前后缀，只输出 JSON。\n")
	b.WriteString("5. 每个变体只针对一条失分检查，别把三件事塞进一个变体（那样配对证据分不清是谁起的作用）。\n\n")

	b.WriteString("## 可调面（当前真实值，from 由引擎校正，你只需给 to）\n")
	surface := TrainableSurface(req.Nodes)
	sort.Slice(surface, func(i, j int) bool {
		if surface[i].NodeID != surface[j].NodeID {
			return surface[i].NodeID < surface[j].NodeID
		}
		return surface[i].Key < surface[j].Key
	})
	for _, k := range surface {
		if k.GateLocked {
			continue
		}
		opts := ""
		if len(k.Options) > 0 {
			opts = " 可选值：" + strings.Join(k.Options, " / ")
		}
		fmt.Fprintf(&b, "- %s.（%s，%s）= %s%s\n", k.NodeID, k.NodeType, k.Key, jsonText(k.Current), opts)
	}

	b.WriteString("\n## 门在漏什么（按失分占比排序）\n")
	attrs := append([]Attribution(nil), req.Attrs...)
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].LossShare > attrs[j].LossShare })
	shown := 0
	for _, a := range attrs {
		if a.Failed == 0 || shown >= 6 {
			continue
		}
		shown++
		fmt.Fprintf(&b, "- 检查 %q（门节点 %s，层级 %s）：%d/%d 次不过，占全部失分 %.0f%%，判定 %s\n",
			a.GateCheck, orDash(a.NodeID), orDash(a.Layer), a.Failed, a.Total, a.LossShare*100, a.Verdict)
		if len(a.Knobs) > 0 {
			var ks []string
			for _, k := range a.Knobs {
				// 门上的判据在这里也要挡掉：归因里带上它们是为了让人看见
				// "这条门按多少判"，写进提示词等于把作弊选项摆到模型面前。
				if k.GateLocked {
					continue
				}
				ks = append(ks, fmt.Sprintf("%s.%s=%s", k.NodeID, k.Key, jsonText(k.Current)))
			}
			if len(ks) > 0 {
				fmt.Fprintf(&b, "  相关旋钮：%s\n", strings.Join(ks, "，"))
			}
		}
	}
	if shown == 0 {
		b.WriteString("- （没有可归因的失分：门全过，别再改参数了）\n")
	}
	if strings.TrimSpace(req.Prompt) != "" {
		fmt.Fprintf(&b, "\n## 本次额外目标\n%s\n", strings.TrimSpace(req.Prompt))
	}

	fmt.Fprintf(&b, `
## 输出格式
{"variants":[{"title":"不超过20字的改动名","hypothesis":"为什么这么改能过这条检查","expected":"预期改善哪条检查","edits":[{"nodeId":"节点ID","key":"参数名","to":新值,"why":"一句依据"}]}]}

最多 %d 个变体。`, max)
	return b.String()
}

// ProposeTextualVariants 调一次模型，产出**已过审计**的变体清单。
//
// llm 为空表示模型没配：直接报错，不许退化成"返回空清单"——
// 空清单会被上层误读成"模型认为不需要改"，那是两个完全相反的事实。
func ProposeTextualVariants(req VariantRequest, llm ComposeLLM) ([]TextualVariant, error) {
	if llm == nil {
		return nil, fmt.Errorf("LLM 不可用：文本梯度需要一次模型调用（检查 Ollama 是否在跑）")
	}
	prompt := BuildVariantPrompt(req)
	raw, err := llm(prompt)
	if err != nil {
		return nil, fmt.Errorf("模型调用失败：%w", err)
	}
	variants, perr := ParseVariants(raw)
	if perr != nil {
		return nil, perr
	}

	byID := map[string]*model.NodeInstance{}
	for i := range req.Nodes {
		byID[req.Nodes[i].ID] = &req.Nodes[i]
	}

	out := make([]TextualVariant, 0, len(variants))
	for _, v := range variants {
		// from 一律用图上的真实值；模型给的 from 只当"它以为的"，不作数
		var edits []CandidateEdit
		var bad []string
		for i := range v.Edits {
			e := &v.Edits[i]
			n := byID[e.NodeID]
			if n == nil {
				bad = append(bad, fmt.Sprintf("节点 %q 不存在", e.NodeID))
				continue
			}
			// 键名容错：模型常写成 "input_source.path" / "data.path"（把节点类型或
			// 路径前缀当成了键名的一部分）。剥一层再查，剥完必须真实存在 ——
			// 容错的是写法，不是"图上没有的参数也能加"。
			key := strings.TrimPrefix(strings.TrimSpace(e.Key), "data.")
			if _, ok := n.Data[key]; !ok {
				if dot := strings.Index(key, "."); dot >= 0 {
					if _, ok2 := n.Data[key[dot+1:]]; ok2 {
						key = key[dot+1:]
					}
				}
			}
			cur, ok := n.Data[key]
			if !ok {
				var have []string
				for k := range n.Data {
					if nodeDataInternalKeys[k] {
						continue
					}
					have = append(have, k)
				}
				sort.Strings(have)
				bad = append(bad, fmt.Sprintf("节点 %s 上没有参数 %q（可用：%s）",
					e.NodeID, e.Key, strings.Join(have, ", ")))
				continue
			}
			e.Key = key
			e.From = cur // 模型给的 from 不作数，一律用图上的真实值
			edits = append(edits, CandidateEdit{
				NodeID: e.NodeID, Path: "data." + key, From: cur, To: e.To, Why: e.Why,
			})
		}
		if len(edits) == 0 {
			v.Verdict = "rejected"
			v.Reason = "没有一条编辑落在真实参数上：" + strings.Join(bad, "；")
			out = append(out, v)
			continue
		}

		kept, dropped := FilterGateJudgementEdits(req.Nodes, edits)
		for _, d := range dropped {
			v.Stripped = append(v.Stripped, d.Path)
		}
		if len(kept) == 0 {
			v.Verdict = "rejected"
			v.Reason = "全部编辑都在改门判据（" + strings.Join(v.Stripped, "，") + "）：这是改评分标准"
			out = append(out, v)
			continue
		}

		specEdits := make([]SpecEdit, 0, len(kept))
		for _, e := range kept {
			specEdits = append(specEdits, SpecEdit{
				NodeID: e.NodeID, Path: e.Path, From: e.From, To: e.To, Reason: e.Why,
			})
		}
		next, _, probs := ApplySpecEdits(req.Nodes, specEdits, ApplyOptions{AllowGateChange: true, DryRun: true})
		if len(probs) > 0 {
			var msgs []string
			for _, p := range probs {
				msgs = append(msgs, p.Detail)
			}
			v.Verdict = "rejected"
			v.Reason = "编辑校验不过：" + strings.Join(msgs, "；")
			out = append(out, v)
			continue
		}
		hacks, _ := AuditVariant(req.Nodes, next)
		if len(hacks) > 0 {
			v.Verdict = "rejected"
			v.Hacks = hacks
			v.Reason = "变体触到了防作弊线（" + strings.Join(hacks, "；") + "）"
			out = append(out, v)
			continue
		}

		v.EditsAsCandidate = kept
		v.Projection = ProjectionFor(req.Nodes, specEdits)
		v.Verdict = "accepted"
		if len(v.Stripped) > 0 {
			v.Reason = "已摘掉改判据的编辑：" + strings.Join(v.Stripped, "，")
		}
		out = append(out, v)
	}
	return out, nil
}

// ParseVariants 从模型输出里取出 JSON。
//
// 不假设模型干净：常见是包在 ```json 里，或者前后带一句"好的，以下是…"。
// 找不到 JSON 就报错 —— 不许返回空清单冒充"模型没建议"。
func ParseVariants(raw string) ([]TextualVariant, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, fmt.Errorf("模型返回空文本")
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("模型输出里没有 JSON 对象：%s", clip(text, 200))
	}
	var payload struct {
		Variants []TextualVariant `json:"variants"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &payload); err != nil {
		return nil, fmt.Errorf("模型输出的 JSON 解析失败：%w（原文：%s）", err, clip(text, 200))
	}
	if len(payload.Variants) == 0 {
		return nil, fmt.Errorf("模型给了 JSON 但 variants 为空 —— 这可能是「它认为不该改」，也可能是它没听懂；" +
			"两种情况都只能由人看一次原文再决定，不能当成「无需改动」")
	}
	return payload.Variants, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
