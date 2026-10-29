package service

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
)

// 显存驻留路线表：把「哪个模型占哪一侧、吃多少显存、什么时候能共存」写成数据。
//
// 为什么不在代码里写死（原来 main.go 里的 RegisterModel 就是写死的，而且早就过期：
// 登记的是 qwen2.5:7b/13b + sd_xl + whisper，本机实际跑的是 H3、flux、qwen2.5:14b、
// bonsai 27B —— 预算算术全建立在错误的模型表上）。
//
// 一张卡 24GB 的物理事实不会变，但"谁在上面、占多少"会变。所以：
//   - 表是数据（data/model_residency.json），改模型只改表，不改代码
//   - 表里显式写 side：diffusion / language / audio，与 vram.go 的仲裁侧别对齐
//   - vram_mb 用**实测峰值**，不用权重文件体积（体积只是下界，KV cache 不在里面）
//
// 引擎启动时把表灌进 GPUScheduler 并打一行路线摘要：出了 OOM，
// 第一眼看的就是这行——它必须是人能读懂的，而不是一串数字。

// ResidencyModel 一个模型的驻留画像。
type ResidencyModel struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // llm / vision / text2image / video / asr / tts / upscale
	Side   string `json:"side"` // diffusion / language / audio
	VRAMMB int    `json:"vram_mb"`
	Note   string `json:"note,omitempty"`
	// Cold seconds：冷加载耗时（秒）。用于「值不值得为省一次卸载而共存」的决策。
	ColdSeconds int `json:"cold_seconds,omitempty"`
}

// ResidencyRoute 整张卡的驻留路线。
type ResidencyRoute struct {
	TotalVRAMMB int              `json:"total_vram_mb"`
	Models      []ResidencyModel `json:"models"`
	Path        string           `json:"-"`
}

// DefaultResidencyRoute 本机（单卡 24GB）的实测画像。
// 数值来自 2026-10 的实测/日志，不是权重体积。
func DefaultResidencyRoute() *ResidencyRoute {
	return &ResidencyRoute{
		TotalVRAMMB: 24576,
		Models: []ResidencyModel{
			{Name: "minimax_h3", Type: "video", Side: sideDiffusion, VRAMMB: 20000, ColdSeconds: 180,
				Note: "H3 音视频联合生成（8189 独立工作树）；608×352×124 帧实测峰值 21.8GB"},
			{Name: "flux1-schnell-fp8", Type: "text2image", Side: sideDiffusion, VRAMMB: 17000, ColdSeconds: 183,
				Note: "主实例 8188；冷加载 183s、热生成 3s——这就是共存优化的全部动机"},
			{Name: "moss_image", Type: "text2image", Side: sideDiffusion, VRAMMB: 12000, ColdSeconds: 120,
				Note: "MOSS 专用工作树 8190（与 H3 互斥）"},
			{Name: "qwen2.5:14b", Type: "llm", Side: sideLanguage, VRAMMB: 9000, ColdSeconds: 30,
				Note: "语言侧主力（分镜规划/提示词优化）；q4 量化"},
			{Name: "dsh-lens-8b", Type: "vision", Side: sideLanguage, VRAMMB: 6200, ColdSeconds: 25},
			{Name: "qwen3-vl:2b-instruct", Type: "vision", Side: sideLanguage, VRAMMB: 2000, ColdSeconds: 10},
			{Name: "whisper-large-v3", Type: "asr", Side: sideAudio, VRAMMB: 3000, ColdSeconds: 20,
				Note: "台词验词/人声抽检；可由外部 ASR 服务承担"},
			{Name: "audio8-tts-0.6b", Type: "tts", Side: sideAudio, VRAMMB: 2400, ColdSeconds: 40,
				Note: "念稿音轨；默认走 CPU 服务 :8103"},
		},
	}
}

// LoadResidencyRoute 读 data/model_residency.json；缺失或坏掉时回退到内置画像。
// 回退是明确的：打日志说清用的是内置表，绝不静默。
func LoadResidencyRoute(path string) *ResidencyRoute {
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[residency] 未找到 %s，使用内置驻留画像（%d 个模型）", path, len(DefaultResidencyRoute().Models))
		return DefaultResidencyRoute()
	}
	var r ResidencyRoute
	if err := json.Unmarshal(raw, &r); err != nil {
		log.Printf("[residency] %s 解析失败（%v），使用内置驻留画像", path, err)
		return DefaultResidencyRoute()
	}
	if len(r.Models) == 0 {
		log.Printf("[residency] %s 没有模型条目，使用内置驻留画像", path)
		return DefaultResidencyRoute()
	}
	if r.TotalVRAMMB <= 0 {
		r.TotalVRAMMB = 24576
	}
	r.Path = path
	return &r
}

// Apply 把路线灌进调度器。返回实际注册的模型数。
func (r *ResidencyRoute) Apply(s *GPUScheduler) int {
	if s == nil || r == nil {
		return 0
	}
	n := 0
	for _, m := range r.Models {
		if strings.TrimSpace(m.Name) == "" || m.VRAMMB <= 0 {
			continue
		}
		typ := m.Type
		if typ == "" {
			typ = "unknown"
		}
		s.RegisterModel(m.Name, typ, m.VRAMMB)
		n++
	}
	return n
}

// Describe 一行可读的路线摘要，启动时打进日志。
func (r *ResidencyRoute) Describe() string {
	bySide := map[string][]string{}
	for _, m := range r.Models {
		side := m.Side
		if side == "" {
			side = "unknown"
		}
		bySide[side] = append(bySide[side], fmt.Sprintf("%s(%.1fG)", m.Name, float64(m.VRAMMB)/1024))
	}
	order := []string{sideDiffusion, sideLanguage, sideAudio, "unknown"}
	parts := make([]string, 0, len(order))
	for _, side := range order {
		names := bySide[side]
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		parts = append(parts, side+"："+strings.Join(names, " / "))
	}
	return strings.Join(parts, " ｜ ")
}

// HeaviestMB 该侧最重的模型占多少（换侧等待的目标下限）。
func (r *ResidencyRoute) HeaviestMB(side string) int {
	best := 0
	for _, m := range r.Models {
		if m.Side == side && m.VRAMMB > best {
			best = m.VRAMMB
		}
	}
	return best
}
