package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// H3 提示词优化
//
// 依据：ComfyUI 官方 MiniMax H3 文档（docs.comfy.org/tutorials/video/minimax/minimax-h3）
// 与社区实测指南（reapi.ai / aifinitee / minimax-h3.app prompt guide）。
// 核心结论：H3 要的不是「画面描述」，而是「一条有起止的镜头指令 + 声音分镜」，
// 好提示词对成片的影响远大于换 seed——这正是把优化做成引擎内置环节的原因。

// h3PromptInstruction 优化器系统指令（T2VA / I2VA / FL2VA 基础模式）
//
// 2026-10-20 按 MiniMax 官方《Video Prompt Writing Guide》原文重写：
// 规则不再是"社区经验"，逐条可对照官方文档。关键变化：
//   - [Shot 1] 内部第一句必须先声明风格（官方 4.1）
//   - 镜头运动必须写 type + amplitude + speed 三要素（官方 4.3 表格）
//   - 说话人 ID 前必须有可念的音色描述，且身份/动作/语气必须在 <d> 外（官方 4.4）
//   - overall_soundscape 只写环境/动作/非语言人声，台词不重复（官方 4.6）
//   - non_diegetic_music 只写配器/速度/动态，禁情绪词（官方 4.7）
//   - 删掉旧版"每句台词 ≤10 字"的自造限制：官方要求台词原样照抄，不改写不重标点
const h3PromptInstruction = `You are the prompt director for MiniMax H3, a joint audio-video model that generates picture and stereo sound in ONE pass. Rewrite the user's idea into the prompt format defined by MiniMax's official Video Prompt Writing Guide.

Output exactly three fields in this order, separated by blank lines, written in ENGLISH (dialogue stays verbatim in its original language inside the <d> tags):

integrated_multimodal_description: [Shot 1] ...
overall_soundscape: ...
non_diegetic_music: ...

RULES

1. SHOT 1 OPENS WITH STYLE. The first sentence inside [Shot 1] declares the visual style and the initial composition, e.g. "Live-action, cinematic, a medium-wide shot frames ...". Pick a style from: Cinematic, live-action, 2D-animated, 3D CG, claymation, watercolour, vintage film.

2. SHOTS AND CUTS. Only [Shot 1] has no timestamp. Every later shot begins with a strictly increasing cut time inside the clip: "[Shot 2] At 00:03.500, the camera cuts to ...". Use "the camera cuts to" / "the shot cuts to" / "the shot transitions to". A cut must add new information about subject, space, state, viewpoint or time; if only the distance or a slight angle changes, use camera motion instead of a cut.

3. CAMERA = TYPE + AMPLITUDE + SPEED. Type must come from: Zoom In/Out, Push In/Pull Out, Pan Left/Right, Truck Left/Right, Tilt Up/Down, Pedestal Up/Down, Arc Shot, Tracking Shot, Static Shot, Shake Slightly/Strongly, POV, Roll Clockwise/Counterclockwise. Amplitude: "with small amplitude" / "with large amplitude". Speed: "at slow speed" / "at fast speed". Write it as a natural clause inside the shot ("The camera pushes in with small amplitude at slow speed toward the folded letter"), never as labels stacked at the end. Omit amplitude or speed only when they are genuinely unremarkable.

4. EVERY DETAIL MUST BE VISIBLE OR AUDIBLE: subject appearance and position, wardrobe and props, environment and lighting, actions and reactions, state changes, camera movement, and the diegetic sound bound to the visible action. No unverifiable adjectives ("epic", "beautiful", "stunning") — replace them with material, light and motion facts.

5. SPEAKERS. Every voice gets a stable ID (S1), (S2) ... assigned in the order voices actually occur; simultaneous speakers use (S1,S2). Immediately before the ID give enough identity for a voice performance: character type, apparent age, gender, on-screen or off-screen, pitch, timbre, pace. Identity, ID, action and delivery all stay OUTSIDE <d>; inside <d> put only the language tag and the exact original words and punctuation:
   The middle-aged gate captain with a low, gravelly voice (S2) says: <d>[Chinese] 皇城，无你名姓。</d>
   Never translate, rewrite or re-punctuate user-supplied lines. Voiceover uses the exact phrase "says in an off-screen voiceover" and must state that the on-screen lips remain closed. A line crossing a cut gets <scenetrans> on both sides plus an explicit "continues across the cut"; speech cut off by the ending gets <cutoff>.

6. overall_soundscape: one paragraph of 1-4 sentences covering ambience, physical action sounds and non-verbal human sounds (wind, rain, footsteps, fabric, impacts, breathing) for the WHOLE video. Dialogue, singing and diegetic music belong in the description, never here. Write "N/A" only when the user asked for complete silence.

7. non_diegetic_music: 1-3 sentences on instrumentation, tempo, rhythm and dynamic change for score the characters cannot hear. No mood words, no explanation of emotional function. Music audible to characters (radio, phone, an instrument on screen) is diegetic and belongs in the description. Write "N/A" when there is no score.

8. SOUND IS GENERATED WITH THE PICTURE, not layered later. Bind every sound to a visible event and give it distance and direction. H3 voices people by default, so an undescribed mouth is a mouth that may start talking: when a shot must stay wordless, state it as a visible fact inside the shot ("lips stay closed and still, jaw tight, audible breath through the nose", "a wordless grunt") rather than a ban list — there is no negative branch to lean on.

9. SCENE LOCK IS IMMUTABLE. A line beginning with "SCENE LOCK:" is a fixed spatial contract for the set: what sits in each of the nine frame cells and where the light comes from are facts that must survive every shot. Copy it into the description field VERBATIM as the sentence immediately after the opening style sentence of [Shot 1]. Never paraphrase, translate, reorder, shorten or merge it into other sentences — it is the only thing that keeps the location identical across separately generated clips.

10. Output ONLY the three fields. No preface, no explanation, no Markdown, no bullet list, no surrounding quotes, no repetition of these rules.

The user supplies the raw idea and the target duration in seconds. Scale the number and length of visible beats to that duration: roughly one beat per 2-3 seconds.`

