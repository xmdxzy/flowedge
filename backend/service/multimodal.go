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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Text2ImageService 文生图服务（ComfyUI 兼容）
type Text2ImageService struct {
	BaseURL    string
	H3BaseURL  string // H3 专用实例（ComfyUI-H3 工作树，独立 venv/torch）
	Checkpoint string
	OutputDir  string // 生成图片落盘目录（默认 data/generated）
	Template   string // ComfyUI 工作流模板名（data/comfy_workflows/<name>.json）

	// Bundles 逻辑角色 → 权重文件 + 默认参数的配置仓库（data/models.json）。
	// nil 时所有读取回落到内置默认，行为与改造前一致。
	Bundles *BundleStore

	// Instances 模型 → ComfyUI 实例路由（data/instances.json）。
	// nil 时按 kind 直接选 BaseURL / H3BaseURL，行为与改造前一致。
	Instances *InstanceRegistry

	// evictMu 保护 lastEvictBase：跨实例腾显存时要知道「上一次跑在哪个实例」，
	// 同实例连续跑（film 的逐镜、image_batch 的逐张）就直接跳过，一次 HTTP 都不发。
	evictMu       sync.Mutex
	lastEvictBase string

	client *http.Client
}

// defaultHTTPClient 兜底 HTTP 客户端。
//
// 为什么需要：服务可能被结构体字面量构造（测试、以及将来别的装配路径），
// 此时 client 为 nil，直接调 s.client.Post 会 **nil pointer panic**——
// 表现是进程崩掉而不是返回一个错误。这里给共享兜底，
// 让装配失误退化成「正常请求」而不是「崩溃」。
var defaultHTTPClient = &http.Client{Timeout: 300 * time.Second}

