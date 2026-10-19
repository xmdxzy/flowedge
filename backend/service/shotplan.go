package service

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 分镜表（ShotPlan）
//
// 依据：ai-short-film / aigc-production-flow / Moyin Creator / Jellyfish / ArcReel
// 这几个开源短剧流水线的共同骨架，以及影视视听语言的基本规则：
//
//	剧本 → 角色/场景锁定 → 分镜表（每镜带景别·角度·轴线·运镜·衔接） → 逐镜生成 → 按衔接装配
//
// 关键差异（相对上一版「均分 5 秒 + 全部末帧续接」）：
//  1. 每镜时长独立，按内容需要给（3–8 秒），再吸附到 H3 的 17k+5 帧网格
//  2. 衔接方式逐镜声明：cut（硬切，换机位）/ dissolve（叠化）/ continue（末帧续接）
//     —— 多角度阐述必须用 cut，只有「同一镜头延续」才用 continue
//  3. 轴线（axis）写进每一镜：AI 逐镜生成没有跨镜记忆，不逐镜焊住左右关系就会越轴
//  4. 声音只作环境声/音效层，台词默认「无对白」，从根上掐掉乱语

// Shot 一个镜头
type Shot struct {
	ID         int     `json:"id"`
	Seconds    float64 `json:"seconds"`    // 期望时长（会被吸附到 17k+5 帧网格）
	ShotSize   string  `json:"shot_size"`  // 大远景/远景/全景/中景/中近景/近景/特写/大特写
	Angle      string  `json:"angle"`      // 平视/仰拍/俯拍/荷兰角
	Axis       string  `json:"axis"`       // 轴线站位：A侧/B侧/正面（逐镜重复，防越轴）
	Camera     string  `json:"camera"`     // 固定/缓慢推近/缓慢拉远/横移/跟拍/摇
	Action     string  `json:"action"`     // 单一可见动作 + 收尾节拍
	Light      string  `json:"light"`      // 光线与色温
	Ambience   string  `json:"ambience"`   // 持续环境声（全集共用同一句更稳）
	SFX        string  `json:"sfx"`        // 与画面事件绑定的动作音效
	Music      string  `json:"music"`      // 音乐或「无音乐」
	Dialogue   string  `json:"dialogue"`   // 默认「无对白」；有台词时写「身份：原话」
	Transition string  `json:"transition"` // 与下一镜的衔接：cut | dissolve | continue
	Frames     int     `json:"frames"`     // 实际帧数（吸附后回填）
}

// ShotPlan 整条片子的分镜表
type ShotPlan struct {
	Title     string `json:"title"`
	Style     string `json:"style"`     // 全局风格锚（逐镜重复，防风格漂移）
	Character string `json:"character"` // 角色锁定描述（逐镜完整重复，H3 无角色记忆）
	Ambience  string `json:"ambience"`  // 全集统一的底噪
	Shots     []Shot `json:"shots"`
}

// ParseShotPlan 解析 LLM 或用户给的分镜 JSON（容忍 ```json 包裹与前后废话）
func ParseShotPlan(raw string) (*ShotPlan, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 {
		s = s[:j+1]
	}
	var p ShotPlan
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil, fmt.Errorf("分镜 JSON 解析失败：%w", err)
	}
	if len(p.Shots) == 0 {
		return nil, fmt.Errorf("分镜表为空")
	}
	// 兜底与规范化
	if p.Ambience == "" {
		p.Ambience = "远处持续的环境底噪"
	}
	for i := range p.Shots {
		sh := &p.Shots[i]
		if sh.ID == 0 {
			sh.ID = i + 1
		}
		sh.Frames = SnapFrames(sh.Seconds)
		if sh.Dialogue == "" {
			sh.Dialogue = "无对白"
		}
		if sh.Ambience == "" {
			sh.Ambience = p.Ambience
		}
		sh.Transition = normalizeTransition(sh.Transition)
		if sh.Axis == "" {
			sh.Axis = "正面"
		}
	}
	return &p, nil
}

// SnapFrames 把秒数吸附到 H3 的 17k+5 帧网格
//
// 官方：帧数必须落在 17k+5（124=5.17s、141=5.88s、158=6.58s…），
// 不在网格上采样器会直接报错。所以「可变时长」不是任意时长，而是网格上的点。
func SnapFrames(seconds float64) int {
	if seconds <= 0 {
		return 124
	}
	target := seconds * 24.0
	k := int((target - 5) / 17)
	if k < 4 {
		k = 4 // 最短 73 帧 ≈ 3.04s
	}
	if k > 21 {
		k = 21 // 最长 362 帧 ≈ 15.08s（训练上限）
	}
	return 17*k + 5
}

