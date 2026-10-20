package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Capability 单项能力探测结果
type Capability struct {
	OK       bool     `json:"ok"`
	Detail   string   `json:"detail,omitempty"`
	Hint     string   `json:"hint,omitempty"` // 缺失时给出具体的安装/启动指引
	Models   []string `json:"models,omitempty"`
	Endpoint string   `json:"endpoint,omitempty"`
	Path     string   `json:"path,omitempty"`
}

// Capabilities 部署环境能力矩阵
type Capabilities struct {
	OS      string          `json:"os"`
	BaseDir string          `json:"base_dir"`
	GPU     Capability      `json:"gpu"`
	Ollama  Capability      `json:"ollama"`
	ComfyUI Capability      `json:"comfyui"`
	ComfyH3 Capability      `json:"comfy_h3"` // H3 独立实例（音视频联合生成，端口 8189）
	Vision  Capability      `json:"vision"`
	FFmpeg  Capability      `json:"ffmpeg"`
	ASR     Capability      `json:"asr"`
	TTS     Capability      `json:"tts"`
	Ready   map[string]bool `json:"ready"` // llm / image / h3 / comfy / audio / video / vision
	Missing []string        `json:"missing"`
}

var probeClient = &http.Client{Timeout: 1500 * time.Millisecond}

