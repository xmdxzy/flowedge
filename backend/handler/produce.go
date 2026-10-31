package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// Producer 出片入口：选流水线 + 填字段 → 服务端组装固定图 → 起跑。
//
// 这是 FlowEdge 收敛后**唯一**的运行入口。原来那条
// 「画布 → 保存工作流 → 点运行」的路已经去掉：图不再由用户画。
type Producer struct {
	db     *gorm.DB
	engine *service.EngineV2
	queue  *service.JobQueue
	pipes  *service.PipelineStore

	// 依赖预检（SetPreflight 注入；nil 时跳过）
	sup         *service.Supervisor
	ollamaBase  string
	comfyBase   string
	comfyH3Base string
	asrBase     string
	ttsBase     string

	// 质量循环的默认轮数（qc=true 时生效）
	qcRounds int

	// 血缘（V3 P0）：SetLineage 注入。nil 时不生成 manifest，出片行为不变。
	fps           *service.FingerprintCache
	bundles       *service.BundleStore
	engineVersion string
	manifestDir   string

	// 预算门（V3 P0）：runID -> 该次运行的预算与起点。引擎心跳时回查。
	budgetMu sync.Mutex
	budgets  map[uint]*budgetState

	mu sync.Mutex
}

// budgetState 一次运行的预算状态（start 起算，fired 保证只取消一次）。
type budgetState struct {
	b      service.Budget
	est    service.GraphEstimate
	start  time.Time
	fired  bool
	warned bool
}

func NewProducer(db *gorm.DB, engine *service.EngineV2, queue *service.JobQueue, pipes *service.PipelineStore) *Producer {
	p := &Producer{db: db, engine: engine, queue: queue, pipes: pipes, qcRounds: 3,
		budgets: map[uint]*budgetState{}}
	// 预算门挂在引擎心跳上：引擎不认识预算语义，只在 node_progress 时问一句「该不该停」。
	if engine != nil {
		engine.SetBudgetGuard(p.checkBudget)
	}
	return p
}

// setBudget 登记一次运行的预算与预估（start 时调用）。
func (p *Producer) setBudget(runID uint, b service.Budget, est service.GraphEstimate) {
	if runID == 0 || !b.Enabled() {
		return
	}
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	if p.budgets == nil {
		p.budgets = map[uint]*budgetState{}
	}
	p.budgets[runID] = &budgetState{b: b, est: est, start: time.Now()}
}

// clearBudget 运行结束时回收（返回对账结论，写进 manifest）。
func (p *Producer) clearBudget(runID uint) *service.BudgetReconcile {
	p.budgetMu.Lock()
	st, ok := p.budgets[runID]
	delete(p.budgets, runID)
	p.budgetMu.Unlock()
	if !ok || runID == 0 {
		return nil
	}
	actual := time.Since(st.start).Seconds()
	r := st.b.Reconcile(st.est, actual)
	return &r
}

// checkBudget 引擎心跳回调：超限时按 OnExceed 决定是否取消。
//
// warn 模式只在第一次记日志（否则每 5 秒一条，把 engine.log 刷满）。
func (p *Producer) checkBudget(runID uint) (bool, string) {
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	st, ok := p.budgets[runID]
	if !ok || st.fired {
		return false, ""
	}
	exceeded, reason := st.b.Check(time.Since(st.start).Seconds())
	if !exceeded {
		return false, ""
	}
	if !st.warned {
		st.warned = true
		log.Printf("[budget] run=%d %s（on_exceed=%s）", runID, reason, st.b.OnExceed)
	}
	if st.b.ShouldStop() {
		st.fired = true
		return true, reason
	}
	return false, ""
}

// SetPreflight 注入依赖探测所需端点。
func (p *Producer) SetPreflight(sup *service.Supervisor, ollamaBase, comfyBase, comfyH3Base, asrBase, ttsBase string) {
	p.sup = sup
	p.ollamaBase = ollamaBase
	p.comfyBase = comfyBase
	p.comfyH3Base = comfyH3Base
	p.asrBase = asrBase
	p.ttsBase = ttsBase
}

// SetQCRounds 质量循环轮数（1-8），0 或超界回落默认 3。
func (p *Producer) SetQCRounds(n int) {
	if n > 0 && n <= 8 {
		p.qcRounds = n
	}
}

