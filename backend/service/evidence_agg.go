package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 证据聚合与诊断。
//
// 为什么需要
//
//	`evidence.json` 是给**外部评测器**读的单条事实，一封一条 run。
//	但"我这条流水线哪里在漏"这个问题，单条信封回答不了 —— 它需要把 N 条
//	放在一起看分布。原来这一步没有任何工具，所以证据写完就躺在目录里，
//	这等于：**观测有了，但没有观测者**。
//
// 与 harness² / Raven 的对应
//
//	harness² 的改进器要先拿到一批任务的结果才能提候选；Raven 的 Evolver 也是
//	先跑一批再筛。两家的第一步都是「批量汇总」，只是都藏在自己的闭源/内部工具里。
//	这里把它做成 FlowEdge 的一等能力：聚合 → 诊断 → 可登记。
//
// 为什么诊断要做成规则而不是让 LLM 自由发挥
//
//	规则可复算、可解释、可测。LLM 诊断可以作为后续增强，但**必须先有确定性基线**：
//	否则"为什么判这批 run 不好"永远说不清（AGENTS §0.13）。

// RunRecord 一次运行的统一形态。
//
// 两个来源要归一：新数据来自 evidence 文件（有门明细），历史数据来自 runs 表
// （只有状态）。**不能因为老数据没有门明细就把它丢掉** —— 125 条历史分布仍然
// 能回答"哪条流水线最容易挂"，丢掉等于从零开始攒。
type RunRecord struct {
	RunID      uint
	WorkflowID uint
	Workflow   string
	// Result 统一口径：passed / failed / unscored / cancelled / paused / running
	Result             string
	UnscoredReason     string
	DurationMs         int64
	GateScore          *float64
	GateChecks         []GateCheckEvidence
	HumanInterventions int
	// Source evidence（有门明细） / history（只有状态）
	Source string
	// Fingerprint evidence 文件内容的 sha256，用于登记时追溯；历史数据为空
	Fingerprint string
}

// GateFailStat 某个门检查项在这批 run 里的失败情况
type GateFailStat struct {
	Name     string  `json:"name"`
	Level    string  `json:"level"`
	Total    int     `json:"total"`
	Failed   int     `json:"failed"`
	FailRate float64 `json:"failRate"`
}

// WorkflowStats 一条工作流的汇总
type WorkflowStats struct {
	WorkflowID uint   `json:"workflowId"`
	Name       string `json:"name"`
	Runs       int    `json:"runs"`
	Passed     int    `json:"passed"`
	// PassedWithGate 只有这些 passed 真的过过门（来自 evidence）。
	// 历史 runs 表的 success 只表示"流程跑完了"，没跑过任何门，不能算进门通过率。
	PassedWithGate int `json:"passedWithGate"`
	Failed         int `json:"failed"`     // 门跑完了判不过
	Aborted        int `json:"aborted"`    // 执行中断/失败，没走到门（历史数据只有这一档）
	Unscored       int `json:"unscored"`   // 没测到（no_gate / gate_error）
	Incomplete     int `json:"incomplete"` // cancelled / paused / running

	// 三个数缺一不可（见 docs §5.9.2）。
	// CRClean 用指针：没有一条"门判不过"的记录时，分母为 0，此时**不能报 1.0**
	// —— 那会把"没测到"伪装成"全过"。这是真机验证抓到的（125 条历史数据 CR_clean=1.000）。
	CRAll        float64  `json:"crAll"`
	CRClean      *float64 `json:"crClean"`
	AbortedRate  float64  `json:"abortedRate"`
	UnscoredRate float64  `json:"unscoredRate"`

	AvgMs int64 `json:"avgMs"`
	P95Ms int64 `json:"p95Ms"`

	TopFailingGates []GateFailStat `json:"topFailingGates,omitempty"`
	HasEvidence     bool           `json:"hasEvidence"` // false = 只有历史状态，看不出门细节
}

