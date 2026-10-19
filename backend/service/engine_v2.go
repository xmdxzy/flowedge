package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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

// nodeTimeoutFor 按节点类型与工作量估算超时。
//
// 固定 180s 在「批量出图」和「分段视频」上会误杀合法任务（实测 5 条提示词
// 需要 ~3 分钟、2 段 H3 视频需要 ~5 分钟）。这里按实际提交量给预算，
// 仍然保留 data.timeout_ms 覆盖，且有 maxNodeTimeout 兜底。
func nodeTimeoutFor(n *model.NodeInstance) time.Duration {
	switch n.Type {
	case "text2video":
		backend, _ := n.Data["backend"].(string)
		if backend == "h3" {
			segs := int(fieldFloat(n, "segments", 1))
			if segs < 1 {
				segs = 1
			}
			return time.Duration(segs) * 12 * time.Minute // 每段约 2–3 分钟，留余量
		}
		return 25 * time.Minute
	case "text2image":
		prompts := len(splitPromptLines(n.Data["prompts"]))
		batch := int(fieldFloat(n, "batch", 1))
		if batch < 1 {
			batch = 1
		}
		n2 := prompts
		if n2 < batch {
			n2 = batch
		}
		if n2 < 1 {
			n2 = 1
		}
		// 首张含模型加载：给 4 分钟；此后每张按 75 秒累计
		return time.Duration(240+n2*75) * time.Second
	case "shotlist":
		// 分镜执行器：预算必须按「镜数 × 帧数」给，不能按固定每镜 3 分钟。
		// 基准是 AGENTS §5.10 实测 0.85–0.93 秒/帧（608×352 / 12 步 / 2 张参考图），
		// 且出现过单段 556s 的异常争用，所以要留余量而不是卡着平均数走。
		shots, totalFrames := 0, 0
		if raw, ok := n.Data["plan"].(string); ok && raw != "" {
			if p, err := ParseShotPlan(raw); err == nil {
				shots = len(p.Shots)
				for _, sh := range p.Shots {
					totalFrames += sh.Frames
				}
			}
		}
		if shots == 0 {
			// 分镜表来自上游节点（storyboard），编译期读不到 → 给类型上限，运行期再细分
			return maxShotlistTimeout
		}
		return ShotListBudget(shots, totalFrames)
	case "storyboard":
		return 5 * time.Minute // LLM 规划 + JSON 校验
	case "image2image", "inpaint", "upscale":
		return 10 * time.Minute
	case "asr":
		return 15 * time.Minute // 长音频分段转写
	}
	return defaultNodeTimeout
}

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
func classifyError(err error) string {	if err == nil {
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
	llm             *LLMService
	llmStream       *LLMStreamService
	t2i             *Text2ImageService
	asr             *ASRService
	tts             *TTSService
	video           *VideoService
	http            *HTTPService
	rag             *RAGServiceV3
	gpuSched        *GPUScheduler
	wsHub           *WSHub
	tools           *ToolExecutor
	cache           *NodeCache
	eventLog        *EventLogger
	shotJobs        *ShotJobStore    // 镜头级作业（可 nil：无库时降级为不落盘）
	creds           *CredentialStore // 凭据仓库（HTTP 节点按名称取密钥）
	visionModel     string           // 自动探测到的本地视觉模型（vision 节点留空时使用）
	vram            *vramArbiter     // 显存仲裁：同一时刻只跑一个重任务
	deps            *Supervisor      // 依赖进程托管（H3 需要按需切换实例）
	nodes           nodeRegistry     // 节点类型注册表（内置 15 种 + 可注册自定义）
	runMu           sync.Mutex
	activeRuns      map[uint]context.CancelFunc        // runID -> 取消函数（支持手动中止）
	resolveWorkflow func(uint) ([]byte, []byte, error) // 子工作流解析器
	depth           atomic.Int32                       // 当前嵌套深度
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

// SetEventLog 注入事件日志（nil 表示只广播不落库）
func (e *EngineV2) SetEventLog(l *EventLogger) { e.eventLog = l }

// SetCredentials 注入凭据仓库（HTTP 节点 credential 字段生效的前提）
func (e *EngineV2) SetCredentials(c *CredentialStore) { e.creds = c }

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

		// 同一层内所有节点并行执行
		for _, id := range current {
			wg.Add(1)
			go func(nid string) {
				defer wg.Done()

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
				limit := maxNodeTimeout
				if node.Type == "shotlist" {
					limit = maxShotlistTimeout
				}
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
							Data: map[string]interface{}{"output": cached, "cache_hit": true}})
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

				if execErr != nil {
					node.Status = "failed"
					node.Error = execErr.Error()
					node.ErrorCode = classifyError(execErr)
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
						e.emit(runID, Event{Type: "node_failed", NodeID: nid, Message: execErr.Error()})
						return
					}

					layerMu.Lock()
					layerFailed = true
					layerMu.Unlock()
					markFailed(nid, execErr.Error())
					mu.Unlock()
					e.emit(runID, Event{Type: "node_failed", NodeID: nid, Message: execErr.Error()})
					return
				}

				node.Status = "success"
				node.Output = out
				runCtx[nid] = out // 供下游节点以 {{nid.field}} 引用
				if e.cache != nil && cacheKeyStr != "" {
					e.cache.Put(cacheKeyStr, out)
				}
				mu.Unlock()
				e.emit(runID, Event{Type: "node_finished", NodeID: nid, Data: map[string]interface{}{"output": out}})

				// 处理出边（if_else 需按命中分支裁剪未激活侧）
				branchPath := ""
				if node.Type == "if_else" {
					b, _ := out.(map[string]interface{})
					branchPath, _ = b["branch"].(string)
				}
				mu.Lock()
				propagate(nid, out, branchPath)
				mu.Unlock()
			}(id)
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
		"start":          plain(e.execStartNode),
		"llm":            e.execLLMNode,
		"text2image":     plain(e.execText2ImageNode),
		"asr":            plain(e.execASRNode),
		"tts":            plain(e.execTTSNode),
		"video_compose":  plain(e.execVideoNode),
		"if_else":        plain(e.execIfElseNode),
		"http_request":   plain(e.execHTTPNode),
		"data_transform": plain(e.execTransformNode),
		"rag_query":      plain(e.execRAGNode),
		"sub_workflow":   e.execSubWorkflowNode,
		"loop":           e.execLoopNode,
		"aggregate":      plain(e.execAggregateNode),
		"vision":         e.execVisionNode,
		"select":         e.execSelectNode,
		"prompt_opt":     e.execPromptOptNode,
		"storyboard":     e.execStoryboardNode,
		"shotlist":       e.execShotListNode,
		"image2image":    e.execImage2ImageNode,
		"inpaint":        e.execInpaintNode,
		"upscale":        e.execUpscaleNode,
		"text2video":     e.execText2VideoNode,
		"end":            plain(e.execEndNode),
		"code":           e.execCodeNode,
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

	if fn, ok := e.nodes[node.Type]; ok {
		return fn(ctx, node, upstream)
	}
	return nil, fmt.Errorf("unknown node type: %s", node.Type)
}