// SetLineage 注入血缘所需依赖（V3 P0）。未注入时不出 manifest，出片行为与旧版一致。
func (p *Producer) SetLineage(fps *service.FingerprintCache, bundles *service.BundleStore, engineVersion, manifestDir string) {
	p.fps = fps
	p.bundles = bundles
	p.engineVersion = engineVersion
	p.manifestDir = manifestDir
}

// SweepTransient 清掉上次残留的质量循环临时图（进程崩溃会留下）。
func (p *Producer) SweepTransient() int {
	res := p.db.Where("name LIKE ?", "qc:%").Delete(&model.Workflow{})
	return int(res.RowsAffected)
}

// writeRunManifest 落血缘清单（V3 P0）。
//
// 三条纪律：
//  1. 只登记**真实存在**的产物，不编造路径（缺失就留空，让人一眼看出没产出）；
//  2. 模型指纹按节点类型展开（宁可多记，不可漏记）；
//  3. 写失败只记日志，绝不影响出片结论——血缘是附加价值，不是交付前置条件。
func (p *Producer) writeRunManifest(run *model.WorkflowRun, states []model.NodeInstance, reconcile *service.BudgetReconcile) {
	if p == nil || p.fps == nil || run == nil {
		return
	}
	var params map[string]interface{}
	if len(run.Params) > 0 {
		_ = json.Unmarshal(run.Params, &params)
	}

	types := make([]string, 0, len(states))
	for _, n := range states {
		types = append(types, n.Type)
	}
	names := service.ModelNamesForRoles(p.bundles, service.ModelRolesForNodes(types))

	lite := make([]service.NodeInstanceLite, 0, len(states))
	for _, n := range states {
		item := service.NodeInstanceLite{
			ID:         n.ID,
			Type:       n.Type,
			Status:     n.Status,
			ErrorCode:  n.ErrorCode,
			ModelNames: names,
		}
		if n.Data != nil {
			item.Params = n.Data
			if v, ok := n.Data["seed"].(float64); ok {
				item.Seed = int64(v)
			}
		}
		if path := service.ArtifactPathOf(n.Output); path != "" {
			item.ArtifactPaths = []string{path}
		}
		if n.StartedAt != nil && n.EndedAt != nil {
			item.DurationMS = n.EndedAt.Sub(*n.StartedAt).Milliseconds()
		}
		lite = append(lite, item)
	}

	m := service.BuildManifest(service.RunMeta{
		ID:         run.ID,
		PipelineID: run.PipelineID,
		InvokedBy:  run.InvokedBy,
		Params:     params,
		StartedAt:  run.StartedAt,
		EndedAt:    run.EndedAt,
	}, lite, p.fps, p.engineVersion)
	m.Budget = reconcile

	path, err := service.WriteManifest(p.manifestDir, m)
	if err != nil {
		log.Printf("[manifest] run=%d 清单写入失败：%v", run.ID, err)
		return
	}
	run.ManifestPath = path
	if err := p.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
		Update("manifest_path", path).Error; err != nil {
		log.Printf("[manifest] run=%d 路径回写失败：%v", run.ID, err)
	}
	if err := p.fps.Save(); err != nil {
		log.Printf("[manifest] 指纹缓存落盘失败：%v", err)
	}
	log.Printf("[manifest] run=%d 血缘已落盘：%s（%d 个节点）", run.ID, path, len(m.Nodes))
}

// GetManifest 返回一次运行的血缘清单。
//
// 没有清单时返回 404 并说明原因，而不是返回空对象——
// 「没有血缘」和「血缘是空的」是两件事，前者要人去查，后者说明这次没产物。
func (p *Producer) GetManifest(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "run id 不合法"})
		return
	}
	var run model.WorkflowRun
	if err := p.db.First(&run, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "运行记录不存在", "run_id": id})
		return
	}
	if run.ManifestPath == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error":  "这次运行没有血缘清单（V3 之前的运行，或血缘未启用）",
			"run_id": id,
		})
		return
	}
	m, err := service.ReadManifest(run.ManifestPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "清单读取失败：" + err.Error(), "path": run.ManifestPath})
		return
	}
	c.JSON(http.StatusOK, m)
}

// ListPipelines 返回向导要用的流水线目录。
func (p *Producer) ListPipelines(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"pipelines": p.pipes.List(),
		"count":     len(p.pipes.List()),
	})
}

