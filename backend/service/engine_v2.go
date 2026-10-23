package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"flowedge/backend/model"
)

// 节点超时：默认 180s，节点可用 data.timeout_ms 覆盖，上限 30 分钟
// （ComfyUI 首次加载 checkpoint 可达数分钟，硬编码短超时会误杀合法任务）
const (
	defaultNodeTimeout = 180 * time.Second
	maxNodeTimeout     = 30 * time.Minute
	// 分镜执行器（shotlist）例外：它的预算按「镜数 × 帧数」算，天然会超过 30 分钟。
	// 旧实现用 maxNodeTimeout 封顶，导致 12 镜长片必然被杀（12 × 169s ≈ 34min > 30min；
	// 按 AGENTS §5.10 实测 0.85–0.93 秒/帧，12 镜 300+ 帧更是要 40 分钟以上）。
	maxShotlistTimeout = 8 * time.Hour
)

// fieldFloat 读节点数值字段（JSON 反序列化后统一是 float64）
func fieldFloat(n *model.NodeInstance, key string, def float64) float64 {
	if v, ok := n.Data[key].(float64); ok && v != 0 {
		return v
	}
	return def
}

// ErrServiceNotConfigured 依赖的外部服务未配置（ASR / TTS 等）。
// 铁律：未配置必须显式失败，绝不能伪造成功结果——那会让用户以为真的跑通了。
var ErrServiceNotConfigured = errors.New("service not configured")

// nodeExecError 带稳定错误码的执行错误（超时、取消等）
type nodeExecError struct {
	Code string
	Err  error
}

func (e *nodeExecError) Error() string { return e.Err.Error() }
func (e *nodeExecError) Unwrap() error { return e.Err }

// classifyError 把执行错误归类为稳定错误码，供 UI 与告警消费
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	var ne *nodeExecError
	if errors.As(err, &ne) {
		return ne.Code
	}
	if errors.Is(err, ErrServiceNotConfigured) {
		return model.ErrorCodeConfigMissing
	}
	return model.ErrorCodeExecution
}

// ErrorCodeOf 导出给 handler 层判断语义用（例如「主动中止 ≠ 失败」）。
func ErrorCodeOf(err error) string { return classifyError(err) }

// maxSubWorkflowDepth 子工作流/循环最大嵌套深度
const maxSubWorkflowDepth = 4

// maxLoopIterations 循环节点硬迭代上限（安全阀，防止无限循环打爆资源）
const maxLoopIterations = 100

// joinIsAny 节点是否配置为「任一前驱完成即执行」（join=any）
func joinIsAny(n *model.NodeInstance) bool {
	j, _ := n.Data["join"].(string)
	return j == "any"
}

// EngineV2 增强版工作流引擎
type EngineV2 struct {
	llm         *LLMService
	llmStream   *LLMStreamService
	t2i         *Text2ImageService
	asr         *ASRService
	tts         *TTSService
	video       *VideoService
	http        *HTTPService
	rag         *RAGServiceV3
	gpuSched    *GPUScheduler
	wsHub       *WSHub
	tools       *ToolExecutor
	cache       *NodeCache
	eventLog    *EventLogger
	shotJobs    *ShotJobStore    // 镜头级作业（可 nil：无库时降级为不落盘）
	creds       *CredentialStore // 凭据仓库（HTTP 节点按名称取密钥）
	llmProfiles *LLMProfileStore // 自定义模型端点（节点 llm_profile 引用）
	agents      *AgentStore      // 外部智能体名册（agent_task 节点引用）
	// toolchain 工具链（skill / MCP / 插件 / 智能体的统一发现层）。
	// mcp_call 节点靠它按名字找服务器；能 nil（老测试构造的引擎没有它）。
	toolchain *Toolchain
	// agentGate 外部智能体的并发配额。
	//
	// 为什么节点级还要一道：整图准入闸只在提交时问一次「现在能不能跑」，
	// 但它管不了「一张图里 5 个 agent_task 同时起 5 个 Claude Code」——
	// 那是提交之后才发生的扇出。每个 agent 都是独立进程，各吃各的内存，
	// 一起上来能把机器压死，而准入闸那时早就放行了。
	agentGateOnce sync.Once
	agentGate     chan struct{}
	cards         *CardStore // 能力卡片（节点 binding 引用）
	// skillRoots 技能包索引的扫描根（agent_task 的 skill_refs 用它反查路径）。
	// 空 = 用默认（用户级 + 冷存储 + DSH + agents + 项目级 + 额外根文件，见 SkillRoots）。
	skillRoots []string
	// pluginRoots 专家插件市场的技能根（plugins/<插件>/skills/<技能> 两级布局）。
	// 与 skillRoots 分开：来源是外部专家生态，布局不同，合并会让「扫到几个包」失去意义。
	pluginRoots []string
	visionModel string        // 自动探测到的本地视觉模型（vision 节点留空时使用）
	vram        *vramArbiter  // 显存仲裁：同一时刻只跑一个重任务
	vramMu      sync.Mutex    // 保护 vram 的懒初始化（见 arbiter 注释）
	deps        *Supervisor   // 依赖进程托管（H3 需要按需切换实例）
	profiles    *ProfileStore // 显存档案（实测峰值，供提交前预测；可 nil）
	nodes       nodeRegistry  // 节点类型注册表（内置 15 种 + 可注册自定义）
	// hotNodes 运行期热注册的节点（插件重载用）。
	//
	// 为什么不直接写进 e.nodes：内置注册表在启动后是**只读**的，
	// 而热重载发生在引擎跑着的时候（用户点「重新扫描」）。往同一个 map 里写、
	// 另一个 goroutine 在读 → Go 直接 fatal（concurrent map read and map write），
	// 不是"偶发不一致"那么轻。所以热注册单独一份，读写走 RWMutex。
	hotNodes        nodeRegistry
	hotSides        map[string]string
	hotMu           sync.RWMutex
	runMu           sync.Mutex
	activeRuns      map[uint]context.CancelFunc        // runID -> 取消函数（支持手动中止）
	resolveWorkflow func(uint) ([]byte, []byte, error) // 子工作流解析器
	depth           atomic.Int32                       // 当前嵌套深度

	// P0 可观测性：按节点类型维护耗时 EWMA（毫秒），供长任务 ETA 与进度心跳下发
	durMu   sync.Mutex
	durEWMA map[string]float64

	nodeSides map[string]string // 自定义节点声明的显存侧别（插件入口）
	memo      *llmMemo          // 确定性 LLM 调用备忘（翻译 / 优化 / 拆解）
	memoMu    sync.Mutex
	// 显存共存优化开关：true = 关闭（退回「换侧必卸载」的保守行为）。
	// 零值 false 表示开启——只有「显存确实装得下」时才会真的省掉卸载。
	vramCoexistOff bool
	// 侧别感知调度的逃生阀门：false（零值）= 开启分组调度。
	// 置 true 时退回「所有重任务一次性起飞」的旧行为，便于对照测量或排障。
	sideAwareOff bool
	// P1 编排深度：同层并行度上限。0 = 不限（默认）。
	// 显存安全由 vram 仲裁器的 heavyMu 保证（重任务天然串行），
	// 这里限制的是「同一层能同时起飞多少个节点」，防止一张宽图把 CPU/磁盘打满。
	maxParallel int
}

