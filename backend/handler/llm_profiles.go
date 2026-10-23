package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// 自定义模型端点（LLM Profile）的读写。
//
// 设计约束：
//   - **PUT 整体替换**，不做增量合并：清单是一份完整配置，合并会引入"删不掉"的分支。
//   - **密钥不在这里**：profile 只存凭据名，真实 key 在 credentials.json（已加密）。
//   - 保存时校验名字唯一、base_url 合法、model 非空——错在保存时暴露，而不是跑起来才 401。

type LLMProfileHandler struct {
	store *service.LLMProfileStore
}

func NewLLMProfileHandler(s *service.LLMProfileStore) *LLMProfileHandler {
	return &LLMProfileHandler{store: s}
}

// List GET /api/llm/profiles
func (h *LLMProfileHandler) List(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusOK, gin.H{"profiles": []service.LLMProfile{}, "default": ""})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"profiles": h.store.List(),
		"default":  h.store.Default(),
		"path":     h.store.Path(),
	})
}

// Replace PUT /api/llm/profiles
func (h *LLMProfileHandler) Replace(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "模型清单未初始化"})
		return
	}
	var req struct {
		Profiles []service.LLMProfile `json:"profiles"`
		Default  string               `json:"default"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法的 JSON：" + err.Error()})
		return
	}
	if err := h.store.Replace(req.Profiles, req.Default); err != nil {
		// 校验失败是用户输入问题，不是服务故障
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"profiles": h.store.List(), "default": h.store.Default()})
}
