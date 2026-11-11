package service

import (
	"log"
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
	// sideMusic ACE-Step 1.5（XL 权重约 9GB，运行时再叠加 VAE / LM）。
	//
	// 为什么单独开一侧而不是并进 sideAudio：TTS 在 GPU 上能驻留 15–20GB，
	// 与 9–12GB 的音乐侧在同一张 24GB 卡上装不下。并进 audio 的话
	// 「先配音、后出 BGM」这条最常见的顺序不会触发换侧 ⇒ 直接 OOM，
	// 而日志里两边都「正常」。分开两侧才逼出一次真实的卸载。
	sideMusic = "music"
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

// sideOrder 重任务组的执行顺序：扩散（最重、冷启动最贵）→ 语言 → 语音 → 音乐。
// 固定顺序而非按数量排序：避免每次运行顺序漂移，行为可预期才敢无人值守。
var sideOrder = []string{sideDiffusion, sideLanguage, sideAudio, sideMusic}

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
			log.Printf("[vram] 换侧 %s → %s：显存够，扩散侧不卸载（省一次 ~183s 冷加载）", a.lastSide, sideLanguage)
		} else if e.releaseDiffusionAndWait("进语言侧") {
			a.unloads++
		} else {
			log.Printf("[vram] 换侧 %s → %s：扩散侧本就没驻留，无需卸载", a.lastSide, sideLanguage)
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
			// 扩散要吃满显存：语言侧与语音侧都要让位。
			//
			// 语音侧必须先卸：Breeze TTS 2 实测驻留 15–20GB，不腾出来，
			// 下面那句「等 16GB 空闲」必然空转满 12 秒然后继续带病前进。
			e.releaseAudioSide("进扩散侧")
			// 音乐侧同理（ACE-Step XL 约 9GB）：它没有 /unload，只能停进程。
			e.releaseMusicSide("进扩散侧")
			// 关键：Ollama 的卸载返回 ≠ 显存归还（实测要几秒），不等等它，
			// 扩散模型一进来就是「卸载了但没释放」→ OOM。
			if e.llm != nil {
				n := e.llm.UnloadAllLoaded()
				if !e.waitVRAMFree(16000, 12*time.Second) {
					log.Printf("[vram] 语言侧卸载后 12s 内未回收 16GB（可能仍有其他进程占用），照常继续")
				}
				log.Printf("[vram] 换侧 %s → %s：已卸载 %d 个语言模型（Ollama 侧）", a.lastSide, side, n)
			}
		case sideLanguage:
			// 语言/视觉模型进显存前，先把 ComfyUI 里的扩散模型释放并等它真的归还
			if e.releaseDiffusionAndWait("进语言侧") {
				a.unloads++
			}
			// 语音侧同理：Ollama 14B（≈9GB）与 TTS 的 15GB 在 24G 卡上装不下，
			// 共存判断只覆盖了扩散↔语言这一对，语音这一侧没人管过。
			e.releaseAudioSide("进语言侧")
			e.releaseMusicSide("进语言侧")
		case sideAudio:
			// 语音两侧都让位（whisper large-v3 也要 3GB）
			if e.releaseDiffusionAndWait("进语音侧") {
				a.unloads++
			}
			if e.llm != nil {
				e.llm.UnloadAllLoaded()
			}
			e.releaseMusicSide("进语音侧")
		case sideMusic:
			// 音乐侧要 9–12GB：扩散、语言、语音三侧全部让位。
			// 顺序有讲究——先卸最贵的扩散（它有真实的 /free 与等待），
			// 再卸语言，最后卸语音，免得「卸了语音又去等 16GB 空闲」白等。
			if e.releaseDiffusionAndWait("进音乐侧") {
				a.unloads++
			}
			if e.llm != nil {
				e.llm.UnloadAllLoaded()
			}
			e.releaseAudioSide("进音乐侧")
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
func (e *EngineV2) releaseDiffusionAndWait(reason ...string) bool {
	if e.t2i == nil {
		return false
	}
	before, err := e.t2i.FreeVRAMMB()
	if err == nil && before > 16000 {
		return false // 本来就没驻留，免一次往返
	}
	_ = e.t2i.UnloadModels()
	deadline := time.Now().Add(12 * time.Second)
	after := 0
	for time.Now().Before(deadline) {
		time.Sleep(600 * time.Millisecond)
		if free, ferr := e.t2i.FreeVRAMMB(); ferr == nil {
			after = free
			if free > 16000 {
				break
			}
		}
	}
	// 每次**真实卸载**都留一行可读记录。
	//
	// 由来（2026-10-25）：此前这里不打日志，于是「占了 17GB 的扩散模型是谁在什么时候
	// 放掉的」完全无法从 engine.log 反推——今天为了确认一支片子的显存轨迹，
	// 只能靠现场重新量。代价是每次都要花 GPU 时间做实验（§0.12 ②）。
	// 带原因参数：调用方知道自己是「换侧」「降级重试」还是「OOM 兜底」。
	log.Printf("[vram] 释放扩散侧模型（%s）：空闲 %dMB → %dMB", strings.Join(reason, "/"), before, after)
	return true
}

// releaseAudioSide 让 TTS 把显存交出来（扩散侧 / 语言侧要用整张卡时）。
//
// 由来（2026-10-27 实测）：Breeze TTS 2（3.47B BF16）驻留占 15.2GB，冷加载后实测
// 峰值 19.9GB；而 H3 权重就要 20.5GB，两者物理上装不下。此前 enterSide 只管
// Ollama（语言侧）与 ComfyUI（扩散侧）这一对，语音侧**完全没人管**：
//
//	→ 配音跑过之后立刻出片，卡上只剩 3–4GB
//	→ H3 退化成部分加载（实测同镜 87s → 2269s）或直接 generation failed
//
// 优先级：
//  1. /unload —— 进程保留、下次合成自动重载权重，最轻；
//  2. 停服务 —— 后端不支持 /unload（Kokoro / Audio8 / SAPI）时的兜底；
//  3. 都不行就只留日志，不阻塞流程（显存预检那层还会拦）。
//
// CPU 上的 TTS 一个字节显存都不占，直接跳过——别为它无谓地惊动进程
// （判据与队列的 ttsOnCPU() 同一份配置，避免两处各拍一套）。
func (e *EngineV2) releaseAudioSide(reason string) {
	if e.tts == nil || ttsOnCPU() {
		return
	}
	if err := e.tts.Unload(); err == nil {
		log.Printf("[vram] 释放语音侧显存（%s）：TTS 模型已卸载，进程保留", reason)
		return
	} else if e.deps != nil && e.deps.Healthy("tts") {
		if stopErr := e.deps.Stop("tts"); stopErr == nil {
			log.Printf("[vram] 释放语音侧显存（%s）：/unload 不可用（%v），已停 TTS 服务", reason, err)
			return
		}
		log.Printf("[vram] 释放语音侧显存（%s）：/unload 与停服务均失败", reason)
		return
	}
	log.Printf("[vram] 释放语音侧显存（%s）：TTS 不可达，跳过", reason)
}

// releaseMusicSide 让音乐服务把显存交出来。
//
// ACE-Step **没有 /unload 端点**（只有每个任务结束后的 _empty_cache，
// 权重仍驻留），所以这里只有一条路：停托管进程。冷启动代价是重新载入
// 9GB XL 权重 + LM（本机实测 3–5 分钟），因此只在真的要换侧时才调——
// 不是「每次出片都顺手停一下」。
//
// 与 TTS 的区别：TTS 有 /unload（进程保留、只丢权重），音乐没有。
// 别为了「看起来一致」给 MusicService 造一个假 Unload —— 那正是
// Breeze TTS「假卸载」踩过的坑（AGENTS §0.16 那条 2026-10-28 记录）。
func (e *EngineV2) releaseMusicSide(reason string) {
	if e.music == nil || e.deps == nil {
		return
	}
	if !e.deps.Healthy("acestep") {
		return // 本来就没起，别为它惊动进程
	}
	if err := e.deps.Stop("acestep"); err != nil {
		log.Printf("[vram] 释放音乐侧显存（%s）：停服务失败：%v", reason, err)
		return
	}
	log.Printf("[vram] 释放音乐侧显存（%s）：已停 ACE-Step 进程（无 /unload，只能停进程）", reason)
}

// waitVRAMFree 等显存真的回来（目标 minFreeMB，最多 timeout）。
//
// 由来：/free 或 keep_alive:0 返回只代表「请求被接受」，CUDA 异步释放要几秒。
// 2026-10-21 实测：/free 返回后 5–10 秒才真正交还 17GB；
// 不等就进下一侧，会顶到 23.9GB/24GB 直接 OOM，而日志里一切「正常」。
//
// 拿不到 GPU 信息（无 nvidia-smi / 无卡）时返回 false 但**不阻塞**：
// 这类环境下等待没有意义，绝不能把它变成新的挂起点。
func (e *EngineV2) waitVRAMFree(minFreeMB int, timeout time.Duration) bool {
	if e.gpuSched == nil || minFreeMB <= 0 {
		return false
	}
	if info, err := e.gpuSched.GetGPUInfo(); err != nil || info == nil {
		return false
	} else if info.FreeMB >= minFreeMB {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if info, err := e.gpuSched.GetGPUInfo(); err == nil && info != nil && info.FreeMB >= minFreeMB {
			return true
		}
	}
	return false
}

// ResidentSide 当前驻留侧别（供能力面板/调试显示）
func (e *EngineV2) ResidentSide() string {
	a := e.arbiter()
	a.heavyMu.Lock()
	defer a.heavyMu.Unlock()
	return a.lastSide
}
