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
	client  *http.Client
}

func NewLLMService(baseURL, model string) *LLMService {
	return &LLMService{
		BaseURL: baseURL,
		Model:   model,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
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
func (s *LLMService) ChatWithModelUsage(prompt, model string) (string, LLMUsage, error) {
	if model == "" {
		model = s.Model
	}

	reqBody := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("marshal request failed: %w", err)
	}

	url := s.BaseURL + "/v1/chat/completions"
	resp, err := s.client.Post(url, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", LLMUsage{}, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", LLMUsage{}, fmt.Errorf("decode response failed: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", LLMUsage{}, fmt.Errorf("empty response from ollama")
	}

	return chatResp.Choices[0].Message.Content, chatResp.Usage, nil
}

// UnloadModel 让 Ollama 把模型从显存里卸掉。
//
// 用 /api/generate + keep_alive:0（Ollama 文档做法）比 OpenAI 兼容端点可靠：
// 兼容端点不一定透传 keep_alive。请求本身 1 秒内返回。
func (s *LLMService) UnloadModel(model string) error {
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
	resp, err := s.client.Post(url, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, LLMUsage{}, fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, LLMUsage{}, fmt.Errorf("ollama returned status %d", resp.StatusCode)
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
