package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"flowedge/backend/model"
)

// 自进化内核：把「一次运行的事实」变成「下一版引擎该改哪一处」。
//
// 借鉴与不借鉴（评估原文见 docs/自进化-AgentLightning取舍与FlowEdge-Evo-v1.md）
//
//	Agent Lightning v1.0 的三组件（Gateway / Rollout Controller / Trainer）与它
//	"agent 零改动、真实 harness 在回路里"的主张，骨架是对的，这里照搬三件事：
//
//	  ① Gateway 式旁路采集 —— 但 FlowEdge 的拦截点是**节点收尾**而不是 LLM 代理。
//	     好处是不管调用方走不走 chat/completions 都能采到；坏处是粒度比 span 粗。
//	     这恰好换来一样 Agent Lightning 没有的东西：门是节点级输出，**奖励自带归因**。
//	     Agent Lightning v1.0 明说"computes advantages at the rollout level"——
//	     它只在整条轨迹上算优势，不细分到某次调用。我们不用细分，门已经细了。
//
//	  ② rollout 记录与版本绑定 —— 对应 Evidence.WorkflowVersion。没有它就只能按
//	     时间窗口猜"改前改后"，那是最容易得出反向结论的地方。
//
//	  ③ 多臂对比（GRPO 在官方文档里的用法就是"同一输入跑多个 rollout 比 reward"）。
//	     这里换成同一 case 的 baseline / candidate 配对，比的是门分而不是梯度。
//
//	明确不借鉴三件事（照搬会死，理由逐条写在 docs 里）：
//
//	  ① 梯度下降 —— 本地没有可训练的 policy 权重（24G 卡跑不了 9B 的 RL），
//	     也没有"6K 样本"这种量级。这里换成**文本梯度 + 配对选择**（DSPy/GEPA 路线）：
//	     改动是参数值/提示词，验证手段是门。
//	  ② 海量免费 rollout —— Agent Lightning 假设 rollout 便宜（沙箱里跑代码，秒级）。
//	     这里一次真 rollout 是分钟到小时级且抢显存。所以必须先过三级投影，
//	     只有 L0/L1 通过才允许烧 GPU。
//	  ③ 无限并发 attempt —— 本机显存单侧驻留（AGENTS §2），并发是资源问题不是算法问题。
//
// 加了一条 Agent Lightning 不需要、但这里必须有的东西：**reward hacking 防线**。
//
//	它的 reward 在 agent 之外（测试用例是外部的），agent 改不到。
//	FlowEdge 的门就在同一张图里 —— 于是"自进化"最先学会的一定是**放宽门阈值**，
//	分数立刻上涨而质量不变。这不是理论风险：candidate.go 的候选规则里
//	loudness / max_silence_gap / fps 全是门层参数。所以 AuditVariant 把
//	"动评分标准"和"提高质量"硬分开，前者一律不判晋升。

// 自进化候选状态机。
//
// 与序策 %APPDATA%\Xuce\SelfEvolution\registry.json 的那套同构，不自造第二套语义：
// 那边是 proposed → evaluated → shadowing → promoted / rejected / rolled-back。
// 这里少一个 shadowing：FlowEdge 的「影子」就是跑在同一批 case 上的另一次真 run，
// 与 evaluated 是同一动作，多一档只会让人停在中间态。
const (
	EvoProposed   = "proposed"    // 已生成变体，未测
	EvoEvaluated  = "evaluated"   // 配对评测跑完，有结论
	EvoPromoted   = "promoted"    // 已落回图，成为新基线
	EvoRejected   = "rejected"    // 测了不达标
	EvoRolledBack = "rolled_back" // 晋升后被回归监测打回
)

// 投影等级：验证一个变体要花多少代价。
//
// 为什么必须分级：Agent Lightning 的 rollout 便宜到可以闭眼跑一千次，
// 所以它不需要这一层。这里一次 L2 要 120s 起，还要抢 17GB 显存 ——
// 没有分级，"自进化"会变成"每晚烧八小时电，早上看四个候选全挂"。
const (
	ProjectionFree = "L0_structure" // 秒级、零显存：结构分 + 契约 + 端口
	ProjectionDry  = "L1_dryrun"    // 秒级、零显存：依赖预检 + 预算估算 + spec 快照
	ProjectionReal = "L2_render"    // 分钟级起、占显存：真渲一次并过门
)