// nodeProgressTick 进度心跳间隔。做成包级变量是为了让测试能调快，不必真等 2 秒。
var nodeProgressTick = 2 * time.Second

// SetMaxParallel 限制同一拓扑层同时执行的节点数（0 或负 = 不限）。
// 重任务的显存安全不靠它：扩散/语言/语音节点进入执行前会抢 vram 仲裁器的
// heavyMu，天然串行。这里只是给「宽图并发」加一道总闸。
func (e *EngineV2) SetMaxParallel(n int) {
	if n < 0 {
		n = 0
	}
	e.maxParallel = n
}

// SetSideAwareScheduling 开关侧别感知调度（默认开启）。
//
// 关闭后退回旧行为：同层所有重任务一次性起飞，执行顺序由 goroutine 调度决定
// （等价于随机）。保留这个开关是为了两件事：
//  1. 对照测量——同一张图前后各跑一次，直接看换侧次数差多少；
//  2. 逃生——万一调度分组在某张图上引入问题，能立刻退回原路径。
func (e *EngineV2) SetSideAwareScheduling(enabled bool) {
	e.sideAwareOff = !enabled
}

// SetVRAMCoexist 开关「显存装得下就让两侧共存」。
//
// 默认开启：只有实测空闲显存足够（模型体积 ×1.3 + 1GB 留白）才会跳过卸载，
// 否则照旧卸载。万一某台机器上判断偏乐观导致 OOM，把它关掉即回到
// 「换侧必卸载」的旧行为，不需要回滚代码。
func (e *EngineV2) SetVRAMCoexist(enabled bool) {
	e.vramCoexistOff = !enabled
}

// CoexistCount 因显存装得下而免除的驱逐次数（每命中一次约等于省 183s）。
func (e *EngineV2) CoexistCount() int {
	e.vramMu.Lock()
	defer e.vramMu.Unlock()
	if e.vram == nil {
		return 0
	}
	return e.vram.coexists
}

// SideSwitchCount 本引擎实例累计的逻辑换侧次数（诊断/对照测量用）。
func (e *EngineV2) SideSwitchCount() int {
	e.vramMu.Lock()
	defer e.vramMu.Unlock()
	if e.vram == nil {
		return 0
	}
	return e.vram.switches
}

// SideUnloadCount 累计**真实**卸载扩散模型的次数。
// 与 SideSwitchCount 的区别：只有它才对应一次约 180 秒的冷加载
// （实测冷加载 183s / 热生成 3s）。做效率评估时看这个数，不要看逻辑换侧。
func (e *EngineV2) SideUnloadCount() int {
	e.vramMu.Lock()
	defer e.vramMu.Unlock()
	if e.vram == nil {
		return 0
	}
	return e.vram.unloads
}

// memoStore 备忘实例（懒加载：既有测试用结构体字面量构造引擎，字段为 nil）
func (e *EngineV2) memoStore() *llmMemo {
	e.memoMu.Lock()
	defer e.memoMu.Unlock()
	if e.memo == nil {
		e.memo = newLLMMemo(0)
	}
	return e.memo
}

