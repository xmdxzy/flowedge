package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Text2ImageService 文生图服务（ComfyUI 兼容）
type Text2ImageService struct {
	BaseURL    string
	H3BaseURL  string // H3 专用实例（ComfyUI-H3 工作树，独立 venv/torch）
	Checkpoint string
	OutputDir  string // 生成图片落盘目录（默认 data/generated）
	Template   string // ComfyUI 工作流模板名（data/comfy_workflows/<name>.json）
	client     *http.Client
}

func NewText2ImageService(baseURL, checkpoint string) *Text2ImageService {
	if checkpoint == "" {
		checkpoint = "flux1-schnell-fp8.safetensors"
	}
	h3Base := os.Getenv("H3_BASE_URL")
	if h3Base == "" {
		h3Base = "http://127.0.0.1:8189"
	}
	return &Text2ImageService{
		BaseURL:    baseURL,
		H3BaseURL:  h3Base,
		Checkpoint: checkpoint,
		client: &http.Client{
			Timeout: 300 * time.Second,
		},
	}
}

// UnloadModels 让 ComfyUI 释放已加载模型（POST /free）。
//
// 注意：必须带 {"unload_models":true,"free_memory":true} 的请求体。
// 空 body 的 /free 只做了一部分清理，实测显存不归还（flux 17GB 仍占着），
// 结果就是「换侧」后 VLM 再进来直接顶到 23.9GB/24GB。
func (s *Text2ImageService) UnloadModels() error {
	if s.BaseURL == "" {
		return fmt.Errorf("%w: ComfyUI 未配置", ErrServiceNotConfigured)
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	payload, _ := json.Marshal(map[string]bool{"unload_models": true, "free_memory": true})
	resp, err := cl.Post(s.BaseURL+"/free", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("comfyui free status %d", resp.StatusCode)
	}
	return nil
}

// FreeVRAMMB 查询 ComfyUI 侧当前可用显存（MB）。
// 卸载是异步的：/free 返回后显存要几秒才真正归还，换侧前必须等这一刻。
func (s *Text2ImageService) FreeVRAMMB() (int, error) {
	if s.BaseURL == "" {
		return 0, fmt.Errorf("comfyui not configured")
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(s.BaseURL + "/system_stats")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var st struct {
		Devices []struct {
			VRAMFree int64 `json:"vram_free"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return 0, err
	}
	if len(st.Devices) == 0 {
		return 0, fmt.Errorf("comfyui reported no devices")
	}
	return int(st.Devices[0].VRAMFree / (1 << 20)), nil
}

// ComfyTemplateDir ComfyUI 工作流模板目录（用户把自己的 workflow JSON 放这里即可接入）
const ComfyTemplateDir = "data/comfy_workflows"

// ListComfyTemplates 列出可用模板（文件名去 .json）——供前端下拉选择
func ListComfyTemplates() []string {
	entries, err := os.ReadDir(ComfyTemplateDir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	return out
}

// buildGraphFromTemplate 用模板 + 占位符替换构造 ComfyUI API 图。
//
// 约定：模板 JSON 里可写 __PROMPT__ / __NEGATIVE__ / __WIDTH__ / __HEIGHT__ / __SEED__ / __CHECKPOINT__，
// 加载时做字符串替换。这样用户从 ComfyUI 导出的 API 格式工作流（Save (API Format)）
// 改几个占位符就能直接用，不必学一套新的节点映射 DSL。
func (s *Text2ImageService) buildGraphFromTemplate(name, prompt, negative string, width, height int, seed int64) (map[string]interface{}, error) {
	raw, err := os.ReadFile(filepath.Join(ComfyTemplateDir, name+".json"))
	if err != nil {
		return nil, fmt.Errorf("读取模板 %s 失败：%w", name, err)
	}

	quote := func(v string) string {
		b, _ := json.Marshal(v)
		return strings.Trim(string(b), `"`)
	}

	text := string(raw)
	repl := map[string]string{
		"__PROMPT__":     quote(prompt),
		"__NEGATIVE__":   quote(negative),
		"__WIDTH__":      strconv.Itoa(width),
		"__HEIGHT__":     strconv.Itoa(height),
		"__SEED__":       strconv.FormatInt(seed, 10),
		"__CHECKPOINT__": quote(s.Checkpoint),
	}
	for k, v := range repl {
		text = strings.ReplaceAll(text, k, v)
	}

	var graph map[string]interface{}
	if err := json.Unmarshal([]byte(text), &graph); err != nil {
		return nil, fmt.Errorf("模板 %s 不是合法 JSON：%w", name, err)
	}
	return graph, nil
}

// GenerateImage 生成图片：POST /prompt → 轮询 /history → GET /view 下载落盘，返回本地文件路径
func (s *Text2ImageService) GenerateImage(prompt string, width, height int) (string, error) {
	if s.BaseURL == "" {
		return "", fmt.Errorf("comfyui not configured")
	}
	// 兼容矩阵预检（见 comfy_compat.go）：不兼容组合构图前显式失败
	if err := CheckCheckpointCompat(s.Checkpoint); err != nil {
		return "", err
	}
	if width <= 0 {
		width = 1024
	}
	if height <= 0 {
		height = 1024
	}

	link := func(id string, slot int) []interface{} { return []interface{}{id, slot} }

	// flux1-schnell fp8 checkpoint 标准 txt2img API 图（4 步 / cfg 1.0 / euler+simple）
	workflow := map[string]interface{}{
		"4": map[string]interface{}{
			"class_type": "CheckpointLoaderSimple",
			"inputs": map[string]interface{}{
				"ckpt_name": s.Checkpoint,
			},
		},
		"6": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs": map[string]interface{}{
				"text": prompt,
				"clip": link("4", 1),
			},
		},
		"33": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs": map[string]interface{}{
				"text": "",
				"clip": link("4", 1),
			},
		},
		"5": map[string]interface{}{
			"class_type": "EmptyLatentImage",
			"inputs": map[string]interface{}{
				"width":      width,
				"height":     height,
				"batch_size": 1,
			},
		},
		"3": map[string]interface{}{
			"class_type": "KSampler",
			"inputs": map[string]interface{}{
				"seed":         time.Now().UnixNano(),
				"steps":        4,
				"cfg":          1.0,
				"sampler_name": "euler",
				"scheduler":    "simple",
				"denoise":      1.0,
				"model":        link("4", 0),
				"positive":     link("6", 0),
				"negative":     link("33", 0),
				"latent_image": link("5", 0),
			},
		},
		"8": map[string]interface{}{
			"class_type": "VAEDecode",
			"inputs": map[string]interface{}{
				"samples": link("3", 0),
				"vae":     link("4", 2),
			},
		},
		"9": map[string]interface{}{
			"class_type": "SaveImage",
			"inputs": map[string]interface{}{
				"images":          link("8", 0),
				"filename_prefix": "flowedge",
			},
		},
	}

	// 用户提供了模板就用模板：这样 ControlNet / LoRA / img2img / upscale
	// 等任意 ComfyUI 图都能接入，而不必改 FlowEdge 的代码
	if s.Template != "" {
		g, err := s.buildGraphFromTemplate(s.Template, prompt, "", width, height, time.Now().UnixNano())
		if err != nil {
			return "", err
		}
		workflow = g
	}

	// ComfyUI /prompt 协议要求 {"prompt": <graph>} 包裹
	payload := map[string]interface{}{"prompt": workflow}
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal workflow failed: %w", err)
	}

	resp, err := s.client.Post(s.BaseURL+"/prompt", "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("comfyui request failed: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("comfyui rejected prompt (status %d): %.300s", resp.StatusCode, string(respBody))
	}

	var queued struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(respBody, &queued); err != nil || queued.PromptID == "" {
		return "", fmt.Errorf("comfyui returned no prompt_id: %.300s", string(respBody))
	}

	// 轮询执行结果（首次运行含模型冷加载，给足 7 分钟）
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(1500 * time.Millisecond)

		hresp, err := s.client.Get(s.BaseURL + "/history/" + queued.PromptID)
		if err != nil {
			continue
		}
		hbody, _ := io.ReadAll(hresp.Body)
		hresp.Body.Close()
		if hresp.StatusCode != http.StatusOK {
			continue
		}

		var hist map[string]comfyHistoryEntry
		if json.Unmarshal(hbody, &hist) != nil {
			continue
		}
		entry, ok := hist[queued.PromptID]
		if !ok {
			continue // 尚未开始执行
		}
		if entry.Status.StatusStr == "error" {
			return "", fmt.Errorf("comfyui generation failed (prompt_id=%s)", queued.PromptID)
		}

		var imgFile, imgSub, imgType string
		for _, out := range entry.Outputs {
			for _, im := range out.Images {
				imgFile, imgSub, imgType = im.Filename, im.Subfolder, im.Type
				break
			}
			if imgFile != "" {
				break
			}
		}
		if imgFile == "" {
			continue // 仍在生成
		}

		viewURL := fmt.Sprintf("%s/view?filename=%s&subfolder=%s&type=%s",
			s.BaseURL, url.QueryEscape(imgFile), url.QueryEscape(imgSub), url.QueryEscape(imgType))
		dresp, err := s.client.Get(viewURL)
		if err != nil {
			return "", fmt.Errorf("comfyui download failed: %w", err)
		}
		data, err := io.ReadAll(dresp.Body)
		dresp.Body.Close()
		if err != nil {
			return "", err
		}
		if dresp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("comfyui /view returned status %d", dresp.StatusCode)
		}

		dir := "data/generated"
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", err
		}
		outPath := filepath.Join(dir, fmt.Sprintf("flowedge-%d-%s", time.Now().Unix(), imgFile))
		if err := os.WriteFile(outPath, data, 0644); err != nil {
			return "", err
		}
		if abs, err := filepath.Abs(outPath); err == nil {
			return abs, nil
		}
		return outPath, nil
	}

	return "", fmt.Errorf("comfyui generation timed out after 7m (prompt_id=%s)", queued.PromptID)
}

