package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"flowedge/backend/model"
)

// Compose：把「一句话目标」编译成「可执行的图」。
//
// 这是 FlowEdge 唯一缺的那半条腿 —— 以前 spec 只能"从图生成规格"（出口），
// 图本身必须人画。Raven 的 playbook、harness² 的 Compose 都指向同一个动作：
// **规格 → 图**。这里补上。
//
// 与"让 LLM 随便画个图"的区别（三条，缺一条就变成随机生成器）：
//
//	① 只给 LLM **模块目录**，不给整份契约 —— 29 个模块压成 29 行，
//	   否则 dense payload 会吃满上下文（CodeGraph 自曝过这个坑：少处理 token
//	   不等于少占窗口）。
//	② 生成后用**确定性校验器**验伪（ScoreGraph + PortKindAccepts），
//	   不过就把**具体问题**回喂重来。Raven 生成后只能靠 LLM 自检。
//	③ 不自动 run、不自动存库 —— 生成是提案，跑与存是决定。
//
// 为什么用 deterministicChat（温度 0）：构图是结构决策，不是创作。
// 同一句话两次生成不同的图，评测就无从比较。

// ComposeGoal 用户给的目标。字段少而硬，避免"什么都可调"导致没人调对。
type ComposeGoal struct {
	Goal string `json:"goal"`
	// TotalSeconds 总时长（秒）。0 = 不约束
	TotalSeconds float64 `json:"totalSeconds,omitempty"`
	// Aspect 画幅，如 "9:16" / "16:9"
	Aspect string `json:"aspect,omitempty"`
	// NoSpeech 是否明确无台词（氛围片）。nil = 未指定
	NoSpeech *bool `json:"noSpeech,omitempty"`
	// MaxRepairRounds 校验不过时可回喂几轮。默认 2，上限 4
	MaxRepairRounds int `json:"maxRepairRounds,omitempty"`
	// MaxNodes 节点数上限，防 LLM 画出一张 50 个节点的图。默认 12
	MaxNodes int `json:"maxNodes,omitempty"`
}

// ComposeLLM 可注入的 LLM 调用。测试里替换成假函数，才能验证修复循环而不是验证模型。
type ComposeLLM func(prompt string) (string, error)

// ComposeResult 生成结果
type ComposeResult struct {
	Goal   string               `json:"goal"`
	Nodes  []model.NodeInstance `json:"nodes"`
	Edges  []model.EdgeInstance `json:"edges"`
	Rounds int                  `json:"rounds"` // 用了几轮（1 = 首轮即合法）
	Score  GraphScore           `json:"score"`
	Legal  bool                 `json:"legal"`
	// Violations 各轮被检出并修掉的问题（全量留痕，便于判断"这个模型能不能用"）
	Violations []string `json:"violations"`
	// Lessons 本次检索到的经验（卡片来源），让人知道模型参考了什么
	Lessons []string `json:"lessons,omitempty"`
	// Raw 最后一轮的原始输出（只在失败时返回，便于排查）
	Raw string `json:"raw,omitempty"`
}