// translatePromptToEnglish 把图像提示词译成英文（确定性，走备忘）。
//
// 指令要求「只输出英文提示词」：加解释会把解释也拼进 prompt，
// flux 会把 "Translation:" 这类词真的画进去。
func (e *EngineV2) translatePromptToEnglish(prompt string) (string, error) {
	if e.llm == nil {
		return "", fmt.Errorf("LLM 服务未配置")
	}
	instr := "Translate the following image-generation prompt into English. " +
		"Output ONLY the English prompt itself — no explanations, no quotes, " +
		"no prefixes like 'Translation:'. Keep proper nouns, style keywords " +
		"and composition terms intact.\n\n" + prompt

	k := memoKey(e.llm.Model, instr)
	if v, ok := e.memoStore().Get(k); ok {
		return v, nil // 同一段中文只翻一次
	}
	out, err := e.llm.ChatWithModel(instr, e.llm.Model)
	if err != nil {
		return "", fmt.Errorf("提示词自动翻译失败（已用原文继续）：%w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("提示词自动翻译返回空（已用原文继续）")
	}
	e.memoStore().Put(k, out)
	return out, nil
}

// nodeSideOf 节点类型的显存侧别：先查自定义注册时的声明，再回落到内置映射。
func (e *EngineV2) nodeSideOf(nodeType string) string {
	e.hotMu.RLock()
	if s, ok := e.hotSides[nodeType]; ok && s != "" {
		e.hotMu.RUnlock()
		return s
	}
	e.hotMu.RUnlock()
	if e.nodeSides != nil {
		if s, ok := e.nodeSides[nodeType]; ok {
			return s
		}
	}
	return nodeSide(nodeType)
}

// RegisterNodeTypeWithSide 注册自定义节点，并声明它要占哪一侧显存。
//
// 插件 / 自定义节点只要会加载大模型（ComfyUI、Ollama、whisper…），就必须用这个
// 入口而不是 RegisterNodeType：否则调度器按「不吃显存」处理、和别的重任务
// 一起并发起飞，24GB 上会直接 OOM。
// side 传 "" 表示确实不吃显存，等同于 RegisterNodeType。
func (e *EngineV2) RegisterNodeTypeWithSide(nodeType, side string, fn NodeExecutor) {
	if e.nodes == nil {
		e.runMu.Lock()
		if e.nodes == nil {
			e.registerBuiltinNodes()
		}
		e.runMu.Unlock()
	}
	e.runMu.Lock()
	e.nodes[nodeType] = fn
	if side != "" {
		if e.nodeSides == nil {
			e.nodeSides = map[string]string{}
		}
		e.nodeSides[nodeType] = side
	}
	e.runMu.Unlock()
}

// parallelSlots 生成本层的并发令牌通道；无限制时返回 nil（调用方需判空）。
func (e *EngineV2) parallelSlots() chan struct{} {
	if e.maxParallel <= 0 {
		return nil
	}
	ch := make(chan struct{}, e.maxParallel)
	for i := 0; i < e.maxParallel; i++ {
		ch <- struct{}{}
	}
	return ch
}

// SetLLMStream 注入流式 LLM 服务（nil 则 LLM 节点走非流式）
func (e *EngineV2) SetLLMStream(s *LLMStreamService) {
	e.llmStream = s
}

// SetResolver 注入子工作流解析器（由 main 用 DB 实现）
func (e *EngineV2) SetResolver(fn func(uint) ([]byte, []byte, error)) {
	e.resolveWorkflow = fn
}

// SetCache 注入节点缓存（nil 表示不启用）
func (e *EngineV2) SetCache(c *NodeCache) { e.cache = c }

// observeDuration 记录一次节点实际耗时，按 EWMA 更新（新样本权重 0.3）。
// 只看同类型节点的耗时：多模态里 text2video 和 llm 差着两个数量级，混算没意义。
func (e *EngineV2) observeDuration(typ string, ms int64) {
	if typ == "" || ms <= 0 {
		return
	}
	e.durMu.Lock()
	defer e.durMu.Unlock()
	if e.durEWMA == nil {
		e.durEWMA = make(map[string]float64)
	}
	if prev, ok := e.durEWMA[typ]; ok {
		e.durEWMA[typ] = prev*0.7 + float64(ms)*0.3
	} else {
		e.durEWMA[typ] = float64(ms)
	}
}

// ETAFor 返回该类型节点的预计耗时（毫秒）。无历史样本返回 0（前端据此隐藏 ETA）。
func (e *EngineV2) ETAFor(typ string) int64 {
	e.durMu.Lock()
	defer e.durMu.Unlock()
	v, ok := e.durEWMA[typ]
	if !ok || v <= 0 {
		return 0
	}
	return int64(v)
}

// SetEventLog 注入事件日志（nil 表示只广播不落库）
func (e *EngineV2) SetEventLog(l *EventLogger) { e.eventLog = l }

// SetCredentials 注入凭据仓库（HTTP 节点 credential 字段生效的前提）
func (e *EngineV2) SetCredentials(c *CredentialStore) { e.creds = c }

// SetCards 注入能力卡片仓库（节点 binding 字段生效的前提）。
func (e *EngineV2) SetCards(c *CardStore) { e.cards = c }

// SetLLMProfiles 注入自定义模型清单（节点 llm_profile 字段生效的前提）。
// nil 时引用 profile 的节点会显式报错，而不是悄悄退回本机模型——
// "以为在用云端、其实在跑本地"是最难查的一类问题。
func (e *EngineV2) SetLLMProfiles(s *LLMProfileStore) { e.llmProfiles = s }

// SetAgents 注入外部智能体名册（agent_task 节点生效的前提）。
// nil 时该节点显式报"名册未加载"，而不是静默跳过 —— 静默跳过的后果是
// 下游拿到空文本还以为 agent 给了空答案。
func (e *EngineV2) SetAgents(s *AgentStore) { e.agents = s }

// SetSkillRoots 指定技能包扫描根（agent_task 的 skill_refs 靠它找到 SKILL.md 路径）
func (e *EngineV2) SetSkillRoots(roots []string) { e.skillRoots = roots }

// SetPluginRoots 指定专家插件市场的技能根（agent_task 的 skill_refs 也要能反查插件技能）
func (e *EngineV2) SetPluginRoots(roots []string) { e.pluginRoots = roots }

// Agents 当前名册（可能为 nil）
func (e *EngineV2) Agents() *AgentStore { return e.agents }

// SetVisionModel 注入自动探测到的视觉模型名（vision 节点未显式指定模型时使用）
func (e *EngineV2) SetVisionModel(name string) { e.visionModel = name }

// SetSupervisor 注入依赖管理器（H3 需要它来切换 ComfyUI 实例）
func (e *EngineV2) SetSupervisor(s *Supervisor) { e.deps = s }

// ctxKeyRunID runID 在 context 里的键类型（避免与其他包的 context 键冲突）
type ctxKeyRunID struct{}

// withRunID 把 runID 注入 context，供引擎子树取用
func withRunID(ctx context.Context, id uint) context.Context {
	return context.WithValue(ctx, ctxKeyRunID{}, id)
}

// runIDFrom 取 runID；取不到返回 0（表示独立执行，不入事件日志）
func runIDFrom(ctx context.Context) uint {
	if ctx == nil {
		return 0
	}
	if v, ok := ctx.Value(ctxKeyRunID{}).(uint); ok {
		return v
	}
	return 0
}

// emit 广播 + 落库。
// 广播保持原有的非阻塞语义（WSHub 缓冲满即丢弃），落库失败只记日志不阻塞引擎。
func (e *EngineV2) emit(runID uint, ev Event) {
	ev.RunID = runID
	if e.wsHub != nil {
		e.wsHub.Broadcast(ev)
	}
	if e.eventLog != nil {
		e.eventLog.Emit(runID, ev)
	}
}

func NewEngineV2(
	llm *LLMService,
	t2i *Text2ImageService,
	asr *ASRService,
	tts *TTSService,
	video *VideoService,
	http *HTTPService,
	rag *RAGServiceV3,
	gpuSched *GPUScheduler,
	wsHub *WSHub,
) *EngineV2 {
	e := &EngineV2{
		llm:      llm,
		t2i:      t2i,
		asr:      asr,
		tts:      tts,
		video:    video,
		http:     http,
		rag:      rag,
		gpuSched: gpuSched,
		wsHub:    wsHub,
	}
	// 注册表在构造时装配：保持 NewEngineV2 签名不变（26 个既有测试直接调用它）
	e.registerBuiltinNodes()
	return e
}

// Execute 执行工作流（按层并行 DAG + 按边传递输入 + 分支级联跳过 + 节点超时）
// ExecuteWithRunID 可取消的执行入口：注册 runID → 取消函数
func (e *EngineV2) ExecuteWithRunID(nodesJSON, edgesJSON []byte, runID uint, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	ctx, cancel := context.WithCancel(context.Background())
	// runID 注入 context：使主循环与 executeNode 子树内的事件都归属同一次运行，
	// 事件日志落库后可回放与统计 token
	ctx = withRunID(ctx, runID)

	e.runMu.Lock()
	if e.activeRuns == nil {
		e.activeRuns = make(map[uint]context.CancelFunc)
	}
	e.activeRuns[runID] = cancel
	e.runMu.Unlock()

	defer func() {
		cancel()
		e.runMu.Lock()
		delete(e.activeRuns, runID)
		e.runMu.Unlock()
	}()

	return e.runWithContext(ctx, nodesJSON, edgesJSON, nil, "", persist)
}

// ExecuteWithControl 带单步/断点控制的可取消执行入口。
// rc 为 nil 时与 ExecuteWithRunID 完全等价。
func (e *EngineV2) ExecuteWithControl(nodesJSON, edgesJSON []byte, runID uint, rc *RunControl, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withRunID(ctx, runID)
	ctx = withRunControl(ctx, rc)
	// 运行级 agent 并发配额（rc 未声明时注入空 context，行为与原来完全一致）
	ctx = withAgentPool(ctx, rc.AgentConcurrency())

	e.runMu.Lock()
	if e.activeRuns == nil {
		e.activeRuns = make(map[uint]context.CancelFunc)
	}
	e.activeRuns[runID] = cancel
	e.runMu.Unlock()

	defer func() {
		cancel()
		e.runMu.Lock()
		delete(e.activeRuns, runID)
		e.runMu.Unlock()
	}()

	return e.runWithContext(ctx, nodesJSON, edgesJSON, nil, "", persist)
}

// ResumeWithControl 断点续跑 + 单步/断点控制。
// 单步调试的每一次「继续」都是走这里：已成功节点复用输出，只有 pending 节点会真正执行。
func (e *EngineV2) ResumeWithControl(nodesJSON, edgesJSON []byte, prev []model.NodeInstance, runID uint, rc *RunControl, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withRunID(ctx, runID)
	ctx = withRunControl(ctx, rc)
	// 运行级 agent 并发配额（rc 未声明时注入空 context，行为与原来完全一致）
	ctx = withAgentPool(ctx, rc.AgentConcurrency())

	e.runMu.Lock()
	if e.activeRuns == nil {
		e.activeRuns = make(map[uint]context.CancelFunc)
	}
	e.activeRuns[runID] = cancel
	e.runMu.Unlock()

	defer func() {
		cancel()
		e.runMu.Lock()
		delete(e.activeRuns, runID)
		e.runMu.Unlock()
	}()

	return e.runWithContext(ctx, nodesJSON, edgesJSON, prev, "", persist)
}

// CancelRun 中止一个正在运行的工作流，返回是否找到该运行
func (e *EngineV2) CancelRun(runID uint) bool {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	if cancel, ok := e.activeRuns[runID]; ok {
		cancel()
		// 光取消 ctx 不够：长片一镜要跑 3–5 分钟，得让 ComfyUI 立刻停手。
		e.interruptComfy()
		return true
	}
	return false
}

func (e *EngineV2) Execute(nodesJSON, edgesJSON []byte) ([]model.NodeInstance, error) {
	return e.run(nodesJSON, edgesJSON, nil, "", nil)
}

// ExecutePersistent 执行并逐层增量落库（persist 在每层完成后回调，供断点续跑）
func (e *EngineV2) ExecutePersistent(nodesJSON, edgesJSON []byte, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	return e.run(nodesJSON, edgesJSON, nil, "", persist)
}

// Resume 断点续跑：prevStates 中 success 节点复用输出直接跳过、skipped 分支保持跳过，其余节点重新执行
func (e *EngineV2) Resume(nodesJSON, edgesJSON []byte, prevStates []model.NodeInstance, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	return e.run(nodesJSON, edgesJSON, prevStates, "", persist)
}

// ResumeFrom 从指定节点开始重跑：该节点及其全部下游强制重算，
// 已成功的上游保留原输出不变（单节点重跑与「从这继续」共用此入口）。
// fromNode 为空时等同 Resume。
func (e *EngineV2) ResumeFrom(nodesJSON, edgesJSON []byte, prevStates []model.NodeInstance, fromNode string, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	return e.run(nodesJSON, edgesJSON, prevStates, fromNode, persist)
}

// ResumeFromWithRunID 断点续跑的可取消入口（带 runID）。
// 必须走这个入口：否则 ctx 里没有 runID，续跑产生的事件会因 runID=0 而被丢弃，
// 表现为「重跑后执行面板看不到任何新事件」。
func (e *EngineV2) ResumeFromWithRunID(nodesJSON, edgesJSON []byte, prevStates []model.NodeInstance, fromNode string, runID uint, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withRunID(ctx, runID)

	e.runMu.Lock()
	if e.activeRuns == nil {
		e.activeRuns = make(map[uint]context.CancelFunc)
	}
	e.activeRuns[runID] = cancel
	e.runMu.Unlock()

	defer func() {
		cancel()
		e.runMu.Lock()
		delete(e.activeRuns, runID)
		e.runMu.Unlock()
	}()

	return e.runWithContext(ctx, nodesJSON, edgesJSON, prevStates, fromNode, persist)
}

// downstreamClosure 计算 fromNode 及其所有下游（沿边正向可达）
func downstreamClosure(edges []model.EdgeInstance, from string) map[string]bool {
	if from == "" {
		return nil
	}
	succ := make(map[string][]string)
	for _, eg := range edges {
		succ[eg.Source] = append(succ[eg.Source], eg.Target)
	}
	seen := map[string]bool{}
	queue := []string{from}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, succ[id]...)
	}
	return seen
}

