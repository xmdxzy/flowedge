package model

import "time"

// ShotJob 一个镜头级作业：分镜执行器（shotlist）的每个镜头都是一条可复用、可重跑的记录。
//
// 为什么需要它（2026-10-19 审计）：
//  1. 原先是「单节点内大循环」，第 5 镜失败直接 return nil，前 4 镜的成果一起丢掉；
//  2. 「只重拍第 3 镜」做不到，只能整片重烧；
//  3. 一个 6 镜长片在界面上就是一个转圈的节点，不知道跑到第几镜。
//
// 镜头级作业把生成拆成可断点、可复用、可重跑的单元：
//   - 每镜成功立刻落盘 → 失败重跑时已完成镜头直接复用（不重烧显存）
//   - 复用判据是 PromptHash：改了某镜提示词，只有那一镜会重生成
//   - 节点字段 reroll 可强制指定镜号重拍
type ShotJob struct {
	ID        uint `gorm:"primaryKey"`
	RunID     uint `gorm:"index:idx_shot_lookup,priority:1"`
	NodeID    string `gorm:"index:idx_shot_lookup,priority:2;size:64"`
	ShotIndex int    `gorm:"index:idx_shot_lookup,priority:3"`
	PromptHash string `gorm:"index:idx_shot_lookup,priority:4;size:64"`

	ShotID     int `json:"shot_id"`
	Prompt     string `gorm:"type:text"`
	File       string
	Frames     int
	Seconds    float64
	Transition string `gorm:"size:24"`
	Seed       int
	// Status: running / success / failed
	Status  string `gorm:"size:16;index"`
	Error   string `gorm:"type:text"`
	Note    string `gorm:"type:text"`
	ElapsedMS int64
	// VramPeakMB 留给后续显存采样接入（AGENTS §2：显存改动必须实测峰值）
	VramPeakMB int64
	Reused     bool `gorm:"-"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