// ModuleCatalog 生成紧凑模块目录：一行一个类型，只含"接线与选择"需要的信息。
//
// 为什么不用完整契约：完整契约含 KeyParam 标签、Help、GateLayers，29 个模块能到 6k+ 字。
// 构图只需要知道「有这个类型、吃什么、吐什么、干什么用」。
func ModuleCatalog() string {
	types := make([]string, 0, len(Modules))
	for t := range Modules {
		types = append(types, t)
	}
	sort.Strings(types)

	var b strings.Builder
	b.WriteString("type | 用途 | 输入(端口:类型,?=可选) | 输出(端口:类型)\n")
	for _, t := range types {
		c := Modules[t]
		var ins, outs []string
		for _, p := range c.Inputs {
			req := ""
			if !p.Required {
				req = "?"
			}
			ins = append(ins, fmt.Sprintf("%s:%s%s", p.Name, p.Kind, req))
		}
		for _, p := range c.Outputs {
			outs = append(outs, fmt.Sprintf("%s:%s", p.Name, p.Kind))
		}
		inStr, outStr := "-", "-"
		if len(ins) > 0 {
			inStr = strings.Join(ins, " ")
		}
		if len(outs) > 0 {
			outStr = strings.Join(outs, " ")
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s\n", t, c.Title, inStr, outStr)
	}
	return b.String()
}

const composeInstruction = `你是 FlowEdge 的工作流编排器。把用户的一句话目标编译成一张**可执行**的图。

## 规则（违反任一即视为失败）

1. 只能使用下面「模块目录」里出现的 type。不存在的 type 一律不许写。
2. 必须有一个 start 节点和一个 end 节点。
3. 每条边两端必须存在；连线的端口类型必须兼容（text 只能进吃 text 的端口，image 只能进吃 image 的端口）。
4. 不许成环。
5. 节点 id 用短英文（如 s / t1 / pp / e），不要用中文。
6. 节点数不超过上限。
7. 需要判断的活儿（写文案/审片意见）可以用 agent_task，但**优先用确定性模块** —— 出图/转写/合成必须用确定性模块。
8. end 是终点，**它的后面不能再接任何节点**（接了那些节点永远不会执行）。
9. **已有文件必须用 input_source 接入**。目标里出现「这段录音 / 这张图 / 这段视频 / 这个文件」时，
   用文件源接在它前面，**不要**直接 start → asr（start 只输出 text，接不到 audio 端口）：
   - 转写录音：start(text) → input_source(kind=audio, path="待提供") → asr → llm → end
   - 图片放大：input_source(kind=image, path="待提供") → upscale → end
   - 横屏改竖屏：input_source(kind=video, path="待提供") → post_production → end
   还不知道文件路径时，path 一律写「待提供」——图能先跑通结构，运行时它会明确报「还没给文件」。
   input_source 的四个输出端口（text/image/audio/video）**互斥**：kind 选哪个就只出哪个，
   所以下游必须接同类型的端口。
10. 「拆成 N 段 / 分段处理 / 每段不超过 X 字」用 **text_split**：
    mode=by_count 配 count=N，或 mode=by_length 配 max_chars=N，或 mode=by_sentence 逐句。
    要整体念一遍就接它的 text 端口。**不要编造 split_text / text_segment 这类目录里没有的类型。**
11. **构图阶段不要用 loop**：loop 必须引用一条**已经存在的**工作流（workflow_id），
    现在还没有这个 id，画出来一定成环或端口错（已实测连错 3 轮）。
    需要"逐段处理"时：把 text_split 的 parts 接给下游，或在节点数上限内顺序展开几个同类节点。



## 输出格式（只输出 JSON，不要解释、不要 markdown 代码块）

{"nodes":[{"id":"s","type":"start","data":{"prompt":"..."}},{"id":"e","type":"end","data":{}}],"edges":[{"source":"s","target":"e"}]}

data 里只写这个节点**必要的**参数（如文生图的 prompt、后期的 aspect）。不要写 position，布局由引擎自动完成。
`

// ComposePrompt 拼提示词。lessons 是 RAG 检索到的经验卡片（可为空）。
func ComposePrompt(goal ComposeGoal, lessons []string) string {
	var b strings.Builder
	b.WriteString(composeInstruction)
	b.WriteString("\n## 模块目录\n\n")
	b.WriteString(ModuleCatalog())

	b.WriteString("\n## 本次约束\n\n")
	fmt.Fprintf(&b, "- 目标：%s\n", goal.Goal)
	if goal.TotalSeconds > 0 {
		fmt.Fprintf(&b, "- 总时长：约 %.0f 秒（多段生成时按时长分配，不要写死每段）\n", goal.TotalSeconds)
	}
	if goal.Aspect != "" {
		fmt.Fprintf(&b, "- 画幅：%s\n", goal.Aspect)
	}
	if goal.NoSpeech != nil {
		if *goal.NoSpeech {
			b.WriteString("- **无台词**：这是氛围片，必须配 forbid_speech 判据，不要接 tts\n")
		} else {
			b.WriteString("- 有台词：需要 tts 与字幕\n")
		}
	}
	maxNodes := goal.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 12
	}
	fmt.Fprintf(&b, "- 节点数上限：%d\n", maxNodes)

	if len(lessons) > 0 {
		b.WriteString("\n## 相关经验（来自项目知识库，必须遵守）\n\n")
		for i, l := range lessons {
			if i >= 6 {
				break
			}
			// 经验卡片可能很长，只取前 400 字，避免把上下文吃满
			fmt.Fprintf(&b, "- %s\n", truncateRunes(l, 400))
		}
	}
	b.WriteString("\n现在输出 JSON：")
	return b.String()
}

