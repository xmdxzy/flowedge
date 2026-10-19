package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// ArtifactHandler 产物管理：浏览 data/generated 下生成的图片 / 视频 / 音频
//
// 生图、视频、语音的产物以前只能去文件夹里翻；这里提供一个统一入口，
// 前端可以网格预览、点开看大图、把文件路径复制回工作流复用。
type ArtifactHandler struct {
	dir     string
	lineage *service.LineageStore
}

func NewArtifactHandler(dir string) *ArtifactHandler {
	return &ArtifactHandler{dir: dir}
}

// SetLineage 注入血缘仓库（提供 /api/artifacts/:name/lineage）
func (h *ArtifactHandler) SetLineage(l *service.LineageStore) {
	h.lineage = l
}

// Lineage 单件产物的来源链（自身 + 逐层上游）
func (h *ArtifactHandler) Lineage(c *gin.Context) {
	if h.lineage == nil {
		c.JSON(http.StatusOK, gin.H{"available": false})
		return
	}
	name := c.Param("name")
	self, ancestors, ok := h.lineage.Get(name)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"available": false, "name": name, "reason": "该产物没有血缘记录（可能产生于血缘功能上线前）"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"available": true, "self": self, "ancestors": ancestors})
}

// LineageList 全部血缘记录（画廊批量展示来源用）
func (h *ArtifactHandler) LineageList(c *gin.Context) {
	if h.lineage == nil {
		c.JSON(http.StatusOK, gin.H{"items": []service.ArtifactLineage{}, "count": 0})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	items := h.lineage.List(limit)
	c.JSON(http.StatusOK, gin.H{"items": items, "count": len(items)})
}

var artifactExt = map[string]string{
	".png":  "image",
	".jpg":  "image",
	".jpeg": "image",
	".webp": "image",
	".mp4":  "video",
	".webm": "video",
	".wav":  "audio",
	".mp3":  "audio",
}

type Artifact struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

// List 列出产物（按修改时间倒序）
func (h *ArtifactHandler) List(c *gin.Context) {
	abs, _ := filepath.Abs(h.dir)
	items := make([]Artifact, 0)
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		// 目录不存在时返回空列表而不是错误：新装环境还没生成过任何东西
		c.JSON(http.StatusOK, gin.H{"dir": abs, "items": items, "count": 0})
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		kind, ok := artifactExt[ext]
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, Artifact{
			Name:    e.Name(),
			Kind:    kind,
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		})
	}

	sort.Slice(items, func(i, j int) bool { return items[i].ModTime > items[j].ModTime })
	c.JSON(http.StatusOK, gin.H{"dir": abs, "items": items, "count": len(items)})
}

// Serve 提供单个产物文件（只允许平坦文件名，防目录穿越）
func (h *ArtifactHandler) Serve(c *gin.Context) {
	name := c.Param("name")
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid artifact name"})
		return
	}
	full := filepath.Join(h.dir, name)
	if _, err := os.Stat(full); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "artifact not found"})
		return
	}
	c.File(full)
}

// Delete 删除产物
func (h *ArtifactHandler) Delete(c *gin.Context) {
	name := c.Param("name")
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid artifact name"})
		return
	}
	full := filepath.Join(h.dir, name)
	if err := os.Remove(full); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": name})
}
