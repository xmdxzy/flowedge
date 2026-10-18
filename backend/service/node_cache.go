package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// 节点级缓存：相同「配置 + 输入」直接复用上次输出
//
// 适用场景：反复调试同一条链路时，前面的昂贵节点（HTTP 抓取、知识库检索、
// 模板渲染）不必每次重跑。只对显式声明 cache: true 的确定性节点生效——
// 文生图 / LLM / 语音这类有随机性或外部状态的节点默认不缓存。
//
// 实现为带容量上限的 FIFO（够用且实现简单；命中率再优化可以换 LRU）。

type nodeCacheEntry struct {
	output interface{}
	at     time.Time
}

type NodeCache struct {
	mu      sync.Mutex
	entries map[string]nodeCacheEntry
	order   []string
	max     int
	hits    int
	misses  int
}

func NewNodeCache(max int) *NodeCache {
	if max <= 0 {
		max = 200
	}
	return &NodeCache{entries: make(map[string]nodeCacheEntry), max: max}
}

// cacheableNode 只有确定性且可能昂贵的节点才值得缓存
func cacheableNode(nodeType string, data map[string]interface{}) bool {
	if v, ok := data["cache"].(bool); !ok || !v {
		return false
	}
	switch nodeType {
	case "data_transform", "rag_query", "http_request", "sub_workflow":
		return true
	}
	return false
}

// cacheKey 由「节点类型 + 数据 + 上游输出」共同决定
func cacheKey(nodeType string, data map[string]interface{}, upstream map[string]interface{}) string {
	payload := map[string]interface{}{
		"type":     nodeType,
		"data":     data,
		"upstream": upstream,
	}
	// map 序列化顺序不稳定，先排序键再拼
	norm := normalizeForHash(payload)
	b, err := json.Marshal(norm)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// normalizeForHash 递归排序 map 键，保证同一输入得到同一 hash
func normalizeForHash(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]interface{}, len(val))
		for _, k := range keys {
			out[k] = normalizeForHash(val[k])
		}
		return out
	case []interface{}:
		arr := make([]interface{}, len(val))
		for i, item := range val {
			arr[i] = normalizeForHash(item)
		}
		return arr
	}
	return v
}

func (c *NodeCache) Get(key string) (interface{}, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.hits++
		return e.output, true
	}
	c.misses++
	return nil, false
}

func (c *NodeCache) Put(key string, output interface{}) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		c.entries[key] = nodeCacheEntry{output: output, at: time.Now()}
		return
	}
	c.entries[key] = nodeCacheEntry{output: output, at: time.Now()}
	c.order = append(c.order, key)
	for len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

// Stats 命中率（供自检接口展示）
func (c *NodeCache) Stats() map[string]interface{} {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	total := c.hits + c.misses
	rate := 0.0
	if total > 0 {
		rate = float64(c.hits) / float64(total)
	}
	return map[string]interface{}{
		"entries":  len(c.entries),
		"capacity": c.max,
		"hits":     c.hits,
		"misses":   c.misses,
		"hit_rate": rate,
	}
}

// Clear 清空缓存
func (c *NodeCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]nodeCacheEntry)
	c.order = nil
	c.hits, c.misses = 0, 0
}