// ComposeGraph 生成 → 校验 → 回喂修复。
//
// llm 为 nil 直接报错（而不是静默返回空图）。测试可注入假函数。
func ComposeGraph(goal ComposeGoal, lessons []string, llm ComposeLLM) (ComposeResult, error) {
	res := ComposeResult{Goal: goal.Goal, Lessons: lessons}
	if strings.TrimSpace(goal.Goal) == "" {
		return res, fmt.Errorf("目标不能为空")
	}
	if llm == nil {
		return res, fmt.Errorf("LLM 不可用：构图需要一次模型调用（检查 Ollama 是否在跑）")
	}

	maxRounds := goal.MaxRepairRounds
	if maxRounds <= 0 {
		maxRounds = 2
	}
	if maxRounds > 4 {
		maxRounds = 4
	}
	maxNodes := goal.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 12
	}

	prompt := ComposePrompt(goal, lessons)
	var lastRaw string

	for round := 1; round <= maxRounds+1; round++ {
		res.Rounds = round
		raw, err := llm(prompt)
		if err != nil {
			return res, fmt.Errorf("第 %d 轮调用失败：%w", round, err)
		}
		lastRaw = raw

		nodes, edges, perr := parseComposeOutput(raw)
		if perr != nil {
			// 解析失败也要回喂 —— 把"你输出的不是合法 JSON"具体告诉他
			res.Violations = append(res.Violations, fmt.Sprintf("第%d轮解析失败：%v", round, perr))
			prompt = prompt + fmt.Sprintf("\n\n## 上一版的问题\n\n输出无法解析为 JSON：%v\n只输出 JSON 对象，不要 markdown 代码块。", perr)
			continue
		}

		probs := validateComposed(nodes, edges, maxNodes)
		if len(probs) == 0 {
			res.Nodes, res.Edges = nodes, edges
			res.Score = ScoreGraph(nodes, edges)
			res.Legal = res.Score.Legal
			return res, nil
		}

		for _, p := range probs {
			res.Violations = append(res.Violations, fmt.Sprintf("第%d轮：%s", round, p))
		}
		// 回喂必须带两样东西，缺一样模型就只会"原样重交"：
		//  ① 上一版的图 —— 否则它不知道要改哪条边，只能凭记忆重画；
		//  ② 修法指引 —— 只说"端口不兼容"，模型会原地打转（2026-10-23 实测：
		//     同一条 t2i→pp 错了 3 轮，一字不改）。
		prompt = prompt + "\n\n## 上一版的图（在此基础上改，别重画）\n\n```json\n" +
			truncateRunes(lastRaw, 2400) + "\n```" +
			"\n\n## 上一版的问题（逐条修）\n\n- " +
			strings.Join(probs, "\n- ") + composeRepairHint + "\n\n重新输出完整 JSON："
	}

	res.Raw = lastRaw
	return res, fmt.Errorf("生成 %d 轮后仍不合法（最后的问题见 violations）", maxRounds+1)
}

