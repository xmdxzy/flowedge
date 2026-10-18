package service

import (
	"fmt"
	"log"
	"strings"
	"sync"
)

// RAGServiceV3 项目专属语义知识库
// - 语义 embedding（Ollama bge-m3），Ollama 不可用时降级词面向量
// - 按后端标签过滤检索，杜绝混合向量空间
// - 段落感知分块（优先按空行切，超长段落硬切并带重叠）
// - 启动时自动灌入 FlowEdge 项目手册（空库时）
type RAGServiceV3 struct {
	store    *VectorStore
	embedder *Embedder
	mu       sync.Mutex
}

func NewRAGServiceV3(dataDir, ollamaURL, embedModel string) *RAGServiceV3 {
	s := &RAGServiceV3{
		store:    NewVectorStore(dataDir),
		embedder: NewEmbedder(ollamaURL, embedModel),
	}
	if s.store.Count() == 0 {
		// 异步灌入手册：不阻塞后端启动（bge-m3 冷装载可能耗时数分钟）
		go s.SeedProjectManual()
	}
	return s
}

// Backend 当前 embedding 后端
func (s *RAGServiceV3) Backend() EmbedBackend { return s.embedder.Backend() }

// AddDocument 添加文档（段落感知分块 + 逐块向量化）
func (s *RAGServiceV3) AddDocument(content, source string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	chunks := chunkByParagraph(content, 500, 80)
	backend := s.embedder.Backend()
	ids := make([]string, 0, len(chunks))

	for i, chunk := range chunks {
		vec, err := s.embedder.Embed(chunk)
		if err != nil {
			return ids, fmt.Errorf("embed chunk %d failed: %w", i, err)
		}
		id, err := s.store.Add(VectorRecord{
			Content:   chunk,
			Source:    source,
			Embedding: vec,
			Metadata: map[string]interface{}{
				"chunk_index":  i,
				"total_chunks": len(chunks),
				"embedder":     string(backend),
				"embed_model":  s.embedder.Model,
			},
		})
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Query 语义检索（按当前后端标签过滤，topK 上限 10）
func (s *RAGServiceV3) Query(query string, topK int) ([]SearchResult, error) {
	if topK <= 0 {
		topK = 3
	}
	if topK > 10 {
		topK = 10
	}

	log.Printf("[kb] query start: %.40s", query)
	backend := s.embedder.Backend()
	log.Printf("[kb] backend=%s", backend)
	qvec, err := s.embedder.Embed(query)
	if err != nil {
		return nil, err
	}
	log.Printf("[kb] query embedded, dim=%d", len(qvec))

	all := s.store.GetAll()
	scored := make([]SearchResult, 0, len(all))
	for _, r := range all {
		// 向量空间兼容性：只与同后端的记录比较
		if tag, _ := r.Metadata["embedder"].(string); tag != "" && tag != string(backend) {
			continue
		}
		if len(r.Embedding) == 0 || len(r.Embedding) != len(qvec) {
			continue
		}
		scored = append(scored, SearchResult{Record: r, Score: cosineSimilarity(qvec, r.Embedding)})
	}

	// 简单插入排序（KB 规模小，避免引入额外依赖）
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].Score > scored[j-1].Score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}
	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored, nil
}

// GetContext 拼接检索上下文（供 RAG 节点）
func (s *RAGServiceV3) GetContext(query string, topK int) (string, error) {
	results, err := s.Query(query, topK)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "", nil
	}
	var sb strings.Builder
	sb.WriteString("以下是相关参考资料：\n\n")
	for i, r := range results {
		src := r.Record.Source
		if src == "" {
			src = "FlowEdge 手册"
		}
		sb.WriteString(fmt.Sprintf("[%d] (%s, 相似度 %.2f) %s\n", i+1, src, r.Score, r.Record.Content))
	}
	return sb.String(), nil
}

// Count / List / Delete 透传
func (s *RAGServiceV3) Count() int { return s.store.Count() }
func (s *RAGServiceV3) List() []VectorRecord {
	recs := s.store.GetAll()
	out := make([]VectorRecord, len(recs))
	copy(out, recs)
	return out
}
func (s *RAGServiceV3) Delete(id string) error { return s.store.Delete(id) }

