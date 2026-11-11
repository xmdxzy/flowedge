package service

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ─── 空闲显存回收 ──────────────────────────────────────────────────────────
//
// 由来（2026-10-28）：用户的抱怨是「优化显存释放逻辑，无任务时释放我的显存，
// flowedge 白白占用了那么多」。查下来是**两个独立问题**叠在一起，只修任一个都不够：
//
//	① 卸载是假的
//	   POST /unload 返回 {"ok":true,"unloaded":true}，而 nvidia-smi 里该进程仍占
//	   16274 MiB，设备空闲只从 6737 缩到 6730 —— 等于没卸。
//	   根因在 tts_breeze_server.py：FastBreezeStreamingRuntime 与 model 循环引用
//	   （runtime.model → model，model._fast_text_encoder_cudagraph → …，
//	    runtime._backbone_graphs 还持有 CUDAGraph + 静态输入张量）。
//	   只把 _state 里的字段置 None，CPython 的引用计数归不了零，对象活着，
//	   PyTorch caching allocator 视这些块为 allocated 而非 cached，
//	   empty_cache() 无权动它们。
//	   修复见 _release_vram()：断环 → gc.collect()×2 → synchronize → empty_cache。
//	   修复后实测：allocated 8532 → 270 MB、设备空闲 6641 → 20973 MiB
//	   （回收 14.3GB，耗时 6.1s）；再触发一次合成，60s 内自动重载成功、音频正常。
//
//	② 没有统一调度
//	   TTS 服务自带 idle_watcher，但 deps.json 把 tts.idle_unload 设成 0 关掉了。
//	   当时的理由是「重载要 ~20.3GiB 空闲，UE 编辑器/浏览器常驻占 4–6GB 就装不回来，
//	   失败还会卡在 phase runtime 占着 16GB 不 ready」。这两个前提现在都变了：
//	     - 卸载真的还、加载失败也会回滚（load() 里失败即 _release_vram），
//	       不会再留死块；
//	     - 于是可以改成**有守卫的回收**：只在「卸完仍装得回来」时才卸。
//
// 为什么不直接把 tts.idle_unload 打开：
//   - 服务自己的超时只看自己的 last_used，看不见引擎队列里排队/在跑的活，
//     会在「下一个 shot 5 秒后就要配音」时把模型卸掉；
//   - 判据必须只有一个来源（AGENTS §0.16）：引擎才知道「真的没活」，
//     也才能把回收量、跳过原因、耗时写进同一份日志与事件流。
//
// 这一层只做一件事：**连续 N 秒没有任何 GPU 活 → 把托管依赖的权重还回显存**。
// 不杀进程（/unload 与 /free 都保留进程）：下次要用只付权重加载，不付进程启动。
// 实测重载 60s（有 inductor 缓存）vs 重启进程 137s，所以「卸了再装」并不亏。

// IdleReclaimPolicy 空闲回收策略（data/deps.json 的 idle_release 段）。
type IdleReclaimPolicy struct {
	// After 连续无 GPU 活多久后回收；<=0 表示关闭。
	After time.Duration
	// Targets 要回收的依赖名；空 = 默认（tts + comfy* + ollama）。
	Targets []string
	// TTSMinFreeMB 重载 TTS 所需的空闲显存下限（MiB）。
	// 来源：tts_breeze_server.py 的 _need = 20.3 GiB（快路径 CUDA Graph 峰值），
	// 可用环境变量 BREEZE_MIN_FREE_GB 覆盖 —— 两边改一处必须同步这一处。
	TTSMinFreeMB int
	// TTSGuard 为 true 时，只有「卸完的空闲仍 ≥ TTSMinFreeMB」才卸 TTS。
	//
	// 关掉它等于接受：为了腾地方，把配音弄成「下次必须先手动关掉别的程序才能用」。
	// 默认开 —— 「不确定能不能装回来」就当作装不回来，代价只是这次不卸。
	TTSGuard bool
}