func (s *Text2ImageService) httpClient() *http.Client {
	if s != nil && s.client != nil {
		return s.client
	}
	return defaultHTTPClient
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
// GenerateImage 用服务默认 checkpoint 出图。
func (s *Text2ImageService) GenerateImage(prompt string, width, height int) (string, error) {
	return s.GenerateImageWithCkpt(prompt, width, height, "")
}

// GenerateImageWithCkpt 节点级 checkpoint 覆盖：空 = 用服务默认。
// 为什么要能按节点换：同一张图用 flux-schnell 求快、用 qwen_image 求质，
// 是两条不同的流水线；强迫全局改配置等于一次只能存在一种。
// T2IOptions 一次文生图的完整入参。
//
// Backend 为空 = auto：由提示词与布局决定（判据见 ResolveT2IBackend，可审计）。
type T2IOptions struct {
	Backend  T2IBackend
	Prompt   string
	Negative string
	Width    int
	Height   int
	Steps    int
	Cfg      float64
	Ckpt     string // flux：checkpoint 名；空 = 服务默认
	Layout   string // moss：layout JSON；非空时替代 Prompt 作为条件
}

// GenerateImageWithCkpt 节点级 checkpoint 覆盖：空 = 用服务默认。
//
// 为什么要能按节点换：同一张图用 flux-schnell 求快、用 MOSS 求字，
// 是两条不同的流水线；强迫全局改配置等于一次只能存在一种。
func (s *Text2ImageService) GenerateImageWithCkpt(prompt string, width, height int, ckpt string) (string, error) {
	return s.GenerateImageWithBackend(T2IOptions{
		Backend: BackendFlux, Prompt: prompt, Width: width, Height: height, Ckpt: ckpt,
	})
}

// GenerateImageWithBackend 按后端出图：选实例 → 构图 → 提交 → 轮询 → 落盘。
//
// 为什么要多这一层而不是在 GenerateImageWithCkpt 里加 if：换后端换的不只是权重名，
// 是**整套 ComfyUI 图 + 目标实例**。这两件事必须一起决定，分开决定必然出现
// 「图按 MOSS 搭的，却发给了没有 MOSS 节点的主实例」。
func (s *Text2ImageService) GenerateImageWithBackend(opt T2IOptions) (string, error) {
	if s.BaseURL == "" {
		return "", fmt.Errorf("comfyui not configured")
	}
	backend, reason := ResolveT2IBackend(string(opt.Backend), opt.Prompt, strings.TrimSpace(opt.Layout) != "")

	var (
		workflow map[string]interface{}
		baseURL  string
		timeout  time.Duration
	)

	switch backend {
	case BackendMOSS:
		w, h := opt.Width, opt.Height
		if w <= 0 {
			w = int(s.bundleParam(RoleT2IMOSS, "width", mossDefaultW))
		}
		if h <= 0 {
			h = int(s.bundleParam(RoleT2IMOSS, "height", mossDefaultH))
		}
		steps := opt.Steps
		if steps <= 0 {
			steps = int(s.bundleParam(RoleT2IMOSS, "steps", mossDefaultSteps))
		}
		cfg := opt.Cfg
		if cfg <= 0 {
			cfg = s.bundleParam(RoleT2IMOSS, "cfg", mossDefaultCFG)
		}
		unet := s.bundleFile(RoleT2IMOSS, "unet")
		clip := s.bundleFile(RoleT2IMOSS, "clip")
		vae := s.bundleFile(RoleT2IMOSS, "vae")
		baseURL = s.baseForModelName(unet)

		// 换侧（AGENTS §2）：MOSS 权重约 11GB，加主实例驻留的 flux 17GB = 28GB > 24GB。
		// 两者不在同一个进程，/free 是唯一能让显存真正归还的手段。
		// 失败只记日志不返回——主实例可能压根没起，那本来就不需要腾地方。
		if !strings.EqualFold(strings.TrimRight(baseURL, "/"), strings.TrimRight(s.BaseURL, "/")) {
			if err := s.UnloadModels(); err != nil {
				log.Printf("[t2i] 卸载主实例模型失败（继续：主实例可能未启动）：%v", err)
			}
		}

		cond := opt.Prompt
		if strings.TrimSpace(opt.Layout) != "" {
			cond = opt.Layout // layout JSON 本身就是条件文本，直接喂 CLIPTextEncode
		}
		workflow = BuildMOSSGraph(cond, opt.Negative, unet, clip, vae, w, h, steps, cfg, time.Now().UnixNano())
		// 50 步 @1024² 比 flux 的 4 步慢一个数量级，7 分钟会把正常的冷加载出图判成超时
		timeout = 20 * time.Minute

	default:
		ckpt := strings.TrimSpace(opt.Ckpt)
		if ckpt == "" {
			ckpt = s.Checkpoint
		}
		// 兼容矩阵预检（见 comfy_compat.go）：不兼容组合构图前显式失败
		if err := CheckCheckpointCompat(ckpt); err != nil {
			return "", err
		}
		w, h := opt.Width, opt.Height
		if w <= 0 {
			w = int(s.bundleParam(RoleT2IPrimary, "width", 1024))
		}
		if h <= 0 {
			h = int(s.bundleParam(RoleT2IPrimary, "height", 1024))
		}
		steps := opt.Steps
		if steps <= 0 {
			steps = int(s.bundleParam(RoleT2IPrimary, "steps", 4))
		}
		cfg := opt.Cfg
		if cfg <= 0 {
			cfg = s.bundleParam(RoleT2IPrimary, "cfg", 1.0)
		}
		baseURL = s.baseForModelName(ckpt)
		workflow = BuildFluxGraph(opt.Prompt, opt.Negative, ckpt, w, h, steps, cfg, time.Now().UnixNano())

		// 用户提供了模板就用模板：这样 ControlNet / LoRA / img2img / upscale
		// 等任意 ComfyUI 图都能接入，而不必改 FlowEdge 的代码
		if s.Template != "" {
			g, err := s.buildGraphFromTemplate(s.Template, opt.Prompt, opt.Negative, w, h, time.Now().UnixNano())
			if err != nil {
				return "", err
			}
			workflow = g
		}
		timeout = 7 * time.Minute
	}

	log.Printf("[t2i] backend=%s %dx%d → %s（%s）", backend, opt.Width, opt.Height, baseURL, reason)
	return s.runComfyGraph(baseURL, workflow, timeout)
}

// runComfyGraph 提交一张 ComfyUI API 图，轮询 /history，下载产物落盘。
//
// baseURL 显式传入而不是读 s.BaseURL：MOSS 跑在另一个实例的另一个端口上，
// 而这个实例是构图时才决定的。
func (s *Text2ImageService) runComfyGraph(baseURL string, workflow map[string]interface{}, timeout time.Duration) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = s.BaseURL
	}
	payload := map[string]interface{}{"prompt": workflow}
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal workflow failed: %w", err)
	}

	resp, err := s.httpClient().Post(baseURL+"/prompt", "application/json", bytes.NewBuffer(jsonBody))
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

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(1500 * time.Millisecond)

		hresp, err := s.httpClient().Get(baseURL + "/history/" + queued.PromptID)
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
			baseURL, url.QueryEscape(imgFile), url.QueryEscape(imgSub), url.QueryEscape(imgType))
		dresp, err := s.httpClient().Get(viewURL)
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

	return "", fmt.Errorf("comfyui generation timed out after %s (prompt_id=%s)", timeout, queued.PromptID)
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