// ProjectionFor 判断改一处参数至少要跑到哪一级才能有结论。
//
// 判据只看「这个参数会不会改变实际算子的工作量」，不看节点类型字面量 ——
// 与 spec.go / preflight.go 同一条纪律。会改工作量的（步数/时长/分辨率/模型）
// 必须真渲；纯文本与开关类可以先干跑。
func ProjectionFor(nodes []model.NodeInstance, edits []SpecEdit) string {
	heavy := heuristicHeavyKeys()
	for _, e := range edits {
		key := strings.TrimPrefix(e.Path, "data.")
		if i := strings.Index(key, "."); i >= 0 {
			key = key[:i]
		}
		if heavy[key] {
			return ProjectionReal
		}
		for i := range nodes {
			if nodes[i].ID != e.NodeID {
				continue
			}
			// 改模型 = 换整条链路的显存与耗时，必须实跑
			if key == "model" || key == "backend" {
				return ProjectionReal
			}
		}
	}
	return ProjectionDry
}

// heuristicHeavyKeys 会显著改变渲染成本的参数名。
// 用词表而不是"哪个节点"：新增节点只要参数名沿用这些词就自动覆盖。
func heuristicHeavyKeys() map[string]bool {
	return map[string]bool{
		"steps": true, "cfg": true, "width": true, "height": true,
		"seconds": true, "target_seconds": true, "fps": true,
		"segments": true, "frames": true, "num_frames": true,
		"resolution": true, "batch_size": true, "max_iterations": true,
	}
}

// Knob 一处可调旋钮。
type Knob struct {
	NodeID   string      `json:"nodeId"`
	NodeType string      `json:"nodeType"`
	Title    string      `json:"title"`
	Key      string      `json:"key"`
	Current  interface{} `json:"current"`
	Kind     string      `json:"kind"` // numeric / enum / text / bool
	Options  []string    `json:"options,omitempty"`
	// GateLocked 门节点上的判据：改它等于改评分标准，**永不自动调**。
	// 这一位不是提示，AuditVariant 会硬拦。
	GateLocked bool `json:"gateLocked"`
	// GateRelated 参数名属于门层词汇表，但长在**产出节点**上（如 post_production.loudness）。
	// 这类是"改善产物的手段"，允许调；只是改完必须说明理由（沿用 spec/apply 的门锁语义）。
	//
	// 为什么必须把两者分开：gateParamKeys 是一张扁平表，
	// 门上的 loudness_min_db（判据）和后期上的 loudness（归一目标）同名不同质。
	// 不区分就会得出「只要名字出现在门词汇表里就一律不许调」——
	// 那样连"把响度归一到 -14"这条唯一有标准答案的改进都会被自己拦掉。
	GateRelated bool `json:"gateRelated"`
}

// IsGateParam 参数名是否出现在门层词汇表里。
//
// 直接查 spec.go 的 gateParamKeys（第二真相源必漂移），不另立名单。
// 注意：命中只说明"名字属于门层词汇"，是否真的不能调要看它长在哪个节点上。
func IsGateParam(key string) bool {
	return gateKeySet()[key]
}

// TrainableSurface 列出图里所有可调旋钮。
//
// 从 Modules[type].KeyParams 派生，不手写清单：
// 加一个节点只要登记了 KeyParams，它就自动进入可进化面。
// 只读图里**实际写过**的参数（Data 里没有的键不列）—— 没写过的键值等于 0，
// 与其猜一个默认值，不如承认"这个参数没人设过"。
func TrainableSurface(nodes []model.NodeInstance) []Knob {
	var out []Knob
	for i := range nodes {
		n := &nodes[i]
		mod, ok := ModuleContractOf(n.Type)
		if !ok {
			continue
		}
		isGateNode := mod.Role == RoleGate
		for _, kp := range mod.KeyParams {
			cur, exists := n.Data[kp.Key]
			if !exists || cur == nil {
				continue
			}
			// 内部状态字段混在 Data 里是不该的，但先挡掉，避免把 status 当参数调
			if nodeDataInternalKeys[kp.Key] {
				continue
			}
			out = append(out, Knob{
				NodeID:      n.ID,
				NodeType:    n.Type,
				Title:       mod.Title,
				Key:         kp.Key,
				Current:     cur,
				Kind:        knobKind(cur, kp.Options),
				Options:     kp.Options,
				GateLocked:  isGateNode && IsGateParam(kp.Key),
				GateRelated: !isGateNode && IsGateParam(kp.Key),
			})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].NodeID != out[b].NodeID {
			return out[a].NodeID < out[b].NodeID
		}
		return out[a].Key < out[b].Key
	})
	return out
}

