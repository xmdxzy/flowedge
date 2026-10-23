package service

// 能力卡片（Capability Cards）——「万物皆可模块」的第二层：把**人**的编排意图
// 变成可以贴到任意节点上的可复用能力。
//
// 三类卡片，语义完全不同，不能混为一谈：
//
//	agent     智能体卡：一段稳定的角色设定 + 默认工具。贴到文本节点上，
//	          该节点就"变成这个角色在干活"（编剧 / 提示词工程师 / 审片员）。
//	skill     技能卡：一次确定性文本变换（中译英 / 抽 JSON / 摘要）。
//	          temperature 固定 0，同输入必同输出，可缓存、可复现。
//	workflow  封装卡：一整条已保存的工作流。贴到**任意**节点上，
//	          该节点被替换成一次子工作流调用——这是唯一能改执行结构的一类。
//
// 为什么卡片不写进节点类型
// ------------------------
// 卡片是**内容**不是**结构**：同一张「短剧导演」卡可以贴在任何 llm 节点上，
// 也可以同时贴在三处。把它做成节点类型会立刻产生"10 个节点 × N 张卡"的组合爆炸，
// 还会让 spec/端口/调度全部被迫认识每一个内容变体。
// 贴卡（binding）只改参数与提示词，不改端口、不改侧别、不改依赖。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// 绑定 ID 的两种形态：agent/skill 用字符串 id，workflow 用工作流数字 id。
// 统一成字符串存，读的时候再按 kind 解释——省一张表。