// h3Ref2VAInstruction 优化器系统指令（全参考模式 Ref2VA）
//
// 依据 MiniMax 官方《Full-Reference Mode Rewrite Output Format Guide》原文。
//
// 旧实现从来没让 LLM 按 Ref2VA 格式写过——它只是把 T2VA 的产物套一层六段式外壳，
// 于是 subject_definitions 里写的是 "<Picture 1> defines the visual identity"，
// 而官方要求：图片只用来定角色/服装/场景时**不得**单列 Picture 条目，
// 必须写成 <Subject N> 并把图片源引在定义里。标签语义错了，身份锁定就靠运气。
const h3Ref2VAInstruction = `You are the prompt director for MiniMax H3 in full-reference mode (Ref2VA). Rewrite the request into MiniMax's official six-section format, using the reference assets listed below.

Output exactly six sections in this order, English everywhere except dialogue and on-screen text (kept verbatim):

subject_definitions:
<Subject 1> is ...
summary:
[task type prefix] ...
retention_analysis:
<Subject 1> (appears in [Shot 1], ...): fully_preserved - ...
detailed_description:
<style sentence> [Shot 1] ...
overall_soundscape:
...
non_diegetic_music:
...

LABEL RULES (most important)
- <Subject N> is reusable VISIBLE content: a person, animal, object, scene, clothing, prop, style, action or pose. One item per line: what it is, which asset it comes from, and the features that must survive.
  <Subject 1> is the exiled heir in <Picture 1>, lean and in his twenties, hair bound in a cloth topknot, charcoal cross-collar robe under a faded crimson cloak, a pale jade plaque at his waist.
- <Picture N> gets its own line ONLY when that image is an actual frame or composition anchor of a shot ("<Picture 2> is the first frame of [Shot 1]"). If the image merely defines a character, scene, costume or style, do NOT create a picture line — cite it inside the <Subject N> definition instead. This is the rule most often broken.
- <Audio N> is a signal that is copied or referenced. Bind it to its speaker: "<Audio 1> is the voice-timbre reference for <Subject 1> (S1)."
- Indices are per type and keep the same meaning across all six sections. Never renumber.

SUMMARY
Start with a bracketed task-type prefix from: keyframe completion, reference generation, video editing, video continuation, audio reuse, audio reference. Combine with " + " when several apply, e.g. [reference generation + audio reuse]. Do not introduce new labels here.

RETENTION_ANALYSIS
One line per label with the fixed markers. Visible content: fully_preserved / partially_preserved / attribute_transfer / weak_reference. Audio: fully_copy / partially_copy / reference / weak_reference. Never write (Sx) in this section.
  <Subject 1> (appears in [Shot 1], [Shot 2]): fully_preserved - <the specific features that survive>.
  <Audio 1>: fully_copy - <Audio 1> is reused 1:1 as the target video's complete final audio track.

DETAILED_DESCRIPTION
The body of the prompt. 350-500 English words for generation tasks; dialogue-dense requests prioritise fitting the full spoken timeline over word count.
- Before [Shot 1], one or two English sentences establishing the overall style, light and palette.
- [Shot 1] has no timestamp; later shots use "[Shot N] At MM:SS.mmm, the camera cuts to ...".
- Each shot must establish: current composition, subject appearance and position, environment and light, actions and state changes, camera movement (type + amplitude + speed), the sound of this moment, and exactly where referenced content appears or takes effect.
- Cite <Subject N> at its first clear appearance with its retained features and its position in frame; afterwards keep the same label without redefining it.
- Speaker form: <Subject 1> (S1) plus voice identity, action and delivery, then says: <d>[Chinese] ...</d>. Only the language tag and the verbatim words go inside <d>.
- Never reduce the body to a plot summary or a list of reference relationships.

SOUND
- overall_soundscape: 1-4 sentences of ambience, physical sounds and non-verbal human sound for the whole video. When a reference audio supplies that layer, describe it as a relationship instead of writing N/A:
  The copied ambience layer from <Audio 1> continues throughout the target video.
- non_diegetic_music: 1-3 sentences of instrumentation, tempo and dynamics, or "N/A". When a reference audio is the score: <Audio 2> is directly reused as the complete audience-only score.

SCENE LOCK
A line beginning with "SCENE LOCK:" is a fixed spatial contract for the set: what occupies each of the nine frame cells and where the light comes from. Copy it VERBATIM into detailed_description as the sentence immediately after the opening style sentences and before [Shot 1]. Never paraphrase, translate, reorder or merge it — every clip generated from this set must carry the identical sentence, otherwise the location drifts between clips.

QUALITY BAR
No unverifiable adjectives, no ban lists, no Markdown, no explanation, no repetition of these rules. Output only the six sections.

FINAL SELF-CHECK — silently fix every item that fails before answering:
- All six section labels are present, in the given order, one per line-block.
- Every reference is cited WITH angle brackets: <Subject 1>, <Picture 1>, <Audio 1>. A bare "Subject 1" without brackets is a labelling error and the model will not link it.
- The summary prefix lists only task types that actually apply. With no source video, never write video continuation or video editing; identity from a still image is reference generation.
- Every camera move names a type from the vocabulary ("The camera pushes in ..."). Do not write "moves forward", "the camera follows", or "glides" — map them to Push In / Tracking Shot / Truck / Pan.
- The one or two sentences before [Shot 1] name the visual style explicitly (Live-action, cinematic, ...).
- detailed_description is at least 350 English words and reads shot by shot, not as a plot summary.`

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
		// LLM 已经按六段式写了：不套壳，但要做两处确定性校正——
		// 任务类型前缀按实际资产重算、有参考音频却没标 fully_copy 时补上
		return patchRefAudioRetention(normalizeSummaryPrefix(prompt, nImg, nAud), nAud)
	}
	desc := h3FieldValue(prompt, "integrated_multimodal_description")
	if desc == "" {
		desc = h3FieldValue(prompt, "detailed_description")
	}
	if desc == "" {
		desc = strings.TrimSpace(prompt)
	}
	// 源提示词自带 overall_soundscape / non_diegetic_music 时必须先剥掉：
	// 否则它们会被原样嵌进 detailed_description，而下面又追加一份权威值，
	// 同一份提示词里出现两个互相矛盾的 overall_soundscape —— 模型就不知道该不该
	// 照抄参考音频（实测 2026-10-19 22:30：ref_audio 接线正确、fully_copy 也在，
	// 但成片音轨仍被重生成，corr 0.22）。
	desc = stripFieldLines(desc)
	sound := h3FieldValue(prompt, "overall_soundscape")
	music := h3FieldValue(prompt, "non_diegetic_music")

	var b strings.Builder
	b.WriteString("subject_definitions:\n")
	parts := make([]string, 0, nImg+nAud)
	// 官方 2.2：参考图只用来定角色/服装/场景时，**不得**单列 <Picture N>，
	// 必须写成 <Subject N> 并把图片源引在定义里。写成
	// "<Picture 1> defines the visual identity" 会让模型把它当帧锚点，身份就锁不住。
	for i := 1; i <= nImg; i++ {
		parts = append(parts, fmt.Sprintf("<Subject %d> is the character or subject shown in <Picture %d>; its face, hair, wardrobe and props are preserved exactly as depicted.",
			i, i))
	}
	// 只有音频、没有参考图时，绝不能补一句 "<Picture 1> ..."：
	// 图里根本没有 LoadImage 节点，模型会看到指向不存在输入的 <Picture 1>，
	// 于是不认"照抄音频"，退化成自己造一段伪中文（实测 2026-10-19 22:11）。
	for j := 1; j <= nAud; j++ {
		parts = append(parts, fmt.Sprintf("<Audio %d> is the spoken performance reused 1:1 as the target video's complete final audio track.", j))
	}
	// 参考图也没有时退回最保守的一句话，不再引用任何 <Picture N>。
	if len(parts) == 0 {
		parts = append(parts, "The target clip is generated from the text description alone.")
	}
	b.WriteString(strings.Join(parts, "\n"))
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

	// 官方 4.1/4.2：每条参考一行；可见内容用 fully_preserved（不是 Picture 条目）
	b.WriteString("retention_analysis:\n")
	ra := make([]string, 0, nImg+nAud)
	for i := 1; i <= nImg; i++ {
		ra = append(ra, fmt.Sprintf("<Subject %d> (appears in [Shot 1]): fully_preserved - face, hair, wardrobe and props as depicted in <Picture %d> are retained.",
			i, i))
	}
	for j := 1; j <= nAud; j++ {
		ra = append(ra, fmt.Sprintf("<Audio %d>: fully_copy - <Audio %d> is reused 1:1 as the target video's complete final audio track.",
			j, j))
	}
	b.WriteString(strings.Join(ra, "\n"))
	b.WriteString("\n")

	b.WriteString("detailed_description: " + desc + "\n")
	if nAud > 0 {
		// 官方 6：不要用 "N/A" 当垃圾桶。fully_copy 时环境层属于 soundscape，
		// 配乐属于 music —— 两个字段各按可听层说明关系。
		b.WriteString("overall_soundscape: The complete final audio track is <Audio 1> copied 1:1; its ambience and physical sound layers continue across the whole video.\n")
	} else if sound != "" {
		b.WriteString("overall_soundscape: " + sound + "\n")
	} else {
		b.WriteString("overall_soundscape: " + h3DefaultSoundscape + "\n")
	}
	if nAud > 0 {
		b.WriteString("non_diegetic_music: N/A - any score present is part of <Audio 1>'s copied track.")
	} else if music != "" {
		b.WriteString("non_diegetic_music: " + music)
	} else {
		b.WriteString("non_diegetic_music: N/A")
	}
	return b.String()
}

