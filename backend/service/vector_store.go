package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// VectorRecord 向量记录
type VectorRecord struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Source    string    `json:"source"`
	Embedding []float64 `json:"embedding"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// SearchResult 搜索结果
type SearchResult struct {
	Record   VectorRecord
	Score    float64
}

// VectorStore 内存向量存储
type VectorStore struct {
	records  []VectorRecord
	dataDir  string
	mu       sync.RWMutex
}

func NewVectorStore(dataDir string) *VectorStore {
	if dataDir == "" {
		dataDir = "./data/vectors"
	}
	os.MkdirAll(dataDir, 0755)

	vs := &VectorStore{
		dataDir: dataDir,
	}
	vs.loadFromFile()
	return vs
}

// Add 添加向量记录，返回记录 ID
func (s *VectorStore) Add(record VectorRecord) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if record.ID == "" {
		record.ID = generateID(len(s.records))
	}

	s.records = append(s.records, record)
	if err := s.persist(); err != nil {
		return "", err
	}
	return record.ID, nil
}

// Search 向量相似度搜索
func (s *VectorStore) Search(queryEmbedding []float64, topK int) ([]SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if topK <= 0 {
		topK = 5
	}

	var results []SearchResult
	for _, record := range s.records {
		if len(record.Embedding) == 0 {
			continue
		}
		score := cosineSimilarity(queryEmbedding, record.Embedding)
		results = append(results, SearchResult{
			Record: record,
			Score:  score,
		})
	}

	// 按相似度降序排序
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > topK {
		results = results[:topK]
	}

	return results, nil
}

// SearchByText 文本搜索（用简单 embedding 替代）
func (s *VectorStore) SearchByText(query string, topK int) ([]SearchResult, error) {
	embedding := SimpleEmbed(query)
	return s.Search(embedding, topK)
}

// Delete 删除记录
func (s *VectorStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, r := range s.records {
		if r.ID == id {
			s.records = append(s.records[:i], s.records[i+1:]...)
			return s.persist()
		}
	}
	return nil
}

// Count 返回记录数
func (s *VectorStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}

// GetAll 获取所有记录
func (s *VectorStore) GetAll() []VectorRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.records
}

// persist 持久化到文件
func (s *VectorStore) persist() error {
	data, err := json.Marshal(s.records)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dataDir, "vectors.json"), data, 0644)
}

// loadFromFile 从文件加载
func (s *VectorStore) loadFromFile() {
	data, err := os.ReadFile(filepath.Join(s.dataDir, "vectors.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &s.records)
}

// ===== 向量计算工具 =====

// SimpleEmbed 简单的文本 embedding（哈希分桶）
// 注意：这只是 MVP 实现，生产环境应调用 Ollama 的 embedding API
func SimpleEmbed(text string) []float64 {
	const dim = 256
	vec := make([]float64, dim)

	runes := []rune(text)
	for _, ch := range runes {
		idx := int(ch) % dim
		vec[idx] += 1.0
		// 相邻桶也增加一些权重
		vec[(idx+1)%dim] += 0.5
		vec[(idx-1+dim)%dim] += 0.5
	}

	// L2 归一化
	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range vec {
			vec[i] /= norm
		}
	}

	return vec
}

// cosineSimilarity 余弦相似度
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}

	var dot, normA, normB float64
	for i := 0; i < len(a); i++ {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// generateID 生成记录 ID。
//
// 旧实现是 2 个字母 + 1 个数字，实际只有 26*26/2*10 ≈ 3380 个不同取值，
// 超过后必然碰撞；而 Delete 按 ID 匹配第一条即删除，碰撞后会误删无关记录。
// 改为 64 位随机 hex，碰撞概率可忽略。
func generateID(index int) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机数不可用时的兜底：纳秒时间戳 + 序号，同进程内保证唯一
		return fmt.Sprintf("r%x-%d", time.Now().UnixNano(), index)
	}
	return hex.EncodeToString(b[:])
}
