package service

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// 证据信封：一次运行结束时，把引擎**确知的事实**落成一份可被外部评测器直接消费的 JSON。
//
// 为什么要有这个文件
//
//	评测「同一模型、有引擎 vs 无引擎」时，最大的作弊空间不是模型，是口径：
//	谁记了耗时、谁记了 token、失败到底算 0 分还是没测到。这些必须由引擎自己写死，
//	不能让评测脚本各算各的。外部只读这一份，就不会出现两套账。
//
// 边界（很重要，别越界）
//
//	引擎是**证据采集器**，不是评测器。caseId / datasetFingerprint / category
//	属于评测语义，引擎不知道也不该猜 —— 拿不到就写 null。用默认值冒充事实
//	会让一份"看起来齐全"的信封变成假证据（Spec 铁律：只报事实）。
//
// 三态而不是二态
//
//	passed / failed / unscored。unscored 表示「没测到」（主动中止、跑挂了、
//	根本没接门），**绝不能用 false 代替** —— 否则「服务没起导致没测」会被算成
//	「产出不合格」，评测结果直接反向。同理 humanInterventions 拿不到时是 null
//	不是 0：0 是"测到了，人没插手"，null 是"没采集"。

// EvidenceSchemaVersion 信封 schema 版本。字段增删要 +1。
//
// v2（2026-10-23）：GateCheckEvidence 增加 nodeId（归因到具体门节点）。
// 兼容性：只增字段且 omitempty，v1 信封原样可读，读到的 nodeId 为空串。
const EvidenceSchemaVersion = 2

// 证据判定结果（Result 字段取值）
const (
	EvidencePassed   = "passed"
	EvidenceFailed   = "failed"
	EvidenceUnscored = "unscored"
	// cancelled / paused 单独列出而不并进 unscored：
	// 它们在评测里都是"不计分"，但在排查时是完全不同的事实。
	EvidenceCancelled = "cancelled"
	EvidencePaused    = "paused"
)

// GateCheckEvidence 单条门检查（只留评测用得上的三个字段）
type GateCheckEvidence struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Level string `json:"level"` // fail（阻断）/ warn（备忘）
	// NodeID 这条检查是哪个门节点产出的。
	//
	// 为什么必须记：一张图可以接多个门，没有 NodeID 时"响度过不了"无法定位到
	// 是哪个 post_production 的问题，自进化只能给出"全局该调响度"这种无用建议。
	// 加字段而不是新开一份结构，是为了让老信封照样能读（omitempty）。
	NodeID string `json:"nodeId,omitempty"`
}

