package service

import (
	"log"
	"sync"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// JobQueue 持久化任务队列。
//
// 定位：把「任务已受理」落库，用信号量限制并发。
// 单机单 GPU 场景下并发上限默认 2 —— 两个 flux 并行只会互相抢显存。
// 若需要水平扩容，应换外部 broker（Redis/NATS），本实现不做分布式承诺。
type JobQueue struct {
	db     *gorm.DB
	sem    chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

func NewJobQueue(db *gorm.DB, maxConcurrent int) *JobQueue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &JobQueue{
		db:  db,
		sem: make(chan struct{}, maxConcurrent),
	}
}

// Enqueue 异步执行任务。fn 抛 panic 不会拖垮整条队列（仅记日志）。
func (q *JobQueue) Enqueue(job model.Job, fn func()) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.mu.Unlock()

	q.sem <- struct{}{}
	q.wg.Add(1)
	go func() {
		defer func() {
			<-q.sem
			q.wg.Done()
		}()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[Queue] job %d panic recovered: %v", job.ID, r)
				q.db.Model(&model.Job{}).Where("id = ?", job.ID).
					Updates(map[string]interface{}{"status": "failed", "error": "panic in task"})
			}
		}()
		fn()
	}()
}

// RecoverInterrupted 启动时把上次进程留下的 running 任务标记为 interrupted。
// 这类任务的结果不可信（可能只跑了一半），用户需显式续跑。
//
// 2026-10-20 修复：原来只回收 jobs 表，漏了 workflow_runs 表——
// 进程被杀后 run 永远卡在 running（现存 3 条 10-19/10-20 的僵尸记录），
// 执行面板里既不能续跑也不能重跑，只能脏在列表里。两张表必须一起收。
func (q *JobQueue) RecoverInterrupted() int64 {
	var total int64
	res := q.db.Model(&model.Job{}).Where("status = ?", "running").
		Updates(map[string]interface{}{"status": "interrupted", "error": "进程异常退出，任务被中断"})
	if res.Error == nil {
		total += res.RowsAffected
	}
	res2 := q.db.Model(&model.WorkflowRun{}).Where("status = ?", "running").
		Updates(map[string]interface{}{"status": "failed", "error": "进程异常退出，运行被中断（启动时回收）；可在执行记录里续跑"})
	if res2.Error == nil {
		total += res2.RowsAffected
	}
	return total
}

// Wait 优雅退出：等待已受理任务结束
func (q *JobQueue) Wait() { q.wg.Wait() }

// Close 停止接收新任务（已入队的仍会跑完）
func (q *JobQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
}

// ListRecent 查询任务记录（运维排障）
func (q *JobQueue) ListRecent(limit int) []model.Job {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var jobs []model.Job
	q.db.Order("created_at DESC").Limit(limit).Find(&jobs)
	return jobs
}