func knobKind(v interface{}, options []string) string {
	if len(options) > 0 {
		return "enum"
	}
	switch v.(type) {
	case bool:
		return "bool"
	case float64, float32, int, int64, int32:
		return "numeric"
	default:
		return "text"
	}
}

// AuditVariant 检查一个变体是不是在**改评分标准**而不是提高质量。
//
// hacks 非空即拒绝晋升，没有"警告后放行"这一档 —— 放宽验收的收益永远大于提高质量，
// 一旦允许"警告后放行"，进化会稳定收敛到最松的那个门。
//
// 拦四类（只针对**门节点**上的判据）：
//  1. 门节点上的判据参数被改值（loudness_min_db / min_frames / require_audio…）
//  2. 门节点被删掉
//  3. 门节点上原本写过的判据键被移除（等于把判据删了，比改值更隐蔽）
//  4. 节点总数减少（图变短通常意味着砍了步骤；要砍请走 compose 重新设计）
//
// 放行：非门节点上的参数改动（含 post_production.loudness 这类"产出参数"）、
// 提示词改写、新增节点。放行的门层词汇改动进 warnings，不拦但要求留名。
func AuditVariant(base, cand []model.NodeInstance) (hacks []string, warnings []string) {
	baseGate := map[string]bool{}
	for i := range base {
		if mod, ok := ModuleContractOf(base[i].Type); ok && mod.Role == RoleGate {
			baseGate[base[i].ID] = true
		}
	}
	candByID := map[string]*model.NodeInstance{}
	for i := range cand {
		candByID[cand[i].ID] = &cand[i]
	}

	for id := range baseGate {
		if _, ok := candByID[id]; !ok {
			hacks = append(hacks, fmt.Sprintf("门节点 %s 被删除 —— 删门会让通过率无条件上升", id))
		}
	}

	for i := range base {
		b := &base[i]
		c, ok := candByID[b.ID]
		if !ok {
			continue
		}
		for key, oldV := range b.Data {
			newV, stillThere := c.Data[key]
			if baseGate[b.ID] {
				// 门节点上的一切参数都是判据
				if !stillThere {
					hacks = append(hacks, fmt.Sprintf("门节点 %s 的判据 %q 被移除（原值 %s）—— 这是拆门不是调参",
						b.ID, key, jsonText(oldV)))
					continue
				}
				if !jsonEqual(oldV, newV) {
					hacks = append(hacks, fmt.Sprintf("门节点 %s 的判据 %q 从 %s 改成 %s —— 改评分标准不算改进",
						b.ID, key, jsonText(oldV), jsonText(newV)))
				}
				continue
			}
			// 非门节点：只有门层词汇的改动需要留名，其余正常
			if IsGateParam(key) && stillThere && !jsonEqual(oldV, newV) {
				warnings = append(warnings, fmt.Sprintf("节点 %s 的 %q 属门层词汇，从 %s 改成 %s —— 允许，但须写明依据",
					b.ID, key, jsonText(oldV), jsonText(newV)))
			}
		}
	}

	if len(cand) < len(base) {
		hacks = append(hacks, fmt.Sprintf("节点数从 %d 减到 %d —— 砍步骤不是调参，请走 compose 重新设计",
			len(base), len(cand)))
	}
	sort.Strings(hacks)
	sort.Strings(warnings)
	return hacks, warnings
}

// FilterGateJudgementEdits 从候选里剔除「改门判据」的编辑。
//
// candidate.go 的规则表把 post_production.loudness 与 quality_gate.loudness_min_db
// 都列为目标 —— 那是**给人确认**的场景设计的（后期限幅与门下限对齐，一起改很合理）。
// 但自进化不能这么干：改判据等于改尺子，改完分数一定上涨。
//
// 所以自进化路径上只留产出侧（pp1.loudness），判据侧摘掉并如实报出去 ——
// 摘掉不是隐藏：dropped 会原样返回，让人知道"这里还有一条被拒绝了"。
func FilterGateJudgementEdits(nodes []model.NodeInstance, edits []CandidateEdit) (kept, dropped []CandidateEdit) {
	gateNode := map[string]bool{}
	for i := range nodes {
		if mod, ok := ModuleContractOf(nodes[i].Type); ok && mod.Role == RoleGate {
			gateNode[nodes[i].ID] = true
		}
	}
	for _, e := range edits {
		key := strings.TrimPrefix(e.Path, "data.")
		if i := strings.Index(key, "."); i >= 0 {
			key = key[:i]
		}
		if gateNode[e.NodeID] && IsGateParam(key) {
			dropped = append(dropped, e)
			continue
		}
		kept = append(kept, e)
	}
	return kept, dropped
}

