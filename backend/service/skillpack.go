package service

// 技能包索引（Skill Pack Index）
//
// 为什么需要这一层
// ----------------
// FlowEdge 的 skill 卡是「一次确定性文本变换」：一句 system prompt 进去，文本出来，
// temperature 固定 0。而用户机器上那批 WorkBuddy skill 是「SKILL.md + references/ + scripts/」
// 的工程方法论包，最大的一个有 188KB、8 篇参考文档。
//
// 两者形态不兼容。把一篇排错手册压成一句 system prompt，规则全丢——那不是收录，
// 是造一张"看起来收录了"的假卡。所以这里做的是**索引 + 归属判定 + 证据**，不是复制内容。
//
// 判定口径（五种，互斥）
// --------------------
//	mirrored     引擎已用代码实现。技能包是人读镜像，再复制一份提示词就是第二真相源，
//	             必然漂移（真踩过：文档写"LoRA 8 步"，代码是 12 步）。索引只指实现位置。
//	adopted      整个包的内容被提炼成技能卡，能贴到节点上。必须能指到一个真实存在的卡 id。
//	partial      整包不收录，但其中有通用规律被提炼成卡。
//	             与 adopted 的区别是"全部"还是"一部分"——混成一个值会让"收了几张卡"算不清。
//	project      只在某个具体产品里成立（UE 编辑器 / Kaggle / EdgeOne / 磁盘清理），平台不要。
//	             这类默认 Retain=false，从平台视野删掉。
//	harness      预留：agent 侧方法论但尚未提炼成卡。当前判据下应为空——
//	             能提炼的都该走 adopted/partial 落到卡上，落不到卡上的就 Retain=false。
//	deprecated   已停用（目录里没有 SKILL.md）。
//	unclassified 没判定过 —— **默认不收**。宁可漏，不许默默混进通用产物。
//
// 判定表是内置的，`data/skill-policy.json` 可以覆盖。覆盖同样要过校验：
// 说 adopted 指不到卡、说 mirrored 指不到代码行，一律按未判定处理并在 Reason 里写明。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// 技能包归属判定值。
const (
	SkillMirrored     = "mirrored"
	SkillAdopted      = "adopted"
	SkillPartial      = "partial"
	SkillHarness      = "harness"
	SkillProject      = "project"
	SkillDeprecated   = "deprecated"
	SkillUnclassified = "unclassified"
)

// 呈现层级（Tier）—— 与 Verdict 是两个正交的维度，别混：
//
//	Verdict 回答"它变成了什么"（卡 / 引擎实现 / 没变）
//	Tier    回答"默认露不露"（通用能力 / 领域能力 / 不收录）
//
// 为什么必须分两个维度（2026-10-23 张越纠正）：
// 把 UE5 的 skill 从平台里删掉是错的——平台是**通用编排器**，用户完全可能拿它编一张
// 「UE5 资产批处理」的图，那时他需要 ue5-ops 的 7 篇排错参考。
// 「不绑私活」约束的是**默认预设**（首页示例、默认推荐、空画布引导），
// 不是"平台不许承载某个领域的知识"。两件事混在一起，就会把用户要用的东西也删掉。
const (
	// TierCore 通用能力：任何图都可能用到，默认出现在库与推荐里
	TierCore = "core"
	// TierDomain 领域能力：收录可查、可被节点引用，但不预设、不进默认推荐。
	// 用户主动搜或显式引用时才出现——这正是「通用平台承载专业领域」的正确姿势。
	TierDomain = "domain"
	// TierNone 不收录（目前只有已停用的包）
	TierNone = "none"
)

// SkillPack 一个技能包在引擎侧的索引条目。
type SkillPack struct {
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Description string   `json:"description,omitempty"`
	ReadWhen    []string `json:"read_when,omitempty"`
	Bytes       int64    `json:"bytes"`
	Files       int      `json:"files"`
	// Refs 参考文档文件名（references/ 下的 .md）
	Refs []string `json:"refs,omitempty"`
	// Source 来源根的人话标签（用户级 / 冷存储 / DSH 侧 / agents / 项目级·XX / 插件名）。
	// 2026-10-23 张越：「我 GitHub 之前安装了很多啊」——根因是只扫了 ~/.workbuddy/skills
	// 一个根。全盘其实有五处本机技能生态（见 SkillRoots）+ 专家插件市场。
	// 不标来源，用户看到「扫到 14 个」时无法判断是漏扫还是本来就没有。
	Source string `json:"source,omitempty"`
	// Verdict 归属判定，见文件头口径
	Verdict string `json:"verdict"`
	// Reason 一句人话解释为什么这么判。空 Reason 视为判定无效。
	Reason string `json:"reason"`
	// Evidence 证据："相对项目根的路径:行号#锚点"。mirrored 必须能指到实现；
	// adopted 必须能指到卡定义。端点会核验，核验不过降级为 unclassified。
	Evidence []string `json:"evidence,omitempty"`
	// Card adopted 时对应的卡 id（可以是 agent 卡，也可以是 skill 卡）
	Card string `json:"card,omitempty"`
	// Tier 呈现层级：core / domain / none
	Tier string `json:"tier"`
	// Retain 保留在平台里（= Tier != none）。前端旧字段兼容，判断请用 Tier
	Retain bool `json:"retain"`
}

