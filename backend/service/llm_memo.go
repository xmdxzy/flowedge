package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
)

// 确定性 LLM 调用的进程内备忘。
//
// 由来（执行记录审计 + 本次效率评审）：调试一条片子要把同一张工作流跑十几遍，
// 而「提示词优化 / 场景九宫格拆解 / 中译英」这三个调用是纯文本变换——
// 同输入必同输出（temperature=0，不掺随机性），却每次运行都重跑一遍。
// 代价不只是慢（语言侧几秒到几十秒）：
//   - 每次进语言侧都会把扩散侧的 flux 挤下去，回过头又要 40s 冷加载
//   - 也就是「改一个字重跑一次」被放大成「每次都全套重来」
//
// 边界（宁可不缓存，也不缓存错的）：
//   - 只缓存成功且非空的输出；失败不写（该重试就重试）
//   - key 含模型名：换了模型自动失效
//   - 有容量上限，FIFO 淘汰
type llmMemo struct {
	mu      sync.Mutex
	entries map[string]string
	order   []string
	max     int
	hits    int
	misses  int
}

func newLLMMemo(max int) *llmMemo {
	if max <= 0 {
		max = 256
	}
	return &llmMemo{entries: make(map[string]string), max: max}
}

// Get 取备忘；没命中返回 ok=false
func (m *llmMemo) Get(key string) (string, bool) {
	if m == nil || key == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.entries[key]
	if ok {
		m.hits++
	} else {
		m.misses++
	}
	return v, ok
}

// Put 写备忘（已存在则忽略，不更新顺序）
func (m *llmMemo) Put(key, val string) {
	if m == nil || key == "" || strings.TrimSpace(val) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[key]; exists {
		return
	}
	m.entries[key] = val
	m.order = append(m.order, key)
	for len(m.order) > m.max {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.entries, oldest)
	}
}

// Stats 命中率统计（供 /api/capabilities 或状态栏展示）
func (m *llmMemo) Stats() map[string]interface{} {
	if m == nil {
		return map[string]interface{}{"hits": 0, "misses": 0, "size": 0}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	total := m.hits + m.misses
	rate := 0.0
	if total > 0 {
		rate = float64(m.hits) / float64(total)
	}
	return map[string]interface{}{
		"hits":    m.hits,
		"misses":  m.misses,
		"size":    len(m.entries),
		"hitRate": math.Round(rate*100) / 100,
	}
}

// defaultLLMMemo 引擎级共享备忘。进程内单例，靠内部 mutex 保证并发安全
// （同一拓扑层的节点是并行执行的）。
var defaultLLMMemo = newLLMMemo(256)

// deterministicChat 确定性 LLM 文本变换：temperature=0 + 结果备忘。
//
// 适用范围：翻译 / 提示词优化 / 场景九宫格拆解 / 分镜拆解这类
// 「同输入必同输出」的纯文本变换。创作类节点（用户要多样性）不要走这里。
//
// 不缓存失败与空输出：那通常意味着上游或模型异常，缓存它等于缓存故障。
func (e *EngineV2) deterministicChat(model, prompt string) (string, error) {
	if e.llm == nil {
		return "", fmt.Errorf("LLM 服务未配置")
	}
	k := memoKey(model, prompt)
	if v, ok := defaultLLMMemo.Get(k); ok {
		return v, nil
	}
	out, err := e.llm.ChatWithTemperature(prompt, model, 0)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("LLM 返回空结果")
	}
	defaultLLMMemo.Put(k, out)
	return out, nil
}

// memoKey 由「模型 + 完整提示词」决定；指令模板换了也会体现在 prompt 里
func memoKey(model, prompt string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + prompt))
	return hex.EncodeToString(sum[:16])
}
