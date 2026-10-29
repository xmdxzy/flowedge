package service

import (
	"sync/atomic"
	"testing"
	"time"

	"flowedge/backend/model"
)

// waitFor 轮询直到条件成立（超时失败）。队列是异步的，断言前必须等状态落定。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// TestJobClassForNodes 判类必须保守：认不出来的节点一律 GPU。
// 判反的代价不对称——GPU 任务落到 CPU 道就是两条任务抢显存。
//
// tts 的归属**取决于配置的后端**（2026-10-27 两次反复的教训）：
//   - Kokoro-82M：真在 CPU 上跑（实测 RTF≈0.95、零显存）→ voice 进 CPU 道
//   - Breeze TTS 2：写着 device=cpu，实际 fast streaming CUDA-only、常驻 8–16GB → 必须 GPU 道
func TestJobClassForNodes(t *testing.T) {
	orig := ttsOnCPU
	defer func() { ttsOnCPU = orig }()

	voiceNodes := []model.NodeInstance{{Type: "start"}, {Type: "tts"}, {Type: "end"}}
	micNodes := []model.NodeInstance{{Type: "start"}, {Type: "shotlist", Data: map[string]interface{}{}}, {Type: "end"}}

	cases := []struct {
		name   string
		nodes  []model.NodeInstance
		ttsCPU bool
		want   JobClass
	}{
		{"Kokoro（device=cpu）→ voice 走 CPU 道", voiceNodes, true, ClassCPU},
		{"GPU TTS（Breeze/Audio8）→ voice 必须排 GPU 道", voiceNodes, false, ClassGPU},
		{"纯 start/end 才是 CPU 道", []model.NodeInstance{{Type: "start"}, {Type: "end"}}, false, ClassCPU},
		{"image 吃显存", []model.NodeInstance{{Type: "start"}, {Type: "text2image"}, {Type: "end"}}, true, ClassGPU},
		{"film 含 shotlist", micNodes, true, ClassGPU},
		{"混合：只要一步吃显存就整条走 GPU", []model.NodeInstance{{Type: "tts"}, {Type: "text2image"}}, true, ClassGPU},
		{"未知节点按 GPU", []model.NodeInstance{{Type: "some_new_node"}}, true, ClassGPU},
		{"空图按 GPU", nil, true, ClassGPU},
	}
	for _, c := range cases {
		ttsOnCPU = func() bool { return c.ttsCPU }
		if got := JobClassForNodes(c.nodes); got != c.want {
			t.Errorf("%s：JobClassForNodes = %s，want %s", c.name, got, c.want)
		}
	}
}

// TestCPULaneNotBlockedByGPUJob 分道的全部意义：GPU 道占满时，CPU 任务照样开跑。
// 由来：voice 的 TTS 已在 CPU 上跑（deps.json tts.device=cpu），
// 却因为全局闸=1 要排在一条 20 分钟的 film 后面。
func TestCPULaneNotBlockedByGPUJob(t *testing.T) {
	q, _ := newTestQueue(t)
	q.max[ClassGPU] = 1

	release := make(chan struct{})
	gpuDone := make(chan struct{})
	q.EnqueueClass(model.Job{RunID: 1}, ClassGPU, func() {
		<-release
		close(gpuDone)
	})
	waitFor(t, "GPU 道任务起跑", func() bool { return q.Snapshot().RunningGPU == 1 })

	cpuDone := make(chan struct{})
	q.EnqueueClass(model.Job{RunID: 2}, ClassCPU, func() { close(cpuDone) })

	select {
	case <-cpuDone:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("CPU 道任务被 GPU 道挡住了（分道没生效）")
	}
	close(release)
	<-gpuDone
	waitFor(t, "队列清空", func() bool { s := q.Snapshot(); return s.RunningGPU == 0 && s.RunningCPU == 0 })
}

// TestCPULaneHasOwnCap CPU 道也有上限，不会因为「不吃显存」就无限并发。
func TestCPULaneHasOwnCap(t *testing.T) {
	q, _ := newTestQueue(t)
	q.SetCPUConcurrency(1)

	release := make(chan struct{})
	q.EnqueueClass(model.Job{RunID: 1}, ClassCPU, func() { <-release })
	waitFor(t, "第一条 CPU 任务起跑", func() bool { return q.Snapshot().RunningCPU == 1 })

	var second int32
	q.EnqueueClass(model.Job{RunID: 2}, ClassCPU, func() { atomic.AddInt32(&second, 1) })
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&second) != 0 {
		t.Fatal("CPU 道上限 1 时第二条不该开跑")
	}
	close(release)
	waitFor(t, "第二条 CPU 任务接棒", func() bool { return atomic.LoadInt32(&second) == 1 })
}

