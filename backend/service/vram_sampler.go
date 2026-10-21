package service

import (
	"sync/atomic"
	"time"

	"flowedge/backend/model"
)

// 节点级显存峰值采样
//
// 为什么不用全局 telemetry：默认 5 秒一轮，而一次出图只要 3 秒——
// 采不到峰值。局部采样只在重节点执行窗口内开，1 秒一轮，成本可控；
// 节点结束立刻停，不在空闲时烧 CPU。
//
// 承认局限：节点短于采样间隔时，拿到的只是下界。这种情况用
// source="model_size"（按模型体积估算）标记出来，而不是假装是实测峰值。
const vramSampleInterval = time.Second

type vramSampler struct {
	gpu     *GPUScheduler
	stop    chan struct{}
	done    chan struct{}
	peakMB  atomic.Int64
	samples atomic.Int64
	started time.Time
}

// newVRAMSampler 启动采样。gpu 为 nil（未配 GPU 调度器）时返回一个空采样器，
// Stop() 立刻返回 0——调用方不需要自己判空。
func newVRAMSampler(gpu *GPUScheduler) *vramSampler {
	s := &vramSampler{gpu: gpu, stop: make(chan struct{}), done: make(chan struct{}), started: time.Now()}
	if gpu == nil {
		close(s.done)
		return s
	}
	go s.loop()
	return s
}

func (s *vramSampler) loop() {
	defer close(s.done)
	ticker := time.NewTicker(vramSampleInterval)
	defer ticker.Stop()
	// 先立刻采一次：3 秒的节点只能采到 3 次，不能浪费第一次
	s.sampleOnce()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.sampleOnce()
		}
	}
}

func (s *vramSampler) sampleOnce() {
	info, err := s.gpu.queryGPU()
	if err != nil || info == nil {
		return
	}
	s.samples.Add(1)
	for {
		cur := s.peakMB.Load()
		if int64(info.UsedMB) <= cur {
			return
		}
		if s.peakMB.CompareAndSwap(cur, int64(info.UsedMB)) {
			return
		}
	}
}

// Stop 停止采样并返回（峰值 MB, 采样次数）
func (s *vramSampler) Stop() (int, int) {
	if s.gpu != nil {
		select {
		case <-s.stop:
		default:
			close(s.stop)
		}
		<-s.done
	}
	return int(s.peakMB.Load()), int(s.samples.Load())
}

// profileKindForNode 哪些节点类型值得记档案（其余节点显存占用小或参数不可比）
//
// shotlist 故意不记：它在内部循环多镜，单条记录的"参数"没有意义，
// 记下来只会污染档案。
func profileKindForNode(nodeType string) string {
	switch nodeType {
	case "text2image":
		return "image"
	case "image2image":
		return "img2img"
	case "inpaint":
		return "inpaint"
	case "upscale":
		return "upscale"
	case "text2video":
		return "video"
	}
	return ""
}

// isHeavyNode 需要局部采样的节点（显存密集，且执行时长通常 > 1s）
func isHeavyNode(nodeType string) bool {
	s, ok := NodeSpecs[nodeType]
	return ok && s.Heavy
}

// recordProfile 把一次实测写进档案。
//
// peakMB <= 0 表示采样没拿到（节点太短 / nvidia-smi 不可用）：
// 此时退回"模型体积估算"，并标记 source=model_size —— 档案里必须能区分
// 「测到的」和「算出来的」，否则预测会带着假精确度到处跑。
func (e *EngineV2) recordProfile(node *model.NodeInstance, wall time.Duration, peakMB, samples int) {
	if e == nil || e.profiles == nil {
		return
	}
	kind := profileKindForNode(node.Type)
	if kind == "" {
		return
	}
	p := VRAMProfile{
		Kind:   kind,
		Width:  dataInt(node.Data, "width", 0),
		Height: dataInt(node.Data, "height", 0),
		Frames: dataInt(node.Data, "frames", 0),
		Steps:  dataInt(node.Data, "steps", 0),
		WallMS: wall.Milliseconds(),
		Model:  e.modelForKind(node.Type),
	}
	if peakMB > 0 && samples > 0 {
		p.PeakMB = peakMB
		p.Source = "sampled"
	} else {
		p.PeakMB = e.modelSizeEstimateMB(node.Type)
		p.Source = "model_size"
	}
	if p.PeakMB <= 0 {
		return
	}
	e.profiles.Record(p)
	// 每次写盘是为了崩溃不丢档案；写失败不影响执行（档案是辅助信息）
	_ = e.profiles.Save()
}

// modelForKind 取该节点实际使用的权重名（用于档案分档）
func (e *EngineV2) modelForKind(nodeType string) string {
	if e.t2i == nil {
		return ""
	}
	switch nodeType {
	case "text2image":
		return e.t2i.bundleFile(RoleT2IPrimary, "ckpt")
	case "image2image":
		return e.t2i.bundleFile(RoleImg2Img, "ckpt")
	case "inpaint":
		return e.t2i.bundleFile(RoleInpaint, "ckpt")
	case "upscale":
		return e.t2i.bundleFile(RoleUpscale, "model")
	case "text2video":
		return e.t2i.bundleFile(RoleVideoWan, "unet")
	}
	return ""
}

// modelSizeEstimateMB 拿不到采样时的估算（只有量级意义）
func (e *EngineV2) modelSizeEstimateMB(nodeType string) int {
	switch nodeType {
	case "text2image", "image2image", "inpaint":
		return 17000 // flux fp8 常驻约 17GB（实测首图 183s 含加载）
	case "upscale":
		return 4000
	case "text2video":
		return 12000
	}
	return 0
}