// h3FieldBlock 取某个字段的**完整块**（含多行值），返回内容与原文位置。
//
// 为什么需要它：官方六段式里 retention_analysis / subject_definitions 天生是多行
// （每条参考一行）。h3FieldValue 只读到行尾，用它做替换会把其余行整段丢掉——
// 这是很容易埋进去的"改一个字段毁四个字段"。
func h3FieldBlock(prompt, field string) (content string, start, end int) {
	low := strings.ToLower(prompt)
	idx := strings.Index(low, strings.ToLower(field))
	if idx < 0 {
		return "", -1, -1
	}
	keyEnd := idx + len(field)
	// 跳过 "field" 与 ":" 之间的空白
	p := keyEnd
	for p < len(prompt) && (prompt[p] == ' ' || prompt[p] == '\t') {
		p++
	}
	if p >= len(prompt) || prompt[p] != ':' {
		return "", -1, -1
	}
	p++
	if p < len(prompt) && prompt[p] == ' ' {
		p++
	}
	// 块尾 = 下一个"行首字段标签"。用行首正则而不是裸词搜索：
	// 正文里出现 "summary" 这种词是常事，裸搜会把块提前截断。
	if loc := reFieldLine.FindStringIndex(prompt[p:]); loc != nil {
		return prompt[p : p+loc[0]], p, p + loc[0]
	}
	return prompt[p:], p, len(prompt)
}

