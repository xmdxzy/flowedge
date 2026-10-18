package service

import (
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// CronScheduler 定时触发器：轮询带 cron 表达式的已启用工作流并触发执行
//
// 设计要点：
//   - 轮询而非精确 timer：进程重启后不会丢失计划，也不需要维护一堆 timer
//   - 用「分钟键」去重，避免同一分钟被多次触发
//   - 触发动作通过回调注入，避免 service 反向依赖 handler
type CronScheduler struct {
	db       *gorm.DB
	fire     func(wfID uint) error
	interval time.Duration
	lastKey  map[uint]string
	mu       sync.Mutex
	stopCh   chan struct{}
	started  bool
}

func NewCronScheduler(db *gorm.DB, fire func(wfID uint) error) *CronScheduler {
	return &CronScheduler{
		db:      db,
		fire:    fire,
		lastKey: make(map[uint]string),
		stopCh:  make(chan struct{}),
	}
}

// Start 启动轮询（interval 建议 20~30 秒）
func (s *CronScheduler) Start(interval time.Duration) {
	if s == nil || s.db == nil || s.fire == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.interval = interval
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case now := <-ticker.C:
				s.Tick(now)
			}
		}
	}()
}

// Stop 停止轮询
func (s *CronScheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		close(s.stopCh)
		s.started = false
	}
}

// Tick 执行一次扫描（导出以便单元测试直接驱动）
func (s *CronScheduler) Tick(now time.Time) {
	if s == nil || s.db == nil {
		return
	}
	var workflows []model.Workflow
	if err := s.db.Where("trigger_enabled = ? AND cron_expr <> ''", true).Find(&workflows).Error; err != nil {
		return
	}

	key := now.Format("20060102T1504") // 分钟级去重键
	for _, wf := range workflows {
		spec, err := ParseCron(wf.CronExpr)
		if err != nil {
			continue
		}
		if !spec.Match(now) {
			continue
		}

		s.mu.Lock()
		if s.lastKey[wf.ID] == key {
			s.mu.Unlock()
			continue
		}
		s.lastKey[wf.ID] = key
		s.mu.Unlock()

		fireAt := now
		s.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Update("last_triggered_at", &fireAt)
		// 同步触发：fire 内部只做「建记录 + 起协程执行」，本身很快；
		// 同步语义也让「同分钟只触发一次」可被单元测试确定性验证
		if err := s.fire(wf.ID); err != nil {
			log.Printf("[cron] 触发工作流 #%d（%s）失败：%v", wf.ID, wf.Name, err)
			continue
		}
		log.Printf("[cron] 已按计划触发工作流 #%d（%s）", wf.ID, wf.Name)
	}
}

// CronSummary 供前端展示的调度概览
func (s *CronScheduler) CronSummary() []map[string]interface{} {
	if s == nil || s.db == nil {
		return nil
	}
	var workflows []model.Workflow
	s.db.Where("trigger_enabled = ?", true).Find(&workflows)
	out := make([]map[string]interface{}, 0, len(workflows))
	for _, wf := range workflows {
		desc := ""
		if spec, err := ParseCron(wf.CronExpr); err == nil {
			desc = spec.Describe()
		}
		out = append(out, map[string]interface{}{
			"workflow_id": wf.ID,
			"name":        wf.Name,
			"cron":        wf.CronExpr,
			"description": desc,
			"last_fired":  wf.LastTriggeredAt,
		})
	}
	return out
}

// ValidateCron 校验并返回人话描述
func ValidateCron(expr string) (string, error) {
	spec, err := ParseCron(expr)
	if err != nil {
		return "", err
	}
	return spec.Describe(), nil
}
