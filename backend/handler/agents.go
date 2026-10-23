package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// 外部智能体名册的 HTTP 出口。
//
// 为什么要有这一层：名册是 data/agents.json，手改文件当然能改，但"改完不知道装没装"
// 是实际使用中最耗时间的一步。面板要能一眼看到「配置了」和「真的能用」的区别，
// 所以列表默认带探测结果。
type AgentHandler struct {
	engine *service.EngineV2
}

func NewAgentHandler(engine *service.EngineV2) *AgentHandler {
	return &AgentHandler{engine: engine}
}

// List GET /api/agents?probe=0
//
// probe=0 跳过可用性探测（探测会做 LookPath / TCP dial，名册大时慢）。
func (h *AgentHandler) List(c *gin.Context) {
	store := h.engine.Agents()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "外部智能体名册未加载（data/agents.json）"})
		return
	}
	if c.Query("probe") == "0" {
		c.JSON(http.StatusOK, gin.H{"agents": store.List()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"agents": store.ProbeAll(c.Request.Context())})
}

// Probe POST /api/agents/:id/probe —— 单个体检
func (h *AgentHandler) Probe(c *gin.Context) {
	store := h.engine.Agents()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "名册未加载"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"agent": store.Probe(c.Request.Context(), c.Param("id"))})
}

// Upsert PUT /api/agents —— 新增或覆盖（同 id 视为更新）
//
// 只校验"能不能形成一次执行"（cli 要有 command、http 要有 base_url），
// 不去猜命令对不对 —— 那是 Probe 的事，猜错了反而误导。
func (h *AgentHandler) Upsert(c *gin.Context) {
	store := h.engine.Agents()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "名册未加载"})
		return
	}
	var def service.AgentDef
	if err := c.ShouldBindJSON(&def); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if err := store.Upsert(def); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"agent": store.Probe(c.Request.Context(), def.ID)})
}

// Delete DELETE /api/agents/:id
func (h *AgentHandler) Delete(c *gin.Context) {
	store := h.engine.Agents()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "名册未加载"})
		return
	}
	if err := store.Delete(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "id": c.Param("id")})
}
