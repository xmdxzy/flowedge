package service

import (
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// JobClass 任务的资源类。
//
// 为什么需要分道（2026-10-27 复盘）：
//
//	原实现只有一个全局并发闸（默认 1）。单卡上这**对显卡**是对的——两条扩散任务
//	并行只会互相抢显存（实测 H3 跑片时并发一条文生图，后者直接撞 5m15s 超时）；
//	但它把「不吃显存的任务」也一起串了：voice 只跑 CPU 上的 TTS + ffmpeg
//	（`运行/data/deps.json` 的 tts.device=cpu，实测 RTF 慢但不碰显存），
//	却要排在一条 20 分钟的 film 后面。用户听一句话的代价是 20 分钟。
//
// 所以闸门按资源分，不按任务分：
//   - ClassGPU：吃扩散显存 → 单卡串行（这是显卡的物理约束，不是保守选择）
//   - ClassCPU：确定不碰显存的节点 → 与 GPU 道并行，自己的闸（默认 2）
//
// 判类必须保守：**认不出来的节点一律按 GPU 处理**（见 JobClassForNodes）。
// 判错的方向不对称——把 GPU 任务判成 CPU 会让两张卡上的活互抢显存，
// 把 CPU 任务判成 GPU 只是多等一会儿。
type JobClass int

const (
	// ClassGPU 吃扩散/语言侧显存的任务（image / video / film / shotlist）。
	ClassGPU JobClass = iota
	// ClassCPU 确定不碰显存的任务（voice：TTS 在 CPU 上跑）。
	ClassCPU
)

func (c JobClass) String() string {
	if c == ClassCPU {
		return "cpu"
	}
	return "gpu"
}

// cpuOnlyNodeTypes 「只吃 CPU」的节点白名单。
//
// 白名单而不是黑名单：新增节点默认走 GPU 道。历史教训（AGENTS §0.12 ④）：
// 默认放行的那一侧，出问题时才发现判反了——而这里判反的代价是抢显存。
//
// tts 不在这里：它到底吃不吃显存**取决于配置的后端**，不是节点类型决定的。
// 2026-10-27 两次反复正好说明这一点：
//   - Breeze TTS 2 写着 device=cpu，实际 fast streaming 是 CUDA-only（常驻 8–16GB），
//     放 CPU 道就会与 H3 抢卡；
//   - Kokoro-82M 真在 CPU 上跑（实测 RTF≈0.95、零显存），可以随便并行。
//
// 所以 tts 走单独判断：`ttsRunsOnCPU()` 为真才放 CPU 道（见 JobClassForNodes）。
var cpuOnlyNodeTypes = map[string]bool{
	"start": true,
	"end":   true,
	// code_video 是 headless 浏览器 + ffmpeg，纯 CPU、一个字节显存都不占；
	// 放 GPU 道会白等 H3 排完才渲染，与「并行省时间」的设计目的相反。
	// 渲染自身的并发由节点参数 workers + lemo 的 RENDER_SLOTS 控制。
	"code_video": true,
}

// ttsOnCPU 判断当前 TTS 后端是否真的跑在 CPU 上（可被测试替换）。
//
// 判据是**配置里的 device**，缺省按「不是 CPU」处理（保守：宁可让 voice 排 GPU 道）。
// 注意配置不等于事实——Breeze 那次配置写 cpu、实际 CUDA-only。
// 所以这条只能算「声明」，真正的兜底是 voice 跑起来后 OOM/超时会显式失败，
// 而不是悄悄抢显存（生成链路本来就有 OOM 降级与超时两条防线）。
var ttsOnCPU = func() bool {
	return strings.EqualFold(strings.TrimSpace(LoadDepsFile().TTS.Device), "cpu")
}

// JobClassForNodes 按节点类型判定任务资源类。
//
// 只有一个节点不在白名单里就整条任务判成 GPU：一条流水线里只要有任何一步碰显存，
// 它就得跟扩散任务排队。
func JobClassForNodes(nodes []model.NodeInstance) JobClass {
	if len(nodes) == 0 {
		// 空图不可执行；真出现了也按 GPU 处理（保守方向见上）
		return ClassGPU
	}
	ttsCPU := ttsOnCPU()
	for _, n := range nodes {
		if n.Type == "tts" {
			if ttsCPU {
				continue
			}
			return ClassGPU
		}
		if !cpuOnlyNodeTypes[n.Type] {
			return ClassGPU
		}
	}
	return ClassCPU
}

// JobQueue 持久化任务队列。
//
// 两条硬约束（都踩过）：
//  1. **Enqueue 绝不阻塞**。旧实现是 `sem <- struct{}{}`：信号量满时调用方
//     （HTTP handler）就卡住，界面表现为「点出片转圈 3 分钟后超时」——
//     而任务其实还在排队。现在改成入队 + 调度，调用方立即返回。
//  2. 默认 GPU 并发 1。这是单卡产品，两条出片并行只会互相抢显存：
//     实测 H3 跑片时并发一条文生图，后者直接撞 5m15s 节点超时。
//     多卡场景用 FLOWEDGE_QUEUE_CONCURRENCY 显式调大。
//
// 第三条（2026-10-27 补）：**排队就是排队**。任务落库后状态是 pending，
// 真正被调度起来那一刻才写 running。旧实现在 handler 里 Enqueue 之前就把
// job 置成 running，于是「在队列里等 20 分钟」和「正在烧卡」在面板上一模一样——
// 用户以为卡住了，而看门狗和 ETA 也在给一个没开始的任务算时间。
// 这和第 2.6 条（ComfyUI 内部把排队当成在算）是同一个错，只是高了一层。
type JobQueue struct {
	db *gorm.DB

	mu      sync.Mutex
	pending []queuedJob
	running map[JobClass]int
	max     map[JobClass]int
	closed  bool

	wg sync.WaitGroup
}

type queuedJob struct {
	job   model.Job
	class JobClass
	fn    func()
}

// QueueState 队列快照（运维/测试用；不暴露给用户界面也能看懂）。
type QueueState struct {
	PendingGPU int `json:"pending_gpu"`
	PendingCPU int `json:"pending_cpu"`
	RunningGPU int `json:"running_gpu"`
	RunningCPU int `json:"running_cpu"`
	MaxGPU     int `json:"max_gpu"`
	MaxCPU     int `json:"max_cpu"`
}

func NewJobQueue(db *gorm.DB, maxConcurrent int) *JobQueue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &JobQueue{
		db:      db,
		running: map[JobClass]int{},
		max:     map[JobClass]int{ClassGPU: maxConcurrent, ClassCPU: defaultCPUConcurrency},
	}
}

