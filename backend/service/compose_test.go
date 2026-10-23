package service

import (
	"strings"
	"testing"
)

// 构图的测试重点是**修复循环**，不是模型能力：
// 用假 LLM 精确控制"第几轮返回什么"，才能验证"违规被检出 → 回喂 → 修好"这条链。
// 用真模型测这个，等于把模型的不稳定当成代码的不稳定。

func fakeComposeLLM(responses ...string) ComposeLLM {
	i := 0
	return func(_ string) (string, error) {
		if i >= len(responses) {
			return responses[len(responses)-1], nil
		}
		out := responses[i]
		i++
		return out, nil
	}
}

const composeGoodGraph = `{"nodes":[
 {"id":"s","type":"start","data":{"prompt":"雨夜"}},
 {"id":"l","type":"llm","data":{"prompt":"写旁白"}},
 {"id":"e","type":"end","data":{}}],
 "edges":[{"source":"s","target":"l"},{"source":"l","target":"e"}]}`

// 首轮就合法 → Rounds=1，且不该有多余回喂
func TestCompose_FirstRoundLegal(t *testing.T) {
	res, err := ComposeGraph(ComposeGoal{Goal: "写一段旁白"}, nil, fakeComposeLLM(composeGoodGraph))
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if res.Rounds != 1 {
		t.Fatalf("应首轮成功，实际 %d 轮", res.Rounds)
	}
	if !res.Legal {
		t.Fatalf("应合法：%s", res.Score.Summarize())
	}
	if len(res.Nodes) != 3 || len(res.Edges) != 2 {
		t.Fatalf("节点/边数不对：%d/%d", len(res.Nodes), len(res.Edges))
	}
	if len(res.Violations) != 0 {
		t.Fatalf("这一轮不该有违规记录：%v", res.Violations)
	}
}

// 用了不存在的 type → 检出并回喂 → 第二轮修好
func TestCompose_RepairsUnknownType(t *testing.T) {
	bad := `{"nodes":[
	 {"id":"s","type":"start","data":{}},
	 {"id":"x","type":"magic_video","data":{}},
	 {"id":"e","type":"end","data":{}}],
	 "edges":[{"source":"s","target":"x"},{"source":"x","target":"e"}]}`

	res, err := ComposeGraph(ComposeGoal{Goal: "测试", MaxRepairRounds: 2}, nil, fakeComposeLLM(bad, composeGoodGraph))
	if err != nil {
		t.Fatalf("第二轮应修好：%v", err)
	}
	if res.Rounds != 2 {
		t.Fatalf("应 2 轮，实际 %d", res.Rounds)
	}
	if len(res.Violations) == 0 {
		t.Fatal("第一轮的问题必须被记录（否则看不到修复过程）")
	}
	joined := strings.Join(res.Violations, " | ")
	if !strings.Contains(joined, "magic_video") {
		t.Fatalf("违规里要点名那个不存在的 type：%s", joined)
	}
}

// 每轮都不合法 → 报错，且带最后一版原始输出（调提示词的材料）
func TestCompose_FailsAfterAllRounds(t *testing.T) {
	bad := `{"nodes":[{"id":"s","type":"start"},{"id":"x","type":"nope"}],"edges":[]}`
	res, err := ComposeGraph(ComposeGoal{Goal: "测试", MaxRepairRounds: 1}, nil, fakeComposeLLM(bad, bad))
	if err == nil {
		t.Fatal("两轮都不合法应报错")
	}
	if res.Raw == "" {
		t.Fatal("失败时要返回原始输出，否则没法判断是模型问题还是解析问题")
	}
	if res.Rounds != 2 {
		t.Fatalf("maxRepairRounds=1 时应跑 2 轮，实际 %d", res.Rounds)
	}
}

// LLM 常把 JSON 包在 ```json 里，或前面加一句解释 —— 必须能吸收
func TestCompose_ParsesDirtyOutput(t *testing.T) {
	dirty := "好的，这是构图：\n```json\n" + composeGoodGraph + "\n```\n希望有帮助。"
	res, err := ComposeGraph(ComposeGoal{Goal: "测试"}, nil, fakeComposeLLM(dirty))
	if err != nil {
		t.Fatalf("脏输出应能解析：%v", err)
	}
	if len(res.Nodes) != 3 {
		t.Fatalf("解析结果不对：%d 个节点", len(res.Nodes))
	}
}