// RegisterNodeType 注册自定义节点类型（插件 / 测试替身入口）
func (e *EngineV2) RegisterNodeType(typ string, fn NodeExecutor) {
	if e.nodes == nil {
		e.nodes = make(nodeRegistry)
	}
	e.nodes[typ] = fn
}

// NodeTypes 已注册的节点类型（供能力自检端点下发到前端）
func (e *EngineV2) NodeTypes() []string {
	out := make([]string, 0, len(e.nodes))
	for k := range e.nodes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ===== 各节点执行逻辑 =====

func (e *EngineV2) execStartNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if prompt, ok := node.Data["prompt"].(string); ok && prompt != "" {
		return map[string]interface{}{"text": prompt}, nil
	}
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				return map[string]interface{}{"text": text}, nil
			}
		}
	}
	return map[string]interface{}{"text": ""}, nil
}

func (e *EngineV2) execLLMNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// LLM 与扩散模型共用 24GB：换侧时让扩散侧先释放
	release := e.enterSide(sideLanguage)
	defer release()

	// 节点级提示词 = 指令前缀；上游文本 = 输入主体。两者拼接，缺一不可丢
	runID := runIDFrom(ctx)
	instruction := ""
	if p, ok := node.Data["prompt"].(string); ok {
		instruction = p
	}

	upstreamText := ""
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok && text != "" {
				upstreamText = text
			}
		}
	}

	prompt := upstreamText
	if instruction != "" {
		if upstreamText != "" {
			prompt = instruction + "\n\n" + upstreamText
		} else {
			prompt = instruction
		}
	}

	if prompt == "" {
		return nil, fmt.Errorf("LLM node has no input prompt")
	}

	// 节点级模型覆盖（空 = 使用服务级默认模型）
	reqModel, _ := node.Data["model"].(string)

	// 显存调度：请求加载 LLM
	if e.gpuSched != nil {
		schedName := reqModel
		if schedName == "" {
			schedName = "qwen2.5:7b"
		}
		_ = e.gpuSched.AcquireModel(schedName)
		defer e.gpuSched.ReleaseModel(schedName)
	}

	// 工具调用：节点声明了 tools 时走「工具轮（非流式）+ 最终答案轮（流式）」
	if toolNames := nodeToolNames(node); len(toolNames) > 0 && e.llm != nil {
		return e.execLLMWithTools(ctx, node, prompt, reqModel, toolNames)
	}

	// 流式优先：token 经 WS 实时推送前端（打字机效果）
	if e.llmStream != nil {
		ch, err := e.llmStream.ChatStreamWithModel(prompt, reqModel)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		usage := LLMUsage{}
		for chunk := range ch {
			if chunk.Done {
				// Done 块携带本次请求的完整 token 用量
				usage = chunk.Usage
				break
			}
			sb.WriteString(chunk.Content)
			e.emit(runID, Event{Type: "node_token", NodeID: node.ID, Data: map[string]interface{}{"delta": chunk.Content}})
		}
		out := map[string]interface{}{"text": sb.String()}
		if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
			out["_usage"] = usage // 供事件日志汇总 token 消耗
		}
		return out, nil
	}

	result, u, err := e.llm.ChatWithModelUsage(prompt, reqModel)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"text": result}
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		out["_usage"] = u
	}
	return out, nil
}

