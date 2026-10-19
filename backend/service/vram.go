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

func (e *EngineV2) arbiter() *vramArbiter {
	if e.vram == nil {
		e.vram = &vramArbiter{}
	}
	return e.vram
}

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