// TierLabel 层级的人话名。
func TierLabel(t string) string {
	switch t {
	case TierCore:
		return "通用能力"
	case TierDomain:
		return "领域能力"
	default:
		return "不收录"
	}
}

// VerdictCategory 把判定翻译成面板分组名。
func VerdictCategory(v string) string {
	switch v {
	case SkillMirrored:
		return "引擎内实现"
	case SkillAdopted:
		return "整包收录"
	case SkillPartial:
		return "部分提炼成卡"
	case SkillHarness:
		return "智能体方法论"
	case SkillProject:
		return "具体业务"
	case SkillDeprecated:
		return "已停用"
	default:
		return "未判定"
	}
}

// skillVerdict 判定表的一行。
type skillVerdict struct {
	Verdict  string
	Reason   string
	Evidence []string
	Card     string
	// Tier 呈现层级（core / domain / none）。空值按 domain 处理——
	// 没标过的包不该因为漏填就跑到默认推荐里去。
	Tier string
	// Retain 保留在平台里。与 Tier 正交：Tier 管"露不露"，Retain 管"收不收"。
	Retain bool
}

// skillVerdicts 内置判定表。
//
// 写这张表的依据不是"我觉得"，是逐个 grep 过实现位置。证据里的行号由测试核验，
// 改了被指向的文件而忘了改这里，测试会红——这是让表不会悄悄过期的手段。
//
// 2026-10-23 二次重判。上一版把「只在某个具体产品里成立」的包直接从平台视野删掉，
// 那是**过度矫正**：平台是通用编排器，用户完全可能编一张「UE5 资产批处理」的图，
// 那时 ue5-ops 的七篇排错参考正是他要的。不绑私活约束的是**默认预设**
// （首页示例 / 默认推荐 / 空画布引导），不是"平台不许承载某个领域的知识"。
// 所以拆成两级：Verdict 说"它变成了什么"，Tier 说"默认露不露"。
var skillVerdicts = map[string]skillVerdict{
	// ================= 通用能力：任何图都可能用到，默认可见 =================

	"agent-orchestration-dispatch": {
		Verdict: SkillAdopted, Tier: TierCore, Card: "job-orchestrator",
		Reason: "平台的核心动作就是编排：把目标拆成子任务、分派、按可判定的验收标准回收。" +
			"全篇提炼成 job-orchestrator 角色卡（分派单四要素 + 验收必须能返回 False + 不猜上下文）。",
	},
	"long-run-goal-mode": {
		Verdict: SkillAdopted, Tier: TierCore, Card: "long-run-executor",
		Reason: "长任务是通用平台的主要负载（训练、批量生成、多轮迭代）。" +
			"终止三出口 / 每轮必须有 delta / 状态外置 / 半程汇报 四条提炼成 long-run-executor 卡，" +
			"贴到 llm 或 agent_task 节点即生效。",
	},
	"local-multimodal-ops": {
		Verdict: SkillPartial, Tier: TierCore, Card: "env-doctor",
		Reason: "任何机器上都要面对「服务没起 / 显存被占 / 静默降级」。可迁移的判断纪律" +
			"（先找反证再下不可用结论、区分不可达与空结果、报错不是根因）已提炼成 env-doctor 卡；" +
			"ComfyUI 专属路径与节点细节留在包内，需要时按包名引用。",
	},
	"windows-dev-env": {
		Verdict: SkillPartial, Tier: TierCore, Card: "env-doctor",
		Reason: "代理劫持本地端口、venv 装不上 CUDA 版、后台进程被回收——这些在任何 Windows 机器上" +
			"都会复现，且直接决定平台能不能跑起来。核心三条已并入 env-doctor 卡。",
	},
	"xuce-evolution-paired-evaluation": {
		Verdict: SkillPartial, Tier: TierCore, Card: "eval-designer",
		Reason: "平台有 evidence 与门，缺的是「怎么证明改动真的更好」。配对对照、判据先行、" +
			"分母不许缩、三态分开四条已提炼成 eval-designer 卡；序策 registry 状态机细节留在包内。",
	},
	"flowedge-shot-consistency": {
		Verdict: SkillMirrored,
		Reason: "多段出片的场景一致性方法论。两层——文本侧九宫格锁（scene_grid.go）与像素侧" +
			"一致性门（consistency.go）——引擎都已用代码实现；技能包是给人读的口径与排错手册，" +
			"再复制一份提示词就是第二真相源，必然漂移。判定镜像到实现，不复制内容。",
		Evidence: []string{
			"backend/service/consistency.go:762#execConsistencyGateNode", // 一致性门执行体
			"backend/service/consistency.go:511#csCompare",               // 三条判据
			"backend/service/consistency.go:280#csSignature",             // 帧签名（块色相/直方图）
			"backend/service/scene_grid.go:102#CompileSceneLock",         // 文本侧九宫格锁
		},
		Tier: TierCore,
	},
	"h3-prompt-optimizer": {
		Verdict: SkillMirrored, Tier: TierCore,
		Reason: "三段式提示词、ref2va 六段式、九宫格锁、中置相消四件事引擎都已用代码实现，" +
			"技能包自己写明「与 service/prompt_opt.go 同一份」。保留索引是为了查证，不复制内容。",
		Evidence: []string{
			"backend/service/prompt_opt.go:29#h3PromptInstruction", // 三段式系统指令
			"backend/service/prompt_opt.go:141#ensureRef2VAPrompt", // 六段式补齐
			"backend/service/prompt_opt.go:442#ensureDialogueFree", // 正面口型描述
			"backend/service/scene_grid.go:102#CompileSceneLock",   // 九宫格编译
			"backend/service/shotlist.go:414#中置相消",                 // 中置相消兜底
			"backend/service/filmgates.go:411#CheckFilmGates",      // 五层门运行期 enforcement
			"scripts/film-gates.py:1#python3",                      // 五层门
		},
	},

	// ================= 领域能力：收录可查可引用，但不预设 =================
	// 用户在库里搜得到、能给节点填 skill_refs 让外部智能体自己去读；
	// 但首页示例、默认推荐、空画布引导里不出现——那才是「不绑私活」的边界。

	"ue5-ops": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "编「UE5 工程诊断」类图时会用到：MCP 会话、崩溃/编译失败、LiveCoding 锁、场景性能审计。" +
			"平台本身没有 UE 对象，所以不预设；但图里跑 UE 命令行或 Python 脚本节点时，" +
			"这七篇排错参考就是省时间的东西。",
	},
	"ue-asset-governance": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "编「资产批处理」类图时会用到：命名规范、台账、重复检测、安全改名迁移（引用预检 + 重定向器清理）、" +
			"台账漂移差分。资产表属于具体工程，平台不预设。",
	},
	"ue-asset-pipeline": {
		Verdict: SkillPartial, Tier: TierDomain, Card: "style-anchor",
		Reason: "概念图→3D→绑骨→入库六道门。其中唯一与项目无关的规律「成对锚 + 单变量」已提炼成 " +
			"style-anchor 卡（通用，默认可见）；3D/绑骨/UE 导入的具体口径留在包内，做资产图时按包引用。",
	},
	"kaggle-ml-ops": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "编「训练 / 批量评测」类图时会用到：SFT/DPO/GGUF 的十七个自毁陷阱、kernel 提交三连坑、" +
			"loss 漂亮但效果没好的假成功判定。训练平台本身是外部工具，走 agent_task 调它，不预设。",
	},
	"edgeone-makers-fullstack": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "编「构建→部署」类图时会用到：零构建全栈部署、边缘函数、签名 URL 401/302 四个坑。" +
			"部署目标是外部平台，不预设。",
	},
	"windows-disk-audit": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "编「磁盘清理 / 空间审计」类图时会用到：只读侦察、空间黑洞定位、分级清理。" +
			"**不预设的理由是安全**：删除是不可逆动作，平台不该把它做成默认能力；" +
			"要跑必须显式引用，且该包引用的 disk-hygiene.ps1 本机并不存在，按手动配方用。",
	},
	"windows-gui-automation-ctypes": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "编「代操桌面程序」类图时会用到：枚举窗口、点击、截图、置前，以及 DPI 缩放导致的点击偏移。" +
			"平台自身没有 GUI 目标，但 agent_task 调外部智能体去操作桌面时用得上。",
	},
	"xuce-resource-governor": {
		Verdict: SkillMirrored, Tier: TierDomain,
		Reason: "硬件资源治理手册。引擎侧已有等价实现（governor_gate.go 走 preflight 准入闸，fail-open），" +
			"包内是人的操作说明（热调阈值、违规台账、急停）。引擎实现为准，索引供查证。",
		Evidence: []string{"backend/service/governor_gate.go:1#package service"},
	},

	// ================= 不收录 =================

	"xuce-task-notification-workbuddy": {
		Verdict: SkillDeprecated, Tier: TierNone,
		Reason: "已停用：目录里刻意没有 SKILL.md。留记录只为让审计不漏项。",
	},

	// ================= 2026-10-23 扩根后新增登记的包 =================
	// 背景：上一版只扫 ~/.workbuddy/skills，用户「GitHub 之前安装了很多」的技能其实分布在
	// 冷存储 / DSH 侧 / agents / 别的工作区项目级 / 专家插件市场五处。
	// 收录口径不变：Verdict 说「变成了什么」，Tier 说「默认露不露」。
	// 这批绝大多数是 project + domain —— 平台不预设，但编对应领域的图时查得到、能引用。

	// ---- 冷存储（~/.workbuddy/skills-cold，manifest.json 记有移出理由）----

	"cn-form-autofill-engine": {
		Verdict: SkillProject, Tier: TierNone,
		Reason: "校招网申表单自动填写。场景已转序策 01-职业战略线（私人业务线），平台无对应用例；" +
			"冷存储 manifest 记明近 5 日零引用。留索引记录，不让审计漏项。",
	},
	"howtolivebetter-life-guide": {
		Verdict: SkillProject, Tier: TierNone,
		Reason: "生活方式指南，与工作流编排无关，且包自身标了 disable-model-invocation（不参与自动选型）。" +
			"平台是干活的操作台，不承载生活内容。",
	},
	"unity-bepinex-mod-repair": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "Unity BepInEx 模组修复（反编译、打补丁、重打包、版本对齐）。编「Unity 模组修复」类图时" +
			"会用到；平台没有 Unity 对象，不预设，按包名引用。",
	},
	"xuce-agnes-bridge": {
		Verdict: SkillDeprecated, Tier: TierNone,
		Reason: "Agnes 委派链已于 2026-10-17 随客户端退役，全局规则明令禁调用（并行执行改走 " +
			"agent-orchestration-dispatch 的多智能体派发）。登记为已停用，防止误引用。",
	},
	"tars": {
		Verdict: SkillDeprecated, Tier: TierNone,
		Reason: "TARS GUI 自动化已停用——桌面操作 2026-10-20 起改走 windows-mcp 连接器网关" +
			"（先 Snapshot 再动作）。冷池里连 SKILL.md 都已清空，仅存目录名。",
	},

	// ---- DSH 侧独有（~/.dsh/skills，与用户级不同步的那几个）----

	"dsh-xuce-brain": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "DSH 侧经 xuce-brain MCP 检索序策智脑（brain_route 资产检索 / brain_rules 铁律核对 / " +
			"brain_remember 记忆提案）。编「跨端记忆 / 知识检索」类图时可用；序策生态绑定，不预设。",
	},
	"ue5-package-pipeline": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "UE5 打包链路排错（cooker 参数、Launch 变体、打包后验证）。与 ue5-ops 同域互补：" +
			"ops 管编辑器内排错，这个管出包。编 UE 工程图时按包引用。",
	},
	"workspace-distill-and-purge": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "工作区蒸馏与清理方法论（什么该沉淀、什么该删、token 优化怎么做的）。" +
			"编「知识库治理 / 上下文瘦身」类图时会用到；清理动作不可逆，平台不预设。",
	},
	"xuce-flow-engine-change": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "序策 flow 引擎变更纪律（改前先读契约、双轨验证、禁盲试）。序策生态私有规范；" +
			"平台自身的同类纪律在 AGENTS.md 与 evidence 体系里，不复制这份。",
	},
	"xuce-graph-first-engineering": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "图优先工程方法论。与平台「万物皆可模块」理念同源，但属序策私有表述——" +
			"复制进卡就是第二真相源，只索引；做工程规划图时可查。",
	},

	// ---- agents 约定目录（~/.agents/skills）----

	"reverse-engineer-anything": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "任意二进制/文件的逆向工程方法（字符串探查、结构推断、行为观测、最小复现）。" +
			"编「逆向 / 协议分析 / 遗留系统摸排」类图时会用到；工具链在包内，按包名引用。",
	},

	// ---- 别的工作区的项目级技能（data/skill-roots.json 登记）----

	"xuce-flow-invoke": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "序策 flow 调用编排（flow_run / flow_decide / flow_status）。序策生态私有 MCP 面；" +
			"平台不预设，做跨端编排图时可按包名引用。",
	},
	"xuce-plan-graph": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "规划图化：把工程规划编译成序策引擎的图（对应全局规则 M）。序策生态私有；" +
			"平台自身的规划产物是 workflow JSON，不复制这套表述。",
	},
	"xuce-skill": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "简历生成工具链（profiles/jobs/scripts，私人职业线业务）。按禁擅绑业务铁律不预设；" +
			"收录只为索引完整——用户明确要求全盘可查。",
	},
	"daomen-concept-to-3d": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "道门圣境（AIStarter/UE 5.8）概念图→3A 3D 资产全链路（混元 3.1 网页生成 + hy3d 净化绑骨 + " +
			"UE 导入门）。与 ue-asset-pipeline 同域；道门项目绑定，不预设。",
	},
	"lemo-opuscar": {
		Verdict: SkillProject, Tier: TierDomain,
		Reason: "纯代码拍短片技能（Canvas/WebGL 逐帧渲染 + 采样库配乐 + TTS 旁白 + ffmpeg 合成，59 种风格）。" +
			"引擎自身的出片走 ComfyUI/H3 扩散管线，不复制这套渲染实现；编「程序化生成短片/动画」图时" +
			"按包名引用——它给的正是「不抽卡、可逐帧改」的那条路。",
	},
}