// Evidence 一次运行的评测证据信封
type Evidence struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         uint   `json:"runId"`
	WorkflowID    uint   `json:"workflowId"`
	InvokedBy     string `json:"invokedBy,omitempty"`

	// 以下三项由评测层注入；引擎不知道，故为指针且默认 null。
	CaseID             *string `json:"caseId"`
	DatasetFingerprint *string `json:"datasetFingerprint"`
	Category           *string `json:"category"`
	Role               string  `json:"role,omitempty"` // baseline / candidate

	// WorkflowVersion 本次运行用的图版本（快照号）。
	// 没有它就无法回答"改动前后谁更好" —— 只能按时间窗口猜。0 = 旧信封没记。
	WorkflowVersion int `json:"workflowVersion,omitempty"`

	Result         string `json:"result"`
	Passed         *bool  `json:"passed"` // nil = 没测到；false = 测了不合格
	UnscoredReason string `json:"unscoredReason,omitempty"`

	StartedAt  *time.Time `json:"startedAt,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
	DurationMS int64      `json:"durationMs"`

	EffectiveTokens int `json:"effectiveTokens"`

	// GateScore = 通过的检查项 / 总检查项（0~1）。
	// 对应 harness² 的 Partial 分（criteria 通过比例）：比"过/不过"信息量大得多，
	// 且不需要 LLM judge —— 分子分母都是确定性的。
	GateScore  *float64            `json:"gateScore"`
	GateChecks []GateCheckEvidence `json:"gateChecks,omitempty"`
	GateNodes  []string            `json:"gateNodes,omitempty"`

	SideUnloadCount int  `json:"sideUnloadCount"`
	PeakVramMB      *int `json:"peakVramMb"` // 恒为 null，原因见 BuildEvidence 注释

	HumanInterventions *int `json:"humanInterventions"`
}

// BuildEvidence 汇总一次运行的客观事实。
//
// states 是引擎收尾时手上的节点终态（内存对象，含 output._usage）；不查 DB，
// 因为事件日志是批量异步落库的，收尾那一刻可能还没写完 —— 查库会得到"少几个
// 节点 token"的假数据。内存对象不会骗人。
//
// resumeCount 是该 run 被人为续跑的次数（来自 job 表 trigger=resume 计数）；
// 传负数表示未采集，此时 HumanInterventions 写 null。
func BuildEvidence(run model.WorkflowRun, states []model.NodeInstance, sideUnloads int, resumeCount int) Evidence {
	ev := Evidence{
		SchemaVersion: EvidenceSchemaVersion,
		RunID:         run.ID,
		WorkflowID:    run.WorkflowID,
		InvokedBy:     run.InvokedBy,
		// 评测语义直接从 run 上搬：这两个事实在同一条记录里，搬错不了。
		// 指针原样传：nil 就是 nil（没采集），不许在这里补默认值。
		CaseID:             run.CaseID,
		DatasetFingerprint: run.DatasetFingerprint,
		Category:           run.Category,
		Role:               run.Role,
		WorkflowVersion:    run.WorkflowVersion,
		StartedAt:          run.StartedAt,
		EndedAt:            run.EndedAt,
		EffectiveTokens:    UsageOfNodes(states).Total(),
		SideUnloadCount:    sideUnloads,
		// 峰值显存恒 null：gpu_scheduler 的 TimelineEvent 没有 runId 字段，
		// 按时间窗反推会在并发 run 时张冠李戴。要真值先给 timeline 加 run 关联，
		// 在那之前宁可空着 —— 一个错的分位数比没有更坏。
		PeakVramMB: nil,
	}
	if run.StartedAt != nil && run.EndedAt != nil {
		ev.DurationMS = run.EndedAt.Sub(*run.StartedAt).Milliseconds()
	}
	if resumeCount >= 0 {
		n := resumeCount
		ev.HumanInterventions = &n
	}

	score, checks, gateNodes, blocked, crashed := gateFacts(states)
	ev.GateScore = score
	ev.GateChecks = checks
	ev.GateNodes = gateNodes

	// 三态判定。顺序与理由见每条注释。
	switch run.Status {
	case "cancelled":
		// 人按停的。既不是合格也不是不合格 —— 记 0 分会把"我中止的"算成"引擎做不出"。
		ev.Result = EvidenceCancelled
		ev.UnscoredReason = "cancelled"
	case "paused":
		ev.Result = EvidencePaused
		ev.UnscoredReason = "paused"
	case "failed":
		switch {
		case blocked:
			// 门跑完了检查项并判不过：这是"测了，不合格"，可以记 failed。
			ev.Result = EvidenceFailed
			f := false
			ev.Passed = &f
		case crashed:
			// 门自己崩了（缺输入产物、ffprobe 不可达）：那是"没测到"。
			ev.Result = EvidenceUnscored
			ev.UnscoredReason = "gate_error"
		default:
			// 没走到门就挂了（依赖没起、进程崩、超时）：同样是"没测到"。
			// 这类失败若记 failed，评测会误判成"引擎产出不合格"，反向甩锅。
			ev.Result = EvidenceUnscored
			ev.UnscoredReason = "execution_error"
		}
	case "success":
		if len(gateNodes) == 0 {
			// 没接门 = 没验收。**不能算通过**，否则"去掉门"反而提高通过率，
			// 评测就成了鼓励拆门（门必须有牙齿的延伸）。
			ev.Result = EvidenceUnscored
			ev.UnscoredReason = "no_gate"
		} else {
			ev.Result = EvidencePassed
			t := true
			ev.Passed = &t
		}
	default:
		ev.Result = EvidenceUnscored
		ev.UnscoredReason = "unknown_status:" + run.Status
	}
	return ev
}

// Total 总 token（输入 + 输出）。
//
// 评测只认这一个口径：外部脚本各算各的就会出现"一边算输入一边算输出"的两套账，
// 而 token 是成本对比的主指标之一，口径不一致等于结论无效。
func (u TokenUsage) Total() int { return u.PromptTokens + u.CompletionTokens }

// UsageOfNodes 从节点终态里累加 token（output._usage）。
//
// 走一次 JSON 往返而不是类型断言：_usage 在内存里是 LLMUsage 结构体，
// 经 node_states 持久化后再读回来是 map[string]interface{}，两种形态都会遇到。
// 断言写法只能覆盖一半，JSON 往返两种都覆盖，且字段名就是契约里的字段名。
func UsageOfNodes(states []model.NodeInstance) TokenUsage {
	var total TokenUsage
	for i := range states {
		out := states[i].Output
		if out == nil {
			continue
		}
		raw, err := json.Marshal(out)
		if err != nil {
			continue
		}
		var m struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"_usage"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		total.PromptTokens += m.Usage.PromptTokens
		total.CompletionTokens += m.Usage.CompletionTokens
	}
	return total
}

// gateFacts 从节点终态里收集门的检查项，算通过比例。
//
// 判定"是不是门节点"用模块契约的角色（RoleGate），不看类型字面量 ——
// 与 spec.go / preflight.go 同一条纪律（不许 switch n.Type）。
//
// 返回两个互斥的失败标记，这是本文件最容易被写错的地方：
//   - blocked：门跑完了检查项，明确判不过 → "测了，不合格"
//   - crashed：门自己执行崩了（缺输入、ffprobe 不可达、ffmpeg 缺失）→ "没测到"
//
// 两者在 run 上都表现为 status=failed。若合并成一个布尔，就会把
// "服务没起"记成"产出不合格"，评测结论直接反向（AGENTS §0.6）。
// 判据是**有没有产出 checks**：门跑完了必然留下逐项结果。
func gateFacts(states []model.NodeInstance) (score *float64, checks []GateCheckEvidence, nodes []string, blocked, crashed bool) {
	total, okCount := 0, 0
	for i := range states {
		s := &states[i]
		mod, has := ModuleContractOf(s.Type)
		if !has || mod.Role != RoleGate {
			continue
		}
		nodes = append(nodes, s.ID)

		var parsed []GateCheckEvidence
		if s.Output != nil {
			if raw, err := json.Marshal(s.Output); err == nil {
				var m struct {
					Checks []GateCheckEvidence `json:"checks"`
				}
				if err := json.Unmarshal(raw, &m); err == nil {
					parsed = m.Checks
				}
			}
		}

		if s.Status == "failed" {
			if len(parsed) > 0 {
				blocked = true // 门干活了，判不过
			} else {
				crashed = true // 门没干成活
			}
		}
		for _, c := range parsed {
			c.NodeID = s.ID
			checks = append(checks, c)
			total++
			if c.OK {
				okCount++
			}
		}
	}
	if total > 0 {
		v := float64(okCount) / float64(total)
		score = &v
	}
	return score, checks, nodes, blocked, crashed
}

// CountResumes 统计该 run 被人为续跑的次数（job 表 trigger=resume）。
//
// 这是「人类介入」的可查口径：pause 后 resume、或 rerun-from 都算一次。
// 查不到返回 -1（未采集），而不是 0 —— 0 是"测到了，人没插手"，
// 这两个事实在评测里的含义完全相反。
func CountResumes(db *gorm.DB, runID uint) int {
	if db == nil {
		return -1
	}
	var n int64
	err := db.Model(&model.Job{}).
		Where("run_id = ? AND trigger = ?", runID, "resume").
		Count(&n).Error
	if err != nil {
		return -1
	}
	return int(n)
}

// EvidenceDir 证据落盘目录
func EvidenceDir() string {
	if d := os.Getenv("EVIDENCE_DIR"); d != "" {
		return d
	}
	return "data/evidence"
}

// WriteEvidence 把信封写到 <dir>/run-<id>.json。
//
// 原子写：临时文件 + rename。评测读到半截 JSON 会直接判「不可达」，
// 那比没有证据更糟（会把一次成功的运行变成一次"无结果"）。
//
// 返回落盘路径；失败只记日志不返回错误 —— 证据是旁路，
// 它坏了不能反过来把一次成功的运行判成失败。
func WriteEvidence(ev Evidence) string {
	dir := EvidenceDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[Evidence] 建目录失败 %s：%v", dir, err)
		return ""
	}
	body, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		log.Printf("[Evidence] 序列化失败 run=%d：%v", ev.RunID, err)
		return ""
	}
	final := filepath.Join(dir, fmt.Sprintf("run-%d.json", ev.RunID))
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		log.Printf("[Evidence] 写临时文件失败 %s：%v", tmp, err)
		return ""
	}
	if err := os.Rename(tmp, final); err != nil {
		log.Printf("[Evidence] rename 失败 %s：%v", final, err)
		_ = os.Remove(tmp)
		return ""
	}
	return final
}
