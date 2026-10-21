package service

import (
	"strings"
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

	// switches 逻辑换侧次数（lastSide 非空且与新侧不同）。
	switches int
	// unloads 真实执行了卸载的次数。与 switches 分开统计：
	// 扩散侧本来就没驻留时，换侧是空操作、不掏冷加载的钱。
	// 只有 unloads 才对得上那 180 秒（实测冷加载 183s / 热生成 3s）。
	unloads int
	// coexists 因为「显存装得下」而免除的驱逐次数。
	// 每命中一次 ≈ 省下一次 183s 冷加载。这是共存优化的直接收益计数。
	coexists int
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
	if s, ok := NodeSpecs[nodeType]; ok {
		return s.Side
	}
	// 未登记的类型按"不吃显存"处理，与旧行为一致。
	// 会加载大模型的自定义节点必须用 RegisterNodeTypeWithSide 声明，不能指望这里猜。
	return ""
}

// sideOrder 重任务组的执行顺序：扩散（最重、冷启动最贵）→ 语言 → 语音。
// 固定顺序而非按数量排序：避免每次运行顺序漂移，行为可预期才敢无人值守。
var sideOrder = []string{sideDiffusion, sideLanguage, sideAudio}

const (
	// vramCoexistMargin 共存判断的安全余量。
	// 模型磁盘体积只是显存占用的下界（KV cache 随上下文长度增长），
	// 判断错的代价是把已经驻留的扩散模型挤出去 → 下次出图重新加载 183 秒，
	// 比多加载一次更糟。所以宁可多留，不可少留。
	vramCoexistMargin = 1.30
	// vramCoexistFloorMB 硬性留白：CUDA 上下文、显存碎片、桌面/其他进程占用
	vramCoexistFloorMB = 1024
)

// canCoexistWithLanguage 判断「能否不卸载扩散模型就让语言模型进来」。
//
// 由来（2026-10-21 实测）：卸载后重新加载 flux 要 183.3s，而生成只要 3.0s。
// 也就是说每次不必要的驱逐白烧约 180 秒。但 AGENTS §2「同一时刻只让一侧驻留」
// 是为 17GB 的 flux + 9GB 的 14B 准备的——**小模型其实装得下**
// （flux 17GB + VLM 6GB ≈ 23GB，24G 卡放得开），这类组合不该白白卸载。
//
// 判定纪律：任何一项信息拿不到就返回 false（保守卸载）。
// 「不确定」绝不等于「应该可以」——这条踩过太多次。
func (e *EngineV2) canCoexistWithLanguage(model string) bool {
	if e.vramCoexistOff || e.llm == nil || e.t2i == nil {
		return false
	}
	if strings.TrimSpace(model) == "" {
		model = e.llm.Model
	}
	sizeMB := e.llm.ModelSizeMB(model)
	if sizeMB <= 0 {
		return false // 查不到模型体积：不动
	}
	freeMB, err := e.t2i.FreeVRAMMB()
	if err != nil || freeMB <= 0 {
		return false // 查不到真实空闲显存：不动
	}
	need := int(float64(sizeMB)*vramCoexistMargin) + vramCoexistFloorMB
	return freeMB > need
}

// enterLanguageSide 进入语言侧；装得下就与扩散模型共存，不卸载。
//
// 与 enterSide(sideLanguage) 的唯一区别就是这一次判断：
//   - 装得下 → 不卸载（省一次 183s 冷加载），计 coexists
//   - 装不下 → 卸载（原行为），计 unloads
//   - 拿不到信息 → 按装不下处理（保守）
//
// model 为空时用服务默认模型。
func (e *EngineV2) enterLanguageSide(model string) func() {
	a := e.arbiter()
	a.heavyMu.Lock()
	if a.lastSide != "" && a.lastSide != sideLanguage {
		a.switches++
	}
	if a.lastSide != sideLanguage {
		if e.canCoexistWithLanguage(model) {
			a.coexists++ // 省下的一次驱逐，对应约 183s
		} else if e.releaseDiffusionAndWait() {
			a.unloads++
		}
		a.lastSide = sideLanguage
	}
	return a.heavyMu.Unlock
}

// enterSide 进入某一侧重任务：串行化 + 换侧时卸载另一侧
func (e *EngineV2) enterSide(side string) func() {
	a := e.arbiter()
	a.heavyMu.Lock()
	if a.lastSide != "" && a.lastSide != side {
		a.switches++ // 真正的换侧（有卸载成本）；从空进入第一侧不算
	}
	if a.lastSide != side {
		switch side {
		case sideDiffusion:
			// 扩散要吃满显存：语言侧全部让位
			if e.llm != nil {
				e.llm.UnloadAllLoaded()
			}
		case sideLanguage:
			// 语言/视觉模型进显存前，先把 ComfyUI 里的扩散模型释放并等它真的归还
			if e.releaseDiffusionAndWait() {
				a.unloads++
			}
		case sideAudio:
			// 语音两侧都让位（whisper large-v3 也要 3GB）
			if e.releaseDiffusionAndWait() {
				a.unloads++
			}
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
// 返回是否**真的**执行了卸载（本身就没驻留时是空操作）。
// 这个布尔值很关键：实测冷加载一张 512×512 要 183s、热生成只要 3s，
// 差 61 倍。所以「逻辑换侧」与「真实卸载」必须分开统计——
// 只有后者才真掏那 180 秒。
func (e *EngineV2) releaseDiffusionAndWait() bool {
	if e.t2i == nil {
		return false
	}
	if free, err := e.t2i.FreeVRAMMB(); err == nil && free > 16000 {
		return false // 本来就没驻留，免一次往返
	}
	_ = e.t2i.UnloadModels()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(600 * time.Millisecond)
		if free, err := e.t2i.FreeVRAMMB(); err == nil && free > 16000 {
			break
		}
	}
	return true
}

// ResidentSide 当前驻留侧别（供能力面板/调试显示）
func (e *EngineV2) ResidentSide() string {
	a := e.arbiter()
	a.heavyMu.Lock()
	defer a.heavyMu.Unlock()
	return a.lastSide
}
