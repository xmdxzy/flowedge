package service

import (
	"fmt"
	"sort"
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

// minVecScore 语义阈值：bge-m3 下不相关文本的余弦通常 < 0.35，0.30 是保守下限。
// 仅在后端为 Ollama（真语义向量）时生效 —— 降级到 SimpleEmbed 时向量分不可信，
// 此时关键词一路（BM25）是主要命中来路，不能再用向量阈值把它过滤掉。
const minVecScore = 0.30

// AddDocument 添加文档（段落感知分块 + 批量向量化 + 单次落盘）
func (s *RAGServiceV3) AddDocument(content, source string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addChunks(chunkByParagraph(content, 500, 80), source)
}

// addChunks 批量写入：先全部向量化，再一次持久化。
// 原实现每块调用 store.Add（每次都全量 marshal 覆写 vectors.json），
// 导入 N 块是 O(n²) 写放大；现在降为一次。
func (s *RAGServiceV3) addChunks(chunks []string, source string) ([]string, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	backend := s.embedder.Backend()

	vecs, err := s.embedder.EmbedBatch(chunks)
	if err != nil {
		return nil, err
	}

	records := make([]VectorRecord, 0, len(chunks))
	for i, chunk := range chunks {
		records = append(records, VectorRecord{
			Content:   chunk,
			Source:    source,
			Embedding: vecs[i],
			Metadata: map[string]interface{}{
				"chunk_index":  i,
				"total_chunks": len(chunks),
				"embedder":     string(backend),
				"embed_model":  s.embedder.Model,
			},
		})
	}
	return s.store.AddBatch(records)
}

// Query 混合检索：向量（语义）+ BM25（关键词）双路，RRF 融合后 MMR 去重。
// sourceContains 非空时按来源子串过滤（元数据过滤的最小可用形态）。
func (s *RAGServiceV3) Query(query string, topK int) ([]SearchResult, error) {
	return s.QueryFiltered(query, topK, "")
}

func (s *RAGServiceV3) QueryFiltered(query string, topK int, sourceContains string) ([]SearchResult, error) {
	if topK <= 0 {
		topK = 3
	}
	if topK > 10 {
		topK = 10
	}

	backend := s.embedder.Backend()

	// 候选集：向量空间兼容（同后端标签）+ 可选来源过滤
	all := s.store.GetAll()
	cands := make([]VectorRecord, 0, len(all))
	for _, r := range all {
		if tag, _ := r.Metadata["embedder"].(string); tag != "" && tag != string(backend) {
			continue
		}
		if len(r.Embedding) == 0 {
			continue
		}
		if sourceContains != "" &&
			!strings.Contains(strings.ToLower(r.Source), strings.ToLower(sourceContains)) {
			continue
		}
		cands = append(cands, r)
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// 第一路：向量（语义）
	qvec, err := s.embedder.Embed(query)
	if err != nil {
		return nil, err
	}
	vecScored := make([]SearchResult, 0, len(cands))
	for _, r := range cands {
		if len(r.Embedding) != len(qvec) {
			continue
		}
		vecScored = append(vecScored, SearchResult{Record: r, Score: cosineSimilarity(qvec, r.Embedding)})
	}
	sortByScoreDesc(vecScored)

	// 第二路：BM25（关键词）。embedding 降级到 SimpleEmbed 时，它几乎是唯一能命中的路。
	terms := tokenize(query)
	docs := make([][]string, len(cands))
	for i, r := range cands {
		docs[i] = tokenize(r.Content)
	}
	kw := newBM25(docs).score(terms)
	kwScored := make([]SearchResult, 0, len(cands))
	for i, r := range cands {
		if kw[i] > 0 {
			kwScored = append(kwScored, SearchResult{Record: r, Score: kw[i]})
		}
	}
	sortByScoreDesc(kwScored)

	// RRF 融合：两路各取前 30 名参与（两路绝对分数量纲不同，只比排名）
	cut := func(rs []SearchResult) []SearchResult {
		if len(rs) > 30 {
			return rs[:30]
		}
		return rs
	}
	vecRank := idsOf(cut(vecScored))
	kwRank := idsOf(cut(kwScored))
	fused := rrfFuse([][]string{vecRank, kwRank}, 60.0)

	byID := make(map[string]SearchResult, len(vecScored))
	for _, r := range vecScored {
		byID[r.Record.ID] = r
	}
	out := make([]SearchResult, 0, len(fused))
	for id, fuse := range fused {
		r, ok := byID[id]
		if !ok {
			for _, k := range kwScored {
				if k.Record.ID == id {
					r = k
					break
				}
			}
		}
		if r.Record.ID == "" {
			continue
		}
		r.Fused = fuse
		out = append(out, r)
	}
	sortByFusedDesc(out)

	// 阈值过滤：语义后端下丢掉不相关结果（否则 RAG 会拿噪声充上下文）
	if backend == EmbedBackendOllama {
		kept := out[:0]
		for _, r := range out {
			if r.Score < minVecScore && r.Fused < 0.016 {
				continue
			}
			kept = append(kept, r)
		}
		out = kept
	}

	// MMR 去重：硬切 + 80 字重叠会让同一自然段产生多个近似块
	out = mmrDiversify(out, topK, 0.7)
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func idsOf(rs []SearchResult) []string {
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, r.Record.ID)
	}
	return ids
}

func sortByScoreDesc(rs []SearchResult) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Score > rs[j].Score })
}

