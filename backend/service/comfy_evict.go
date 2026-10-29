package service

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// 跨实例腾显存：同一张卡上，A 实例驻留的模型会挡住 B 实例进场。
//
// 由来（2026-10-25 实测）：一次 image_edit 跑完 48 分钟后，主实例 8188 仍驻留
// 17.1GB（flux fp8），而 H3 要 ~21.8GB。17.1 + 21.8 = 38.9GB > 24.5GB，
// 下一次视频任务必然 OOM —— 而 OOM 之后 oom_degrade.go 会「降一档重试」，
// 于是用户拿到的是一支分辨率更低、帧数更少的片子，根因却是**另一个实例没交还显存**。
//
// 既有三套机制各缺一格，缺口正好落在「扩散侧内部换实例」上：
//   - enterSide(sideDiffusion) 只驱语言侧（Ollama）；扩散→扩散什么都不做
//   - LockForBase 只排队（mutex）：它保证「不同时跑」，不保证「不同时驻留」；
//     而且 main 实例根本不在 gpu-exclusive 组里，8188↔8189 连排队都没有
//   - oom_degrade.go 只有事后降质量，没有事前腾地方
//
// 这里的规则只有一句：**提交前探一次设备级空闲显存，不够就把别的实例腾空**。
// 只在不够时动手 ⇒ 同实例连续跑（image→image、shot→shot）不会白丢热权重，
// 那 183 秒冷加载只有真的换实例时才付一次。
//
// 顺序也很关键：先过预检、再腾地方。图本身就是坏的（模型名写错/节点没装）时
// 不该付腾显存的代价，更不能因为一次 400 就把别人的热模型赶走。

const (
	// vramEvictThresholdMB 触发腾显存的空闲显存下限（21GB）。
	//
	// 为什么取 21GB 而不是「16GB」或「目标模型的实测峰值」：
	//   - 本机最大的单模型是 H3，实测峰值 21.8GB。空闲 < 21GB 就说明卡上
	//     有别的大模型驻留，此时不管目标是谁都该先腾地方；
	//   - 空闲 ≥ 21GB 时卡上只剩桌面/上下文这类小占用，任何实例都装得下，
	//     不必打扰其它实例（也就不会误杀别人的热权重）。
	// 阈值故意偏高：判断偏保守的代价只是两次空转的 /free（毫秒级），
	// 判断偏乐观的代价是一次 6–40 分钟任务 OOM。两者不对等。
	vramEvictThresholdMB = 21504
	// vramEvictWait 腾完之后等显存真回来的上限。
	// 实测 /free 返回后要 5–10 秒 CUDA 才把 17GB 交还（AGENTS §2）。
	// 不等就提交 = 「卸载了但没释放」→ 目标模型进场直接顶爆。
	vramEvictWait = 12 * time.Second
)

// vramEvictOff 逃生阀：置 true 退回「各实例各管各的」旧行为。
// 放在包级而非引擎字段，是因为收口点在 Text2ImageService（RunGraphAt），
// 那里没有 engine 引用；而且这是纯运维开关，不需要每引擎一份。
var vramEvictOff atomic.Bool

// SetVRAMEvict 开关「提交前跨实例腾显存」（默认开启）。
//
// 什么时候该关：同一张卡上你自己安排了别的进程长期占显存（比如另开的
// 推理服务），不希望引擎去 /free 打扰它。关掉后换实例就可能 OOM，
// 由 oom_degrade 兜底降级。
func SetVRAMEvict(enabled bool) { vramEvictOff.Store(!enabled) }

// VRAMEvictEnabled 当前是否启用（供面板/测试读取）。
func VRAMEvictEnabled() bool { return !vramEvictOff.Load() }

// probeVRAMFreeMB 读某实例报告的**设备级**空闲显存（MB）。
//
// 用设备级而不是进程级：ComfyUI 的 vram_free 是整卡可用量，
// 别的实例占掉的部分在这里就体现成「少了」，这正是我们要看的信号。
func probeVRAMFreeMB(base string) (int, error) {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(strings.TrimRight(base, "/") + "/system_stats")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, io.ErrUnexpectedEOF
	}
	var st struct {
		Devices []struct {
			VRAMFree int64 `json:"vram_free"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return 0, err
	}
	if len(st.Devices) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	return int(st.Devices[0].VRAMFree / (1 << 20)), nil
}

// probeVRAMFreeAndTotalMB 一次拿到（空闲, 总量），用于判断"模型是否已经驻留"。
//
// 与 probeVRAMFreeMB 走同一个端点，只多读一个字段；单独写一个而不是改签名，
// 是为了不让既有调用方被迫接受新参数。
// 依据见 vram_advisory.go 的 AlreadyResidentForNodes（热实例被误判那一条）。
func probeVRAMFreeAndTotalMB(base string) (freeMB, totalMB int, err error) {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(strings.TrimRight(base, "/") + "/system_stats")
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, io.ErrUnexpectedEOF
	}
	var st struct {
		Devices []struct {
			VRAMFree  int64 `json:"vram_free"`
			VRAMTotal int64 `json:"vram_total"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return 0, 0, err
	}
	if len(st.Devices) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	return int(st.Devices[0].VRAMFree / (1 << 20)), int(st.Devices[0].VRAMTotal / (1 << 20)), nil
}

