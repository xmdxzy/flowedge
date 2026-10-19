package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// CredentialHandler 凭据管理（值永不下发，只回名称与 header 名）
type CredentialHandler struct {
	store *service.CredentialStore
}

func NewCredentialHandler(store *service.CredentialStore) *CredentialHandler {
	return &CredentialHandler{store: store}
}

// List 列出凭据（脱敏）
func (h *CredentialHandler) List(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusOK, gin.H{"items": []service.MaskedCredential{}, "count": 0})
		return
	}
	items := h.store.List()
	c.JSON(http.StatusOK, gin.H{"items": items, "count": len(items)})
}

type credRequest struct {
	Name    string            `json:"name"`
	Headers map[string]string `json:"headers"`
	Query   map[string]string `json:"query"`
	Note    string            `json:"note"`
}

// Set 新建或覆盖凭据
func (h *CredentialHandler) Set(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "凭据仓库不可用（数据库未初始化）"})
		return
	}
	var req credRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.Set(req.Name, service.CredentialSecret{
		Headers: req.Headers,
		Query:   req.Query,
		Note:    req.Note,
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "saved", "name": req.Name})
}

// Delete 删除凭据
func (h *CredentialHandler) Delete(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "凭据仓库不可用"})
		return
	}
	if err := h.store.Delete(c.Param("name")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}