// defaultCPUConcurrency CPU 道默认并发。
//
// 取 2 而不是 1：CPU 道里是「TTS + 拼接」这类单线程为主的活，本机 18 核，
// 两条并行各自变慢但都结束；取 1 等于把它们又串回 GPU 道后面。
// 取 2 而不是更大：TTS 服务本身是单进程排队，堆太多只会把内存和磁盘 IO 顶满。
const defaultCPUConcurrency = 2

// SetCPUConcurrency 调整 CPU 道并发（<=0 时不改）。
func (q *JobQueue) SetCPUConcurrency(n int) {
	if n <= 0 {
		return
	}
	q.mu.Lock()
	q.max[ClassCPU] = n
	q.mu.Unlock()
	q.dispatch()
}

// Enqueue 受理任务并立即返回（不阻塞调用方），按 FIFO 顺序调度。
// 老签名保留：默认按 GPU 道入队（保守，不会误把抢显存的任务并行）。
func (q *JobQueue) Enqueue(job model.Job, fn func()) {
	q.EnqueueClass(job, ClassGPU, fn)
}

// EnqueueClass 按资源类入队。
func (q *JobQueue) EnqueueClass(job model.Job, class JobClass, fn func()) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		q.markDB(job.ID, map[string]interface{}{"status": "interrupted", "error": "引擎正在关闭，任务未执行"})
		return
	}
	q.pending = append(q.pending, queuedJob{job: job, class: class, fn: fn})
	q.mu.Unlock()
	q.dispatch()
}

// dispatch 在对应资源道有空位时把最早的候选拉起来（可重入：任务结束时再调一次）。
//
// 候选选择规则：**队首优先，但只在它的道有空位时**。
// 也就是说 GPU 道占满时，后面的 CPU 任务直接越过它开跑（分道隔离的效果），
// 而同类任务之间仍然严格 FIFO（不插队，避免短任务把长任务饿死）。
func (q *JobQueue) dispatch() {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return
		}
		idx := -1
		var cls JobClass
		for i, item := range q.pending {
			if q.running[item.class] < q.slotCap(item.class) {
				idx, cls = i, item.class
				break
			}
		}
		if idx < 0 {
			q.mu.Unlock()
			return
		}
		item := q.pending[idx]
		q.pending = append(q.pending[:idx:idx], q.pending[idx+1:]...)
		q.running[cls]++
		q.mu.Unlock()

		q.wg.Add(1)
		go q.runOne(item, cls)
	}
}

// slotCap 读取某条道的上限（调用方需持锁）。
func (q *JobQueue) slotCap(class JobClass) int {
	n := q.max[class]
	if n < 1 {
		n = 1
	}
	return n
}