// Diagnosis 一条可执行的诊断结论
type Diagnosis struct {
	Severity string `json:"severity"` // P0 / P1 / P2
	// WorkflowID 这条诊断属于哪条工作流（0 = 全局）。
	// 前端要靠它一键"生成候选"，否则只能按名字猜。
	WorkflowID uint   `json:"workflowId,omitempty"`
	Kind       string `json:"kind"`   // env_unstable / gate_weak / pipeline_weak / long_tail / evidence_missing
	Target     string `json:"target"` // 工作流名或门名
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
	Evidence   []uint `json:"evidence,omitempty"` // 支撑这条结论的 runID（可点开）
	Metric     string `json:"metric,omitempty"`   // 触发值，形如 "audio.failRate=0.38 (12/32)"
}

// AggregateReport 聚合报告
type AggregateReport struct {
	Total        int             `json:"total"`
	FromEvidence int             `json:"fromEvidence"`
	FromHistory  int             `json:"fromHistory"`
	Overall      WorkflowStats   `json:"overall"`
	Workflows    []WorkflowStats `json:"workflows"`
	Diagnoses    []Diagnosis     `json:"diagnoses"`
	// Trustworthy 这批分数能不能直接下结论（unscored_rate > 20% 时不能）
	Trustworthy bool   `json:"trustworthy"`
	Note        string `json:"note,omitempty"`
}

// AggregateRuns 把一批 run 记录汇总成报告。纯函数，不读盘不碰库。
func AggregateRuns(recs []RunRecord) AggregateReport {
	rep := AggregateReport{Total: len(recs)}

	byWF := map[uint][]RunRecord{}
	for _, r := range recs {
		byWF[r.WorkflowID] = append(byWF[r.WorkflowID], r)
		if r.Source == "evidence" {
			rep.FromEvidence++
		} else {
			rep.FromHistory++
		}
	}

	rep.Overall = statOf(0, "全部", recs)
	for id, list := range byWF {
		name := ""
		for _, r := range list {
			if r.Workflow != "" {
				name = r.Workflow
				break
			}
		}
		rep.Workflows = append(rep.Workflows, statOf(id, name, list))
	}
	sort.Slice(rep.Workflows, func(i, j int) bool {
		if rep.Workflows[i].Failed != rep.Workflows[j].Failed {
			return rep.Workflows[i].Failed > rep.Workflows[j].Failed
		}
		return rep.Workflows[i].Runs > rep.Workflows[j].Runs
	})

	rep.Diagnoses = diagnose(rep)
	rep.Trustworthy = rep.Overall.UnscoredRate <= 0.2
	if !rep.Trustworthy {
		rep.Note = fmt.Sprintf("不可测占比 %.0f%%（>20%%）：先修环境再看分数，否则拿到的是环境的分数",
			rep.Overall.UnscoredRate*100)
	}
	if rep.FromEvidence == 0 {
		rep.Note = appendNote(rep.Note, "全部来自历史 runs 表，没有门明细 —— 只能看成败，看不出哪个门在漏")
	}
	return rep
}

func appendNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}

