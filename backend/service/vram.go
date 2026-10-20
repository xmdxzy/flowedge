package service

import (
	"sync"
	"time"
)

// 显存仲裁：24GB 上「一次只让一侧的大模型驻留」
//
// 实测占用：
//
//	flux1-schnell-fp8  ≈ 17GB（常驻）
//	qwen2.5:14b q4     ≈  9GB
//	qwen2.5vl:7b       ≈  6GB
//	whisper large-v3   ≈  3GB
//
// 任意两个大的并存就超 24GB：轻则换页慢十倍，重则直接 OOM。
// 这里把「换侧才卸载」固化成规则：
//
//	heavyMu  串行化所有重任务（并行节点不再互相抢显存）
//	lastSide 记录当前驻留的是哪一侧；只有真正换侧才触发卸载，
//	         同一侧连续调用不重复卸载（避免 40 秒一次的 flux 冷加载）
type vramArbiter struct {
	heavyMu  sync.Mutex
	lastSide string // diffusion | language | audio | ""
}

// 侧别常量
const (
	sideDiffusion = "diffusion" // ComfyUI：flux / qwen-image / 视频
	sideLanguage  = "language"  // Ollama：LLM / VLM
	sideAudio     = "audio"     // faster-whisper / Kokoro
)

// arbiter 懒初始化显存仲裁器。
//
// 必须加锁：引擎同层节点是并行起飞的（见 ExecuteWithRunID 的 wg + goroutine），
// 于是「引擎起来后第一批重任务」会有多个 goroutine 同时走到这里。
// 无锁的 `if e.vram == nil { e.vram = ... }` 会让它们各自 new 一个仲裁器，
// 各锁各的——结果几个扩散节点同时进 ComfyUI，24GB 卡直接 OOM。
// 这个坑是 TestHeavyNodesSerializeOnVRAMArbiter 用并发度探针测出来的
// （表现为偶发失败：调度偶尔恰好错开就蒙混过关）。
func (e *EngineV2) arbiter() *vramArbiter {
	e.vramMu.Lock()
	defer e.vramMu.Unlock()
	if e.vram == nil {
		e.vram = &vramArbiter{}
	}
	return e.vram
}

// nodeSide 节点类型属于哪一侧显存；返回 "" 表示不吃显存（可自由并行）。
//
// 判定口径是「该节点执行时**第一个**进入的侧别」——决定它起飞时会不会引发换侧。
// 典型例子：text2video 先在语言侧做提示词优化，再进扩散侧生成。按"第一个"
// 归类到语言侧，它就能和同层的 LLM / VLM 节点凑成一批，只换一次侧；
// 若按最终侧别（扩散）归类，它会在扩散批次内部又来回切一次。
//
// 与节点执行器里的 enterSide(...) 调用一一对应，改节点时必须同步改这里，
// 否则调度器会把重任务误判成轻任务、按无限并发起飞（24GB 上直接 OOM）。
// TestNodeSideMappingRegression 会用源码扫描盯住这个对应关系。
func nodeSide(nodeType string) string {
	switch nodeType {
	// 扩散侧：ComfyUI（flux / qwen-image / wan / H3）
	case "text2image", "image2image", "inpaint", "upscale", "shotlist":
		return sideDiffusion
	// 语言侧：Ollama。text2video 归此（先优化再生成，见上）
	case "llm", "vision", "prompt_opt", "scene_grid", "select", "storyboard", "text2video":
		return sideLanguage
	// 语音侧：faster-whisper / Kokoro（CPU 为主，但与扩散权重抢内存带宽）
	case "asr", "tts":
		return sideAudio
	}
	return ""
}

// sideOrder 重任务组的执行顺序：扩散（最重、冷启动最贵）→ 语言 → 语音。
// 固定顺序而非按数量排序：避免每次运行顺序漂移，行为可预期才敢无人值守。
var sideOrder = []string{sideDiffusion, sideLanguage, sideAudio}

// enterSide 进入某一侧重任务：串行化 + 换侧时卸载另一侧
func (e *EngineV2) enterSide(side string) func() {
	a := e.arbiter()
	a.heavyMu.Lock()
	if a.lastSide != side {
		switch side {
		case sideDiffusion:
			// 扩散要吃满显存：语言侧全部让位
			if e.llm != nil {
				e.llm.UnloadAllLoaded()
			}
		case sideLanguage:
			// 语言/视觉模型进显存前，先把 ComfyUI 里的扩散模型释放并等它真的归还
			e.releaseDiffusionAndWait()
		case sideAudio:
			// 语音两侧都让位（whisper large-v3 也要 3GB）
			e.releaseDiffusionAndWait()
			if e.llm != nil {
				e.llm.UnloadAllLoaded()
			}
		}
		a.lastSide = side
	}
	return a.heavyMu.Unlock
}

// releaseDiffusionAndWait 让 ComfyUI 卸载扩散模型，并等显存真正回来。
//
// 实测：/free 返回后要 5–10 秒才把 17GB 交还（CUDA 异步释放）。
// 不等就会「卸载了但没释放」，紧接着 VLM 进来直接顶到 23.9GB/24GB。
// 这里最多等 12 秒，够就提前走；ComfyUI 没起或已空载时立即返回。
func (e *EngineV2) releaseDiffusionAndWait() {
	if e.t2i == nil {
		return
	}
	if free, err := e.t2i.FreeVRAMMB(); err == nil && free > 16000 {
		return // 本来就没驻留，免一次往返
	}
	_ = e.t2i.UnloadModels()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(600 * time.Millisecond)
		if free, err := e.t2i.FreeVRAMMB(); err == nil && free > 16000 {
			return
		}
	}
}

// ResidentSide 当前驻留侧别（供能力面板/调试显示）
func (e *EngineV2) ResidentSide() string {
	a := e.arbiter()
	a.heavyMu.Lock()
	defer a.heavyMu.Unlock()
	return a.lastSide
}