// run 引擎主循环
func (e *EngineV2) run(nodesJSON, edgesJSON []byte, prev []model.NodeInstance, fromNode string, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	return e.runWithContext(context.Background(), nodesJSON, edgesJSON, prev, fromNode, persist)
}

func (e *EngineV2) runWithContext(ctx context.Context, nodesJSON, edgesJSON []byte, prev []model.NodeInstance, fromNode string, persist func([]model.NodeInstance)) ([]model.NodeInstance, error) {
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance

	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return nil, fmt.Errorf("parse nodes failed: %w", err)
	}
	if err := json.Unmarshal(edgesJSON, &edges); err != nil {
		return nil, fmt.Errorf("parse edges failed: %w", err)
	}

	// 图结构校验：环 / 自环 / 悬空边必须显式报错。
	// 没有这一步，成环时 remaining 永不归零 → 主循环零执行且不返回任何错误，
	// 用户会以为工作流在正常运行（最恶劣的一类静默失败）。
	if issues := ValidateDAG(nodes, edges); HasBlockingIssue(issues) {
		first := FirstBlocking(issues)
		for i := range nodes {
			nodes[i].Status = "skipped"
			nodes[i].ErrorCode = string(first.Code)
		}
		return nodes, fmt.Errorf("%s (code=%s)", first.Message, first.Code)
	}

	// runID 来自 context：0 表示独立执行（Execute/Resume），不入事件日志
	runID := runIDFrom(ctx)

	// 构建邻接表
	adj := make(map[string][]model.EdgeInstance)
	nodeMap := make(map[string]*model.NodeInstance)
	order := make([]string, 0, len(nodes))
	remaining := make(map[string]int)                     // 该节点尚未完成的前驱数
	nodeInputs := make(map[string]map[string]interface{}) // nodeID -> {sourceID: output}
	preds := make(map[string][]string)                    // nodeID -> 前驱列表（判断分支是否会合）
	runCtx := make(map[string]interface{})                // 变量系统：已完成节点的输出（{{id.text}}）

	// 断点回填：上次 success 的节点复用输出；skipped 重置为 pending
	// （由本轮执行重新裁剪分支）；failed/running/pending 重跑
	// fromNode 模式：从指定节点（含下游）重跑，闭包内的节点即使上次成功也强制重算
	rerunSet := downstreamClosure(edges, fromNode)
	prevMap := make(map[string]model.NodeInstance, len(prev))
	for _, pn := range prev {
		prevMap[pn.ID] = pn
	}
	for i := range nodes {
		n := &nodes[i]
		if pn, ok := prevMap[n.ID]; ok {
			if pn.Status == "success" && !rerunSet[n.ID] {
				n.Status = "success"
				n.Output = pn.Output
				n.StartedAt = pn.StartedAt
				n.EndedAt = pn.EndedAt
			} else {
				// 重算节点：清空上次结果与错误标记，避免旧状态污染本轮判定
				n.Status = "pending"
				n.Output = nil
				n.Error = ""
				n.ErrorCode = ""
				n.StartedAt = nil
				n.EndedAt = nil
			}
		} else {
			n.Status = "pending"
		}
		nodeMap[n.ID] = n
		order = append(order, n.ID)
		remaining[n.ID] = 0
		nodeInputs[n.ID] = make(map[string]interface{})
	}

	var mu sync.Mutex
	var wfErr error
	var errOnce sync.Once
	markFailed := func(id, msg string) {
		errOnce.Do(func() { wfErr = fmt.Errorf("node %s (%s) failed: %s", id, nodeMap[id].Type, msg) })
	}

	// skipBFS：从被跳过的节点出发，级联标记整条未激活分支为 skipped
	skipBFS := func(startID string) {
		queue := []string{startID}
		seen := map[string]bool{}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			n := nodeMap[id]
			if n.Status != "pending" {
				continue // 只裁剪尚未执行的节点，重复调用安全
			}
			// 汇合节点保护：只要还有「尚未定局」的前驱（pending/running），
			// 该节点就不属于死分支。已 success 的前驱不再传播（如 if_else
			// 未命中的那一侧），不能算作活跃
			active := false
			for _, pid := range preds[id] {
				if pn := nodeMap[pid]; pn != nil && (pn.Status == "pending" || pn.Status == "running") {
					active = true
					break
				}
			}
			if len(preds[id]) > 0 && active {
				continue
			}
			n.Status = "skipped"
			for _, eg := range adj[id] {
				// 被跳过的前驱永远不会完成，必须把它从下游的前驱计数里扣掉，
				// 否则分支汇合节点会永远等不到 remaining==0（死等）
				if remaining[eg.Target] > 0 {
					remaining[eg.Target]--
				}
				queue = append(queue, eg.Target)
			}
		}
	}

	var skipSeeds []string // Resume 阶段的分支裁剪种子（须等 remaining 统计完成后再处理）
	for _, edge := range edges {
		adj[edge.Source] = append(adj[edge.Source], edge)
		preds[edge.Target] = append(preds[edge.Target], edge.Source)
		src := nodeMap[edge.Source]
		switch {
		case src != nil && src.Status == "success":
			// 已完成节点：复用输出、不占前驱计数（它不会再进入执行层）。
			// if_else 需按上次分支结果路由，未命中的分支照常级联跳过
			if src.Type == "if_else" {
				branch, _ := src.Output.(map[string]interface{})
				branchPath, _ := branch["branch"].(string)
				label := edge.SourceHandle
				if label == "" {
					label = "true"
				}
				if branchPath != label {
					skipSeeds = append(skipSeeds, edge.Target)
					continue
				}
			}
			nodeInputs[edge.Target][edge.Source] = src.Output
		default:
			remaining[edge.Target]++
		}
	}
	for _, t := range skipSeeds {
		skipBFS(t)
	}

	// 初始层：无未满足前驱的待执行节点
	current := []string{}
	for _, id := range order {
		if remaining[id] == 0 && nodeMap[id].Status == "pending" {
			current = append(current, id)
		}
	}

	if persist != nil {
		persist(nodes)
	}

	for len(current) > 0 {
		// 每层开始前检查取消：节点内部无法中断，但已完成的层不会浪费
		select {
		case <-ctx.Done():
			for _, id := range order {
				if nodeMap[id].Status == "pending" {
					nodeMap[id].Status = "skipped"
					nodeMap[id].ErrorCode = model.ErrorCodeCancelled
				}
			}
			return nodes, &nodeExecError{Code: model.ErrorCodeCancelled, Err: fmt.Errorf("运行已取消")}
		default:
		}

		var wg sync.WaitGroup
		next := []string{}
		inNext := map[string]bool{}
		layerFailed := false
		var layerMu sync.Mutex

		// 并行度总闸：无限制时 slots 为 nil，完全走原来的无约束并发
		slots := e.parallelSlots()

		// propagate 写出边（调用方必须持有 mu）：
		//   1. 写入下游输入槽（多前驱时天然形成汇合输入）
		//   2. 递减前驱计数
		//   3. 按 join 策略决定是否入队：all=等齐前驱；any=任一前驱完成即执行
		propagate := func(nid string, out interface{}, branchPath string) {
			for _, eg := range adj[nid] {
				if branchPath != "" {
					label := eg.SourceHandle
					if label == "" {
						label = "true"
					}
					if label != branchPath {
						skipBFS(eg.Target)
						continue
					}
				}

				nodeInputs[eg.Target][nid] = out
				if remaining[eg.Target] > 0 {
					remaining[eg.Target]--
				}

				tgt := nodeMap[eg.Target]
				if tgt == nil || tgt.Status != "pending" || inNext[eg.Target] {
					continue
				}
				if remaining[eg.Target] <= 0 || joinIsAny(tgt) {
					inNext[eg.Target] = true
					next = append(next, eg.Target)
				}
			}
		}

		// 按显存侧别给本层节点分组："" = 不吃显存
		heavy := map[string][]string{}
		var light []string
		for _, id := range current {
			if n := nodeMap[id]; n != nil && e.nodeSideOf(n.Type) != "" {
				heavy[e.nodeSideOf(n.Type)] = append(heavy[e.nodeSideOf(n.Type)], id)
			} else {
				light = append(light, id)
			}
		}

		// runNode 单节点执行体：断点申请 → 输入快照 → 执行 → 缓存 → 事件上报
		runNode := func(nid string) {

			// 单步 / 断点：先向控制对象申请执行许可。
			// 命中断点或预算用尽时节点保持 pending（不置 running、不写 started_at），
			// 这样 Resume 时它会被当作「未执行」重新排队，语义与用户预期一致。
			if rc := runControlFrom(ctx); rc != nil {
				if rc.ShouldPauseBefore(nid) {
					rc.MarkPaused(nid, "breakpoint")
					return
				}
				if !rc.Take() {
					rc.MarkPaused(nid, "step")
					return
				}
			}

			mu.Lock()
			node := nodeMap[nid]
			node.Status = "running"
			now := time.Now()
			node.StartedAt = &now
			// 快照输入：只来自与本节点入边相连的前驱
			upstream := make(map[string]interface{})
			for k, v := range nodeInputs[nid] {
				upstream[k] = v
			}
			// 变量解析：把 {{nodeId.field}} 换成上游输出。
			// 用 data 的副本执行，避免替换结果被写回工作流定义
			ctxSnapshot := make(map[string]interface{}, len(runCtx))
			for k, v := range runCtx {
				ctxSnapshot[k] = v
			}
			nodeCopy := *node
			nodeCopy.Data = resolveData(node.Data, ctxSnapshot)
			execNode := &nodeCopy
			mu.Unlock()

			e.emit(runID, Event{Type: "node_started", NodeID: nid, Data: map[string]interface{}{"type": node.Type}})

			// P0 可观测性：执行期间定期推进度心跳（已用 / 预计）。
			// 多模态一个节点常跑一两分钟，此前界面只有一个转圈、看不到进度。
			hbStop := make(chan struct{})
			hbStart := time.Now()
			if node.StartedAt != nil {
				hbStart = *node.StartedAt
			}
			go func() {
				ticker := time.NewTicker(nodeProgressTick)
				defer ticker.Stop()
				for {
					select {
					case <-hbStop:
						return
					case <-ticker.C:
						e.emit(runID, Event{Type: "node_progress", NodeID: nid,
							Data: map[string]interface{}{
								"type":       node.Type,
								"elapsed_ms": time.Since(hbStart).Milliseconds(),
								"eta_ms":     e.ETAFor(node.Type),
							}})
					}
				}
			}()
			defer close(hbStop)

			// 带超时 + 重试执行节点（retry / retry_delay_ms 数据驱动，重试上限 5）
			type execResult struct {
				out interface{}
				err error
			}
			retryCount := 0
			if r, ok := node.Data["retry"].(float64); ok && r > 0 {
				retryCount = int(r)
				if retryCount > 5 {
					retryCount = 5
				}
			}
			retryDelay := 1000 * time.Millisecond
			if d, ok := node.Data["retry_delay_ms"].(float64); ok && d >= 0 {
				retryDelay = time.Duration(d) * time.Millisecond
			}

			// 节点级超时：data.timeout_ms 覆盖默认 180s。
			// 上限按节点类型给：shotlist 允许到 8h（长片本来就该跑几十分钟），
			// 其余仍是 30 分钟。
			timeout := nodeTimeoutFor(node)
			limit := nodeMaxTimeoutFor(node.Type)
			if ms, ok := node.Data["timeout_ms"].(float64); ok && ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
				if timeout > limit {
					timeout = limit
				}
			}

			// 节点缓存：命中则跳过执行（省 GPU / 网络）
			cacheKeyStr := ""
			if e.cache != nil && cacheableNode(execNode.Type, execNode.Data) {
				cacheKeyStr = cacheKey(execNode.Type, execNode.Data, upstream)
				if cached, ok := e.cache.Get(cacheKeyStr); ok {
					mu.Lock()
					endCached := time.Now()
					node.EndedAt = &endCached
					node.Status = "success"
					node.Output = cached
					runCtx[nid] = cached
					branchCached := ""
					if node.Type == "if_else" {
						if b, ok := cached.(map[string]interface{}); ok {
							branchCached, _ = b["branch"].(string)
						}
					}
					propagate(nid, cached, branchCached)
					mu.Unlock()
					e.emit(runID, Event{Type: "node_finished", NodeID: nid,
						Data: map[string]interface{}{"output": cached, "cache_hit": true,
							"duration_ms": endCached.Sub(hbStart).Milliseconds()}})
					return
				}
			}

			var out interface{}
			var execErr error
			for attempt := 0; attempt <= retryCount; attempt++ {
				if attempt > 0 {
					time.Sleep(retryDelay)
					e.emit(runID, Event{Type: "node_retry", NodeID: nid, Message: fmt.Sprintf("attempt %d/%d", attempt+1, retryCount+1)})
				}
				resCh := make(chan execResult, 1)
				go func() {
					o, err := e.executeNode(ctx, execNode, upstream)
					resCh <- execResult{o, err}
				}()
				select {
				case res := <-resCh:
					out, execErr = res.out, res.err
				case <-time.After(timeout):
					execErr = &nodeExecError{
						Code: model.ErrorCodeTimeout,
						Err:  fmt.Errorf("node timeout after %s", timeout),
					}
				}
				if execErr == nil {
					break
				}
			}

			mu.Lock()
			endNow := time.Now()
			node.EndedAt = &endNow
			// P0 可观测性：本次实际耗时入 EWMA，供后续同类型节点算 ETA
			durMs := endNow.Sub(hbStart).Milliseconds()
			e.observeDuration(node.Type, durMs)

			if execErr != nil {
				node.Status = "failed"
				node.Error = execErr.Error()
				node.ErrorCode = classifyError(execErr)
				// 失败也留产物：门类节点「判不过」时会同时返回逐项检查与错误。
				// 丢掉它，证据层就只能看到「节点失败且没有 checks」，把一次
				// 「判不过」记成「没测到」(unscored) —— 评测结论直接反向，
				// 且自进化会把门当成总在崩。判据是**有没有 checks**（evidence.go gateFacts），
				// 所以这里保住 out 就是保住判据（2026-10-23 抓到）。
				if out != nil {
					node.Output = out
				}
				// 重试耗尽：仅在无法归类的通用执行错误时标记，
				// 保留 timeout / config_missing 等更具体的错误码
				if node.ErrorCode == model.ErrorCodeExecution && retryCount > 0 {
					node.ErrorCode = model.ErrorCodeRetryExhausted
				}

				// continue_on_error：节点失败但不中断工作流，向下游传播错误对象
				// （与 join=any 组合即「任一成功即可，失败分支忽略」）
				cont, _ := node.Data["continue_on_error"].(bool)
				if cont {
					failOut := map[string]interface{}{"text": "", "error": execErr.Error()}
					node.Output = failOut
					propagate(nid, failOut, "")
					mu.Unlock()
					e.emit(runID, Event{Type: "node_failed", NodeID: nid, Message: execErr.Error(),
						Data: map[string]interface{}{"duration_ms": durMs}})
					return
				}

				layerMu.Lock()
				layerFailed = true
				layerMu.Unlock()
				markFailed(nid, execErr.Error())
				mu.Unlock()
				e.emit(runID, Event{Type: "node_failed", NodeID: nid, Message: execErr.Error(),
					Data: map[string]interface{}{"duration_ms": durMs}})
				return
			}

			node.Status = "success"
			node.Output = out
			runCtx[nid] = out // 供下游节点以 {{nid.field}} 引用
			if e.cache != nil && cacheKeyStr != "" {
				e.cache.Put(cacheKeyStr, out)
			}
			mu.Unlock()
			e.emit(runID, Event{Type: "node_finished", NodeID: nid,
				Data: map[string]interface{}{"output": out, "duration_ms": durMs}})

			// 处理出边（if_else 需按命中分支裁剪未激活侧）
			branchPath := ""
			if node.Type == "if_else" {
				b, _ := out.(map[string]interface{})
				branchPath, _ = b["branch"].(string)
			}
			mu.Lock()
			propagate(nid, out, branchPath)
			mu.Unlock()
		}

		// 侧别感知调度（P4 效率）：本层重任务按侧别分组，组内并行、组间串行，
		// 换侧次数从 O(本层节点数) 压到 ≤3。
		//
		// 换侧是这套引擎最贵的一次开销：卸载 flux 要等 CUDA 异步归还
		// （实测 5–10s，见 releaseDiffusionAndWait），重新加载又要 ~40s 冷启动。
		// 而同层节点的起飞顺序原本完全由 goroutine 调度决定（随机），
		// 「LLM → 出图 → VLM → 配音 → 出图」这类交替编排最坏情况每个节点都在换侧。
		//
		// 拓扑约束不受影响：同一层内部节点互不依赖，这里改的只是起飞顺序。
		launch := func(ids []string, g *sync.WaitGroup) {
			for _, id := range ids {
				if slots != nil {
					<-slots // 起飞前占令牌（并发已满时在这里排队，不额外起 goroutine）
				}
				g.Add(1)
				go func(nid string) {
					defer g.Done()
					if slots != nil {
						defer func() { slots <- struct{}{} }() // 结束（含断点暂停）即归还
					}
					runNode(nid)
				}(id)
			}
		}

		// 不吃显存的节点（HTTP / 模板 / 数据流等）始终全速并行
		launch(light, &wg)

		if e.sideAwareOff {
			// 回退路径（旧行为）：重任务一次性全起飞，顺序交给 goroutine 调度
			var hwg sync.WaitGroup
			for _, ids := range heavy {
				launch(ids, &hwg)
			}
			hwg.Wait()
		} else {
			// 重任务成批推进：扩散（最重）→ 语言 → 语音，组间设屏障
			for _, s := range sideOrder {
				if len(heavy[s]) == 0 {
					continue
				}
				var hwg sync.WaitGroup
				launch(heavy[s], &hwg)
				hwg.Wait()
			}
		}

		wg.Wait()

		// 单步 / 断点命中：把当前进度落库后正常返回（不是失败）。
		// 调用方从 RunControl.State() 读出停在哪个节点，再决定继续还是中止。
		if rc := runControlFrom(ctx); rc != nil {
			if paused, _, _ := rc.State(); paused {
				if persist != nil {
					persist(nodes)
				}
				return nodes, nil
			}
		}

		if layerFailed {
			for _, id := range order {
				if nodeMap[id].Status == "pending" {
					nodeMap[id].Status = "skipped"
				}
			}
			return nodes, wfErr
		}

		// 扫尾：skipBFS 扣减前驱计数后，可能有节点已经就绪却没被入队
		// （分支汇合、continue_on_error 传播、孤立节点）
		for _, id := range order {
			n := nodeMap[id]
			if n.Status == "pending" && !inNext[id] && remaining[id] <= 0 {
				inNext[id] = true
				next = append(next, id)
			}
		}

		// 每层完成后增量落库（断点续跑依据）
		if persist != nil {
			persist(nodes)
		}

		current = next
	}

	return nodes, nil
}

