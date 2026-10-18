package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

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
}

func (s *LLMService) Chat(prompt string) (string, error) {
	reqBody := chatRequest{
		Model: s.Model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request failed: %w", err)
	}

	url := s.BaseURL + "/v1/chat/completions"
	resp, err := s.client.Post(url, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("decode response failed: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("empty response from ollama")
	}

	return chatResp.Choices[0].Message.Content, nil
}