// reFieldLine 匹配行首的六段式/三段式字段标签（允许前导空白与全角冒号后的空格）。
var reFieldLine = regexp.MustCompile(`(?im)^[ \t]*(subject_definitions|retention_analysis|detailed_description|overall_soundscape|non_diegetic_music|integrated_multimodal_description|summary)[ \t]*:`)

// normalizeSummaryPrefix 用实际参考资产覆盖 LLM 猜错的任务类型前缀。
//
// 实测（qwen2.5:14b 两次）：明明只有"图片定身份 + 音频复用"，它写成
// [video continuation] 或 [keyframe completion]。前缀不是装饰——它决定模型
// 把参考当"帧锚点"还是"身份来源"，猜错就等于用错参考。
// 前缀可以从资产清单直接推出来，所以不让模型猜。
func normalizeSummaryPrefix(prompt string, nImg, nAud int) string {
	low := strings.ToLower(prompt)
	if strings.Contains(low, "<video ") {
		return prompt // 真有源视频时保留 LLM 的判断
	}
	prefix := "[reference generation"
	if nAud > 0 {
		prefix += " + audio reuse"
	}
	prefix += "]"
	if loc := reSummaryPrefix.FindStringSubmatchIndex(prompt); loc != nil {
		return prompt[:loc[2]] + prefix + prompt[loc[3]:]
	}
	return prompt
}