// NodeExecutor 节点执行函数。ctx 透传 runID（事件归属性）与取消信号。
type NodeExecutor func(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error)

// nodeRegistry 节点类型 → 执行函数。用 map 取代 switch：
// 新增节点类型（插件、测试替身）不必改引擎分发逻辑，engine_v2.go 从此只负责编排。
type nodeRegistry map[string]NodeExecutor

// registerBuiltinNodes 注册全部内置节点类型
func (e *EngineV2) registerBuiltinNodes() {
	// 不需要 ctx 的节点统一包一层，避免逐个给执行函数改签名
	plain := func(fn func(*model.NodeInstance, map[string]interface{}) (interface{}, error)) NodeExecutor {
		return func(_ context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
			return fn(node, upstream)
		}
	}

	e.nodes = nodeRegistry{
		"start": plain(e.execStartNode),
		// 外部文件入口：让「转写一段录音」这类目标在图上能正常表达，
		// 而不是靠"孤立节点 + 参数"绕（模型不认那个写法，标定里 3/10 条就死在这）
		"input_source":  plain(e.execInputSourceNode),
		// 长文本按句/段/固定段数/字数拆开。缺它时 compose 会编造一个 split_text
		// （2026-10-23 标定唯一剩下的失败就是这条）
		"text_split":    plain(e.execTextSplitNode),
		"llm":           e.execLLMNode,
		"text2image":    plain(e.execText2ImageNode),
		"asr":           plain(e.execASRNode),
		"tts":           plain(e.execTTSNode),
		"video_compose": plain(e.execVideoNode),
		// 字幕烧录：把「分镜台词 / 逐行文本 / 现成 srt」变成硬字幕成片。
		// 短视频交付的准入门槛之一，之前 AddSubtitles 写好了却零调用。
		"subtitles": plain(e.execSubtitlesNode),
		// 后期包装：竖屏重构 + BGM 侧链闪避 + 可选字幕，一次重编码
		"post_production": plain(e.execPostProductionNode),
		"if_else":         plain(e.execIfElseNode),
		"http_request":    plain(e.execHTTPNode),
		"data_transform":  plain(e.execTransformNode),
		"rag_query":       plain(e.execRAGNode),
		"sub_workflow":    e.execSubWorkflowNode,
		"loop":            e.execLoopNode,
		"aggregate":       plain(e.execAggregateNode),
		"vision":          e.execVisionNode,
		"select":          e.execSelectNode,
		"prompt_opt":      e.execPromptOptNode,
		"scene_grid":      e.execSceneGridNode,
		"storyboard":      e.execStoryboardNode,
		"shotlist":        e.execShotListNode,
		"quality_gate":    e.execQualityGateNode,
		// 一致性门：像素侧的场景/配色/光比一致性判定（文本侧由 scene_grid 的锁句守）
		"consistency_gate": e.execConsistencyGateNode,
		"agent_task":       e.execAgentTaskNode,
		// 外部 MCP 服务器的一个工具 = 图里一个节点。
		// 「工具只出不进」的缺口在这里补上：别人的 MCP 服务器现在能编进流水线。
		"mcp_call":    e.execMCPCallNode,
		"image2image": e.execImage2ImageNode,
		"inpaint":     e.execInpaintNode,
		"upscale":     e.execUpscaleNode,
		"text2video":  e.execText2VideoNode,
		"end":         plain(e.execEndNode),
		"code":        e.execCodeNode,
		// 本地命令：agent_task 的确定性对偶（agent 改，command 验）。
		// 缺它时"跑一遍测试"只能外挂 HTTP 服务，多一个进程就多一条失败面。
		"command": e.execCommandNode,
		// 导入任意 ComfyUI API 格式工作流（社区几万个 workflow JSON 的接入点）
		"comfy_workflow": plain(e.execComfyWorkflowNode),
	}
}