// nodeToolNames 读取节点声明的工具名列表（data.tools，支持数组或逗号串）
func nodeToolNames(node *model.NodeInstance) []string {
	raw, ok := node.Data["tools"]
	if !ok {
		return nil
	}
	var names []string
	switch v := raw.(type) {
	case []interface{}:
		for _, it := range v {
			if str, ok := it.(string); ok && str != "" {
				names = append(names, str)
			}
		}
	case []string:
		for _, str := range v {
			if str != "" {
				names = append(names, str)
			}
		}
	case string:
		for _, part := range strings.Split(v, ",") {
			if t := strings.TrimSpace(part); t != "" {
				names = append(names, t)
			}
		}
	}
	return names
}

// execLLMWithTools 工具调用循环：
//  1. 带 tools 声明请求模型（非流式，需要完整 tool_calls）
//  2. 模型要工具 → 本地执行 → 结果回填 messages → 再请求
//  3. 模型给出最终答案 → 流式重放一次以保留打字机效果
//
// 轮次上限 4，防止模型无限索要工具
func (e *EngineV2) execLLMWithTools(ctx context.Context, node *model.NodeInstance, prompt, reqModel string, toolNames []string) (interface{}, error) {
	const maxRounds = 4
	runID := runIDFrom(ctx)
	usage := LLMUsage{} // 累计所有工具轮与最终答案轮的 token 消耗

	if e.tools == nil {
		e.tools = NewToolExecutor(e.http, e.rag)
	}
	specs := e.tools.Specs(toolNames)
	if len(specs) == 0 {
		return nil, fmt.Errorf("LLM node declares tools %v but none are known", toolNames)
	}

	messages := []toolChatMessage{{Role: "user", Content: prompt}}
	trace := make([]map[string]interface{}, 0, 4)

	for round := 0; round < maxRounds; round++ {
		msg, u, err := e.llm.ChatWithToolsUsage(messages, specs, reqModel)
		if err != nil {
			return nil, err
		}
		usage = usage.add(u)

		calls := msg.ToolCalls
		textMode := false
		if len(calls) == 0 {
			// 兼容不支持 tool_calls 字段的模型（如 qwen2.5 会把调用写成 content 里的 JSON）
			if parsed := parseTextToolCalls(msg.Content, specs); len(parsed) > 0 {
				calls = parsed
				textMode = true
			} else {
				final := msg.Content
				if e.llmStream != nil {
					messages = append(messages, toolChatMessage{Role: "assistant", Content: msg.Content})
					if streamed, answerUsage, serr := e.streamFinalAnswer(ctx, node, messages, reqModel); serr == nil && streamed != "" {
						final = streamed
						usage = usage.add(answerUsage)
					}
				}
				out := map[string]interface{}{"text": final}
				if len(trace) > 0 {
					out["tool_calls"] = trace
					out["tool_rounds"] = len(trace)
				}
				if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
					out["_usage"] = usage
				}
				return out, nil
			}
		}

		// 标准 tool_calls：assistant 消息带工具请求入历史
		// 文本形态：把模型原始输出作为 assistant 内容，工具结果用 user 消息回填（模型无关）
		if textMode {
			messages = append(messages, toolChatMessage{Role: "assistant", Content: msg.Content})
		} else {
			messages = append(messages, *msg)
		}

		for _, tc := range calls {
			e.emit(runID, Event{
				Type:   "tool_call",
				NodeID: node.ID,
				Data:   map[string]interface{}{"tool": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
			result, runErr := e.tools.Run(tc.Function.Name, tc.Function.Arguments)
			status := "ok"
			if runErr != nil {
				result = "工具执行失败: " + runErr.Error()
				status = "error"
			}
			rec := map[string]interface{}{
				"tool":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"result":    truncateRunes(result, 500),
				"status":    status,
			}
			if textMode {
				rec["mode"] = "text"
			}
			trace = append(trace, rec)

			if textMode {
				messages = append(messages, toolChatMessage{
					Role: "user",
					Content: "工具 " + tc.Function.Name + " 的执行结果：\n" + result +
						"\n\n请基于这个结果直接给出最终回答，不要再输出工具调用的 JSON。",
				})
			} else {
				messages = append(messages, toolChatMessage{Role: "tool", ToolCallID: tc.ID, Content: result})
			}
		}
	}

	return nil, fmt.Errorf("tool calling exceeded %d rounds", maxRounds)
}

// streamFinalAnswer 用完整消息历史再做一次流式补全（保留打字机效果）
func (e *EngineV2) streamFinalAnswer(ctx context.Context, node *model.NodeInstance, messages []toolChatMessage, reqModel string) (string, LLMUsage, error) {
	runID := runIDFrom(ctx)
	ch, err := e.llmStream.ChatStreamMessages(messages, reqModel)
	if err != nil {
		return "", LLMUsage{}, err
	}
	var sb strings.Builder
	usage := LLMUsage{}
	for chunk := range ch {
		if chunk.Done {
			usage = chunk.Usage
			break
		}
		sb.WriteString(chunk.Content)
		e.emit(runID, Event{Type: "node_token", NodeID: node.ID, Data: map[string]interface{}{"delta": chunk.Content}})
	}
	return sb.String(), usage, nil
}

// splitPromptLines 把「一行一条」的批量提示词拆成切片
func splitPromptLines(v interface{}) []string {
	raw, ok := v.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (e *EngineV2) execText2ImageNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 24GB 上不能同时养 flux 与 14B LLM：换侧（清语言侧）并独占执行
	release := e.enterSide(sideDiffusion)
	defer release()
	// H3 实例在跑时默认实例是停的，出图前自动切回（已健康时是空操作）
	if err := e.ensureComfy(""); err != nil {
		return nil, err
	}

	// 显存调度：flux 与 LLM 抢同一块 24GB 显存，不纳入调度就会出现
	// 「LLM 已加载 + 直接起 flux」→ OOM。此前只有 LLM 节点走调度。
	if e.gpuSched != nil && e.t2i != nil && e.t2i.Checkpoint != "" {
		// 串行门：同层多个文生图节点不会同时进 ComfyUI（并行只会互相 OOM）
		e.gpuSched.LockText2Image()
		defer e.gpuSched.UnlockText2Image()

		_ = e.gpuSched.AcquireModel(e.t2i.Checkpoint)
		defer e.gpuSched.ReleaseModel(e.t2i.Checkpoint)
	}

	prompt := ""
	if p, ok := node.Data["prompt"].(string); ok {
		prompt = p
	}

	if prompt == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					prompt = text
				}
			}
		}
	}

	if prompt == "" {
		return nil, fmt.Errorf("text2image node has no prompt")
	}

	width := 1024
	height := 1024
	if w, ok := node.Data["width"].(float64); ok {
		width = int(w)
	}
	if h, ok := node.Data["height"].(float64); ok {
		height = int(h)
	}

	// 多提示词批量（分镜 / 角色表 / 表情包）：
	//   prompts 字段一行一条；或勾了 split_lines 时把上游整段文本按行拆。
	// 关键收益不是并行，而是**只加载一次 17GB 模型**——逐条跑每条都要重载。
	prompts := splitPromptLines(node.Data["prompts"])
	if len(prompts) < 2 {
		if v, ok := node.Data["split_lines"].(bool); ok && v {
			prompts = splitPromptLines(prompt)
		}
	}
	if len(prompts) > 1 {
		if len(prompts) > 12 {
			prompts = prompts[:12] // 单次图上限：再多只会把排队时间拉长，收益不再增长
		}
		paths, err := e.t2i.GenerateImagesFromPrompts(prompts, width, height)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"text":    prompts[0],
			"image":   paths[0],
			"images":  paths,
			"count":   len(paths),
			"prompts": prompts,
		}, nil
	}

	// 批量：一次提交出 n 张，共用同一次模型加载（省掉 n 次冷启动）。
	// 输出同时给 image（第一张）与 images（全部），旧工作流不用改。
	batch := 1
	if v, ok := node.Data["batch"].(float64); ok && int(v) > 1 {
		batch = int(v)
		if batch > 8 {
			batch = 8 // 24GB 上一次 8 张已经够用，再多只会把显存与时间拉满
		}
	}

	if batch > 1 {
		paths, err := e.t2i.GenerateImages(prompt, width, height, batch)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			return nil, fmt.Errorf("批量出图没有产出")
		}
		return map[string]interface{}{
			"text":   prompt,
			"image":  paths[0],
			"images": paths,
			"count":  len(paths),
		}, nil
	}

	imagePath, err := e.t2i.GenerateImage(prompt, width, height)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"text":  prompt,
		"image": imagePath,
	}, nil
}