// ProbeCapabilities 并发探测各项外部依赖（总耗时 ≈ 最慢一项，通常 < 1.5s）
//
// comfyH3Base 是 H3 工作树的独立实例（独立 venv / torch 2.14，端口 8189）。
// 它与主实例靠 DepSpec.Exclusive 互斥，所以必须分开探测：只探 8188 会把
// 「H3 在跑、出图不可用」误报成「出图缺失」，反方向则会把文生图标成可用。
func ProbeCapabilities(gpu *GPUScheduler, baseDir, ollamaBase, comfyBase, comfyH3Base, asrBase, ttsBase string) Capabilities {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		caps  = Capabilities{OS: runtime.GOOS + "/" + runtime.GOARCH, BaseDir: baseDir}
		setOK = func(dst *Capability, c Capability) {
			mu.Lock()
			*dst = c
			mu.Unlock()
		}
	)

	// GPU
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := Capability{Hint: "未检测到 NVIDIA GPU。本引擎依赖 CUDA 推理，请安装显卡驱动后重试。"}
		if gpu != nil {
			if info, err := gpu.GetGPUInfo(); err == nil && info != nil {
				c.OK = true
				c.Detail = fmt.Sprintf("显存 %dMB / 已用 %dMB / 利用率 %d%%", info.TotalMB, info.UsedMB, info.Utilization)
				c.Hint = ""
			} else if err != nil {
				c.Detail = err.Error()
			}
		}
		setOK(&caps.GPU, c)
	}()

	// Ollama（顺带取模型列表，供前端提示「可用的模型有哪些」）
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := Capability{
			Endpoint: ollamaBase,
			Hint:     "未连接到 Ollama。请安装 Ollama（ollama.com）后运行 ollama serve，并拉取模型：ollama pull qwen2.5-coder:7b",
		}
		if ollamaBase != "" {
			if body, err := httpGetBody(ollamaBase + "/api/tags"); err == nil {
				var tags struct {
					Models []struct {
						Name string `json:"name"`
					} `json:"models"`
				}
				if json.Unmarshal(body, &tags) == nil {
					c.OK = true
					c.Hint = ""
					for _, m := range tags.Models {
						c.Models = append(c.Models, m.Name)
					}
					if len(c.Models) == 0 {
						c.Hint = "Ollama 已运行但没有可用模型。请执行：ollama pull qwen2.5-coder:7b"
					}
					c.Detail = fmt.Sprintf("%d 个模型", len(c.Models))
				}
			}
		}
		setOK(&caps.Ollama, c)
	}()

	// ComfyUI（文生图）
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := Capability{
			Endpoint: comfyBase,
			Hint:     "未连接到 ComfyUI。文生图需要本地 ComfyUI：启动后设置环境变量 COMFYUI_BASE_URL（默认 http://127.0.0.1:8188）",
		}
		if comfyBase != "" {
			if _, err := httpGetBody(comfyBase + "/system_stats"); err == nil {
				c.OK = true
				c.Hint = ""
				c.Detail = "已连接"
			}
		}
		setOK(&caps.ComfyUI, c)
	}()

	// ComfyUI-H3（音视频联合生成）：另一个工作树 + 另一套 venv，端口 8189
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := Capability{
			Endpoint: comfyH3Base,
			Hint:     "未连接到 ComfyUI-H3。音视频联合生成（文生视频/分镜执行）需要独立工作树 D:/AI/ComfyUI-H3：在「环境」面板一键启动，或设置 H3_BASE_URL（默认 http://127.0.0.1:8189）",
		}
		if comfyH3Base != "" {
			if _, err := httpGetBody(comfyH3Base + "/system_stats"); err == nil {
				c.OK = true
				c.Hint = ""
				c.Detail = "已连接（H3 音视频实例）"
			}
		}
		setOK(&caps.ComfyH3, c)
	}()

	// 视觉理解（VLM）：从本地 Ollama 已装模型里找能看图的
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := Capability{
			Endpoint: ollamaBase,
			Hint:     "未检测到视觉模型。拉一个即可：ollama pull qwen2.5vl:7b（约 6GB，3090 上可跑）",
		}
		if ollamaBase != "" {
			if m := PickVisionModel(ollamaBase); m != "" {
				c.OK = true
				c.Hint = ""
				c.Detail = m
				c.Models = []string{m}
			}
		}
		setOK(&caps.Vision, c)
	}()

	// ffmpeg（视频合成）
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := Capability{Hint: "未找到 ffmpeg。视频合成需要它：从 gyan.dev/ffmpeg/builds 下载后把 bin 目录加入 PATH（或设置 FFMPEG_PATH）"}
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			c.OK = true
			c.Path = p
			c.Detail = p
			c.Hint = ""
		}
		setOK(&caps.FFmpeg, c)
	}()

	// ASR / TTS（本地 Python 服务，可选）
	probeLocal := func(dst *Capability, base, name, hint string) {
		defer wg.Done()
		c := Capability{Endpoint: base, Hint: hint}
		if base != "" {
			if _, err := httpGetBody(base + "/health"); err == nil {
				c.OK = true
				c.Hint = ""
				c.Detail = name + " 服务已就绪"
			}
		}
		setOK(dst, c)
	}
	// 注意：未配置时 ASR/TTS 节点会显式失败（error_code=config_missing），
	// 绝不伪造转写文本或假音频——提示文案必须如实说明这一点
	wg.Add(1)
	go probeLocal(&caps.ASR, asrBase, "ASR",
		"未配置：ASR 节点会直接失败（不会伪造转写结果）。两种启用方式：① 界面「环境」面板点「启动 ASR」由引擎托管 faster-whisper；② 手动运行 FlowEdge.exe --export-services 导出脚本后 python asr_server.py（首次会从镜像下载 whisper 模型约 500MB）")
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := Capability{Endpoint: ttsBase, Hint: "未配置：TTS 节点会直接失败（不会写出假音频）。非 Windows 环境请设置 TTS_BASE_URL 指向本地 TTS 服务"}
		if ttsBase != "" {
			if _, err := httpGetBody(ttsBase + "/health"); err == nil {
				c.OK = true
				c.Hint = ""
				c.Detail = "TTS 服务已就绪"
				setOK(&caps.TTS, c)
				return
			}
		}
		// 零依赖兜底：Windows 自带 SAPI 语音引擎，不需要 Python 服务
		if SAPIAvailable() {
			c.OK = true
			c.Hint = ""
			c.Detail = "Windows 内置 SAPI（无需 Python 服务）"
			if info := SAPIInfo(); info != "" {
				c.Detail = "Windows 内置 SAPI：" + info
			}
			c.Path = "sapi"
		}
		setOK(&caps.TTS, c)
	}()

	wg.Wait()

	// image / h3 / comfy 三者语义必须分开，否则会重演两种假状态（AGENTS §0.6）：
	//   ① H3 在 8189 跑着，界面却报「文生图缺失」；
	//   ② 只有 H3、没有主实例，却把文生图标成可用（H3 不出图）。
	//
	// audio / asr / tts 同理必须分开（2026-10-20 修）：
	// TTS 走 Windows 内置 SAPI 时零服务可用（tts.ok=true），但只要 ASR 没起，
	// 合并口径 audio=false 就会把「配音」整条链路挡在预检外面——能力明明在，
	// 报错却说「ASR/TTS 服务未启动」。所以：
	//   asr/tts = 单边真值（各自能不能干）
	//   audio   = 整条音频链路（双向都通才为真），保留给「要转写又要合成」的图
	caps.Ready = map[string]bool{
		"llm":    caps.Ollama.OK && len(caps.Ollama.Models) > 0,
		"image":  caps.ComfyUI.OK,                    // 出图/重绘/超分：必须主实例 8188
		"h3":     caps.ComfyH3.OK,                    // 音视频联合生成：H3 实例 8189
		"comfy":  caps.ComfyUI.OK || caps.ComfyH3.OK, // 任一实例在跑就算「有 ComfyUI」
		"audio":  caps.ASR.OK && caps.TTS.OK,
		"asr":    caps.ASR.OK,
		"tts":    caps.TTS.OK,
		"video":  caps.FFmpeg.OK,
		"vision": caps.Vision.OK,
	}
	for name, ok := range caps.Ready {
		// asr/tts 是 audio 的分项诊断，不再重复计进 missing，避免缺项清单噪音
		if !ok && name != "asr" && name != "tts" {
			caps.Missing = append(caps.Missing, name)
		}
	}
	return caps
}

