package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 画布面：节点目录、工作流 CRUD、图体检、干跑。
//
// 设计边界（与 docs/画布与流水线统一-20261029.md 对应）：
//   - 画布与出片台是**同一份数据**（workflows 表）的两个视图，
//     流水线只是"命好名、填好默认值的预置图"，由 pipelines.json 反向生成，不入库；
//   - 保存与运行前必须过三层校验（结构 / 端口类型 / 参数），error 级一律拒绝；
//   - 估算永不返回单值（AGENTS §0.3）；
//   - 图里出现的节点类型必须能在 node-catalog 里查到，否则拒绝保存——
//     这一条是 v1 "unknown node type 跑到一半才炸" 的正面修法。
type GraphHandler struct {
	db    *gorm.DB
	pipes *service.PipelineStore

	// 依赖预检（SetProbe/SetSupervisor 注入；nil 时干跑只报结构与类型问题）
	probe func() service.Capabilities
	sup   *service.Supervisor
}

func NewGraphHandler(db *gorm.DB, pipes *service.PipelineStore) *GraphHandler {
	return &GraphHandler{db: db, pipes: pipes}
}

// SetProbe 注入能力探测（与 /api/preflight 用同一个探测器，保证口径一致）。
func (g *GraphHandler) SetProbe(fn func() service.Capabilities) { g.probe = fn }

// SetSupervisor 注入依赖托管器（判断 ComfyUI 实例能不能被拉起）。
func (g *GraphHandler) SetSupervisor(sup *service.Supervisor) { g.sup = sup }

// ---- 节点目录 ----

// Catalog 调色板的唯一数据源：类型 / 显存侧别 / 端口 / 字段 / 成本模型。
// 前端不许再硬编码任何节点类型——这是 v1 元数据双写的正解。
func (g *GraphHandler) Catalog(c *gin.Context) {
	catalog := service.NodeCatalog()
	c.JSON(http.StatusOK, gin.H{
		"nodes": catalog,
		"count": len(catalog),
	})
}

// ---- 工作流 CRUD ----

// 画布上新建图的请求体。
type graphBody struct {
	Name  string               `json:"name"`
	Nodes []model.NodeInstance `json:"nodes"`
	Edges []model.EdgeInstance `json:"edges"`
	// Origin 允许 user / agent。默认 user；agent 建的图默认不自动运行（由调用方决定）。
	Origin string `json:"origin,omitempty"`
}

const (
	originUser  = "user"
	originAgent = "agent"
	// transientPrefix 质量循环临时图的保留前缀；画布不许占用。
	transientPrefix = "qc:"
)

func normalizeOrigin(s string) (string, bool) {
	switch strings.TrimSpace(s) {
	case "", originUser:
		return originUser, true
	case originAgent:
		return originAgent, true
	}
	return "", false
}

// validateForSave 保存前的守门：名字、空图、重复节点、三层校验。
// 返回 (报告, 面向用户的一段错误话)；错误话为空表示可以保存。
func validateForSave(body *graphBody) (service.GraphReport, string) {
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return service.GraphReport{}, "工作流需要一个名字"
	}
	if strings.HasPrefix(name, transientPrefix) {
		return service.GraphReport{}, "名字不能以 " + transientPrefix + " 开头：这是质量循环临时图的保留前缀"
	}
	if len(body.Nodes) == 0 {
		return service.GraphReport{}, "图里一个节点都没有"
	}

	seen := make(map[string]bool, len(body.Nodes))
	for _, n := range body.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			return service.GraphReport{}, "有节点缺少 id"
		}
		if seen[n.ID] {
			return service.GraphReport{}, fmt.Sprintf("节点 id 重复：%s（id 是连线的唯一依据）", n.ID)
		}
		seen[n.ID] = true
	}

	rep := service.ValidateGraph(body.Nodes, body.Edges)
	if !rep.OK {
		msg := "图有问题，不能保存"
		if len(rep.BlockingCodes) > 0 {
			msg += "（" + strings.Join(rep.BlockingCodes, ", ") + "）"
		}
		first := firstBlockingMessage(rep)
		if first != "" {
			msg += "：" + first
		}
		return rep, msg
	}
	return rep, ""
}

func firstBlockingMessage(rep service.GraphReport) string {
	for _, i := range rep.DAG {
		if i.Level == "error" {
			return i.Message
		}
	}
	for _, i := range rep.Ports {
		if i.Level == "error" {
			return i.Message
		}
	}
	if len(rep.Params) > 0 {
		return rep.Params[0].Message
	}
	return ""
}

func marshalGraph(body *graphBody) ([]byte, []byte, error) {
	nodes, err := json.Marshal(body.Nodes)
	if err != nil {
		return nil, nil, err
	}
	edges, err := json.Marshal(body.Edges)
	if err != nil {
		return nil, nil, err
	}
	return nodes, edges, nil
}