func (e *EngineV2) execASRNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	audioPath := ""
	if p, ok := node.Data["audio_path"].(string); ok {
		audioPath = p
	}

	if audioPath == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if audio, ok := m["audio"].(string); ok {
					audioPath = audio
				}
			}
		}
	}

	if audioPath == "" {
		return nil, fmt.Errorf("asr node has no audio input")
	}

	release := e.enterSide(sideAudio)
	defer release()

	text, err := e.asr.Transcribe(audioPath)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{"text": text}, nil
}

func (e *EngineV2) execTTSNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	text := ""
	if p, ok := node.Data["text"].(string); ok {
		text = p
	}

	if text == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					text = t
				}
			}
		}
	}

	if text == "" {
		return nil, fmt.Errorf("tts node has no text input")
	}

	outputPath := fmt.Sprintf("data/generated/tts-%d.wav", time.Now().UnixNano())
	if p, ok := node.Data["output_path"].(string); ok && p != "" {
		outputPath = p
	}

	// 语音名 / 语速：Windows 内置 SAPI 直接支持；HTTP 服务端可选支持
	voice, _ := node.Data["voice"].(string)
	rate := 0
	if v, ok := node.Data["rate"].(float64); ok {
		rate = int(v)
	}

	release := e.enterSide(sideAudio)
	defer release()

	backend, err := e.tts.SynthesizeWith(text, outputPath, TTSOptions{Voice: voice, Rate: rate})
	if err != nil {
		return nil, err
	}
	// 统一输出绝对路径：下游节点与画廊复制粘贴都能直接用，不依赖进程 CWD
	if abs, absErr := filepath.Abs(outputPath); absErr == nil {
		outputPath = abs
	}

	return map[string]interface{}{
		"text":    text,
		"audio":   outputPath,
		"backend": backend,
	}, nil
}