// skillPolicyFile data/skill-policy.json：用户可以改写判定。
type skillPolicyFile struct {
	Overrides map[string]skillVerdict `json:"overrides"`
}

// LoadSkillPolicy 读判定覆盖文件。文件不存在不算错（内置表就是默认值）。
func LoadSkillPolicy(path string) map[string]skillVerdict {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f skillPolicyFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return f.Overrides
}

// SkillRoots 取要扫描的技能根目录。
//
// 2026-10-23 扩根。上一版只扫 ~/.workbuddy/skills，用户反问「我 GitHub 之前安装了
// 很多啊，不可能只有这么多」。全盘摸排后本机技能其实分布在五处：
//
//	~/.workbuddy/skills          用户级活跃（WorkBuddy 安装/沉淀的主库）
//	~/.workbuddy/skills-cold     冷存储（刻意移出注入清单，但包还在，manifest.json 有移出理由）
//	~/.dsh/skills                 DSH（DeepSeek Harness）侧技能库，与用户级有同步也有独有
//	~/.agents/skills              opencode/agents 约定目录
//	<工作区>/.workbuddy/skills    项目级（baseDir 往上一级）+ data/skill-roots.json 登记的额外根
//
// 为什么默认全扫而不是只扫主库：「收录」回答「平台能不能找到」，冷存储/DSH 回答的是
// 「WorkBuddy 这一轮要不要自动注入」——两件事。用户完全可能编一张图正需要某个冷包。
// 同名包先到先得（用户级在前），不会因为扩根冒出重复项。
//
// 优先级：FLOWEDGE_SKILL_ROOTS（分号分隔）> 默认根 + 额外根文件。
func SkillRoots(baseDir string) []string {
	if raw := strings.TrimSpace(os.Getenv("FLOWEDGE_SKILL_ROOTS")); raw != "" {
		var out []string
		for _, p := range strings.Split(raw, ";") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".workbuddy", "skills"),
			filepath.Join(home, ".workbuddy", "skills-cold"),
			filepath.Join(home, ".dsh", "skills"),
			filepath.Join(home, ".agents", "skills"),
		)
	}
	// 项目级技能：baseDir 是 运行/，上一级才是仓库根
	if baseDir != "" {
		out = append(out, filepath.Join(filepath.Dir(baseDir), ".workbuddy", "skills"))
		out = append(out, extraSkillRoots(baseDir)...)
	}
	return out
}

