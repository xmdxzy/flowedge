package service

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

func newTestQueue(t *testing.T) (*JobQueue, *gorm.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(&model.Job{}); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return NewJobQueue(db, 2), db
}

// TestJobQueue_EnqueueRunsTask 入队后任务必须真的执行
func TestJobQueue_EnqueueRunsTask(t *testing.T) {
	q, db := newTestQueue(t)

	job := model.Job{RunID: 1, Status: "running", Trigger: "manual"}
	db.Create(&job)

	var done int32
	q.Enqueue(job, func() {
		if db.Model(&model.Job{}).Where("id = ?", job.ID).Update("status", "done").Error != nil {
			t.Error("update job failed")
		}
		atomic.StoreInt32(&done, 1)
	})
	q.Wait()

	if atomic.LoadInt32(&done) != 1 {
		t.Fatal("enqueued task did not run")
	}
	var got model.Job
	db.First(&got, job.ID)
	if got.Status != "done" {
		t.Fatalf("job status got %q want done", got.Status)
	}
}

// TestJobQueue_ConcurrencyLimit 并发上限必须被信号量强制
func TestJobQueue_ConcurrencyLimit(t *testing.T) {
	q, _ := newTestQueue(t)

	var mu sync.Mutex
	inFlight, maxSeen := 0, 0
	for i := 0; i < 8; i++ {
		q.Enqueue(model.Job{ID: uint(i + 1), Status: "running"}, func() {
			mu.Lock()
			inFlight++
			if inFlight > maxSeen {
				maxSeen = inFlight
			}
			mu.Unlock()

			time.Sleep(20 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
		})
	}
	q.Wait()

	if maxSeen > 2 {
		t.Fatalf("concurrency exceeded limit: maxSeen=%d", maxSeen)
	}
	if maxSeen < 1 {
		t.Fatal("nothing ran")
	}
}

// TestJobQueue_RecoverInterrupted 启动时必须把上次残留的 running 标记为中断
func TestJobQueue_RecoverInterrupted(t *testing.T) {
	q, db := newTestQueue(t)

	db.Create(&model.Job{RunID: 1, Status: "running", Trigger: "manual"})
	db.Create(&model.Job{RunID: 2, Status: "running", Trigger: "cron"})
	db.Create(&model.Job{RunID: 3, Status: "done", Trigger: "manual"})

	n := q.RecoverInterrupted()
	if n != 2 {
		t.Fatalf("expected 2 interrupted jobs, got %d", n)
	}

	var pending []model.Job
	db.Where("status IN ?", []string{"running", "pending"}).Find(&pending)
	if len(pending) != 0 {
		t.Fatalf("no jobs should remain running/pending, got %d", len(pending))
	}

	var job model.Job
	db.Where("run_id = ?", 1).First(&job)
	if job.Status != "interrupted" || job.Error == "" {
		t.Fatalf("interrupted job must carry a reason, got status=%q err=%q", job.Status, job.Error)
	}
}

// TestJobQueue_PanicDoesNotBreakQueue 任务 panic 不得拖垮队列
func TestJobQueue_PanicDoesNotBreakQueue(t *testing.T) {
	q, _ := newTestQueue(t)

	var after int32
	q.Enqueue(model.Job{ID: 1, Status: "running"}, func() {
		panic("boom")
	})
	q.Enqueue(model.Job{ID: 2, Status: "running"}, func() {
		atomic.StoreInt32(&after, 1)
	})
	q.Wait()

	if atomic.LoadInt32(&after) != 1 {
		t.Fatal("subsequent task did not run after a panic")
	}
}

// TestJobQueue_CloseStopsAccepting 关闭后不再接收新任务
func TestJobQueue_CloseStopsAccepting(t *testing.T) {
	q, _ := newTestQueue(t)
	q.Close()

	var ran int32
	q.Enqueue(model.Job{ID: 1, Status: "running"}, func() {
		atomic.StoreInt32(&ran, 1)
	})
	q.Wait()
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatal("closed queue must not run new tasks")
	}
}

// TestJobQueue_ListRecent 记录查询与上限
func TestJobQueue_ListRecent(t *testing.T) {
	q, db := newTestQueue(t)
	for i := 1; i <= 5; i++ {
		db.Create(&model.Job{RunID: uint(i), Status: "done"})
	}
	jobs := q.ListRecent(3)
	if len(jobs) != 3 {
		t.Fatalf("expected 3, got %d", len(jobs))
	}
	if jobs[0].ID < jobs[1].ID {
		t.Fatalf("expected newest-first ordering, got %d < %d", jobs[0].ID, jobs[1].ID)
	}
}

var _ = fmt.Sprintf
