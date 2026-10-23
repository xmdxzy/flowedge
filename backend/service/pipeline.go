package service

// 节点能力管线：把「贴卡」从单张升级成三段式处理链。
//
// 为什么需要三段
// --------------
// 一个节点干的事其实是三步：**处理上游输入 → 干本体的活 → 处理自己的产出**。
// 只允许挂一张卡，等于强迫用户把"翻译 → 改写 → 出图"拆成三个节点，图上连一堆线，
// 而这三次文本变换共用同一个模型、同一侧显存、同一份上下文——拆开只是让图变难看。
//
// 所以绑定改成有序列表，每张卡声明自己作用于哪一段：
//
//	stage=input    上游文本进来先过它（可多张，按 order 串行）
//	stage=body     本体换角色（at most 1：两个 system prompt 会互相打架）
//	stage=output   本体产出再过它（可多张，按 order 串行）
//	stage=replace  整个节点被一条工作流替换（at most 1，唯一改执行结构）
//
// replace 与 input/output 可以共存：先处理输入 → 跑那条工作流 → 再处理它的产出。
// replace 与 body 互斥：本体都被替换了，角色卡没有落点。
//
// 兼容
// ----
// 旧数据是 `node.Data["binding"] = {kind,id}`（单选）。读取时自动迁移：
// agent/skill → stage=body，workflow → stage=replace。**不写回**，避免保存时改动用户数据。
//
// 成本
// ----
// 每张 agent/skill 卡都是一次 LLM 调用（几秒）。UI 必须显示"这个节点会调用 N 次模型"，
// 否则用户会在 input 段堆五张卡，然后疑惑为什么一键运行跑了半分钟还没出图。

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"flowedge/backend/model"
)

// 管线阶段。
const (
	StageInput   = "input"
	StageBody    = "body"
	StageOutput  = "output"
	StageReplace = "replace"
)

// StageLabel 人话标签（面板与任务书共用，避免两处叫法不一致）。
var StageLabel = map[string]string{
	StageInput:   "输入处理",
	StageBody:    "本体角色",
	StageOutput:  "输出处理",
	StageReplace: "整节点替换",
}

// CardRef 一条绑定。
type CardRef struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Stage string `json:"stage"`
	// Order 同段内的执行顺序（小的先跑）。空/0 视为按数组顺序。
	Order int `json:"order,omitempty"`
}

// Pipeline 一个节点的完整处理链。
type Pipeline struct {
	Input   []CardRef
	Body    *CardRef
	Output  []CardRef
	Replace *CardRef
}

// ParsePipeline 从 node.Data 解析管线，并兼容旧的单选 binding。
func ParsePipeline(data map[string]interface{}) Pipeline {
	var p Pipeline

	// 新格式
	if raw, ok := data["bindings"].([]interface{}); ok {
		for _, item := range raw {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			ref := CardRef{
				Kind:  str(m["kind"]),
				ID:    str(m["id"]),
				Stage: str(m["stage"]),
			}
			ref.Order = intOf(m["order"])
			if !validCardKind(ref.Kind) || ref.ID == "" {
				continue
			}
			if ref.Stage == "" {
				ref.Stage = defaultStage(ref.Kind)
			}
			p.add(ref)
		}
	} else if raw, ok := data["binding"].(map[string]interface{}); ok {
		// 旧格式：单选
		ref := CardRef{Kind: str(raw["kind"]), ID: str(raw["id"])}
		if validCardKind(ref.Kind) && ref.ID != "" {
			ref.Stage = defaultStage(ref.Kind)
			p.add(ref)
		}
	}

	sortRefs(p.Input)
	sortRefs(p.Output)
	return p
}

// add 按阶段归档，并强制「body / replace 各限 1 张」。
// 冲突时保留先出现的那张——先声明的优先级更高，行为可预测。
func (p *Pipeline) add(ref CardRef) {
	switch ref.Stage {
	case StageInput:
		p.Input = append(p.Input, ref)
	case StageOutput:
		p.Output = append(p.Output, ref)
	case StageBody:
		if p.Body == nil {
			c := ref
			p.Body = &c
		}
	case StageReplace:
		if p.Replace == nil {
			c := ref
			p.Replace = &c
		}
	}
}

