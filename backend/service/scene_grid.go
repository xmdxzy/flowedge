package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"flowedge/backend/model"
)

// 场景九宫格锁（Scene Grid Lock）
//
// 要解决的问题：H3 没有场景记忆。同一条片拆成 N 段生成时，每段独立采样，
// 光位、道具摆放、背景结构都会各自漂移——段与段之间像换了个场地。
// 角色可以靠定妆图 / <Picture N> 锁，场景之前只能靠散文里一句"昏暗的石殿"，
// 而这句在每段里的解释都不一样。
//
// 做法：把画面划成 3×3 九个格位，每格写死**看得见的物理事实**（材质、颜色、形状、
// 光源位置），编译成一句固定的 SCENE LOCK，**逐字相同**地注入每一段。
//
// 为什么是九格而不是形容词更多：模型对"左侧有灯"这种位置描述，每段的理解重点不同；
// 九格强制它按空间索引对齐——左上永远同一块天花，右下永远同一片地砖。
// 「精准约束」的来源不是字数，是**同一批事实在每段里出现在同一个空间位置**。
//
// 与官方规则的关系：官方要求 [Shot 1] 第一句声明风格。锁句一律插在风格句之后，
// 不抢第一句的位置，否则风格声明失效。

// SceneGrid 场景九宫格。
//
// Cells 顺序固定为阅读顺序（见 gridCellNames），长度不足 9 时按序补齐，多余忽略。
type SceneGrid struct {
	Name    string   `json:"name,omitempty"`
	Style   string   `json:"style,omitempty"`   // 视觉风格，与提示词里的风格句保持一致
	Light   string   `json:"light,omitempty"`   // 全局光位：方向 + 色温 + 硬度
	Palette string   `json:"palette,omitempty"` // 主色关系，锁色调漂移
	Depth   string   `json:"depth,omitempty"`   // 机位高度与焦段，锁透视漂移
	Cells   []string `json:"cells"`             // 九格内容，空串表示该格不约束
}

// gridCellNames 九格的空间短语（英文，直接进提示词）。
// 顺序 = 阅读顺序：上排从左到右，中排从左到右，下排从左到右。
var gridCellNames = [9]string{
	"top-left", "top-center", "top-right",
	"middle-left", "center", "middle-right",
	"bottom-left", "bottom-center", "bottom-right",
}

// gridCellAliases 中文/变体位置名 → 格位下标。
// 用户手写九宫格时位置名五花八门，这里全部收口，认不出来就按行序填。
var gridCellAliases = map[string]int{
	"左上": 0, "上左": 0, "top-left": 0, "topleft": 0, "tl": 0,
	"上中": 1, "正上": 1, "中上": 1, "top-center": 1, "top": 1, "tc": 1,
	"右上": 2, "上右": 2, "top-right": 2, "topright": 2, "tr": 2,
	"左中": 3, "中左": 3, "middle-left": 3, "left": 3, "ml": 3,
	"中心": 4, "正中": 4, "中": 4, "中间": 4, "center": 4, "centre": 4, "mc": 4,
	"右中": 5, "中右": 5, "middle-right": 5, "right": 5, "mr": 5,
	"左下": 6, "下左": 6, "bottom-left": 6, "bottomleft": 6, "bl": 6,
	"下中": 7, "正下": 7, "中下": 7, "bottom-center": 7, "bottom": 7, "bc": 7,
	"右下": 8, "下右": 8, "bottom-right": 8, "bottomright": 8, "br": 8,
}

// sceneLockPrefix 锁句的固定前缀。
// 提取与哈希都靠它——一旦换词，历史提示词就再也认不出来，等于丢掉全部旧片的一致性证据。
const sceneLockPrefix = "SCENE LOCK:"