// Attribution 一条失分归因：哪个门的哪条检查在漏，以及能不能靠调参救。
type Attribution struct {
	NodeID    string  `json:"nodeId,omitempty"`
	GateCheck string  `json:"gateCheck"`
	Layer     string  `json:"layer,omitempty"`
	Failed    int     `json:"failed"`
	Total     int     `json:"total"`
	FailRate  float64 `json:"failRate"`
	// LossShare 该检查占总失分的比例（0~1）。用来排序，不用来下结论。
	LossShare float64 `json:"lossShare"`
	// Verdict 该怎么修：
	//   param_tunable —— 图上有对应的可调旋钮，且该参数有行业标准值
	//   gate_side     —— 检查本身就是门判据，唯一正当动作是核阈值来源，不是改阈值
	//   needs_human   —— 有旋钮但没有标准值（提示词/模型选择/风格），只能人来定
	//   unmeasured    —— 失败的 run 大多没测到（服务没起/门崩了），归因无意义
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Knobs   []Knob `json:"knobs,omitempty"`
}

// AttributeEvidence 把一批运行证据归因到具体检查项。
//
// 三条纪律：
//
//	① unscored 不参与归因。它们不是"不合格"，是"没测到"——
//	   混进来会让"服务没起"看起来像"这一项老不过"（AGENTS §0.6）。
//	② 分母只用真正跑过该检查的 run 数，不是总 run 数。
//	③ 给不出可调旋钮的，宁可就写 needs_human，不许编一个"建议调 0.5"。
func AttributeEvidence(evs []Evidence, nodes []model.NodeInstance) []Attribution {
	type agg struct {
		failed, total int
		layer         string
		nodeID        string
		check         string
	}
	stats := map[string]*agg{}
	order := []string{}
	unscored, scored := 0, 0

	for _, ev := range evs {
		switch ev.Result {
		case EvidencePassed, EvidenceFailed:
			scored++
		default:
			unscored++
			continue
		}
		for _, c := range ev.GateChecks {
			// 按 (检查名, 门节点) 聚合：两个门都有「响度」时，
			// 合成一行会让人误以为是一处问题
			key := c.Name + "\x00" + c.NodeID
			a, ok := stats[key]
			if !ok {
				a = &agg{nodeID: c.NodeID, check: c.Name}
				stats[key] = a
				order = append(order, key)
			}
			a.total++
			if !c.OK {
				a.failed++
			}
			if a.layer == "" {
				a.layer = gateLayerOfCheck(c.Name)
			}
		}
	}

	totalLoss := 0
	for _, a := range stats {
		totalLoss += a.failed
	}
	// 一条能下结论的失分都没有：不许返回空结论，要说明为什么空
	if len(order) == 0 {
		if scored == 0 {
			return []Attribution{{
				GateCheck: "", Verdict: "unmeasured",
				Reason: fmt.Sprintf("%d 条运行里没有一条真正跑过门（unscored=%d），归因无意义 —— 先把门接上再谈进化",
					len(evs), unscored),
			}}
		}
		return nil
	}

	knobsByType := map[string][]Knob{}
	for _, k := range TrainableSurface(nodes) {
		knobsByType[k.NodeType] = append(knobsByType[k.NodeType], k)
	}

	out := make([]Attribution, 0, len(order))
	for _, key := range order {
		a := stats[key]
		name := a.check
		at := Attribution{
			NodeID:    a.nodeID,
			GateCheck: name,
			Layer:     a.layer,
			Failed:    a.failed,
			Total:     a.total,
			FailRate:  float64(a.failed) / float64(a.total),
		}
		if totalLoss > 0 {
			at.LossShare = float64(a.failed) / float64(totalLoss)
		}
		// 未测到的比例过半就标 unmeasured：此时"这条老不过"多半是采集问题
		if len(evs) > 0 && float64(unscored)/float64(len(evs)) > 0.5 && a.failed > 0 {
			at.Verdict = "unmeasured"
			at.Reason = fmt.Sprintf("%d/%d 条运行没测到（unscored 过半），这条失败率不可信",
				unscored, len(evs))
			out = append(out, at)
			continue
		}
		if a.failed == 0 {
			at.Verdict = "healthy"
			at.Reason = "全部通过"
			out = append(out, at)
			continue
		}

		// 有标准答案的参数规则能命中 → 可调
		if rule, ok := matchCandidateRule(name); ok {
			var knobs []Knob
			for _, t := range rule.Targets {
				for _, k := range knobsByType[t.NodeType] {
					if k.Key == t.Key {
						knobs = append(knobs, k)
					}
				}
			}
			if len(knobs) > 0 {
				at.Verdict = "param_tunable"
				at.Knobs = knobs
				at.Reason = rule.Basis
				out = append(out, at)
				continue
			}
			at.Verdict = "gate_side"
			at.Reason = "这条检查有标准值可依（" + rule.Basis +
				"），但图里没有对应的可调节点 —— 唯一正当动作是核对判据来源，不是改阈值"
			out = append(out, at)
			continue
		}

		// 命中不了规则的：把同层可调旋钮列出来供人判断，但不给目标值
		at.Verdict = "needs_human"
		at.Reason = "没有行业标准值可依，改哪个值属于创作判断；只能由人或外部 agent 提出假设，再配对验证"
		if a.layer != "" {
			if byLayer := knobsForLayer(a.layer, knobsByType); len(byLayer) > 0 {
				at.Knobs = byLayer
			}
		}
		out = append(out, at)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].LossShare != out[j].LossShare {
			return out[i].LossShare > out[j].LossShare
		}
		return out[i].GateCheck < out[j].GateCheck
	})
	return out
}