// ValidatePipeline 保存期校验：返回人话错误。
func ValidatePipeline(data map[string]interface{}) []string {
	var issues []string
	raw, _ := data["bindings"].([]interface{})
	if len(raw) > 8 {
		issues = append(issues, fmt.Sprintf("单个节点最多挂 8 张卡（当前 %d 张）：每张卡都是一次模型调用，堆太多会让单步变慢", len(raw)))
	}

	var bodySeen, replaceSeen bool
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		kind, id, stage := str(m["kind"]), str(m["id"]), str(m["stage"])
		if !validCardKind(kind) {
			issues = append(issues, fmt.Sprintf("未知卡片类型 %q", kind))
			continue
		}
		if id == "" {
			issues = append(issues, "有一张卡没有选具体的卡片")
			continue
		}
		if stage == "" {
			stage = defaultStage(kind)
		}
		if stage != StageInput && stage != StageBody && stage != StageOutput && stage != StageReplace {
			issues = append(issues, fmt.Sprintf("卡片 %s 的阶段 %q 不合法（只能是 input/body/output/replace）", id, stage))
		}
		if stage == StageBody {
			if bodySeen {
				issues = append(issues, "本体角色只能有 1 张：两个角色设定会互相打架")
			}
			bodySeen = true
			if kind == CardWorkflow {
				issues = append(issues, "工作流卡不能放在本体角色段：它替换的是整个节点（用 replace）")
			}
		}
		if stage == StageReplace {
			if replaceSeen {
				issues = append(issues, "整节点替换只能有 1 张工作流卡")
			}
			replaceSeen = true
			if kind != CardWorkflow {
				issues = append(issues, "只有工作流卡可以做整节点替换")
			}
		}
	}
	if bodySeen && replaceSeen {
		issues = append(issues, "本体角色卡与整节点替换卡不能同时存在：本体已被替换，角色没有落点")
	}
	return issues
}

// PipelineCallCount 该节点会触发多少次 LLM 调用（input + body + output）。
// 前端用它显示成本，避免用户在输入段堆五张卡。
func PipelineCallCount(p Pipeline) int {
	n := len(p.Input) + len(p.Output)
	if p.Body != nil {
		n++
	}
	return n
}

func defaultStage(kind string) string {
	if kind == CardWorkflow {
		return StageReplace
	}
	return StageBody
}

func validCardKind(k string) bool {
	return k == CardAgent || k == CardSkill || k == CardWorkflow
}

func sortRefs(refs []CardRef) {
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Order < refs[j].Order })
}

func str(v interface{}) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// ---- 执行 ----------------------------------------------------------------

// cardMedia 这张卡吃什么媒体（"" 表示纯文本卡）。
func (e *EngineV2) cardMedia(ref CardRef) string {
	if e.cards == nil || ref.Kind != CardSkill {
		return ""
	}
	if c, ok := e.cards.Skill(ref.ID); ok {
		return c.Media
	}
	return ""
}

// imagePathsOf 从节点产出里抽取图片路径（兼容单张、数组、聚合的 values）。
func imagePathsOf(m map[string]interface{}) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if s, ok := m["image"].(string); ok {
		add(s)
	}
	switch v := m["images"].(type) {
	case []string:
		for _, s := range v {
			add(s)
		}
	case []interface{}:
		for _, x := range v {
			if s, ok := x.(string); ok {
				add(s)
			}
		}
	}
	if arr, ok := m["values"].([]interface{}); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok && looksLikeImagePath(s) {
				add(s)
			}
		}
	}
	return out
}

// cardSystem 取一张卡的提示词。
func (e *EngineV2) cardSystem(ref CardRef) (string, bool) {
	if e.cards == nil {
		return "", false
	}
	switch ref.Kind {
	case CardAgent:
		if c, ok := e.cards.Agent(ref.ID); ok {
			return c.System, true
		}
	case CardSkill:
		if c, ok := e.cards.Skill(ref.ID); ok {
			return c.System, true
		}
	}
	return "", false
}

// cardTrace 一个节点这次执行里，各张卡的执行结果。
//
// 没有它，卡片是"黑盒"：用户贴了"中译英"，跑完只看到一段英文，
// 不知道是卡翻的还是模型本来就会；某张卡报错也会被静默降级吞掉。
type cardTrace struct {
	mu    sync.Mutex
	items []map[string]interface{}
}

func (t *cardTrace) add(stage string, ref CardRef, ok bool, err string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rec := map[string]interface{}{
		"stage": stage,
		"kind":  ref.Kind,
		"id":    ref.ID,
		"ok":    ok,
	}
	if err != "" {
		rec["error"] = err
	}
	t.items = append(t.items, rec)
}

func (t *cardTrace) list() []map[string]interface{} {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]map[string]interface{}, len(t.items))
	copy(out, t.items)
	return out
}

// runStage 依次用每张卡处理文本；任何一张失败都保留上一步结果并记进 trace，
// 不阻断节点执行——预处理是增强，不该把主流程带崩。
func (e *EngineV2) runStage(node *model.NodeInstance, text string, refs []CardRef, stage string, ct *cardTrace) string {
	out := text
	if strings.TrimSpace(out) == "" {
		return out
	}
	for _, ref := range refs {
		sys, ok := e.cardSystem(ref)
		if !ok {
			ct.add(stage, ref, false, "卡片不存在（可能已被删除）")
			continue
		}
		// 卡片提示词 = 指令，原文 = 输入主体。与 execLLMNode 的拼接方式保持一致，
		// 避免"同一个卡在不同节点上被喂成不同形状"。
		next, err := e.deterministicChat("", sys+"\n\n"+out)
		if err != nil || strings.TrimSpace(next) == "" {
			// 失败不冒泡：保留上一步结果继续，但在 trace 里留痕
			msg := "返回空结果"
			if err != nil {
				msg = err.Error()
			}
			ct.add(stage, ref, false, msg)
			continue
		}
		out = strings.TrimSpace(next)
		ct.add(stage, ref, true, "")
	}
	return out
}

