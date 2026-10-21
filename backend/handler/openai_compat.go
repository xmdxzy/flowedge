package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// OpenAI 兼容出口（/v1/*）
//
// 这是「生态广度」里成本最低、见效最快的一个接入点：
// 任何支持 OpenAI API 的客户端（Open WebUI / SillyTavern / Chatbox /
// LangChain / LlamaIndex / 各类 SDK）**零改动**就能用本地 GPU 出图、转写、配音。
//
// 边界（必须写清，否则会被误解成「FlowEdge 就是个模型服务器」）：
//   - 这四个端点只调底层单一能力，不做 DAG 编排。
//     需要工作流编排的调用方走 POST /api/workflows/:id/run。
//   - 它们不经过显存仲裁（那些逻辑在引擎节点里）。所以与编排任务并跑时
//     仍可能互相挤显存——这是刻意的取舍：协议端点要能当"无状态服务器"用。
//   - /v1/audio/speech 只原生出 wav。要 mp3/opus 必须有本机 ffmpeg，否则显式报错，
//     绝不返回一个扩展名是 mp3 实际是 wav 的文件。
type OpenAICompatHandler struct {
	T2I       *service.Text2ImageService
	ASR       *service.ASRService
	TTS       *service.TTSService
	LLM       *service.LLMService
	LLMStream *service.LLMStreamService
	OutputDir string
}

func NewOpenAICompatHandler(t2i *service.Text2ImageService, asr *service.ASRService,
	tts *service.TTSService, llm *service.LLMService, lls *service.LLMStreamService, outputDir string) *OpenAICompatHandler {
	if outputDir == "" {
		outputDir = "data/generated"
	}
	return &OpenAICompatHandler{T2I: t2i, ASR: asr, TTS: tts, LLM: llm, LLMStream: lls, OutputDir: outputDir}
}

// openAIError 统一错误体：客户端 SDK 靠这个结构解析错误
func openAIError(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": gin.H{
		"message": msg,
		"type":    "invalid_request_error",
		"code":    status,
	}})
}

// ---------- /v1/images/generations ----------

type imageGenRequest struct {
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Model          string `json:"model"`
	ResponseFormat string `json:"response_format"`
	Seed           int64  `json:"seed"`
}

// ImagesGenerations 兼容 OpenAI 文生图
//
// 只支持单 prompt × n 张（OpenAI 语义）。要「多条不同 prompt 一次加载」
// 走工作流的 text2image 节点（那边有多提示词批量）。
func (h *OpenAICompatHandler) ImagesGenerations(c *gin.Context) {
	if h.T2I == nil {
		openAIError(c, http.StatusServiceUnavailable, "文生图服务未初始化")
		return
	}
	var req imageGenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		openAIError(c, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		openAIError(c, http.StatusBadRequest, "prompt 不能为空")
		return
	}
	n := req.N
	if n <= 0 {
		n = 1
	}
	if n > 8 {
		// 上限来自 ComfyUI batch_size 的实用区间：再大只会 OOM
		openAIError(c, http.StatusBadRequest, fmt.Sprintf("n 最大 8（当前 %d）", n))
		return
	}
	width, height := 1024, 1024
	if req.Size != "" {
		parts := strings.SplitN(strings.ToLower(req.Size), "x", 2)
		if len(parts) != 2 {
			openAIError(c, http.StatusBadRequest, "size 格式应为 WxH（如 1024x1024）")
			return
		}
		w, err1 := strconv.Atoi(parts[0])
		hh, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || w <= 0 || hh <= 0 {
			openAIError(c, http.StatusBadRequest, "size 里的宽高必须是正整数")
			return
		}
		width, height = w, hh
	}

	// 格式先校验，再出图。
	// 顺序很关键：先跑 GenerateImages 再发现 response_format 不合法，
	// 等于白烧几十秒 GPU 才告诉用户参数错了。
	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format == "" {
		format = "b64_json" // 本地部署没有公网地址，默认内联比给 url 实用
	}
	if format != "b64_json" && format != "url" {
		openAIError(c, http.StatusBadRequest, "response_format 只支持 b64_json 或 url")
		return
	}

	paths, err := h.T2I.GenerateImages(req.Prompt, width, height, n)
	if err != nil {
		openAIError(c, http.StatusBadGateway, "出图失败："+err.Error())
		return
	}

	data := make([]gin.H, 0, len(paths))
	for _, p := range paths {
		switch format {
		case "b64_json":
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				openAIError(c, http.StatusInternalServerError, "读取产物失败："+rerr.Error())
				return
			}
			data = append(data, gin.H{"b64_json": base64.StdEncoding.EncodeToString(raw)})
		case "url":
			// 指向本机 /api/artifacts/<文件名>，调用方拿到的是可下载的绝对路径
			scheme := "http"
			if c.Request.TLS != nil {
				scheme = "https"
			}
			data = append(data, gin.H{"url": fmt.Sprintf("%s://%s/api/artifacts/%s",
				scheme, c.Request.Host, filepath.Base(p))})
		}
	}
	c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": data})
}

