package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

func newCompatRouter(h *OpenAICompatHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/images/generations", h.ImagesGenerations)
	r.POST("/v1/audio/speech", h.AudioSpeech)
	r.POST("/v1/chat/completions", h.ChatCompletions)
	r.POST("/v1/audio/transcriptions", h.AudioTranscriptions)
	return r
}

func postJSON(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestImagesGenerations_ValidatesInput 参数错误必须是 OpenAI 错误体，
// 否则客户端 SDK 解析不了（只会抛一个空的 HTTP 400）。
func TestImagesGenerations_ValidatesInput(t *testing.T) {
	h := NewOpenAICompatHandler(&service.Text2ImageService{BaseURL: "http://127.0.0.1:1"}, nil, nil, nil, nil, t.TempDir())
	r := newCompatRouter(h)

	cases := []string{
		`{}`,
		`{"prompt":""}`,
		`{"prompt":"cat","n":99}`,
		`{"prompt":"cat","size":"big"}`,
		`{"prompt":"cat","size":"1024"}`,
		`{"prompt":"cat","response_format":"webp"}`,
	}
	for _, body := range cases {
		w := postJSON(r, "/v1/images/generations", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, w.Code)
			continue
		}
		var resp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Errorf("body %s: response is not JSON: %s", body, w.Body.String())
			continue
		}
		if resp.Error.Message == "" {
			t.Errorf("body %s: error.message must be non-empty, got %s", body, w.Body.String())
		}
	}
}

// TestAudioSpeech_ValidatesInput 空 input 与不支持的格式都必须显式拒绝
func TestAudioSpeech_ValidatesInput(t *testing.T) {
	h := NewOpenAICompatHandler(nil, nil, service.NewTTSService(""), nil, nil, t.TempDir())
	r := newCompatRouter(h)
	if w := postJSON(r, "/v1/audio/speech", `{"input":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty input: status = %d, want 400", w.Code)
	}
	if w := postJSON(r, "/v1/audio/speech", `{"input":"hi","response_format":"flac"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unsupported format: status = %d, want 400", w.Code)
	}
}

// TestAudioTranscriptions_RequiresASR 未配置 ASR 时必须 503 并说清怎么配，
// 不能返回空文本（那会让调用方以为「音频里没人说话」）。
func TestAudioTranscriptions_RequiresASR(t *testing.T) {
	h := NewOpenAICompatHandler(nil, service.NewASRService(""), nil, nil, nil, t.TempDir())
	r := newCompatRouter(h)
	w := postJSON(r, "/v1/audio/transcriptions", `{}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ASR_BASE_URL") {
		t.Errorf("error must say how to configure it: %s", w.Body.String())
	}
}

// TestChatCompletions_NonStreamShape 非流式响应必须是标准 chat.completion 结构
func TestChatCompletions_NonStreamShape(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"你好"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	defer up.Close()

	llm := &service.LLMService{BaseURL: up.URL, Model: "local-model", Backend: service.BackendOpenAI}
	h := NewOpenAICompatHandler(nil, nil, nil, llm, nil, t.TempDir())
	r := newCompatRouter(h)

	w := postJSON(r, "/v1/chat/completions", `{"model":"local-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 {
		t.Fatalf("bad shape: %s", w.Body.String())
	}
	if resp.Choices[0].Message.Content != "你好" || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("bad choice: %+v", resp.Choices[0])
	}
	if resp.Usage.TotalTokens != 12 {
		t.Errorf("usage.total_tokens = %d, want 12", resp.Usage.TotalTokens)
	}
}

// TestChatCompletions_StreamShape SSE 必须含 role 首块、content 增量、finish 与 [DONE]
func TestChatCompletions_StreamShape(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	llm := &service.LLMService{BaseURL: up.URL, Model: "m", Backend: service.BackendOpenAI}
	llms := &service.LLMStreamService{BaseURL: up.URL, Model: "m", Backend: service.BackendOpenAI}
	h := NewOpenAICompatHandler(nil, nil, nil, llm, llms, t.TempDir())
	r := newCompatRouter(h)

	w := postJSON(r, "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Errorf("missing role chunk: %s", body)
	}
	if !strings.Contains(body, `"content":"你"`) || !strings.Contains(body, `"content":"好"`) {
		t.Errorf("missing content deltas: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("must end with [DONE]: %s", body)
	}
}

// TestFlattenMessages 多轮 history 不能被丢掉（丢掉 = 模型失忆）
func TestFlattenMessages(t *testing.T) {
	type m = struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	msgs := []m{
		{Role: "system", Content: json.RawMessage(`"只回答中文"`)},
		{Role: "user", Content: json.RawMessage(`"你是谁"`)},
		{Role: "assistant", Content: json.RawMessage(`"我是助手"`)},
		{Role: "user", Content: json.RawMessage(`"继续"`)},
	}
	got, err := flattenMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"只回答中文", "你是谁", "我是助手", "继续"} {
		if !strings.Contains(got, want) {
			t.Errorf("flattened prompt lost %q: %s", want, got)
		}
	}
}

// TestExtractContent_MultimodalArray OpenAI 多模态数组里的 text 部分必须被取出
func TestExtractContent_MultimodalArray(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"描述这张图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,xx"}}]`)
	got, err := extractContent(raw)
	if err != nil || got != "描述这张图" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if _, err := extractContent(json.RawMessage(`123`)); err == nil {
		t.Error("non-string/non-array content must error explicitly")
	}
}
