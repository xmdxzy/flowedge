package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// StreamChunk 流式输出块
type StreamChunk struct {
	Content string   `json:"content"`
	Done    bool     `json:"done"`
	Usage   LLMUsage `json:"usage,omitempty"` // Done 时随最后一块返回（需服务端支持 include_usage）
}

// LLMStreamService 流式 LLM 服务
type LLMStreamService struct {
	BaseURL string
	Model   string
	// Backend 后端类型（ollama / openai）；空 = 按 BaseURL 现场判定。见 llm_provider.go
	Backend string
	client  *http.Client
}

func (s *LLMStreamService) httpClient() *http.Client {
	if s != nil && s.client != nil {
		return s.client
	}
	return defaultLLMHTTPClient
}

func NewLLMStreamService(baseURL, model string) *LLMStreamService {
	return &LLMStreamService{
		BaseURL: baseURL,
		Model:   model,
		Backend: ResolveLLMBackend(baseURL, os.Getenv("LLM_PROVIDER")),
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// streamOptions 请求服务端在流末尾附上 usage（不支持的实现方会忽略该字段）
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type streamChatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions streamOptions `json:"stream_options"`
}

type streamChatResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage LLMUsage `json:"usage"`
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
		Model:         model,
		Messages:      []chatMessage{{Role: "user", Content: prompt}},
		Stream:        true,
		StreamOptions: streamOptions{IncludeUsage: true},
	})
}

// streamRequest 发起一次流式请求并解析 SSE
func (s *LLMStreamService) streamRequest(payload interface{}) (<-chan StreamChunk, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request failed: %w", err)
	}

	url := s.BaseURL + "/v1/chat/completions"
	resp, err := s.httpClient().Post(url, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("%s 请求失败（%s）：%w", s.BackendLabel(), url, err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s 返回 %d（%s）", s.BackendLabel(), resp.StatusCode, url)
	}

	ch := make(chan StreamChunk, 100)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		usage := LLMUsage{}
		forwardChunk := func(sc StreamChunk) { ch <- sc }

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
					forwardChunk(StreamChunk{Done: true, Usage: usage})
					return
				}
				var chunk streamChatResponse
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					continue
				}
				// 部分实现在单独的 usage-only chunk 里返回用量
				if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
					usage = usage.add(chunk.Usage)
				}
				if len(chunk.Choices) > 0 {
					content := chunk.Choices[0].Delta.Content
					if content != "" {
						forwardChunk(StreamChunk{Content: content})
					}
					if chunk.Choices[0].FinishReason == "stop" {
						forwardChunk(StreamChunk{Done: true, Usage: usage})
						return
					}
				}
			}
		}
		forwardChunk(StreamChunk{Done: true, Usage: usage})
	}()

	return ch, nil
}
