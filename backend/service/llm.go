package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
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