func (e *EngineV2) execVideoNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	var images []string
	audioPath := ""

	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if img, ok := m["image"].(string); ok {
				images = append(images, img)
			}
			if audio, ok := m["audio"].(string); ok {
				audioPath = audio
			}
		}
	}

	if len(images) == 0 {
		return nil, fmt.Errorf("video compose node has no image input")
	}

	outputPath := fmt.Sprintf("data/generated/video-%d.mp4", time.Now().UnixNano())
	if p, ok := node.Data["output_path"].(string); ok && p != "" {
		outputPath = p
	}

	// 每张图轮播时长（秒），默认 3；上限由 ComposeVideoMulti 钳制
	perImage := 3.0
	if v, ok := node.Data["image_duration"].(float64); ok && v > 0 {
		perImage = v
	}

	err := e.video.ComposeVideoMulti(images, audioPath, outputPath, perImage)
	if err != nil {
		return nil, err
	}
	if abs, absErr := filepath.Abs(outputPath); absErr == nil {
		outputPath = abs
	}

	return map[string]interface{}{
		"video": outputPath,
	}, nil
}

func (e *EngineV2) execIfElseNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 获取上游文本
	inputText := ""
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				inputText = text
			}
		}
	}

	// 条件配置
	conditionType, _ := node.Data["condition_type"].(string)
	compareValue, _ := node.Data["compare_value"].(string)

	branch := "false"
	if matchCondition(conditionType, inputText, compareValue) {
		branch = "true"
	}

	return map[string]interface{}{
		"text":   inputText,
		"branch": branch,
	}, nil
}

