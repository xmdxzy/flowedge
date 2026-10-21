package service

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 显存档案库：按「模型 × 分辨率 × 帧数 × 步数」记录实测峰值与墙钟。
//
// 为什么这是真护城河（2026-10-21）：
//
//	竞品都说自己"支持视频生成"，但没人敢在提交前告诉你
//	「这条 8 镜 30 秒 608×352 预计 22.1GB / 28 分钟，第 3 镜会撞 24GB 上限」。
//	给不出这个数，是因为它们不知道用户机器上要跑什么。
//
// 三条硬纪律（违反就变成"假精确"，比不给数字更糟）：
//  1. **不跨参数外推**：没有同档位样本时，只能给"最近档位 + 明确标注外推"，
//     并且只给上界。
//  2. **样本不足要说出来**：Samples < 3 时 Confidence 必须降级。
//  3. **采样漏峰要承认**：采样间隔大于节点时长时，peak 是下界而非峰值，
//     用 PeakUnderestimated 标出来。
const profileSchemaVersion = 1

// VRAMProfile 一条实测档位
type VRAMProfile struct {
	Kind    string `json:"kind"`  // image | video | h3 | upscale | inpaint | img2img | llm | asr | tts
	Model   string `json:"model"` // 权重/模型标识（空 = 该 kind 的默认）
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	Frames  int    `json:"frames,omitempty"`
	Steps   int    `json:"steps,omitempty"`
	PeakMB  int    `json:"peak_mb"`
	WallMS  int64  `json:"wall_ms"`
	Samples int    `json:"samples"`
	Source  string `json:"source,omitempty"` // sampled | model_size
	Updated string `json:"updated,omitempty"`
}

// Prediction 一次提交前预测
type Prediction struct {
	Kind               string `json:"kind"`
	Model              string `json:"model,omitempty"`
	PeakMB             int    `json:"peak_mb"`
	PeakMinMB          int    `json:"peak_min_mb"`
	PeakMaxMB          int    `json:"peak_max_mb"`
	WallMS             int64  `json:"wall_ms"`
	WallMinMS          int64  `json:"wall_min_ms"`
	WallMaxMS          int64  `json:"wall_max_ms"`
	Samples            int    `json:"samples"`
	Confidence         string `json:"confidence"` // high | medium | low | none
	Basis              string `json:"basis"`      // 依据说明（人话）
	Extrapolated       bool   `json:"extrapolated"`
	PeakUnderestimated bool   `json:"peak_underestimated,omitempty"`
	Warning            string `json:"warning,omitempty"`
}

// ProfileStore 档案仓库（线程安全，可落盘）
type ProfileStore struct {
	path  string
	mu    sync.RWMutex
	items []VRAMProfile
	// totalVRAMMB 用于判断「会不会撞墙」；0 = 未知，不做撞墙判断
	totalVRAMMB int
}

