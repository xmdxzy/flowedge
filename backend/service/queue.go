package service

import (
	"log"
	"sync"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// JobQueue 持久化任务队列。
//
// 定位：把「任务已受理」落库，用并发上限限制同时执行数量。
//
// 两条硬约束（都踩过）：
//  1. **Enqueue 绝不阻塞**。旧实现是 `sem <- struct{}{}`：信号量满时调用方
//     （HTTP handler）就卡住，界面表现为「点出片转圈 3 分钟后超时」——
//     而任务其实还在排队。现在改成入队 + 调度，调用方立即返回。
//  2. 默认并发 1。这是单卡产品，两条出片并行只会互相抢显存：
//     实测 H3 跑片时并发一条文生图，后者直接撞 5m15s 节点超时。
//     多卡/纯 CPU 场景用 FLOWEDGE_QUEUE_CONCURRENCY 显式调大。
type JobQueue struct {
	db *gorm.DB

	mu      sync.Mutex
	pending []queuedJob
	running int
	max     int
	closed  bool

	wg sync.WaitGroup
}

type queuedJob struct {
	job model.Job
	fn  func()
}

func NewJobQueue(db *gorm.DB, maxConcurrent int) *JobQueue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &JobQueue{db: db, max: maxConcurrent}
}

// Enqueue 受理任务并立即返回（不阻塞调用方），按 FIFO 顺序调度。
func (q *JobQueue) Enqueue(job model.Job, fn func()) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		q.db.Model(&model.Job{}).Where("id = ?", job.ID).
			Updates(map[string]interface{}{"status": "interrupted", "error": "引擎正在关闭，任务未执行"})
		return
	}
	q.pending = append(q.pending, queuedJob{job: job, fn: fn})
	q.mu.Unlock()
	q.dispatch()
}

// dispatch 在有空位时把队头任务拉起来（可重入：任务结束时再调一次）。
func (q *JobQueue) dispatch() {
	for {
		q.mu.Lock()
		if q.closed || q.running >= q.max || len(q.pending) == 0 {
			q.mu.Unlock()
			return
		}
		next := q.pending[0]
		q.pending = q.pending[1:]
		q.running++
		q.mu.Unlock()

		q.wg.Add(1)
		go func(item queuedJob) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Queue] job %d panic recovered: %v", item.job.ID, r)
					q.db.Model(&model.Job{}).Where("id = ?", item.job.ID).
						Updates(map[string]interface{}{"status": "failed", "error": "panic in task"})
				}
				q.mu.Lock()
				q.running--
				q.mu.Unlock()
				q.wg.Done()
				q.dispatch()
			}()
			item.fn()
		}(next)
	}
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

// Pending 队列里还没开始的任务数（运维/看板用）。
func (q *JobQueue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
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