// ---------- /v1/audio/transcriptions ----------

// AudioTranscriptions 兼容 OpenAI 语音转写（multipart/form-data: file）
func (h *OpenAICompatHandler) AudioTranscriptions(c *gin.Context) {
	if h.ASR == nil || h.ASR.BaseURL == "" {
		openAIError(c, http.StatusServiceUnavailable,
			"ASR 服务未配置（设置 ASR_BASE_URL，或启动 local-services 后设 ASR_BASE_URL=http://127.0.0.1:8101）")
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		openAIError(c, http.StatusBadRequest, "缺少 file 字段（multipart/form-data）")
		return
	}
	// 不允许客户端指定落盘路径：临时目录 + 固定扩展名
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext == "" {
		ext = ".wav"
	}
	tmp, err := os.CreateTemp("", "flowedge-asr-*"+ext)
	if err != nil {
		openAIError(c, http.StatusInternalServerError, "创建临时文件失败："+err.Error())
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	src, err := file.Open()
	if err != nil {
		tmp.Close()
		openAIError(c, http.StatusBadRequest, "读取上传文件失败："+err.Error())
		return
	}
	if _, err := io.Copy(tmp, src); err != nil {
		src.Close()
		tmp.Close()
		openAIError(c, http.StatusInternalServerError, "写入临时文件失败："+err.Error())
		return
	}
	src.Close()
	tmp.Close()

	opt := service.ASROptions{
		Model:  c.PostForm("model"),
		Device: c.PostForm("device"),
	}
	// OpenAI 的 whisper-1 是"模型名"，本地 faster-whisper 用档位名（tiny/base/small/medium/large-v3）。
	// 直接把 whisper-1 传下去会让 service 拒绝——这里做一次显式映射而不是静默忽略。
	if opt.Model == "whisper-1" || opt.Model == "" {
		opt.Model = ""
	}
	text, err := h.ASR.TranscribeWith(tmpName, opt)
	if err != nil {
		openAIError(c, http.StatusBadGateway, "转写失败："+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"text": text})
}

// ---------- /v1/audio/speech ----------

type speechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

// AudioSpeech 兼容 OpenAI 语音合成，返回二进制音频
func (h *OpenAICompatHandler) AudioSpeech(c *gin.Context) {
	if h.TTS == nil {
		openAIError(c, http.StatusServiceUnavailable, "TTS 服务未初始化")
		return
	}
	var req speechRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		openAIError(c, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		openAIError(c, http.StatusBadRequest, "input 不能为空")
		return
	}
	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format == "" {
		format = "wav"
	}
	if format != "wav" && format != "mp3" {
		openAIError(c, http.StatusBadRequest, "response_format 只支持 wav 或 mp3（本地后端原生为 wav）")
		return
	}

	// Voice 语义映射：OpenAI 的 alloy/echo/fable… 在本地没有对应音色。
	// 不假装支持——原样交给 TTS 服务，服务认不出就用系统默认并在响应头里说明。
	opt := service.TTSOptions{Voice: req.Voice}
	if req.Speed > 0 {
		// SAPI 的 Rate 是 -10..10；OpenAI 的 speed 是 0.25..4.0 倍速
		rate := int((req.Speed - 1.0) * 10)
		if rate > 10 {
			rate = 10
		}
		if rate < -10 {
			rate = -10
		}
		opt.Rate = rate
	}
	if err := os.MkdirAll(h.OutputDir, 0o755); err != nil {
		openAIError(c, http.StatusInternalServerError, "创建输出目录失败："+err.Error())
		return
	}
	wavPath := filepath.Join(h.OutputDir, fmt.Sprintf("tts-%d.wav", time.Now().UnixNano()))
	backend, err := h.TTS.SynthesizeWith(req.Input, wavPath, opt)
	if err != nil {
		openAIError(c, http.StatusBadGateway, "合成失败："+err.Error())
		return
	}
	c.Header("X-FlowEdge-TTS-Backend", backend)
	defer os.Remove(wavPath) // 端点语义是无状态：给完就删

	if format == "wav" {
		c.Header("Content-Type", "audio/wav")
		c.File(wavPath)
		return
	}
	// mp3：必须有 ffmpeg。缺了就显式失败——不能把 wav 冒充成 mp3。
	ffmpeg, lookErr := exec.LookPath("ffmpeg")
	if lookErr != nil {
		openAIError(c, http.StatusNotImplemented,
			"response_format=mp3 需要本机 ffmpeg（当前未找到）。改用 wav，或安装 ffmpeg 后重试")
		return
	}
	mp3Path := strings.TrimSuffix(wavPath, ".wav") + ".mp3"
	defer os.Remove(mp3Path)
	out, runErr := exec.Command(ffmpeg, "-y", "-loglevel", "error", "-i", wavPath, "-codec:a", "libmp3lame", "-q:a", "4", mp3Path).CombinedOutput()
	if runErr != nil {
		openAIError(c, http.StatusInternalServerError,
			"ffmpeg 转码失败："+runErr.Error()+" "+truncateStr(string(out), 200))
		return
	}
	c.Header("Content-Type", "audio/mpeg")
	c.File(mp3Path)
}

// ---------- /v1/chat/completions ----------

type compatChatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream      bool     `json:"stream"`
	Temperature *float64 `json:"temperature"`
	MaxTokens   int      `json:"max_tokens"`
}