// ReloadPipelines 热重载 data/pipelines.json（改完不用重启）。
func (p *Producer) ReloadPipelines(c *gin.Context) {
	if err := p.pipes.Reload(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "count": len(p.pipes.List())})
}

// produceRequest 出片请求。
type produceRequest struct {
	Pipeline  string                 `json:"pipeline"`
	Params    map[string]interface{} `json:"params"`
	InvokedBy string                 `json:"invoked_by"`
	// QCRounds 覆盖默认轮数（可选）
	QCRounds int `json:"qc_rounds,omitempty"`
	// AllowPartialVRAM 明确接受"显存装不下、可能换页"的硬跑。
	//
	// 预检门只拦「连权重都装不下」这一档（too_small）——那一档不是"慢一点"，
	// 是按步换页，实测同一镜 85s → 2269s，然后撞节点超时整支白跑。
	// 想验证换页行为时显式传 true，让"我就是要硬跑"和"我没注意"分得开。
	AllowPartialVRAM bool `json:"allow_partial_vram,omitempty"`
}

// Produce 起一次出片。
func (p *Producer) Produce(c *gin.Context) {
	var req produceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体需要 {pipeline, params}"})
		return
	}
	run, status, errMsg := p.start(req)
	if errMsg != "" {
		ret := gin.H{"error": errMsg}
		if run != nil {
			ret["run_id"] = run.ID
		}
		c.JSON(status, ret)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"run_id":     run.ID,
		"pipeline":   run.PipelineID,
		"status":     run.Status,
		"params":     json.RawMessage(run.Params),
		"created_at": run.CreatedAt,
	})
}

// vramGateBlocks 显存预检是否该拦下这次出片。
//
// 只拦 too_small（连权重都装不下 → 按步换页，实测同一镜 85s → 2269s）；
// reserve 档（装得下、但保不住预留）实测仍能 82–112s 出片，所以**只告警不拦**。
// allow_partial_vram=true 表示"我知道会换页，就是要跑"。
func vramGateBlocks(adv service.VRAMAdvisory, allowPartial bool) bool {
	return adv.Level == service.VRAMLevelTooSmall && !allowPartial
}

// vramBlockNow 开跑前复判一次显存（返回非空 = 这次运行该失败）。
//
// 为什么提交时判过还要再判：提交时的结论会过期——排队期间可能起了别的实例、
// 或前一条任务把权重换成了另一个模型。复判只要几毫秒，比白烧十几分钟便宜。
//
// 给三次机会（间隔 2s）而不是判一次：CUDA 释放是异步的（AGENTS §2：
// /free 返回后要 5–10 秒显存才真回来），前一条刚跑完就复判，
// 很容易读到「还没还回来」的假数字，把一条好任务判死。
func (p *Producer) vramBlockNow(nodes []model.NodeInstance, allowPartial bool) string {
	if allowPartial {
		return ""
	}
	var adv service.VRAMAdvisory
	for attempt := 0; attempt < 3; attempt++ {
		free := service.FreeVRAMForNodes(nodes, p.comfyBase, p.comfyH3Base)
		if free <= 0 {
			// 探不到（实例没起/端口不通）：那是依赖预检的活，这里不重复拦
			return ""
		}
		adv = service.VRAMAdvisoryForNodes(nodes, free)
		if !vramGateBlocks(adv, false) {
			return ""
		}
		if service.AlreadyResidentForNodes(nodes, p.comfyBase, p.comfyH3Base, adv.NeedMB) {
			return ""
		}
		if attempt < 2 {
			time.Sleep(2 * time.Second)
		}
	}
	return fmt.Sprintf("排队到开跑时显存仍然装不下（连判三次：空闲 %dMB / 需要 %dMB）：%s",
		adv.FreeMB, adv.NeedMB, adv.Message)
}