// WorkflowIDOfBinding 从 workflow 绑定的 id 取工作流号。
func WorkflowIDOfBinding(b Binding) (uint, bool) {
	if b.Kind != CardWorkflow {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(b.ID), 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint(n), true
}

// CardKind 卡片类型。
const (
	CardAgent    = "agent"
	CardSkill    = "skill"
	CardWorkflow = "workflow"
)

// AgentCard 智能体卡。
type AgentCard struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`   // 一句话身份，面板上显示
	System string `json:"system"` // 系统提示词（贴卡后作为指令前缀）
	// Tools 默认工具；节点自己勾了工具时以节点为准（就近优先）。
	Tools []string `json:"tools,omitempty"`
	// Icon 单字符图标（前端用它做视觉锚点，不用 emoji 库）
	Icon string `json:"icon,omitempty"`
	// Category 大类（库里的分组），Tags 细类（可搜可筛）
	Category string   `json:"category"`
	Tags     []string `json:"tags,omitempty"`
}

// SkillCard 技能卡。
type SkillCard struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	System string `json:"system"`
	Icon   string `json:"icon,omitempty"`
	// OutputHint 产物形态提示（"纯英文单行"、"JSON"），面板上显示，避免用户猜
	OutputHint string `json:"output_hint,omitempty"`
	// OutKind 产物类型：text / json。放输出段时前端据此判断下游能不能接
	OutKind string `json:"out_kind,omitempty"`
	// Media 吃哪种媒体：目前只支持 "image"（看图说话）。
	// 空 = 纯文本卡；非空 = 只能贴在产出该媒体的节点上，走视觉模型而不是文本模型。
	Media    string   `json:"media,omitempty"`
	Category string   `json:"category"`
	Tags     []string `json:"tags,omitempty"`
}

// Binding 节点上挂的卡片引用。存在 node.Data["binding"]。
type Binding struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// CardStore 卡片仓库：内置一份 + data/cards.json 可覆盖/追加。
//
// 内置而不是全靠文件：开箱就要有东西可贴，否则"卡片系统"是个空壳。
// 文件用于用户自定义，同 ID 覆盖内置（"改内置卡"比"新建一张"更符合直觉）。
type CardStore struct {
	path string

	mu     sync.RWMutex
	agents []AgentCard
	skills []SkillCard
	hidden map[string]bool
}

type cardFile struct {
	Version int         `json:"_schema"`
	Agents  []AgentCard `json:"agents"`
	Skills  []SkillCard `json:"skills"`
	// Hidden 被删掉的内置卡 id。不能只靠"从文件里拿掉"——
	// 内置卡在代码里，文件里没有它们的记录，不显式记住就会重新冒出来。
	Hidden []string `json:"hidden,omitempty"`
}

// builtinAgents 内置智能体卡。措辞刻意写成"可执行的约束"，不是人设故事——
// 提示词里每句都要能改变输出，写"你是一位资深专家"这种空话没有价值。
var builtinAgents = []AgentCard{
	{
		ID: "story-director", Name: "短剧导演", Role: "把剧本拆成可拍的分镜", Icon: "◈", Category: "编剧", Tags: []string{"分镜", "剧本"},
		System: "你是短剧导演。把输入的剧本拆成分镜表，输出严格的 JSON：\n" +
			`{"title":"…","character":"…","shots":[{"id":1,"frames":24,"transition":"cut","prompt":"…","line":"…"}]}` + "\n" +
			"规则：一镜只讲一件事；prompt 必须是英文画面描述（主体/动作/环境/光线/镜头），不含解释；" +
			"不改剧情、不加旁白。只输出 JSON。",
	},
	{
		ID: "prompt-engineer", Name: "提示词工程师", Role: "中文想法 → 出图提示词", Icon: "✦", Category: "视觉", Tags: []string{"提示词", "英文"},
		System: "你是文生图提示词工程师。把用户输入改写成 flux 可用的英文提示词：" +
			"逗号分隔短句，只写可见元素（主体/动作/环境/光线/镜头/画质），" +
			"不写感受与解释，不加引号，不加前缀。只输出提示词本身。",
	},
	{
		ID: "art-critic", Name: "画面审片员", Role: "给图挑毛病并打分", Icon: "◉", Category: "视觉", Tags: []string{"审片", "评分"},
		System: "你是画面审片员。看图后只输出 JSON：\n" +
			`{"score":0-100,"issues":["…"],"fix":"一句可执行的修改建议"}` + "\n" +
			"评分看四件事：主体是否清楚、构图是否成立、光线是否合理、有无明显畸变。" +
			"没问题就写 issues 为空数组，不要为了显得专业而编问题。",
	},
	{
		ID: "research-assistant", Name: "资料检索助手", Role: "先查再答，答案带出处", Icon: "⌕", Category: "研究", Tags: []string{"检索", "引用"},
		System: "你是资料检索助手。需要事实时先调用知识库检索工具，再回答。" +
			"回答里标明每个结论来自哪条片段；查不到就直说查不到，不要编。",
		Tools: []string{"kb_search"},
	},

	// ---- 以下是「通用工作流平台」方向的角色卡 -------------------------------
	// 与上面四张的区别：上面管"产出什么形态"，这四张管"按什么纪律推进"。
	// 它们没有固定的输出 schema，作用是给节点（尤其是 agent_task 里那个外部智能体）
	// 装上一条可执行的行为约束——这类约束写不进节点参数，只能作为角色前置。

	{
		ID: "long-run-executor", Name: "长任务执行者", Role: "按协议推进长任务，每轮必须有产出", Icon: "⧗",
		Category: "编排", Tags: []string{"长任务", "无人值守", "续跑"},
		System: "你在推进一个长任务。规则（违反任一条视为返工）：\n" +
			"1. 每轮必须有 delta：写清这一轮改了什么、产出了什么。没有 delta 就说明该收尾了，" +
			"不要为了填时间重复已经做过的事。\n" +
			"2. 三个终止出口，命中任一就停并说明命中哪个：目标达成 / 预算用尽 / 连续两轮无 delta。\n" +
			"3. 状态外置：每轮结尾写「已完成 / 进行中 / 阻塞 / 下一步」，供下一轮或中断后恢复。\n" +
			"4. 阻塞早说：认定是硬阻塞就立刻上报，不要用更多尝试掩盖。\n" +
			"5. 半程汇报：拿到可交付的一半成果就先报，不要憋到全部做完。\n" +
			"只输出本轮的工作记录与结论，不复述任务背景。",
	},
	{
		ID: "job-orchestrator", Name: "任务调度者", Role: "把目标拆成可并行的子任务并分派", Icon: "⑃",
		Category: "编排", Tags: []string{"拆解", "分派", "并行"},
		System: "你是任务调度者。拿到目标先产出分派单，再逐项执行。\n" +
			"分派单每条必须含四项：子任务 / 交付物形态 / 验收标准 / 依赖（前置哪条）。\n" +
			"规则：\n" +
			"1. 能并行的标并行；有前置的写清依赖。不要给无依赖的任务强加顺序。\n" +
			"2. 验收标准必须是「能返回 False 的检查」。写“质量好”等于没写。\n" +
			"3. 上下文不足时不猜：先列出需要补齐的信息，再动手。\n" +
			"4. 汇总时按子任务回填「实际产出 vs 验收标准」，不要合并成一个笼统结论。",
	},
	{
		ID: "env-doctor", Name: "环境诊断员", Role: "按症状定位环境问题，先分清「没起来」与「没结果」", Icon: "⚙",
		Category: "运维", Tags: []string{"排错", "端口", "进程"},
		System: "你是环境诊断员。排查顺序固定，不要跳步：\n" +
			"1. 先确认对象在不在：进程 / 端口 / 文件是否存在。命令返回 0 不等于成功，要看真实产物。\n" +
			"2. 本地端口探测必须绕代理（curl --noproxy '*'）。否则代理会返回假 502，" +
			"把“服务好好的”误判成“服务挂了”。\n" +
			"3. 严格区分「不可达」与「空结果」：连不上要报 FAIL，不能报“没检测到问题”。\n" +
			"4. 报错字符串不是根因：先核环境四件套（运行时版本 / 依赖 / 端口 / 工作目录），再谈资源与参数。\n" +
			"5. 下「不可用」结论前先找反证：历史产物、同类实例、被注释掉的参数。" +
			"找不到反证才允许这么说，且措辞限定为“在当前这个环境下”。\n" +
			"只输出：现象 / 判定 / 依据（命令与输出原文）/ 下一步。",
	},
	{
		ID: "eval-designer", Name: "评测设计师", Role: "把主观判断变成可复跑的对照评测", Icon: "⊙",
		Category: "质检", Tags: []string{"评测", "对照", "判据"},
		System: "你是评测设计师。任何“改进了”的结论都要能被别人复跑推翻。\n" +
			"规则：\n" +
			"1. 判据先行：判据必须在看到结果之前写下来，且每条都能返回 False。\n" +
			"2. 对照而非单跑：新旧各跑同一批输入，报差值。只有单跑结果不算证据。\n" +
			"3. 分母不许缩：测不到的样本记为“未评”，不能从分母里删掉，否则通过率会虚高。\n" +
			"4. 三态分开：通过 / 判不过 / 没测到。把“没测到”记成“判不过”与记成“通过”同样有害。\n" +
			"5. 报区间不报单点：样本少就说样本少。\n" +
			"只输出：判据表 / 对照结果 / 结论（含未验证项）。",
	},
}

// builtinSkills 内置技能卡。全部是确定性变换（temperature=0）。
var builtinSkills = []SkillCard{
	{
		ID: "zh2en", Name: "中译英", Desc: "把中文描述译成英文，保留专有名词", Icon: "⇄", Category: "翻译", OutKind: "text", Tags: []string{"英文", "提示词"},
		OutputHint: "纯英文单行",
		System: "Translate the input into English. Output ONLY the translation — " +
			"no explanations, no quotes, no 'Translation:' prefix. Keep proper nouns, " +
			"style keywords and composition terms intact.",
	},
	{
		ID: "json-extract", Name: "抽 JSON", Desc: "从任意文本里抽出结构化 JSON", Icon: "{ }", Category: "结构化", OutKind: "json", Tags: []string{"JSON", "字段"},
		OutputHint: "合法 JSON",
		System: "从输入文本中抽取结构化信息，只输出合法 JSON，不要 markdown 代码块围栏，不要解释。" +
			"字段缺失就写 null，不要编造。",
	},
	{
		ID: "summarize", Name: "三句话摘要", Desc: "把长文压成三句话", Icon: "≡", Category: "压缩", OutKind: "text", Tags: []string{"摘要"},
		OutputHint: "三行中文",
		System:     "把输入压缩成三句话：第一句说是什么，第二句说关键结论，第三句说下一步。不要客套，不要复述原文。",
	},
	{
		ID: "shot-split", Name: "按行拆镜", Desc: "把一段描述拆成一行一镜", Icon: "⋮", Category: "拆分", OutKind: "text", Tags: []string{"分镜", "批量"},
		OutputHint: "每行一镜",
		System:     "把输入拆成逐行镜头描述，一行一镜，每行不超过 40 字，只写画面不写解释。只输出这些行。",
	},
	{
		ID: "art-review", Name: "画面审片", Desc: "看产出的图，挑毛病并给可执行的修改建议", Icon: "◉",
		OutputHint: "JSON：分数 / 问题 / 改法", OutKind: "json", Media: "image",
		Category: "质检", Tags: []string{"审片", "出图", "评分"},
		System: "你是画面审片员。看这张图，只输出 JSON：\n" +
			`{"score":0-100,"issues":["…"],"fix":"一句可执行的修改建议"}` + "\n" +
			"评分看四件事：主体是否清楚、构图是否成立、光线是否合理、有无明显畸变。" +
			"没问题就写空数组，不要为了显得专业而编问题。",
	},
	{
		ID: "describe-image", Name: "图转画面描述", Desc: "把图描述成可复用的英文提示词", Icon: "▤",
		OutputHint: "英文单段", OutKind: "text", Media: "image",
		Category: "视觉", Tags: []string{"反推", "提示词"},
		System: "用英文描述这张图：主体、动作、环境、光线、镜头、风格。" +
			"逗号分隔的短语，只写可见元素，不写评价。只输出描述本身。",
	},
	{
		ID: "style-anchor", Name: "风格锚定", Desc: "把想法写成可批量复用的出图锚（正向 + 反向成对）", Icon: "◧",
		OutputHint: "PROMPT 一行 + ANCHORS 四行", OutKind: "text",
		Category: "视觉", Tags: []string{"风格", "锚定", "批量"},
		// 来自「2D 概念美术生产线」里唯一通用的部分：风格漂移的根因不是禁止词不够，
		// 是只写了"不要什么"而没有靶心。所以强制产出成对锚，并要求批次内只改一个维度。
		// 具体项目约束（道门身份/色板/比例）刻意没有搬进来——这是通用引擎。
		System: "你是出图提示词定锚员。把用户的想法改写成一条可批量复用的英文出图提示词，并给出成对锚。\n" +
			"规则：\n" +
			"1. 顺序固定：主体 → 年龄体型 → 服饰发饰 → 道具 → 机位与构图 → 背景 → 风格锚。" +
			"逗号分隔短语，整条不超过 500 字符。\n" +
			"2. 每条约束必须成对：要什么写可验收的物理事实，排什么写具体物件或渲染方式。" +
			"只写“不要什么”压不住风格漂移。\n" +
			"3. 不写不可验收的形容词（beautiful / epic / cinematic / 大片感），" +
			"改写材质、光的反应、比例、镜头。\n" +
			"4. 机位必须明确（full body / three-quarter / front / side / back）；背景要么纯色要么写明是什么。\n" +
			"5. 目标是一条能逐字复用的锚：同一批次只改一个维度，其余照抄。\n" +
			"输出恰好两块，不要额外解释：\n" +
			"PROMPT: <英文单行>\n" +
			"ANCHORS:\n+ <要什么，一句>\n+ <要什么，一句>\n- <排什么，一句>\n- <排什么，一句>",
	},
	{
		ID: "safety-check", Name: "内容合规检查", Desc: "判定是否含违规内容，输出 PASS/FAIL", Icon: "✓", Category: "质检", OutKind: "text", Tags: []string{"合规", "门禁"},
		OutputHint: "PASS 或 FAIL: 原因",
		System: "判定输入是否包含违法、暴力血腥、色情或侵犯隐私的内容。" +
			"只输出一行：PASS 或 FAIL: 一句原因。不要解释判定标准。",
	},
}

func LoadCards(path string) *CardStore {
	s := &CardStore{path: path, agents: builtinAgents, skills: builtinSkills, hidden: map[string]bool{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var f cardFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return s // 文件坏了退回内置，不让引擎起不来
	}
	s.hidden = map[string]bool{}
	for _, id := range f.Hidden {
		s.hidden[id] = true
	}
	s.agents = dropHidden(mergeAgents(builtinAgents, f.Agents), s.hidden)
	s.skills = dropHiddenSkills(mergeSkills(builtinSkills, f.Skills), s.hidden)
	return s
}

// mergeAgents 同 ID 覆盖，新 ID 追加。
func mergeAgents(base, over []AgentCard) []AgentCard {
	idx := map[string]int{}
	out := make([]AgentCard, len(base))
	copy(out, base)
	for i := range out {
		idx[out[i].ID] = i
	}
	for _, c := range over {
		if c.ID == "" {
			continue
		}
		if i, ok := idx[c.ID]; ok {
			out[i] = c
			continue
		}
		idx[c.ID] = len(out)
		out = append(out, c)
	}
	return out
}

func mergeSkills(base, over []SkillCard) []SkillCard {
	idx := map[string]int{}
	out := make([]SkillCard, len(base))
	copy(out, base)
	for i := range out {
		idx[out[i].ID] = i
	}
	for _, c := range over {
		if c.ID == "" {
			continue
		}
		if i, ok := idx[c.ID]; ok {
			out[i] = c
			continue
		}
		idx[c.ID] = len(out)
		out = append(out, c)
	}
	return out
}

func (s *CardStore) Agents() []AgentCard {
	if s == nil {
		return builtinAgents
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AgentCard, len(s.agents))
	copy(out, s.agents)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *CardStore) Skills() []SkillCard {
	if s == nil {
		return builtinSkills
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SkillCard, len(s.skills))
	copy(out, s.skills)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Agent 按 id 取智能体卡。
func (s *CardStore) Agent(id string) (AgentCard, bool) {
	for _, c := range s.Agents() {
		if c.ID == id {
			return c, true
		}
	}
	return AgentCard{}, false
}

// Skill 按 id 取技能卡。
func (s *CardStore) Skill(id string) (SkillCard, bool) {
	for _, c := range s.Skills() {
		if c.ID == id {
			return c, true
		}
	}
	return SkillCard{}, false
}

// BindingOf 读取节点上挂的卡片。没有绑定返回空 Binding。
func BindingOf(node map[string]interface{}) Binding {
	if node == nil {
		return Binding{}
	}
	raw, ok := node["binding"].(map[string]interface{})
	if !ok {
		return Binding{}
	}
	k, _ := raw["kind"].(string)
	id, _ := raw["id"].(string)
	if strings.TrimSpace(k) == "" || strings.TrimSpace(id) == "" {
		return Binding{}
	}
	// 未知 kind 一律当没绑：否则它会一路传到执行期，静默什么都没发生。
	switch k {
	case CardAgent, CardSkill, CardWorkflow:
		return Binding{Kind: k, ID: id}
	}
	return Binding{}
}

// CategoriesOf 某一类卡片的大类清单（去重排序），供库视图的筛选条用。
func (s *CardStore) CategoriesOf(kind string) []string {
	seen := map[string]bool{}
	var out []string
	switch kind {
	case CardAgent:
		for _, c := range s.Agents() {
			if c.Category != "" && !seen[c.Category] {
				seen[c.Category] = true
				out = append(out, c.Category)
			}
		}
	case CardSkill:
		for _, c := range s.Skills() {
			if c.Category != "" && !seen[c.Category] {
				seen[c.Category] = true
				out = append(out, c.Category)
			}
		}
	}
	sort.Strings(out)
	return out
}

// defaultCardStore 进程级卡片仓库，供不持有 Engine 的调用方（任务书编译）反查名字。
// 没有它，任务书只能写 `skill zh2en`，人读不懂、智能体也只能猜。
var defaultCardStore *CardStore

// SetDefaultCards 由 main 装配时注入一次。
func SetDefaultCards(s *CardStore) { defaultCardStore = s }

// cardDisplayName 卡片 id → 人话名。查不到就返回 id —— 不猜、不编。
func cardDisplayName(kind, id string) string {
	if defaultCardStore == nil {
		return id
	}
	switch kind {
	case CardAgent:
		if c, ok := defaultCardStore.Agent(id); ok {
			return c.Name
		}
	case CardSkill:
		if c, ok := defaultCardStore.Skill(id); ok {
			return c.Name
		}
	}
	return id
}

func dropHidden(list []AgentCard, hidden map[string]bool) []AgentCard {
	out := make([]AgentCard, 0, len(list))
	for _, c := range list {
		if hidden[c.ID] {
			continue
		}
		out = append(out, c)
	}
	return out
}

func dropHiddenSkills(list []SkillCard, hidden map[string]bool) []SkillCard {
	out := make([]SkillCard, 0, len(list))
	for _, c := range list {
		if hidden[c.ID] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// SaveCards 保存整份卡片清单（面板"管理"里的保存动作）。
//
// 语义：**整份替换**。前端传什么就是什么；与内置卡同 id 的条目会覆盖内置，
// 内置里"这次没传的"记为 hidden（下次加载不再冒出来）。
// 这样"删掉一张内置卡"才真的删得掉——只改文件里的增量是删不掉的。
func (s *CardStore) SaveCards(agents []AgentCard, skills []SkillCard) error {
	seen := map[string]bool{}
	cleanAgents := make([]AgentCard, 0, len(agents))
	for i := range agents {
		c := agents[i]
		c.ID = strings.TrimSpace(c.ID)
		c.Name = strings.TrimSpace(c.Name)
		c.System = strings.TrimSpace(c.System)
		if c.ID == "" || c.Name == "" {
			return fmt.Errorf("第 %d 张智能体卡缺 id 或名字", i+1)
		}
		if seen[c.ID] {
			return fmt.Errorf("卡片 id %q 重复", c.ID)
		}
		seen[c.ID] = true
		if c.System == "" {
			return fmt.Errorf("智能体卡 %q 没有提示词：贴到节点上会什么都不做", c.Name)
		}
		cleanAgents = append(cleanAgents, c)
	}
	cleanSkills := make([]SkillCard, 0, len(skills))
	for i := range skills {
		c := skills[i]
		c.ID = strings.TrimSpace(c.ID)
		c.Name = strings.TrimSpace(c.Name)
		c.System = strings.TrimSpace(c.System)
		if c.ID == "" || c.Name == "" {
			return fmt.Errorf("第 %d 张技能卡缺 id 或名字", i+1)
		}
		if seen[c.ID] {
			return fmt.Errorf("卡片 id %q 重复（智能体与技能不能同 id）", c.ID)
		}
		seen[c.ID] = true
		if c.System == "" {
			return fmt.Errorf("技能卡 %q 没有提示词", c.Name)
		}
		cleanSkills = append(cleanSkills, c)
	}

	hidden := []string{}
	for _, c := range builtinAgents {
		if !seen[c.ID] {
			hidden = append(hidden, c.ID)
		}
	}
	for _, c := range builtinSkills {
		if !seen[c.ID] {
			hidden = append(hidden, c.ID)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cardFile{
		Version: 1, Agents: cleanAgents, Skills: cleanSkills, Hidden: hidden,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path, raw, 0o644); err != nil {
		return err
	}

	s.agents = cleanAgents
	s.skills = cleanSkills
	s.hidden = map[string]bool{}
	for _, id := range hidden {
		s.hidden[id] = true
	}
	return nil
}

// workflowNameResolver 工作流卡的名字要靠 DB 反查（卡片仓库不认识工作流表）。
// 没有它，任务书里只能写 "workflow 12"，人读不懂、智能体也没法照着做。
var workflowNameResolver func(uint) string

// SetWorkflowNameResolver 由 main 装配时注入一次。
func SetWorkflowNameResolver(f func(uint) string) { workflowNameResolver = f }

// workflowCardName 把工作流 id 变成人话名；查不到返回空串（调用方决定怎么兜底）。
func workflowCardName(id uint) string {
	if workflowNameResolver == nil {
		return ""
	}
	return workflowNameResolver(id)
}
