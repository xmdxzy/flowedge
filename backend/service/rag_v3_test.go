package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 段落感知分块：短段落合并、超长段落硬切带重叠
func TestRAGV3_ChunkByParagraph(t *testing.T) {
	// 两个短段落 → 合并为一个块
	short := "第一段内容。\n\n第二段内容。"
	chunks := chunkByParagraph(short, 500, 80)
	if len(chunks) != 1 {
		t.Fatalf("short paragraphs chunk count = %d, want 1", len(chunks))
	}
	if !strings.Contains(chunks[0], "第一段") || !strings.Contains(chunks[0], "第二段") {
		t.Errorf("merged chunk lost content: %q", chunks[0])
	}

	// 超长段落（1200 字）→ 硬切多块且带重叠
	long := strings.Repeat("字", 1200)
	chunks = chunkByParagraph(long, 500, 80)
	if len(chunks) < 3 {
		t.Fatalf("long paragraph chunk count = %d, want >= 3", len(chunks))
	}
	// 相邻块应有重叠：块1尾部应包含块2开头附近的内容
	overlapFound := strings.Contains(chunks[0][400:], chunks[1][:50])
	if !overlapFound {
		t.Errorf("expected overlap between consecutive chunks")
	}
}

// Ollama 不可用 → 降级 SimpleEmbed，词面检索仍然可用（启动灌手册后）
func TestRAGV3_FallbackSearch(t *testing.T) {
	// 指向不可达端口，强制走降级
	s := &RAGServiceV3{
		store:    NewVectorStore(t.TempDir()),
		embedder: NewEmbedder("http://127.0.0.1:1", "bge-m3"),
	}
	s.SeedProjectManual()
	if s.Count() == 0 {
		t.Fatalf("manual seeding produced no records")
	}

	results, err := s.Query("断点续跑", 3)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected at least one result")
	}
	if !strings.Contains(results[0].Record.Content, "断点续跑") {
		t.Errorf("top result should mention 断点续跑, got: %.80s", results[0].Record.Content)
	}
}

// mock 语义管线：/api/embeddings 返回受控向量，验证 embedding 通路 + 排序 + 标签过滤
func TestRAGV3_MockEmbedder(t *testing.T) {
	var gotModel string
	// 受控向量：请求体含「苹果」→[1,0]；含「香蕉」→[0,1]；否则零向量
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			// 健康探测：返回版本即可，不参与 embedding 断言
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"version":"mock"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		vec := []float64{0, 0}
		if strings.Contains(string(body), "苹果") {
			vec = []float64{1, 0}
		} else if strings.Contains(string(body), "香蕉") {
			vec = []float64{0, 1}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"embedding": [%v, %v]}`, vec[0], vec[1])
	}))
	defer srv.Close()

	s := &RAGServiceV3{
		store:    NewVectorStore(t.TempDir()),
		embedder: NewEmbedder(srv.URL, "bge-m3"),
	}

	if _, err := s.AddDocument("苹果是一种水果", "fruit-doc"); err != nil {
		t.Fatalf("add doc1: %v", err)
	}
	if _, err := s.AddDocument("香蕉是一种热带水果", "fruit-doc"); err != nil {
		t.Fatalf("add doc2: %v", err)
	}

	// 查询「苹果」应命中 doc1（向量 [1,0] 完全匹配）
	results, err := s.Query("我爱吃苹果", 2)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("no results")
	}
	if results[0].Score < 0.99 {
		t.Errorf("top score = %.3f, want ~1.0", results[0].Score)
	}
	if !strings.Contains(results[0].Record.Content, "苹果是一种水果") {
		t.Errorf("top result = %.60s", results[0].Record.Content)
	}
	if gotModel != "bge-m3" {
		t.Errorf("embed request model = %q, want bge-m3", gotModel)
	}

	// 标签过滤：手工塞入一条不同后端的记录，查询时必须被排除
	if _, err := s.store.Add(VectorRecord{
		Content:   "混入的异后端记录",
		Embedding: []float64{1, 0},
		Metadata:  map[string]interface{}{"embedder": "simple"},
	}); err != nil {
		t.Fatalf("add foreign record: %v", err)
	}
	results, err = s.Query("苹果", 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	for _, r := range results {
		if strings.Contains(r.Record.Content, "异后端") {
			t.Errorf("foreign-backend record leaked into results")
		}
	}
}