// CreateWorkflow 保存一张新图。
func (g *GraphHandler) CreateWorkflow(c *gin.Context) {
	var body graphBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON：" + err.Error()})
		return
	}
	if _, msg := validateForSave(&body); msg != "" {
		rep := service.ValidateGraph(body.Nodes, body.Edges)
		c.JSON(http.StatusBadRequest, gin.H{"error": msg, "report": rep})
		return
	}
	origin, ok := normalizeOrigin(body.Origin)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "origin 只能是 user 或 agent"})
		return
	}
	// 坐标缺失就自动分层布局：模板图与 API 建图都没有坐标，
	// 堆在原点等于把用户扔回"一片乱麻"。
	service.AutoLayout(body.Nodes, body.Edges)

	nodesJSON, edgesJSON, err := marshalGraph(&body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "序列化失败：" + err.Error()})
		return
	}
	wf := model.Workflow{
		Name: strings.TrimSpace(body.Name), Nodes: nodesJSON, Edges: edgesJSON,
		Origin: origin, SchemaVersion: 1,
	}
	if err := g.db.Create(&wf).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败：" + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": wf.ID, "name": wf.Name, "origin": wf.Origin})
}

// ListWorkflows 列出用户图（不含质量循环临时图）。
func (g *GraphHandler) ListWorkflows(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}
	var total int64
	q := g.db.Model(&model.Workflow{}).Where("name NOT LIKE ?", transientPrefix+"%")
	q.Count(&total)

	var wfs []model.Workflow
	if err := q.Order("updated_at desc").Limit(limit).Offset(offset).Find(&wfs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	items := make([]gin.H, 0, len(wfs))
	for _, wf := range wfs {
		var nodes []model.NodeInstance
		var edges []model.EdgeInstance
		_ = json.Unmarshal(wf.Nodes, &nodes)
		_ = json.Unmarshal(wf.Edges, &edges)
		items = append(items, gin.H{
			"id": wf.ID, "name": wf.Name, "origin": wf.Origin,
			"schema_version": wf.SchemaVersion,
			"node_count":     len(nodes), "edge_count": len(edges),
			"updated_at": wf.UpdatedAt, "created_at": wf.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"workflows": items, "total": total, "count": len(items)})
}

// GetWorkflow 读一张图的完整定义。
func (g *GraphHandler) GetWorkflow(c *gin.Context) {
	wf, ok := g.loadWorkflow(c)
	if !ok {
		return
	}
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "库里的 nodes 不是合法 JSON：" + err.Error()})
		return
	}
	if err := json.Unmarshal(wf.Edges, &edges); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "库里的 edges 不是合法 JSON：" + err.Error()})
		return
	}
	rep := service.ValidateGraph(nodes, edges)
	c.JSON(http.StatusOK, gin.H{
		"id": wf.ID, "name": wf.Name, "origin": wf.Origin,
		"schema_version": wf.SchemaVersion,
		"nodes":          nodes, "edges": edges,
		"report": rep,
		"created_at": wf.CreatedAt, "updated_at": wf.UpdatedAt,
	})
}

// UpdateWorkflow 覆盖保存一张图。
func (g *GraphHandler) UpdateWorkflow(c *gin.Context) {
	wf, ok := g.loadWorkflow(c)
	if !ok {
		return
	}
	// 临时图只属于某一次运行，改它等于改正在跑的 run。
	if strings.HasPrefix(wf.Name, transientPrefix) {
		c.JSON(http.StatusForbidden, gin.H{"error": "质量循环临时图不许直接编辑"})
		return
	}
	var body graphBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON：" + err.Error()})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		body.Name = wf.Name // 允许只改图不改名
	}
	origin := wf.Origin
	if body.Origin != "" {
		o, ok := normalizeOrigin(body.Origin)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "origin 只能是 user 或 agent"})
			return
		}
		origin = o
	}
	if _, msg := validateForSave(&body); msg != "" {
		rep := service.ValidateGraph(body.Nodes, body.Edges)
		c.JSON(http.StatusBadRequest, gin.H{"error": msg, "report": rep})
		return
	}
	service.AutoLayout(body.Nodes, body.Edges)
	nodesJSON, edgesJSON, err := marshalGraph(&body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "序列化失败：" + err.Error()})
		return
	}
	if err := g.db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Updates(map[string]interface{}{
		"name": strings.TrimSpace(body.Name), "nodes": nodesJSON, "edges": edgesJSON, "origin": origin,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败：" + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": wf.ID, "name": strings.TrimSpace(body.Name), "origin": origin})
}