// ASROptions 转写档位（节点级覆盖服务默认；空 = 用服务端配置）
type ASROptions struct {
	Model  string // faster-whisper 模型名：tiny / base / small / medium / large-v3
	Device string // cpu / cuda / auto（默认 cpu：把显存留给图像与 LLM）
}

func NewASRService(baseURL string) *ASRService {
	return &ASRService{
		BaseURL: baseURL,
		client: &http.Client{
			Timeout: 300 * time.Second, // large-v3 首次加载 + 长音频，small 的 120s 会误杀
		},
	}
}

// Transcribe 语音转文字（服务端默认档位）
func (s *ASRService) Transcribe(audioPath string) (string, error) {
	return s.TranscribeWith(audioPath, ASROptions{})
}

// TranscribeWith 语音转文字（可指定模型档位与设备）
func (s *ASRService) TranscribeWith(audioPath string, opt ASROptions) (string, error) {
	if s.BaseURL == "" {
		// 未配置必须显式失败：伪造转写文本会让用户以为真的转写成功了
		return "", fmt.Errorf("%w: ASR 服务未配置（设置 ASR_BASE_URL，或由桌面客户端自动拉起语音服务）", ErrServiceNotConfigured)
	}

	file, err := os.Open(audioPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// 简单 multipart 上传（file + 可选 model/device 文本字段）
	var buf bytes.Buffer
	buf.WriteString("--boundary\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"audio.wav\"\r\n")
	buf.WriteString("Content-Type: audio/wav\r\n\r\n")
	io.Copy(&buf, file)
	buf.WriteString("\r\n--boundary\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"model\"\r\n\r\n")
	buf.WriteString(opt.Model)
	buf.WriteString("\r\n--boundary\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"device\"\r\n\r\n")
	buf.WriteString(opt.Device)
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
			// 这里只是兜底上限，真正的超时由 synthesizeHTTP 按工作量算（见该函数注释）。
			// 旧值写死 60s，而 Audio8-TTS 0.6B 克隆模式实测 RTF 18~32（58 字台词 ≈157s），
			// 每次克隆都会超时 → 静默回退 SAPI → 用户拿到的是系统音色还以为成功了。
			Timeout: 20 * time.Minute,
		},
	}
}

