package handler

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

// KBHandler 项目专属知识库管理接口
type KBHandler struct {
	rag *service.RAGServiceV3
}

func NewKBHandler(rag *service.RAGServiceV3) *KBHandler {
	return &KBHandler{rag: rag}
}

type kbAddRequest struct {
	Content string `json:"content"`
	Source  string `json:"source"`
}

type kbQueryRequest struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

// AddDocument 添加文档（自动分块 + 向量化）
func (h *KBHandler) AddDocument(c *gin.Context) {
	var req kbAddRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content is empty"})
		return
	}
	ids, err := h.rag.AddDocument(req.Content, req.Source)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"chunks":   len(ids),
		"ids":      ids,
		"total":    h.rag.Count(),
		"embedder": h.rag.Backend(),
	})
}

// ListDocuments 列出全部知识块
func (h *KBHandler) ListDocuments(c *gin.Context) {
	docs := h.rag.List()
	out := make([]gin.H, 0, len(docs))
	for _, d := range docs {
		out = append(out, gin.H{
			"id":       d.ID,
			"source":   d.Source,
			"content":  d.Content,
			"metadata": d.Metadata,
		})
	}
	c.JSON(http.StatusOK, gin.H{"total": len(out), "documents": out})
}

// DeleteDocument 删除知识块
func (h *KBHandler) DeleteDocument(c *gin.Context) {
	if err := h.rag.Delete(c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// Query 知识库检索（调试/前端用）
func (h *KBHandler) Query(c *gin.Context) {
	var req kbQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is empty"})
		return
	}
	results, err := h.rag.Query(req.Query, req.TopK)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(results))
	for _, r := range results {
		out = append(out, gin.H{
			"id":      r.Record.ID,
			"source":  r.Record.Source,
			"content": r.Record.Content,
			"score":   r.Score,
		})
	}
	c.JSON(http.StatusOK, gin.H{"query": req.Query, "results": out})
}

// ImportFile 上传 .md/.txt/.csv/.docx/.pdf 文件导入知识库
func (h *KBHandler) ImportFile(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file field"})
		return
	}
	// 文本类限 2MB；PDF/DOCX 是二进制容器，放宽到 20MB
	maxSize := int64(2 << 20)
	if !strings.HasSuffix(strings.ToLower(file.Filename), ".md") && !strings.HasSuffix(strings.ToLower(file.Filename), ".txt") {
		maxSize = 20 << 20
	}
	if file.Size > maxSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("file too large (max %dMB)", maxSize>>20)})
		return
	}
	if !service.IsSupportedDoc(file.Filename) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "unsupported file type",
			"support": service.SupportedDocExts,
		})
		return
	}

	src, err := file.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	text, err := service.ExtractDocumentText(file.Filename, data)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "file": file.Filename})
		return
	}
	if strings.TrimSpace(text) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is empty"})
		return
	}

	ids, err := h.rag.AddDocument(text, file.Filename)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"file":     file.Filename,
		"chunks":   len(ids),
		"ids":      ids,
		"chars":    len([]rune(text)),
		"total":    h.rag.Count(),
		"embedder": h.rag.Backend(),
	})
}

type kbImportDirRequest struct {
	Dir       string `json:"dir"`
	Recursive bool   `json:"recursive"`
}

// ImportDir 批量导入目录下的 .md/.txt（递归可选），跳过隐藏目录与常见噪音目录
func (h *KBHandler) ImportDir(c *gin.Context) {
	var req kbImportDirRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dir is empty"})
		return
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dir not found or not a directory: " + dir})
		return
	}

	skipDir := map[string]bool{
		"node_modules": true, ".git": true, "dist": true, "build": true,
		".venv": true, "venv": true, "data": true, "__pycache__": true,
	}

	type imported struct {
		File   string `json:"file"`
		Chunks int    `json:"chunks"`
	}
	var (
		files    []imported
		skipped  []string
		total    int
		scanned  int
		readErrs []string
	)

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			readErrs = append(readErrs, path+": "+err.Error())
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != dir && (skipDir[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if !req.Recursive && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		scanned++
		if !service.IsSupportedDoc(d.Name()) {
			skipped = append(skipped, path)
			return nil
		}
		st, statErr := d.Info()
		limit := int64(2 << 20)
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			limit = 20 << 20
		}
		if statErr != nil || st.Size() > limit {
			skipped = append(skipped, path)
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			readErrs = append(readErrs, path+": "+readErr.Error())
			return nil
		}
		content, convErr := service.ExtractDocumentText(d.Name(), raw)
		if convErr != nil {
			readErrs = append(readErrs, path+": "+convErr.Error())
			return nil
		}
		content = strings.TrimSpace(content)
		if content == "" {
			skipped = append(skipped, path)
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = d.Name()
		}
		ids, addErr := h.rag.AddDocument(content, rel)
		if addErr != nil {
			readErrs = append(readErrs, rel+": "+addErr.Error())
			return nil
		}
		files = append(files, imported{File: rel, Chunks: len(ids)})
		total += len(ids)
		return nil
	})

	if walkErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": walkErr.Error()})
		return
	}
	if files == nil {
		files = []imported{}
	}
	c.JSON(http.StatusCreated, gin.H{
		"dir":       dir,
		"files":     files,
		"fileCount": len(files),
		"chunks":    total,
		"scanned":   scanned,
		"skipped":   len(skipped),
		"errors":    readErrs,
		"total":     h.rag.Count(),
		"embedder":  h.rag.Backend(),
	})
}

// Status 知识库状态（后端类型 + 文档数）
func (h *KBHandler) Status(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"embedder": h.rag.Backend(),
		"model":    "bge-m3",
		"chunks":   h.rag.Count(),
	})
}
