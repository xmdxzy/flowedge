package service

import (
	"context"
	"fmt"
	"strings"

	"flowedge/backend/model"
)

// H3 提示词优化
//
// 依据：ComfyUI 官方 MiniMax H3 文档（docs.comfy.org/tutorials/video/minimax/minimax-h3）
// 与社区实测指南（reapi.ai / aifinitee / minimax-h3.app prompt guide）。
// 核心结论：H3 要的不是「画面描述」，而是「一条有起止的镜头指令 + 声音分镜」，
// 好提示词对成片的影响远大于换 seed——这正是把优化做成引擎内置环节的原因。

// h3PromptInstruction 优化器系统指令（规则全部可追溯到官方/社区指南）
//
// 2026-10-19 重写：官方 prompt guide 明确了三段式字段结构，并且明确要求用英文：
//
//	integrated_multimodal_description / overall_soundscape / non_diegetic_music
//
// 「胡言乱语」的直接原因是后两个字段空着——H3 会把没填的字段自行脑补，
// 包括让它的人声轨即兴编一串含糊台词。所以这里不只是"写好看"，是"把字段填满"。
const h3PromptInstruction = `You are the prompt director for MiniMax H3, a joint audio-video model. Rewrite the user's idea into ONE prompt using the official three-field format, in ENGLISH:

integrated_multimodal_description: [Shot 1] ...
overall_soundscape: ...
non_diegetic_music: ...

Rules:
1. integrated_multimodal_description: exactly one continuous shot with visible progression and one dominant action that reaches a visible end state. Camera gets ONE main move with amplitude and speed (e.g. "slow small push-in"), never vague words like "dynamic camera". Repeat the character's full appearance and clothing instead of pronouns.
2. Speaker rule (the single most important rule): H3 voices people by default. Unless the user explicitly supplies spoken lines, the shot must contain NO speech. Achieve this POSITIVELY, not with ban lists — cfg is 1.0 and there is no negative branch, so banned concepts can leak in:
   - describe mouth state as visible fact: "lips closed and still", "jaw tight, breathing hard through the nose"
   - describe the action as silent exertion: "a sharp inhale", "a wordless grunt", "cloth snapping taut"
   Never write "no dialogue", "silent", "without speaking", "do not talk" as negations.
3. overall_soundscape: 1-4 sentences, ambient + SFX + non-verbal only. Bind every sound to a visible event, with distance and direction ("continuous wind over broken stone behind him", "a low crackle as the blade charges"). This field is what stops the model from improvising voices — never leave it empty.
4. non_diegetic_music: score only, 1-3 sentences. If the user did not ask for music, write exactly "N/A". Never invent background music.
5. If the user DID supply lines: put them inside <d> tags with speaker IDs, e.g. (S1) says <d>[English] Hold the line.</d> Keep punctuation inside the tag, max 10 characters of content per line, and state that the lips move only for that line.
6. No unverifiable adjectives ("cinematic", "epic", "beautiful"); replace with concrete material and light behaviour.
7. Output ONLY the three fields, no explanation, no Markdown, no bullet list, no surrounding quotes. Soundscape and music stay in English; any on-screen Chinese text stays verbatim in its original language.

The user gives: the raw idea + target duration. Scale the number of events to that duration.`

