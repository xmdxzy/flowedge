package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

	reqBody := streamChatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: true,
	}

	jsonBody, err := json.Marshal(reqBody)
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

		// 流结束了
		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

// ReadAll 读取所有流式内容并拼接
func (s *LLMStreamService) ReadAll(prompt string) (string, error) {
	ch, err := s.ChatStream(prompt)
	if err != nil {
		return "", err
	}

	var result string
	for chunk := range ch {
		if chunk.Done {
			break
		}
		result += chunk.Content
	}

	return result, nil
}

// 确保 io.Reader 被引用
var _ io.Reader = (*bytes.Buffer)(nil)
