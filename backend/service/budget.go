package service

// 预算门（V3 P0）
//
// 现状：FE 能估（EstimateGraph 出 P50/P90/worst），但**估完就没人管**——
// 跑起来之后无论烧多久都不会停，用户只能盯屏幕或等它自己撞节点超时。
// 于是"30 秒的片子要多久"这个问题，答案永远是"看情况"。
//
// 本文件只做一件事：给一次运行一个**可声明的墙钟上限**，超了就优雅取消并保留已完成产物。
//
// 刻意不做 GPU 秒预算：引擎目前没有实时采集 GPU 秒（只有节点耗时与峰值显存），
// 没有数据就不做字段——否则又是"配了却不生效"的那一类（AGENTS §0.4）。

import (
	"fmt"
	"strings"
)

// Budget 一次运行的预算声明。
type Budget struct {
	// MaxWallSeconds 墙钟上限（秒）。<=0 表示不限制。
	MaxWallSeconds float64 `json:"max_wall_seconds"`
	// OnExceed 超限动作：warn（只告警，默认）| stop（取消运行）。
	// 默认 warn 是刻意的：预算门误杀一条跑了 20 分钟的长片，代价远大于多烧几分钟。
	OnExceed string `json:"on_exceed"`
}

// ParseBudget 从出片参数里读预算声明。
//
// 支持两种写法（都容错，不因写错就让出片失败）：
//   - params.budget = {"max_wall_seconds": 900, "on_exceed": "stop"}
//   - params.max_wall_seconds = 900（简写，OnExceed 取默认）
func ParseBudget(params map[string]interface{}) Budget {
	b := Budget{OnExceed: "warn"}
	if params == nil {
		return b
	}
	if raw, ok := params["budget"].(map[string]interface{}); ok {
		if v, ok := raw["max_wall_seconds"].(float64); ok && v > 0 {
			b.MaxWallSeconds = v
		}
		if s, ok := raw["on_exceed"].(string); ok {
			b.OnExceed = normalizeOnExceed(s)
		}
	}
	if b.MaxWallSeconds <= 0 {
		if v, ok := params["max_wall_seconds"].(float64); ok && v > 0 {
			b.MaxWallSeconds = v
		}
	}
	return b
}

func normalizeOnExceed(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "stop", "cancel", "abort":
		return "stop"
	default:
		return "warn"
	}
}

// Enabled 是否声明了预算。
func (b Budget) Enabled() bool { return b.MaxWallSeconds > 0 }

// ShouldStop 是否应该在超限时取消（只有显式声明 stop 才取消）。
func (b Budget) ShouldStop() bool { return b.Enabled() && b.OnExceed == "stop" }

// Check 判断是否超限。返回 (超限, 人话原因)。
func (b Budget) Check(elapsedSeconds float64) (bool, string) {
	if !b.Enabled() || elapsedSeconds <= b.MaxWallSeconds {
		return false, ""
	}
	return true, fmt.Sprintf("已跑 %.0fs，超过声明上限 %.0fs（on_exceed=%s）",
		elapsedSeconds, b.MaxWallSeconds, b.OnExceed)
}

// Describe 给日志/响应用的短描述。
func (b Budget) Describe() string {
	if !b.Enabled() {
		return "未声明预算"
	}
	return fmt.Sprintf("墙钟 ≤%.0fs，超限 %s", b.MaxWallSeconds, b.OnExceed)
}

// BudgetReconcile 预估与实耗的对账（写进 manifest，供后续校准估算模型）。
type BudgetReconcile struct {
	PredictedP50Sec   float64 `json:"predicted_p50_sec,omitempty"`
	PredictedP90Sec   float64 `json:"predicted_p90_sec,omitempty"`
	PredictedWorstSec float64 `json:"predicted_worst_sec,omitempty"`
	ActualSec         float64 `json:"actual_sec,omitempty"`
	// Verdict 实耗落在哪一档：under_p50 / in_p50_p90 / over_p90 / over_worst / unknown
	Verdict string `json:"verdict,omitempty"`
	// WithinBudget 是否在声明预算内；未声明预算时为空。
	WithinBudget *bool `json:"within_budget,omitempty"`
}

// Reconcile 生成对账结论。actual<=0 时返回 unknown（没跑完就没有结论，不猜）。
func (b Budget) Reconcile(est GraphEstimate, actualSec float64) BudgetReconcile {
	r := BudgetReconcile{
		PredictedP50Sec:   est.P50Sec,
		PredictedP90Sec:   est.P90Sec,
		PredictedWorstSec: est.WorstSec,
		ActualSec:         actualSec,
	}
	switch {
	case actualSec <= 0:
		r.Verdict = "unknown"
	case est.P50Sec > 0 && actualSec <= est.P50Sec:
		r.Verdict = "under_p50"
	case est.P90Sec > 0 && actualSec <= est.P90Sec:
		r.Verdict = "in_p50_p90"
	case est.WorstSec > 0 && actualSec <= est.WorstSec:
		r.Verdict = "over_p90"
	default:
		r.Verdict = "over_worst"
	}
	if b.Enabled() {
		ok := actualSec <= b.MaxWallSeconds
		r.WithinBudget = &ok
	}
	return r
}
