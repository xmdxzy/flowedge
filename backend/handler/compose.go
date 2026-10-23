package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 构图出口：把一句话目标编译成可执行的图 + 任务书。
//
// 这一条补的是 FlowEdge 一直缺的半条腿 —— 以前图必须人画（spec 只能"从图生成规格"）。
//
// 三条边界（与 service/compose.go 的注释同一套理由）：
//   1. 不自动 run
//   2. 不自动存库（要存就传 save=true，且存的是草稿，跑之前人还能改）
//   3. 返回必带任务书 —— 让人先看"这张图要干什么"，再决定跑不跑
type ComposeHandler struct {
	engine *service.EngineV2
	db     *gorm.DB
}

func NewComposeHandler(engine *service.EngineV2, db *gorm.DB) *ComposeHandler {
	return &ComposeHandler{engine: engine, db: db}
}

// Compose POST /api/workflows/compose
//
// body:
//
//	{ "goal":"30 秒竖屏修仙短片，夜雨，无台词",
//	  "totalSeconds":30, "aspect":"9:16", "noSpeech":true,
//	  "maxRepairRounds":2, "maxNodes":10,
//	  "useLessons":true,     // 默认 true：从知识库检索相关经验
//	  "save":false }         // true 则存成草稿工作流
func (h *ComposeHandler) Compose(c *gin.Context) {
	// 不用嵌入 service.ComposeGoal：它有个字段也叫 Goal，嵌入后 req.Goal 会被
	// 提升成 string 字段而不是子结构，编译期就报 "type string has no field Goal"。
	var req struct {
		Goal            string  `json:"goal"`
		TotalSeconds    float64 `json:"totalSeconds,omitempty"`
		Aspect          string  `json:"aspect,omitempty"`
		NoSpeech        *bool   `json:"noSpeech,omitempty"`
		MaxRepairRounds int     `json:"maxRepairRounds,omitempty"`
		MaxNodes        int     `json:"maxNodes,omitempty"`
		UseLessons      *bool   `json:"useLessons,omitempty"`
		Save            bool    `json:"save,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if strings.TrimSpace(req.Goal) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "goal 不能为空"})
		return
	}

	var lessons []string
	useLessons := req.UseLessons == nil || *req.UseLessons
	if useLessons {
		// 用目标原文检索经验卡片。检索失败不阻断构图（RAGQuery 已吞错返回空）。
		lessons = h.engine.RAGQuery(req.Goal, 4)
	}

	goal := service.ComposeGoal{
		Goal: req.Goal, TotalSeconds: req.TotalSeconds, Aspect: req.Aspect,
		NoSpeech: req.NoSpeech, MaxRepairRounds: req.MaxRepairRounds, MaxNodes: req.MaxNodes,
	}
	res, err := service.ComposeGraph(goal, lessons, h.engine.ComposeLLM())
	if err != nil {
		// 失败时把最后一版的违规清单与原始输出一起给出去 —— 这是调提示词的材料，
		// 只说"失败"等于让人去猜。
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":      err.Error(),
			"rounds":     res.Rounds,
			"violations": res.Violations,
			"raw":        res.Raw,
			"hint":       "可以把 violations 贴回去重试，或调大 maxRepairRounds（上限 4）",
		})
		return
	}

	out := gin.H{
		"rounds":     res.Rounds,
		"legal":      res.Legal,
		"score":      res.Score,
		"summary":    res.Score.Summarize(),
		"violations": res.Violations,
		"lessons":    res.Lessons,
		"nodeCount":  len(res.Nodes),
		"edgeCount":  len(res.Edges),
	}
	// 任务书一并返回（§3.6.5 的判据：给的不是一张图，是"一张图 + 一份可验收的规格"）。
	// 编译失败不影响返回图 —— 任务书是附加信息，不是生成的前置条件。
	if spec, err := compileDraftSpec(res.Nodes, res.Edges, goal.Goal); err == nil {
		out["spec"] = spec
	} else {
		out["specError"] = err.Error()
	}

	if !req.Save {
		// 不存库时把图原样返回，前端可先渲染成草稿再决定
		out["draft"] = gin.H{"nodes": res.Nodes, "edges": res.Edges}
		out["hint"] = "未保存。确认无误后传 save=true，或把 draft 放进 POST /api/workflows 自己存"
		c.JSON(http.StatusOK, out)
		return
	}

	wf, err := h.saveDraft(c, req.Goal, res.Nodes, res.Edges)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "存草稿失败：" + err.Error()})
		return
	}
	out["workflowId"] = wf.ID
	out["hint"] = "已存为草稿。先看任务书确认，再 POST /api/workflows/" + itoa(int(wf.ID)) + "/run"
	c.JSON(http.StatusCreated, out)
}

// compileDraftSpec 把草稿图编译成任务书（markdown）。
func compileDraftSpec(nodes []model.NodeInstance, edges []model.EdgeInstance, goal string) (string, error) {
	nj, err := json.Marshal(nodes)
	if err != nil {
		return "", err
	}
	ej, err := json.Marshal(edges)
	if err != nil {
		return "", err
	}
	wf := &model.Workflow{Name: goal, Nodes: nj, Edges: ej}
	doc, err := service.CompileSpec(wf, "md")
	if err != nil {
		return "", err
	}
	return doc.Markdown, nil
}

// saveDraft 存成工作流草稿。
// 自动布局：LLM 不输出 position（那是无意义的负担），按拓扑层排。
func (h *ComposeHandler) saveDraft(_ *gin.Context, goal string, nodes []model.NodeInstance, edges []model.EdgeInstance) (*model.Workflow, error) {
	laid := autoLayoutComposed(nodes, edges)
	nodesJSON, err := json.Marshal(laid)
	if err != nil {
		return nil, err
	}
	edgesJSON, err := json.Marshal(edges)
	if err != nil {
		return nil, err
	}
	name := goal
	if r := []rune(name); len(r) > 24 {
		name = string(r[:24]) + "…"
	}
	wf := model.Workflow{Name: "构图：" + name, Nodes: nodesJSON, Edges: edgesJSON}
	if err := h.db.Create(&wf).Error; err != nil {
		return nil, err
	}
	return &wf, nil
}

// autoLayoutComposed 按拓扑层横向排布，层内纵向居中。
//
// 为什么要布局：前端 React Flow 需要 position，缺了会全部叠在 (0,0)。
// 但**不该让 LLM 算坐标** —— 它算不准，而且坐标与图正确性无关。
func autoLayoutComposed(nodes []model.NodeInstance, edges []model.EdgeInstance) []model.NodeInstance {
	// 复用 spec 的分层结果（同一套拓扑规则，不另写一份）
	layers, _ := service.TopologyLayers(nodes, edges)
	layerOf := map[string]int{}
	for i, layer := range layers {
		for _, id := range layer {
			layerOf[id] = i
		}
	}

	const colW, rowH = 260, 130
	countInLayer := map[int]int{}
	for i := range nodes {
		l := layerOf[nodes[i].ID]
		idx := countInLayer[l]
		countInLayer[l] = idx + 1
		nodes[i].Position = map[string]float64{"x": 80 + float64(l*colW), "y": 80 + float64(idx*rowH)}
	}
	return nodes
}
