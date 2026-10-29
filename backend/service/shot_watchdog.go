package service

import (
	"log"
	"sync"
	"time"
)

// 单镜看门狗：跑到「明显不可能正常完成」时就地中止，而不是干等节点预算烧完。
//
// 由来（2026-10-26 现场）：run 211 第 1 镜 56 帧（基准 67s）跑到 13 分钟还没完，
// 当时只剩 699MB 空闲、GPU 利用率 100%——典型的换页跑法。
// 节点预算是 20.5 分钟，也就是说这 13 分钟里 GPU 一直在做**最后必定被丢弃**的功，
// 而「慢镜告警」（JudgeShotSpeed）只在**跑完之后**才说话，对永不返回的那一镜完全无效。
//
// 机制：起一个计时 goroutine，累计「真正在跑」的时间，超过 limit 就喊一声并中断。
//
// 2026-10-26 补（排队不算时间）：原来从**提交那一刻**开始计时，可是提交之后
// 可能先在 ComfyUI 队列里干等。run 224 就是这样被误杀的——看门狗 5m35s 触发时
// 那条 prompt 还在排队，压根没开始算图。现在 pending 期间 Pause()，
// 开始执行再 Resume()，排队多久都不计入生成时限。
//
// 阈值取 5 倍基准：健康区间实测 0.9–1.5 倍，换页/卡死是 11–27 倍，中间没有重叠。
// 拿不定时宁可让它跑（少赚一次中止），也不要误杀真实的长任务。
const shotAbortFactor = 5.0

// ShotGenerationLimit 单镜的看门狗上限（0 表示不设限）。
//
// 上限 = 该镜健康成本 × 5。健康成本按 帧数 × 分辨率 × 步数 算（见 shotCostSeconds），
// 不能只按帧数——768×448@20 步 的成本是 608×352@12 步的 2.5 倍，
// 一刀切会在高分辨率档上误杀真实任务。
func ShotGenerationLimit(frames, width, height, steps int) time.Duration {
	if frames <= 0 {
		return 0
	}
	base := time.Duration(shotCostSeconds(frames, width, height, steps) * float64(time.Second))
	return time.Duration(float64(base) * shotAbortFactor)
}

// shotWatchdog 一次生成过程的看门狗（只累计「在算」的时间）。
type shotWatchdog struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}

	mu        sync.Mutex
	aborted   bool
	paused    bool
	limit     time.Duration
	active    time.Duration // 累计在跑时长（不含排队/暂停）
	queueWait time.Duration // 累计排队时长（仅用于日志说明）
}

const watchdogTick = 100 * time.Millisecond

// armShotWatchdog 在累计「在跑」时间到点时调 onAbort（只调一次）。limit<=0 时不设防。
func armShotWatchdog(limit time.Duration, onAbort func()) *shotWatchdog {
	w := &shotWatchdog{stopCh: make(chan struct{}), done: make(chan struct{}), limit: limit}
	if limit <= 0 {
		close(w.done)
		return w
	}
	go func() {
		defer close(w.done)
		t := time.NewTicker(watchdogTick)
		defer t.Stop()
		for {
			select {
			case <-w.stopCh:
				return
			case <-t.C:
				w.mu.Lock()
				if w.paused {
					w.queueWait += watchdogTick
				} else {
					w.active += watchdogTick
				}
				fire := !w.aborted && w.active >= w.limit
				if fire {
					w.aborted = true
				}
				waited := w.queueWait
				w.mu.Unlock()
				if fire {
					if waited > 0 {
						log.Printf("[shotlist] 该镜排队等了 %s（不计入生成时限），实际生成超过 %s 仍未完成",
							waited.Round(time.Second), limit.Round(time.Second))
					}
					if onAbort != nil {
						onAbort()
					}
					return
				}
			}
		}
	}()
	return w
}

// Stop 生成已返回（无论成败）时调用：取消计时并等 goroutine 退出。
// 必须等：否则测试里会出现「goroutine 在测试结束后才 fire」。
func (w *shotWatchdog) Stop() {
	if w == nil {
		return
	}
	if w.limit <= 0 {
		return // 空操作版没有 goroutine 可等
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.done
}

// Pause 提交还在排队时调用：这段时间不该算进「生成太久」。
func (w *shotWatchdog) Pause() {
	if w == nil || w.limit <= 0 {
		return
	}
	w.mu.Lock()
	w.paused = true
	w.mu.Unlock()
}

// Resume 提交开始真正执行时调用。
func (w *shotWatchdog) Resume() {
	if w == nil || w.limit <= 0 {
		return
	}
	w.mu.Lock()
	w.paused = false
	w.mu.Unlock()
}

// Aborted 这次生成是否是被看门狗掐断的（用于把错误说清楚）。
func (w *shotWatchdog) Aborted() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.aborted
}

// logShotAborted 中止时的统一措辞：说清「是我们主动掐的」+「为什么」。
func logShotAborted(shotID, idx, total int, limit time.Duration) {
	log.Printf("[shotlist] 第 %d/%d 镜 shot=%d 已跑满 %s（%.0f 倍基准）仍未完成——"+
		"判定为换页/卡死，主动中止，不再等节点超时把预算烧完",
		idx, total, shotID, limit.Round(time.Second), shotAbortFactor)
}
