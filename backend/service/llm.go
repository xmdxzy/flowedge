package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LLMUsage 一次补全的 token 消耗（Ollama OpenAI 兼容端点的 usage 字段）
type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u LLMUsage) add(o LLMUsage) LLMUsage {
	return LLMUsage{
		PromptTokens:     u.PromptTokens + o.PromptTokens,
		CompletionTokens: u.CompletionTokens + o.CompletionTokens,
	}
}

type LLMService struct {
	BaseURL string
	Model   string
	// Backend 后端类型（ollama / openai）。空 = 按 BaseURL 与 LLM_PROVIDER 现场判定，
	// 这样老的 NewLLMService 调用路径不用改。见 llm_provider.go。
	Backend string
	client  *http.Client
}

// defaultLLMHTTPClient LLM 兜底客户端（长上下文/慢生成需要比默认更宽的窗口）。
// 与 Text2ImageService.httpClient 同一动机：字面量构造时 client 为 nil 会 panic，
// 装配失误应该退化成可用行为而不是崩进程。
var defaultLLMHTTPClient = &http.Client{Timeout: 300 * time.Second}

func (s *LLMService) httpClient() *http.Client {
	if s != nil && s.client != nil {
		return s.client
	}
	return defaultLLMHTTPClient
}

func NewLLMService(baseURL, model string) *LLMService {
	return &LLMService{
		BaseURL: baseURL,
		Model:   model,
		Backend: ResolveLLMBackend(baseURL, os.Getenv("LLM_PROVIDER")),
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// Temperature 用指针：nil = 不下发，交给服务端默认（保留既有行为）。
	// 引擎内部的「文本变换」类调用（翻译 / 提示词优化 / 场景拆解）显式传 0，
	// 这类调用不掺随机性才能复现、才能安全缓存。
	Temperature *float64 `json:"temperature,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage LLMUsage `json:"usage"`
}

func (s *LLMService) Chat(prompt string) (string, error) {
	return s.ChatWithModel(prompt, "")
}

// ChatWithModel 执行一次补全；model 为空时回落到服务级默认模型
func (s *LLMService) ChatWithModel(prompt, model string) (string, error) {
	text, _, err := s.ChatWithModelUsage(prompt, model)
	return text, err
}

// ChatWithModelUsage 与 ChatWithModel 相同，额外返回 token 用量（运行级统计用）
//
// 调用方若需要可复现的输出（翻译 / 优化 / 拆解这类纯文本变换），
// 用 e.deterministicChat：temperature=0 + 结果备忘，不要直接走这里。
func (s *LLMService) ChatWithModelUsage(prompt, model string) (string, LLMUsage, error) {
	return s.chatWithOptions(prompt, model, nil)
}

// ChatWithTemperature 指定温度的补全；temp < 0 表示不下发（服务端默认）
func (s *LLMService) ChatWithTemperature(prompt, model string, temp float64) (string, error) {
	text, _, err := s.chatWithOptions(prompt, model, &temp)
	return text, err
}

func (s *LLMService) chatWithOptions(prompt, model string, temp *float64) (string, LLMUsage, error) {
	if model == "" {
		model = s.Model
	}

	reqBody := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Stream:      false,
		Temperature: temp,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("marshal request failed: %w", err)
	}

	url := s.BaseURL + "/v1/chat/completions"
	resp, err := s.httpClient().Post(url, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("%s 请求失败（%s）：%w", s.BackendLabel(), url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", LLMUsage{}, fmt.Errorf("%s 返回 %d（%s）", s.BackendLabel(), resp.StatusCode, url)
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", LLMUsage{}, fmt.Errorf("decode response failed: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", LLMUsage{}, fmt.Errorf("%s 返回空内容（模型 %s）", s.BackendLabel(), model)
	}

	return chatResp.Choices[0].Message.Content, chatResp.Usage, nil
}

// UnloadModel 让 Ollama 把模型从显存里卸掉。
//
// 用 /api/generate + keep_alive:0（Ollama 文档做法）比 OpenAI 兼容端点可靠：
// 兼容端点不一定透传 keep_alive。请求本身 1 秒内返回。
func (s *LLMService) UnloadModel(model string) error {
	if !s.SupportsNativeUnload() {
		// 不是故障，是能力边界：OpenAI 兼容后端（vLLM / LM Studio / 商业端点）
		// 没有「从外部卸载」这个动作，显存由它自己管。
		return unloadUnsupportedError{backend: s.backend()}
	}
	if model == "" {
		model = s.Model
	}
	body := map[string]interface{}{
		"model":      model,
		"keep_alive": 0,
	}
	b, _ := json.Marshal(body)
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Post(s.BaseURL+"/api/generate", "application/json", bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama unload status %d", resp.StatusCode)
	}
	return nil
}

// toolChatMessage OpenAI 兼容消息（支持工具调用与工具结果回填）
type toolChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolChatRequest struct {
	Model    string            `json:"model"`
	Messages []toolChatMessage `json:"messages"`
	Tools    []ToolSpec        `json:"tools,omitempty"`
	Stream   bool              `json:"stream"`
}

type toolChatResponse struct {
	Choices []struct {
		Message      toolChatMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage LLMUsage `json:"usage"`
}

// ChatWithTools 一次带工具声明的补全（非流式），返回 assistant 消息
// （工具轮需要完整的 tool_calls，流式下会被拆成碎片，所以这里不用流式）
func (s *LLMService) ChatWithTools(messages []toolChatMessage, tools []ToolSpec, model string) (*toolChatMessage, error) {
	msg, _, err := s.ChatWithToolsUsage(messages, tools, model)
	return msg, err
}

// ChatWithToolsUsage 与 ChatWithTools 相同，额外返回 token 用量
func (s *LLMService) ChatWithToolsUsage(messages []toolChatMessage, tools []ToolSpec, model string) (*toolChatMessage, LLMUsage, error) {
	if model == "" {
		model = s.Model
	}

	reqBody := toolChatRequest{
		Model:    model,
		Messages: messages,
		Tools:    tools,
		Stream:   false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, LLMUsage{}, fmt.Errorf("marshal request failed: %w", err)
	}

	url := s.BaseURL + "/v1/chat/completions"
	resp, err := s.httpClient().Post(url, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, LLMUsage{}, fmt.Errorf("%s 请求失败（%s）：%w", s.BackendLabel(), url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, LLMUsage{}, fmt.Errorf("%s 返回 %d（%s）", s.BackendLabel(), resp.StatusCode, url)
	}

	var out toolChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, LLMUsage{}, fmt.Errorf("decode response failed: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, LLMUsage{}, fmt.Errorf("empty response from model")
	}
	msg := out.Choices[0].Message
	return &msg, out.Usage, nil
}

// ===== 视觉理解（VLM）=====
//
// 走 Ollama 原生 /api/chat（只有它接受 images 数组），而不是 OpenAI 兼容端点。
// 图片以 base64（不带 data: 前缀）随消息发送，单图上限 8MB——本地视觉模型
// 对超大图只会更慢，不会更准，先挡在入口比让用户等十分钟更诚实。

const maxVisionImageBytes = 8 << 20

type ollamaChatMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type ollamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
}

type ollamaChatResponse struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// VisionChat 图文问答：prompt + 若干本地图片路径
//
// 两种后端的图片传法不同，必须分开：
//   - ollama：原生 /api/chat 的 images 数组（base64 不带前缀）
//   - openai 兼容（vLLM / LM Studio / llama.cpp）：content 数组里的 image_url
//     （data: URI，OpenAI 标准）。用 ollama 格式去打这些后端会 400。
func (s *LLMService) VisionChat(prompt string, imagePaths []string, model string) (string, LLMUsage, error) {
	if model == "" {
		model = s.Model
	}
	if len(imagePaths) == 0 {
		return "", LLMUsage{}, fmt.Errorf("vision node has no image input")
	}

	images := make([]string, 0, len(imagePaths))
	for _, p := range imagePaths {
		st, err := os.Stat(p)
		if err != nil {
			return "", LLMUsage{}, fmt.Errorf("图片不存在：%s", p)
		}
		if st.Size() > maxVisionImageBytes {
			return "", LLMUsage{}, fmt.Errorf("图片过大（%.1fMB > 8MB）：%s", float64(st.Size())/(1<<20), filepath.Base(p))
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return "", LLMUsage{}, fmt.Errorf("读取图片失败：%w", err)
		}
		images = append(images, base64.StdEncoding.EncodeToString(raw))
	}

	if s.backend() == BackendOpenAI {
		return s.visionChatOpenAICompatible(prompt, imagePaths, images, model)
	}

	reqBody := ollamaChatRequest{
		Model: model,
		Messages: []ollamaChatMessage{
			{Role: "user", Content: prompt, Images: images},
		},
		Stream: false,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", LLMUsage{}, err
	}

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Post(s.BaseURL+"/api/chat", "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("ollama vision request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return "", LLMUsage{}, fmt.Errorf("ollama vision status %d: %s", resp.StatusCode, string(body))
	}

	var out ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", LLMUsage{}, fmt.Errorf("decode vision response failed: %w", err)
	}
	if strings.TrimSpace(out.Message.Content) == "" {
		return "", LLMUsage{}, fmt.Errorf("视觉模型返回空内容（模型 %s 可能不支持图片输入）", model)
	}
	return out.Message.Content, LLMUsage{PromptTokens: out.PromptEvalCount, CompletionTokens: out.EvalCount}, nil
}

// visionChatOpenAICompatible OpenAI 标准的多模态消息（content 数组 + image_url）
func (s *LLMService) visionChatOpenAICompatible(prompt string, paths, images []string, model string) (string, LLMUsage, error) {
	type imageURL struct {
		URL string `json:"url"`
	}
	type contentPart struct {
		Type     string    `json:"type"`
		Text     string    `json:"text,omitempty"`
		ImageURL *imageURL `json:"image_url,omitempty"`
	}
	type msg struct {
		Role    string        `json:"role"`
		Content []contentPart `json:"content"`
	}
	parts := make([]contentPart, 0, len(images)+1)
	parts = append(parts, contentPart{Type: "text", Text: prompt})
	for i, b64 := range images {
		parts = append(parts, contentPart{
			Type:     "image_url",
			ImageURL: &imageURL{URL: "data:" + imageMIME(paths[i]) + ";base64," + b64},
		})
	}
	body := map[string]interface{}{
		"model":    model,
		"messages": []msg{{Role: "user", Content: parts}},
		"stream":   false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", LLMUsage{}, err
	}
	url := s.BaseURL + "/v1/chat/completions"
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(raw))
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("%s 视觉请求失败（%s）：%w", s.BackendLabel(), url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return "", LLMUsage{}, fmt.Errorf("%s 视觉接口返回 %d：%s", s.BackendLabel(), resp.StatusCode, string(b))
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", LLMUsage{}, fmt.Errorf("解析视觉响应失败：%w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", LLMUsage{}, fmt.Errorf("视觉模型返回空内容（模型 %s 可能不支持图片输入）", model)
	}
	return out.Choices[0].Message.Content, out.Usage, nil
}

// imageMIME 按扩展名推断 MIME。猜错只会让个别后端拒图，
// 因此默认给 image/png（本地出图绝大多数是 png）。
func imageMIME(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

// visionModelHints 常见视觉模型的命名特征（用于自动挑选）
var visionModelHints = []string{"vl", "vision", "llava", "minicpm-v", "gemma3", "qwen2.5vl", "qwen3vl", "moondream", "internvl"}

// PickVisionModel 从本地已装模型里挑一个能看图的
func PickVisionModel(ollamaBase string) string {
	body, err := httpGetBody(ollamaBase + "/api/tags")
	if err != nil {
		return ""
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &tags) != nil {
		return ""
	}
	for _, m := range tags.Models {
		lower := strings.ToLower(m.Name)
		for _, h := range visionModelHints {
			if strings.Contains(lower, h) {
				return m.Name
			}
		}
	}
	return ""
}

// ModelSizeMB 模型在磁盘上的体积（MB）；查不到返回 0。
//
// 只当作显存占用的**下界**：真正加载还要加 KV cache（随上下文长度增长）。
// 所以任何"装不装得下"的判断都必须在这个数字上留余量，不能直接比。
func (s *LLMService) ModelSizeMB(model string) int {
	if s == nil || s.BaseURL == "" || strings.TrimSpace(model) == "" {
		return 0
	}
	body, err := httpGetBody(s.BaseURL + "/api/tags")
	if err != nil {
		return 0
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &tags) != nil {
		return 0
	}
	base := strings.SplitN(model, ":", 2)[0]
	for _, m := range tags.Models {
		// 精确匹配优先；退一步按「不带 tag 的名字」匹配（qwen2.5 → qwen2.5:latest）
		if m.Name == model || strings.SplitN(m.Name, ":", 2)[0] == base {
			if m.Size <= 0 {
				return 0
			}
			return int(m.Size / (1024 * 1024))
		}
	}
	return 0
}

// LoadedModels 列出当前驻留在显存/内存里的模型（Ollama /api/ps）
func (s *LLMService) LoadedModels() []string {
	body, err := httpGetBody(s.BaseURL + "/api/ps")
	if err != nil {
		return nil
	}
	var ps struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size_vram"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &ps) != nil {
		return nil
	}
	out := make([]string, 0, len(ps.Models))
	for _, m := range ps.Models {
		out = append(out, m.Name)
	}
	return out
}

// UnloadAllLoaded 把 Ollama 里所有已驻留模型卸掉（keep_alive=0）。
//
// 为什么需要：3090 只有 24GB，flux 一类扩散模型就要 17GB，
// 再叠一个 14B LLM 立刻 OOM。执行前按需卸载，比事后 OOM 重试省得多。
func (s *LLMService) UnloadAllLoaded() int {
	names := s.LoadedModels()
	n := 0
	for _, name := range names {
		if err := s.UnloadModel(name); err == nil {
			n++
		}
	}
	return n
}
