package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

type WorkflowHandler struct {
	db      *gorm.DB
	engine  *service.EngineV2
	queue   *service.JobQueue     // nil 时退化为直接 goroutine 起跑（保持向后兼容）
	lineage *service.LineageStore // nil 时跳过血缘记录
	// 预检依赖（SetPreflight 注入；nil 时跳过运行前依赖预检）
	runs *service.RunService // 续跑编排（含单镜重跑），与 HTTP 无关

	sup         *service.Supervisor
	ollamaBase  string
	comfyBase   string
	comfyH3Base string
	asrBase     string
	ttsBase     string

	// 资源准入闸（nil 时跳过：离线单测、无治理器的环境）
	governor *service.GovernorGate
}

func NewWorkflowHandler(db *gorm.DB, engine *service.EngineV2, queue *service.JobQueue) *WorkflowHandler {
	return &WorkflowHandler{
		db:     db,
		engine: engine,
		queue:  queue,
		runs:   service.NewRunService(db, engine, queue),
	}
}

// SetGovernor 注入资源准入闸。
//
// 传 nil = 不启用（默认行为与接入前完全一致，保证向后兼容）。
// 注意它的语义是 fail-open：问不到治理器就放行，只有治理器明确说不才拦。
func (h *WorkflowHandler) SetGovernor(g *service.GovernorGate) {
	h.governor = g
}

// SetLineage 注入血缘仓库（运行结束后记录产物来源）
func (h *WorkflowHandler) SetLineage(l *service.LineageStore) {
	h.lineage = l
}

// SetPreflight 注入运行前依赖预检所需的探测器与端点。
// 预检意义：服务没起时任务在入口处显式失败并说明「缺什么、怎么起」，
// 而不是让节点跑进去再报一句连接错误（2026-10-20 执行记录审计：
// 17 次 failed 里 5 次是这类，用户看到的是「工作流失败」而非「TTS 没起」）。
func (h *WorkflowHandler) SetPreflight(sup *service.Supervisor, ollamaBase, comfyBase, comfyH3Base, asrBase, ttsBase string) {
	h.sup = sup
	h.ollamaBase = ollamaBase
	h.comfyBase = comfyBase
	h.comfyH3Base = comfyH3Base
	h.asrBase = asrBase
	h.ttsBase = ttsBase
}

// preflight 现场探测依赖并核对节点需求。sup 未注入时直接放行（向后兼容）。
func (h *WorkflowHandler) preflight(nodesJSON, edgesJSON []byte) []service.DepRequirement {
	if h.sup == nil {
		return nil
	}
	nodes, err := service.ParseNodeInstances(nodesJSON)
	if err != nil {
		return nil // 解析失败交给执行层报，预检不重复拦
	}
	caps := service.ProbeCapabilities(nil, "", h.ollamaBase, h.comfyBase, h.comfyH3Base, h.asrBase, h.ttsBase)
	return service.PreflightDeps(nodes, caps, h.sup)
}

// preflightErrorText 把缺失清单压成一行错误文本（run 记录的 error 字段用）
//
// 每项后面带上修复动作：只说「缺 TTS」等于让用户自己去猜去哪儿点，
// 而依赖面板/一键修复路径是现成的，把那句话直接递过去。
func preflightErrorText(missing []service.DepRequirement) string {
	parts := make([]string, 0, len(missing))
	for _, m := range missing {
		item := fmt.Sprintf("%s（节点 %s）", m.Label, m.NodeID)
		if m.Fix != "" {
			item += " → " + m.Fix
		}
		parts = append(parts, item)
	}
	return "依赖服务未就绪，工作流未开始执行：" + strings.Join(parts, "、")
}