// runOne 执行一条任务，并在**真正开始**的这一刻把状态写成 running。
//
// 顺序有讲究：先落库再执行。反过来的话，一条 3 秒的任务可能执行完了
// 才写 running，面板上会出现「pending → done」的跳变，读起来像没跑过。
func (q *JobQueue) runOne(item queuedJob, class JobClass) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Queue] job %d panic recovered: %v", item.job.ID, r)
			q.markDB(item.job.ID, map[string]interface{}{"status": "failed", "error": "panic in task"})
		}
		q.mu.Lock()
		q.running[class]--
		q.mu.Unlock()
		// 先补位再 Done：反过来的话，计数器会短暂归零 → Wait() 可能提前返回，
		// 而下一条任务紧接着 Add(1)，Go 会报 "WaitGroup is reused before previous Wait
		// has returned"（2026-10-27 实测确实踩到）。
		q.dispatch()
		q.wg.Done()
	}()

	q.markDB(item.job.ID, map[string]interface{}{"status": "running", "started_at": time.Now()})
	if item.class == ClassCPU {
		log.Printf("[Queue] 起跑（CPU 道，与 GPU 任务并行）：job=%d run=%d%s",
			item.job.ID, item.job.RunID, waitedFor(item.job))
	} else {
		log.Printf("[Queue] 起跑（GPU 道）：job=%d run=%d%s",
			item.job.ID, item.job.RunID, waitedFor(item.job))
	}
	item.fn()
}

// waitedFor 排队时长的日志片段（CreatedAt 为空时不编造一个天文数字）。
func waitedFor(job model.Job) string {
	if job.CreatedAt.IsZero() {
		return ""
	}
	return "，排队 " + time.Since(job.CreatedAt).Truncate(time.Second).String()
}

// markDB 改任务状态（db 为空时静默跳过，测试里允许不接库）。
func (q *JobQueue) markDB(jobID uint, fields map[string]interface{}) {
	if q.db == nil || jobID == 0 {
		return
	}
	q.db.Model(&model.Job{}).Where("id = ?", jobID).Updates(fields)
}

// Cancel 取消一条**还在排队**的任务，返回是否找到。
//
// 由来（2026-10-27）：`/runs/:id/cancel` 原来只认「正在执行」的运行
// （EngineV2.CancelRun 查 activeRuns），排在队列里的运行一律返回 409
// 「该运行已结束或未在进行中」——用户以为停了，它 20 分钟后照样开跑、
// 照样烧卡。和 §2.2「/interrupt 对排队中的 prompt 无效」是同一个错，
// 只是发生在上面一层：ComfyUI 的队列改了，我们自己的队列没改。
func (q *JobQueue) Cancel(runID uint) bool {
	if runID == 0 {
		return false
	}
	q.mu.Lock()
	found := -1
	var jobID uint
	for i, item := range q.pending {
		if item.job.RunID == runID {
			found, jobID = i, item.job.ID
			break
		}
	}
	if found >= 0 {
		q.pending = append(q.pending[:found:found], q.pending[found+1:]...)
	}
	q.mu.Unlock()
	if found < 0 {
		return false
	}
	q.markDB(jobID, map[string]interface{}{
		"status": "interrupted",
		"error":  "排队中被用户取消（未开始执行，未占用显存）",
	})
	log.Printf("[Queue] 已从队列里撤掉 job=%d run=%d（排队取消，不会在后台自己跑起来）", jobID, runID)
	return true
}

// HasGPUWork 是否有 GPU 道的活正在跑或在排队。
//
// 用来区分「显存被自家任务占着」和「显存被外来进程占着」：
// 前者应该排队等，后者才该在开机前拦住（见 handler/produce.go 的显存前瞻）。
func (q *JobQueue) HasGPUWork() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running[ClassGPU] > 0 {
		return true
	}
	for _, item := range q.pending {
		if item.class == ClassGPU {
			return true
		}
	}
	return false
}

// Snapshot 队列快照。
func (q *JobQueue) Snapshot() QueueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := QueueState{
		RunningGPU: q.running[ClassGPU],
		RunningCPU: q.running[ClassCPU],
		MaxGPU:     q.slotCap(ClassGPU),
		MaxCPU:     q.slotCap(ClassCPU),
	}
	for _, item := range q.pending {
		if item.class == ClassCPU {
			st.PendingCPU++
		} else {
			st.PendingGPU++
		}
	}
	return st
}

// RecoverInterrupted 启动时把上次进程留下的 running 任务标记为 interrupted。
// 这类任务的结果不可信（可能只跑了一半），用户需显式续跑。
//
// 2026-10-20 修复：原来只回收 jobs 表，漏了 workflow_runs 表——
// 进程被杀后 run 永远卡在 running（现存 3 条 10-19/10-20 的僵尸记录），
// 执行面板里既不能续跑也不能重跑，只能脏在列表里。两张表必须一起收。
//
// 2026-10-27 补：**pending 也要收**。队列只在内存里，进程一退，排在队里的
// 任务就再也不会被调度；留着 pending 会让面板永远显示「排队中」，
// 用户等一个不会发生的运行。
func (q *JobQueue) RecoverInterrupted() int64 {
	var total int64
	res := q.db.Model(&model.Job{}).Where("status = ?", "running").
		Updates(map[string]interface{}{"status": "interrupted", "error": "进程异常退出，任务被中断"})
	if res.Error == nil {
		total += res.RowsAffected
	}
	resPending := q.db.Model(&model.Job{}).Where("status = ?", "pending").
		Updates(map[string]interface{}{"status": "interrupted", "error": "进程重启，排队中的任务已失效（未开始执行）；可重新提交"})
	if resPending.Error == nil {
		total += resPending.RowsAffected
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
