package model

import "time"

// 晋升/保留/回滚台账（Promotion）
//
// 解决的问题：自进化闭环里"人按下晋升"这个动作以前只存在于对话里。
// 三个月后没人答得上来「v12 是谁在哪天凭什么判成晋升的」——而晋升恰恰是
// 整个闭环里唯一不可自动重放的决策（判定可以重跑，人按的那一下不能）。
//
// 这里把每次判定落成一条不可变记录：谁、哪天、哪个工作流、从哪版到哪版、
// 引擎当时的结论与数值快照、理由。回滚与监测结论同样落库，
// 事后审计不需要重跑评测就能复核"当时凭什么"。

// 晋升动作：人按下的键（archive 是引擎结论归档，人未按键）
const (
	PromotionActionPromote  = "promote"  // 晋升：候选版本成为新基线
	PromotionActionKeep     = "keep"     // 保留：维持基线，不采纳候选
	PromotionActionRollback = "rollback" // 回滚：把当前图退回历史版本
	PromotionActionArchive  = "archive"  // 引擎结论归档（decide/monitor 落库）
)

// 记录来源：这条记录是从哪个端点落的
const (
	PromotionSourcePromote  = "promote"  // POST /api/evolution/promote
	PromotionSourceDecide   = "decide"   // POST /api/evolution/decide（record=true）
	PromotionSourceMonitor  = "monitor"  // GET  /api/evolution/monitor（record=true）
	PromotionSourceRollback = "rollback" // POST /api/evolution/rollback（apply=true）
)

// Promotion 一条晋升/保留/回滚记录
type Promotion struct {
	ID         uint `gorm:"primaryKey" json:"id"`
	WorkflowID uint `gorm:"index" json:"workflow_id"`

	// Action 判定类型：promote / keep / rollback / archive
	Action string `gorm:"size:16;index" json:"action"`
	// Source 来源端点：promote / decide / monitor / rollback
	Source string `gorm:"size:16" json:"source"`
	// Verdict 引擎当时的结论原文（decide: promote/keep_baseline/inconclusive；
	// monitor: healthy/regressed/insufficient）。纯人工记录可为空。
	Verdict string `gorm:"size:24" json:"verdict,omitempty"`

	// FromVersion / ToVersion 从哪一版到哪一版。
	// keep 时 ToVersion=0（不动图）；promote/rollback 必须 >0。
	FromVersion int `json:"from_version"`
	ToVersion   int `json:"to_version"`

	// 判定数值快照：复核时不用重跑评测。没有对应的数就留 0，
	// 配合 Pairs 一起读（Pairs=0 表示当时没有配对证据）。
	Pairs               int     `json:"pairs"`
	MatchedCases        int     `json:"matched_cases"`
	GateScoreDelta      float64 `json:"gate_score_delta"`
	TokenDeltaPct       float64 `json:"token_delta_pct"`
	LatencyDeltaPct     float64 `json:"latency_delta_pct"`
	CriticalRegressions int     `json:"critical_regressions"`

	// Reason 一句话理由。**必填**：没有理由的判定不许落库 ——
	// 台账的全部价值就在于"当时凭什么"，空理由等于没记。
	Reason string `gorm:"type:text" json:"reason"`
	// Actor 谁按的：manual（默认）/ engine（自动归档）/ 调用方显式传入的名字
	Actor string `gorm:"size:64" json:"actor"`
	// Note 附加说明（可选）
	Note string `gorm:"type:text" json:"note,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName 表名
func (Promotion) TableName() string {
	return "promotions"
}
