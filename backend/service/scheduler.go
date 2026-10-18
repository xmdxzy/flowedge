package service

import (
	"log"
	"sync"
	"time"
)

// TriggerConfig 定时触发配置
type TriggerConfig struct {
	ID         string `json:"id"`
	WorkflowID uint   `json:"workflow_id"`
	CronExpr   string `json:"cron_expr"` // 简化版：* * * * *
	Enabled    bool   `json:"enabled"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
}

// TriggerScheduler 定时触发器
type TriggerScheduler struct {
	triggers map[string]*TriggerConfig
	stopCh   chan struct{}
	mu       sync.RWMutex
	runner   func(workflowID uint)
}

func NewTriggerScheduler(runner func(workflowID uint)) *TriggerScheduler {
	return &TriggerScheduler{
		triggers: make(map[string]*TriggerConfig),
		stopCh:   make(chan struct{}),
		runner:   runner,
	}
}

// Start 启动调度器
func (s *TriggerScheduler) Start() {
	go s.runLoop()
	log.Println("[Scheduler] trigger scheduler started")
}

// Stop 停止调度器
func (s *TriggerScheduler) Stop() {
	close(s.stopCh)
}

// AddTrigger 添加定时任务
func (s *TriggerScheduler) AddTrigger(config *TriggerConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.calculateNextRun(config.CronExpr, time.Now())
	config.NextRunAt = &next
	s.triggers[config.ID] = config
}

// RemoveTrigger 移除定时任务
func (s *TriggerScheduler) RemoveTrigger(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.triggers, id)
}

// EnableTrigger 启用/禁用
func (s *TriggerScheduler) EnableTrigger(id string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if trigger, ok := s.triggers[id]; ok {
		trigger.Enabled = enabled
	}
}

// ListTriggers 列出所有触发器
func (s *TriggerScheduler) ListTriggers() []*TriggerConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*TriggerConfig, 0, len(s.triggers))
	for _, t := range s.triggers {
		result = append(result, t)
	}
	return result
}

// runLoop 主循环
func (s *TriggerScheduler) runLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case now := <-ticker.C:
			s.checkTriggers(now)
		}
	}
}

// checkTriggers 检查并触发到期的任务
func (s *TriggerScheduler) checkTriggers(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, trigger := range s.triggers {
		if !trigger.Enabled {
			continue
		}

		if trigger.NextRunAt == nil {
			continue
		}

		if now.After(*trigger.NextRunAt) || now.Equal(*trigger.NextRunAt) {
			log.Printf("[Scheduler] triggering workflow %d", trigger.WorkflowID)

			// 执行
			if s.runner != nil {
				go s.runner(trigger.WorkflowID)
			}

			trigger.LastRunAt = &now
			next := s.calculateNextRun(trigger.CronExpr, now)
			trigger.NextRunAt = &next
		}
	}
}

// calculateNextRun 简化版 cron 计算
// 支持的格式：
// * * * * * = 每分钟
// 0 * * * * = 每小时
// 0 0 * * * = 每天
// 0 9 * * 1-5 = 工作日早上9点
// */5 * * * * = 每5分钟
func (s *TriggerScheduler) calculateNextRun(expr string, from time.Time) time.Time {
	next := from.Add(1 * time.Minute)

	if expr == "" {
		return next
	}

	// 简化处理
	switch expr {
	case "* * * * *":
		return next
	case "*/5 * * * *":
		return next.Add(4 * time.Minute)
	case "*/15 * * * *":
		return next.Add(14 * time.Minute)
	case "*/30 * * * *":
		return next.Add(29 * time.Minute)
	case "0 * * * *":
		next = next.Add(-time.Duration(next.Minute()) * time.Minute)
		return next
	case "0 0 * * *":
		next = next.AddDate(0, 0, 1)
		next = time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, next.Location())
		return next
	case "0 9 * * *":
		next = next.AddDate(0, 0, 1)
		next = time.Date(next.Year(), next.Month(), next.Day(), 9, 0, 0, 0, next.Location())
		return next
	default:
		return next
	}
}
