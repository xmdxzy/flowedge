package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 图结构评分出口。
//
// 两种用法，对应两个不同时机：
//
//	{ "workflow_id": 57 }                → 评一条已存的工作流（看自己的图健不健康）
//	{ "nodes": [...], "edges": [...] }   → 评一张还没存图的草稿（compose 生成后先验）
//
// 第二种是关键：生成式构图必须先过结构评分再决定要不要落库，
// 否则库里会堆满跑不起来的图。
type GraphHandler struct {
	db *gorm.DB
}

func NewGraphHandler(db *gorm.DB) *GraphHandler { return &GraphHandler{db: db} }

// Score POST /api/graph/score
func (h *GraphHandler) Score(c *gin.Context) {
	var req struct {
		WorkflowID *uint             `json:"workflow_id"`
		Nodes      []json.RawMessage `json:"nodes"`
		Edges      []json.RawMessage `json:"edges"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}

	var nodesJSON, edgesJSON []byte

	switch {
	case req.WorkflowID != nil:
		var wf model.Workflow
		if err := h.db.First(&wf, *req.WorkflowID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
		nodesJSON, edgesJSON = wf.Nodes, wf.Edges
	case len(req.Nodes) > 0:
		var err error
		nodesJSON, err = json.Marshal(req.Nodes)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nodes 序列化失败"})
			return
		}
		edgesJSON, _ = json.Marshal(req.Edges)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "要么给 workflow_id，要么给 nodes/edges"})
		return
	}

	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "节点数据不合法：" + err.Error()})
		return
	}
	_ = json.Unmarshal(edgesJSON, &edges)

	sc := service.ScoreGraph(nodes, edges)
	c.JSON(http.StatusOK, gin.H{
		"score":     sc,
		"summary":   sc.Summarize(),
		"legal":     sc.Legal,
		"nodeCount": len(nodes),
		"edgeCount": len(edges),
	})
}