// matchCandidateRule 复用 candidate.go 的规则表（同一个真相源）
func matchCandidateRule(gateName string) (candidateRule, bool) {
	low := strings.ToLower(gateName)
	for _, r := range candidateRules {
		for _, kw := range r.MatchGate {
			if strings.Contains(low, strings.ToLower(kw)) {
				return r, true
			}
		}
	}
	return candidateRule{}, false
}

// gateLayerOfCheck 检查名反查门层。用 spec.go 的词汇表，不另立。
func gateLayerOfCheck(name string) string {
	low := strings.ToLower(name)
	words := map[string][]string{
		"audio":     {"响度", "静音", "音轨", "人声", "loudness", "silence", "audio", "speech"},
		"timing":    {"帧数", "时长", "fps", "节奏", "timing", "duration", "frames"},
		"identity":  {"一致", "锁", "参考图", "identity", "scene", "角色"},
		"technical": {"分辨率", "码率", "宽高", "technical", "resolution", "bitrate"},
		"budget":    {"预算", "耗时", "budget", "wall"},
	}
	for layer, kws := range words {
		for _, kw := range kws {
			if strings.Contains(low, strings.ToLower(kw)) {
				return layer
			}
		}
	}
	return ""
}

func knobsForLayer(layer string, byType map[string][]Knob) []Knob {
	// 层 → 最相关的节点类型，同样从 Modules 的 GateLayers 派生
	var out []Knob
	for t, ks := range byType {
		mod, ok := ModuleContractOf(t)
		if !ok {
			continue
		}
		carries := false
		for _, l := range mod.GateLayers {
			if l == layer {
				carries = true
				break
			}
		}
		if !carries {
			continue
		}
		for _, k := range ks {
			if !k.GateLocked {
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// EvoPolicy 判定门槛。默认值与序策 DEFAULT_EVOLUTION_POLICY 对齐，
// 免得两套自进化用两种"够了"标准。
type EvoPolicy struct {
	// MinPairs 至少几对同 caseId 的配对才允许下结论。
	// 8 是序策的 minimumEvaluationCases；低于这个数的"提升"大多是小样本噪声。
	MinPairs int
	// MinObjectiveDelta gateScore 至少涨这么多才算改善（绝对值，0~1）
	MinObjectiveDelta float64
	// MaxTokenRegressionPct token 涨幅上限（%）。默认 0：涨 token 换分数要单独论证。
	MaxTokenRegressionPct float64
	// MaxLatencyPct 耗时涨幅上限（%）
	MaxLatencyPct float64
	// MaxCriticalRegressions 允许几条"基线过、候选不过"的 fail 级回退
	MaxCriticalRegressions int
}

// DefaultEvoPolicy 保守默认值：宁可判 inconclusive，不判 promote。
func DefaultEvoPolicy() EvoPolicy {
	return EvoPolicy{
		MinPairs:               8,
		MinObjectiveDelta:      0.03,
		MaxTokenRegressionPct:  0,
		MaxLatencyPct:          10,
		MaxCriticalRegressions: 0,
	}
}

// FuelStatus 自进化的燃料：现在这批证据够不够支撑一次判断。
//
// 这一节存在的唯一理由：**数据不够时，"进化"会退化成随机改参数**。
// 序策那边已经用 minimumEvaluationCases / minimumShadowRuns 挡过一次，
// 这里把同一个判断提前到"提议之前"，而不是等评测跑完才发现样本不够。
type FuelStatus struct {
	Evidence     int     `json:"evidence"`
	Scored       int     `json:"scored"`
	UnscoredRate float64 `json:"unscoredRate"`
	// PairableCases 同一 caseId 在 baseline / candidate 两侧都有的 case 数
	PairableCases int      `json:"pairableCases"`
	PairableRuns  int      `json:"pairableRuns"`
	Workflows     int      `json:"workflows"`
	Categories    []string `json:"categories,omitempty"`
	Ready         bool     `json:"ready"`
	Blockers      []string `json:"blockers,omitempty"`
}

// InspectFuel 判断这批证据能不能支撑一次配对判断。
func InspectFuel(evs []Evidence, p EvoPolicy) FuelStatus {
	fs := FuelStatus{Evidence: len(evs)}
	if p.MinPairs <= 0 {
		p = DefaultEvoPolicy()
	}
	wf := map[uint]bool{}
	cats := map[string]bool{}
	base := map[string]bool{}
	cand := map[string]bool{}

	for _, ev := range evs {
		wf[ev.WorkflowID] = true
		switch ev.Result {
		case EvidencePassed, EvidenceFailed:
			fs.Scored++
		default:
			continue
		}
		if ev.CaseID != nil && *ev.CaseID != "" {
			switch ev.Role {
			case "baseline":
				base[*ev.CaseID] = true
			case "candidate":
				cand[*ev.CaseID] = true
			}
		}
		if ev.Category != nil && *ev.Category != "" {
			cats[*ev.Category] = true
		}
	}
	fs.Workflows = len(wf)
	for c := range cats {
		fs.Categories = append(fs.Categories, c)
	}
	sort.Strings(fs.Categories)
	if fs.Evidence > 0 {
		fs.UnscoredRate = float64(fs.Evidence-fs.Scored) / float64(fs.Evidence)
	}
	pairable := 0
	for c := range base {
		if cand[c] {
			pairable++
		}
	}
	fs.PairableCases = pairable
	fs.PairableRuns = 0
	if pairable > 0 {
		fs.PairableRuns = pairable * 2
	}

	if fs.Scored == 0 {
		fs.Blockers = append(fs.Blockers,
			"一条跑过门的运行都没有：先把 quality_gate 接进工作流，否则所有归因都是猜")
	}
	if fs.UnscoredRate > 0.2 && fs.Evidence > 0 {
		fs.Blockers = append(fs.Blockers, fmt.Sprintf(
			"unscored 占 %.0f%%（>20%%）：先修「没测到」的原因（服务没起/门崩了），否则通过率会被稀释",
			fs.UnscoredRate*100))
	}
	if fs.PairableCases < p.MinPairs {
		fs.Blockers = append(fs.Blockers, fmt.Sprintf(
			"可配对的 case 只有 %d 个（要 %d）：caseId 由评测层注入，缺它就分不清「改前改后」",
			fs.PairableCases, p.MinPairs))
	}
	if len(fs.Categories) < 2 {
		fs.Blockers = append(fs.Blockers, fmt.Sprintf(
			"只有 %d 个类别（要 ≥2）：单一类别上的提升可能只是过拟合那一个 case",
			len(fs.Categories)))
	}
	fs.Ready = len(fs.Blockers) == 0
	return fs
}

// PairOutcome 一个 case 的配对结果
type PairOutcome struct {
	CaseID string
	Base   *float64
	Cand   *float64
	// CriticalRegression candidate 出现了基线没有的 fail 级失败
	CriticalRegression bool
	BaseChecks         int
	CandChecks         int
}

// Decision 配对判定的结论。
//
// Verdict 只有三档，**没有"可能更好"**：自进化要能自动跑，
// 就不能留一个需要人解释的中间态。
type Decision struct {
	Verdict string   `json:"verdict"` // promote / keep_baseline / inconclusive
	Reasons []string `json:"reasons"`

	Pairs int `json:"pairs"`
	// MatchedCases 能配上的 case 数（两侧都跑过门）。Pairs 只统计两侧都有门分的，
	// 两个数分开报：MatchedCases 大而 Pairs 小，说明有 case 没测到 —— 那是采集问题不是效果问题。
	MatchedCases        int     `json:"matchedCases"`
	Better              int     `json:"better"`
	Worse               int     `json:"worse"`
	Tie                 int     `json:"tie"`
	GateScoreDelta      float64 `json:"gateScoreDelta"`
	TokenDeltaPct       float64 `json:"tokenDeltaPct"`
	LatencyDeltaPct     float64 `json:"latencyDeltaPct"`
	CriticalRegressions int     `json:"criticalRegressions"`

	// Hacks 审计出的"改评分标准"行为，非空即不可能 promote
	Hacks []string `json:"hacks,omitempty"`
	// Warnings 门层词汇改名（产出参数）这类需要留名、但不拦的改动
	Warnings []string `json:"warnings,omitempty"`
	// ScopeReduced 候选侧的检查项变少了（验收口径缩小）
	ScopeReduced bool          `json:"scopeReduced"`
	Outcomes     []PairOutcome `json:"outcomes,omitempty"`
}

// Decide 比较一批 baseline 与 candidate 证据，给出晋升判定。
//
// 配对口径：**同 caseId**。没有 caseId 的一律不配对 —— 按顺序配会得出
// "第 1 条改前的和第 1 条改后的是一回事"这种默认假设，而它们大概率不是。
func Decide(base, cand []Evidence, p EvoPolicy) Decision {
	if p.MinPairs <= 0 {
		p = DefaultEvoPolicy()
	}
	d := Decision{Verdict: "inconclusive"}

	baseBy := indexByCase(base)
	candBy := indexByCase(cand)
	cases := make([]string, 0, len(baseBy))
	for c := range baseBy {
		if _, ok := candBy[c]; ok {
			cases = append(cases, c)
		}
	}
	sort.Strings(cases)
	d.MatchedCases = len(cases)

	if len(cases) < p.MinPairs {
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"可配对 case 只有 %d 个（要求 %d）—— 样本不足时不产结论，避免把小样本噪声当成进化",
			len(cases), p.MinPairs))
		return d
	}

	var sumBase, sumCand float64
	var tokBase, tokCand, durBase, durCand int64
	for _, c := range cases {
		b, cnd := baseBy[c], candBy[c]
		o := PairOutcome{CaseID: c, Base: b.GateScore, Cand: cnd.GateScore,
			BaseChecks: len(b.GateChecks), CandChecks: len(cnd.GateChecks)}
		switch {
		case b.GateScore == nil || cnd.GateScore == nil:
			continue // 三态纪律：任一侧没测到，这个 case 作废
		case *cnd.GateScore > *b.GateScore+1e-9:
			d.Better++
		case *cnd.GateScore < *b.GateScore-1e-9:
			d.Worse++
		default:
			d.Tie++
		}
		if o.CandChecks < o.BaseChecks {
			d.ScopeReduced = true
		}
		if hasNewCriticalFailure(b.GateChecks, cnd.GateChecks) {
			o.CriticalRegression = true
			d.CriticalRegressions++
		}
		if b.GateScore != nil && cnd.GateScore != nil {
			sumBase += *b.GateScore
			sumCand += *cnd.GateScore
		}
		tokBase += int64(b.EffectiveTokens)
		tokCand += int64(cnd.EffectiveTokens)
		durBase += b.DurationMS
		durCand += cnd.DurationMS
		d.Outcomes = append(d.Outcomes, o)
	}
	d.Pairs = d.Better + d.Worse + d.Tie
	if d.Pairs == 0 {
		d.Reasons = append(d.Reasons, "配上了 case，但两侧门分都是 null（没测到），无法比较")
		return d
	}
	d.GateScoreDelta = sumCand/float64(d.Pairs) - sumBase/float64(d.Pairs)
	if tokBase > 0 {
		d.TokenDeltaPct = (float64(tokCand) - float64(tokBase)) / float64(tokBase) * 100
	}
	if durBase > 0 {
		d.LatencyDeltaPct = (float64(durCand) - float64(durBase)) / float64(durBase) * 100
	}

	// __ 硬门（任何一条不过就 keep_baseline，不是 inconclusive）__
	// 这些是"测了，明确不该晋升"，不是"没测够"。
	if d.ScopeReduced {
		d.Verdict = "keep_baseline"
		d.Reasons = append(d.Reasons,
			"候选侧的检查项比基线少 —— 验收口径被缩小，分数上涨不代表质量上涨")
		return d
	}
	if d.CriticalRegressions > p.MaxCriticalRegressions {
		d.Verdict = "keep_baseline"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"出现 %d 条 fail 级回退（基线过、候选不过）—— 这是硬伤，一票否决",
			d.CriticalRegressions))
		return d
	}
	if p.MaxTokenRegressionPct >= 0 && d.TokenDeltaPct > p.MaxTokenRegressionPct {
		d.Verdict = "keep_baseline"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"token 涨了 %.1f%%（上限 %.1f%%）—— 用成本换分数要单独论证",
			d.TokenDeltaPct, p.MaxTokenRegressionPct))
		return d
	}
	if p.MaxLatencyPct >= 0 && d.LatencyDeltaPct > p.MaxLatencyPct {
		d.Verdict = "keep_baseline"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"耗时涨了 %.1f%%（上限 %.1f%%）—— 一分钟的出片变两分钟不算进化",
			d.LatencyDeltaPct, p.MaxLatencyPct))
		return d
	}

	// __ 目标改善（要至少满足一条，与序策 objectiveImproved 同口径）__
	improved := d.GateScoreDelta >= p.MinObjectiveDelta ||
		(d.TokenDeltaPct <= -3) || (d.LatencyDeltaPct <= -3)
	if !improved {
		d.Verdict = "keep_baseline"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"门分只变了 %+.3f（要求 ≥%.3f），token 与耗时也没降 3%% 以上 —— 没有可确认的收益",
			d.GateScoreDelta, p.MinObjectiveDelta))
		return d
	}
	// 配对胜率也要过半：均分上涨可能只靠一个 case 大涨，其余变差
	if d.Better <= d.Worse {
		d.Verdict = "keep_baseline"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"配对胜率 %d 胜 / %d 负 —— 均分涨了但赢少输多，靠单个 case 拉平均不算改善",
			d.Better, d.Worse))
		return d
	}

	d.Verdict = "promote"
	d.Reasons = append(d.Reasons, fmt.Sprintf(
		"门分 %+.3f（%d 胜 / %d 负 / %d 平），token %+.1f%%，耗时 %+.1f%%，无 fail 级回退",
		d.GateScoreDelta, d.Better, d.Worse, d.Tie, d.TokenDeltaPct, d.LatencyDeltaPct))
	return d
}

