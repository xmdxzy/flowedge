package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
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
	FFmpeg  Capability      `json:"ffmpeg"`
	ASR     Capability      `json:"asr"`
	TTS     Capability      `json:"tts"`
	Ready   map[string]bool `json:"ready"` // llm / image / audio / video
	Missing []string        `json:"missing"`
}

var probeClient = &http.Client{Timeout: 1500 * time.Millisecond}

// ProbeCapabilities 并发探测各项外部依赖（总耗时 ≈ 最慢一项，通常 < 1.5s）
func ProbeCapabilities(gpu *GPUScheduler, baseDir, ollamaBase, comfyBase, asrBase, ttsBase string) Capabilities {
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
	wg.Add(1)
	go probeLocal(&caps.ASR, asrBase, "ASR",
		"可选能力。先运行 FlowEdge.exe --export-services 导出脚本到 local-services/，再装 Python 并执行 python asr_server.py（首次会自动下载 whisper 模型约 500MB）")
	wg.Add(1)
	go probeLocal(&caps.TTS, ttsBase, "TTS",
		"可选能力。先运行 FlowEdge.exe --export-services 导出脚本到 local-services/，再装 Python 并执行 python tts_server.py（用 Windows 自带语音引擎，无需下载模型）")

	wg.Wait()

	caps.Ready = map[string]bool{
		"llm":   caps.Ollama.OK && len(caps.Ollama.Models) > 0,
		"image": caps.ComfyUI.OK,
		"audio": caps.ASR.OK && caps.TTS.OK,
		"video": caps.FFmpeg.OK,
	}
	for name, ok := range caps.Ready {
		if !ok {
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
	for _, m := range names {
		if containsFold(m, "coder") {
			return m
		}
	}
	for _, m := range names {
		if containsFold(m, "qwen") {
			return m
		}
	}
	return names[0]
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