// ensureRef2VAPrompt 把普通提示词补成 ref2va 的六段式结构。
//
// 官方 ref 指南要求六段：subject_definitions / summary / retention_analysis /
// detailed_description / overall_soundscape / non_diegetic_music。
// 少写 subject_definitions 与 retention_analysis，模型就不知道"参考音频要照抄"，
// 会把它当风格提示重新生成一段伪中文——这正是"念稿变成乱语"的机制。
//
// 关键约定：一旦给了参考音频，就用 fully_copy（参考音频 = 成片完整音轨），
// 因此整体声景必须写 N/A，否则模型会往音轨里加东西。环境声要在送进来之前
// 就混进 wav（TTS 人声 + 环境底噪），这样成片声音完全可控。
func ensureRef2VAPrompt(prompt string, nImg, nAud int) string {
	low := strings.ToLower(prompt)
	if strings.Contains(low, "subject_definitions") {
		return prompt
	}
	desc := h3FieldValue(prompt, "integrated_multimodal_description")
	if desc == "" {
		desc = h3FieldValue(prompt, "detailed_description")
	}
	if desc == "" {
		desc = strings.TrimSpace(prompt)
	}
	sound := h3FieldValue(prompt, "overall_soundscape")
	music := h3FieldValue(prompt, "non_diegetic_music")

	var b strings.Builder
	b.WriteString("subject_definitions: ")
	parts := make([]string, 0, nImg+nAud)
	for i := 1; i <= nImg; i++ {
		parts = append(parts, fmt.Sprintf("<Picture %d> defines the visual identity, wardrobe and props of the characters in the shot.", i))
	}
	for j := 1; j <= nAud; j++ {
		parts = append(parts, fmt.Sprintf("<Audio %d> is the spoken performance to be lip-synced.", j))
	}
	if len(parts) == 0 {
		parts = append(parts, "<Picture 1> defines the visual identity of the characters in the shot.")
	}
	b.WriteString(strings.Join(parts, " "))
	b.WriteString("\n")

	tasks := "[reference generation"
	if nAud > 0 {
		tasks += " + audio reuse"
	}
	tasks += "]"
	b.WriteString("summary: " + tasks + " The target video follows the reference images for identity")
	if nAud > 0 {
		b.WriteString(" and reuses the reference audio as its final audio track")
	}
	b.WriteString(".\n")

	b.WriteString("retention_analysis: ")
	ra := make([]string, 0, nImg+nAud)
	for i := 1; i <= nImg; i++ {
		ra = append(ra, fmt.Sprintf("<Picture %d> (appears in [Shot 1]): fully_preserved - identity, wardrobe and props are retained.", i))
	}
	for j := 1; j <= nAud; j++ {
		ra = append(ra, fmt.Sprintf("<Audio %d>: fully_copy - is reused 1:1 as the target video's complete final audio track.", j))
	}
	b.WriteString(strings.Join(ra, " "))
	b.WriteString("\n")

	b.WriteString("detailed_description: " + desc + "\n")
	if nAud > 0 {
		b.WriteString("overall_soundscape: N/A - the complete final audio track is the copied reference audio.\n")
	} else if sound != "" {
		b.WriteString("overall_soundscape: " + sound + "\n")
	} else {
		b.WriteString("overall_soundscape: " + h3DefaultSoundscape + "\n")
	}
	if nAud > 0 {
		b.WriteString("non_diegetic_music: N/A")
	} else if music != "" {
		b.WriteString("non_diegetic_music: " + music)
	} else {
		b.WriteString("non_diegetic_music: N/A")
	}
	return b.String()
}

