package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Document 文档
type Document struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Source  string `json:"source"`
}

// RAGService 简单的 RAG 知识库服务
type RAGService struct {
	docs      []Document
	mu        sync.RWMutex
	dataDir   string
}

func NewRAGService(dataDir string) *RAGService {
	if dataDir == "" {
		dataDir = "./data/rag"
	}
	os.MkdirAll(dataDir, 0755)

	return &RAGService{
		dataDir: dataDir,
	}
}

// AddDocument 添加文档到知识库
func (s *RAGService) AddDocument(content, source string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	docID := fmt.Sprintf("doc-%d", len(s.docs)+1)
	doc := Document{
		ID:      docID,
		Content: content,
		Source:  source,
	}

	s.docs = append(s.docs, doc)

	// 持久化
	s.saveToFile()

	return docID, nil
}

// Search 简单的关键词搜索（MVP 阶段用关键词匹配，后续换成向量检索）
func (s *RAGService) Search(query string, topK int) ([]Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if topK <= 0 {
		topK = 3
	}

	var results []Document
	for _, doc := range s.docs {
		// 简单的关键词匹配
		if contains(doc.Content, query) {
			results = append(results, doc)
			if len(results) >= topK {
				break
			}
		}
	}

	return results, nil
}

// GetContext 拼接检索结果作为上下文
func (s *RAGService) GetContext(query string) (string, error) {
	docs, err := s.Search(query, 3)
	if err != nil {
		return "", err
	}

	if len(docs) == 0 {
		return "", nil
	}

	context := "以下是相关参考资料：\n\n"
	for i, doc := range docs {
		context += fmt.Sprintf("[%d] %s\n", i+1, doc.Content)
	}

	return context, nil
}

// saveToFile 保存到文件
func (s *RAGService) saveToFile() {
	data, err := json.Marshal(s.docs)
	if err != nil {
		return
	}

	path := filepath.Join(s.dataDir, "documents.json")
	os.WriteFile(path, data, 0644)
}

// contains 简单字符串包含
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && indexOf(s, substr) >= 0))
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
