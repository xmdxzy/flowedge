package service

import (
	"fmt"
	"os/exec"
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
	Status     ModelStatus
	LoadedAt   time.Time
	LastUsedAt time.Time
	LoadCostMS int // 历史平均加载耗时
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
	mu                sync.Mutex
	listening         bool
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
		models: make(map[string]*ModelInstance),
	}
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

// queryGPU 现场执行 nvidia-smi（无锁：调用方自行决定锁策略）
func (s *GPUScheduler) queryGPU() (*GPUInfo, error) {
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=memory.total,memory.used,memory.free,utilization.gpu",
		"--format=csv,noheader,nounits")

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

// AcquireModel 请求加载模型
// 返回是否需要等待、是否成功
func (s *GPUScheduler) AcquireModel(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	model, exists := s.models[name]
	if !exists {
		return fmt.Errorf("model not registered: %s", name)
	}

	// 如果已经加载完成，直接返回
	if model.Status == ModelStatusReady {
		model.LastUsedAt = time.Now()
		return nil
	}

	// 检查显存是否足够
	gpuInfo, err := s.GetGPUInfo()
	if err != nil {
		return fmt.Errorf("cannot get GPU info: %w", err)
	}

	requiredMB := model.VramMB
	availableMB := gpuInfo.FreeMB

	// 如果显存不足，触发 LRU 卸载
	if availableMB < requiredMB {
		s.evictLRU(requiredMB - availableMB)

		// 重新检查
		gpuInfo, _ = s.GetGPUInfo()
		availableMB = gpuInfo.FreeMB
		if availableMB < requiredMB {
			return fmt.Errorf("insufficient VRAM: need %d MB, free %d MB", requiredMB, availableMB)
		}
	}

	// 模拟加载过程（实际由推理服务完成）
	model.Status = ModelStatusLoading
	model.LoadedAt = time.Now()
	model.LastUsedAt = time.Now()

	return nil
}

// ReleaseModel 释放模型（标记为可卸载）
func (s *GPUScheduler) ReleaseModel(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if model, exists := s.models[name]; exists {
		model.LastUsedAt = time.Now()
		// 不立即卸载，交给 LRU 策略
	}
}

// evictLRU 按 LRU 策略卸载模型，释放 neededMB 显存
func (s *GPUScheduler) evictLRU(neededMB int) {
	// 收集所有可卸载的模型（idle 或 ready 状态）
	type candidate struct {
		name     string
		lastUsed time.Time
		vramMB   int
	}

	var candidates []candidate
	for name, m := range s.models {
		if m.Status == ModelStatusReady || m.Status == ModelStatusIdle {
			candidates = append(candidates, candidate{
				name:     name,
				lastUsed: m.LastUsedAt,
				vramMB:   m.VramMB,
			})
		}
	}

	// 按最久未使用排序
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[i].lastUsed.After(candidates[j].lastUsed) {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	// 逐个卸载，直到释放足够显存
	freedMB := 0
	for _, c := range candidates {
		if freedMB >= neededMB {
			break
		}
		s.models[c.name].Status = ModelStatusIdle
		freedMB += c.vramMB
	}
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