// PickDefaultModel 在用户没指定模型时，从本地 Ollama 里挑一个可用的
// 优先级：coder > 通用 qwen 系 > 第一个（保证「装上就能跑」）
func PickDefaultModel(ollamaBase string) string {
	body, err := httpGetBody(ollamaBase + "/api/tags")
	if err != nil {
		return ""
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &tags) != nil || len(tags.Models) == 0 {
		return ""
	}
	names := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		names = append(names, m.Name)
	}

	// 引擎调用 LLM 的场景是分镜、提示词改写、图片判分、文档解析——全是自然语言创作。
	// 旧逻辑优先返回 coder 模型，实测自动选到 qwen2.5-coder:32b：写出来的镜头描述
	// 像工程注释，且 32B 与扩散/语音抢显存。改成优先通用对话模型，coder 放到最后。
	usable := func(m string) bool {
		l := strings.ToLower(m)
		return !strings.Contains(l, "embed") && !strings.Contains(l, "bge") && !strings.Contains(l, "rerank")
	}
	pick := func(filter func(string) bool) string {
		cand := make([]string, 0, len(names))
		for _, m := range names {
			if usable(m) && filter(m) {
				cand = append(cand, m)
			}
		}
		if len(cand) == 0 {
			return ""
		}
		// 同类里按名字排序取第一个：qwen2.5:14b 会排在 qwen3.8:27b 之前。
		// 14B 在 24G 卡上和扩散/语音共存更从容，实测对提示词指令的遵循也更稳。
		sort.Strings(cand)
		return cand[0]
	}
	for _, key := range []string{"qwen", "glm", "llama", "mistral", "gemma", "deepseek"} {
		if m := pick(func(s string) bool {
			return strings.Contains(strings.ToLower(s), key) && !strings.Contains(strings.ToLower(s), "coder")
		}); m != "" {
			return m
		}
	}
	if m := pick(func(s string) bool { return strings.Contains(strings.ToLower(s), "coder") }); m != "" {
		return m
	}
	return pick(func(string) bool { return true })
}

func httpGetBody(url string) ([]byte, error) {
	resp, err := probeClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 8192)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 1<<20 {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

func containsFold(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	ls, lsub := lower(s), lower(sub)
	for i := 0; i+len(sub) <= len(s); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return true
		}
	}
	return false
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
