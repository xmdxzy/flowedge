package service

import (
	"encoding/json"
	"fmt"
	"os"
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
	Ambience   string  `json:"ambience"`   // 结构化模式：持续环境声描述；富模式：环境声文件路径
	SFX        string  `json:"sfx"`        // 与画面事件绑定的动作音效
	Music      string  `json:"music"`      // 音乐或「无音乐」
	Dialogue   string  `json:"dialogue"`   // 默认「无对白」；有台词时写「身份：原话」
	Transition string  `json:"transition"` // 与下一镜的衔接：cut | dissolve | continue
	Frames     int     `json:"frames"`     // 实际帧数（富模式显式声明；缺省由秒数吸附）

	// ---- 富模式（scripts/shot-film.py 的实战 schema）字段 ----
	// 与结构化模式共用同一个 Shot：两种 plan 只是同一事物的两种写法。
	// 富模式手写整段六段式提示词（质量上限更高），结构化模式字段编译（更省事）。
	Prompt    string     `json:"prompt"`     // 富模式：六段式整段提示词（优先于字段编译）
	Lines     []PlanLine `json:"lines"`      // 富模式：台词表（TTS 念稿 + ASR 验词的依据）
	Lead      float64    `json:"lead"`       // 富模式：首句前导静音（秒；≤0 时按 0.25）
	Beat      string     `json:"beat"`       // 叙事功能名（setup/clash/finish…），节奏权重的键
	RefImages *bool      `json:"ref_images"` // 富模式：显式 false = 本段不用参考图（会换脸，门要拦）

	AmbienceGain float64 `json:"ambience_gain"` // 富模式：环境声混入增益（缺省 0.85）
}

// PlanLine 一句台词（富模式台词表）
type PlanLine struct {
	Speaker string  `json:"speaker"`
	Text    string  `json:"text"`
	Speed   float64 `json:"speed"` // 语速倍率（缺省 1.0）
	Pause   float64 `json:"pause"` // 句间停顿秒数（缺省 0.35，对齐 dub-track.py）
}

// UnmarshalJSON 台词行的缺省值必须和 dub-track.py 逐字对齐：
// speed 缺省 1.0、pause 缺省 0.35。用指针字段区分「没给」与「给了 0」——
// 直接给零值会把「没给 pause」静默变成「不停顿」。
func (l *PlanLine) UnmarshalJSON(b []byte) error {
	var raw struct {
		Speaker string   `json:"speaker"`
		Text    string   `json:"text"`
		Speed   *float64 `json:"speed"`
		Pause   *float64 `json:"pause"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	l.Speaker = raw.Speaker
	l.Text = raw.Text
	l.Speed = 1.0
	if raw.Speed != nil {
		l.Speed = *raw.Speed
	}
	l.Pause = 0.35
	if raw.Pause != nil {
		l.Pause = *raw.Pause
	}
	return nil
}

// HasDialogue 这个镜头有没有台词（两种模式统一判定：
// 富模式看台词表，结构化模式看 dialogue 字段是不是「无对白」）。
func (sh *Shot) HasDialogue() bool {
	if len(sh.Lines) > 0 {
		for _, ln := range sh.Lines {
			if strings.TrimSpace(ln.Text) != "" {
				return true
			}
		}
	}
	d := strings.TrimSpace(sh.Dialogue)
	return d != "" && !strings.Contains(d, "无对白") && !strings.EqualFold(d, "none")
}

// DialogueText 这个镜头要念的原话（富模式取台词表第一句；
// 结构化模式取「身份：原话」冒号后的部分，与 CompileShotPrompt 同一套切分）。
func (sh *Shot) DialogueText() string {
	for _, ln := range sh.Lines {
		if t := strings.TrimSpace(ln.Text); t != "" {
			return t
		}
	}
	d := strings.TrimSpace(sh.Dialogue)
	if d == "" || strings.Contains(d, "无对白") || strings.EqualFold(d, "none") {
		return ""
	}
	// 用 rune 切片：中文冒号是 3 字节，按 byte+1 截会切出乱码
	if r := []rune(d); len(r) > 0 {
		for i, c := range r {
			if (c == '：' || c == ':') && i < len(r)-1 {
				return strings.TrimSpace(string(r[i+1:]))
			}
		}
	}
	return d
}

// ShotPlan 整条片子的分镜表
type ShotPlan struct {
	Title     string `json:"title"`
	Style     string `json:"style"`     // 全局风格锚（逐镜重复，防风格漂移）
	Character string `json:"character"` // 角色锁定描述（逐镜完整重复，H3 无角色记忆）
	Ambience  string `json:"ambience"`  // 全集统一的底噪（结构化：描述；富模式：wav 文件路径）

	// ---- 富模式顶层字段 ----
	Characters []string               `json:"characters"` // 定妆图路径（身份由图像锁，不靠文字）
	RefAudio   []string               `json:"ref_audio"`  // 参考音频（音色基准，≤3）
	Width      int                    `json:"width"`      // 富模式可声明分辨率
	Height     int                    `json:"height"`
	Gates      map[string]interface{} `json:"gates"`    // 五层控制门（真相源，spec 与运行期都读它）
	Segments   []Shot                 `json:"segments"` // shots 的富模式别名
	Shots      []Shot                 `json:"shots"`
}

// AmbienceFile 解析环境声文件路径：段级 > plan 级，且必须是真实存在的文件。
// 结构化模式里 Ambience 是文字描述（「远处持续的环境底噪」），这个方法返回空——
// 两种模式共用一个字段，用「文件是否存在」判别，不靠猜。
func (p *ShotPlan) AmbienceFile(sh Shot) string {
	for _, cand := range []string{sh.Ambience, p.Ambience} {
		c := strings.TrimSpace(cand)
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// ParseShotPlan 解析 LLM 或用户给的分镜 JSON（容忍 ```json 包裹与前后废话）
//
// 兼容两种 schema：
//   - 结构化模式（storyboard 节点产出）：shots[] + 字段编译成提示词
//   - 富模式（scripts/shot-film.py 实战 schema）：segments[] + 手写六段式提示词
//   - characters 定妆图 + lines 台词表 + gates 五层门
//
// 两者只是同一事物的两种写法，解析后统一成 ShotPlan.Shots。
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
	// 富模式别名合并：segments 与 shots 二选一，两个都给时以 shots 为准。
	// 合并后清空别名——下游只认 Shots 一个真相源（两处并存必漂移）。
	if len(p.Shots) == 0 && len(p.Segments) > 0 {
		p.Shots = p.Segments
	}
	p.Segments = nil
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
		// 富模式显式声明帧数时**不许**吸附覆盖：总长/切点的数学全部建立在
		// 声明值上，静默改动会让门检结论与成片对不上。缺省才按秒数吸附。
		if sh.Frames <= 0 {
			sh.Frames = SnapFrames(sh.Seconds)
		}
		if sh.Dialogue == "" {
			sh.Dialogue = "无对白"
		}
		if sh.Ambience == "" {
			sh.Ambience = p.Ambience
		}
		if sh.Lead <= 0 {
			sh.Lead = 0.25 // 对齐 shot-film.py 的 seg.get("lead") or 0.25
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
