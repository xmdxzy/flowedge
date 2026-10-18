package handler

import (
	"io"
	"net/http"
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

// ImportFile 上传 .md/.txt 文件导入知识库
func (h *KBHandler) ImportFile(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file field"})
		return
	}
	if file.Size > 2<<20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file too large (max 2MB)"})
		return
	}
	name := strings.ToLower(file.Filename)
	if !strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, ".txt") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .md/.txt supported"})
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
	if strings.TrimSpace(string(data)) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is empty"})
		return
	}

	ids, err := h.rag.AddDocument(string(data), file.Filename)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"file":    file.Filename,
		"chunks":  len(ids),
		"ids":     ids,
		"total":   h.rag.Count(),
		"embedder": h.rag.Backend(),
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