// statOf 单个分组统计
func statOf(id uint, name string, list []RunRecord) WorkflowStats {
	st := WorkflowStats{WorkflowID: id, Name: name, Runs: len(list)}

	var durs []int64
	gateTotal := map[string]*GateFailStat{}

	for _, r := range list {
		switch r.Result {
		case EvidencePassed:
			st.Passed++
			if r.Source == "evidence" {
				st.PassedWithGate++
			}
		case EvidenceFailed:
			st.Failed++
		case "aborted":
			st.Aborted++
		case EvidenceUnscored, "running", "unknown", "":
			st.Unscored++
		default: // cancelled / paused
			st.Incomplete++
		}
		if r.DurationMs > 0 {
			durs = append(durs, r.DurationMs)
		}
		if r.Source == "evidence" {
			st.HasEvidence = true
		}
		for _, c := range r.GateChecks {
			g, ok := gateTotal[c.Name]
			if !ok {
				g = &GateFailStat{Name: c.Name, Level: c.Level}
				gateTotal[c.Name] = g
			}
			g.Total++
			if !c.OK {
				g.Failed++
			}
		}
	}

	// 三数：全部分母 / 可测分母 / 不可测占比
	if st.Runs > 0 {
		st.CRAll = float64(st.Passed) / float64(st.Runs)
		st.UnscoredRate = float64(st.Unscored) / float64(st.Runs)
		st.AbortedRate = float64(st.Aborted) / float64(st.Runs)
	}
	// 门通过率的分母只认"真的过过门"的样本：evidence 里的 passed + failed。
	// 历史 runs 表的 success 不算 —— 它只证明流程跑完，不证明产物合格。
	// 一条门样本都没有时返回 null，而不是 1.0（那会把"没测过"伪装成"全过"）。
	if measurable := st.PassedWithGate + st.Failed; measurable > 0 {
		v := float64(st.PassedWithGate) / float64(measurable)
		st.CRClean = &v
	}

	if len(durs) > 0 {
		var sum int64
		for _, d := range durs {
			sum += d
		}
		st.AvgMs = sum / int64(len(durs))
		sorted := append([]int64(nil), durs...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		idx := int(float64(len(sorted)-1) * 0.95)
		st.P95Ms = sorted[idx]
	}

	for _, g := range gateTotal {
		g.FailRate = safeDiv(float64(g.Failed), float64(g.Total))
		if g.Failed > 0 {
			st.TopFailingGates = append(st.TopFailingGates, *g)
		}
	}
	sort.Slice(st.TopFailingGates, func(i, j int) bool {
		if st.TopFailingGates[i].Failed != st.TopFailingGates[j].Failed {
			return st.TopFailingGates[i].Failed > st.TopFailingGates[j].Failed
		}
		return st.TopFailingGates[i].Name < st.TopFailingGates[j].Name
	})
	if len(st.TopFailingGates) > 3 {
		st.TopFailingGates = st.TopFailingGates[:3]
	}
	return st
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// diagnose 规则诊断。每条都必须能指到具体 runID 或具体数值。
//
// 排序即优先级：P0 在前，同级别按影响面。
func diagnose(rep AggregateReport) []Diagnosis {
	var out []Diagnosis

	// 规则 1：不可测占比过高 → 分数不可信（这是最容易被忽略、后果最严重的一条）
	if rep.Overall.Runs >= 5 && rep.Overall.UnscoredRate > 0.2 {
		out = append(out, Diagnosis{
			Severity: "P0", Kind: "env_unstable",
			Target: "全部",
			Message: fmt.Sprintf("不可测占比 %.0f%%（%d/%d）：这批分数不能用来比较方案优劣",
				rep.Overall.UnscoredRate*100, rep.Overall.Unscored, rep.Overall.Runs),
			Suggestion: "先查依赖服务是否稳定（Ollama/ComfyUI/门所需的 ffprobe），再重跑；`unscoredReason` 里有归类",
			Metric:     fmt.Sprintf("unscoredRate=%.2f", rep.Overall.UnscoredRate),
		})
	}

	// 规则 2：某个门检查项稳定在漏 → 点名它，并给出该门所在的 run
	for _, wf := range rep.Workflows {
		for _, g := range wf.TopFailingGates {
			if g.Total < 3 || g.FailRate < 0.3 {
				continue // 样本太少或偶发，不构成"稳定在漏"
			}
			out = append(out, Diagnosis{
				Severity: "P1", Kind: "gate_weak", WorkflowID: wf.WorkflowID,
				Target: wf.Name + " / " + g.Name,
				Message: fmt.Sprintf("门检查「%s」在 %d 次里失败 %d 次（%.0f%%）",
					g.Name, g.Total, g.Failed, g.FailRate*100),
				Suggestion: fmt.Sprintf("先跑一次该门对应的单项验证（如 `python scripts/verify-media.py` 看 %s），再决定是改参数还是改流程", g.Name),
				Metric:     fmt.Sprintf("%s.failRate=%.2f (%d/%d)", g.Name, g.FailRate, g.Failed, g.Total),
			})
		}
	}

	// 规则 3：某条流水线失败率过半 → 不是偶发，是结构问题
	for _, wf := range rep.Workflows {
		if wf.Runs < 3 {
			continue
		}
		hardFail := float64(wf.Failed) / float64(wf.Runs)
		if hardFail > 0.5 {
			out = append(out, Diagnosis{
				Severity: "P1", Kind: "pipeline_weak", WorkflowID: wf.WorkflowID,
				Target:     wf.Name,
				Message:    fmt.Sprintf("这条流水线 %d 次里失败 %d 次（%.0f%%）—— 不是偶发", wf.Runs, wf.Failed, hardFail*100),
				Suggestion: "取一次失败 run 的任务书，对照结构分（POST /api/graph/score）看是否有无效连线或必填无源",
				Metric:     fmt.Sprintf("failed=%d/%d", wf.Failed, wf.Runs),
			})
		}
	}

	// 规则 4：长尾 —— p95 远超中位（重试或卡在某个节点上）
	for _, wf := range rep.Workflows {
		if wf.Runs < 5 || wf.AvgMs <= 0 || wf.P95Ms <= 0 {
			continue
		}
		if float64(wf.P95Ms) > 2.2*float64(wf.AvgMs) {
			out = append(out, Diagnosis{
				Severity: "P2", Kind: "long_tail", WorkflowID: wf.WorkflowID,
				Target: wf.Name,
				Message: fmt.Sprintf("耗时 p95 是中位的 %.1f 倍（%dms vs %dms）：存在长尾，多半在重试或等冷装载",
					float64(wf.P95Ms)/float64(wf.AvgMs), wf.P95Ms, wf.AvgMs),
				Suggestion: "看这几次 run 的节点耗时分布；若是首张冷加载（flux 183s），考虑保持模型驻留而不是卸载",
				Metric:     fmt.Sprintf("p95/avg=%.1f", float64(wf.P95Ms)/float64(wf.AvgMs)),
			})
		}
	}

	// 规则 5：全部来自历史表 → 无法诊断细节（把限制说出来，而不是装作诊断过了）
	if rep.FromEvidence == 0 && rep.Total > 0 {
		out = append(out, Diagnosis{
			Severity: "P2", Kind: "evidence_missing",
			Target:     "全部",
			Message:    fmt.Sprintf("%d 条记录全部来自历史 runs 表，没有一条带门明细", rep.Total),
			Suggestion: "门明细从本次版本起才写入（data/evidence/）。跑一批新任务后本报告才能给出门级诊断",
		})
	}

	// 稳定排序：P0 → P1 → P2，同级按 kind
	rank := map[string]int{"P0": 0, "P1": 1, "P2": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// intOr 指针解引用，nil 时给默认值
func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// ===== 读证据文件 =====

// LoadEvidenceRecords 读 <dir>/run-*.json。
//
// 单条坏了不影响整批：跳过并计数。评测读半截 JSON 会判不可达，
// 但聚合器不该因为一条坏文件就整体失败 —— 那是"因为一个坏样本丢掉 124 个好样本"。
func LoadEvidenceRecords(dir string) ([]RunRecord, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}

	var out []RunRecord
	bad := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "run-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			bad++
			continue
		}
		var ev Evidence
		if err := json.Unmarshal(raw, &ev); err != nil {
			bad++
			continue
		}
		sum := sha256.Sum256(raw)
		out = append(out, RunRecord{
			RunID:              ev.RunID,
			WorkflowID:         ev.WorkflowID,
			Result:             ev.Result,
			UnscoredReason:     ev.UnscoredReason,
			DurationMs:         ev.DurationMS,
			GateScore:          ev.GateScore,
			GateChecks:         ev.GateChecks,
			HumanInterventions: intOr(ev.HumanInterventions, 0),
			Source:             "evidence",
			Fingerprint:        hex.EncodeToString(sum[:]),
		})
	}
	return out, bad, nil
}