// matchCondition 通用条件判定，if_else 分支与 loop 的 until 终止共用
func matchCondition(condType, text, compareValue string) bool {
	switch condType {
	case "contains":
		return strings.Contains(text, compareValue)
	case "not_contains":
		return !strings.Contains(text, compareValue)
	case "not_empty":
		return strings.TrimSpace(text) != ""
	case "empty":
		return strings.TrimSpace(text) == ""
	case "equals":
		return text == compareValue
	}
	return false
}

func (e *EngineV2) execEndNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	for _, out := range upstream {
		return out, nil
	}
	return map[string]interface{}{"text": "done"}, nil
}

func (e *EngineV2) execHTTPNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	method, _ := node.Data["method"].(string)
	if method == "" {
		method = "GET"
	}

	url, _ := node.Data["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("http node has no url")
	}

	body, _ := node.Data["body"].(string)
	headers := make(map[string]string)
	if h, ok := node.Data["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			headers[k] = fmt.Sprintf("%v", v)
		}
	}

	// 凭据注入：节点只写凭据名，明文 header 不落在工作流 JSON 里
	credName, _ := node.Data["credential"].(string)
	if credName != "" {
		if e.creds == nil {
			return nil, fmt.Errorf("http node 引用了凭据 %q，但凭据仓库未初始化", credName)
		}
		credHeaders, credQuery, err := e.creds.Resolve(credName)
		if err != nil {
			return nil, err
		}
		for k, v := range credHeaders {
			// 节点显式写的 header 优先，便于临时覆盖
			if _, exists := headers[k]; !exists {
				headers[k] = v
			}
		}
		if len(credQuery) > 0 {
			url = appendQuery(url, credQuery)
		}
	}

	respBody, statusCode, err := e.http.HTTPRequest(method, url, headers, body)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"text":        respBody,
		"status_code": statusCode,
	}, nil
}

func (e *EngineV2) execTransformNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	inputText := upstreamText(upstream)

	template, _ := node.Data["template"].(string)
	if template == "" {
		return map[string]interface{}{"text": inputText}, nil
	}

	// 输入是合法 JSON 时按对象处理（支持 {{a.b}}），否则只替换 {{input}}
	var data interface{} = inputText
	if m, err := parseJSONObject(inputText); err == nil {
		m["input"] = inputText
		data = m
	}

	return map[string]interface{}{"text": replacePlaceholders(template, data)}, nil
}