// indexByCase 按 caseId 建索引；缺 caseId 的丢弃（并在上层 FuelStatus 里报出来）
func indexByCase(evs []Evidence) map[string]Evidence {
	out := map[string]Evidence{}
	for _, ev := range evs {
		if ev.CaseID == nil || *ev.CaseID == "" {
			continue
		}
		switch ev.Result {
		case EvidencePassed, EvidenceFailed:
			out[*ev.CaseID] = ev
		}
	}
	return out
}

// LoadEvidenceFiles 读 <dir>/run-*.json 成完整信封。
//
// 与 evidence_agg 的 LoadEvidenceRecords 的区别：那边只要聚合用的几个字段，
// 这里要 CaseID / Role / WorkflowVersion —— 配对判定缺一个都判不了。
// 坏文件跳过并计数，不因为一个坏文件让整批不可用（但计数要报出去，
// 否则"少读了 40 个文件"会静默地让样本量看起来刚好）。
func LoadEvidenceFiles(dir string) (evs []Evidence, bad int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil // 还没跑过任何带门的 run，是正常状态不是错误
		}
		return nil, 0, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "run-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			bad++
			continue
		}
		var ev Evidence
		if jerr := json.Unmarshal(raw, &ev); jerr != nil {
			bad++
			continue
		}
		evs = append(evs, ev)
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].RunID < evs[j].RunID })
	return evs, bad, nil
}

// FilterEvidence 只留某条工作流的证据（workflowID=0 表示全留）
func FilterEvidence(evs []Evidence, workflowID uint) []Evidence {
	if workflowID == 0 {
		return evs
	}
	out := make([]Evidence, 0, len(evs))
	for _, ev := range evs {
		if ev.WorkflowID == workflowID {
			out = append(out, ev)
		}
	}
	return out
}

// hasNewCriticalFailure 候选侧出现了基线没有的 fail 级失败检查
func hasNewCriticalFailure(baseChecks, candChecks []GateCheckEvidence) bool {
	passed := map[string]bool{}
	for _, c := range baseChecks {
		if c.OK {
			passed[c.Name] = true
		}
	}
	for _, c := range candChecks {
		if !c.OK && c.Level == "fail" && passed[c.Name] {
			return true
		}
	}
	return false
}