// executeNode 查注册表执行节点；未注册类型显式报错
func (e *EngineV2) executeNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 懒装配：调用方若用结构体字面量构造（既有测试 / 旧调用路径），
	// 这里补齐注册表，行为与走 NewEngineV2 完全一致。
	// 双重检查 + 复用 runMu：同层节点并行执行时会并发走到这里。
	if e.nodes == nil {
		e.runMu.Lock()
		if e.nodes == nil {
			e.registerBuiltinNodes()
		}
		e.runMu.Unlock()
	}

	if fn, ok := e.lookupNode(node.Type); ok {
		// 输入处理段：所有节点统一在派发前过一遍上游文本。
		// 放在这里而不是逐个节点里做，是因为"哪些节点要吃预处理"这件事
		// 不该由 20 个执行体各答一次——那种写法必然漏，且新节点默认没有。
		cardTraceOf := &cardTrace{}
		upstream = e.applyInputStage(node, upstream, cardTraceOf)

		// 能力卡片：工作流封装卡把**这个节点**替换成一次子工作流调用。
		// 这是唯一能改执行结构的一类卡片；agent/skill 只改提示词，见 execLLMNode。
		pipe := ParsePipeline(node.Data)
		if pipe.Replace != nil {
			wfID, bound := WorkflowIDOfBinding(Binding{Kind: pipe.Replace.Kind, ID: pipe.Replace.ID})
			if bound {
				inner := func(ctx context.Context, n *model.NodeInstance, up map[string]interface{}) (interface{}, error) {
					if e.resolveWorkflow == nil {
						return nil, fmt.Errorf("节点 %s 贴了工作流卡，但引擎没有装配工作流解析器", n.ID)
					}
					if d := e.depth.Add(1); d > maxSubWorkflowDepth {
						e.depth.Add(-1)
						return nil, fmt.Errorf("工作流卡嵌套过深（>%d）：%s", maxSubWorkflowDepth, n.ID)
					}
					defer e.depth.Add(-1)
					out, err := e.executeInnerWorkflow(ctx, wfID, upstreamText(up), false)
					if err != nil {
						return nil, fmt.Errorf("工作流卡 #%d（节点 %s）: %w", wfID, n.ID, err)
					}
					return out, nil
				}
				return inner(ctx, node, upstream)
			}
		}
		// 输出处理段：与输入段对称，统一在派发后做。
		// 只改 map 里的 text 字段，不动其它产物（image / tool_calls / _usage）。
		finish := func(out interface{}, err error) (interface{}, error) {
			if err != nil {
				return out, err
			}
			return e.applyOutputStage(node, out, pipe.Output, cardTraceOf), nil
		}

		// 重节点：局部采样显存峰值，落显存档案（供下次提交前预测）。
		// 只在执行窗口内采样，空闲不烧 CPU（见 vram_sampler.go）。
		if e.profiles != nil && isHeavyNode(node.Type) {
			start := time.Now()
			smp := newVRAMSampler(e.gpuSched)
			out, err := fn(ctx, node, upstream)
			peak, samples := smp.Stop()
			e.recordProfile(node, time.Since(start), peak, samples)
			return finish(out, err)
		}
		return finish(fn(ctx, node, upstream))
	}
	return nil, fmt.Errorf("unknown node type: %s", node.Type)
}