// reSceneLock 匹配一整条锁句。
//
// 依赖一个硬约定：**格位内容里不允许出现句号**（编译时会被替换成逗号）。
// 因此 "到第一个句号为止" 就是锁句边界，正则不需要处理嵌套标点。
var reSceneLock = regexp.MustCompile(`(?is)SCENE LOCK:[^.]*\.`)

// sceneCellText 把一格内容规整成可安全拼进锁句的英文片段。
//
// 句号必须清掉（会破坏锁句边界）；分号必须清掉（会破坏格位分隔）；
// 换行必须拍平（否则字段块被切断）；前缀词必须清掉（否则重复锁产生嵌套）。
func sceneCellText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 空占位：用户常用这几种表示"这格不管"
	switch strings.ToLower(s) {
	case "-", "--", "—", "无", "空", "none", "null", "n/a", "na":
		return ""
	}
	s = strings.NewReplacer(
		"\r", " ", "\n", " ", ".", ",", ";", ",", "，", ",", "。", ",",
		sceneLockPrefix, "", "scene lock", "",
	).Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " ,")
	return s
}

// CompileSceneLock 把九宫格编译成一句可复现的锁定文本。
//
// 纯确定性函数：同一份 grid 永远产出同一串字节。这是多段一致性的地基——
// 只要有一处随机会话状态（时间戳、随机排序、map 遍历），段与段就对不上。
func CompileSceneLock(g SceneGrid) (string, error) {
	var parts []string
	for i, raw := range padGridCells(g.Cells) {
		if c := sceneCellText(raw); c != "" {
			parts = append(parts, fmt.Sprintf("the %s holds %s", gridCellNames[i], c))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("场景九宫格全为空：至少填一格才谈得上锁场景")
	}

	var b strings.Builder
	b.WriteString(sceneLockPrefix)
	b.WriteString(" every shot keeps this exact staging unchanged for the whole clip — ")
	b.WriteString(strings.Join(parts, "; "))
	if s := sceneCellText(g.Light); s != "" {
		b.WriteString("; the light stays " + s)
	}
	if s := sceneCellText(g.Palette); s != "" {
		b.WriteString("; the palette stays " + s)
	}
	if s := sceneCellText(g.Depth); s != "" {
		b.WriteString("; the camera height and lens stay " + s)
	}
	b.WriteString(".")
	return b.String(), nil
}

// padGridCells 补齐/截断到 9 格，保证下标与 gridCellNames 严格对齐。
func padGridCells(in []string) [9]string {
	var out [9]string
	for i := 0; i < 9 && i < len(in); i++ {
		out[i] = in[i]
	}
	return out
}

// SceneLockOf 取出提示词里的锁句原文（没有则空串）。
func SceneLockOf(prompt string) string {
	if m := reSceneLock.FindString(prompt); m != "" {
		return strings.TrimSpace(m)
	}
	return ""
}

// StripSceneLock 删除提示词里已有的锁句。
//
// 存在的理由：LLM 拆段后会自己重写/微调锁句（改个介词、换个词序），
// 肉眼看不出来，但两段的锁句已经不同字节——一致性就在这一步悄悄丢掉。
// 所以每段都先剥干净，再注入编译好的**同一串**。
func StripSceneLock(prompt string) string {
	out := reSceneLock.ReplaceAllString(prompt, "")
	out = strings.ReplaceAll(out, "  ", " ")
	out = strings.ReplaceAll(out, " .", ".")
	return strings.TrimSpace(out)
}

// applySceneLock 把场景锁注入提示词，返回新提示词与「是否真的注入了」。
//
// 直通语义（与门禁一致）：提示词里已经有锁句时原样返回，不重复注入。
// 门禁按字节全等校验（h3.scene_lock_missing / h3.scene_lock_drift），
// 重复注入会让锁句叠加两次，锁本身反而不再全等——那是把保护措施变成故障源。
func applySceneLock(prompt, lock string) (string, bool) {
	if strings.TrimSpace(lock) == "" {
		return prompt, false
	}
	if SceneLockOf(prompt) != "" {
		return prompt, false // 已有锁：直通，下游不许重编
	}
	out := InjectSceneLock(prompt, lock)
	return out, out != prompt
}

// InjectSceneLock 把锁句注入提示词的主描述字段。
//
// 三条规则：
//  1. 先剥旧锁，保证幂等（重复注入不会叠句子）；
//  2. 插在 [Shot 1] 的**第一句之后**——官方要求第一句声明风格，锁句不能抢位；
//  3. 没有结构化字段时自己套一层三段式，宁可重排也不能把锁丢在字段外面
//     （字段外的文字模型会当注释忽略）。
func InjectSceneLock(prompt, lock string) string {
	p := StripSceneLock(strings.TrimSpace(prompt))
	if strings.TrimSpace(lock) == "" {
		return p
	}

	field := "integrated_multimodal_description"
	content, start, end := h3FieldBlock(p, field)
	if start < 0 {
		field = "detailed_description"
		content, start, end = h3FieldBlock(p, field)
	}
	if start < 0 {
		return field + ": [Shot 1] " + lock + " " + p
	}
	return p[:start] + insertLockInField(content, lock) + p[end:]
}

// insertLockInField 在字段值内部找插入点。
//
// 两种形态必须分开处理，否则锁句会跑到风格声明前面、把风格句顶掉：
//   - T2VA：风格句在 [Shot 1] 内部第一句 → 锁句接在这句之后
//   - Ref2VA：风格句在 [Shot 1] 之前（官方要求）→ 锁句接在风格句之后、镜头标记之前
//
// 判据是 [Shot 1] 前面有没有正文，而不是靠字段名猜——字段名会变，结构不会。
func insertLockInField(content, lock string) string {
	idx := strings.Index(content, "[Shot 1]")
	if idx < 0 {
		// 没有镜头标记（拆段产物常见）：只有认出首句是风格声明，才敢把锁句放到它后面。
		// 认不出来就把锁放最前——宁可牺牲一句风格前置，也不能让锁被当正文稀释。
		if j := firstSentenceEnd(content); j > 0 {
			first := strings.ToLower(content[:j])
			for _, w := range h3StyleWords {
				if strings.Contains(first, w) {
					return content[:j] + " " + lock + content[j:]
				}
			}
		}
		return lock + " " + strings.TrimSpace(content)
	}
	if head := strings.TrimSpace(content[:idx]); head != "" {
		return strings.TrimRight(content[:idx], " \n\t") + " " + lock + " " + content[idx:]
	}
	head := content[:idx+len("[Shot 1]")]
	rest := content[idx+len("[Shot 1]"):]
	if j := firstSentenceEnd(rest); j > 0 {
		return head + rest[:j] + " " + lock + rest[j:]
	}
	return head + " " + lock + rest
}

// abbreviations 句末判定时要跳过的常见缩写，避免把 "Mr." 当成句子结束。
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "st": true,
	"e.g": true, "i.e": true, "vs": true, "etc": true, "no": true,
}

