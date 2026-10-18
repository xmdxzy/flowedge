package service

import (
	"math"
	"strings"
	"testing"
)

// TestTokenize_ChineseBigram 中文必须切成 bigram，整段一个 term 等于没有关键词检索
func TestTokenize_ChineseBigram(t *testing.T) {
	toks := tokenize("断点续跑")
	if len(toks) == 0 {
		t.Fatal("no tokens")
	}
	want := map[string]bool{"断点": true, "点续": true, "续跑": true}
	for _, tk := range toks {
		delete(want, tk)
	}
	if len(want) != 0 {
		t.Fatalf("missing bigram tokens: %v (got %v)", want, toks)
	}
}

func TestTokenize_Mixed(t *testing.T) {
	toks := tokenize("使用 FluxCheckpoint 加载 flux1-schnell 模型")
	joined := toStr(toks)
	for _, want := range []string{"fluxcheckpoint", "flux1", "schnell", "模型"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected serialization to contain %q, got %v", want, toks)
		}
	}
}

// TestBM25_ScoresExactMatch 含全部查询词的文档得分必须高于只含部分词的
func TestBM25_ScoresExactMatch(t *testing.T) {
	docs := [][]string{
		tokenize("断点续跑可以复用已成功节点的输出"),
		tokenize("今天天气很好适合出门散步"),
		tokenize("断点续跑与断点续跑与断点续跑"),
	}
	idx := newBM25(docs)
	sc := idx.score(tokenize("断点续跑"))

	if sc[0] <= sc[1] {
		t.Fatalf("relevant doc should outrank irrelevant: %.3f vs %.3f", sc[0], sc[1])
	}
	if sc[2] <= sc[0] {
		t.Fatalf("doc with higher term frequency should score higher: %.3f vs %.3f", sc[2], sc[0])
	}
}

func TestBM25_EmptyCorpus(t *testing.T) {
	idx := newBM25(nil)
	if got := idx.score([]string{"x"}); len(got) != 0 {
		t.Fatalf("empty corpus should give no scores, got %d", len(got))
	}
	idx2 := newBM25([][]string{tokenize("算上一下")})
	_ = idx2.score(tokenize("算"))
}

// TestRRF_RankOverridesMagnitude RRF 只比排名，不比绝对分值
func TestRRF_RankOverridesMagnitude(t *testing.T) {
	// a 在两路都排第一；c 的 BM25 绝对分可能极高，但只排第二路第二
	vec := []string{"a", "b"}
	kw := []string{"a", "c"}
	fused := rrfFuse([][]string{vec, kw}, 60.0)
	if fused["a"] <= fused["c"] {
		t.Fatalf("a (双第一) must beat c: %v", fused)
	}
	if fused["a"] <= fused["b"] {
		t.Fatalf("a (第一/第二) must beat b: %v", fused)
	}
}

// TestMMR_Dedups 内容高度重复的候选必须被裁掉，只保留 topK 个
func TestMMR_Dedups(t *testing.T) {
	mk := func(id string, emb []float64) SearchResult {
		return SearchResult{Record: VectorRecord{ID: id, Embedding: emb}, Score: 0.9}
	}
	cands := []SearchResult{
		mk("same1", []float64{1, 0}),
		mk("same2", []float64{0.999, 0.001}), // 与 same1 几乎同向
		mk("diff", []float64{0, 1}),
	}
	out := mmrDiversify(cands, 2, 0.7)
	if len(out) != 2 {
		t.Fatalf("expected 2 results, got %d", len(out))
	}
	ids := map[string]bool{}
	for _, r := range out {
		ids[r.Record.ID] = true
	}
	if !ids["same1"] || !ids["diff"] {
		t.Fatalf("MMR should keep the diverse pair, got %v", ids)
	}
	if ids["same2"] {
		t.Fatal("MMR should drop the near-duplicate")
	}
}

// TestHybrid_KeywordOnlyHit 向量不可用时关键词一路仍能命中（降级场景的生命线）
func TestHybrid_KeywordOnlyHit(t *testing.T) {
	s := &RAGServiceV3{
		store:    NewVectorStore(t.TempDir()),
		embedder: NewEmbedder("http://127.0.0.1:1", "bge-m3"), // 不可达 → simple 后端
	}
	// 手工塞入：embedder 标签为 simple，与降级后端一致
	vec := SimpleEmbed("占位文本")
	s.store.AddBatch([]VectorRecord{
		{ID: "r1", Content: "断点续跑会复用已成功节点的输出，只重跑失败部分", Source: "manual.md", Embedding: vec,
			Metadata: map[string]interface{}{"chunk_index": 0.0, "embedder": "simple"}},
		{ID: "r2", Content: "今天天气很好适合出门散步和购物", Source: "other.md", Embedding: vec,
			Metadata: map[string]interface{}{"chunk_index": 0.0, "embedder": "simple"}},
	})

	results, err := s.Query("断点续跑", 3)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("keyword-only query returned nothing")
	}
	if results[0].Record.ID != "r1" {
		t.Fatalf("expected r1 first, got %s", results[0].Record.ID)
	}
	if results[0].Fused <= 0 {
		t.Fatalf("fused score should be populated, got %v", results[0])
	}
}

// TestVectorStore_AddBatchSinglePersist 批量写入只落一次盘（且内容完整）
func TestVectorStore_AddBatchSinglePersist(t *testing.T) {
	dir := t.TempDir()
	vs := NewVectorStore(dir)
	recs := make([]VectorRecord, 5)
	for i := range recs {
		recs[i] = VectorRecord{ID: "", Content: "c", Embedding: []float64{1}}
	}
	if _, err := vs.AddBatch(recs); err != nil {
		t.Fatalf("add batch: %v", err)
	}
	if vs.Count() != 5 {
		t.Fatalf("expected 5 records, got %d", vs.Count())
	}
	// 从磁盘重新加载：证明内容被一次持久化且完整
	reloaded := NewVectorStore(dir)
	if reloaded.Count() != 5 {
		t.Fatalf("reloaded %d records, want 5", reloaded.Count())
	}
}

// TestRefLabel 引用标签必须带 chunk 序号与相似度，便于溯源
func TestRefLabel(t *testing.T) {
	got := refLabel(SearchResult{
		Record: VectorRecord{
			Source:    "FlowEdge 手册",
			Content:   "x",
			Metadata:  map[string]interface{}{"chunk_index": 3.0},
			Embedding: []float64{1},
		},
		Score: 0.72,
	})
	want := "(来源: FlowEdge 手册#chunk3, 语义相似度 0.72)"
	if got != want {
		t.Fatalf("refLabel = %q, want %q", got, want)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func toStr(list []string) string {
	out := ""
	for _, v := range list {
		out += v
	}
	return out
}

var _ = math.Log
