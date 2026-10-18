package model

import (
	"encoding/json"
	"time"
)

// WorkflowVersion 工作流版本
type WorkflowVersion struct {
	ID         uint            `gorm:"primaryKey" json:"id"`
	WorkflowID uint            `gorm:"index" json:"workflow_id"`
	Version    int             `json:"version"`
	Name       string          `json:"name"`
	Nodes      json.RawMessage `gorm:"type:text" json:"nodes"`
	Edges      json.RawMessage `gorm:"type:text" json:"edges"`
	ChangeNote string          `gorm:"type:text" json:"change_note,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// TableName 表名
func (WorkflowVersion) TableName() string {
	return "workflow_versions"
}