// firstSentenceEnd 返回第一句结束位置（含标点，不含其后空格）。
// 找不到句末标点返回 -1。
func firstSentenceEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', '!', '?':
			if i+1 >= len(s) {
				return i + 1
			}
			if n := s[i+1]; n == ' ' || n == '\n' || n == '\r' {
				if s[i] == '.' && abbreviations[strings.ToLower(lastWord(s[:i]))] {
					continue
				}
				return i + 1
			}
		}
	}
	return -1
}

// lastWord 取 s 末尾的连续字母（用于缩写判定）。
func lastWord(s string) string {
	i := len(s)
	for i > 0 {
		c := s[i-1]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '.' {
			i--
			continue
		}
		break
	}
	return strings.Trim(s[i:], ".")
}

// auditSceneConsistency 多段一致性检查——这是「有牙齿」的那道门。
//
// 只检查"有没有写锁句"是没用的（写了也可能各不相同）。真正的失败模式是
// 锁句被 LLM 改写后悄悄漂移，成片看起来像换了场地而时长、帧数全部正常。
// 所以按**原文全等**比对，不是模糊匹配。
func auditSceneConsistency(prompts []string) []h3PromptIssue {
	var issues []h3PromptIssue
	if len(prompts) == 0 {
		return issues
	}
	seen := map[string]int{}
	var first string
	for i, p := range prompts {
		lock := SceneLockOf(p)
		if lock == "" {
			issues = append(issues, h3PromptIssue{
				Level: "FAIL", Code: "h3.scene_lock_missing",
				Msg: fmt.Sprintf("第 %d 段没有 SCENE LOCK：该段会重新抽场景，接缝处必然换景", i+1),
			})
			continue
		}
		if first == "" {
			first = lock
		}
		seen[lock]++
		if lock != first {
			issues = append(issues, h3PromptIssue{
				Level: "FAIL", Code: "h3.scene_lock_drift",
				Msg: fmt.Sprintf("第 %d 段锁句与其他段不一致（长度 %d vs %d）：场景会漂移", i+1, len(lock), len(first)),
			})
			break
		}
	}
	return issues
}