const (
	// defaultIdleReclaimAfter 默认空闲阈值：10 分钟。
	//
	// 取 10 分钟而不是 1–2 分钟：一次出片动辄 6–20 分钟，镜与镜之间常有几十秒到
	// 几分钟的装配空档。阈值太短会在片子还没跑完时把权重卸掉，下一镜又要重载 ——
	// 省下的显存换来的是一次次 60s 冷加载，得不偿失。
	//
	// 取 10 分钟而不是永不：用户要的是「不开工的时候别占着我的卡」，
	// 10 分钟足以区分「正在跑片的间隙」和「今天用完了」。
	defaultIdleReclaimAfter = 10 * time.Minute

	// defaultTTSReloadMinFreeMB Breeze 快路径重载所需空闲显存（MiB）= 20.3 GiB。
	defaultTTSReloadMinFreeMB = 20787

	// idleReclaimPoll 检查间隔。30s 足够细（阈值是分钟级），又不至于太吵。
	idleReclaimPoll = 30 * time.Second
)

// DefaultIdleReclaimPolicy 默认策略：空闲 10 分钟回收，TTS 带「能装回来」守卫。
func DefaultIdleReclaimPolicy() IdleReclaimPolicy {
	return IdleReclaimPolicy{
		After:        defaultIdleReclaimAfter,
		TTSMinFreeMB: defaultTTSReloadMinFreeMB,
		TTSGuard:     true,
	}
}

// ReclaimResult 一次回收的结果。写日志、也能直接序列化给前端/调试看。
type ReclaimResult struct {
	At         time.Time `json:"at"`
	IdleForMS  int64     `json:"idle_for_ms"`
	FreeBefore int       `json:"free_before_mb"`
	FreeAfter  int       `json:"free_after_mb"`
	FreedMB    int       `json:"freed_mb"`
	Done       []string  `json:"done"`
	Skipped    []string  `json:"skipped"`
}

// IdleReclaimer 空闲显存回收器。
type IdleReclaimer struct {
	engine *EngineV2
	queue  *JobQueue
	policy IdleReclaimPolicy

	mu          sync.Mutex
	idleSince   time.Time
	lastAttempt time.Time // 上次真正动手的时刻（重试间隔按它算）；本轮空闲还没动过时为零值
	last        *ReclaimResult
}

// NewIdleReclaimer 构造回收器。queue 可以为 nil（只按 activeRuns 判定）。
//
// 注意 After 的零值语义是**关闭**，不是「用默认」：默认值由
// depsFile.IdleReclaimPolicy() 给出。这里不替调用方兜底，否则
// 「配置里明确写了 after_seconds=0」会被悄悄改回 10 分钟。
func NewIdleReclaimer(engine *EngineV2, queue *JobQueue, policy IdleReclaimPolicy) *IdleReclaimer {
	if policy.TTSMinFreeMB == 0 {
		policy.TTSMinFreeMB = defaultTTSReloadMinFreeMB
	}
	return &IdleReclaimer{engine: engine, queue: queue, policy: policy}
}

// Enabled 策略是否生效。
func (r *IdleReclaimer) Enabled() bool {
	return r != nil && r.policy.After > 0
}

// Policy 当前策略（供 /health 或面板展示）。
func (r *IdleReclaimer) Policy() IdleReclaimPolicy {
	if r == nil {
		return IdleReclaimPolicy{}
	}
	return r.policy
}

// LastResult 最近一次真正执行过的回收结果（没执行过为 nil）。
func (r *IdleReclaimer) LastResult() *ReclaimResult {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Run 启动后台循环，直到 stop 被关闭。
func (r *IdleReclaimer) Run(stop <-chan struct{}) {
	if !r.Enabled() {
		log.Printf("[idle-reclaim] 已关闭（idle_release.after_seconds<=0）")
		return
	}
	log.Printf("[idle-reclaim] 已开启：连续空闲 %s 后回收托管模型显存（守卫=%v，目标=%v）",
		r.policy.After, r.policy.TTSGuard, r.targetNames())
	t := time.NewTicker(idleReclaimPoll)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			r.Tick(now)
		}
	}
}

