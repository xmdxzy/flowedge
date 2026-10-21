package model

import "time"

// Job 运行任务：持久化队列的事实来源。
//
// 原实现用 `go func()` 直接起跑，进程重启后 DB 里的 running 记录永远停在 running，
// 任务也不再有后续。Job 表把「已受理」这个事实落库，启动时可识别并标记为中断。
type Job struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	RunID      uint       `gorm:"index" json:"run_id"`
	WorkflowID uint       `gorm:"index" json:"workflow_id"`
	Status     string     `gorm:"size:20;default:pending" json:"status"` // pending/running/done/failed/interrupted
	Trigger    string     `gorm:"size:20" json:"trigger"`                // manual/webhook/cron/resume
	Error      string     `gorm:"type:text" json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
}