// start 是 Produce / Rerun / MCP 共用的真正起跑逻辑。
func (p *Producer) start(req produceRequest) (*model.WorkflowRun, int, string) {
	built, err := p.pipes.Build(req.Pipeline, req.Params)
	if err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}

	nodes, edges := built.Nodes, built.Edges

	// 保存期参数校验 + 依赖预检，都针对**用户真正要的那张图**，
	// 必须在质量循环包壳之前做：壳里只有一个 quality_loop 节点，
	// 对壳做预检等于什么都没查（那会变成"最贵的路径反而跳过预检"）。
	if issues := service.ValidateNodeParams(nodes, service.InboundSet(edges)); len(issues) > 0 {
		msgs := make([]string, 0, len(issues))
		for _, it := range issues {
			msgs = append(msgs, it.Message)
		}
		return nil, http.StatusBadRequest, strings.Join(msgs, "；")
	}
	if missing := p.preflight(nodes, edges); len(missing) > 0 {
		return nil, http.StatusServiceUnavailable, preflightText(missing)
	}

	// 显存前瞻：换页跑法**能出片**，但会慢一个数量级（实测同一镜 85s → 2269s），
	// 然后撞上节点超时整支白跑。这个判断是纯算术，先算一句，
	// 比事后从 sqlite 里挖 elapsed_ms 便宜得多（见 service/vram_advisory.go）。
	//
	// 分两档处理（2026-10-26 定案）：
	//   - reserve（装得下、但保不住预留）：只告警。这一档实测仍能出片（82–112s/镜）。
	//   - too_small（连权重都装不下）：**拦下**。这一档是"按步换页"，
	//     花十几分钟证明它跑不动。要硬跑就显式传 allow_partial_vram=true。
	if free := service.FreeVRAMForNodes(nodes, p.comfyBase, p.comfyH3Base); free > 0 {
		adv := service.VRAMAdvisoryForNodes(nodes, free)
		if adv.Level != service.VRAMLevelOK {
			log.Printf("[produce] pipeline=%s 显存前瞻 %s：%s", built.Pipeline.ID, adv.Level, adv.Message)
		}
		if vramGateBlocks(adv, req.AllowPartialVRAM) {
			// 热实例是"空闲少"而不是"装不下"：模型已经在显存里，再拦就是纯误伤
			// （2026-10-26 实测 run 247：空闲 5318MB 被 409 拦掉，其实一次加载都不用）。
			switch {
			case service.AlreadyResidentForNodes(nodes, p.comfyBase, p.comfyH3Base, adv.NeedMB):
				log.Printf("[produce] 显存前瞻 too_small，但目标实例已驻留模型（空闲 %dMB / 需 %dMB）——放行",
					adv.FreeMB, adv.NeedMB)
			case p.queue != nil && p.queue.HasGPUWork():
				// 2026-10-27 补：**自家任务占着卡，不该拦，该排。**
				// 这是 409 误伤的第二种形态（第一种是热实例）：GPU 道串行跑着一条 film，
				// 此时提交第二条看到的空闲只有 5GB —— 一旦拦下，用户要等 20 分钟再手动重提。
				// 而它排队到前一条结束再开跑时，显存早就腾空了。
				// 分寸：只对「自家占用」让路；外来进程（手工实例/浏览器）占着，仍然开机前拦，
				// 因为那种占用**不会自己消失**。
				log.Printf("[produce] 显存前瞻 too_small，但占用来自本引擎的队列任务（GPU 道在跑/排队）——"+
					"不拦，排到它后面；开跑时会再判一次（空闲 %dMB / 需 %dMB）", adv.FreeMB, adv.NeedMB)
			default:
				return nil, http.StatusConflict, fmt.Sprintf(
					"显存装不下，已在开机前拦下（不再用十几分钟去证明它跑不动）：%s。"+
						"先腾显存（停掉别的 ComfyUI 实例/浏览器大页）或把分辨率、帧数降一档；"+
						"确认要硬跑就传 allow_partial_vram=true", adv.Message)
			}
		}
	}

	// 端口是这个依赖的，但进程不是我们起的 —— 2026-10-26 那晚 27 倍慢的真凶。
	// 当时 8189 上跑着别人起的第二个 ComfyUI-H3，抢走 20GB，日志里一句话都没有。
	// 这条在出片时就喊，别等人烧完 15 分钟再来问。
	if p.sup != nil {
		dep := "comfyui"
		if service.UsesH3(nodes) {
			dep = "comfy_h3"
		}
		if p.sup.ServingButUnmanaged(dep) {
			log.Printf("[produce] 警告：%s 的端口由**非引擎托管的实例**提供服务。"+
				"如果同卡上还有一份同样的实例，显存会被分掉，单镜可能慢 10–27 倍并撞超时", dep)
		}
	}

	// 质量循环：把「刚组好的这张图」落成一条临时工作流，再用 quality_loop 包起来。
	// 为什么不直接内嵌：quality_loop 的语义就是"反复重跑另一张图并择优"，
	// 它按 workflow_id 从库里取图（engine.SetResolver）。
	var transientID uint
	if qc, _ := built.Values["qc"].(bool); qc {
		outer, id, werr := p.wrapQualityLoop(built, req.QCRounds)
		if werr != nil {
			return nil, http.StatusBadRequest, werr.Error()
		}
		nodes, edges, transientID = outer.Nodes, outer.Edges, id
	}

	nodesJSON, err := json.Marshal(nodes)
	if err != nil {
		p.dropTransient(transientID)
		return nil, http.StatusInternalServerError, "节点图序列化失败：" + err.Error()
	}
	edgesJSON, err := json.Marshal(edges)
	if err != nil {
		p.dropTransient(transientID)
		return nil, http.StatusInternalServerError, "连线序列化失败：" + err.Error()
	}

	paramsJSON, _ := json.Marshal(built.Values)
	now := time.Now()
	run := &model.WorkflowRun{
		WorkflowID: transientID,
		PipelineID: built.Pipeline.ID,
		Params:     json.RawMessage(paramsJSON),
		Status:     "running",
		InvokedBy:  normalizeInvoker(req.InvokedBy),
		StartedAt:  &now,
		CreatedAt:  now,
	}
	if err := p.db.Create(run).Error; err != nil {
		p.dropTransient(transientID)
		return nil, http.StatusInternalServerError, "运行记录写入失败：" + err.Error()
	}

	// 预算门（V3 P0）：解析声明 + 登记预估。未声明预算时是空操作。
	// 估算用同一张图（质量循环包壳前已经算过的话以包壳前为准：壳只有一个节点，估不出东西）。
	budget := service.ParseBudget(built.Values)
	if budget.Enabled() {
		est := service.EstimateGraph(built.Nodes, built.Edges)
		p.setBudget(run.ID, budget, est)
		log.Printf("[budget] run=%d 已声明预算：%s；预估 P50=%.0fs P90=%.0fs worst=%.0fs",
			run.ID, budget.Describe(), est.P50Sec, est.P90Sec, est.WorstSec)
	}

	job := model.Job{
		RunID:      run.ID,
		WorkflowID: transientID,
		Status:     "pending",
		Trigger:    run.InvokedBy,
		CreatedAt:  now,
	}
	p.db.Create(&job)

	persist := func(states []model.NodeInstance) {
		statesJSON, err := json.Marshal(states)
		if err != nil {
			return
		}
		p.db.Model(&model.WorkflowRun{}).Where("id = ?", run.ID).
			Update("node_states", json.RawMessage(statesJSON))
	}

	exec := func() {
		// 开跑前复判（2026-10-27）：提交时的显存结论到这一刻可能已经过期。
		// 只拦 too_small 且模型确实没驻留的情况——这时再跑就是按步换页，
		// 花十几分钟证明它跑不动；宁可 3 秒内失败并说清原因。
		if msg := p.vramBlockNow(nodes, req.AllowPartialVRAM); msg != "" {
			endNow := time.Now()
			run.EndedAt = &endNow
			run.Status = "failed"
			run.Error = msg
			job.Status = "failed"
			job.Error = msg
			job.EndedAt = &endNow
			p.db.Save(run)
			p.db.Model(&model.Job{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
				"status": job.Status, "error": job.Error, "ended_at": endNow,
			})
			p.dropTransient(transientID)
			log.Printf("[produce] run=%d 开跑前复判被拦下（排队期间显存没腾出来）：%s", run.ID, msg)
			return
		}

		nodeStates, execErr := p.engine.ExecuteWithRunID(nodesJSON, edgesJSON, run.ID, persist)

		endNow := time.Now()
		run.EndedAt = &endNow
		job.EndedAt = &endNow
		switch {
		case execErr != nil && service.ErrorCodeOf(execErr) == model.ErrorCodeCancelled:
			run.Status = "cancelled"
			run.Error = execErr.Error()
			job.Status = "interrupted"
			job.Error = execErr.Error()
		case execErr != nil:
			run.Status = "failed"
			run.Error = execErr.Error()
			job.Status = "failed"
			job.Error = execErr.Error()
		default:
			run.Status = "success"
			job.Status = "done"
			// 贴字属于交付物本身：做不成就不许报成功，否则用户拿到一支没字的片子
			// 还以为一切正常（AGENTS §0.12 ②）。没请求贴字时这里是空操作。
			if _, err := p.engine.ApplyTextOverlay(nodeStates, req.Params); err != nil {
				run.Status = "failed"
				run.Error = err.Error()
				job.Status = "failed"
				job.Error = err.Error()
			}
		}
		statesJSON, _ := json.Marshal(nodeStates)
		run.NodeStates = json.RawMessage(statesJSON)
		p.db.Save(run)
		// V3 P0：预算对账 + 血缘清单。失败的血缘同样是证据，所以不区分成败都写。
		reconcile := p.clearBudget(run.ID)
		p.writeRunManifest(run, nodeStates, reconcile)
		// 只更新状态三列：整行 Save 会把 StartedAt 从 NULL 写回去
		// （内存里那份 job 的 StartedAt 是空的，真正的时间由队列落库）。
		p.db.Model(&model.Job{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
			"status": job.Status, "error": job.Error, "ended_at": endNow,
		})
		p.dropTransient(transientID)
	}

	// 资源分道（2026-10-27）：GPU 道串行，CPU 道（voice：TTS 在 CPU 上跑）与它并行。
	// 旧实现只有一个全局闸，一句话的配音要排在 20 分钟的 film 后面。
	if p.queue != nil {
		p.queue.EnqueueClass(job, service.JobClassForNodes(nodes), exec)
	} else {
		job.Status = "running"
		job.StartedAt = &now
		p.db.Save(&job)
		go exec()
	}

	return run, http.StatusAccepted, ""
}

