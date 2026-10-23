package handler

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// 模块目录出口：把「一个模块是什么」一次性说清。
//
// 这是「万物皆可模块」的对外形态——逻辑模块（条件分支、循环）与模态模块
// （图、视、音、文本）在同一份清单里，字段完全一致：
//
//	端口（Inputs/Outputs） + 参数标签（KeyParams） + 门层（GateLayers） + 执行元数据（side/heavy/media）
//
// 前端要做"数据驱动卡片"时读它；智能体要"知道自己能用什么积木"时也读它。
// 加一个模态只要在 Modules 里加一条，这里自动出现，**不需要改这个文件**。

type ModuleView struct {
	Type       string             `json:"type"`
	Title      string             `json:"title"`
	Role       string             `json:"role"`
	Inputs     []service.Port     `json:"inputs"`
	Outputs    []service.Port     `json:"outputs"`
	KeyParams  []service.KeyParam `json:"key_params"`
	GateLayers []string           `json:"gate_layers,omitempty"`
	Agent      string             `json:"agent,omitempty"`
	// 执行元数据（来自 NodeSpecs，调度器读的那份）
	Side  string `json:"side,omitempty"`
	Heavy bool   `json:"heavy"`
	Media bool   `json:"media"`
}

type ModuleHandler struct{}

func NewModuleHandler() *ModuleHandler { return &ModuleHandler{} }

// List GET /api/modules
func (h *ModuleHandler) List(c *gin.Context) {
	views := make([]ModuleView, 0, len(service.Modules))
	for typ, ct := range service.Modules {
		v := ModuleView{
			Type: typ, Title: ct.Title, Role: string(ct.Role),
			Inputs: ct.Inputs, Outputs: ct.Outputs,
			KeyParams: ct.KeyParams, GateLayers: ct.GateLayers, Agent: ct.Agent,
		}
		if s, ok := service.NodeSpecOf(typ); ok {
			v.Side, v.Heavy, v.Media = s.Side, s.Heavy, s.Media
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Type < views[j].Type })

	c.JSON(http.StatusOK, gin.H{
		"count":   len(views),
		"modules": views,
		// 端口类型是"模态"在整个系统里唯一出现的地方
		"port_kinds": []string{"text", "image", "video", "audio", "json", "any"},
	})
}
