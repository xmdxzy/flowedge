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

	// 触发器：外部系统可通过 Webhook token 调用，或按 cron 定时执行
	TriggerToken    string     `gorm:"size:64;index" json:"trigger_token"`
	CronExpr        string     `gorm:"size:64" json:"cron_expr"`
	TriggerEnabled  bool       `gorm:"default:false" json:"trigger_enabled"`
	LastTriggeredAt *time.Time `json:"last_triggered_at"`
}

// WorkflowRun 工作流执行记录
type WorkflowRun struct {
	ID         uint            `gorm:"primaryKey" json:"id"`
	WorkflowID uint            `gorm:"index" json:"workflow_id"`
	Status     string          `gorm:"size:20;default:pending" json:"status"` // pending, running, paused, success, failed
	NodeStates json.RawMessage `gorm:"type:text" json:"node_states"`
	Error      string          `gorm:"type:text" json:"error,omitempty"`
	// 断点调试：停在哪个节点 / 为什么停（step=单步耗尽，breakpoint=断点）
	// InvokedBy 谁触发了这次运行：manual / workbuddy / dsh / mcp / webhook / cron。
	// 与开始节点声明的 entry_agent 分开记：声明是「预期」，这里是「实际」，
	// 两者不一致时要在提交响应里告诉调用方（而不是默默按预期跑）。
	InvokedBy   string `gorm:"size:32" json:"invoked_by,omitempty"`
	PausedAt    string `gorm:"size:64" json:"paused_at,omitempty"`
	PauseReason string `gorm:"size:32" json:"pause_reason,omitempty"`

	// 评测语义：由评测层在**起跑时**注入，引擎只落库、只回填，绝不猜。
	//
	// 为什么放在 run 上而不是事后补：证据信封在运行结束那一刻落盘，
	// 事后补等于改一份已经写死的「事实」——那是伪造证据，不是采集证据。
	// 字段用指针：没传就是 null（没采集），不能写成空串冒充「采集到了空值」。
	CaseID             *string `gorm:"size:128;index" json:"case_id,omitempty"`
	DatasetFingerprint *string `gorm:"size:64" json:"dataset_fingerprint,omitempty"`
	Category           *string `gorm:"size:64;index" json:"category,omitempty"`
	Role               string  `gorm:"size:16;index" json:"role,omitempty"` // baseline / candidate
	EvalBatch          string  `gorm:"size:64;index" json:"eval_batch,omitempty"`

	// WorkflowVersion 起跑那一刻的图版本（0 = 没有快照）。
	//
	// 必须在起跑时钉住：原来在收尾时读「当前最大版本号」，一旦运行期间
	// 有人改图并存了版本，一次旧图的运行会被记成新图的成绩 —— 配对评测
	// 直接张冠李戴，且事后完全看不出来（版本号看着都对）。
	WorkflowVersion int `gorm:"default:0" json:"workflow_version"`

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