// wrapQualityLoop 把组装好的图收进一条临时工作流，外层只留 start → quality_loop → end。
func (p *Producer) wrapQualityLoop(built *service.BuiltPipeline, rounds int) (*service.BuiltPipeline, uint, error) {
	if rounds <= 0 || rounds > 8 {
		rounds = p.qcRounds
	}
	nodesJSON, err := json.Marshal(built.Nodes)
	if err != nil {
		return nil, 0, err
	}
	edgesJSON, err := json.Marshal(built.Edges)
	if err != nil {
		return nil, 0, err
	}
	p.mu.Lock()
	inner := model.Workflow{Name: "qc:" + built.Pipeline.ID, Nodes: nodesJSON, Edges: edgesJSON}
	err = p.db.Create(&inner).Error
	p.mu.Unlock()
	if err != nil {
		return nil, 0, fmt.Errorf("质量循环临时图写入失败：%w", err)
	}

	prompt, _ := built.Values["prompt"].(string)
	outer := &service.BuiltPipeline{
		Pipeline: built.Pipeline,
		Values:   built.Values,
		Nodes: []model.NodeInstance{
			{ID: "start-1", Type: "start", Data: map[string]interface{}{"prompt": prompt}, Status: "pending"},
			{ID: "ql-1", Type: "quality_loop", Status: "pending", Data: map[string]interface{}{
				"workflow_id":  float64(inner.ID),
				"max_rounds":   float64(rounds),
				"target_score": float64(100),
				"repair":       "auto",
				"input":        prompt,
			}},
			{ID: "end-1", Type: "end", Data: map[string]interface{}{}, Status: "pending"},
		},
		Edges: []model.EdgeInstance{
			{ID: "e1", Source: "start-1", Target: "ql-1"},
			{ID: "e2", Source: "ql-1", Target: "end-1"},
		},
	}
	return outer, inner.ID, nil
}