// parseJSONObject 尝试把文本解析为 JSON 对象
func parseJSONObject(s string) (map[string]interface{}, error) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (e *EngineV2) execRAGNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 获取查询文本
	query := ""
	if q, ok := node.Data["query"].(string); ok {
		query = q
	}

	if query == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					query = text
				}
			}
		}
	}

	if query == "" {
		return nil, fmt.Errorf("rag node has no query")
	}

	// 检索知识库（top_k 可由节点数据配置，默认 3）
	topK := 3
	if tk, ok := node.Data["top_k"].(float64); ok && tk > 0 {
		topK = int(tk)
	}
	// 检索一次，同时产出「文本上下文」与「结构化引用」
	// （此前 GetContext 内部会再检索一次，等于每次 RAG 节点多付一轮 embedding）
	results, err := e.rag.Query(query, topK)
	if err != nil {
		return nil, err
	}
	context := e.rag.ContextFrom(results)

	// 结构化引用：下游可写 {{rag-1.sources[0].content}} 精确取某一条，
	// 也能据此在 UI 上做点击跳转溯源
	sources := make([]interface{}, 0, len(results))
	for _, r := range results {
		idx := 0
		if v, ok := r.Record.Metadata["chunk_index"].(float64); ok {
			idx = int(v)
		}
		sources = append(sources, map[string]interface{}{
			"source":      r.Record.Source,
			"chunk_index": idx,
			"score":       r.Score,
			"content":     r.Record.Content,
		})
	}

	// 拼接上下文和查询
	fullPrompt := query
	if context != "" {
		fullPrompt = fmt.Sprintf("参考以下资料回答问题：\n\n%s\n\n问题：%s", context, query)
	}

	return map[string]interface{}{
		"text":    fullPrompt,
		"context": context,
		"sources": sources,
		"hits":    len(results),
	}, nil
}

// executeInnerWorkflow 解析并内联执行一个内层工作流
// injectText 非空时注入内层 start：overwrite=true 强制覆盖（循环每轮迭代用），
// false 仅在 start 未配置 prompt 时注入（子工作流节点语义）
// executeInnerWorkflow 解析并内联执行一个内层工作流。
// ctx 透传使内层节点的事件也归属同一次运行（否则子工作流执行过程不可追溯）。
func (e *EngineV2) executeInnerWorkflow(ctx context.Context, wfID uint, injectText string, overwrite bool) (interface{}, error) {
	nodesJSON, edgesJSON, err := e.resolveWorkflow(wfID)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow %d failed: %w", wfID, err)
	}

	var sub []model.NodeInstance
	if err := json.Unmarshal(nodesJSON, &sub); err != nil {
		return nil, fmt.Errorf("parse workflow %d nodes failed: %w", wfID, err)
	}

	if injectText != "" && len(sub) > 0 && sub[0].Type == "start" {
		if cur, _ := sub[0].Data["prompt"].(string); overwrite || cur == "" {
			sub[0].Data["prompt"] = injectText
		}
	}

	subNodesJSON, err := json.Marshal(sub)
	if err != nil {
		return nil, err
	}

	states, err := e.runWithContext(ctx, subNodesJSON, edgesJSON, nil, "", nil)
	if err != nil {
		return nil, fmt.Errorf("workflow %d failed: %w", wfID, err)
	}

	// 返回内层 end 节点输出；缺失时退化为最后一个节点输出
	for i := range states {
		if states[i].Type == "end" && states[i].Status == "success" {
			return states[i].Output, nil
		}
	}
	if len(states) > 0 {
		return states[len(states)-1].Output, nil
	}
	return map[string]interface{}{"text": ""}, nil
}

// execSubWorkflowNode 子工作流：解析 workflow_id → 注入上游文本到内层 start → 嵌套执行 → 返回内层 end 输出
func (e *EngineV2) execSubWorkflowNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if e.resolveWorkflow == nil {
		return nil, fmt.Errorf("sub_workflow resolver not configured")
	}

	wfID := uint(dataInt(node.Data, "workflow_id", 0))
	if wfID == 0 {
		return nil, fmt.Errorf("sub_workflow node has no workflow_id")
	}

	if d := e.depth.Add(1); d > maxSubWorkflowDepth {
		e.depth.Add(-1)
		return nil, fmt.Errorf("sub_workflow nesting too deep (>%d)", maxSubWorkflowDepth)
	}
	defer e.depth.Add(-1)

	out, err := e.executeInnerWorkflow(ctx, wfID, upstreamText(upstream), false)
	if err != nil {
		return nil, fmt.Errorf("sub workflow %d: %w", wfID, err)
	}
	return out, nil
}