// PreflightCheck 只体检不运行：把当前画布上的节点清单送进来，
// 返回缺失依赖 + 每项的可执行修复动作（含能否一键拉起）。
//
// 与运行时预检用同一套判定（同一份 PreflightDeps），所以「体检说 OK」和
// 「运行不报错」不会出现两套结论。
func (h *WorkflowHandler) PreflightCheck(c *gin.Context) {
	var body struct {
		Nodes json.RawMessage `json:"nodes"`
		Edges json.RawMessage `json:"edges"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体需要 nodes / edges"})
		return
	}
	if len(body.Nodes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nodes 为空"})
		return
	}
	edges := body.Edges
	if len(edges) == 0 {
		edges = json.RawMessage("[]")
	}
	missing := h.preflight([]byte(body.Nodes), []byte(edges))
	fixable := 0
	for _, m := range missing {
		if m.Fixable {
			fixable++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":      len(missing) == 0,
		"missing": missing,
		"count":   len(missing),
		"fixable": fixable,
	})
}

// List 获取所有工作流
func (h *WorkflowHandler) List(c *gin.Context) {
	var workflows []model.Workflow
	h.db.Order("updated_at DESC").Find(&workflows)
	c.JSON(http.StatusOK, workflows)
}

// Get 获取单个工作流
func (h *WorkflowHandler) Get(c *gin.Context) {
	id := c.Param("id")
	var workflow model.Workflow
	if err := h.db.First(&workflow, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	c.JSON(http.StatusOK, workflow)
}

// validateGraph 保存期图结构校验：阻止把非法 DAG 存进库。
// 返回 (0, nil) 表示通过；否则返回 HTTP 状态码与错误体。
// 只阻断 error 级问题（环 / 自环 / 悬空边），warn 级（重复边、孤立节点）随响应一并返回供前端提示。
func validateGraph(nodes, edges json.RawMessage) (int, gin.H) {
	issues, err := service.ValidateDAGJSON(nodes, edges)
	if err != nil {
		return http.StatusBadRequest, gin.H{"error": err.Error()}
	}
	blocking := make([]service.DAGIssue, 0)
	for _, i := range issues {
		if i.Level == "error" {
			blocking = append(blocking, i)
		}
	}
	if len(blocking) > 0 {
		return http.StatusBadRequest, gin.H{
			"error":  blocking[0].Message,
			"code":   string(blocking[0].Code),
			"issues": issues,
		}
	}
	return 0, nil
}

// validateNodeParams 保存期节点参数校验：缺 prompt / 缺输入图这类
// 「静态可判定的必然失败」不许存进库（2026-10-20 审计：text2image
// 无 prompt、upscale 缺输入图都能保存并运行，然后必然炸）。
func validateNodeParams(nodes, edges json.RawMessage) (int, gin.H) {
	ns, err := service.ParseNodeInstances(nodes)
	if err != nil {
		return http.StatusBadRequest, gin.H{"error": err.Error()}
	}
	var es []model.EdgeInstance
	if len(edges) > 0 {
		if err := json.Unmarshal(edges, &es); err != nil {
			return http.StatusBadRequest, gin.H{"error": "parse edges failed: " + err.Error()}
		}
	}
	if issues := service.ValidateNodeParams(ns, service.InboundSet(es)); len(issues) > 0 {
		return http.StatusBadRequest, gin.H{
			"error":  issues[0].Message,
			"code":   "node_param_missing",
			"issues": issues,
		}
	}
	return 0, nil
}

// validateBindings 保存期校验能力卡：结构合法 + 工作流卡引用的那条流水线真实存在。
//
// 为什么必须在保存时拦：工作流卡会**替换整个节点的执行**，
// 贴错一张（不存在的 id / 贴到自己身上）要到运行时才炸，而且是"运行到一半炸"。
func (h *WorkflowHandler) validateBindings(nodes json.RawMessage, selfID uint) (int, gin.H) {
	ns, err := service.ParseNodeInstances(nodes)
	if err != nil {
		return http.StatusBadRequest, gin.H{"error": err.Error()}
	}
	for _, n := range ns {
		if issues := service.ValidatePipeline(n.Data); len(issues) > 0 {
			return http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("节点 %s 的能力卡配置有误：%s", n.ID, issues[0]),
				"code":  "binding_invalid",
			}
		}
		pipe := service.ParsePipeline(n.Data)
		if pipe.Replace == nil {
			continue
		}
		// 子工作流 / 循环节点本来就自带 workflow_id：再贴一张工作流卡，
		// 卡会覆盖节点自身那条，用户以为是"再套一层"，实际是"换掉"。
		// 这是最容易误解的一种叠加，保存时直接拒。
		if n.Type == "sub_workflow" || n.Type == "loop" {
			return http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("节点 %s 是「%s」，已经自带要执行的工作流；不要再贴工作流卡（要换就把节点里的 workflow_id 改掉）", n.ID, n.Type),
				"code":  "binding_conflict",
			}
		}
		wfID, ok := service.WorkflowIDOfBinding(service.Binding{Kind: pipe.Replace.Kind, ID: pipe.Replace.ID})
		if !ok {
			return http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("节点 %s 的工作流卡 id 不合法（%q）", n.ID, pipe.Replace.ID),
				"code":  "binding_invalid",
			}
		}
		if selfID != 0 && wfID == selfID {
			return http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("节点 %s 把这条工作流自己贴成了工作流卡：会无限递归", n.ID),
				"code":  "binding_self_reference",
			}
		}
		if h.db != nil {
			var cnt int64
			if err := h.db.Model(&model.Workflow{}).Where("id = ?", wfID).Count(&cnt).Error; err == nil && cnt == 0 {
				return http.StatusBadRequest, gin.H{
					"error": fmt.Sprintf("节点 %s 贴的工作流卡 #%d 不存在（可能已被删除）", n.ID, wfID),
					"code":  "binding_missing_workflow",
				}
			}
		}
	}
	return 0, nil
}

// Create 创建工作流
func (h *WorkflowHandler) Create(c *gin.Context) {
	var wf model.Workflow
	if err := c.ShouldBindJSON(&wf); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if code, body := validateGraph(wf.Nodes, wf.Edges); code != 0 {
		c.JSON(code, body)
		return
	}
	if code, body := validateNodeParams(wf.Nodes, wf.Edges); code != 0 {
		c.JSON(code, body)
		return
	}
	if code, body := h.validateBindings(wf.Nodes, 0); code != 0 {
		c.JSON(code, body)
		return
	}
	if err := h.db.Create(&wf).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, wf)
}

// Update 更新工作流（局部更新）
//
// 之前是无条件写 name/nodes/edges 三个字段：调用方只想改名时，nodes/edges 为空
// 就会把整张图覆盖成 null——一次「重命名」直接把工作流清空，属于静默数据丢失。
// 现在只更新请求体里真正出现的字段。
func (h *WorkflowHandler) Update(c *gin.Context) {
	id := c.Param("id")
	var wf model.Workflow
	if err := h.db.First(&wf, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	var updates struct {
		Name  *string         `json:"name"`
		Nodes json.RawMessage `json:"nodes"`
		Edges json.RawMessage `json:"edges"`
	}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	fields := map[string]interface{}{}
	if updates.Name != nil && strings.TrimSpace(*updates.Name) != "" {
		fields["name"] = strings.TrimSpace(*updates.Name)
	}
	if len(updates.Nodes) > 0 || len(updates.Edges) > 0 {
		// 图结构必须成对提交：只改节点不改连线会得到一张自相矛盾的图
		if len(updates.Nodes) == 0 || len(updates.Edges) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nodes 与 edges 必须一起提交"})
			return
		}
		if code, body := h.validateBindings(updates.Nodes, wf.ID); code != 0 {
			c.JSON(code, body)
			return
		}
		if code, body := validateGraph(updates.Nodes, updates.Edges); code != 0 {
			c.JSON(code, body)
			return
		}
		if code, body := validateNodeParams(updates.Nodes, updates.Edges); code != 0 {
			c.JSON(code, body)
			return
		}
		fields["nodes"] = updates.Nodes
		fields["edges"] = updates.Edges
	}
	if len(fields) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可更新的字段"})
		return
	}

	h.db.Model(&wf).Updates(fields)
	h.db.First(&wf, id)
	c.JSON(http.StatusOK, wf)
}

// Delete 删除工作流
func (h *WorkflowHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	h.db.Delete(&model.Workflow{}, id)
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// RunOptions 起跑参数。
//
// 为什么单开一个结构而不是继续加可选参数：起跑这件事的入参已经超过
// 「谁触发的」一个维度了（还有评测语义、用哪份图跑）。继续用变参
// 会让「传了 5 个字符串但顺序记错」变成一种可行的错误。
type RunOptions struct {
	InvokedBy          string
	CaseID             string
	DatasetFingerprint string
	Category           string
	Role               string
	EvalBatch          string
	// Nodes/Edges 非空时用这份图跑（评测跑历史版本用），**不写库**。
	// 评测必须能跑「第 3 版的图」而不动当前图，否则每次对比都要来回回滚。
	Nodes json.RawMessage
	Edges json.RawMessage
	// WorkflowVersion 0 = 起跑时取当前最新版本
	WorkflowVersion int
	// AgentConcurrency 本次运行的 agent 并发上限；0 = 用引擎默认。
	// 同一张图在不同机器上能扛的并发不同，所以它是运行参数而不是工作流参数。
	AgentConcurrency int
	// Vars 本次运行的输入注入：图里 data 字符串中的 {{key}} 会被替换。
	//
	// 评测的每个 case 靠它区分输入 —— 没有它，"跑 8 个 case"其实是同一个 prompt 跑 8 遍。
	// 只影响这次 run 的内存图，不写库（见 vars.go）。
	Vars map[string]interface{}
}

// StartRun 创建执行记录并异步跑完（触发器与手动运行共用同一条路径）。
// trigger 可选：manual / webhook / cron；留空按 manual 记录。
func (h *WorkflowHandler) StartRun(wf *model.Workflow, trigger ...string) *model.WorkflowRun {
	invokedBy := "manual"
	if len(trigger) > 0 && trigger[0] != "" {
		invokedBy = trigger[0]
	}
	return h.StartRunWith(wf, RunOptions{InvokedBy: invokedBy})
}

// StartRunWith 起跑（可带评测语义与临时图）。
func (h *WorkflowHandler) StartRunWith(wf *model.Workflow, opts RunOptions) *model.WorkflowRun {
	nodesJSON := []byte(wf.Nodes)
	edgesJSON := []byte(wf.Edges)
	if len(opts.Nodes) > 0 {
		nodesJSON = []byte(opts.Nodes)
	}
	if len(opts.Edges) > 0 {
		edgesJSON = []byte(opts.Edges)
	}
	// 输入注入：在**预检之前**渲染，否则 {{key}} 这种占位会被当成真实参数去估依赖。
	nodesJSON = renderNodeVars(nodesJSON, opts.Vars)

	// 运行前依赖预检：服务没起时创建一条 failed 记录留痕，不进入执行。
	// （手动入口 Run 直接 503 返回；cron/trigger 走这里，留痕才能追溯）
	if missing := h.preflight(nodesJSON, edgesJSON); len(missing) > 0 {
		now := time.Now()
		run := h.newRun(wf.ID, "failed", opts)
		run.Error = preflightErrorText(missing)
		run.StartedAt = &now
		run.EndedAt = &now
		h.db.Create(run)
		return run
	}

	invokedBy := "manual"
	if opts.InvokedBy != "" {
		invokedBy = opts.InvokedBy
	}
	now := time.Now()
	run := h.newRun(wf.ID, "running", opts)
	run.InvokedBy = invokedBy
	run.StartedAt = &now
	h.db.Create(run)

	// 任务落库：进程崩溃后 DB 里仍保留「曾受理过」的事实，
	// 下次启动由 JobQueue.RecoverInterrupted 把残留 running 标记为 interrupted
	jobTrigger := "manual"
	if opts.InvokedBy != "" {
		jobTrigger = opts.InvokedBy
	}
	job := model.Job{
		RunID:      run.ID,
		WorkflowID: wf.ID,
		Status:     "pending",
		Trigger:    jobTrigger,
		CreatedAt:  now,
	}
	h.db.Create(&job)

	// 增量落库：每层完成后更新 node_states（断点续跑依据）
	persist := func(states []model.NodeInstance) {
		statesJSON, err := json.Marshal(states)
		if err != nil {
			return
		}
		h.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
			Update("node_states", json.RawMessage(statesJSON))
	}

	// 断点：节点上勾了 breakpoint 就停，运行时不必再开调试模式
	breakpoints := collectBreakpoints(nodesJSON)
	var rc *service.RunControl
	if len(breakpoints) > 0 {
		rc = service.NewBreakpointControl(mapFromSlice(breakpoints))
	} else if opts.AgentConcurrency > 0 {
		// 没断点但声明了并发：仍需一个控制对象承载运行级参数。
		// 用全速控制（budget=-1、无断点），推进语义与不传 rc 完全一致。
		rc = service.NewFullControl()
	}
	if rc != nil && opts.AgentConcurrency > 0 {
		rc.SetAgentConcurrency(opts.AgentConcurrency)
	}

	exec := func() {
		var nodeStates []model.NodeInstance
		var err error
		if rc != nil {
			// 走可取消 + 断点控制入口
			nodeStates, err = h.engine.ExecuteWithControl(nodesJSON, edgesJSON, run.ID, rc, persist)
		} else {
			nodeStates, err = h.engine.ExecuteWithRunID(nodesJSON, edgesJSON, run.ID, persist)
		}

		paused, pauseAt, pauseReason := "", "", ""
		if rc != nil {
			p, at, reason := rc.State()
			if p {
				paused, pauseAt, pauseReason = "paused", at, reason
			}
		}

		endNow := time.Now()
		run.EndedAt = &endNow
		job.EndedAt = &endNow
		switch {
		case err != nil && service.ErrorCodeOf(err) == model.ErrorCodeCancelled:
			// 主动中止 ≠ 失败：台账与界面都要能区分「我按停的」和「跑挂了」
			run.Status = "cancelled"
			run.Error = err.Error()
			job.Status = "interrupted"
			job.Error = err.Error()
		case err != nil:
			run.Status = "failed"
			run.Error = err.Error()
			job.Status = "failed"
			job.Error = err.Error()
		case paused != "":
			run.Status = "paused"
			run.PausedAt = pauseAt
			run.PauseReason = pauseReason
			job.Status = "paused"
		default:
			run.Status = "success"
			job.Status = "done"
		}
		statesJSON, _ := json.Marshal(nodeStates)
		run.NodeStates = json.RawMessage(statesJSON)
		h.db.Save(run)
		h.db.Save(&job)

		// 产物血缘：运行结束（含暂停）都记一次，已产出的图/音/视频都能溯源
		if h.lineage != nil {
			var edges []model.EdgeInstance
			_ = json.Unmarshal(edgesJSON, &edges)
			h.lineage.RecordRun(wf.ID, run.ID, nodeStates, edges)
		}

		// 证据信封：外部评测器只读这一份，避免「谁记了耗时 / 失败算 0 分还是没测到」
		// 各算各的账。旁路写入，失败只记日志，绝不反过来影响本次运行的结论。
		// 版本号来自 run 自己（起跑时钉住的），不再读「当前最新版本」——
		// 运行期间有人改图存版本时，后者会把旧图的成绩记到新版本上。
		ev := service.BuildEvidence(
			*run, nodeStates, h.engine.SideUnloadCount(), service.CountResumes(h.db, run.ID))
		service.WriteEvidence(ev)
	}

	if h.queue != nil {
		job.Status = "running"
		job.StartedAt = &now
		h.db.Save(&job)
		h.queue.Enqueue(job, exec)
	} else {
		go exec()
	}

	return run
}

// newRun 建一条未落库的运行记录，把评测语义与版本在**起跑这一刻**钉住。
//
// 版本取「当前最新快照号」：起跑后有人改图并新建版本，不会影响这条记录；
// 反过来说，如果这条 run 用的是一份显式指定的历史图，调用方要在
// RunOptions.WorkflowVersion 里写清是哪一版（评测跑 baseline 时正是如此）。
func (h *WorkflowHandler) newRun(workflowID uint, status string, opts RunOptions) *model.WorkflowRun {
	run := &model.WorkflowRun{WorkflowID: workflowID, Status: status, InvokedBy: opts.InvokedBy}
	if opts.CaseID != "" {
		v := opts.CaseID
		run.CaseID = &v
	}
	if opts.DatasetFingerprint != "" {
		v := opts.DatasetFingerprint
		run.DatasetFingerprint = &v
	}
	if opts.Category != "" {
		v := opts.Category
		run.Category = &v
	}
	run.Role = opts.Role
	run.EvalBatch = opts.EvalBatch
	run.WorkflowVersion = opts.WorkflowVersion
	if run.WorkflowVersion == 0 {
		run.WorkflowVersion = h.latestVersion(workflowID)
		if run.WorkflowVersion == 0 {
			// 这张图从没被快照过（新建后直接跑、或历史遗留图）。
			//
			// 不补这一条的话，证据里 workflowVersion 恒为 0（omitempty 时字段直接消失），
			// 自进化的配对评测就只能靠 role 区分"改前/改后" —— 而 role 是评测层自己标的，
			// 可以标错、也可以被标错之后没人发现。版本号是引擎的事实，不该缺。
			//
			// 并发安全：建完再以库里的最大值为准。两个 run 同时起跑时，后到的会拿到同一个号，
			// 不会出现"两个 v1"或"版本号跳空"。
			var wf model.Workflow
			if err := h.db.First(&wf, workflowID).Error; err == nil {
				h.snapshotBaseline(wf)
			}
			run.WorkflowVersion = h.latestVersion(workflowID)
		}
	}
	return run
}

// snapshotBaseline 给"从没被快照过"的图存一个基线版本，返回版本号（失败返回 0）。
//
// 与 snapshotBefore 的区别：那个是"改图前留一手"，这个是"第一次跑之前先立个桩"。
// 快照失败不阻断起跑 —— 宁可让这条 run 的版本号是 0（事实：没留下版本），
// 也不要因为它没跑起来。
func (h *WorkflowHandler) snapshotBaseline(wf model.Workflow) int {
	ver := model.WorkflowVersion{
		WorkflowID: wf.ID,
		Version:    h.latestVersion(wf.ID) + 1,
		Name:       wf.Name,
		Nodes:      wf.Nodes,
		Edges:      wf.Edges,
		ChangeNote: "起跑时自动建立基线（此前该图没有可回滚的快照）",
	}
	if err := h.db.Create(&ver).Error; err != nil {
		return 0
	}
	return ver.Version
}

// CancelRun 中止进行中的运行（已完成的节点保留，剩余标记为 skipped）
func (h *WorkflowHandler) CancelRun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	if !h.engine.CancelRun(uint(id)) {
		c.JSON(http.StatusConflict, gin.H{"error": "该运行已结束或未在进行中"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": id, "status": "cancelling"})
}

// Run 手动执行工作流
func (h *WorkflowHandler) Run(c *gin.Context) {
	var wf model.Workflow
	if err := h.db.First(&wf, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	// 谁在调这条工作流。默认 manual（画布点运行），外部智能体走 MCP 时自报。
	// 只做「记录 + 不一致提示」，不做硬拦：人工调试声明给 WorkBuddy 的流水线是正当需求。
	invokedBy := "manual"
	// 评测语义：评测层起跑时显式注入，引擎原样落库（评审时能回答「这条成绩
	// 是哪个 case、哪一侧」）。不传就是 null —— 引擎不替评测层编。
	var req struct {
		InvokedBy          string `json:"invoked_by"`
		CaseID             string `json:"caseId"`
		DatasetFingerprint string `json:"datasetFingerprint"`
		Category           string `json:"category"`
		Role               string `json:"role"`
		EvalBatch          string `json:"evalBatch"`
		// Vars 评测 case 的输入：图里 {{key}} 会被替换（不起作用就留空）
		Vars map[string]interface{} `json:"vars"`
		// AgentConcurrency 本次运行的 agent 并发上限；不传/0 = 引擎默认。
		AgentConcurrency int `json:"agent_concurrency"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.AgentConcurrency < 0 || req.AgentConcurrency > 16 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("agent_concurrency=%d 超出范围（1..16；不传则用引擎默认）", req.AgentConcurrency),
			"code":  "bad_agent_concurrency",
		})
		return
	}
	if v := strings.TrimSpace(req.InvokedBy); v != "" {
		if !validInvoker(v) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("invoked_by=%q 不认识（可用：manual / workbuddy / dsh / mcp / webhook / cron）", v),
				"code":  "bad_invoker",
			})
			return
		}
		invokedBy = v
	}
	role := strings.TrimSpace(req.Role)
	// role 只有三档且**不能猜**：写错一个字母就会让配对判定把两侧混成一组。
	if role != "" && role != "baseline" && role != "candidate" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("role=%q 不认识（可用：baseline / candidate；不传表示这不是一次评测运行）", role),
			"code":  "bad_role",
		})
		return
	}
	warn := invokerMismatch(&wf, invokedBy)
	// 手动入口：预检不过直接 503 + 缺失清单（缺什么、怎么起），不创建 run 记录
	if missing := h.preflight([]byte(wf.Nodes), []byte(wf.Edges)); len(missing) > 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "依赖服务未就绪，工作流未开始执行",
			"code":    "preflight_failed",
			"missing": missing,
			"hint":    "在「环境」面板启动缺失的服务后重试；服务刚启动的话稍等几秒再点",
		})
		return
	}
	// 资源准入：机器压力高时推迟，而不是硬跑然后 OOM。
	//
	// 顺序上放在依赖预检之后 —— 缺服务是"根本跑不了"，不必再问压力。
	// 语义是 fail-open：治理器问不到（没起 / 超时 / 返回异常）就放行，
	// 只有它明确说不才拦。FlowEdge 必须能独立工作，不能因为另一个进程
	// 没起就拒绝出片。
	if h.governor != nil && h.governor.Enabled() {
		if v := h.governor.Check(c.Request.Context()); !v.Allowed {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":    "资源紧张，本次运行已推迟",
				"code":     "governor_declined",
				"governor": v,
				"hint":     governorHint(v.Reasons),
			})
			return
		}
	}
	run := h.StartRunWith(&wf, RunOptions{
		InvokedBy:          invokedBy,
		Vars:               req.Vars,
		CaseID:             strings.TrimSpace(req.CaseID),
		DatasetFingerprint: strings.TrimSpace(req.DatasetFingerprint),
		Category:           strings.TrimSpace(req.Category),
		Role:               role,
		EvalBatch:          strings.TrimSpace(req.EvalBatch),
		AgentConcurrency:   req.AgentConcurrency,
	})
	resp := gin.H{"run_id": run.ID, "status": "running", "invoked_by": invokedBy}
	if run.CaseID != nil {
		resp["caseId"] = *run.CaseID
		resp["role"] = run.Role
		resp["workflowVersion"] = run.WorkflowVersion
	}
	if warn != "" {
		resp["warning"] = warn
	}
	c.JSON(http.StatusAccepted, resp)
}