// Tick 推进一次判定。抽出来是为了让测试直接驱动，不必真等 10 分钟。
func (r *IdleReclaimer) Tick(now time.Time) {
	if !r.Enabled() {
		return
	}

	if r.busy() {
		r.mu.Lock()
		r.idleSince = time.Time{}
		r.lastAttempt = time.Time{}
		r.mu.Unlock()
		return
	}

	r.mu.Lock()
	if r.idleSince.IsZero() {
		r.idleSince = now
		r.mu.Unlock()
		return
	}
	waiting := now.Sub(r.idleSince)
	prevAttempt := r.lastAttempt
	r.mu.Unlock()

	if waiting < r.policy.After {
		return
	}

	// 重试间隔 = 空闲阈值（下限 1 分钟），避免每 30s poll 一次就发一轮请求。
	//
	// 为什么不是「一轮空闲只试一次」（旧实现，2026-11-06 修）：托管依赖完全可能
	// 在回收之后才被加载——面板上点启动、直接 curl /load、外部脚本起服务，这些
	// 都不经过引擎队列，busy() 看不见。旧的一次性闩锁只在 busy() 里复位，于是
	// 权重重新常驻之后再没人管。
	// 实测现场：10:57 回收一次（回收 0MB）→ 13:48 手动起 ACE-Step 7.3GB →
	// 15:30 手动起 Breeze TTS 11.6GB → 到 16:13 回收器一次都没再判定，卡上
	// 20.6GB 常驻，日志最后一笔还停在 10:57。
	retryGap := r.policy.After
	if retryGap < time.Minute {
		retryGap = time.Minute
	}
	if !prevAttempt.IsZero() && now.Sub(prevAttempt) < retryGap {
		return
	}

	// 先占位再动手：回收本身要几秒到十几秒（/unload 6s、/free 最多等 12s），
	// 不占位的话下一轮 tick 会重复发一遍请求。
	r.mu.Lock()
	r.lastAttempt = now
	r.mu.Unlock()

	res := r.reclaim()
	res.IdleForMS = waiting.Milliseconds()

	r.mu.Lock()
	r.last = &res
	r.mu.Unlock()

	r.report(res)
}

// report 把结果写成一行可 grep 的日志。
func (r *IdleReclaimer) report(res ReclaimResult) {
	if len(res.Done) == 0 {
		log.Printf("[idle-reclaim] 空闲 %s，本次未回收：%s",
			time.Duration(res.IdleForMS)*time.Millisecond, strings.Join(res.Skipped, "；"))
		return
	}
	log.Printf("[idle-reclaim] 空闲 %s，已回收 %d MB 显存（空闲 %d → %d MB）：%s",
		time.Duration(res.IdleForMS)*time.Millisecond, res.FreedMB,
		res.FreeBefore, res.FreeAfter, strings.Join(res.Done, "；"))
}

// reclaim 执行一次回收。绝不返回 error：单项失败要能继续做下一项，
// 且失败原因必须留下来（§0.6：不许把「做不到」静默成「做完了」）。
func (r *IdleReclaimer) reclaim() ReclaimResult {
	res := ReclaimResult{At: time.Now()}

	// 复核一次：从「判定空闲」到「真的动手」之间可能刚好有任务进来。
	// 这里多花一次判断，换的是不打断正在起跑的片子。
	if r.busy() {
		res.Skipped = append(res.Skipped, "回收前复核发现已有 GPU 任务，取消本次")
		return res
	}

	res.FreeBefore = r.deviceFreeMB()
	r.reclaimTTS(&res)
	r.reclaimDeps(&res)
	res.FreeAfter = r.deviceFreeMB()
	if res.FreedMB = res.FreeAfter - res.FreeBefore; res.FreedMB < 0 {
		res.FreedMB = 0 // 期间有外来进程占卡时读数是负的，别报「回收了 -300MB」
	}
	return res
}