func sortByFusedDesc(rs []SearchResult) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Fused > rs[j].Fused })
}

// GetContext 拼接检索上下文（供 RAG 节点）
func (s *RAGServiceV3) GetContext(query string, topK int) (string, error) {
	results, err := s.Query(query, topK)
	if err != nil {
		return "", err
	}
	return s.ContextFrom(results), nil
}

// ContextFrom 把检索结果拼成上下文文本。
// 单独拆出来：RAG 节点可以只检索一次，同时拿到文本上下文与结构化引用。
func (s *RAGServiceV3) ContextFrom(results []SearchResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("以下是相关参考资料：\n\n")
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("[%d] %s %s\n", i+1, refLabel(r), r.Record.Content))
	}
	return sb.String()
}

// refLabel 生成可溯源的引用标签：source.md#chunk3 (语义相似度 0.72)
func refLabel(r SearchResult) string {
	rec := r.Record
	src := rec.Source
	if src == "" {
		src = "FlowEdge 手册"
	}
	idx := 0
	if v, ok := rec.Metadata["chunk_index"].(float64); ok {
		idx = int(v)
	}
	return fmt.Sprintf("(来源: %s#chunk%d, 语义相似度 %.2f)", src, idx, r.Score)
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

FlowEdge 支持 13 种节点类型：开始、结束、大语言模型（LLM）、文生图、语音转写（ASR）、语音合成（TTS）、视频合成、条件分支（if_else）、循环（loop）、子工作流、HTTP 请求、数据转换、知识库检索（RAG）。节点从左侧节点库拖拽到画布，连线即数据流，同一层无依赖的节点会并行执行。

断点续跑：每次执行会逐层增量保存节点状态。当某次运行失败后，修复工作流定义，再点击顶栏的「断点续跑」按钮（或调用 POST /runs/<runId>/resume），引擎会复用已成功节点的输出，只重新执行失败或未执行的节点。已成功的节点时间戳保持不变，证明其输出被直接复用。

节点级重试：在节点的数据里配置 retry（重试次数，上限 5）与 retry_delay_ms（重试间隔毫秒，默认 1000）。执行失败时会自动按间隔重试，并通过 WebSocket 推送 node_retry 事件。

模型与环境：LLM 使用 Ollama（本机可用 qwen2.5-coder:32b，19.9GB，首次冷装载约 6 分钟，之后常驻显存推理约 16 token/s）；文生图使用 ComfyUI 的 flux1-schnell-fp8 checkpoint（4 步、cfg 1.0，768x768 约 8 秒）；知识库 embedding 使用 bge-m3。桌面客户端为 Electron 壳，由 Go 后端统一托管 API 与前端静态资源。

循环：loop 节点迭代执行另一个工作流（同样用 workflow_id 引用），有三种模式。固定次数（count）按指定次数跑，每一轮以上一轮的输出作为下一轮的输入，适合反复打磨、逐轮精炼；遍历列表（foreach）把上游文本按分隔符切成多条（默认换行），逐项喂给内层工作流，适合批量处理；条件循环（until）至少执行一轮，每轮检查输出是否满足停止条件（包含、不包含、等于、不为空），满足即停，适合「跑到成功为止」。为防止死循环，最大迭代默认 20、硬上限 100，达到上限会中止并返回已有结果，结果的 stopped_by 字段说明是哪一种停止方式（completed / exhausted / condition / max_iterations）。

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
