package service

import (
	"fmt"
	"strings"
)

// RAGServiceV2 向量检索版 RAG 服务
type RAGServiceV2 struct {
	vectorStore *VectorStore
}

func NewRAGServiceV2(dataDir string) *RAGServiceV2 {
	return &RAGServiceV2{
		vectorStore: NewVectorStore(dataDir),
	}
}

// AddDocument 添加文档并生成 embedding
func (s *RAGServiceV2) AddDocument(content, source string, metadata map[string]interface{}) (string, error) {
	embedding := SimpleEmbed(content)

	record := VectorRecord{
		Content:   content,
		Source:    source,
		Embedding: embedding,
		Metadata:  metadata,
	}

	return s.vectorStore.Add(record)
}

// AddDocuments 批量添加文档（分块）
func (s *RAGServiceV2) AddDocuments(content string, chunkSize int, source string) ([]string, error) {
	if chunkSize <= 0 {
		chunkSize = 500
	}

	chunks := chunkText(content, chunkSize)
	ids := make([]string, 0, len(chunks))

	for i, chunk := range chunks {
		id, err := s.AddDocument(chunk, source, map[string]interface{}{
			"chunk_index": i,
			"total_chunks": len(chunks),
		})
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}

	return ids, nil
}

// Query 向量检索
func (s *RAGServiceV2) Query(query string, topK int) ([]SearchResult, error) {
	return s.vectorStore.SearchByText(query, topK)
}

// GetContext 获取拼接后的上下文
func (s *RAGServiceV2) GetContext(query string, topK int) (string, error) {
	results, err := s.Query(query, topK)
	if err != nil {
		return "", err
	}

	if len(results) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("以下是相关参考资料：\n\n")
	for i, result := range results {
		sb.WriteString(fmt.Sprintf("[%d] (相似度: %.2f) %s\n", i+1, result.Score, result.Record.Content))
	}

	return sb.String(), nil
}

// Count 文档数量
func (s *RAGServiceV2) Count() int {
	return s.vectorStore.Count()
}

// GetDocuments 获取所有文档
func (s *RAGServiceV2) GetDocuments() []VectorRecord {
	return s.vectorStore.GetAll()
}

// DeleteDocument 删除文档
func (s *RAGServiceV2) DeleteDocument(id string) error {
	return s.vectorStore.Delete(id)
}

// chunkText 文本分块
func chunkText(text string, chunkSize int) []string {
	runes := []rune(text)
	var chunks []string

	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}

	return chunks
}