// TTSOptions 语音合成参数
type TTSOptions struct {
	Voice    string  // 音色：SAPI 语音名片段 / Kokoro 音色名 / Audio8 已注册音色名
	Rate     int     // 语速 -10..10（SAPI 语义）
	Speed    float64 // 语速倍率（Kokoro /tts 的 speed 语义；>0 时才发给服务端）
	RefAudio string  // 参考音频路径：非空即启用零样本克隆（Audio8 专用）
	RefText  string  // 参考音频的逐字稿：克隆必需，与实际内容对不上会明显掉音质
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
		} else if opt.RefAudio != "" {
			// 克隆请求绝不回退 SAPI：换后端等于换了个人在说话，产出的音色与参考
			// 音频毫无关系，而节点会报「成功」。宁可显式失败，也不伪造结果。
			return "", fmt.Errorf("TTS 克隆合成失败（已指定参考音频 %s，不回退 SAPI 以免音色被悄悄换掉）：%w", opt.RefAudio, err)
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

// ttsFirstCallBudget HTTP 预算的固定部分：买「模型此刻不在显存里」这件事。
//
// Breeze TTS 2（3.47B BF16）冷加载实测 180s（2026-10-27 逐 10 秒采样确认：
// 3.0GB → 11.5GB → 19.9GB，loaded=true 恰好 180s）。旧值 60s 意味着
// 「服务刚起」或「被显存仲裁卸载后」的每一次请求都会在 HTTP 层超时，
// 而服务端其实正在正常加载——节点只看到 timeout，看不出是预算不够。
// 240s = 180s 实测 + 服务启动/首句余量。
const ttsFirstCallBudget = 240 * time.Second

// synthesizeHTTP 走本地 / 远程 TTS 服务
//
// 超时按工作量估，不用魔法常数：Audio8-TTS 0.6B 在本机实测 RTF 7.9（默认音色）/
// 18~32（克隆音色），即 1 秒音频要 8~32 秒算。所以预算 = ttsFirstCallBudget
// （覆盖冷加载 + 首帧）+ 每字 1.5s，封顶 20 分钟。60 字台词 ⇒ 约 390s，
// 同时覆盖 Breeze 的 180s 冷加载与 Audio8 克隆模式的最坏情况。
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
	// Kokoro 系服务读 speed（倍率），SAPI 读 rate（-10..10）。两个键都发，
	// 服务端认哪个用哪个—— dub-track.py 实测发的就是 speed。
	if opt.Speed > 0 {
		reqBody["speed"] = opt.Speed
	}
	// Audio8 专用：给了参考音频就走零样本克隆，优先级高于 voice
	if opt.RefAudio != "" {
		reqBody["ref_audio"] = opt.RefAudio
		if opt.RefText != "" {
			reqBody["ref_text"] = opt.RefText
		}
	}
	jsonBody, _ := json.Marshal(reqBody)

	budget := ttsFirstCallBudget + time.Duration(len([]rune(text)))*1500*time.Millisecond
	if budget > 20*time.Minute {
		budget = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/tts", bytes.NewBuffer(jsonBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("tts 请求失败（预算 %s）：%w", budget, err)
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

// Unload 请求服务端释放模型与显存（POST /unload），**进程保留**。
//
// 用途：扩散侧（H3 权重 20.5GB）要整张卡时让语音侧让位。Breeze TTS 2 驻留
// 实测 15–20GB，两者物理上装不下；不放掉它，扩散只能退化成部分加载
// （实测 87s → 2269s）或直接 failed。
//
// 选 /unload 而不是停进程：下次合成只需重新加载权重，省掉进程启动与端口探测；
// 且服务仍 Healthy，execTTSNode 不会误判成「没起」再走一遍 Start 流程。
//
// 后端不支持该端点（Kokoro / Audio8 / SAPI）时返回错误，调用方应退回停服务。
func (s *TTSService) Unload() error {
	if s.BaseURL == "" {
		return fmt.Errorf("tts service not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/unload", nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("backend does not support /unload")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unload returned status %d", resp.StatusCode)
	}
	return nil
}

// TTSVRAM TTS 服务进程侧的显存读数（/health 的 vram 字段，单位 MiB）。
//
// 为什么要这个：空闲回收器必须在**卸载之前**判断「卸完还装不装得回来」。
// Breeze 快路径要 20.3GiB 空闲才加载得动（见 tts_breeze_server.py 的预检），
// 盲目卸掉只会得到一个「loaded=false 且下次必然失败」的服务——比不卸更糟。
//
// ReservedMB 是判据而不是 AllocatedMB：caching allocator 保留的块
// （含 CUDA Graph 私有池）同样占着设备，卸载后这部分才会还给驱动。
type TTSVRAM struct {
	AllocatedMB int `json:"allocated_mb"`
	ReservedMB  int `json:"reserved_mb"`
	FreeMB      int `json:"free_mb"`
	TotalMB     int `json:"total_mb"`
}

// VRAMInfo 读 TTS 服务的显存读数。后端不提供 /health 的 vram 段时返回零值 + 错误，
// 调用方应当按「不知道」处理，而不是按「0」处理。
func (s *TTSService) VRAMInfo() (TTSVRAM, error) {
	var out TTSVRAM
	if s.BaseURL == "" {
		return out, fmt.Errorf("tts service not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/health", nil)
	if err != nil {
		return out, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("health returned status %d", resp.StatusCode)
	}
	var body struct {
		Loaded bool    `json:"loaded"`
		VRAM   TTSVRAM `json:"vram"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return out, err
	}
	return body.VRAM, nil
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

	cmd := hiddenExecContext(ctx, s.FFmpegPath, args...)
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
	cmd := hiddenExec(s.FFmpegPath,
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