// extraSkillRoots 读 baseDir/data/skill-roots.json 登记的额外技能根。
//
// 为什么用文件而不是硬编码：序策/AIStarter 这类「别的工作区的项目级技能」是本机部署
// 事实，不是平台通用逻辑。写进 data/ 让每台机器登记自己的，二进制保持中性可分发性。
// 文件不存在 = 没有额外根，不算错。
func extraSkillRoots(baseDir string) []string {
	if baseDir == "" {
		return nil
	}
	path := filepath.Join(baseDir, "data", "skill-roots.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		Roots []string `json:"roots"`
	}
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	var out []string
	for _, r := range f.Roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if strings.HasPrefix(r, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				r = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(r, "~"), "/\\"))
			}
		}
		out = append(out, filepath.Clean(r))
	}
	return out
}

// PluginSkillRoots 专家插件市场的技能根。
//
// 结构是两级：<root>/plugins/<插件>/skills/<技能>/SKILL.md（.codebuddy-plugin 清单声明）。
// 与技能根分开返回：布局不同、来源语义不同（外部专家生态，不是本机沉淀的方法论包），
// 合并成一种会让「扫到几个包」这个数字失去意义。
func PluginSkillRoots(baseDir string) []string {
	if raw := strings.TrimSpace(os.Getenv("FLOWEDGE_PLUGIN_ROOTS")); raw != "" {
		var out []string
		for _, p := range strings.Split(raw, ";") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	if home, err := os.UserHomeDir(); err == nil {
		return []string{filepath.Join(home, ".workbuddy", "plugins", "marketplaces", "experts")}
	}
	return nil
}

// sourceLabel 根目录 → 人话来源。用户看到「扫到 N 个」时必须能回答「从哪扫的」。
func sourceLabel(root string) string {
	clean := filepath.Clean(root)
	switch {
	case strings.HasSuffix(clean, filepath.Join(".workbuddy", "skills-cold")):
		return "冷存储"
	case strings.HasSuffix(clean, filepath.Join(".dsh", "skills")):
		return "DSH 侧"
	case strings.HasSuffix(clean, filepath.Join(".agents", "skills")):
		return "agents"
	case strings.HasSuffix(clean, filepath.Join(".workbuddy", "skills")):
		if home, err := os.UserHomeDir(); err == nil &&
			filepath.Dir(clean) == filepath.Join(home, ".workbuddy") {
			return "用户级"
		}
		ws := filepath.Base(filepath.Dir(filepath.Dir(clean)))
		if ws == "" || ws == "." || ws == string(filepath.Separator) {
			return "项目级"
		}
		return "项目级·" + ws
	default:
		return "额外·" + filepath.Base(clean)
	}
}

// ScanSkillPacks 扫描技能根目录。
//
// 不递归：技能包的约定是「一个目录一个包」，包内 references/ 由 pack 自己再列。
// 同名包先到先得——用户级在前，项目级同名不覆盖，避免一个过期副本顶掉主副本。
func ScanSkillPacks(roots []string, policy map[string]skillVerdict) []SkillPack {
	seen := map[string]bool{}
	var out []SkillPack
	for _, root := range roots {
		label := sourceLabel(root)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
				continue
			}
			name := e.Name()
			if seen[name] {
				continue
			}
			dir := filepath.Join(root, name)
			skill := filepath.Join(dir, "SKILL.md")
			raw, err := os.ReadFile(skill)
			if err != nil {
				// 没有 SKILL.md：只有 _DEPRECATED.md 时按"已停用"登记，其余忽略
				if _, derr := os.Stat(filepath.Join(dir, "_DEPRECATED.md")); derr == nil {
					seen[name] = true
					out = append(out, applyVerdict(SkillPack{
						Name: name, Path: dir, Source: label, Bytes: dirSize(dir), Files: countFiles(dir),
					}, policy))
				}
				continue
			}
			seen[name] = true
			meta := parseSkillMeta(raw)
			p := SkillPack{
				Name:        name,
				Path:        dir,
				Source:      label,
				Description: meta.description,
				ReadWhen:    meta.readWhen,
				Bytes:       dirSize(dir),
				Files:       countFiles(dir),
				Refs:        listRefs(filepath.Join(dir, "references")),
			}
			if p.Description == "" {
				p.Description = meta.summary
			}
			out = append(out, applyVerdict(p, policy))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ScanPluginPacks 扫专家插件市场的技能：<root>/plugins/<插件>/skills/<技能>/SKILL.md。
//
// 默认判定 project/domain：这些是外部专家生态自带的领域技能（游戏美术、小说工坊、
// Remotion 视频……），平台不复制内容，但用户编对应领域的图时应该查得到、能按包名引用。
// 判定表里写了同名条目就以表为准（覆盖权在 skillVerdicts / data/skill-policy.json）。
func ScanPluginPacks(roots []string, policy map[string]skillVerdict) []SkillPack {
	seen := map[string]bool{}
	var out []SkillPack
	for _, root := range roots {
		plugins, err := os.ReadDir(filepath.Join(root, "plugins"))
		if err != nil {
			continue
		}
		for _, pl := range plugins {
			if !pl.IsDir() || strings.HasPrefix(pl.Name(), ".") {
				continue
			}
			skillsDir := filepath.Join(root, "plugins", pl.Name(), "skills")
			entries, err := os.ReadDir(skillsDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
					continue
				}
				name := e.Name()
				if seen[name] {
					continue
				}
				dir := filepath.Join(skillsDir, name)
				raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
				if err != nil {
					continue
				}
				seen[name] = true
				meta := parseSkillMeta(raw)
				p := SkillPack{
					Name:        name,
					Path:        dir,
					Source:      "插件·" + pl.Name(),
					Description: meta.description,
					ReadWhen:    meta.readWhen,
					Bytes:       dirSize(dir),
					Files:       countFiles(dir),
					Refs:        listRefs(filepath.Join(dir, "references")),
				}
				if p.Description == "" {
					p.Description = meta.summary
				}
				out = append(out, applyVerdict(p, policy))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// pluginSkillLabels 专家插件的人话名，用于生成默认判定的理由。
var pluginSkillLabels = map[string]string{
	"game-art-expert":            "游戏美术资产生成（角色/场景/像素/UI/宣传图）",
	"team-novel-studio":          "长篇小说创作工坊",
	"remotion-video-generator":   "Remotion 程序化视频生成",
	"video-gen-team":             "视频内容采集与渲染",
	"godot-game-suite":           "Godot 游戏开发套件",
	"godot-render-shader":        "Godot 渲染与着色器",
	"godot-game-script-engineer": "Godot 脚本工程",
	"python-fullstack-engineer":  "Python 全栈工程",
	"fund-research-analyst":      "基金研究分析",
	"designkit-rotation-video":   "轮播视频设计",
	"bugly-quality-expert":       "质量分析",
	"game-designer":              "游戏设计",
	"game-development-studio":    "游戏开发工作室",
	"unreal-system-engineer":     "Unreal 系统引擎",
	"unreal-technical-artist":    "Unreal 技术美术",
	"workspace-builder":          "工作区搭建",
}

// applyVerdict 套判定表：覆盖文件 > 内置表 > 未判定。
func applyVerdict(p SkillPack, policy map[string]skillVerdict) SkillPack {
	v, ok := policy[p.Name]
	if !ok {
		v, ok = skillVerdicts[p.Name]
	}
	if !ok {
		// 专家插件市场的包没逐条登记：按「外部生态领域技能」给默认判定。
		// 不逐条写的理由——插件生态随用户安装而变，表追不完；但默认判定必须
		// 有理由、有层级、可被表覆盖，不能悄悄变成 unclassified 混进通用产物。
		if plugin := strings.TrimPrefix(p.Source, "插件·"); plugin != p.Source {
			label := pluginSkillLabels[plugin]
			if label == "" {
				label = plugin
			}
			p.Verdict = SkillProject
			p.Reason = "来自专家插件「" + plugin + "」（" + label + "）。外部智能体生态自带的领域技能：" +
				"平台不复制内容（复制即第二真相源），但编对应领域的图时可按包名填进外部智能体节点的技能包参考。"
			p.Tier = TierDomain
			p.Retain = true
			return p
		}
		p.Verdict = SkillUnclassified
		p.Reason = "未登记判定。默认按「领域能力」处理：可查、可被节点引用，但不进默认推荐。"
		p.Tier = TierDomain
		p.Retain = true
		return p
	}
	p.Verdict = v.Verdict
	p.Reason = v.Reason
	p.Evidence = v.Evidence
	p.Card = v.Card
	p.Tier = v.Tier
	if p.Tier == "" {
		// 没标层级的包按领域能力走：漏填不该把它推进默认推荐
		p.Tier = TierDomain
	}
	p.Retain = p.Tier != TierNone
	return p
}

// VerifySkillPacks 核验判定是否站得住，返回问题清单（空 = 全部可核验）。
//
// 三件事必须成立，否则这个索引就是装饰：
//  1. 每条判定都有 Reason（说不出理由 = 没判过）
//  2. mirrored 必须指到「项目根下真实存在且非空」的代码行
//  3. adopted 必须指到一张真实存在的技能卡
//
// 核验不过的条目不删除，改成 unclassified 并保留原判据在 Reason 里——
// 让问题显形，比悄悄放过更安全。
func VerifySkillPacks(packs []SkillPack, projectRoot string) (out []SkillPack, problems []string) {
	out = make([]SkillPack, len(packs))
	copy(out, packs)
	for i := range out {
		p := &out[i]
		var bad []string
		if strings.TrimSpace(p.Reason) == "" {
			bad = append(bad, "没有 Reason")
		}
		// 层级只有三档。写错一个字母会让包从"领域"掉进"不收录"，而且是静默的。
		if p.Tier != TierCore && p.Tier != TierDomain && p.Tier != TierNone {
			bad = append(bad, fmt.Sprintf("层级 %q 非法（只能是 core/domain/none）", p.Tier))
		}
		switch p.Verdict {
		case SkillMirrored:
			if len(p.Evidence) == 0 {
				bad = append(bad, "mirrored 但没给证据")
			}
			for _, ev := range p.Evidence {
				if err := VerifyEvidence(projectRoot, ev); err != nil {
					bad = append(bad, fmt.Sprintf("证据 %s 不可核验：%v", ev, err))
				}
			}
		case SkillAdopted, SkillPartial:
			if p.Card == "" {
				bad = append(bad, "判为 "+p.Verdict+" 但没指到卡 id")
			} else if !cardExists(p.Card) {
				bad = append(bad, fmt.Sprintf("卡 %q 不存在", p.Card))
			}
		}
		if len(bad) > 0 {
			problems = append(problems, p.Name+": "+strings.Join(bad, "；"))
			p.Reason = "【核验未通过，降级为未判定】原判 " + p.Verdict + "：" + strings.Join(bad, "；")
			p.Verdict = SkillUnclassified
			// 降级 ≠ 删除：包还在，仍可查、仍可被节点引用，只是不进默认推荐。
			p.Tier = TierDomain
			p.Retain = true
		}
	}
	return out, problems
}

// VerifyEvidence 校验 `相对路径:行号#锚点` 形式的证据。
//
// 三件事都要成立，缺一即失效：
//  1. 文件存在
//  2. 行号落在文件内
//  3. 该行包含锚点子串（不区分大小写，锚点可省）
//
// 为什么要锚点：只验行号，被人插几行之后证据就指到隔壁去了，表面还"可核验"。
// 锚点让漂移立刻变红——证据要么能对着原文念出来，要么不算证据。
func VerifyEvidence(projectRoot, ev string) error {
	anchor := ""
	if i := strings.Index(ev, "#"); i >= 0 {
		ev, anchor = ev[:i], ev[i+1:]
	}
	idx := strings.LastIndex(ev, ":")
	if idx <= 0 {
		return fmt.Errorf("缺少行号")
	}
	rel, lineStr := ev[:idx], ev[idx+1:]
	n, err := strconv.Atoi(lineStr)
	if err != nil || n <= 0 {
		return fmt.Errorf("行号不是正整数")
	}
	// 证据里的路径大多以 backend/ 或 scripts/ 开头，都相对仓库根
	path := filepath.Join(projectRoot, filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("文件不存在")
	}
	lines := strings.Split(string(b), "\n")
	if n > len(lines) {
		return fmt.Errorf("文件只有 %d 行", len(lines))
	}
	line := lines[n-1]
	if strings.TrimSpace(line) == "" {
		return fmt.Errorf("第 %d 行是空行", n)
	}
	if anchor != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(anchor)) {
		return fmt.Errorf("第 %d 行不含锚点 %q（证据已漂移）", n, anchor)
	}
	return nil
}

// LookupSkillPack 按包名取索引条目（含真实路径与参考文档清单）。
//
// 每次现扫不缓存：用户新加一个 skill 包，节点引用应当立刻可用，而不是等重启引擎。
// 扫十几个目录实测 <20ms——比起"改了不生效"的困惑，这点开销不值一提。
func LookupSkillPack(roots []string, name string) (SkillPack, bool) {
	for _, p := range ScanSkillPacks(roots, nil) {
		if p.Name == name {
			return p, true
		}
	}
	return SkillPack{}, false
}

// LookupPluginSkillPack 在专家插件市场里按技能名反查。
// 与 LookupSkillPack 分开：布局是 plugins/<插件>/skills/<技能>，同名时技能根优先。
func LookupPluginSkillPack(roots []string, name string) (SkillPack, bool) {
	for _, p := range ScanPluginPacks(roots, nil) {
		if p.Name == name {
			return p, true
		}
	}
	return SkillPack{}, false
}

// builtinSkillByID 在内置技能卡里查 id。adopted 判定的核验靠它。
func builtinSkillByID(id string) (SkillCard, bool) {
	for _, c := range builtinSkills {
		if c.ID == id {
			return c, true
		}
	}
	return SkillCard{}, false
}

// builtinAgentByID 在内置智能体卡里查 id。角色卡的核验靠它。
func builtinAgentByID(id string) (AgentCard, bool) {
	for _, c := range builtinAgents {
		if c.ID == id {
			return c, true
		}
	}
	return AgentCard{}, false
}

// cardExists 卡片 id 是否真实存在（agent / skill 两种都认）。
// 提炼出来的可以是角色卡也可以是技能卡——判据只问"这张卡贴不贴得上"，不问它是哪一类。
func cardExists(id string) bool {
	if _, ok := builtinSkillByID(id); ok {
		return true
	}
	if _, ok := builtinAgentByID(id); ok {
		return true
	}
	return false
}

// skillMeta frontmatter 里我们关心的字段。
type skillMeta struct {
	name        string
	description string
	summary     string
	readWhen    []string
}

// parseSkillMeta 解析 SKILL.md 的 YAML frontmatter。
//
// 手写而不引 yaml 库：只需要三个字段，且真实文件里有 BOM、有块标量（description: |）、
// 有列表（read_when），三种都要过。引一个依赖去解析 20 行 YAML 不划算。
func parseSkillMeta(raw []byte) skillMeta {
	var m skillMeta
	text := strings.TrimPrefix(string(raw), "\ufeff")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return m
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return m
	}
	blockKey := "" // 正在收集块标量（description: |）
	inList := ""   // 正在收集列表（read_when:）
	for _, line := range lines[1:end] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		// 列表项
		if strings.HasPrefix(trimmed, "- ") && inList != "" {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			if inList == "read_when" {
				m.readWhen = append(m.readWhen, unquote(item))
			}
			continue
		}
		// 块标量续行（缩进 > 0 且不是 key）
		if blockKey != "" && indent > 0 && !strings.Contains(trimmed, ": ") {
			if blockKey == "description" {
				if m.description != "" {
					m.description += " "
				}
				m.description += trimmed
			}
			continue
		}
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		blockKey, inList = "", ""
		switch key {
		case "name":
			m.name = unquote(val)
		case "description":
			if val == "|" || val == ">" || val == "" {
				blockKey = "description"
			} else {
				m.description = unquote(val)
			}
		case "summary":
			m.summary = unquote(val)
		case "read_when":
			inList = "read_when"
		}
	}
	return m
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// listRefs 列 references/ 下的 .md 文件名。
func listRefs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func countFiles(dir string) int {
	n := 0
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// ReadSkillDoc 读技能包正文（只允许读已索引的包，防路径穿越）。
func ReadSkillDoc(src PathGetter, name string) (string, error) {
	for _, p := range src.Packs() {
		if p.Name != name {
			continue
		}
		for _, fn := range []string{"SKILL.md", "_DEPRECATED.md"} {
			b, err := os.ReadFile(filepath.Join(p.Path, fn))
			if err != nil {
				continue
			}
			const maxDoc = 400 << 10
			if len(b) > maxDoc {
				return string(b[:maxDoc]) + "\n\n…（已截断，原文 " +
					strconv.Itoa(len(b)) + " 字节）", nil
			}
			return string(b), nil
		}
		return "", fmt.Errorf("技能包 %q 没有正文文件", name)
	}
	return "", os.ErrNotExist
}

// PathGetter 供 ReadSkillDoc 取当前索引，避免它依赖全局状态。
type PathGetter interface {
	Packs() []SkillPack
}