// sceneGridInstruction LLM 拆格指令。
//
// 要求全部可判定：九行、固定顺序、英文、无句号、有词数区间。
// 不写"要具体一点"这种没法验收的话——模型会当没看见。
const sceneGridInstruction = `你是场景美术指导。把用户给的场景拆成 3×3 九宫格，用于多段视频生成时锁定场景。

输出严格 9 行，顺序固定，不要编号、不要解释、不要 Markdown：
左上|<english phrase>
上中|<english phrase>
右上|<english phrase>
左中|<english phrase>
中心|<english phrase>
右中|<english phrase>
左下|<english phrase>
下中|<english phrase>
右下|<english phrase>

每条规则都必须照做：
1. 只写看得见的物理事实：材质、颜色、形状、数量、物体之间的位置关系、光源所在位置与照射方向。禁止形容词（beautiful / epic / stunning）、禁止比喻、禁止情绪与叙事。
2. 上排三格写远景层（天花、天空、墙面上部、悬挂物）；中排三格写主体所在的中景层（建筑立面、柱、门、家具）；下排三格写地面与前景（地砖、台阶、散落物）。
3. 光源写到具体格位与方向，例如 "a single iron lantern at the top-left casts hard warm light down the left wall"。
4. 每格 6 到 24 个英文单词，全部小写开头即可，不要写句号（句号会破坏锁句边界）。
5. 该格确实没有内容就写 - 。
6. 保持术语在九格之间一致：同一个物体只用一个名字，不要一格叫 pillar 另一格叫 column。`

// ParseSceneGrid 解析用户手写的九宫格。
//
// 支持三种写法，按优先级：
//  1. JSON（{"cells":[...],"light":"..."}）——节点输出直接回流时走这条
//  2. 每行 "位置|内容" / "位置:内容" / "位置：内容"——位置名认不出就按行序填
//  3. 纯 9 行内容——严格按阅读顺序
func ParseSceneGrid(text string) (SceneGrid, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return SceneGrid{}, fmt.Errorf("九宫格内容为空")
	}

	if strings.HasPrefix(text, "{") {
		var g SceneGrid
		if err := json.Unmarshal([]byte(text), &g); err == nil {
			if len(g.Cells) > 0 {
				return g, nil
			}
		}
	}

	var g SceneGrid
	cells := make([]string, 9)
	next := 0
	for _, raw := range strings.Split(text, "\n") {
		ln := strings.TrimSpace(raw)
		if ln == "" {
			continue
		}
		pos, body, ok := splitGridLine(ln)
		if ok {
			if idx, hit := gridCellAliases[strings.ToLower(strings.TrimSpace(pos))]; hit {
				cells[idx] = body
				continue
			}
		}
		if next < 9 {
			cells[next] = ln
			next++
		}
	}
	g.Cells = cells
	if err := validateSceneGrid(g); err != nil {
		return SceneGrid{}, err
	}
	return g, nil
}