// SetProfiles 注入显存档案仓库（nil 时不做档案记录）
func (e *EngineV2) SetProfiles(p *ProfileStore) { e.profiles = p }

// Profiles 当前档案仓库（可能为 nil）
func (e *EngineV2) Profiles() *ProfileStore { return e.profiles }

// RegisterNodeType 注册自定义节点类型（插件 / 测试替身入口）
func (e *EngineV2) RegisterNodeType(typ string, fn NodeExecutor) {
	if e.nodes == nil {
		e.nodes = make(nodeRegistry)
	}
	e.nodes[typ] = fn
}

// ReplaceHotNodes 整体替换运行期热注册的节点表（插件重载入口）。
//
// 语义是"替换"而不是"合并"：插件被删掉/被停用后，它的节点类型必须跟着消失，
// 否则画布上还能拖出来一个永远报"插件不存在"的节点。所以每次重载传入的是
// **重扫后的完整集合**，不是增量。
func (e *EngineV2) ReplaceHotNodes(nodes map[string]NodeExecutor, sides map[string]string) {
	e.hotMu.Lock()
	defer e.hotMu.Unlock()
	if nodes == nil {
		e.hotNodes = nil
		e.hotSides = nil
		return
	}
	m := make(nodeRegistry, len(nodes))
	for k, v := range nodes {
		m[k] = v
	}
	s := make(map[string]string, len(sides))
	for k, v := range sides {
		s[k] = v
	}
	e.hotNodes = m
	e.hotSides = s
}