func (p *Producer) dropTransient(id uint) {
	if id == 0 {
		return
	}
	p.db.Delete(&model.Workflow{}, id)
}

// preflight 现场探测依赖并核对节点需求。
func (p *Producer) preflight(nodes []model.NodeInstance, edges []model.EdgeInstance) []service.DepRequirement {
	if p.sup == nil {
		return nil
	}
	caps := service.ProbeCapabilities(nil, "", p.ollamaBase, p.comfyBase, p.comfyH3Base, p.asrBase, p.ttsBase)
	return service.PreflightDeps(nodes, caps, p.sup)
}

// preflightText 把缺失清单压成一句话，每项带修复动作。
func preflightText(missing []service.DepRequirement) string {
	parts := make([]string, 0, len(missing))
	for _, m := range missing {
		item := m.Label
		if m.Fix != "" {
			item += " → " + m.Fix
		}
		parts = append(parts, item)
	}
	return "依赖服务未就绪，未开始出片：" + strings.Join(parts, "、")
}

// normalizeInvoker 只接受已知调用方；未知一律记 manual（不猜）。
func normalizeInvoker(v string) string {
	switch v {
	case "workbuddy", "dsh", "mcp", "manual":
		return v
	case "":
		return "manual"
	}
	return "manual"
}

// PreflightPipelines 出片前体检：按流水线+字段组装一次图，只探测依赖不运行。
//
// warnings 是「能跑，但你先看一眼」的那类提醒：显存会不会换页、同参数是不是刚跑过。
// 它们**不拦**出片（拦住等于把"慢"升级成"不可用"），只是把代价说在前面。
func (p *Producer) PreflightPipelines(c *gin.Context) {
	var req produceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体需要 {pipeline, params}"})
		return
	}
	built, err := p.pipes.Build(req.Pipeline, req.Params)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	missing := p.preflight(built.Nodes, built.Edges)
	if missing == nil {
		// nil 切片会被序列化成 null，前端/智能体拿到 null 会当"字段不存在"。
		// 「什么都不缺」必须是空数组，不能是 null。
		missing = []service.DepRequirement{}
	}
	fixable := 0
	for _, m := range missing {
		if m.Fixable {
			fixable++
		}
	}
	warnings := p.preflightWarnings(built)
	// 区间估算（V3 预算门）：把 EstimateGraph 接到流水线入口。
	// 以前它只在画布的 /graph/dry-run 用，于是「这条片子要多久」在出片路径上没人答得出来。
	est := service.EstimateGraph(built.Nodes, built.Edges)
	c.JSON(http.StatusOK, gin.H{
		"ok":       len(missing) == 0,
		"missing":  missing,
		"fixable":  fixable,
		"warnings": warnings,
		"estimate": est,
	})
}