// dataInt 从节点数据里取整数（JSON 数字是 float64，也兼容 int）
func dataInt(data map[string]interface{}, key string, def int) int {
	switch v := data[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

// upstreamText 取上游输出里第一个非空 text
func upstreamText(upstream map[string]interface{}) string {
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if t, ok := m["text"].(string); ok && t != "" {
				return t
			}
		}
		if s, ok := out.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// execLoopNode 循环节点：迭代执行内层工作流
//
//	mode=count  固定次数（iterations），每轮以上一轮输出为输入（链式精炼）
//	mode=foreach 遍历 items（或按 separator 切分上游文本），逐项输入
//	mode=until  至少跑一轮，直到 stop_condition 命中后停止
//
// 安全阀：max_iterations（默认 20，硬上限 100）
func (e *EngineV2) execLoopNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	runID := runIDFrom(ctx)
	if e.resolveWorkflow == nil {
		return nil, fmt.Errorf("loop resolver not configured")
	}

	wfID := uint(dataInt(node.Data, "workflow_id", 0))
	if wfID == 0 {
		return nil, fmt.Errorf("loop node has no workflow_id")
	}

	mode, _ := node.Data["mode"].(string)
	if mode == "" {
		mode = "count"
	}

	maxIter := dataInt(node.Data, "max_iterations", 20)
	if maxIter <= 0 {
		maxIter = 20
	}
	if maxIter > maxLoopIterations {
		maxIter = maxLoopIterations
	}

	if d := e.depth.Add(1); d > maxSubWorkflowDepth {
		e.depth.Add(-1)
		return nil, fmt.Errorf("loop nesting too deep (>%d)", maxSubWorkflowDepth)
	}
	defer e.depth.Add(-1)

	seed := upstreamText(upstream)
	if v, ok := node.Data["input"].(string); ok && v != "" {
		seed = v
	}

	// foreach：优先显式 items 数组，其次按分隔符切分上游文本
	var items []string
	if mode == "foreach" {
		if arr, ok := node.Data["items"].([]interface{}); ok {
			for _, it := range arr {
				items = append(items, fmt.Sprintf("%v", it))
			}
		}
		if len(items) == 0 {
			sep, _ := node.Data["separator"].(string)
			if sep == "" {
				sep = "\n"
			}
			for _, line := range strings.Split(seed, sep) {
				if t := strings.TrimSpace(line); t != "" {
					items = append(items, t)
				}
			}
		}
		if len(items) == 0 {
			return map[string]interface{}{
				"text":       "",
				"iterations": 0,
				"stopped_by": "empty_items",
				"items":      []interface{}{},
			}, nil
		}
		if len(items) > maxIter {
			items = items[:maxIter]
		}
	}

	condType, _ := node.Data["condition_type"].(string)
	condValue, _ := node.Data["compare_value"].(string)
	if condType == "" {
		condType = "not_empty"
	}

	current := seed
	var texts []string
	records := make([]interface{}, 0, maxIter)
	stoppedBy := ""
	iter := 0

	for {
		if iter >= maxIter {
			stoppedBy = "max_iterations"
			break
		}
		if mode == "foreach" && iter >= len(items) {
			stoppedBy = "exhausted"
			break
		}

		input := current
		if mode == "foreach" {
			input = items[iter]
		}

		out, err := e.executeInnerWorkflow(ctx, wfID, input, true)
		if err != nil {
			return nil, fmt.Errorf("loop iteration %d failed: %w", iter+1, err)
		}

		text := ""
		if m, ok := out.(map[string]interface{}); ok {
			text, _ = m["text"].(string)
		}
		texts = append(texts, text)
		records = append(records, map[string]interface{}{
			"index":  iter + 1,
			"input":  input,
			"output": text,
		})
		iter++

		// 每轮广播进度（低频事件，前端可显示迭代计数）
		e.emit(runID, Event{
			Type:   "loop_iteration",
			NodeID: node.ID,
			Data: map[string]interface{}{
				"iteration": iter,
				"max":       maxIter,
				"output":    truncateRunes(text, 120),
			},
		})
		if mode == "until" && matchCondition(condType, text, condValue) {
			stoppedBy = "condition"
			break
		}

		current = text
		if mode == "count" {
			// count 模式按 iterations 指定的次数跑
			want := dataInt(node.Data, "iterations", 3)
			if want <= 0 {
				want = 3
			}
			if want > maxIter {
				want = maxIter
			}
			if iter >= want {
				stoppedBy = "completed"
				break
			}
		}
	}

	last := seed
	if len(texts) > 0 {
		last = texts[len(texts)-1]
	}

	return map[string]interface{}{
		"text":       last,
		"all":        strings.Join(texts, "\n---\n"),
		"iterations": iter,
		"stopped_by": stoppedBy,
		"items":      records,
	}, nil
}

// truncateRunes 按字符截断（避免切坏多字节）
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