// chunkByParagraph 段落感知分块：
//   - 独立段落 ≥ glueMin 字：自成一块（保证检索粒度）
//   - 碎段（< glueMin）：向后黏合，直到凑够一块
//   - 超长段落（> chunkSize）：硬切并带重叠
func chunkByParagraph(text string, chunkSize, overlap int) []string {
	if chunkSize <= 0 {
		chunkSize = 500
	}
	if overlap < 0 {
		overlap = 0
	}
	const glueMin = 120 // 低于此长度的段落才参与黏合

	var chunks []string
	var buf []string
	bufLen := 0

	flush := func() {
		if bufLen == 0 {
			return
		}
		chunks = append(chunks, strings.TrimSpace(strings.Join(buf, "\n")))
		buf = nil
		bufLen = 0
	}

	for _, para := range strings.Split(text, "\n\n") {
		p := strings.TrimSpace(para)
		if p == "" {
			continue
		}
		pr := []rune(p)
		if len(pr) > chunkSize {
			flush()
			for i := 0; i < len(pr); {
				end := i + chunkSize
				if end > len(pr) {
					end = len(pr)
				}
				chunks = append(chunks, string(pr[i:end]))
				if end >= len(pr) {
					break
				}
				i = end - overlap
				if i <= 0 {
					break
				}
			}
			continue
		}
		// 独立段落足够长：先冲掉黏合缓冲，自己成块
		if len(pr) >= glueMin {
			flush()
			chunks = append(chunks, p)
			continue
		}
		if bufLen+len(pr) > chunkSize {
			flush()
		}
		buf = append(buf, p)
		bufLen += len(pr)
	}
	flush()
	return chunks
}

// projectManual FlowEdge 项目手册（专属知识库种子内容）
const projectManual = `FlowEdge 是一个面向消费级单 GPU（RTX 3090 24GB）的私有化多模态 AI 工作流引擎，对标 Dify、n8n、ComfyUI、vLLM、Temporal。全部推理在本地完成：LLM 走 Ollama，文生图走 ComfyUI，语音走 SAPI 与 faster-whisper，视频走 FFmpeg，不依赖任何云端 API。

FlowEdge 支持 12 种节点类型：开始、结束、大语言模型（LLM）、文生图、语音转写（ASR）、语音合成（TTS）、视频合成、条件分支（if_else）、子工作流、HTTP 请求、数据转换、知识库检索（RAG）。节点从左侧节点库拖拽到画布，连线即数据流，同一层无依赖的节点会并行执行。

断点续跑：每次执行会逐层增量保存节点状态。当某次运行失败后，修复工作流定义，再点击顶栏的「断点续跑」按钮（或调用 POST /runs/<runId>/resume），引擎会复用已成功节点的输出，只重新执行失败或未执行的节点。已成功的节点时间戳保持不变，证明其输出被直接复用。

节点级重试：在节点的数据里配置 retry（重试次数，上限 5）与 retry_delay_ms（重试间隔毫秒，默认 1000）。执行失败时会自动按间隔重试，并通过 WebSocket 推送 node_retry 事件。

模型与环境：LLM 使用 Ollama（本机可用 qwen2.5-coder:32b，19.9GB，首次冷装载约 6 分钟，之后常驻显存推理约 16 token/s）；文生图使用 ComfyUI 的 flux1-schnell-fp8 checkpoint（4 步、cfg 1.0，768x768 约 8 秒）；知识库 embedding 使用 bge-m3。桌面客户端为 Electron 壳，由 Go 后端统一托管 API 与前端静态资源。

子工作流：sub_workflow 节点通过 workflow_id 引用另一个工作流，执行时会把上游文本注入内层的开始节点，内层结束节点的输出回传给外层下游。嵌套深度上限为 4 层，防止无限递归。`

// SeedProjectManual 空库时灌入项目手册
func (s *RAGServiceV3) SeedProjectManual() {
	s.mu.Lock()
	defer s.mu.Unlock()

	chunks := chunkByParagraph(projectManual, 500, 80)
	backend := s.embedder.Backend()
	for i, chunk := range chunks {
		vec, err := s.embedder.Embed(chunk)
		if err != nil {
			continue
		}
		s.store.Add(VectorRecord{
			Content:   chunk,
			Source:    "FlowEdge 项目手册",
			Embedding: vec,
			Metadata: map[string]interface{}{
				"chunk_index":  i,
				"total_chunks": len(chunks),
				"embedder":     string(backend),
				"embed_model":  s.embedder.Model,
			},
		})
	}
}