// h3FieldValue 取出三段式里某个字段的值（到行尾）；没有就返回空。
func h3FieldValue(prompt, field string) string {
	low := strings.ToLower(prompt)
	idx := strings.Index(low, strings.ToLower(field))
	if idx < 0 {
		return ""
	}
	rest := prompt[idx+len(field):]
	rest = strings.TrimPrefix(strings.TrimSpace(rest), ":")
	if i := strings.IndexAny(rest, "\r\n"); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

// setH3Field 覆盖某个字段的值；字段不存在则追加。用于让多段共用同一句环境声。
func setH3Field(prompt, field, value string) string {
	if strings.TrimSpace(value) == "" {
		return prompt
	}
	low := strings.ToLower(prompt)
	idx := strings.Index(low, strings.ToLower(field))
	if idx < 0 {
		return strings.TrimRight(prompt, " \n") + "\n" + field + ": " + value
	}
	start := idx + len(field)
	rest := prompt[start:]
	colon := strings.Index(rest, ":")
	if colon < 0 || colon > 3 {
		return strings.TrimRight(prompt, " \n") + "\n" + field + ": " + value
	}
	tail := rest[colon+1:]
	end := len(tail)
	if i := strings.IndexAny(tail, "\r\n"); i >= 0 {
		end = i
	}
	return prompt[:start] + ": " + value + tail[end:]
}

// ensureDialogueFree 给「本镜不该说话」的场景加一道正面约束。
//
// 官方说明：cfg=1 没有负分支，写「不要说话」无效甚至反效果；有效的是
// 把口型状态写成可见事实（嘴唇闭合/咬牙/急促鼻息）。
// 只在提示词完全没有台词标记 <d> 时生效，且写进画面描述字段内（不是文末）。
func ensureDialogueFree(prompt string) string {
	low := strings.ToLower(prompt)
	if strings.Contains(low, "<d>") {
		return prompt
	}
	if strings.Contains(low, "lips closed") || strings.Contains(low, "jaw tight") ||
		strings.Contains(low, "mouth stays shut") {
		return prompt
	}
	const mouth = " Lips stay closed and still, jaw tight, breath audible through the nose."
	desc := h3FieldValue(prompt, "integrated_multimodal_description")
	if desc == "" {
		return strings.TrimRight(prompt, " \n") + mouth
	}
	return setH3Field(prompt, "integrated_multimodal_description", desc+mouth)
}

// h3DefaultSoundscape 当优化器漏填 overall_soundscape 时的兜底。
//
// 为什么要有兜底：空字段 = 模型自行填充 = 人声乱语。宁可给一句泛化环境声，
// 也不能让字段空着。措辞按官方要求用正面描述，不用否定式清单。
const h3DefaultSoundscape = "Continuous ambient sound of the setting, mixed with the physical sounds of the visible action; the only sound sources are the environment and what is visibly happening."

// ensureH3AudioFields 保证三段式字段存在（确定性兜底，不依赖 LLM 是否听话）。
//
// 空着的 audio 字段会被模型脑补成人声，这是"胡言乱语"最直接的来源。
// 返回 (提示词, 是否补过)。
func ensureH3AudioFields(prompt string) (string, bool) {
	p := strings.TrimSpace(prompt)
	patched := false

	if !strings.Contains(strings.ToLower(p), "integrated_multimodal_description") {
		p = "integrated_multimodal_description: " + p
		patched = true
	}
	lower := strings.ToLower(p)
	if !strings.Contains(lower, "overall_soundscape") {
		p = strings.TrimRight(p, " \n") + "\noverall_soundscape: " + h3DefaultSoundscape
		patched = true
	}
	if !strings.Contains(lower, "non_diegetic_music") {
		p = strings.TrimRight(p, " \n") + "\nnon_diegetic_music: N/A"
		patched = true
	}
	return p, patched
}

// OptimizePromptForH3 把原始想法改写成 H3 可用的镜头指令。
// 走语言侧 LLM（调用方负责先 enterSide(sideLanguage) 让扩散侧让位）。
func (e *EngineV2) OptimizePromptForH3(ctx context.Context, raw string, seconds int) (string, error) {
	if e.llm == nil {
		return "", fmt.Errorf("LLM 服务不可用，无法优化提示词")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("原始提示词为空")
	}
	if seconds <= 0 {
		seconds = 5
	}
	user := fmt.Sprintf("目标时长：%d 秒\n原始想法：%s", seconds, raw)
	modelName := e.llm.Model
	out, err := e.llm.ChatWithModel(fmt.Sprintf("%s\n\n%s", h3PromptInstruction, user), modelName)
	if err != nil {
		return "", err
	}
	out = cleanOptimizedPrompt(out)
	if out == "" {
		return "", fmt.Errorf("提示词优化返回空结果")
	}
	// 确定性兜底：LLM 漏填的声音字段由这里补齐，不依赖模型是否听话
	out, _ = ensureH3AudioFields(out)
	return out, nil
}

// stripMarkdownLead 去掉模型带出的 Markdown 前缀（### 、- 、* 、1. 等）
//
// 实测 qwen2.5 会把分镜写成「### 拍1 …」，这些符号进了 H3 提示词会降低指令清晰度。
func stripMarkdownLead(s string) string {
	s = strings.TrimSpace(s)
	for {
		changed := false
		for _, p := range []string{"#", "*", "-", "•", ">"} {
			if strings.HasPrefix(s, p) {
				s = strings.TrimSpace(strings.TrimPrefix(s, p))
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	r := []rune(s)
	if len(r) > 2 && r[0] >= '0' && r[0] <= '9' && (r[1] == '.' || r[1] == '、' || r[1] == ')') {
		s = strings.TrimSpace(string(r[2:]))
	}
	// 尾部也要清：模型常写 "**加粗**" 这类闭合标记
	s = strings.TrimRight(s, "*# ")
	return strings.TrimSpace(s)
}

// cleanOptimizedPrompt 去掉模型偶尔带的引号/前缀/Markdown
//
// 注意：三段式字段（integrated_multimodal_description / overall_soundscape /
// non_diegetic_music）**必须保留换行**——压成一段会让字段边界消失，
// 等于又回到"没写声音字段"的老问题。
func cleanOptimizedPrompt(s string) string {
	s = strings.TrimSpace(s)
	// 顺序很重要：先清 Markdown，再清中文前缀，最后再清一次
	// （模型常输出 "### 提示词：…"，反过来清不掉）
	s = stripMarkdownLead(s)
	for _, p := range []string{"提示词：", "优化后：", "优化后提示词："} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	s = stripMarkdownLead(s)
	s = strings.Trim(s, "\"“”` \n")

	structured := strings.Contains(strings.ToLower(s), "overall_soundscape") ||
		strings.Contains(strings.ToLower(s), "non_diegetic_music")

	lines := make([]string, 0, 6)
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\\"))
		if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "- ") || strings.HasPrefix(ln, "* ") {
			continue
		}
		lines = append(lines, ln)
	}
	if structured {
		s = strings.Join(lines, "\n")
	} else {
		// 普通镜头指令：官方建议单段
		s = strings.Join(lines, "")
	}
	return strings.TrimSpace(s)
}

// execPromptOptNode 独立的「提示词优化」节点：
// 上游 text（或自身 text 字段）→ 按 mode 优化 → text。
// mode 目前只有 h3；image/raw 直接透传（留扩展位）。
func (e *EngineV2) execPromptOptNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	mode, _ := node.Data["mode"].(string)
	if mode == "" {
		mode = "h3"
	}

	raw, _ := node.Data["text"].(string)
	if strings.TrimSpace(raw) == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok && strings.TrimSpace(t) != "" {
					raw = t
					break
				}
			}
		}
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("提示词优化缺少输入：连上游文本或填 text")
	}

	seconds := 5
	if v, ok := node.Data["seconds"].(float64); ok && int(v) > 0 {
		seconds = int(v)
	}

	release := e.enterSide(sideLanguage)
	defer release()

	opt, err := e.OptimizePromptForH3(ctx, raw, seconds)
	if err != nil {
		// 优化失败不炸工作流：透传原文，让用户看到原因后自行决定
		return map[string]interface{}{
			"text":     raw,
			"original": raw,
			"mode":     mode,
			"error":    err.Error(),
		}, nil
	}
	return map[string]interface{}{
		"text":     opt,
		"original": raw,
		"mode":     mode,
	}, nil
}

