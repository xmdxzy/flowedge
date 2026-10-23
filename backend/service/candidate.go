package service

import (
	"fmt"
	"sort"
	"strings"

	"flowedge/backend/model"
)

// 候选生成：把「诊断」翻成「具体改哪一行参数」。
//
// 这是 D4 闭环里最难的一环，也是最容易做成"随机改参数"的一环。
//
// 定下的三条规矩（不遵守就会变成噪音生成器）：
//
//	① 只提议**有标准答案**的参数。loudness=-14 LUFS 是平台惯例，fps=25 是整数标准，
//	   max_silence_gap=0.5 是口播常规 —— 这些改了可以说"按标准对齐"。
//	   而 prompt / 模型选择 / 分镜权重**没有标准答案**，一概不碰，只写"需要人判断"。
//
//	② from 必须从图里**真读出来**，不许猜当前值。猜错的话 spec/apply 的乐观锁会拒，
//	   但更糟的是"提议看起来成立、应用时说值不对"——那会让人不再相信这些提议。
//
//	③ 每条都带"预期改善哪个指标"和"风险"。没有预期效果的改动不是候选，是折腾。
//
// 为什么不做成 LLM 生成：LLM 会给出听起来合理但无法验证的改动（"把提示词写得更丰富"），
// 而候选的价值恰恰在于**能被试出来**。规则候选可以一试一个准，LLM 候选只能靠人判断。

// CandidateEdit 一条具体改动（与 spec/apply 的 SpecEdit 同构，可直接下发）
type CandidateEdit struct {
	NodeID string      `json:"nodeId"`
	Path   string      `json:"path"`
	From   interface{} `json:"from"`
	To     interface{} `json:"to"`
	Why    string      `json:"why"`
}

// TrialCandidate 一条可试验的候选
type TrialCandidate struct {
	ID            string          `json:"id"`
	WorkflowID    uint            `json:"workflowId"`
	Title         string          `json:"title"`
	DiagnosisKind string          `json:"diagnosisKind"`
	Edits         []CandidateEdit `json:"edits"`
	// Expected 预期改善哪个指标（必须是能测的）
	Expected string `json:"expected"`
	// Risk 风险（人要不要先看一眼）
	Risk string `json:"risk"`
	// Confidence high=有行业标准可依 / medium=经验值 / low=需人判断（一般不给)
	Confidence string `json:"confidence"`
	// Basis 依据（写清"凭什么是这个值"，方便人驳回）
	Basis string `json:"basis"`
}

// candidateTarget 可改的 (节点类型, 参数名)
type candidateTarget struct {
	NodeType string
	Key      string
}

// candidateRule 一条「参数 → 标准值」规则。
//
// 命名类型而不是匿名 struct：自进化内核（evolution.go）要复用同一张表做归因，
// 匿名 struct 无法跨函数签名传递，只能复制一份 —— 那就是第二真相源。
type candidateRule struct {
	// MatchGate 门检查名里包含这些词之一就匹配
	MatchGate []string
	// Targets 可改的 (节点类型, 参数名)
	Targets []candidateTarget
	// Value 目标值
	Value interface{}
	// Basis 依据
	Basis string
	// Why 为什么这么改
	Why string
	// Expected 预期改善哪个指标
	Expected string
	Risk     string
}

// candidateRules 参数 → 标准值。**只收有行业标准/物理常识的项。**
var candidateRules = []candidateRule{
	{
		MatchGate: []string{"响度", "loudness", "LUFS"},
		Targets:   []candidateTarget{{"post_production", "loudness"}, {"quality_gate", "loudness_min_db"}},
		Value:     -14,
		Basis:     "短视频平台（抖音/视频号）普遍以 -14 LUFS 为基准，偏离会被平台二次压缩",
		Why:       "把响度归一到平台基准，避免发布后被重新压限导致人声忽大忽小",
		Expected:  "audio 门通过率（loudness 项）",
		Risk:      "改后需重听一次，确认 BGM 没有被压过头",
	},
	{
		MatchGate: []string{"静音", "silence"},
		Targets:   []candidateTarget{{"post_production", "max_silence_gap"}, {"quality_gate", "max_silence_gap"}},
		Value:     0.5,
		Basis:     "口播类内容超过 0.5s 的静音会被观众感知为卡顿",
		Why:       "收紧静音上限，逼出 TTS 前导静音与段间空隙的问题",
		Expected:  "audio 门通过率（静音缺口项）",
		Risk:      "若片子本来就是氛围片（长镜头无对白），这条不适用",
	},
	{
		MatchGate: []string{"音轨", "audio", "audio_track"},
		Targets:   []candidateTarget{{"quality_gate", "require_audio"}, {"post_production", "audio_muxed"}},
		Value:     true,
		Basis:     "有音频产物的链路必须校验音轨存在（曾出现 -map 漏写产出纯视频而时长正常）",
		Why:       "打开音轨强制校验，把「没有声音」变成 FAIL 而不是靠人发现",
		Expected:  "audio 门通过率（音轨存在项）",
		Risk:      "纯默片场景不适用",
	},
	{
		MatchGate: []string{"人声", "speech", "台词"},
		Targets:   []candidateTarget{{"quality_gate", "forbid_speech"}},
		Value:     true,
		Basis:     "氛围片（无台词）出现人声属硬伤，H3 生成时常带出无意义语音",
		Why:       "开启氛围模式，让「疑似有人声」直接判 FAIL",
		Expected:  "audio 门通过率（人声项）",
		Risk:      "有台词的片子开了会全部 FAIL —— 先确认这片到底要不要台词",
	},
}