// comfyHistoryEntry ComfyUI /history 返回条目
type comfyHistoryEntry struct {
	Status struct {
		StatusStr string `json:"status_str"`
	} `json:"status"`
	Outputs map[string]struct {
		Images []comfyFileRef `json:"images"`
		Gifs   []comfyFileRef `json:"gifs"`
		Videos []comfyFileRef `json:"videos"`
	} `json:"outputs"`
}

// comfyFileRef ComfyUI 产物引用（图片 / gif / 视频共用）
type comfyFileRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// ASRService 语音转写服务
type ASRService struct {
	BaseURL string
	client  *http.Client
}

func NewASRService(baseURL string) *ASRService {
	return &ASRService{
		BaseURL: baseURL,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Transcribe 语音转文字
func (s *ASRService) Transcribe(audioPath string) (string, error) {
	if s.BaseURL == "" {
		// 未配置必须显式失败：伪造转写文本会让用户以为真的转写成功了
		return "", fmt.Errorf("%w: ASR 服务未配置（设置 ASR_BASE_URL，或由桌面客户端自动拉起语音服务）", ErrServiceNotConfigured)
	}

	file, err := os.Open(audioPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// 简单 multipart 上传
	var buf bytes.Buffer
	buf.WriteString("--boundary\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"audio.wav\"\r\n")
	buf.WriteString("Content-Type: audio/wav\r\n\r\n")
	io.Copy(&buf, file)
	buf.WriteString("\r\n--boundary--\r\n")

	resp, err := s.client.Post(s.BaseURL+"/asr", "multipart/form-data; boundary=boundary", &buf)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("asr service returned status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	text, _ := result["text"].(string)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("asr service returned empty transcription")
	}
	return text, nil
}

// TTSService 语音合成服务
type TTSService struct {
	BaseURL string
	client  *http.Client
}

func NewTTSService(baseURL string) *TTSService {
	return &TTSService{
		BaseURL: baseURL,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// TTSOptions 语音合成参数（当前仅内置 SAPI 后端使用；HTTP 服务端可选支持）
type TTSOptions struct {
	Voice string // 语音名片段，如 "Huihui"；空则用系统默认
	Rate  int    // 语速 -10..10
}

// Synthesize 文字转语音（兼容旧调用）
func (s *TTSService) Synthesize(text string, outputPath string) error {
	_, err := s.SynthesizeWith(text, outputPath, TTSOptions{})
	return err
}

// SynthesizeWith 文字转语音，返回实际使用的后端（service / sapi / sapi-fallback）
//
// 降级顺序：显式配置的 TTS_BASE_URL 服务 → Windows 内置 SAPI。
// 两条路都不通时显式失败——绝不写出假音频让下游「成功」产出垃圾。
//
// 「配了服务但服务没起来」也算第一路失败（2026-10-20 修）：
// 能力探测早就把这种情况判成 tts.ok=true（SAPI 兜底），但节点路径却直接把
// connection refused 抛出去——预检说能跑、节点说不能跑，同一件事两个答案。
// 现在统一：服务不可达/报错 → 用 SAPI 真合成，后端标记 sapi-fallback 并留日志，
// 既不伪造结果，也不让一个没启动的可选服务废掉本来能干的活。
func (s *TTSService) SynthesizeWith(text string, outputPath string, opt TTSOptions) (string, error) {
	if s.BaseURL != "" {
		if err := s.synthesizeHTTP(text, outputPath, opt); err == nil {
			return "service", nil
		} else if SAPIAvailable() {
			if sapiErr := synthesizeSAPI(text, outputPath, opt.Voice, opt.Rate); sapiErr == nil {
				log.Printf("[tts] 服务 %s 不可用（%v），已回退 Windows 内置 SAPI（产出的是真音频）", s.BaseURL, err)
				return "sapi-fallback", nil
			} else {
				return "", fmt.Errorf("TTS 服务不可用（%v），内置 SAPI 也失败（%v）", err, sapiErr)
			}
		} else {
			return "", err
		}
	}
	if SAPIAvailable() {
		if err := synthesizeSAPI(text, outputPath, opt.Voice, opt.Rate); err != nil {
			return "", err
		}
		return "sapi", nil
	}
	return "", fmt.Errorf("%w: TTS 不可用：未配置 TTS_BASE_URL，且当前系统不支持 Windows 内置语音（非 Windows，或已设置 FLOWEDGE_TTS_SAPI=0）", ErrServiceNotConfigured)
}

// synthesizeHTTP 走本地 / 远程 TTS 服务
func (s *TTSService) synthesizeHTTP(text string, outputPath string, opt TTSOptions) error {
	reqBody := map[string]interface{}{
		"text": text,
	}
	if opt.Voice != "" {
		reqBody["voice"] = opt.Voice
	}
	if opt.Rate != 0 {
		reqBody["rate"] = opt.Rate
	}
	jsonBody, _ := json.Marshal(reqBody)

	resp, err := s.client.Post(s.BaseURL+"/tts", "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("tts service returned status %d: %s", resp.StatusCode, string(body))
	}

	// 保存音频文件
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	n, err := io.Copy(out, resp.Body)
	if err != nil {
		return err
	}
	if n < 1000 {
		return fmt.Errorf("tts service returned too-small audio (%d bytes)", n)
	}
	return nil
}

// VideoService 视频处理服务（FFmpeg）
type VideoService struct {
	FFmpegPath string
}

// NewVideoService 构造视频服务。
// 支持 FFMPEG_PATH 覆盖（能力自检的提示文案一直宣称可设置该变量，但实现里
// 从未读取——典型的「文档说支持、代码不支持」）。
func NewVideoService() *VideoService {
	p := os.Getenv("FFMPEG_PATH")
	if p == "" {
		p = "ffmpeg"
	}
	return &VideoService{
		FFmpegPath: p,
	}
}

// ComposeVideo 用第一张图 + 可选音轨生成视频（兼容旧调用）
func (s *VideoService) ComposeVideo(imagePaths []string, audioPath string, outputPath string) error {
	return s.ComposeVideoMulti(imagePaths, audioPath, outputPath, 3)
}

// ComposeVideoMulti 多图合成：每张图按 perImageSec 秒轮播，再叠加音轨。
//
// 原实现只用 imagePaths[0]，其余图片被静默丢弃（用户的 4 张分镜只出 1 张）。
// 现在用 concat filter 一次调用完成，避免多次转码带来的画质损失。
//
// 超时保护：exec.Command 没有 ctx 时，ffmpeg 卡住会永久占用一个 goroutine 且
// 节点超时后仍在跑（进程泄漏）。这里统一 10 分钟上限。
func (s *VideoService) ComposeVideoMulti(imagePaths []string, audioPath string, outputPath string, perImageSec float64) error {
	if len(imagePaths) == 0 {
		return fmt.Errorf("no images provided")
	}
	if perImageSec <= 0 {
		perImageSec = 3
	}
	if perImageSec > 30 {
		perImageSec = 30
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	args := []string{"-y"}
	for _, img := range imagePaths {
		args = append(args, "-loop", "1", "-t", fmt.Sprintf("%.2f", perImageSec), "-i", img)
	}
	if audioPath != "" {
		args = append(args, "-i", audioPath)
	}

	// 视频流拼接。
	//
	// 单图场景曾经有个隐蔽 bug：只有多图才加 -map [v]，单图时又用 -map 1:a 指了音轨，
	// ffmpeg 在有显式 -map 时只输出被映射的流 → 产出一个「只有声音没有画面」的 mp4。
	// 这里统一显式映射视频流，单图也必须走 -map 0:v。
	if len(imagePaths) > 1 {
		var filter string
		for i := range imagePaths {
			filter += fmt.Sprintf("[%d:v]", i)
		}
		filter += fmt.Sprintf("concat=n=%d:v=1:a=0[v]", len(imagePaths))
		args = append(args, "-filter_complex", filter, "-map", "[v]")
	} else {
		args = append(args, "-map", "0:v")
	}

	args = append(args, "-c:v", "libx264", "-tune", "stillimage", "-pix_fmt", "yuv420p")

	if audioPath != "" {
		// 音频输入索引 = 图片数量
		args = append(args, "-map", fmt.Sprintf("%d:a", len(imagePaths)),
			"-c:a", "aac", "-b:a", "192k", "-shortest")
	} else {
		args = append(args, "-t", fmt.Sprintf("%.2f", perImageSec*float64(len(imagePaths))))
	}

	args = append(args, outputPath)

	cmd := exec.CommandContext(ctx, s.FFmpegPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("ffmpeg 超时（10 分钟）已终止")
		}
		return fmt.Errorf("ffmpeg failed: %w, output: %s", err, truncateRunes(string(output), 600))
	}
	return nil
}

func (s *VideoService) AddSubtitles(videoPath string, subtitlePath string, outputPath string) error {
	cmd := exec.Command(s.FFmpegPath,
		"-y", "-i", videoPath,
		"-vf", fmt.Sprintf("subtitles=%s", subtitlePath),
		"-c:a", "copy",
		outputPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("add subtitles failed: %w, output: %s", err, string(output))
	}
	return nil
}
