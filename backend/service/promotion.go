package service

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// 晋升台账（PromotionStore）
//
// 与 LineageStore 同构：一个薄存储层，牙齿全在 Record 的校验里。
// 校验规则不是"为了严谨而严谨"，每一条都对应一种真实会发生的事故：
//
//   - action 不在枚举内 → 台账里混进看不懂的记录，审计时要猜
//   - 空理由 → "当时凭什么"这一栏永远空着，台账退化成日记
//   - promote/rollback 的 toVersion=0 → 记了"晋升"却不知道升到哪，
//     想复核得先把所有版本翻一遍
//   - workflowId=0 → 挂到不存在的图上的记录，等于没挂

// PromotionStore 晋升记录读写
type PromotionStore struct {
	db *gorm.DB
}

// NewPromotionStore db 为 nil 时返回 nil（调用方按"无台账"降级，
// 与 LineageStore / 其它旁路存储一致：进化是旁路，不能因为它把主链路打挂）
func NewPromotionStore(db *gorm.DB) *PromotionStore {
	if db == nil {
		return nil
	}
	return &PromotionStore{db: db}
}

// ValidatePromotion 落库前的校验。错误信息必须能让调用方直接改对，
// 不许只说"invalid"。
func ValidatePromotion(rec model.Promotion) error {
	if rec.WorkflowID == 0 {
		return fmt.Errorf("workflowId 必填：晋升记录必须挂到具体的工作流上")
	}
	switch rec.Action {
	case model.PromotionActionPromote, model.PromotionActionKeep,
		model.PromotionActionRollback, model.PromotionActionArchive:
	default:
		return fmt.Errorf("action 必须是 promote / keep / rollback / archive 之一，收到 %q", rec.Action)
	}
	switch rec.Source {
	case model.PromotionSourcePromote, model.PromotionSourceDecide,
		model.PromotionSourceMonitor, model.PromotionSourceRollback:
	default:
		return fmt.Errorf("source 必须是 promote / decide / monitor / rollback 之一，收到 %q", rec.Source)
	}
	if strings.TrimSpace(rec.Reason) == "" {
		return fmt.Errorf("reason 必填：没有理由的判定不许落库（台账的价值就在「当时凭什么」）")
	}
	if rec.FromVersion < 0 || rec.ToVersion < 0 {
		return fmt.Errorf("版本号不能为负（from=%d, to=%d）", rec.FromVersion, rec.ToVersion)
	}
	// 晋升与回滚都必须有明确目标版本：只写"晋升了"不写"升到哪版"，
	// 事后复核要把所有版本翻一遍，那等于没记。
	if (rec.Action == model.PromotionActionPromote || rec.Action == model.PromotionActionRollback) &&
		rec.ToVersion == 0 {
		return fmt.Errorf("action=%s 必须给出 toVersion（要晋升/回滚到哪一版）", rec.Action)
	}
	return nil
}

// Record 落一条台账。校验不过返回 error，**不落库**。
// Actor 为空按 manual 记；CreatedAt 由存储层统一打，不接受调用方传入的时间。
func (s *PromotionStore) Record(rec model.Promotion) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("晋升台账未装配（db 为 nil）")
	}
	if err := ValidatePromotion(rec); err != nil {
		return err
	}
	rec.Reason = strings.TrimSpace(rec.Reason)
	if strings.TrimSpace(rec.Actor) == "" {
		rec.Actor = "manual"
	}
	rec.CreatedAt = time.Now()
	return s.db.Create(&rec).Error
}

// List 一个工作流的晋升台账，新的在前。limit<=0 或 >500 按 200。
func (s *PromotionStore) List(workflowID uint, limit int) []model.Promotion {
	out := []model.Promotion{}
	if s == nil || s.db == nil {
		return out
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	s.db.Where("workflow_id = ?", workflowID).Order("id desc").Limit(limit).Find(&out)
	return out
}

// Latest 最近一条台账（没有则 ok=false）。用于面板上"当前基线是谁按下的"。
func (s *PromotionStore) Latest(workflowID uint) (model.Promotion, bool) {
	var rec model.Promotion
	if s == nil || s.db == nil {
		return rec, false
	}
	if res := s.db.Where("workflow_id = ?", workflowID).Order("id desc").Limit(1).Find(&rec); res.Error != nil || rec.ID == 0 {
		return rec, false
	}
	return rec, true
}

// FromDecision 把一次配对判定翻成台账条目（archive：引擎结论归档，人未按键）。
//
// verdict 原样搬（promote / keep_baseline / inconclusive），action 按口径映射：
// 引擎说 promote 只是"建议"，不等于人按了晋升 —— 所以判 promote 也记 archive，
// 人真按下时走 POST /api/evolution/promote，那一条才是 promote。
func FromDecision(workflowID uint, d Decision, reason string) model.Promotion {
	return model.Promotion{
		WorkflowID:          workflowID,
		Action:              model.PromotionActionArchive,
		Source:              model.PromotionSourceDecide,
		Verdict:             d.Verdict,
		Pairs:               d.Pairs,
		MatchedCases:        d.MatchedCases,
		GateScoreDelta:      d.GateScoreDelta,
		TokenDeltaPct:       d.TokenDeltaPct,
		LatencyDeltaPct:     d.LatencyDeltaPct,
		CriticalRegressions: d.CriticalRegressions,
		Reason:              reason,
		Actor:               "engine",
	}
}

// FromRegression 把一次回归体检翻成台账条目（archive）。
func FromRegression(workflowID uint, rep RegressionReport, reason string) model.Promotion {
	return model.Promotion{
		WorkflowID:          workflowID,
		Action:              model.PromotionActionArchive,
		Source:              model.PromotionSourceMonitor,
		Verdict:             rep.Verdict,
		FromVersion:         rep.CandidateVersion,
		ToVersion:           rep.RollbackTo,
		Pairs:               rep.Decision.Pairs,
		MatchedCases:        rep.Decision.MatchedCases,
		GateScoreDelta:      rep.Decision.GateScoreDelta,
		TokenDeltaPct:       rep.Decision.TokenDeltaPct,
		LatencyDeltaPct:     rep.Decision.LatencyDeltaPct,
		CriticalRegressions: rep.Decision.CriticalRegressions,
		Reason:              reason,
		Actor:               "engine",
	}
}