// parseComposeOutput 从 LLM 输出里抠出 JSON 并转成节点/边。
//
// LLM 常见的三种脏输出：```json 包裹、前后有解释、字段名大小写不一致。
// 全部在这里吸收掉 —— 不要因为模型加了三个反引号就判"生成失败"。
func parseComposeOutput(raw string) ([]model.NodeInstance, []model.EdgeInstance, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	if s == "" {
		return nil, nil, fmt.Errorf("输出里没有 JSON 对象")
	}

	var payload struct {
		Nodes []struct {
			ID    string                 `json:"id"`
			Type  string                 `json:"type"`
			Data  map[string]interface{} `json:"data"`
			Label string                 `json:"label"`
		} `json:"nodes"`
		Edges []struct {
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(s), &payload); err != nil {
		return nil, nil, err
	}
	if len(payload.Nodes) == 0 {
		return nil, nil, fmt.Errorf("nodes 为空")
	}

	nodes := make([]model.NodeInstance, 0, len(payload.Nodes))
	for i, n := range payload.Nodes {
		id := strings.TrimSpace(n.ID)
		if id == "" {
			id = fmt.Sprintf("n%d", i+1)
		}
		data := n.Data
		if data == nil {
			data = map[string]interface{}{}
		}
		nodes = append(nodes, model.NodeInstance{ID: id, Type: strings.TrimSpace(n.Type), Data: data})
	}
	edges := make([]model.EdgeInstance, 0, len(payload.Edges))
	for i, e := range payload.Edges {
		edges = append(edges, model.EdgeInstance{
			ID:     fmt.Sprintf("ce%d", i+1),
			Source: strings.TrimSpace(e.Source),
			Target: strings.TrimSpace(e.Target),
		})
	}
	return nodes, edges, nil
}

// validateComposed 校验生成的图。返回人话问题清单（会原样回喂给 LLM）。
//
// 顺序有意为之：先说"类型不存在"（最致命，模型最容易编），再说接线，最后结构。
func validateComposed(nodes []model.NodeInstance, edges []model.EdgeInstance, maxNodes int) []string {
	var probs []string

	if len(nodes) > maxNodes {
		probs = append(probs, fmt.Sprintf("节点数 %d 超过上限 %d，请合并步骤", len(nodes), maxNodes))
	}

	byID := map[string]*model.NodeInstance{}
	hasStart, hasEnd := false, false
	for i := range nodes {
		n := &nodes[i]
		if _, dup := byID[n.ID]; dup {
			probs = append(probs, fmt.Sprintf("节点 id %q 重复", n.ID))
		}
		byID[n.ID] = n
		if c, ok := ModuleContractOf(n.Type); ok {
			// 用 Role 判，不写 "start"/"end" 字面量 —— 与 spec/preflight 同一条纪律
			if c.Role == RoleSource {
				hasStart = true
			}
			if c.Role == RoleSink {
				hasEnd = true
			}
			// 「引用已存工作流」的模块在构图阶段不可用：那张子图还不存在，id 无从填。
			// 判据用**参数名**，不写节点类型：契约里声明了 workflow_id 就是这类模块。
			//
			// 2026-10-23 实测漏判：`text_split → loop → tts → subtitles` 被判 legal=1.00，
			// 而 loop 没有 workflow_id，运行时必然报 "loop node has no workflow_id" ——
			// 结构分给满分、跑起来必挂，这是门没有牙齿的典型。
			for _, kp := range c.KeyParams {
				if kp.Key != "workflow_id" {
					continue
				}
				if dataInt(n.Data, "workflow_id", 0) == 0 {
					probs = append(probs, fmt.Sprintf(
						"节点 %q（%s）需要引用一条已存的工作流（workflow_id），构图阶段还没有这个 id。"+
							"把它换掉：要逐段处理就把 text_split 的 parts 接给下游，不要用需要引用子流程的模块",
						n.ID, n.Type))
				}
			}
		} else {
			probs = append(probs, fmt.Sprintf("节点 %q 的 type=%q 不存在，只能从模块目录里选", n.ID, n.Type))
		}
	}
	if !hasStart {
		probs = append(probs, "缺少 start 节点")
	}
	if !hasEnd {
		probs = append(probs, "缺少 end 节点")
	}

	for _, e := range edges {
		src, okS := byID[e.Source]
		if !okS {
			probs = append(probs, fmt.Sprintf("边 %s→%s 的源节点不存在", e.Source, e.Target))
		}
		if _, okT := byID[e.Target]; !okT {
			probs = append(probs, fmt.Sprintf("边 %s→%s 的目标节点不存在", e.Source, e.Target))
		}
		// end 是终点（RoleSink），它后面再挂节点意味着那些节点永远不会执行。
		// 2026-10-23 标定第 6 条就是这样混过去的：start→upscale→end→text2image→vision
		// 结构评分给了 1.00，因为它只查"边两端存不存在、端口通不通"。
		if okS {
			if c, ok := ModuleContractOf(src.Type); ok && c.Role == RoleSink {
				probs = append(probs, fmt.Sprintf("节点 %q 是终点（%s），它后面不能再接 %q —— 那个节点永远不会执行",
					src.ID, src.Type, e.Target))
			}
		}
	}

	// 结构分：端口兼容 / 成环 / 必填无源，复用同一个校验器
	sc := ScoreGraph(nodes, edges)
	for _, m := range sc.PortMismatches {
		probs = append(probs, "端口不兼容："+m)
	}
	if len(sc.Cycles) > 0 {
		probs = append(probs, "图里成环，涉及节点："+strings.Join(sc.Cycles, ", "))
	}
	for _, r := range sc.RequiredNoFeed {
		probs = append(probs, "必填输入没有来源："+r)
	}
	return probs
}

// composeRepairHint 教「怎么修」，不只说「哪里错」。
//
// 刻意只写元规则、不写具体节点链条：上一轮试过给 few-shot 示例，
// 模型把示例当模板套到底（只有 asr 那个例子会了，upscale/post_production 全不会）。
// 这里说的是"怎么查、往哪个方向补"，具体补哪个模块由它按模块目录自己判。
const composeRepairHint = `

修端口不兼容的顺序：
1. 查模块目录里上游模块的「输出」列，看它到底产出什么类型；
2. 再查下游模块的「输入」列，看它到底要什么类型；
3. 两者不同就往中间补一步转换，例如「图片 → 视频」需要视频生成模块先出片；
   不要保留那条不兼容的边原样重交，也不要靠删掉下游节点来"消掉"错误（那会让目标完不成）。`
