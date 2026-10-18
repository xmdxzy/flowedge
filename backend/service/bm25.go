package service

import (
	"math"
	"unicode"
)

// tokenize 中文友好的分词：非字母数字切开；连续 CJK 切成 bigram。
//
// 中文不做分词直接跑 BM25，整段会被当成一个 term，等于没有关键词检索。
// bigram 是最省心且零词典依赖的做法（"断点续跑" → "断点" "点续" "续跑"）。
// 保留重复以维持词频（tf 是 BM25 的核心输入，不能去重）。
func tokenize(s string) []string {
	words := splitAlnum(s)
	var toks []string
	for _, w := range words {
		rs := []rune(w)
		if allCJK(rs) {
			if len(rs) >= 2 {
				for i := 0; i+2 <= len(rs); i++ {
					toks = append(toks, string(rs[i:i+2]))
				}
			} else {
				toks = append(toks, w)
			}
			continue
		}
		// 拉丁词统一小写化：查询写 FluxCheckpoint、文档写 fluxcheckpoint 应视为同一个词
		toks = append(toks, toLower(w))
	}
	return toks
}

func splitAlnum(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func allCJK(rs []rune) bool {
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if r < 0x4E00 || r > 0x9FFF {
			return false
		}
	}
	return true
}

// toLower 供外部复用
func toLower(s string) string {
	out := []rune(s)
	for i, r := range out {
		out[i] = unicode.ToLower(r)
	}
	return string(out)
}

// bm25Index 内存 BM25 索引。KB 规模小（万级以内），全量构建可接受。
type bm25Index struct {
	docs [][]string
	tf   []map[string]int
	df   map[string]int
	dl   []int
	avg  float64
	k1   float64
	b    float64
}

func newBM25(docs [][]string) *bm25Index {
	idx := &bm25Index{
		docs: docs,
		tf:   make([]map[string]int, len(docs)),
		dl:   make([]int, len(docs)), // 必须预留：nil slice 直接下标赋值会 panic
		df:   map[string]int{},
		k1:   1.5,
		b:    0.75,
	}
	total := 0
	for i, d := range docs {
		m := map[string]int{}
		counted := map[string]bool{}
		for _, t := range d {
			m[t]++
			// df 按「文档」计，不能按出现次数计：同一词在一个文档里出现 N 次
			// 会让 df 超过文档总数，idf 公式里的 (N-n+0.5) 变负 → idf 为负 → 排序完全错乱
			if !counted[t] {
				idx.df[t]++
				counted[t] = true
			}
		}
		idx.tf[i] = m
		idx.dl[i] = len(d)
		total += len(d)
	}
	if len(docs) > 0 {
		idx.avg = float64(total) / float64(len(docs))
	}
	return idx
}

// idf BM25 逆文档频率（含平滑）
func (x *bm25Index) idf(term string) float64 {
	n := float64(x.df[term])
	N := float64(len(x.docs))
	if n == 0 {
		return 0
	}
	return math.Log(1 + (N-n+0.5)/(n+0.5))
}

// score 返回每篇文档的 BM25 得分
func (x *bm25Index) score(queryTerms []string) []float64 {
	scores := make([]float64, len(x.docs))
	for i := range x.docs {
		var s float64
		for _, q := range queryTerms {
			f := float64(x.tf[i][q])
			if f == 0 {
				continue
			}
			norm := f * (x.k1 + 1) / (f + x.k1*(1-x.b+x.b*float64(x.dl[i])/math.Max(x.avg, 1e-9)))
			s += x.idf(q) * norm
		}
		scores[i] = s
	}
	return scores
}

// rrfFuse RRF（Reciprocal Rank Fusion）融合多路排名。
// k=60 是文献与工业界通用的平滑常数；每路只贡献排名不贡献绝对分值，
// 因此向量余弦（0~1）与 BM25（0~30+）天然不同量纲也能安全相加。
func rrfFuse(rankings [][]string, k float64) map[string]float64 {
	out := map[string]float64{}
	for _, rank := range rankings {
		for pos, id := range rank {
			out[id] += 1.0 / (k + float64(pos) + 1)
		}
	}
	return out
}

// mmrDiversify MMR 去重：在相关性与多样性之间折中。
// 单纯 topK 常返回同一段落的多个切块（尤其硬切 + 80 字重叠时）。
func mmrDiversify(cands []SearchResult, k int, lambda float64) []SearchResult {
	if len(cands) <= k {
		return cands
	}
	selected := make([]SearchResult, 0, k)
	pool := append([]SearchResult(nil), cands...)

	emb := func(r SearchResult) []float64 { return r.Record.Embedding }

	for len(selected) < k && len(pool) > 0 {
		bestIdx, bestScore := 0, -math.MaxFloat64
		for i, c := range pool {
			relevance := c.Score
			redundancy := 0.0
			for _, s := range selected {
				if sim := cosineSimilarity(emb(c), emb(s)); sim > redundancy {
					redundancy = sim
				}
			}
			score := lambda*relevance - (1-lambda)*redundancy
			if score > bestScore {
				bestScore, bestIdx = score, i
			}
		}
		selected = append(selected, pool[bestIdx])
		pool = append(pool[:bestIdx], pool[bestIdx+1:]...)
	}
	return selected
}