// lookupNode 查执行函数：内置优先，其次热注册。
// 内置优先是刻意的——插件不许覆盖内置节点类型去劫持整条流水线。
func (e *EngineV2) lookupNode(typ string) (NodeExecutor, bool) {
	if fn, ok := e.nodes[typ]; ok {
		return fn, true
	}
	e.hotMu.RLock()
	defer e.hotMu.RUnlock()
	fn, ok := e.hotNodes[typ]
	return fn, ok
}

// HotNodeTypes 当前热注册的节点类型（插件面板显示"装了哪些节点"）
func (e *EngineV2) HotNodeTypes() []string {
	e.hotMu.RLock()
	defer e.hotMu.RUnlock()
	out := make([]string, 0, len(e.hotNodes))
	for k := range e.hotNodes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NodeTypes 已注册的节点类型（供能力自检端点下发到前端）
func (e *EngineV2) NodeTypes() []string {
	seen := make(map[string]bool, len(e.nodes))
	out := make([]string, 0, len(e.nodes))
	for k := range e.nodes {
		seen[k] = true
		out = append(out, k)
	}
	e.hotMu.RLock()
	for k := range e.hotNodes {
		if !seen[k] {
			out = append(out, k)
		}
	}
	e.hotMu.RUnlock()
	sort.Strings(out)
	return out
}

// ComposeLLM 暴露一个「温度 0 的单轮对话」给构图用。
//
// 不直接把 e 传出去：构图只需要"给提示词、拿文本"这一个能力，
// 传整个引擎会让调用方顺手干别的事（改图、起跑），边界就糊了。
func (e *EngineV2) ComposeLLM() ComposeLLM {
	return func(prompt string) (string, error) {
		model := ""
		if e.llm != nil {
			model = e.llm.Model
		}
		return e.deterministicChat(model, prompt)
	}
}

// RAGQuery 供 handler 检索经验卡片（构图时作为参考经验）。
// rag 未配置时返回空，不报错 —— 没有经验库也能构图，只是质量差些。
func (e *EngineV2) RAGQuery(query string, topK int) []string {
	if e.rag == nil {
		return nil
	}
	results, err := e.rag.Query(query, topK)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(results))
	for _, r := range results {
		if r.Record.Content != "" {
			out = append(out, r.Record.Content)
		}
	}
	return out
}
