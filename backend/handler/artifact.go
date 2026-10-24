package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// ArtifactHandler 产物管理：浏览 data/generated 下生成的图片 / 视频 / 音频
//
// 生图、视频、语音的产物以前只能去文件夹里翻；这里提供一个统一入口，
// 前端可以网格预览、点开看大图、把文件路径复制回工作流复用。
type ArtifactHandler struct {
	dir string
}

func NewArtifactHandler(dir string) *ArtifactHandler {
	return &ArtifactHandler{dir: dir}
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

// List 列出产物（按修改时间倒序）。?kind=image|video|audio 只看一类。
func (h *ArtifactHandler) List(c *gin.Context) {
	abs, _ := filepath.Abs(h.dir)
	want := c.Query("kind")
	items := make([]Artifact, 0)
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		// 目录不存在时返回空列表而不是错误：新装环境还没生成过任何东西
		c.JSON(http.StatusOK, gin.H{"dir": abs, "kind": want, "items": items, "count": 0})
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
		if want != "" && kind != want {
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
	c.JSON(http.StatusOK, gin.H{"dir": abs, "kind": want, "items": items, "count": len(items)})
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