// postFree 让某实例释放已加载模型（空 body 的 /free 不归还显存，必须带两个 true）。
func postFree(base string) error {
	cl := &http.Client{Timeout: 10 * time.Second}
	payload, _ := json.Marshal(map[string]bool{"unload_models": true, "free_memory": true})
	resp, err := cl.Post(strings.TrimRight(base, "/")+"/free", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// evictSiblingsFor 提交到 base 之前，把同一张卡上其它实例的模型腾掉。
//
// 返回 (释放了哪些实例, 腾完后的空闲显存 MB)。没动手时返回 (nil, 探测值)。
// 绝不返回错误：腾显存是**优化**，失败不能阻断出片（真装不下会由
// oom_degrade 兜底降级，那条路径有完整留痕）。
func (s *Text2ImageService) evictSiblingsFor(base string) ([]string, int) {
	if vramEvictOff.Load() || s == nil || strings.TrimSpace(base) == "" {
		return nil, 0
	}
	key := strings.TrimRight(base, "/")

	// 同一实例连续提交（film 逐镜、image_batch 逐张）直接返回。
	//
	// 这不是「省一次探测」的小优化，而是必须的：热着的实例本来就占着 20GB，
	// 设备级空闲永远到不了阈值——若每次提交都去等显存涨回来，一支 8 镜的片子
	// 会白等 8×12 秒，比 OOM 更难察觉。上一发就是它，兄弟实例没有旁人去占用，
	// 判定为「无需动作」是安全的（引擎内所有提交都过 RunGraphAt + heavyMu 串行）。
	s.evictMu.Lock()
	same := strings.EqualFold(strings.TrimRight(s.lastEvictBase, "/"), key)
	s.lastEvictBase = key
	s.evictMu.Unlock()
	if same {
		return nil, 0
	}

	freeBefore, err := probeVRAMFreeMB(base)
	if err != nil || freeBefore <= 0 {
		// 探不到就不动手：不知道卡上有多少东西的时候，驱逐谁都是猜。
		return nil, 0
	}
	if freeBefore >= vramEvictThresholdMB {
		return nil, freeBefore
	}

	var freed []string
	if s.Instances != nil {
		for _, it := range s.Instances.All() {
			if !it.Enabled || strings.TrimSpace(it.BaseURL) == "" {
				continue
			}
			if strings.EqualFold(strings.TrimRight(it.BaseURL, "/"), key) {
				continue // 自己是目标，不动
			}
			// 没起的实例直接跳过：连接失败说明它本来就不占显存。
			if err := postFree(it.BaseURL); err != nil {
				continue
			}
			freed = append(freed, it.Name)
		}
	}
	if len(freed) == 0 {
		return nil, freeBefore
	}

	freeAfter := waitVRAMRise(base, vramEvictThresholdMB, vramEvictWait)
	log.Printf("[vram] 换实例前腾显存：%s 空闲 %dMB < %dMB，已释放 %v，等待后 %dMB",
		base, freeBefore, vramEvictThresholdMB, freed, freeAfter)
	return freed, freeAfter
}

// waitVRAMRise 等设备级空闲显存涨到 wantMB，或在「确认不再涨」时提前返回。
//
// 为什么要有「停止上涨」这个出口：腾完之后显存会分几次交还（CUDA 异步释放），
// 但如果目标实例自己就占着大部分显存，空闲永远到不了阈值——只按 wantMB 判，
// 每次换实例都要白等满 12 秒。连续两次采样几乎没涨就说明「该还的已经还完了」。
// 这也让「历史残留（上一个引擎进程留下的驻留）」这个探不到主人的场景不至于罚站。
func waitVRAMRise(base string, wantMB int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	last, flat := 0, 0
	for time.Now().Before(deadline) {
		time.Sleep(600 * time.Millisecond)
		free, err := probeVRAMFreeMB(base)
		if err != nil {
			continue
		}
		if free >= wantMB {
			return free
		}
		if last > 0 && free-last < 64 {
			if flat++; flat >= 2 {
				return free
			}
		} else {
			flat = 0
		}
		last = free
	}
	return last
}
