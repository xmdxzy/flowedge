package service

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// GPUInfo GPU 显存信息
type GPUInfo struct {
	TotalMB     int
	UsedMB      int
	FreeMB      int
	Utilization int
}

// ModelStatus 模型运行状态
type ModelStatus string

const (
	ModelStatusIdle      ModelStatus = "idle"
	ModelStatusLoading   ModelStatus = "loading"
	ModelStatusReady     ModelStatus = "ready"
	ModelStatusUnloading ModelStatus = "unloading"
)

// ModelInstance 模型实例
type ModelInstance struct {
	Name       string
	Type       string // llm, text2image, asr, tts
	VramMB     int    // 预估显存占用
	Refs       int    // 正在使用中的引用数；>0 的模型禁止卸载
	Status     ModelStatus
	LoadedAt   time.Time
	LastUsedAt time.Time
	LoadCostMS int // 历史平均加载耗时
}

// TimelineEvent 显存时间线事件（前端可视化：谁在什么时候占了显存）
type TimelineEvent struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"` // reserve / release / unload / reject
	Model  string    `json:"model,omitempty"`
	VRAMMB int       `json:"vram_mb,omitempty"`
	FreeMB int       `json:"free_mb,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// SchedulerConfig 调度器配置
type SchedulerConfig struct {
	TotalVramMB     int // 总显存
	SafeThresholdMB int // 安全线（低于此值才允许缓存）
	WarnThresholdMB int // 预警线（超过此值开始卸载）
	RedThresholdMB  int // 红线（强制卸载所有非活跃模型）
}

// GPUScheduler 显存调度器
type GPUScheduler struct {
	config            SchedulerConfig
	models            map[string]*ModelInstance
	unloader          func(name, modelType string) error // 真实卸载钩子（Ollama keep_alive:0 / ComfyUI /free）
	mu                sync.Mutex
	t2iGate           sync.Mutex // 文生图串行门：flux 单实例约 11GB，并行只会互相 OOM
	timeline          []TimelineEvent
	cachedGPU         *GPUInfo
	cachedAt          time.Time
	telemetryInterval time.Duration
}

func NewGPUScheduler(totalVramMB int) *GPUScheduler {
	if totalVramMB == 0 {
		totalVramMB = 24576 // 3090 默认 24GB
	}

	return &GPUScheduler{
		config: SchedulerConfig{
			TotalVramMB:     totalVramMB,
			SafeThresholdMB: int(float64(totalVramMB) * 0.7),  // 70% 安全线
			WarnThresholdMB: int(float64(totalVramMB) * 0.85), // 85% 预警线
			RedThresholdMB:  int(float64(totalVramMB) * 0.92), // 92% 红线
		},
		models:   make(map[string]*ModelInstance),
		timeline: make([]TimelineEvent, 0, 64),
	}
}

// SetUnloader 注入真实卸载函数。
//
// 在此之前，evictLRU 只把内存里的 Status 改成 idle，**模型仍留在显存里**
// —— 显存实际没有释放，下一次 Acquire 又会失败。真实卸载由推理服务自己完成
// （Ollama：发一次带 keep_alive:0 的请求；ComfyUI：调 /free），调度器只负责在
// 该卸的时候调用。
func (s *GPUScheduler) SetUnloader(fn func(name, modelType string) error) {
	s.mu.Lock()
	s.unloader = fn
	s.mu.Unlock()
}

// recordTimeline 追加时间线事件（最近 200 条，调用方持锁）
func (s *GPUScheduler) recordTimeline(ev TimelineEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	s.timeline = append(s.timeline, ev)
	if len(s.timeline) > 200 {
		s.timeline = s.timeline[len(s.timeline)-200:]
	}
}

// LockText2Image / UnlockText2Image 串行化文生图任务。
//
// 单卡 24GB 上并行跑两个 flux 并不会更快：两个 11GB 模型同时驻留会直接 OOM，
// 或者被驱动换页到内存里慢十倍。串行是这里唯一正确的策略。
// LLM 不在此列 —— 小模型可共存，靠 Acquire/Release + LRU 协调即可。
func (s *GPUScheduler) LockText2Image()   { s.t2iGate.Lock() }
func (s *GPUScheduler) UnlockText2Image() { s.t2iGate.Unlock() }

// VRAMBudgetMB 当前可用显存预算（总显存 - 已用 - 安全余量）
func (s *GPUScheduler) VRAMBudgetMB() int {
	info, err := s.GetGPUInfo()
	if err != nil || info == nil {
		return s.config.TotalVramMB / 2
	}
	budget := info.FreeMB - 1024 // 留 1GB 余量给 CUDA context / 显示输出
	if budget < 0 {
		budget = 0
	}
	return budget
}

// Timeline 返回显存时间线（最近 N 条）
func (s *GPUScheduler) Timeline(limit int) []TimelineEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	out := make([]TimelineEvent, 0, limit)
	n := len(s.timeline)
	start := 0
	if n > limit {
		start = n - limit
	}
	out = append(out, s.timeline[start:]...)
	return out
}

// StartTelemetry 启动后台 nvidia-smi 轮询，缓存真实遥测
// （避免每个请求/每次 Acquire 都拉子进程；拿不到 nvidia-smi 时静默降级）
func (s *GPUScheduler) StartTelemetry(interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	s.mu.Lock()
	if s.telemetryInterval > 0 {
		s.mu.Unlock()
		return // 已启动
	}
	s.telemetryInterval = interval
	s.mu.Unlock()

	go func() {
		refresh := func() {
			if info, err := s.queryGPU(); err == nil {
				s.mu.Lock()
				s.cachedGPU = info
				s.cachedAt = time.Now()
				s.mu.Unlock()
			}
		}
		refresh()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			refresh()
		}
	}()
}

// GetGPUInfo 获取当前GPU显存状态（优先用遥测缓存，过期则现场查询）
func (s *GPUScheduler) GetGPUInfo() (*GPUInfo, error) {
	s.mu.Lock()
	cached, cachedAt := s.cachedGPU, s.cachedAt
	interval := s.telemetryInterval
	s.mu.Unlock()

	if cached != nil && time.Since(cachedAt) < 2*interval {
		return cached, nil
	}

	info, err := s.queryGPU()
	if err != nil {
		if cached != nil {
			return cached, nil // 现场查询失败时退回缓存
		}
		return nil, err
	}

	s.mu.Lock()
	s.cachedGPU = info
	s.cachedAt = time.Now()
	s.mu.Unlock()
	return info, nil
}

// gpuInfoLocked 供**已持锁**的调用方读取 GPU 状态。
//
// AcquireModel 持有 s.mu 时不能调用 GetGPUInfo —— 后者内部也会 s.mu.Lock()，
// 而 sync.Mutex 不可重入，会直接自死锁。这个函数只读缓存 / 直接查 nvidia-smi。
func (s *GPUScheduler) gpuInfoLocked() (*GPUInfo, error) {
	interval := s.telemetryInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if s.cachedGPU != nil && time.Since(s.cachedAt) < 2*interval {
		return s.cachedGPU, nil
	}
	return s.queryGPU()
}

// queryGPU 现场执行 nvidia-smi（无锁：调用方自行决定锁策略）
//
// 必须走隐藏窗口：这个函数由 StartTelemetry 每 5 秒调一次，
// 而 nvidia-smi 是控制台程序。Windows 11 把「默认终端应用」指向
// Windows Terminal 后，任何未声明 CREATE_NO_WINDOW 的控制台子进程
// 都会被系统重新拉起一个 Windows Terminal 窗口——每 5 秒闪一次黑框。
func (s *GPUScheduler) queryGPU() (*GPUInfo, error) {
	cmd := hiddenExec("nvidia-smi",
		"--query-gpu=memory.total,memory.used,memory.free,utilization.gpu",
		"--format=csv,noheader,nounits")
	cmd.SysProcAttr = hiddenWindowAttr()

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}

	re := regexp.MustCompile(`(\d+),\s*(\d+),\s*(\d+),\s*(\d+)%?`)
	matches := re.FindStringSubmatch(string(output))
	if len(matches) < 5 {
		return nil, fmt.Errorf("parse nvidia-smi output failed")
	}

	total, _ := strconv.Atoi(matches[1])
	used, _ := strconv.Atoi(matches[2])
	free, _ := strconv.Atoi(matches[3])
	util, _ := strconv.Atoi(matches[4])

	return &GPUInfo{
		TotalMB:     total,
		UsedMB:      used,
		FreeMB:      free,
		Utilization: util,
	}, nil
}

// RegisterModel 注册模型
func (s *GPUScheduler) RegisterModel(name, modelType string, vramMB int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.models[name]; !exists {
		s.models[name] = &ModelInstance{
			Name:   name,
			Type:   modelType,
			VramMB: vramMB,
			Status: ModelStatusIdle,
		}
	}
}

// AcquireModel 请求加载模型（引用计数：同一模型被多个节点并发使用时只算一次，卸载时不会被误卸）
func (s *GPUScheduler) AcquireModel(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, exists := s.models[name]
	if !exists {
		return fmt.Errorf("model not registered: %s", name)
	}

	// 已加载：仅累加引用
	if m.Status == ModelStatusReady {
		m.Refs++
		m.LastUsedAt = time.Now()
		s.recordTimeline(TimelineEvent{Action: "reserve", Model: name, VRAMMB: m.VramMB})
		return nil
	}

	gpuInfo, err := s.gpuInfoLocked()
	if err != nil {
		// 遥测不可用（没装 nvidia-smi / 驱动异常 / 远程会话）：
		// **不阻断执行**，按「假定显存充足」继续并记入时间线。
		// 单机单卡场景下，宁可让它跑，也不要因为探测失败就让整个工作流失败。
		s.recordTimeline(TimelineEvent{
			Action: "telemetry_unavailable", Model: name, VRAMMB: m.VramMB, Detail: err.Error(),
		})
		m.Status = ModelStatusLoading
		m.Refs++
		m.LoadedAt = time.Now()
		m.LastUsedAt = time.Now()
		return nil
	}

	requiredMB := m.VramMB
	availableMB := gpuInfo.FreeMB

	if availableMB < requiredMB {
		s.evictLRULocked(requiredMB - availableMB)
		if gpuInfo, err = s.gpuInfoLocked(); err == nil {
			availableMB = gpuInfo.FreeMB
		}
		if availableMB < requiredMB {
			s.recordTimeline(TimelineEvent{
				Action: "reject", Model: name, VRAMMB: requiredMB, FreeMB: availableMB,
				Detail: fmt.Sprintf("显存不足：需要 %d MB，空闲 %d MB", requiredMB, availableMB),
			})
			return fmt.Errorf("insufficient VRAM: need %d MB, free %d MB", requiredMB, availableMB)
		}
	}

	m.Status = ModelStatusLoading
	m.Refs++
	m.LoadedAt = time.Now()
	m.LastUsedAt = time.Now()
	s.recordTimeline(TimelineEvent{Action: "reserve", Model: name, VRAMMB: m.VramMB, FreeMB: availableMB})
	return nil
}

// ReleaseModel 释放一个引用。引用归零后模型进入「可被 LRU 淘汰」状态，
// 但不立即卸载——缓存能加速后续节点重复使用。
func (s *GPUScheduler) ReleaseModel(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if m, exists := s.models[name]; exists {
		if m.Refs > 0 {
			m.Refs--
		}
		m.LastUsedAt = time.Now()
		s.recordTimeline(TimelineEvent{Action: "release", Model: name, VRAMMB: m.VramMB})
	}
}

// evictLRULocked 按 LRU 卸载模型直到释放 neededMB（调用方必须持锁）。
// 关键修正：
//  1. Refs > 0（正在被节点使用）的模型绝不卸载，否则会把在跑的节点一脚踢翻
//  2. 真正调用 unloader 释放显存，而不是只改内存里的 Status 字段
//  3. 卸载失败时保留状态并继续尝试下一个模型
func (s *GPUScheduler) evictLRULocked(neededMB int) {
	type candidate struct {
		name     string
		lastUsed time.Time
		vramMB   int
	}

	var candidates []candidate
	for name, m := range s.models {
		if m.Status != ModelStatusReady && m.Status != ModelStatusIdle {
			continue // loading / unloading 状态不碰
		}
		if m.Refs > 0 {
			continue // 正在使用，禁止卸载
		}
		candidates = append(candidates, candidate{name: name, lastUsed: m.LastUsedAt, vramMB: m.VramMB})
	}

	// 最久未使用优先
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[i].lastUsed.After(candidates[j].lastUsed) {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	freedMB := 0
	for _, c := range candidates {
		if freedMB >= neededMB {
			break
		}
		m := s.models[c.name]
		if s.unloader != nil {
			if err := s.unloader(c.name, m.Type); err != nil {
				s.recordTimeline(TimelineEvent{
					Action: "unload_failed", Model: c.name, VRAMMB: c.vramMB, Detail: err.Error(),
				})
				continue // 卸载失败：不能把 status 改成 idle，否则账实不符
			}
		}
		m.Status = ModelStatusIdle
		freedMB += c.vramMB
		s.recordTimeline(TimelineEvent{Action: "unload", Model: c.name, VRAMMB: c.vramMB})
	}
}

// evictLRU 供外部调用的无锁包装
func (s *GPUScheduler) evictLRU(neededMB int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLRULocked(neededMB)
}

// GetSchedulerStatus 获取调度器状态
func (s *GPUScheduler) GetSchedulerStatus() map[string]interface{} {
	// 先取 GPU 快照（GetGPUInfo 内部有锁），再拿模型锁——避免非重入死锁
	gpuInfo, err := s.GetGPUInfo()
	if err != nil {
		gpuInfo = &GPUInfo{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	modelsList := make([]map[string]interface{}, 0, len(s.models))
	for _, m := range s.models {
		modelsList = append(modelsList, map[string]interface{}{
			"name":      m.Name,
			"type":      m.Type,
			"vram_mb":   m.VramMB,
			"refs":      m.Refs,
			"status":    m.Status,
			"loaded_at": m.LoadedAt,
			"last_used": m.LastUsedAt,
		})
	}

	return map[string]interface{}{
		"gpu":    gpuInfo,
		"config": s.config,
		"models": modelsList,
	}
}