// reclaimTTS 处理语音侧（两个后端里最占显存的那个）。
func (r *IdleReclaimer) reclaimTTS(res *ReclaimResult) {
	if !r.wants("tts") || r.engine == nil || r.engine.tts == nil {
		return
	}
	if ttsOnCPU() {
		return // CPU 上的 TTS 一个字节显存都不占（判据与队列同源）
	}

	info, err := r.engine.tts.VRAMInfo()
	if err != nil {
		res.Skipped = append(res.Skipped, "tts：读不到显存读数（"+err.Error()+"），按不知道处理，不卸")
		return
	}
	if info.ReservedMB <= 0 && info.AllocatedMB <= 0 {
		res.Skipped = append(res.Skipped, "tts：模型未加载，无需释放")
		return
	}

	resident := info.ReservedMB
	if resident < info.AllocatedMB {
		resident = info.AllocatedMB
	}
	if r.policy.TTSGuard && r.policy.TTSMinFreeMB > 0 {
		freeAfter := res.FreeBefore + resident
		if freeAfter < r.policy.TTSMinFreeMB {
			res.Skipped = append(res.Skipped, fmt.Sprintf(
				"tts：卸完约 %d MB 空闲 < 重载所需 %d MB，装不回来就不卸"+
					"（确要硬卸把 idle_release.tts_guard 设 false）",
				freeAfter, r.policy.TTSMinFreeMB))
			return
		}
	}

	if err := r.engine.tts.Unload(); err != nil {
		res.Skipped = append(res.Skipped, "tts：卸载请求失败（"+err.Error()+"）")
		return
	}
	res.Done = append(res.Done, fmt.Sprintf("tts 驻留 %d MB 已还", resident))
}

// reclaimDeps 处理扩散侧（ComfyUI 各实例）与语言侧（Ollama）。
// ASR 不在这里：它是 CPU/小占用，且没有卸载接口。
func (r *IdleReclaimer) reclaimDeps(res *ReclaimResult) {
	if r.engine == nil || r.engine.deps == nil {
		return
	}
	for _, st := range r.engine.deps.Status() {
		if st.Name == "tts" || !r.wants(st.Name) || !st.Running {
			continue
		}
		ur, err := r.engine.deps.Unload(st.Name)
		switch {
		case err != nil:
			res.Skipped = append(res.Skipped, st.Name+"："+err.Error())
		case !ur.Supported:
			res.Skipped = append(res.Skipped, st.Name+"：无卸载接口")
		default:
			res.Done = append(res.Done, fmt.Sprintf("%s %s", st.Name, ur.Note))
		}
	}
}

// wants 该依赖是否在回收目标里。目标为空时走默认集合。
func (r *IdleReclaimer) wants(name string) bool {
	if len(r.policy.Targets) == 0 {
		return name == "tts" || name == "ollama" || strings.HasPrefix(name, "comfy")
	}
	for _, t := range r.policy.Targets {
		if t == name {
			return true
		}
	}
	return false
}

func (r *IdleReclaimer) targetNames() []string {
	if len(r.policy.Targets) > 0 {
		return r.policy.Targets
	}
	return []string{"tts", "comfy*", "ollama"}
}

// busy 是否有 GPU 活：队列里有排队/在跑的，或引擎里有活着的 run。
//
// 两个判据都要看：队列覆盖 /api/produce 这条主路径，activeRuns 覆盖
// 直接调用引擎（测试、重跑、事件回放）的路径。只看一个会漏。
func (r *IdleReclaimer) busy() bool {
	if r.queue != nil && r.queue.HasGPUWork() {
		return true
	}
	if r.engine != nil && r.engine.ActiveRunCount() > 0 {
		return true
	}
	return false
}

// deviceFreeMB 设备级空闲显存（MiB）。拿不到返回 0 —— 调用方应按「不知道」处理。
//
// 走 queryGPU 而不是 GetGPUInfo：后者有 5s 遥测缓存（2*interval 内直接返回旧值），
// 而这里刚卸完就要读，拿到旧读数会把「回收了 14.3GB」记成「回收了 7MB」。
func (r *IdleReclaimer) deviceFreeMB() int {
	if r.engine == nil || r.engine.gpuSched == nil {
		return 0
	}
	info, err := r.engine.gpuSched.queryGPU()
	if err != nil || info == nil {
		return 0
	}
	return info.FreeMB
}

// ActiveRunCount 当前正在执行的 run 数。
func (e *EngineV2) ActiveRunCount() int {
	if e == nil {
		return 0
	}
	e.runMu.Lock()
	defer e.runMu.Unlock()
	return len(e.activeRuns)
}
