package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 能力卡片目录：智能体卡 / 技能卡 / 工作流封装卡。
//
// 三类同源同出口，但**语义不同**，前端必须分开呈现：
//   - agent / skill 来自 CardStore（内置 + data/cards.json），只影响提示词与工具；
//   - workflow 来自 workflows 表，贴到节点上会**改执行结构**（换成子工作流调用）。
//
// 把 workflow 也放进同一个接口，是因为用户的动作是一样的（"给这个节点贴一张卡"），
// 至于背后是改提示词还是改结构，由引擎按 kind 分派，不该让用户在兩個入口之间找。

type CardHandler struct {
	db    *gorm.DB
	store *service.CardStore
}

func NewCardHandler(db *gorm.DB, s *service.CardStore) *CardHandler {
	return &CardHandler{db: db, store: s}
}

type workflowCard struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Nodes int    `json:"nodes"`
	Desc  string `json:"desc"`
}

// List GET /api/cards
func (h *CardHandler) List(c *gin.Context) {
	agents := h.store.Agents()
	skills := h.store.Skills()

	var wfs []workflowCard
	if h.db != nil {
		var rows []model.Workflow
		if err := h.db.Select("id", "name", "nodes").Order("name").Find(&rows).Error; err == nil {
			for _, w := range rows {
				n := 0
				var arr []map[string]interface{}
				if len(w.Nodes) > 0 {
					if jsonUnmarshal(w.Nodes, &arr) == nil {
						n = len(arr)
					}
				}
				wfs = append(wfs, workflowCard{
					ID:    uintToString(w.ID),
					Name:  w.Name,
					Nodes: n,
					Desc:  "整条工作流封装：贴到节点上，该节点变成这条流水线的入口",
				})
			}
		}
	}
	if wfs == nil {
		wfs = []workflowCard{}
	}

	// 大类清单：前端据此渲染筛选条，不用自己从卡片里猜
	categories := map[string][]string{
		service.CardAgent:    h.store.CategoriesOf(service.CardAgent),
		service.CardSkill:    h.store.CategoriesOf(service.CardSkill),
		service.CardWorkflow: workflowCategories(wfs),
	}

	c.JSON(http.StatusOK, gin.H{
		"agents":     agents,
		"skills":     skills,
		"workflows":  wfs,
		"categories": categories,
		// 每种卡贴上去会发生什么——前端不用自己编这段说明
		"effects": map[string]string{
			service.CardAgent:    "该节点改用这张卡的角色设定与默认工具（提示词以卡片为准）",
			service.CardSkill:    "该节点执行一次确定性变换（同输入必同输出）",
			service.CardWorkflow: "该节点被替换成一次子工作流调用（会改执行结构）",
		},
	})
}

// workflowCategories 工作流按"能不能跑得动"粗分两类。
// 不做语义分类（名字里有什么就归什么），那是猜；只按节点数给出规模提示。
func workflowCategories(wfs []workflowCard) []string {
	small, big := false, false
	for _, w := range wfs {
		if w.Nodes <= 5 {
			small = true
		} else {
			big = true
		}
	}
	out := []string{}
	if small {
		out = append(out, "轻量（≤5 节点）")
	}
	if big {
		out = append(out, "完整流水线（>5 节点）")
	}
	return out
}

// Save PUT /api/cards —— 整份替换（面板「管理」里的保存）。
//
// 为什么不提供 POST 单卡 / DELETE 单卡：卡片清单是一份**配置**，
// 逐条增删会引入"本地状态与文件不一致"的窗口；整份替换只有一种结果状态。
func (h *CardHandler) Save(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "卡片仓库未初始化"})
		return
	}
	var req struct {
		Agents []service.AgentCard `json:"agents"`
		Skills []service.SkillCard `json:"skills"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON：" + err.Error()})
		return
	}
	if err := h.store.SaveCards(req.Agents, req.Skills); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"agents": h.store.Agents(),
		"skills": h.store.Skills(),
	})
}