func normalizeTransition(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cut", "硬切":
		return "cut"
	case "dissolve", "叠化", "fade":
		return "dissolve"
	case "continue", "续接", "延续":
		return "continue"
	}
	return "cut"
}

// CompileShotPrompt 把结构化字段编译成 H3 提示词。
//
// 为什么用模板而不是让 LLM 自由写：字段化之后每个维度都可控、可回查，
// 也不会再出现「模型把台词编出来」这类漂移。LLM 只负责规划分镜表。
func CompileShotPrompt(plan *ShotPlan, sh Shot, withFirstFrame bool) string {
	var b strings.Builder

	// 0) 官方三段式头部：字段空着，模型就自行脑补（含即兴人声）
	b.WriteString("integrated_multimodal_description: [Shot " + strconv.Itoa(sh.ID) + "] ")

	// 1) 景别 + 角度 + 轴线 + 运镜（影视语言的四要素，逐镜写死）
	parts := make([]string, 0, 6)
	if sh.ShotSize != "" {
		parts = append(parts, sh.ShotSize)
	}
	if sh.Angle != "" {
		parts = append(parts, sh.Angle+"机位")
	}
	if sh.Axis != "" {
		parts = append(parts, "保持轴线"+sh.Axis)
	}
	if sh.Camera != "" {
		parts = append(parts, sh.Camera)
	}
	b.WriteString(strings.Join(parts, "，"))
	b.WriteString("。")

	// 2) 主体与场景（角色/风格逐镜完整重复 —— H3 没有跨镜记忆）
	if plan.Character != "" {
		b.WriteString(plan.Character)
		b.WriteString("。")
	}
	if plan.Style != "" {
		b.WriteString("风格：" + plan.Style + "。")
	}
	if withFirstFrame {
		b.WriteString("画面首位与参考首帧一致（人物服装、道具、光线不得改变）。")
	}

	// 3) 动作：单一可见动作 + 收尾
	if sh.Action != "" {
		b.WriteString("动作：" + sh.Action + "。")
	}

	// 4) 光线
	if sh.Light != "" {
		b.WriteString("光线：" + sh.Light + "。")
	}

	// 5) 台词：绝不写否定式禁言（cfg=1 无负分支，写「不要说话」反而会把概念带进来）。
	//    没有台词时改用可见的口型事实，把"不说话"表达成画面上看得见的状态。
	//    顺序要求：这段必须写在 overall_soundscape **之前**，否则会拼到声音那一行上。
	d := strings.TrimSpace(sh.Dialogue)
	if d == "" || strings.Contains(d, "无对白") || strings.EqualFold(d, "none") {
		b.WriteString(" 两人嘴唇紧闭、咬紧牙关，只用鼻子呼吸，呼吸声随动作加重。")
	} else {
		line := d
		// 用 rune 切片：中文冒号是 3 字节，按 byte+1 截会切出乱码
		if r := []rune(d); len(r) > 0 {
			for i, c := range r {
				if (c == '：' || c == ':') && i < len(r)-1 {
					line = strings.TrimSpace(string(r[i+1:]))
					break
				}
			}
		}
		b.WriteString(" (S1) says <d>[Chinese] " + line + "</d>")
	}

	// 6) 声音：环境声（持续）+ 音效 —— 结构化写进 overall_soundscape
	//
	// 官方 prompt guide：声音必须落在独立字段里，空着模型就自行脑补（含即兴人声）。
	snd := make([]string, 0, 3)
	if sh.Ambience != "" {
		snd = append(snd, sh.Ambience+"（持续）")
	}
	if sh.SFX != "" {
		snd = append(snd, sh.SFX)
	}
	if len(snd) > 0 {
		b.WriteString("\noverall_soundscape: " + strings.Join(snd, "；") + "。")
	} else {
		b.WriteString("\noverall_soundscape: 持续的环境底噪。")
	}

	// 7) 配乐字段：不要音乐就写 N/A，不留空、不写「无音乐」
	music := strings.TrimSpace(sh.Music)
	if music == "" || strings.Contains(music, "无音乐") || strings.EqualFold(music, "none") {
		b.WriteString("\nnon_diegetic_music: N/A")
	} else {
		b.WriteString("\nnon_diegetic_music: " + music)
	}

	// 说明：旧版末尾还有一句「禁止文字、字幕、logo、水印、分屏、越轴、镜头反向」。
	// 官方明确 cfg=1 没有负分支，否定清单可能把概念带进来，已移除；
	// 越轴靠逐镜 axis 正面锁死，水印靠事后抽帧检查。
	return b.String()
}