// ProposeCandidates 把一条诊断翻成候选。
//
// 返回空切片是**正常结果**：环境类诊断（unstable / long_tail）不该产生"改图"的候选，
// 那种情况下改参数是掩盖问题。
func ProposeCandidates(diag Diagnosis, workflowID uint, wfName string, nodes []model.NodeInstance) []TrialCandidate {
	if len(nodes) == 0 {
		return nil
	}
	var out []TrialCandidate

	switch diag.Kind {
	case "gate_weak":
		// 诊断的 Target 形如 "工作流名 / 门检查名"，取门检查名做匹配
		gateName := diag.Target
		if i := strings.LastIndex(gateName, " / "); i >= 0 {
			gateName = gateName[i+3:]
		}
		gateName = strings.ToLower(gateName)

		for _, rule := range candidateRules {
			hit := false
			for _, kw := range rule.MatchGate {
				if strings.Contains(gateName, strings.ToLower(kw)) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			edits := collectEdits(nodes, rule.Targets, rule.Value, rule.Why)
			if len(edits) == 0 {
				continue // 图里已经没有可改的点，或已经是目标值
			}
			out = append(out, TrialCandidate{
				ID:            fmt.Sprintf("cand-%s-%d", diag.Kind, len(out)+1),
				WorkflowID:    workflowID,
				Title:         "按标准对齐「" + gateName + "」相关参数",
				DiagnosisKind: diag.Kind,
				Edits:         edits,
				Expected:      rule.Expected,
				Risk:          rule.Risk,
				Confidence:    "high",
				Basis:         rule.Basis,
			})
		}

	case "pipeline_weak":
		// 结构性问题不猜参数，直接指路：先看结构分
		out = append(out, TrialCandidate{
			ID:            fmt.Sprintf("cand-%s-%d", diag.Kind, 1),
			WorkflowID:    workflowID,
			Title:         "先做结构检查（不改参数）",
			DiagnosisKind: diag.Kind,
			Edits:         nil,
			Expected:      "定位无效连线 / 必填无源（这些会让运行期直接挂）",
			Risk:          "无（只读）",
			Confidence:    "high",
			Basis:         "失败率过半通常不是参数问题，是图结构问题；先跑 POST /api/graph/score 看 legal 与分项",
		})
	}
	return out
}

// collectEdits 从图里读出当前值，与目标值不同才生成一条编辑
func collectEdits(nodes []model.NodeInstance, targets []candidateTarget, want interface{}, why string) []CandidateEdit {

	var out []CandidateEdit
	for _, t := range targets {
		for i := range nodes {
			n := &nodes[i]
			if n.Type != t.NodeType {
				continue
			}
			cur, ok := n.Data[t.Key]
			if !ok || cur == nil {
				// 没有这个键：**不主动新增**。加一个没写过的参数等于替用户做决定，
				// 而且 from 无从核对（spec/apply 要求 from 与现场一致）。
				continue
			}
			if jsonEqual(cur, want) {
				continue // 已经是目标值
			}
			out = append(out, CandidateEdit{
				NodeID: n.ID,
				Path:   "data." + t.Key,
				From:   cur,
				To:     want,
				Why:    why,
			})
		}
	}
	// 稳定的顺序，便于对照 diff
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// ToSpecEdits 转成 spec/apply 的入参（复用它的四条硬拦，不另开一条写路径）
func (c TrialCandidate) ToSpecEdits() []SpecEdit {
	out := make([]SpecEdit, 0, len(c.Edits))
	for _, e := range c.Edits {
		out = append(out, SpecEdit{
			NodeID: e.NodeID,
			Path:   e.Path,
			From:   e.From,
			To:     e.To,
			Reason: e.Why,
		})
	}
	return out
}