// extractDialogue 从原始需求里抠出台词原文（<d> 块或中英文引号内的短句）。
//
// 为什么需要：14B 级模型会把台词改写成英文注释、甚至整段丢掉 <d> 标签
// （实测 qwen2.5:14b 把 <d>[Chinese] 十年了。</d> 写成 "十年了." (Ten years have passed.)）。
// 台词是用户唯一不可让渡的内容，改写等于毁片，所以必须能检出并保底。
func extractDialogue(raw string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		// 只认可能是台词的片段：含中日韩字符，或长度在 2-30 字符之间的整句
		hasCJK := false
		for _, r := range s {
			if r >= 0x4E00 && r <= 0x9FFF {
				hasCJK = true
				break
			}
		}
		if !hasCJK && len([]rune(s)) > 30 {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range reDialogueTag.FindAllStringSubmatch(raw, -1) {
		add(m[1])
	}
	for _, m := range reQuoted.FindAllStringSubmatch(raw, -1) {
		add(m[1])
	}
	return out
}

// protectDialogue 校验优化结果是否保住了每一句台词。
// 返回空字符串表示"不可信，应退回原文"，而不是硬用一份改过台词的提示词。
func protectDialogue(raw, optimized string) string {
	lines := extractDialogue(raw)
	if len(lines) == 0 {
		return optimized
	}
	for _, l := range lines {
		if !strings.Contains(optimized, l) {
			return ""
		}
	}
	return optimized
}

// patchRefAudioRetention 补上"有参考音频却没标 fully_copy"这一致命缺失。
//
// 不标 fully_copy，H3 会把参考音频当"风格提示"重新生成一段——念稿直接退化成
// 含糊的伪中文。这是整条链路里代价最高的一次静默失败，所以做成确定性补齐。
func patchRefAudioRetention(prompt string, nAud int) string {
	if nAud <= 0 || strings.Contains(strings.ToLower(prompt), "fully_copy") {
		return prompt
	}
	lines := make([]string, 0, nAud)
	for j := 1; j <= nAud; j++ {
		lines = append(lines, fmt.Sprintf("<Audio %d>: fully_copy - <Audio %d> is reused 1:1 as the target video's complete final audio track.", j, j))
	}
	block, start, end := h3FieldBlock(prompt, "retention_analysis")
	if start < 0 {
		// 没有该字段：补一个完整字段（放在 summary 之后最合理，没有就追加到末尾）
		add := "retention_analysis:\n" + strings.Join(lines, "\n") + "\n"
		if _, ss, se := h3FieldBlock(prompt, "summary"); ss >= 0 {
			return prompt[:se] + add + prompt[se:]
		}
		return strings.TrimRight(prompt, " \n") + "\n" + add
	}
	merged := strings.TrimRight(block, " \n") + "\n" + strings.Join(lines, "\n") + "\n"
	return prompt[:start] + merged + prompt[end:]
}

// stripFieldLines 去掉源提示词里自带的三段式字段行，只留画面描述正文。
//
// 背景：h3FieldValue 找不到 integrated_multimodal_description / detailed_description 时
// 会把整段原文当 desc。用户常常自带 overall_soundscape / non_diegetic_music，
// 于是这些行被嵌进 detailed_description，函数末尾又追加一份权威值 —— 字段重复且互相
// 矛盾，模型无所适从（实测：ref2va 的 fully_copy 因此不生效，音频被重生成）。
func stripFieldLines(s string) string {
	labels := []string{
		"overall_soundscape:", "non_diegetic_music:",
		"integrated_multimodal_description:", "detailed_description:",
		"subject_definitions:", "summary:", "retention_analysis:",
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		low := strings.ToLower(trimmed)
		skip := false
		for _, l := range labels {
			if strings.HasPrefix(low, l) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, " ")
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

// OptimizePromptForRef2VA 全参考模式的六段式改写。
//
// 与基础模式的差别不只是"多几个字段"：Ref2VA 的标签语义决定了身份锁不锁得住——
// 图片只做身份来源时必须写成 <Subject N>（图片源引在定义里），
// 写成 "<Picture 1> defines the visual identity" 会让模型把它当成帧锚点而不是角色定义。
func (e *EngineV2) OptimizePromptForRef2VA(ctx context.Context, raw string, seconds, nImg, nAud int) (string, error) {
	return e.OptimizePromptForRef2VAWithGrid(ctx, raw, seconds, nImg, nAud, "")
}

// OptimizePromptForRef2VAWithGrid 带场景锁的 Ref2VA 改写。
func (e *EngineV2) OptimizePromptForRef2VAWithGrid(ctx context.Context, raw string, seconds, nImg, nAud int, sceneLock string) (string, error) {
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
	var b strings.Builder
	fmt.Fprintf(&b, "目标时长：%d 秒\n", seconds)
	if nImg > 0 {
		fmt.Fprintf(&b, "参考图：%d 张，标签依次为 <Picture 1>..<Picture %d>（按接线顺序）。"+
			"若某张只用来定角色/服装/场景，请写进 <Subject N> 定义而不单列 <Picture N>。\n", nImg, nImg)
	}
	if nAud > 0 {
		fmt.Fprintf(&b, "参考音频：%d 段，标签依次为 <Audio 1>..<Audio %d>，"+
			"retention_analysis 里必须标 fully_copy。\n", nAud, nAud)
	}
	if l := strings.TrimSpace(sceneLock); l != "" {
		fmt.Fprintf(&b, "场景锁定（逐字保留在 detailed_description 的风格句之后、[Shot 1] 之前）：%s\n", l)
	}
	fmt.Fprintf(&b, "原始需求：%s", raw)

	// 确定性调用（temperature=0）：同一份输入每次得到同一份优化结果，
	// 这样「改一个字重跑」时能确定差异来自改动本身，而不是随机采样。
	out, err := e.deterministicChat(e.llm.Model, fmt.Sprintf("%s\n\n%s", h3Ref2VAInstruction, b.String()))
	if err != nil {
		return "", err
	}
	out = cleanOptimizedPrompt(out)
	if out == "" {
		return "", fmt.Errorf("提示词优化返回空结果")
	}
	// 台词是唯一不可让渡的内容：模型改了字、翻了译、或丢了 <d>，就退回原文重搭结构。
	// 宁可少一点文学性，也不能让成片念错词。
	if protectDialogue(raw, out) == "" {
		out = ensureRef2VAPrompt(raw, nImg, nAud)
	}
	out, _ = ensureH3AudioFields(out)
	// 六段式的确定性兜底：LLM 漏段时补齐，绝不把残缺结构喂给模型
	out = ensureRef2VAPrompt(out, nImg, nAud)
	// 场景锁最后注入：先让模型写完自己的结构，再由我们盖上同一枚章。
	// 交给 LLM 自己"保留"是不可靠的——它会把锁句改写进正文，肉眼几乎看不出差别。
	if l := strings.TrimSpace(sceneLock); l != "" {
		out = InjectSceneLock(out, l)
	}
	return out, nil
}

// h3PromptIssue 提示词质量检查的一条结果。
//
// 为什么要机器检查：官方指南里最容易漏的不是"写得难看"，而是几处会让模型
// 明显降级的结构性缺失——说话人没有音色描述、镜头运动没有幅度、身份标签用错、
// 有参考音频却没标 fully_copy。这些都能确定性判断，不该靠人眼。
type h3PromptIssue struct {
	Level string // FAIL / WARN
	Code  string
	Msg   string
}

var h3VoiceWords = []string{
	"voice", "timbre", "pitch", "tone", "gravelly", "husky", "raspy", "soft", "quiet",
	"low", "high", "deep", "young", "old", "elderly", "middle-aged", "teenage",
	"male", "female", "breathy", "steady", "measured", "hoarse", "warm", "cold",
}

var h3StyleWords = []string{
	"cinematic", "live-action", "live action", "2d-animated", "3d cg", "cg",
	"claymation", "watercolour", "watercolor", "vintage film", "anime", "photoreal",
}

// auditH3Prompt 对优化后的提示词做确定性体检。返回空切片表示无问题。
func auditH3Prompt(prompt string) []h3PromptIssue {
	var out []h3PromptIssue
	low := strings.ToLower(prompt)

	add := func(level, code, msg string) {
		out = append(out, h3PromptIssue{Level: level, Code: code, Msg: msg})
	}

	// 1. 主体字段
	hasBody := strings.Contains(low, "integrated_multimodal_description") ||
		strings.Contains(low, "detailed_description")
	if !hasBody {
		add("FAIL", "body_missing", "缺少画面主体字段（integrated_multimodal_description / detailed_description）")
	}
	if !strings.Contains(low, "overall_soundscape") {
		add("FAIL", "soundscape_missing", "缺少 overall_soundscape：空字段会让 H3 自行脑补人声")
	}
	if !strings.Contains(low, "non_diegetic_music") {
		add("FAIL", "music_missing", "缺少 non_diegetic_music（无配乐也要显式写 N/A）")
	}

	// 2. 身份标签误用：官方要求"只定身份"的图写进 Subject，不单列 Picture
	if rePictureIdentity.MatchString(prompt) {
		add("FAIL", "picture_identity_misuse",
			"用了 <Picture N> defines/is the visual identity：官方要求改为 <Subject N> 定义，图片源引在其中")
	}

	// 3. 说话人缺音色描述（官方 4.4：ID 前要给可念的身份/音色）
	for _, m := range reSpeaker.FindAllStringIndex(prompt, -1) {
		start := m[0] - 180
		if start < 0 {
			start = 0
		}
		ctx := strings.ToLower(prompt[start:m[0]])
		ok := false
		for _, w := range h3VoiceWords {
			if strings.Contains(ctx, w) {
				ok = true
				break
			}
		}
		if !ok {
			add("WARN", "speaker_no_voice",
				"说话人 "+prompt[m[0]:m[1]]+" 前 180 字符内没有音色/年龄/性别描述，口型和语气会失控")
			break
		}
	}

	// 4. 有参考音频却没标 fully_copy：念稿会退化成伪中文
	if strings.Contains(low, "<audio ") && !strings.Contains(low, "fully_copy") {
		add("FAIL", "audio_not_fully_copy",
			"引用了 <Audio N> 但 retention_analysis 没有 fully_copy：参考音频会被当风格提示重新生成")
	}

	// 5. 镜头运动用了非官方词汇。
	// 注意：官方 4.3 明确说"幅度与速度只在有意义时写，中等幅度常速通常省略"，
	// 所以**不能**因为缺 amplitude 就报警——只查"moves forward / camera follows"
	// 这类表达，它们在官方词汇表里都有明确对应（Push In / Tracking Shot）。
	for _, w := range []string{"camera moves", "moves forward", "camera drifts",
		"camera follows", "camera glides", "camera slides"} {
		if strings.Contains(low, w) {
			add("WARN", "camera_nonstandard",
				"镜头运动用了非官方表达（"+w+"）：官方词汇表要求 Push In / Pull Out / Pan / Truck / Tilt / Pedestal / Arc / Tracking / Zoom")
			break
		}
	}

	// 6. 风格开场：官方要求 [Shot 1] 内第一句先立风格
	if i := strings.Index(low, "[shot 1]"); i >= 0 {
		tail := low[i:]
		if j := strings.Index(tail, "[shot 2]"); j > 0 {
			tail = tail[:j]
		}
		ok := false
		for _, w := range h3StyleWords {
			if strings.Contains(tail, w) {
				ok = true
				break
			}
		}
		if !ok {
			add("WARN", "style_missing", "[Shot 1] 里没有风格声明（Live-action, cinematic, ...），画面质感会掉")
		}
	}

	// 7. 正文太短：官方给生成任务的基准是 350-500 词
	if body := h3FieldValue(prompt, "detailed_description"); body != "" {
		if n := len(strings.Fields(body)); n < 250 {
			add("WARN", "body_too_short", fmt.Sprintf("detailed_description 只有 %d 词，官方基准 350-500 词", n))
		}
	} else if body := h3FieldValue(prompt, "integrated_multimodal_description"); body != "" {
		if n := len(strings.Fields(body)); n < 180 {
			add("WARN", "body_too_short", fmt.Sprintf("integrated_multimodal_description 只有 %d 词，细节偏少", n))
		}
	}

	// 8. summary 任务类型与实际参考不符（实测 LLM 会把"图片定身份"写成 video continuation）
	if m := reSummaryPrefix.FindStringSubmatch(prompt); m != nil {
		prefix := strings.ToLower(m[1])
		if (strings.Contains(prefix, "video continuation") || strings.Contains(prefix, "video editing")) &&
			!strings.Contains(low, "<video ") {
			add("FAIL", "summary_task_mismatch",
				"summary 写了 video continuation/editing，但没有任何 <Video N> 参考：任务类型必须与实际资产一致")
		}
		if strings.Contains(low, "fully_copy") && !strings.Contains(prefix, "audio reuse") {
			add("WARN", "summary_missing_audio_reuse",
				"audio 标了 fully_copy，summary 前缀应含 audio reuse")
		}
	}

	// 9. 标签裸写：Subject 1 不带尖括号的引用模型不认
	if n := countBareLabels(prompt, "Subject"); n > 0 {
		add("WARN", "label_without_brackets",
			fmt.Sprintf("有 %d 处 \"Subject N\" 没写尖括号，模型不会把它当参考标签", n))
	}

	// 10. 场景锁：写了就必须是英文单句，且必须在画面主体字段内。
	// 锁句写中文等于没写（H3 正文一律英文），锁句掉到字段外会被当注释忽略。
	if lock := SceneLockOf(prompt); lock != "" {
		for _, r := range lock {
			if r >= 0x4E00 && r <= 0x9FFF {
				add("WARN", "scene_lock_not_english",
					"SCENE LOCK 里含中文：H3 正文必须英文，中文格位会被弱化甚至忽略")
				break
			}
		}
		if body := h3FieldValue(prompt, "integrated_multimodal_description"); body != "" {
			if !strings.Contains(body, "SCENE LOCK:") {
				add("WARN", "scene_lock_outside_body",
					"SCENE LOCK 不在画面主体字段内，模型会把它当说明文字跳过")
			}
		}
	}

	return out
}

// countBareLabels 数出"标签名 + 编号"但没被尖括号包住的次数。
func countBareLabels(prompt, name string) int {
	count := 0
	idx := 0
	needle := name + " "
	for {
		i := strings.Index(prompt[idx:], needle)
		if i < 0 {
			return count
		}
		pos := idx + i
		// 形如 <Subject 1> 的跳过；只看后面紧跟数字的
		rest := prompt[pos+len(needle):]
		if len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
			if pos == 0 || prompt[pos-1] != '<' {
				count++
			}
		}
		idx = pos + len(needle)
	}
}

// auditSummary 把体检结果压成一行，便于写进日志与节点输出。
func auditSummary(issues []h3PromptIssue) string {
	if len(issues) == 0 {
		return ""
	}
	parts := make([]string, 0, len(issues))
	for _, it := range issues {
		parts = append(parts, it.Level+" "+it.Code)
	}
	return strings.Join(parts, "; ")
}

var (
	rePictureIdentity = regexp.MustCompile(`<Picture \d+>\s+(defines|is)\s+(the\s+)?(visual\s+)?(identity|appearance)`)
	reSpeaker         = regexp.MustCompile(`\(S\d(?:,S\d)*\)`)
	reSummaryPrefix   = regexp.MustCompile(`summary:\s*(\[[^\]]+\])`)
	reDialogueTag     = regexp.MustCompile(`<d>\s*(?:\[[^\]]*\])?\s*([^<]+?)\s*</d>`)
	reQuoted          = regexp.MustCompile(`["“]([^"”]{2,30})["”]`)
)

// OptimizePromptForH3 把原始想法改写成 H3 可用的镜头指令。
// 走语言侧 LLM（调用方负责先 enterSide(sideLanguage) 让扩散侧让位）。
func (e *EngineV2) OptimizePromptForH3(ctx context.Context, raw string, seconds int) (string, error) {
	return e.OptimizePromptForH3WithGrid(ctx, raw, seconds, "")
}

// OptimizePromptForH3WithGrid 带场景锁的三段式改写。
//
// sceneLock 为空时行为与旧版一致；非空时锁句会逐字注入 [Shot 1] 风格句之后。
// 注入用确定性函数而不是靠 LLM 听话：多段一致性只认字节相等。
func (e *EngineV2) OptimizePromptForH3WithGrid(ctx context.Context, raw string, seconds int, sceneLock string) (string, error) {
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
	// 诊断优先（V3）：先把「缺什么」判出来，只把这几处交给 LLM 修。
	// 诊断 ok 时 guide 为空 ⇒ 不注入 ⇒ 旧行为逐字不变（重要：prompt_opt 默认开启，
	// 不能让一条本来没问题的提示词因为这次改动而换一种写法）。
	guide := DiagnoseGuide(DiagnosePrompt(raw, true))
	var user string
	if l := strings.TrimSpace(sceneLock); l != "" {
		user = fmt.Sprintf("目标时长：%d 秒\n场景锁定（逐字保留在 [Shot 1] 风格句之后）：%s\n原始想法：%s",
			seconds, l, raw)
	} else {
		user = fmt.Sprintf("目标时长：%d 秒\n原始想法：%s", seconds, raw)
	}
	if guide != "" {
		// 必须放在 sceneLock 赋值**之后**：先拼后覆盖会把诊断指导静默丢掉。
		user += "\n\n" + guide
	}
	modelName := e.llm.Model
	out, err := e.deterministicChat(modelName, fmt.Sprintf("%s\n\n%s", h3PromptInstruction, user))
	if err != nil {
		return "", err
	}
	out = cleanOptimizedPrompt(out)
	if out == "" {
		return "", fmt.Errorf("提示词优化返回空结果")
	}
	// 确定性兜底：LLM 漏填的声音字段由这里补齐，不依赖模型是否听话
	out, _ = ensureH3AudioFields(out)
	if l := strings.TrimSpace(sceneLock); l != "" {
		out = InjectSceneLock(out, l)
	}
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

// 走语言侧 LLM（调用方负责 enterSide(sideLanguage)）。
func (e *EngineV2) SplitPromptIntoBeats(prompt string, n int) ([]string, error) {
	if e.llm == nil {
		return nil, fmt.Errorf("LLM 服务不可用，无法拆分分镜")
	}
	if n < 2 {
		return []string{prompt}, nil
	}
	user := fmt.Sprintf("拆成 %d 段。镜头总指令：%s", n, prompt)
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

	out, err := e.deterministicChat(e.llm.Model, fmt.Sprintf("%s\n\n%s", beatSplitInstruction, user))
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
