package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Unload 让依赖把**模型权重**交还显存，但保留进程。
//
// 由来（2026-10-26）：平台只有 start / stop 两个动作，而"把 H3 占着的 20GB 要回来"
// 只有 stop 一条路——代价是下次冷启动 30–60 秒 + 权重重新加载 3 分钟。
// 但 ComfyUI 自己的 /free 只卸权重、进程还在：下次出片只需加载权重，
// 省掉进程启动那一段。**这是个缺了很久的中间档**。
//
// 另一处用处：出现两个实例抢卡时（2026-10-26 那晚的 27 倍慢），
// 可以把不干活的那份权重先卸掉，而不必杀进程、不必担心中断正在跑的活。
//
// 支持的依赖：ComfyUI 三实例（/free）、Ollama（keep_alive=0）。
// ASR/TTS 没有卸载接口，如实说"不支持"，不假装成功（§0.6）。
func (s *Supervisor) Unload(name string) (UnloadResult, error) {
	spec, ok := s.specs[name]
	if !ok {
		return UnloadResult{Name: name}, fmt.Errorf("未知依赖：%s", name)
	}
	res := UnloadResult{Name: name}
	if !s.Healthy(name) {
		return res, fmt.Errorf("%s 未运行，无需卸载（启动它才需要显存）", name)
	}

	before := s.deviceFreeVRAMMB()

	switch {
	case strings.HasPrefix(name, "comfy"):
		res.Supported = true
		if err := postFree(spec.Endpoint); err != nil {
			return res, fmt.Errorf("让 %s 释放模型失败：%w", name, err)
		}
		res.Note = "已请求 ComfyUI 卸载模型（进程保留，下次出片只需重新加载权重）"
	case name == "tts":
		// TTS 也走 /unload（2026-10-28 补）。
		//
		// 此前这里落到 default，界面点「卸载」会答「该依赖没有卸载接口
		// （权重常驻内存/CPU，不在显存里）」——那是 Audio8/Kokoro 时代的事实。
		// 对现在这台机器上的 Breeze（GPU、驻留实测 16274 MiB）这句是错的，
		// 而且它会让用户以为「没有别的办法」，只能整进程 stop。
		//
		// Breeze 的 /unload 修好后（见 tts_breeze_server.py 的 _release_vram）
		// 实测回收 14.3GB / 6.1s，下次合成 60s 自动重载，所以这是一档正经的中间态。
		if ttsOnCPU() {
			res.Supported = false
			res.Note = "TTS 跑在 CPU 上，不占显存，无需卸载"
			return res, nil
		}
		if err := postEmpty(spec.Endpoint, "/unload"); err != nil {
			if errors.Is(err, errDepEndpointMissing) {
				res.Supported = false
				res.Note = "该 TTS 后端不支持 /unload（Kokoro / Audio8 / SAPI），" +
					"要释放显存只能停服务：POST /api/deps/tts/stop"
				return res, nil
			}
			return res, fmt.Errorf("让 %s 释放模型失败：%w", name, err)
		}
		res.Supported = true
		res.Note = "已请求 TTS 卸载模型（进程保留，下次合成自动重载）"
	case name == "ollama":
		res.Supported = true
		n, err := unloadOllamaModels(spec.Endpoint)
		if err != nil {
			return res, fmt.Errorf("让 Ollama 卸载模型失败：%w", err)
		}
		res.Note = fmt.Sprintf("已要求 Ollama 卸载 %d 个已驻留模型（keep_alive=0）", n)
	default:
		res.Note = "该依赖没有卸载接口（权重常驻内存/CPU，不在显存里）"
		res.Supported = false
		return res, nil
	}

	// 拿不到读数就别等：没有「之前多少」就没有「还回来多少」，
	// 干等 12 秒只是把接口变慢（实测 Ollama 卸载在无 ComfyUI 实例时要白等满一轮）。
	if before <= 0 {
		res.Note += "；拿不到设备级空闲显存读数，无法确认实际回收量"
		return res, nil
	}

	after := s.waitDeviceFreeRise(before, 12*time.Second)
	res.FreeMB = after
	if before > 0 && after > before {
		res.FreedMB = after - before
	}
	return res, nil
}

// ServingButUnmanaged 该依赖是否「端口通、但不是本进程托管的」。
//
// 由来（2026-10-26 那晚）：8189 上是别人起的第二个 ComfyUI-H3，
// 它抢走 20GB 显存，真正的服务端只剩 752MB，出片从 85s 退化到 2269s，
// 而当时**没有任何一行日志**把「这个端口不是我们起的」说出来。
// 这个判定把那种状态变成一个可查询的事实，出片时就能喊一声。
func (s *Supervisor) ServingButUnmanaged(name string) bool {
	if s == nil {
		return false
	}
	if _, ok := s.specs[name]; !ok {
		return false
	}
	if !s.Healthy(name) {
		return false
	}
	s.mu.Lock()
	_, ours := s.procs[name]
	s.mu.Unlock()
	return !ours
}