// applyOutputStage 对 LLM 节点的文本产出应用输出段。
// 只改 text 字段，其它字段（_usage / tool_calls / image…）原样保留：
// 输出段的职责是"把话说成我要的样子"，不是重造产物。
func (e *EngineV2) applyOutputStage(node *model.NodeInstance, out interface{}, refs []CardRef, ct *cardTrace) interface{} {
	m, ok := out.(map[string]interface{})
	if !ok {
		return out
	}
	if len(refs) > 0 {
		// 1) 文本整形：只对 text 生效
		if t, ok := m["text"].(string); ok && strings.TrimSpace(t) != "" {
			m["text"] = e.runStage(node, t, refs, StageOutput, ct)
		}
		// 2) 媒体段：产出是图时，跑声明了 media=image 的卡（审片 / 反推描述）。
		//
		// 为什么必须单独一条路：文本卡走的是 ChatWithTemperature，图片要走 VisionChat
		// （base64 图像 + VL 模型）。以前统一按"改 text 字段"处理，图节点就永远没有输出段可做。
		if imgs := imagePathsOf(m); len(imgs) > 0 {
			e.runMediaStage(node, m, imgs, refs, ct)
		}
	}
	if tr := ct.list(); len(tr) > 0 {
		m["_cards"] = tr
	}
	return m
}

// runMediaStage 让 media=image 的卡看产出的图，结论写回 review（无 text 时同时写 text）。
//
// 代价要说清楚：VL 模型在语言侧，图刚出完就跑审片会触发一次换侧；
// 一张图一分钟起。所以它是**可选**的，不是默认行为。
func (e *EngineV2) runMediaStage(node *model.NodeInstance, m map[string]interface{}, images []string, refs []CardRef, ct *cardTrace) {
	for _, ref := range refs {
		if e.cardMedia(ref) != "image" {
			continue
		}
		sys, ok := e.cardSystem(ref)
		if !ok {
			ct.add(StageOutput, ref, false, "卡片不存在（可能已被删除）")
			continue
		}
		modelName, merr := e.resolveVisionModel(node)
		if merr != nil {
			ct.add(StageOutput, ref, false, "没有可用的视觉模型："+merr.Error())
			continue
		}
		release := e.enterLanguageSide(modelName)
		text, _, err := e.llm.VisionChat(sys, images, modelName)
		release()
		if err != nil {
			ct.add(StageOutput, ref, false, err.Error())
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			ct.add(StageOutput, ref, false, "视觉模型返回空结果")
			continue
		}
		m["review"] = text
		if _, has := m["text"]; !has {
			// 图节点本来没有 text：补上，好让下游的质检门 / LLM 直接吃审片结论
			m["text"] = text
		}
		ct.add(StageOutput, ref, true, "")
	}
}

// intOf 宽容取整：JSON 解出来是 float64，Go 字面量可能是 int/int64。
// 只认 float64 的话，order 会静默变 0，多卡顺序就随机了。
func intOf(v interface{}) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	}
	return 0
}

// applyInputStage 在节点派发前统一处理上游文本。
//
// 为什么在这里做，而不是每个执行体各做一次：
//   - 20 个节点各写一遍，必然漏几个，且"新加节点默认没有"是个隐形坑；
//   - 预处理只关心"文本进节点前长什么样"，与节点内部逻辑无关。
//
// 注意：**不改原 map**。上游 map 可能同时被同层其他节点共享（并行执行），
// 就地改会把别人的输入也改掉——这类 bug 只在并发下偶发，极难查。
func (e *EngineV2) applyInputStage(node *model.NodeInstance, upstream map[string]interface{}, ct *cardTrace) map[string]interface{} {
	pipe := ParsePipeline(node.Data)
	if len(pipe.Input) == 0 || len(upstream) == 0 {
		return upstream
	}
	out := make(map[string]interface{}, len(upstream))
	for k, v := range upstream {
		m, ok := v.(map[string]interface{})
		if !ok {
			out[k] = v
			continue
		}
		cp := make(map[string]interface{}, len(m))
		for mk, mv := range m {
			cp[mk] = mv
		}
		if t, ok := cp["text"].(string); ok && strings.TrimSpace(t) != "" {
			cp["text"] = e.runStage(node, t, pipe.Input, StageInput, ct)
		}
		out[k] = cp
	}
	return out
}
