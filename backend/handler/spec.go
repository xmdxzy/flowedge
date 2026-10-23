package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 任务书（Spec）出口。
//
// 定位：**画布 → 可执行规格**的编译结果。给人看是 Markdown，给智能体看是 JSON。
// 两类消费方共用同一份编译结果，避免"文档一套、API 一套"再次漂移。
//
// 为什么挂在 handler 层而不是让前端自己拼：
//   - 拓扑分层、端口类型、门的阈值都在后端有唯一事实来源；
//   - MCP / 脚本 / 外部 agent 都能直接取，不必依赖浏览器；
//   - 前端只负责"下载"这一个动作。

// Spec GET /api/workflows/:id/spec?format=md|json
//
// format=md（默认）→ text/markdown，浏览器直接可读、可直接喂给智能体；
// format=json     → 结构化文档（含 markdown 字段），供程序消费。
func (h *WorkflowHandler) Spec(c *gin.Context) {
	var wf model.Workflow
	if err := h.db.First(&wf, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	format := c.DefaultQuery("format", "md")
	if format != "md" && format != "json" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "format 只能是 md 或 json"})
		return
	}

	doc, err := service.CompileSpec(&wf, format)
	if err != nil {
		// 图本身坏了（JSON 解析失败）属于用户数据问题，不是服务故障
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	if format == "json" {
		c.JSON(http.StatusOK, doc)
		return
	}

	// 浏览器直接打开时按纯文本渲染，避免被当成下载文件
	if c.Query("download") == "1" {
		c.Header("Content-Disposition", "attachment; filename=\"workflow-"+c.Param("id")+"-spec.md\"")
	}
	c.Data(http.StatusOK, "text/markdown; charset=utf-8", []byte(doc.Markdown))
}