// UnloadResult 一次卸载的结果（给界面/脚本看的人话 + 数字）
type UnloadResult struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	FreedMB   int    `json:"freed_mb"`
	FreeMB    int    `json:"free_mb"`
	Note      string `json:"note"`
}

// deviceFreeVRAMMB 通过任意一个可达的 ComfyUI 实例读**设备级**空闲显存。
//
// 用设备级而不是进程级：我们要的是"整张卡还有多少"，别的地方占的也算数。
// 一个都读不到返回 0（调用方据此跳过前后对比，不猜）。
func (s *Supervisor) deviceFreeVRAMMB() int {
	s.mu.Lock()
	specs := make([]*DepSpec, 0, len(s.specs))
	for n, sp := range s.specs {
		if strings.HasPrefix(n, "comfy") && sp.Endpoint != "" {
			specs = append(specs, sp)
		}
	}
	s.mu.Unlock()
	for _, sp := range specs {
		if free, err := probeVRAMFreeMB(sp.Endpoint); err == nil && free > 0 {
			return free
		}
	}
	// 回退到驱动读数。
	//
	// 2026-10-28 实测的误导现场：只托管 TTS、没有任何 comfy 实例在跑时，
	// 上面那个循环一个端点都探不到 ⇒ 返回 0 ⇒ 日志打出
	// 「拿不到设备级空闲显存读数，无法确认实际回收量」，
	// 而那一刻实际上回收了 14353 MB。**把「没查」写成了「查不到」**，
	// 正是 §0.6 要求区分「不可达」与「空结果」的同型错误。
	return nvidiaFreeVRAMMB()
}

// nvidiaFreeVRAMMB 直接问驱动整机空闲显存（MiB）。拿不到返回 0。
//
// 用 nounits：默认输出带 "9999 MiB" 后缀，多显卡时按行取第一张，
// 与本项目「单卡」的既有假设一致（总显存 24576MB 也来自同一处假设）。
func nvidiaFreeVRAMMB() int {
	out, err := hiddenExec("nvidia-smi", "--query-gpu=memory.free",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// errDepEndpointMissing 依赖不提供这个端点（例如 Kokoro / Audio8 没有 /unload）。
//
// 定义成哨兵而不是普通 error，是为了让调用方区分「后端不支持」和「请求失败」：
// 前者该如实说明能力缺失（§0.6），后者才是真错误。
var errDepEndpointMissing = errors.New("dependency does not expose this endpoint")

// postEmpty POST 到一个不需要 body 的端点（TTS 的 /unload 就是这种）。
// 超时给 60s：Breeze 真实卸载要 6s 左右（gc + synchronize + empty_cache）。
func postEmpty(base, path string) error {
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Post(strings.TrimRight(base, "/")+path, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return errDepEndpointMissing
	default:
		return fmt.Errorf("%s returned status %d", path, resp.StatusCode)
	}
}

// waitDeviceFreeRise 等空闲显存涨上来（卸载是异步的：CUDA 要几秒才交还），
// 连续两次采样不再明显上涨就返回——不能干等满 12 秒（见 comfy_evict.go 同一教训）。
func (s *Supervisor) waitDeviceFreeRise(before int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	last, flat := before, 0
	for time.Now().Before(deadline) {
		time.Sleep(600 * time.Millisecond)
		free := s.deviceFreeVRAMMB()
		if free <= 0 {
			continue
		}
		if before > 0 && free-before >= 256 {
			return free // 已经还回来一批，够了
		}
		if last > 0 && free-last < 32 {
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

// unloadOllamaModels 让 Ollama 里所有已驻留模型立即卸载（keep_alive=0），返回卸载个数。
func unloadOllamaModels(base string) (int, error) {
	base = strings.TrimRight(base, "/")
	cl := &http.Client{Timeout: 10 * time.Second}

	resp, err := cl.Get(base + "/api/ps")
	if err != nil {
		return 0, err
	}
	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ps)
	resp.Body.Close()
	if err != nil {
		return 0, err
	}
	if len(ps.Models) == 0 {
		return 0, nil
	}
	n := 0
	for _, m := range ps.Models {
		payload := fmt.Sprintf(`{"model":%q,"keep_alive":0}`, m.Name)
		req, err := http.NewRequest(http.MethodPost, base+"/api/generate", strings.NewReader(payload))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		r, err := cl.Do(req)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
		r.Body.Close()
		if r.StatusCode == http.StatusOK {
			n++
		}
	}
	return n, nil
}