// DeleteWorkflow 删一张图。临时图禁删（会让正在跑的 run 找不到内层图）。
func (g *GraphHandler) DeleteWorkflow(c *gin.Context) {
	wf, ok := g.loadWorkflow(c)
	if !ok {
		return
	}
	if strings.HasPrefix(wf.Name, transientPrefix) {
		c.JSON(http.StatusForbidden, gin.H{"error": "质量循环临时图由引擎自动清理，不许手动删"})
		return
	}
	if err := g.db.Delete(&model.Workflow{}, wf.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": wf.ID})
}

func (g *GraphHandler) loadWorkflow(c *gin.Context) (model.Workflow, bool) {
	var wf model.Workflow
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "工作流 id 必须是正整数"})
		return wf, false
	}
	// 用 Limit(1).Find 而不是 First：First 找不到会打一条 "record not found" 噪音日志，
	// 而「打开一个已删除的图」是正常路径（用户在历史列表里点旧记录）。
	if res := g.db.Limit(1).Find(&wf, id); res.Error != nil || wf.ID == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("没有 id=%d 的工作流", id)})
		return wf, false
	}
	return wf, true
}

// ---- 起步模板（不入库，按 pipelines.json 现生成） ----

// Templates 把每条流水线反向组装成一张图，作为画布的起步模板。
//
// 为什么不入库：pipelines.json 一改，库里的模板就是旧的 —— 那正是"模板与产品
// 漂移"的来源。现生成永远不会过期，代价只有一次 Build（毫秒级）。
// 必填文本字段会填一句占位提示，否则 Build 会因缺必填直接失败。
func (g *GraphHandler) Templates(c *gin.Context) {
	type tpl struct {
		PipelineID string               `json:"pipeline_id"`
		Name       string               `json:"name"`
		Summary    string               `json:"summary"`
		Output     string               `json:"output"`
		EstSeconds int                  `json:"est_seconds,omitempty"`
		Nodes      []model.NodeInstance `json:"nodes"`
		Edges      []model.EdgeInstance `json:"edges"`
		Report     service.GraphReport  `json:"report"`
	}
	out := make([]tpl, 0, 8)
	for _, p := range g.pipes.List() {
		built, err := g.pipes.Build(p.ID, templateDefaults(p))
		if err != nil {
			// 组装不出来是流水线定义本身的问题（pipeline_test 会先红），
			// 这里不吞掉：以一条不可用的模板返回，界面上能看见原因。
			out = append(out, tpl{PipelineID: p.ID, Name: p.Name, Summary: err.Error(), Output: p.Output})
			continue
		}
		service.AutoLayout(built.Nodes, built.Edges)
		out = append(out, tpl{
			PipelineID: p.ID, Name: p.Name, Summary: p.Summary, Output: p.Output,
			EstSeconds: p.EstSeconds, Nodes: built.Nodes, Edges: built.Edges,
			Report: service.ValidateGraph(built.Nodes, built.Edges),
		})
	}
	c.JSON(http.StatusOK, gin.H{"templates": out, "count": len(out)})
}

// templateDefaults 模板的默认参数：用字段默认值，必填文本补一句占位。
func templateDefaults(p service.Pipeline) map[string]interface{} {
	params := make(map[string]interface{}, len(p.Fields))
	for _, f := range p.Fields {
		if f.Default != nil {
			params[f.Key] = f.Default
			continue
		}
		if f.Required && (f.Type == "text" || f.Type == "textarea" || f.Type == "path") {
			params[f.Key] = "（从模板新建：在这里填「" + f.Label + "」）"
		}
	}
	return params
}

// ---- 图体检 / 干跑 ----

type graphRequest struct {
	Nodes []model.NodeInstance `json:"nodes"`
	Edges []model.EdgeInstance `json:"edges"`
}

func bindGraph(c *gin.Context) (graphRequest, bool) {
	var body graphRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON：" + err.Error()})
		return body, false
	}
	return body, true
}

// Validate 只体检、不接触任何依赖。200 + ok 布尔：
// 这是"查询"不是"提交"，把问题报出来才是它的职责，用 4xx 反而让前端丢报告。
func (g *GraphHandler) Validate(c *gin.Context) {
	body, ok := bindGraph(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, service.ValidateGraph(body.Nodes, body.Edges))
}

// DryRun 干跑：体检 + 依赖预检 + 成本估算，**不提交、不占卡、不改任何进程状态**。
func (g *GraphHandler) DryRun(c *gin.Context) {
	body, ok := bindGraph(c)
	if !ok {
		return
	}
	rep := service.ValidateGraph(body.Nodes, body.Edges)
	estimate := service.EstimateGraph(body.Nodes, body.Edges)

	var deps []service.DepRequirement
	if g.probe != nil {
		deps = service.PreflightDeps(body.Nodes, g.probe(), g.sup)
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":          rep.OK && len(deps) == 0,
		"run_ready":   rep.OK && len(deps) == 0,
		"report":      rep,
		"deps":        deps,
		"estimate":    estimate,
		"probe_state": probeState(g.probe),
	})
}

// probeState 明确区分「没探测」与「探测了没问题」——
// AGENTS §0.6：不可达 ≠ 空结果。没注入探测器时不能说"依赖都齐了"。
func probeState(probe func() service.Capabilities) string {
	if probe == nil {
		return "skipped"
	}
	return "probed"
}