// splitGridLine 拆 "位置|内容" 这类行。冒号分隔只在左侧确实是位置名时才认，
// 否则会把 "the altar: cracked marble" 这种正文误当位置。
func splitGridLine(ln string) (pos, body string, ok bool) {
	for _, sep := range []string{"|", "：", ":"} {
		if i := strings.Index(ln, sep); i > 0 {
			cand := strings.TrimSpace(ln[:i])
			if _, hit := gridCellAliases[strings.ToLower(cand)]; hit {
				return cand, strings.TrimSpace(ln[i+len(sep):]), true
			}
			if sep == "|" {
				// 竖线不可能出现在正文里，一定是位置分隔符
				return cand, strings.TrimSpace(ln[i+1:]), true
			}
		}
	}
	return "", "", false
}

// validateSceneGrid 至少一格有内容，否则编译出来是空锁。
func validateSceneGrid(g SceneGrid) error {
	for _, c := range g.Cells {
		if sceneCellText(c) != "" {
			return nil
		}
	}
	return fmt.Errorf("九宫格九格全空：至少写一格内容（或用 - 表示留空）")
}

// execSceneGridNode 「场景九宫格」节点。
//
// 两条路径：
//   - 手动：用户直接写九行 → 只做编译，不花 LLM 时间（可复现，适合固定片场）
//   - 生成：给一句场景想法 → LLM 拆格 → 编译（一次定稿，后续各段复用同一份）
//
// 节点只产出锁句，不产出画面。它必须在上游、在提示词优化之前跑完，
// 否则每段会各自生成一份锁，等于没锁。
func (e *EngineV2) execSceneGridNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	rawCells, _ := node.Data["cells"].(string)
	idea, _ := node.Data["idea"].(string)
	style, _ := node.Data["style"].(string)
	light, _ := node.Data["light"].(string)
	palette, _ := node.Data["palette"].(string)
	depth, _ := node.Data["depth"].(string)

	// 上游若已经给了九宫格（例如 storyboard 产出），直接用它
	if strings.TrimSpace(rawCells) == "" {
		for _, out := range upstream {
			m, ok := out.(map[string]interface{})
			if !ok {
				continue
			}
			if s, ok := m["cells_text"].(string); ok && strings.TrimSpace(s) != "" {
				rawCells = s
				break
			}
		}
	}

	var g SceneGrid
	source := "manual"
	if strings.TrimSpace(rawCells) != "" {
		parsed, err := ParseSceneGrid(rawCells)
		if err != nil {
			return nil, err
		}
		g = parsed
	} else {
		if strings.TrimSpace(idea) == "" {
			return nil, fmt.Errorf("场景九宫格缺少输入：填 cells（九行）或 idea（一句场景描述）")
		}
		if e.llm == nil {
			return nil, fmt.Errorf("LLM 服务不可用，无法从想法拆九宫格；请直接填 cells")
		}
		release := e.enterSide(sideLanguage)
		// 确定性调用（temperature=0）：同一句想法每次拆出同一套九宫格，
		// 否则锁句会漂移，跨镜头一致性就失去基准。
		out, err := e.deterministicChat(e.llm.Model, fmt.Sprintf("%s\n\n场景：%s", sceneGridInstruction, idea))
		release()
		if err != nil {
			return nil, fmt.Errorf("九宫格拆解失败：%w", err)
		}
		parsed, perr := ParseSceneGrid(cleanOptimizedPrompt(out))
		if perr != nil {
			// 模型常把位置名写成英文短语或加序号；退回逐行填充再试一次
			parsed, perr = ParseSceneGrid(strings.ReplaceAll(out, "|", "|"))
		}
		if perr != nil {
			return nil, fmt.Errorf("九宫格拆解结果无法解析：%w", perr)
		}
		g = parsed
		source = "llm"
	}

	// 节点字段优先于解析结果：用户手填的光位/风格不该被 LLM 版本覆盖
	if strings.TrimSpace(g.Style) == "" {
		g.Style = strings.TrimSpace(style)
	}
	if strings.TrimSpace(g.Light) == "" {
		g.Light = strings.TrimSpace(light)
	}
	if strings.TrimSpace(g.Palette) == "" {
		g.Palette = strings.TrimSpace(palette)
	}
	if strings.TrimSpace(g.Depth) == "" {
		g.Depth = strings.TrimSpace(depth)
	}

	lock, err := CompileSceneLock(g)
	if err != nil {
		return nil, err
	}
	log.Printf("[scene-grid] 锁句已生成（来源 %s，%d 字）：%s", source, len(lock), lock)

	return map[string]interface{}{
		"text":       lock, // 直接连到下游 text 端口即可
		"lock":       lock, // 明确语义，避免与普通 text 混淆
		"grid":       g,    // 结构化回流（下游可继续编辑）
		"cells":      g.Cells,
		"cells_text": strings.Join(g.Cells, "\n"),
		"source":     source,
	}, nil
}