// preflightWarnings 组装「能跑但不便宜」的提醒。全部是**建议**，不改变 ok。
func (p *Producer) preflightWarnings(built *service.BuiltPipeline) []string {
	out := []string{}
	if built == nil {
		return out
	}

	// 1) 显存前瞻：换页跑法能出片，但实测同一镜 85s → 2269s。
	if free := service.FreeVRAMForNodes(built.Nodes, p.comfyBase, p.comfyH3Base); free > 0 {
		if adv := service.VRAMAdvisoryForNodes(built.Nodes, free); adv.Level != service.VRAMLevelOK {
			out = append(out, adv.Message)
		}
	}

	// 2) 端口是我们的，进程不是我们的：同卡上可能还跑着一份同样的实例。
	if p.sup != nil {
		dep := "comfyui"
		if service.UsesH3(built.Nodes) {
			dep = "comfy_h3"
		}
		if p.sup.ServingButUnmanaged(dep) {
			out = append(out, fmt.Sprintf(
				"%s 的端口由非引擎托管的实例提供服务。若同卡上还有一份同样的实例，"+
					"显存会被分掉，单镜可能慢 10–27 倍并撞超时——先确认端口上只有一个实例", dep))
		}
	}

	// 3) 同参数刚成功过：用户很可能是「忘了已出过」或「重复点了两次」。
	//    实测 208/209 就是同一份指纹连跑两次（各约 4 分钟 GPU）。
	if dupID, when := p.findDuplicateRun(built.Pipeline.ID, marshalParams(built.Values)); dupID > 0 {
		out = append(out, fmt.Sprintf(
			"同一条指令在 %s 已成功出片（run %d）——确认要再跑一次吗？"+
				"同样的参数会得到不同的采样结果，但每次都是全额 GPU 时间", when, dupID))
	}

	return out
}

