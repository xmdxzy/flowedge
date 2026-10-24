package model

import (
	"encoding/json"
	"time"
)

// 节点失败原因的稳定错误码（前端与 API 直接消费，便于定位与告警）
const (
	ErrorCodeTimeout        = "timeout"         // 节点执行超时
	ErrorCodeConfigMissing  = "config_missing"  // 依赖的外部服务未配置（如 ASR/TTS 未启动）
	ErrorCodeUpstream       = "upstream_failed" // 上游节点失败导致本节点无法执行
	ErrorCodeRetryExhausted = "retry_exhausted" // 重试次数耗尽
	ErrorCodeCancelled      = "cancelled"       // 运行被中止
	ErrorCodeCycle          = "cycle"           // 图存在循环依赖
	ErrorCodeExecution      = "execution_error" // 其他执行错误
)

// Workflow 工作流定义
type Workflow struct {
	ID        uint            `gorm:"primaryKey" json:"id"`
	Name      string          `gorm:"size:255;not null" json:"name"`
	Nodes     json.RawMessage `gorm:"type:text" json:"nodes"`
	Edges     json.RawMessage `gorm:"type:text" json:"edges"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`

	// 没有触发器/没有 cron：出片只能由人（或 MCP 客户端）显式发起。
}

// WorkflowRun 工作流执行记录
type WorkflowRun struct {
	ID         uint            `gorm:"primaryKey" json:"id"`
	WorkflowID uint            `gorm:"index" json:"workflow_id"`
	Status     string          `gorm:"size:20;default:pending" json:"status"` // pending, running, paused, success, failed
	NodeStates json.RawMessage `gorm:"type:text" json:"node_states"`
	Error      string          `gorm:"type:text" json:"error,omitempty"`
	// PipelineID 这次出片用的是哪条流水线（data/pipelines.json 里的 id）。
	// 与 WorkflowID 分工：WorkflowID 是实际执行的那张图（质量循环时是临时图），
	// PipelineID 是用户点的那个东西 —— 重跑、看板分组都靠它。
	PipelineID string `gorm:"size:64;index" json:"pipeline_id,omitempty"`
	// Params 起跑时生效的参数（默认值已合并）。重跑直接用这一份，
	// 不重新猜默认值 —— 否则"重跑"会悄悄换了参数，比较就失去意义。
	Params json.RawMessage `gorm:"type:text" json:"params,omitempty"`
	// InvokedBy 谁发起的这次出片：manual / workbuddy / dsh / mcp。
	// 如实记录，不猜；未知一律记 manual。
	InvokedBy string `gorm:"size:32" json:"invoked_by,omitempty"`

	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// NodeInstance 节点运行时状态
type NodeInstance struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Position  map[string]float64     `json:"position"`
	Data      map[string]interface{} `json:"data"`
	Status    string                 `json:"status"` // pending, running, success, failed, skipped
	Output    interface{}            `json:"output,omitempty"`
	Error     string                 `json:"error,omitempty"`
	ErrorCode string                 `json:"error_code,omitempty"` // 见 ErrorCode* 常量
	StartedAt *time.Time             `json:"started_at,omitempty"`
	EndedAt   *time.Time             `json:"ended_at,omitempty"`
}

// EdgeInstance 连线（字段命名与 React Flow 对齐）
type EdgeInstance struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	Target       string `json:"target"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	TargetHandle string `json:"targetHandle,omitempty"`
}