// sceneGridFromNode 从节点数据/上游解析出九宫格（供 text2video 与 prompt_opt 共用）。
//
// 优先级：已编译好的 lock > 结构化 grid > 文本 cells。
// 理由：lock 是上游一次定稿的产物，下游**不允许**重新编译——重新编译就等于允许漂移。
func sceneGridFromNode(node *model.NodeInstance, upstream map[string]interface{}) (SceneGrid, string, bool) {
	if node != nil {
		if s, ok := node.Data["scene_lock"].(string); ok && strings.TrimSpace(s) != "" {
			return SceneGrid{}, strings.TrimSpace(s), true
		}
	}
	for _, out := range upstream {
		m, ok := out.(map[string]interface{})
		if !ok {
			continue
		}
		if s, ok := m["scene_lock"].(string); ok && strings.TrimSpace(s) != "" {
			return SceneGrid{}, strings.TrimSpace(s), true
		}
		if s, ok := m["lock"].(string); ok && strings.TrimSpace(s) != "" {
			return SceneGrid{}, strings.TrimSpace(s), true
		}
	}
	if node != nil {
		switch v := node.Data["scene_grid"].(type) {
		case map[string]interface{}:
			b, _ := json.Marshal(v)
			if g, err := ParseSceneGrid(string(b)); err == nil {
				return g, "", true
			}
		case string:
			if strings.TrimSpace(v) != "" {
				// 直接粘贴编译好的锁句：原样使用，不再解析成格位。
				// 重新解析会把它当"左上格的内容"，锁句套锁句，模型直接读糊。
				if strings.Contains(strings.ToUpper(v), sceneLockPrefix) {
					return SceneGrid{}, strings.TrimSpace(v), true
				}
				if g, err := ParseSceneGrid(v); err == nil {
					return g, "", true
				}
			}
		}
	}
	for _, out := range upstream {
		m, ok := out.(map[string]interface{})
		if !ok {
			continue
		}
		if g, ok := m["grid"].(SceneGrid); ok {
			return g, "", true
		}
		if s, ok := m["grid"].(string); ok && strings.TrimSpace(s) != "" {
			if g, err := ParseSceneGrid(s); err == nil {
				return g, "", true
			}
		}
	}
	return SceneGrid{}, "", false
}

// resolveSceneLock 统一入口：拿到最终要注入的锁句（已编译或现编译）。
func resolveSceneLock(node *model.NodeInstance, upstream map[string]interface{}) (string, error) {
	g, lock, ok := sceneGridFromNode(node, upstream)
	if !ok {
		return "", nil
	}
	if strings.TrimSpace(lock) != "" {
		return lock, nil
	}
	return CompileSceneLock(g)
}
