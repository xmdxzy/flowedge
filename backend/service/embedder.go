package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// EmbedBackend 标识 embedding 后端
type EmbedBackend string

const (
	EmbedBackendOllama EmbedBackend = "ollama"
	EmbedBackendSimple EmbedBackend = "simple"
)

// Embedder 文本向量化器
// 主通道：Ollama /api/embeddings（语义向量，如 bge-m3）
// 降级通道：SimpleEmbed（词面哈希向量）
//
// ⚠️ 正确性约束：不同后端的向量空间不兼容——记录与查询必须使用同一后端，
// 检索时按 Backend() 标签过滤（见 RAGServiceV3.Query）。
type Embedder struct {
	BaseURL  string // Ollama 地址，如 http://127.0.0.1:11434
	Model    string // embedding 模型，如 bge-m3
	Timeout  time.Duration
	fallback func(string) []float64

	mu      sync.RWMutex
	probed  EmbedBackend
	probeAt time.Time
}

func NewEmbedder(baseURL, model string) *Embedder {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	if model == "" {
		model = "bge-m3"
	}
	return &Embedder{
		BaseURL:  baseURL,
		Model:    model,
		Timeout:  180 * time.Second, // bge-m3 冷装载需数分钟，必须容忍
		fallback: SimpleEmbed,
	}
}

// backendCacheTTL 后端探测结果缓存时长。
// 原实现每次调用都真打一次 /api/version：AddDocument 每处理一块 / 每次检索
// 都多付一次 HTTP 往返，Ollama 挂了还会让 KB 反复等待。
const backendCacheTTL = 30 * time.Second

// Backend 当前生效的后端（探测 Ollama 可用性，30 秒内复用探测结果）
func (e *Embedder) Backend() EmbedBackend {
	e.mu.RLock()
	if !e.probeAt.IsZero() && time.Now().Before(e.probeAt.Add(backendCacheTTL)) {
		b := e.probed
		e.mu.RUnlock()
		return b
	}
	e.mu.RUnlock()

	b := EmbedBackendSimple
	if e.ollamaAlive() {
		b = EmbedBackendOllama
	}

	e.mu.Lock()
	e.probed, e.probeAt = b, time.Now()
	e.mu.Unlock()
	return b
}

// EmbedBatch 顺序批量向量化。
// Ollama 无稳定 batch 接口，这里仍逐条请求；真正的收益在调用方：
// 由「每块一次全量持久化」改为「一批一次持久化」，导入耗时从 O(n²) 降为 O(n)。
func (e *Embedder) EmbedBatch(texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, t := range texts {
		v, err := e.Embed(t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (e *Embedder) ollamaAlive() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(e.BaseURL + "/api/version")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float64 `json:"embedding"`
}

// Embed 单文本向量化；Ollama 失败时降级 SimpleEmbed
func (e *Embedder) Embed(text string) ([]float64, error) {
	client := &http.Client{Timeout: e.Timeout}
	body, err := json.Marshal(embedRequest{Model: e.Model, Prompt: text})
	if err != nil {
		return nil, err
	}

	resp, err := client.Post(e.BaseURL+"/api/embeddings", "application/json", bytes.NewBuffer(body))
	if err != nil {
		return e.fallback(text), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return e.fallback(text), nil
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Embedding) == 0 {
		return e.fallback(text), nil
	}
	return out.Embedding, nil
}