// LoadProfileStore 读档案文件；不存在则空仓（不写默认——空仓本身就是有效状态）
func LoadProfileStore(path string, totalVRAMMB int) *ProfileStore {
	s := &ProfileStore{path: path, totalVRAMMB: totalVRAMMB}
	if strings.TrimSpace(path) == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var f struct {
		Profiles []VRAMProfile `json:"profiles"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return s // 坏文件不阻断，从空档开始
	}
	s.items = f.Profiles
	return s
}

// Record 记录一次实测。相近档位合并（取样本加权平均），避免档位爆炸。
func (s *ProfileStore) Record(p VRAMProfile) {
	if s == nil || p.Kind == "" || p.PeakMB <= 0 {
		return
	}
	p.Updated = time.Now().Format("2006-01-02")
	if p.Samples <= 0 {
		p.Samples = 1
	}
	s.mu.Lock()
	for i := range s.items {
		if similarProfile(s.items[i], p) {
			cur := s.items[i]
			n := cur.Samples + p.Samples
			// 加权平均：多档位数字抖动不该让档案越来越"精确"
			cur.PeakMB = (cur.PeakMB*cur.Samples + p.PeakMB*p.Samples) / n
			cur.WallMS = (cur.WallMS*int64(cur.Samples) + p.WallMS*int64(p.Samples)) / int64(n)
			cur.Samples = n
			cur.Updated = p.Updated
			if p.Source != "" {
				cur.Source = p.Source
			}
			s.items[i] = cur
			s.mu.Unlock()
			return
		}
	}
	s.items = append(s.items, p)
	s.mu.Unlock()
}

// similarProfile 同一档位的判定：kind+model 相同，参数在容差内
func similarProfile(a, b VRAMProfile) bool {
	if a.Kind != b.Kind || a.Model != b.Model {
		return false
	}
	if !within(a.Width, b.Width, 0.10) || !within(a.Height, b.Height, 0.10) {
		return false
	}
	if !within(a.Frames, b.Frames, 0.10) {
		return false
	}
	return within(a.Steps, b.Steps, 0.25)
}

// within 相对容差比较（两个都为 0 视为相同；一方为 0 视为不同档位）
func within(a, b int, tol float64) bool {
	if a == b {
		return true
	}
	if a <= 0 || b <= 0 {
		return false
	}
	diff := math.Abs(float64(a-b)) / float64(maxInt(a, b))
	return diff <= tol
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// All 返回当前档案（拷贝，避免外部改内部状态）
func (s *ProfileStore) All() []VRAMProfile {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]VRAMProfile, len(s.items))
	copy(out, s.items)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].PeakMB > out[j].PeakMB
	})
	return out
}

// Save 落盘（path 为空时返回错误，不静默成功）
func (s *ProfileStore) Save() error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("显存档案未绑定文件路径")
	}
	s.mu.RLock()
	items := make([]VRAMProfile, len(s.items))
	copy(items, s.items)
	s.mu.RUnlock()
	raw, err := json.MarshalIndent(map[string]interface{}{
		"$schema":  profileSchemaVersion,
		"_comment": "实测档位，由引擎在每次重节点执行后自动累积。PeakMB 来自 nvidia-smi 局部采样（节点执行窗口内 1s 一次）。",
		"profiles": items,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, append(raw, '\n'), 0o644)
}

// Predict 预测一次执行。
//
// 匹配优先级：
//  1. kind + model 全等且参数在容差内 → 直接用（confidence 按样本数给）
//  2. kind 相同、参数最接近 → **外推**，只给上界，confidence=low
//  3. 该 kind 一条都没有 → confidence=none，明确说"没有样本"
func (s *ProfileStore) Predict(kind, model string, width, height, frames, steps int) Prediction {
	p := Prediction{Kind: kind, Model: model, Confidence: "none", Basis: "该类型没有实测样本"}
	if s == nil {
		return p
	}
	s.mu.RLock()
	items := make([]VRAMProfile, len(s.items))
	copy(items, s.items)
	total := s.totalVRAMMB
	s.mu.RUnlock()

	var exact *VRAMProfile
	var nearest *VRAMProfile
	bestScore := math.MaxFloat64
	for i := range items {
		it := &items[i]
		if it.Kind != kind {
			continue
		}
		if model != "" && it.Model != "" && it.Model != model {
			continue
		}
		if similarProfile(*it, VRAMProfile{
			Kind: kind, Model: it.Model,
			Width: width, Height: height, Frames: frames, Steps: steps,
		}) {
			if exact == nil || it.Samples > exact.Samples {
				exact = it
			}
			continue
		}
		// 距离：分辨率/帧/步数的相对差之和
		score := relDiff(it.Width, width) + relDiff(it.Height, height) +
			relDiff(it.Frames, frames) + relDiff(it.Steps, steps)
		if score < bestScore {
			bestScore = score
			nearest = it
		}
	}

	switch {
	case exact != nil:
		spread := 0.15
		if exact.Samples < 3 {
			spread = 0.25
		}
		p.PeakMB = exact.PeakMB
		p.PeakMinMB = int(float64(exact.PeakMB) * (1 - spread))
		p.PeakMaxMB = int(float64(exact.PeakMB) * (1 + spread))
		p.WallMS = exact.WallMS
		p.WallMinMS = int64(float64(exact.WallMS) * (1 - spread))
		p.WallMaxMS = int64(float64(exact.WallMS) * (1 + spread))
		p.Samples = exact.Samples
		p.Confidence = "high"
		if exact.Samples < 3 {
			p.Confidence = "medium"
		}
		if exact.Source == "model_size" {
			p.PeakUnderestimated = true
			p.Confidence = "low"
		}
		p.Basis = fmt.Sprintf("同档位实测 %d 次%s", exact.Samples, dimsText(exact.Width, exact.Height, exact.Frames, exact.Steps))
		if exact.Source == "model_size" {
			p.Basis += "；峰值由模型体积估算，非采样值"
		}
	case nearest != nil:
		// 外推：只给上界，明确标注
		p.PeakMB = nearest.PeakMB
		p.PeakMinMB = nearest.PeakMB
		p.PeakMaxMB = int(float64(nearest.PeakMB) * 1.4)
		p.WallMS = nearest.WallMS
		p.WallMinMS = nearest.WallMS
		p.WallMaxMS = int64(float64(nearest.WallMS) * 2.0)
		p.Samples = nearest.Samples
		p.Confidence = "low"
		p.Extrapolated = true
		p.Basis = fmt.Sprintf("无同档位样本；最近档位%s（%d 次实测）。**这是外推值，仅供量级参考**",
			dimsText(nearest.Width, nearest.Height, nearest.Frames, nearest.Steps), nearest.Samples)
	default:
		return p
	}

	if total > 0 {
		free := int(float64(total) * 0.96) // 留一点给显示输出
		if p.PeakMaxMB > free {
			p.Warning = fmt.Sprintf("预计峰值上界 %d MB 会超过可用 %d MB：大概率 OOM。"+
				"建议降分辨率或减帧数（引擎会自动降级重试，但降级后画质与时长会变）", p.PeakMaxMB, free)
		} else if p.PeakMB > int(float64(total)*0.85) {
			p.Warning = fmt.Sprintf("预计峰值 %d MB 已到 %d MB 的 %.0f%%，同卡上的其他任务需要先让出显存",
				p.PeakMB, total, float64(p.PeakMB)/float64(total)*100)
		}
	}
	return p
}

// dimsText 拼「512x512 / 124 帧 / 20 步」。
//
// 为 0 的参数不显示——出图节点没有帧数和步数，全部照抄会得到
// 「512x512 / 0 帧 / 0 步」，看着像数据缺失而不是「该节点没有这个参数」。
func dimsText(w, h, frames, steps int) string {
	var b strings.Builder
	if w > 0 && h > 0 {
		fmt.Fprintf(&b, "（%dx%d", w, h)
	} else {
		b.WriteString("（")
	}
	if frames > 0 {
		fmt.Fprintf(&b, " / %d 帧", frames)
	}
	if steps > 0 {
		fmt.Fprintf(&b, " / %d 步", steps)
	}
	b.WriteString("）")
	return b.String()
}

// relDiff 相对差（任一方 <=0 时给一个较大的惩罚，避免把"未指定"当成最佳匹配）
func relDiff(a, b int) float64 {
	if a <= 0 && b <= 0 {
		return 0
	}
	if a <= 0 || b <= 0 {
		return 2.0
	}
	return math.Abs(float64(a-b)) / float64(maxInt(a, b))
}