// beatSplitInstruction 分镜拆解指令：长片（segments>1）必备。
// 关键要求：段间首尾衔接 + 逐段重复角色外貌（H3 没有角色记忆，一致性只能靠文本+首帧引导）。
const beatSplitInstruction = `你是分镜师。把下面这条镜头总指令拆成 N 段（每段约 5 秒），规则：

1. 每段是一条独立的 H3 提示词，可直接执行；段与段之间动作首尾衔接（下一段的开头承接上一段结尾的画面状态）
2. 每段完整重复角色外貌与衣着描述，不要用「他/她」指代；武器、法术光效的颜色每段保持一致
3. 每段只保留一个主导动作 + 可见收尾；镜头每段只给一个主运动
4. 每段自带 1-2 层声音，绑定画面可见事件；**所有段共用同一句环境声描述（写「持续」）**，
   避免每段重启音景；音乐只在指定段出现，其余段写「无音乐」；没有台词就写「无对白」，不要发明台词（否则会生成含糊乱语）
5. 段间运动衔接：下一段的镜头运动方向与上一段结尾**同向同速**，不要反向；主体位置延续上一段结尾
6. N 段要构成完整的起承转合：开场建立、升级对抗、高潮爆发、收尾定格
7. 只输出 N 行，一行一段，不要编号、不要解释、不要 Markdown`

// SplitPromptIntoBeats 把优化后的总指令拆成 N 段衔接提示词。
// 走语言侧 LLM（调用方负责 enterSide(sideLanguage)）。
func (e *EngineV2) SplitPromptIntoBeats(prompt string, n int) ([]string, error) {
	if e.llm == nil {
		return nil, fmt.Errorf("LLM 服务不可用，无法拆分分镜")
	}
	if n < 2 {
		return []string{prompt}, nil
	}
	user := fmt.Sprintf("拆成 %d 段。镜头总指令：%s", n, prompt)
	out, err := e.llm.ChatWithModel(fmt.Sprintf("%s\n\n%s", beatSplitInstruction, user), e.llm.Model)
	if err != nil {
		return nil, err
	}
	var beats []string
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		ln = strings.ReplaceAll(ln, "`", "")
		ln = stripMarkdownLead(ln)
		ln = strings.TrimSpace(strings.TrimPrefix(ln, "提示词："))
		ln = strings.Trim(ln, "\"“”'")
		if len([]rune(ln)) > 15 {
			beats = append(beats, ln)
		}
	}
	if len(beats) < n {
		return nil, fmt.Errorf("分镜拆解只得到 %d 段（要 %d）", len(beats), n)
	}
	return beats[:n], nil
}