// 缺 end → 检出
func TestCompose_MissingEndDetected(t *testing.T) {
	noEnd := `{"nodes":[{"id":"s","type":"start","data":{}}],"edges":[]}`
	res, err := ComposeGraph(ComposeGoal{Goal: "测试", MaxRepairRounds: 0}, nil, fakeComposeLLM(noEnd, noEnd))
	if err == nil {
		t.Fatal("缺 end 应失败")
	}
	joined := strings.Join(res.Violations, " | ")
	if !strings.Contains(joined, "end") {
		t.Fatalf("违规要提到缺 end：%s", joined)
	}
}

// 成环 → 检出（用真实节点类型构造）
func TestCompose_CycleDetected(t *testing.T) {
	cyc := `{"nodes":[
	 {"id":"s","type":"start","data":{}},
	 {"id":"a","type":"llm","data":{}},
	 {"id":"b","type":"llm","data":{}},
	 {"id":"e","type":"end","data":{}}],
	 "edges":[{"source":"s","target":"a"},{"source":"a","target":"b"},
	          {"source":"b","target":"a"},{"source":"b","target":"e"}]}`
	res, err := ComposeGraph(ComposeGoal{Goal: "测试", MaxRepairRounds: 0}, nil, fakeComposeLLM(cyc, cyc))
	if err == nil {
		t.Fatal("成环应失败")
	}
	joined := strings.Join(res.Violations, " | ")
	if !strings.Contains(joined, "成环") && !strings.Contains(joined, "环") {
		t.Fatalf("违规要提到成环：%s", joined)
	}
}

// 节点数超上限 → 检出
func TestCompose_MaxNodesEnforced(t *testing.T) {
	var ns []string
	ns = append(ns, `{"id":"s","type":"start","data":{}}`)
	for i := 0; i < 20; i++ {
		ns = append(ns, `{"id":"n`+string(rune('a'+i))+`","type":"llm","data":{}}`)
	}
	ns = append(ns, `{"id":"e","type":"end","data":{}}`)
	big := `{"nodes":[` + strings.Join(ns, ",") + `],"edges":[]}`

	res, err := ComposeGraph(ComposeGoal{Goal: "测试", MaxNodes: 5, MaxRepairRounds: 0}, nil, fakeComposeLLM(big, big))
	if err == nil {
		t.Fatal("超节点上限应失败")
	}
	joined := strings.Join(res.Violations, " | ")
	if !strings.Contains(joined, "上限") {
		t.Fatalf("违规要提到上限：%s", joined)
	}
}

// 空目标 / nil LLM → 明确报错（不静默返回空图）
func TestCompose_Guardrails(t *testing.T) {
	if _, err := ComposeGraph(ComposeGoal{Goal: "  "}, nil, fakeComposeLLM(composeGoodGraph)); err == nil {
		t.Fatal("空目标应报错")
	}
	if _, err := ComposeGraph(ComposeGoal{Goal: "x"}, nil, nil); err == nil {
		t.Fatal("nil LLM 应报错")
	}
}

// 模块目录必须覆盖全部模块（否则 LLM 永远"选不到"某个节点）
func TestCompose_CatalogCoversAllModules(t *testing.T) {
	cat := ModuleCatalog()
	for typ := range Modules {
		if !strings.Contains(cat, typ+" |") {
			t.Fatalf("模块目录漏了 %s", typ)
		}
	}
	// 目录要紧凑：一行一个模块，别把 Help 全塞进去
	lines := strings.Count(cat, "\n")
	if lines > len(Modules)+2 {
		t.Fatalf("目录 %d 行，模块 %d 个 —— 说明混进了多余内容", lines, len(Modules))
	}
}

