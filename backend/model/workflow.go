package model

import (
	"encoding/json"
	"time"
)

// Workflow 工作流定义
type Workflow struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Name      string         `gorm:"size:255;not null" json:"name"`
	Nodes     json.RawMessage `gorm:"type:text" json:"nodes"`
	Edges     json.RawMessage `gorm:"type:text" json:"edges"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// WorkflowRun 工作流执行记录
type WorkflowRun struct {
	ID         uint            `gorm:"primaryKey" json:"id"`
	WorkflowID uint            `gorm:"index" json:"workflow_id"`
	Status     string          `gorm:"size:20;default:pending" json:"status"` // pending, running, success, failed
	NodeStates json.RawMessage `gorm:"type:text" json:"node_states"`
	Error      string          `gorm:"type:text" json:"error,omitempty"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// NodeInstance 节点运行时状态
type NodeInstance struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Position  map[string]float64     `json:"position"`
	Data      map[string]interface{} `json:"data"`
	Status    string                 `json:"status"` // pending, running, success, failed
	Output    interface{}            `json:"output,omitempty"`
	Error     string                 `json:"error,omitempty"`
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