// ResumeRun 断点续跑：复用已完成节点输出，只重跑失败 / 未执行节点
func (h *WorkflowHandler) ResumeRun(c *gin.Context) {
	h.resumeFrom(c, "")
}

// ResumeFromRun 从指定节点开始重跑（单节点重跑 / 「从这继续」）
// body: {"node_id": "b"}
func (h *WorkflowHandler) ResumeFromRun(c *gin.Context) {
	var body struct {
		NodeID string `json:"node_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.NodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing node_id"})
		return
	}
	h.resumeFrom(c, body.NodeID)
}

// resumeFrom 断点续跑公共实现；fromNode 为空时等同整体续跑
func (h *WorkflowHandler) resumeFrom(c *gin.Context, fromNode string) {
	h.resumeFromWithOverride(c, fromNode, nil)
}

// resumeFromWithOverride 同 resumeFrom，但允许传入改过的节点图。
//
// 编排逻辑已下沉到 service.RunService（见 service/run_service.go 顶部注释：
// 校验运行状态 / 挑节点图 / 落 job / 回写状态本来就和 HTTP 无关）。
// 这里只做三件事：解析参数、调服务、把 error 映射成状态码。
func (h *WorkflowHandler) resumeFromWithOverride(c *gin.Context, fromNode string, nodesOverride []byte) {
	runID, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil || runID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid runId"})
		return
	}

	out, err := h.runs.Resume(uint(runID), fromNode, nodesOverride)
	if err != nil {
		var nf *service.NodeNotFoundError
		switch {
		case errors.As(err, &nf):
			c.JSON(http.StatusBadRequest, gin.H{"error": "node not found in this run", "node_id": nf.NodeID})
		case errors.Is(err, service.ErrRunNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		case errors.Is(err, service.ErrWorkflowNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		case errors.Is(err, service.ErrRunStillRunning):
			c.JSON(http.StatusConflict, gin.H{"error": "run is still running"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"run_id":     out.RunID,
		"status":     "running",
		"resumed":    true,
		"prev_nodes": out.PrevNodes,
	})
}

// GetRun 获取执行记录
func (h *WorkflowHandler) GetRun(c *gin.Context) {
	runID := c.Param("runId")
	var run model.WorkflowRun
	if err := h.db.First(&run, runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	c.JSON(http.StatusOK, run)
}

// ListRuns 列出某工作流的执行记录。
// 保持返回数组以兼容前端执行面板；分页与状态过滤见 ?limit=&offset=&status=（默认 50，上限 200）
func (h *WorkflowHandler) ListRuns(c *gin.Context) {
	wfID := c.Param("id")
	limit := parseIntDefault(c.Query("limit"), 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	status := c.Query("status")

	q := h.db.Model(&model.WorkflowRun{}).Where("workflow_id = ?", wfID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var runs []model.WorkflowRun
	q.Order("created_at DESC").Limit(limit).Find(&runs)

	c.JSON(http.StatusOK, runs)
}

// validInvoker 允许的调用方标识。自由字符串会让"谁在调"失去意义。
func validInvoker(v string) bool {
	switch v {
	// evaluate：自进化配对评测（POST /api/evolution/evaluate 起的跑）。
	// 单独一档而不是借用 manual —— 评测跑与人工调试在统计里必须分得开，
	// 否则「人类介入次数」和「人工调试耗时」会把评测污染掉。
	case "manual", "workbuddy", "dsh", "mcp", "webhook", "cron", "evaluate":
		return true
	}
	return false
}

// invokerMismatch 开始节点声明的入口与实际调用方不一致时，给一句人话提示。
//
// 为什么不直接拒绝：声明的是"这条流水线主要为谁准备"，
// 人工调试、迁移期、临时借用都是正常场景。拒绝会把人逼去改节点，反而失真。
func invokerMismatch(wf *model.Workflow, invokedBy string) string {
	var nodes []model.NodeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		return ""
	}
	for i := range nodes {
		if nodes[i].Type != "start" {
			continue
		}
		declared, _ := nodes[i].Data["entry_agent"].(string)
		if declared == "" || declared == "manual" {
			return ""
		}
		if declared != invokedBy {
			return fmt.Sprintf("这条工作流声明由 %s 触发，本次是 %s 调的（已按实际调用方记录，不影响执行）", declared, invokedBy)
		}
		return ""
	}
	return ""
}

// governorHint 按拒绝原因给**可操作**的提示。
//
// 为什么不能笼统写一句"等一会儿再试"：治理器的拒绝原因差别很大——
// 游戏保护要退游戏、内存压力要等任务、磁盘要清盘、显存要卸模型。
// 指错方向会让人去关一堆无关进程然后仍被拦。
// 真实事故（2026-10-23）：本机 WeGame 启动器常驻触发「游戏保护模式」，
// 而当时的提示写的是"停掉占满显存的其他进程"——完全指错，查了半小时才发现根因。
func governorHint(reasons []string) string {
	for _, r := range reasons {
		low := strings.ToLower(r)
		switch {
		case strings.Contains(r, "游戏"):
			return "检测到游戏相关进程，引擎按保护策略避让（这是配置项，不是故障）。" +
				"确认没在对局时：退出 WeGame 等启动器，或把 WeGame.exe 从序策治理器 " +
				"gameGuard.processes 名单里去掉（只留 League of Legends.exe / LeagueClient.exe）"
		case strings.Contains(r, "显存") || strings.Contains(low, "gpu") || strings.Contains(low, "vram"):
			return "显存不足：先在「环境」面板停掉不用的 ComfyUI 实例（或让它 /free 卸载权重），再重试"
		case strings.Contains(r, "磁盘"):
			return "磁盘余量不足：清理 D 盘后重试（权重与产物都在 D://AI 与运行\\data）"
		case strings.Contains(low, "cpu") || strings.Contains(r, "内存"):
			return "CPU / 内存压力高：等当前编译或渲染任务结束再重试"
		}
	}
	return "等当前任务结束后重试；持续被拦就查治理器状态（:30002 /api/flow/governor/status）里的 reasons"
}
