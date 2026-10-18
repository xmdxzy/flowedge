package service

import (
	"sync"
	"time"
)

// NodeExecutionStats 节点执行统计
type NodeExecutionStats struct {
	NodeType      string        // 节点类型
	AvgDuration   time.Duration // 平均执行时长
	TotalRuns     int           // 总运行次数
	LastRunAt     time.Time     // 最后运行时间
}

// ModelLoadStats 模型加载统计
type ModelLoadStats struct {
	ModelName     string        // 模型名称
	AvgLoadTime   time.Duration // 平均加载耗时
	TotalLoads    int           // 总加载次数
}

// POPSConfig 算法配置
type POPSConfig struct {
	// 预加载触发阈值：剩余时间占比低于此值时触发预加载
	TriggerThreshold float64 // 默认 0.3 = 剩余30%时间时触发
}

// POPSScheduler 流水线重叠预加载调度器
// Pipeline Overlap Preloading Scheduling
type POPSScheduler struct {
	config     POPSConfig
	nodeStats  map[string]*NodeExecutionStats
	modelStats map[string]*ModelLoadStats
	mu         sync.RWMutex
}

func NewPOPSScheduler() *POPSScheduler {
	return &POPSScheduler{
		config: POPSConfig{
			TriggerThreshold: 0.3,
		},
		nodeStats:  make(map[string]*NodeExecutionStats),
		modelStats: make(map[string]*ModelLoadStats),
	}
}

// RecordNodeExecution 记录节点执行时间
func (s *POPSScheduler) RecordNodeExecution(nodeType string, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stat, exists := s.nodeStats[nodeType]
	if !exists {
		stat = &NodeExecutionStats{NodeType: nodeType}
		s.nodeStats[nodeType] = stat
	}

	// 滑动平均更新
	if stat.TotalRuns == 0 {
		stat.AvgDuration = duration
	} else {
		// 指数移动平均，权重 0.2
		stat.AvgDuration = time.Duration(float64(stat.AvgDuration)*0.8 + float64(duration)*0.2)
	}
	stat.TotalRuns++
	stat.LastRunAt = time.Now()
}

// RecordModelLoad 记录模型加载时间
func (s *POPSScheduler) RecordModelLoad(modelName string, loadTime time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stat, exists := s.modelStats[modelName]
	if !exists {
		stat = &ModelLoadStats{ModelName: modelName}
		s.modelStats[modelName] = stat
	}

	if stat.TotalLoads == 0 {
		stat.AvgLoadTime = loadTime
	} else {
		stat.AvgLoadTime = time.Duration(float64(stat.AvgLoadTime)*0.8 + float64(loadTime)*0.2)
	}
	stat.TotalLoads++
}

// ShouldPreload 判断是否应该预加载下一个节点的模型
// currentNodeType: 当前执行的节点类型
// nextNodeType: 下一个节点类型
// nextModelName: 下一个节点需要的模型
// elapsed: 当前节点已经执行的时间
// 返回：是否应该预加载，以及建议的预加载延迟
func (s *POPSScheduler) ShouldPreload(
	currentNodeType string,
	nextNodeType string,
	nextModelName string,
	elapsed time.Duration,
) (bool, time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 获取当前节点预估总时长
	currentStat, exists := s.nodeStats[currentNodeType]
	if !exists || currentStat.AvgDuration == 0 {
		// 没有历史数据，不预加载
		return false, 0
	}

	// 获取下一个模型的预估加载时间
	nextModelStat, exists := s.modelStats[nextModelName]
	if !exists || nextModelStat.AvgLoadTime == 0 {
		// 没有加载时间数据，用默认估计
		nextModelStat = &ModelLoadStats{AvgLoadTime: 5 * time.Second}
	}

	// 计算剩余执行时间
	totalEstimated := currentStat.AvgDuration
	remaining := totalEstimated - elapsed

	if remaining <= 0 {
		return true, 0
	}

	// 计算：如果现在开始预加载，能不能在当前节点执行完之前完成？
	// 预加载需要的时间 <= 剩余执行时间 → 可以重叠
	canOverlap := nextModelStat.AvgLoadTime <= remaining

	// 触发条件：剩余时间 < 阈值 * 总时间，或者加载时间 > 剩余时间的一半
	triggerRatio := float64(remaining) / float64(totalEstimated)
	shouldTrigger := triggerRatio < s.config.TriggerThreshold || canOverlap

	if shouldTrigger {
		// 计算最佳预加载开始时间：
		// 应该在当前节点执行完之前，刚好完成加载
		// 延迟 = 剩余时间 - 加载时间
		delay := remaining - nextModelStat.AvgLoadTime
		if delay < 0 {
			delay = 0
		}
		return true, delay
	}

	return false, 0
}

// GetStats 获取所有统计数据
func (s *POPSScheduler) GetStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodeStats := make([]map[string]interface{}, 0, len(s.nodeStats))
	for _, stat := range s.nodeStats {
		nodeStats = append(nodeStats, map[string]interface{}{
			"node_type":    stat.NodeType,
			"avg_duration_ms": stat.AvgDuration.Milliseconds(),
			"total_runs":   stat.TotalRuns,
		})
	}

	modelStats := make([]map[string]interface{}, 0, len(s.modelStats))
	for _, stat := range s.modelStats {
		modelStats = append(modelStats, map[string]interface{}{
			"model_name":    stat.ModelName,
			"avg_load_ms":   stat.AvgLoadTime.Milliseconds(),
			"total_loads":  stat.TotalLoads,
		})
	}

	return map[string]interface{}{
		"config":      s.config,
		"node_stats":  nodeStats,
		"model_stats": modelStats,
	}
}
