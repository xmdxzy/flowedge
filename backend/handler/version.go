package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

type VersionHandler struct {
	db *gorm.DB
}

func NewVersionHandler(db *gorm.DB) *VersionHandler {
	return &VersionHandler{db: db}
}

// ListVersions 获取工作流的所有版本
func (h *VersionHandler) ListVersions(c *gin.Context) {
	workflowID := c.Param("id")
	var versions []model.WorkflowVersion
	h.db.Where("workflow_id = ?", workflowID).Order("version DESC").Find(&versions)
	c.JSON(http.StatusOK, versions)
}

// CreateVersion 创建新版本（快照）
func (h *VersionHandler) CreateVersion(c *gin.Context) {
	workflowID := c.Param("id")

	var wf model.Workflow
	if err := h.db.First(&wf, workflowID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}

	// 获取当前最新版本号
	var maxVersion int
	h.db.Model(&model.WorkflowVersion{}).
		Where("workflow_id = ?", workflowID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&maxVersion)

	note := c.Query("note")
	if note == "" {
		note = "手动保存版本"
	}

	version := model.WorkflowVersion{
		WorkflowID: wf.ID,
		Version:    maxVersion + 1,
		Name:       wf.Name,
		Nodes:      wf.Nodes,
		Edges:      wf.Edges,
		ChangeNote: note,
	}

	h.db.Create(&version)
	c.JSON(http.StatusCreated, version)
}

// Rollback 回滚到指定版本
func (h *VersionHandler) Rollback(c *gin.Context) {
	workflowID := c.Param("id")
	versionID := c.Param("versionId")

	var version model.WorkflowVersion
	if err := h.db.First(&version, versionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "version not found"})
		return
	}

	// 更新当前工作流为指定版本的内容
	h.db.Model(&model.Workflow{}).Where("id = ?", workflowID).Updates(map[string]interface{}{
		"name":  version.Name,
		"nodes": version.Nodes,
		"edges": version.Edges,
	})

	c.JSON(http.StatusOK, gin.H{"status": "rolled back", "version": version.Version})
}

// GetVersion 获取单个版本
func (h *VersionHandler) GetVersion(c *gin.Context) {
	versionID := c.Param("versionId")
	var version model.WorkflowVersion
	if err := h.db.First(&version, versionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "version not found"})
		return
	}
	c.JSON(http.StatusOK, version)
}