// ChatCompletions 兼容 OpenAI 对话补全（含 SSE 流式）
//
// 这是让 FlowEdge 出现在「模型列表」里的那一个端点：Open WebUI 之类只要把
// Base URL 指到 http://127.0.0.1:8080/v1 就能用它对话。
func (h *OpenAICompatHandler) ChatCompletions(c *gin.Context) {
	if h.LLM == nil {
		openAIError(c, http.StatusServiceUnavailable, "LLM 服务未初始化")
		return
	}
	var req compatChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		openAIError(c, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		openAIError(c, http.StatusBadRequest, "messages 不能为空")
		return
	}
	prompt, err := flattenMessages(req.Messages)
	if err != nil {
		openAIError(c, http.StatusBadRequest, err.Error())
		return
	}
	model := req.Model
	created := time.Now().Unix()
	completionID := fmt.Sprintf("chatcmpl-flowedge-%d", time.Now().UnixNano())

	if !req.Stream {
		content, usage, cerr := h.LLM.ChatWithModelUsage(prompt, model)
		if cerr != nil {
			openAIError(c, http.StatusBadGateway, cerr.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"id":      completionID,
			"object":  "chat.completion",
			"created": created,
			"model":   firstNonEmpty(model, h.LLM.Model),
			"choices": []gin.H{{
				"index":         0,
				"message":       gin.H{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": gin.H{
				"prompt_tokens":     usage.PromptTokens,
				"completion_tokens": usage.CompletionTokens,
				"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
			},
		})
		return
	}

	// 流式：SSE。首块发 role（OpenAI 客户端依赖它建立消息），中间发 delta，
	// 末尾发 finish_reason + [DONE]。
	if h.LLMStream == nil {
		openAIError(c, http.StatusServiceUnavailable, "流式服务未初始化")
		return
	}
	ch, serr := h.LLMStream.ChatStreamWithModel(prompt, model)
	if serr != nil {
		openAIError(c, http.StatusBadGateway, serr.Error())
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)

	send := func(payload gin.H) {
		raw, _ := json.Marshal(payload)
		fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	base := func(delta gin.H, finish interface{}) gin.H {
		return gin.H{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   firstNonEmpty(model, h.LLM.Model),
			"choices": []gin.H{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	send(base(gin.H{"role": "assistant", "content": ""}, nil))
	for chunk := range ch {
		if chunk.Content != "" {
			send(base(gin.H{"content": chunk.Content}, nil))
		}
		if chunk.Done {
			break
		}
	}
	send(base(gin.H{}, "stop"))
	fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// flattenMessages 把 OpenAI 的 messages 压成单条 prompt。
//
// 为什么不保留多轮结构：底层 ChatWithModelUsage 只接受单 prompt
// （多轮在引擎里由 llm 节点的 context 维护）。协议端点必须诚实——
// 把 history 拼成显式分段，比丢掉它让模型失忆要好。
func flattenMessages(msgs []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}) (string, error) {
	var b strings.Builder
	for _, m := range msgs {
		text, err := extractContent(m.Content)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		switch m.Role {
		case "system":
			b.WriteString("系统指令：" + text + "\n\n")
		case "assistant":
			b.WriteString("助手：" + text + "\n\n")
		case "user", "":
			b.WriteString(text + "\n\n")
		default:
			b.WriteString(m.Role + "：" + text + "\n\n")
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("messages 里没有可用文本内容")
	}
	return out, nil
}

// extractContent content 可能是字符串，也可能是 OpenAI 多模态数组
func extractContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("content 既不是字符串也不是多模态数组")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