// TestCancelPendingJob_Dequeues 排队中的运行必须能撤掉，且撤掉之后绝不再跑。
//
// 由来（2026-10-27）：/runs/:id/cancel 原来只认正在执行的那条，
// 排队中的直接 409；用户以为停了，它 20 分钟后照样开跑、照样烧卡。
func TestCancelPendingJob_Dequeues(t *testing.T) {
	q, db := newTestQueue(t)
	q.max[ClassGPU] = 1

	first := model.Job{RunID: 1, Status: "pending"}
	if err := db.Create(&first).Error; err != nil {
		t.Fatalf("创建工作失败：%v", err)
	}
	second := model.Job{RunID: 2, Status: "pending"}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("创建工作失败：%v", err)
	}

	release := make(chan struct{})
	q.EnqueueClass(first, ClassGPU, func() { <-release })
	waitFor(t, "第一条起跑", func() bool { return q.Snapshot().RunningGPU == 1 })

	var ran int32
	q.EnqueueClass(second, ClassGPU, func() { atomic.AddInt32(&ran, 1) })

	// 排队期间状态必须是 pending，不能冒充 running（面板/看门狗/ETA 都看这一列）
	var queued model.Job
	db.First(&queued, second.ID)
	if queued.Status != "pending" {
		t.Fatalf("排队中的任务状态应是 pending，实际 %q", queued.Status)
	}

	if !q.Cancel(2) {
		t.Fatal("排队中的运行应该能取消")
	}
	close(release)
	waitFor(t, "队列清空", func() bool { return q.Snapshot().RunningGPU == 0 })
	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("被取消的任务仍然执行了 %d 次", got)
	}

	db.First(&queued, second.ID)
	if queued.Status != "interrupted" {
		t.Fatalf("被取消的任务状态应为 interrupted，实际 %q", queued.Status)
	}
	if q.Cancel(2) {
		t.Fatal("已撤掉的任务再取消应返回 false（否则界面会一直以为在撤）")
	}
}

// TestCancelRunningJobReturnsFalse 正在执行的那条不走队列取消——
// 它要由 EngineV2.CancelRun 取消 ctx 并让 ComfyUI 停手，否则显存不解。
func TestCancelRunningJobReturnsFalse(t *testing.T) {
	q, _ := newTestQueue(t)
	q.max[ClassGPU] = 1
	release := make(chan struct{})
	defer close(release)
	q.EnqueueClass(model.Job{RunID: 7}, ClassGPU, func() { <-release })
	waitFor(t, "起跑", func() bool { return q.Snapshot().RunningGPU == 1 })
	if q.Cancel(7) {
		t.Fatal("正在执行的任务不该被队列取消（会漏掉停 ComfyUI 这一步）")
	}
}

// TestHasGPUWork 用来区分「显存被自家任务占着」和「被外来进程占着」。
func TestHasGPUWork(t *testing.T) {
	q, _ := newTestQueue(t)
	q.max[ClassGPU] = 1
	if q.HasGPUWork() {
		t.Fatal("空队列不该报有 GPU 活")
	}

	release := make(chan struct{})
	q.EnqueueClass(model.Job{RunID: 1}, ClassCPU, func() { <-release })
	waitFor(t, "CPU 任务起跑", func() bool { return q.Snapshot().RunningCPU == 1 })
	if q.HasGPUWork() {
		t.Fatal("只有 CPU 任务在跑时，不该认为 GPU 被自家占着")
	}

	q.EnqueueClass(model.Job{RunID: 2}, ClassGPU, func() {})
	waitFor(t, "GPU 任务起跑", func() bool { return q.Snapshot().RunningGPU == 1 })
	if !q.HasGPUWork() {
		t.Fatal("GPU 任务在跑却没报出来")
	}
	close(release)
}

// TestRecoverInterruptedClearsPending 进程重启后，队列里没跑的 pending 必须收掉：
// 队列只在内存里，它们再也不会被调度，留着就是「永远排队中」。
func TestRecoverInterruptedClearsPending(t *testing.T) {
	q, db := newTestQueue(t)
	j := model.Job{RunID: 9, Status: "pending"}
	db.Create(&j)
	if n := q.RecoverInterrupted(); n == 0 {
		t.Fatal("应该回收 pending 的僵尸任务")
	}
	var got model.Job
	db.First(&got, j.ID)
	if got.Status != "interrupted" {
		t.Fatalf("回收后应为 interrupted，实际 %q", got.Status)
	}
}