// 提示词必须带约束与经验（否则模型拿到的是"随便画一张图"）
func TestCompose_PromptCarriesConstraints(t *testing.T) {
	no := true
	p := ComposePrompt(ComposeGoal{
		Goal: "雨夜空镜", TotalSeconds: 30, Aspect: "9:16", NoSpeech: &no, MaxNodes: 8,
	}, []string{"响度归一到 -14 LUFS", "amix 必须 normalize=0"})

	for _, want := range []string{"雨夜空镜", "30", "9:16", "无台词", "8", "-14 LUFS", "normalize=0"} {
		if !strings.Contains(p, want) {
			t.Fatalf("提示词缺 %q", want)
		}
	}
}

// 经验过长要截断，不能把上下文吃满（CodeGraph 的教训）。
//
// 断言的是**经验的增量**，不是提示词绝对长度：指令与模块目录本身会随节点增加而变长，
// 用绝对值做阈值会在"加节点"时无缘无故失败（第一版就是这么挂的）。
func TestCompose_LessonTruncated(t *testing.T) {
	base := len([]rune(ComposePrompt(ComposeGoal{Goal: "x"}, nil)))
	long := strings.Repeat("很长的经验内容", 200) // 1400 字
	with := len([]rune(ComposePrompt(ComposeGoal{Goal: "x"}, []string{long})))

	// 一条经验截断到 400 字 + 小标题，增量应在 600 以内
	if delta := with - base; delta > 600 {
		t.Fatalf("经验没截断：提示词多出 %d 字（单条约 400 字上限）", delta)
	}
}

// end 后面还接节点 → 必须拒绝。
//
// 这是 2026-10-23 标定抓到的漏洞：`start→upscale→end→text2image→vision`
// 被结构评分判了 1.00，因为 ScoreGraph 只查"边两端存不存在、端口通不通"，
// 不查"这条边从终点出发是否还有意义"。那些节点永远不会执行。
func TestCompose_NodeAfterEndRejected(t *testing.T) {
	g := `{"nodes":[
	 {"id":"s","type":"start","data":{}},
	 {"id":"u","type":"upscale","data":{}},
	 {"id":"e","type":"end","data":{}},
	 {"id":"t2i","type":"text2image","data":{}}],
	 "edges":[{"source":"s","target":"u"},{"source":"u","target":"e"},{"source":"e","target":"t2i"}]}`
	res, err := ComposeGraph(ComposeGoal{Goal: "测试", MaxRepairRounds: 0}, nil, fakeComposeLLM(g, g))
	if err == nil {
		t.Fatal("end 后面接节点应被判违规")
	}
	joined := strings.Join(res.Violations, " | ")
	if !strings.Contains(joined, "终点") {
		t.Fatalf("违规要说清是终点之后不能接：%s", joined)
	}
}

// 「引用已存工作流」的模块在构图阶段必须被拦下。
//
// 2026-10-23 实测漏判：`text_split → loop → tts → subtitles` 被判 legal=1.00，
// 而 loop 没有 workflow_id，运行时必然报 "loop node has no workflow_id"。
// **结构分给满分、跑起来必挂** —— 这是门没有牙齿的典型，比"判不过"更糟：
// 它让人以为图是好的。
func TestCompose_StoredWorkflowRefRejected(t *testing.T) {
	g := `{"nodes":[
	 {"id":"s","type":"start","data":{"prompt":"x"}},
	 {"id":"l","type":"loop","data":{"mode":"foreach"}},
	 {"id":"e","type":"end","data":{}}],
	 "edges":[{"source":"s","target":"l"},{"source":"l","target":"e"}]}`
	res, err := ComposeGraph(ComposeGoal{Goal: "测试", MaxRepairRounds: 0}, nil, fakeComposeLLM(g, g))
	if err == nil {
		t.Fatal("loop 缺 workflow_id 应被判违规（否则给出一个跑不起来的图）")
	}
	joined := strings.Join(res.Violations, " | ")
	if !strings.Contains(joined, "workflow_id") {
		t.Fatalf("违规要说清是缺 workflow_id：%s", joined)
	}
	// 提示里要给替代方案，否则模型只会换个写法继续错
	if !strings.Contains(joined, "text_split") {
		t.Fatalf("要指出替代做法：%s", joined)
	}
}
