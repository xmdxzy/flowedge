package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"flowedge/backend/model"
)

// 旧实现 Enqueue 用 `sem <- struct{}{}`：并发满时调用方（HTTP handler）阻塞，
// 界面表现为「点出片转圈几分钟然后超时」，而任务其实还在排队。
// 这条钉住：Enqueue 必须立即返回，即使并发槽位已占满。
func TestJobQueue_EnqueueNeverBlocks(t *testing.T) {
	_, db := newTestQueue(t)
	q := NewJobQueue(db, 1) // 单卡默认并发 1，串行执行
	defer q.Close()

	release := make(chan struct{})
	q.Enqueue(model.Job{ID: 1}, func() { <-release }) // 占住唯一的并发槽位

	start := time.Now()
	q.Enqueue(model.Job{ID: 2}, func() {})
	q.Enqueue(model.Job{ID: 3}, func() {})
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Enqueue 阻塞了 %v：并发满时不能卡住调用方（HTTP 会 180s 超时）", elapsed)
	}
	if got := q.Pending(); got != 2 {
		t.Fatalf("排队数应为 2，实际 %d", got)
	}
	close(release)
	q.Wait()
}

// FIFO：先来的先跑（排队顺序就是受理顺序，不能倒序）。
func TestJobQueue_FIFOOrder(t *testing.T) {
	_, db := newTestQueue(t)
	q := NewJobQueue(db, 1) // 单卡默认并发 1，串行执行
	defer q.Close()

	var mu sync.Mutex
	var order []uint
	gate := make(chan struct{})
	for i := uint(1); i <= 3; i++ {
		id := i
		q.Enqueue(model.Job{ID: id}, func() {
			if id == 1 {
				<-gate
			}
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
		})
	}
	close(gate)
	q.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("执行顺序应为 1,2,3，实际 %v", order)
	}
}

// 并发上限必须真的生效（默认 1 = 单卡串行）。
func TestJobQueue_DefaultSerial(t *testing.T) {
	_, db := newTestQueue(t)
	q := NewJobQueue(db, 1) // 单卡默认并发 1，串行执行
	defer q.Close()

	var inFlight, peak int32
	var wg sync.WaitGroup
	wg.Add(4)
	for i := 0; i < 4; i++ {
		q.Enqueue(model.Job{ID: uint(i + 1)}, func() {
			defer wg.Done()
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
		})
	}
	wg.Wait()
	q.Wait()
	if peak > 1 {
		t.Fatalf("并发峰值 %d 超过上限", peak)
	}
}
