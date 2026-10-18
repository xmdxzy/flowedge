package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// StreamChunk 流式输出块
type StreamChunk struct {
	Content string `json:"content"`
	Done    bool   `json:"done"`
}

// LLMStreamService 流式 LLM 服务
type LLMStreamService struct {
	BaseURL string
	Model   string
	client  *http.Client
}

func NewLLMStreamService(baseURL, model string) *LLMStreamService {
	return &LLMStreamService{
		BaseURL: baseURL,
		Model:   model,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

type streamChatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type streamChatResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// ChatStream 流式聊天，通过 channel 逐块返回
func (s *LLMStreamService) ChatStream(prompt string) (<-chan StreamChunk, error) {
	return s.ChatStreamWithModel(prompt, "")
}

// ChatStreamWithModel 流式聊天（model 为空回落服务级默认）
func (s *LLMStreamService) ChatStreamWithModel(prompt, model string) (<-chan StreamChunk, error) {
	if model == "" {
		model = s.Model
	}
	return s.streamRequest(streamChatRequest{
		Model:    model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
		Stream:   true,
	})
}

// ChatStreamMessages 带完整消息历史（含工具调用/工具结果）的流式补全，
// 用于工具调用循环的最终答案轮——前面几轮要拿到完整 tool_calls 所以走非流式
func (s *LLMStreamService) ChatStreamMessages(messages []toolChatMessage, model string) (<-chan StreamChunk, error) {
	if model == "" {
		model = s.Model
	}
	return s.streamRequest(streamMessagesRequest{
		Model:    model,
		Messages: messages,
		Stream:   true,
	})
}

// streamMessagesRequest 带工具消息历史的流式请求体
type streamMessagesRequest struct {
	Model    string            `json:"model"`
	Messages []toolChatMessage `json:"messages"`
	Stream   bool              `json:"stream"`
}

// streamRequest 发起一次流式请求并解析 SSE
func (s *LLMStreamService) streamRequest(payload interface{}) (<-chan StreamChunk, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request failed: %w", err)
	}

	url := s.BaseURL + "/v1/chat/completions"
	resp, err := s.client.Post(url, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("ollama request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	ch := make(chan StreamChunk, 100)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			// SSE 格式：data: {...}
			if len(line) > 6 && line[:6] == "data: " {
				data := line[6:]
				if data == "[DONE]" {
					ch <- StreamChunk{Done: true}
					return
				}
				var chunk streamChatResponse
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					continue
				}
				if len(chunk.Choices) > 0 {
					content := chunk.Choices[0].Delta.Content
					if content != "" {
						ch <- StreamChunk{Content: content}
					}
					if chunk.Choices[0].FinishReason == "stop" {
						ch <- StreamChunk{Done: true}
						return
					}
				}
			}
		}
		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}