// findDuplicateRun 找同一流水线、同一份参数下最近一次**成功**的运行。
//
// 为什么按 Go 侧比对而不是丢给 SQL：sqlite 里 params 是 blob，
// 直接字符串相等会被键顺序/空白搞成假阴性；解成 interface{} 再比才准。
// 只看最近 30 条成功的，够用且不会随库变大而变慢。
func (p *Producer) findDuplicateRun(pipeline string, params []byte) (uint, string) {
	if p.db == nil || len(params) == 0 {
		return 0, ""
	}
	var runs []model.WorkflowRun
	if err := p.db.Where("pipeline_id = ? AND status = ?", pipeline, "success").
		Order("id desc").Limit(30).Find(&runs).Error; err != nil {
		return 0, ""
	}
	for _, r := range runs {
		if !jsonEqualRaw(r.Params, params) {
			continue
		}
		when := "（时间未知）"
		if r.StartedAt != nil {
			when = r.StartedAt.Format("01-02 15:04")
		}
		return r.ID, when
	}
	return 0, ""
}

// marshalParams 与 start 里落库的口径保持一致（同一份 Values 序列化成同一串）。
func marshalParams(v map[string]interface{}) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// jsonEqualRaw 语义比较两段 JSON（键顺序、空白不影响结果）。
func jsonEqualRaw(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var va, vb interface{}
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// GetRun 单次出片详情：状态 + 节点终态 + 生效参数。
func (p *Producer) GetRun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	var run model.WorkflowRun
	if err := p.db.First(&run, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	var params interface{}
	if len(run.Params) > 0 {
		_ = json.Unmarshal(run.Params, &params)
	}
	c.JSON(http.StatusOK, gin.H{
		"run_id":      run.ID,
		"pipeline":    run.PipelineID,
		"status":      run.Status,
		"error":       run.Error,
		"invoked_by":  run.InvokedBy,
		"params":      params,
		"node_states": json.RawMessage(run.NodeStates),
		"started_at":  run.StartedAt,
		"ended_at":    run.EndedAt,
		"created_at":  run.CreatedAt,
	})
}

// CancelRun 中止进行中的出片。
//
// 两条路都要走通（2026-10-27 复盘）：
//   - 正在执行 → 取消 ctx + 让 ComfyUI 立刻停手（引擎侧，见 EngineV2.CancelRun）
//   - **还在排队** → 从队列里撤掉。旧实现只认前者，对排队中的运行直接 409
//     「已结束或未在进行中」；用户以为停了，20 分钟后它照样开跑、照样烧卡。
//     这和第 2.2 条「/interrupt 对排队中的 prompt 无效」是同一个错，只是高了一层。
func (p *Producer) CancelRun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	runID := uint(id)
	if p.engine != nil && p.engine.CancelRun(runID) {
		c.JSON(http.StatusOK, gin.H{"run_id": id, "status": "cancelling"})
		return
	}
	if p.queue != nil && p.queue.Cancel(runID) {
		now := time.Now()
		p.db.Model(&model.WorkflowRun{}).Where("id = ?", runID).Updates(map[string]interface{}{
			"status":   "cancelled",
			"error":    "排队中被用户取消（未开始执行，未占用显存）",
			"ended_at": now,
		})
		log.Printf("[produce] run=%d 在排队阶段被取消", runID)
		c.JSON(http.StatusOK, gin.H{"run_id": id, "status": "cancelled", "dequeued": true})
		return
	}
	c.JSON(http.StatusConflict, gin.H{"error": "该运行已结束、不在执行中，也不在队列里"})
}

// Rerun 用同一份参数再跑一次（换了字段就用 produce）。
func (p *Producer) Rerun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("runId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	var old model.WorkflowRun
	if err := p.db.First(&old, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	var params map[string]interface{}
	if len(old.Params) > 0 {
		if err := json.Unmarshal(old.Params, &params); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "这条运行没有可复用的参数"})
			return
		}
	}
	// 画布图的运行没有 params，重跑就是"再跑同一张图"。
	// 不处理这条会把用户送进 "没有这条流水线：graph:12" 这种莫名其妙的分支。
	if wfID, ok := GraphWorkflowID(old.PipelineID); ok {
		run, status, msg := p.runGraph(wfID, old.InvokedBy, false, false, 0)
		if msg != "" {
			c.JSON(status, gin.H{"error": msg})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"run_id": run.ID, "pipeline": run.PipelineID, "status": run.Status})
		return
	}
	run, status, msg := p.start(produceRequest{
		Pipeline:  old.PipelineID,
		Params:    params,
		InvokedBy: old.InvokedBy,
	})
	if msg != "" {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"run_id": run.ID, "pipeline": run.PipelineID, "status": run.Status})
}
