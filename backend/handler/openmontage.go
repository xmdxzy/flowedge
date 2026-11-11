package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// OpenMontageProxy 代理 OpenMontage Drama Studio API。
//
// OpenMontage 是独立部署的 Web 工坊（React + FastAPI + PostgreSQL + Redis + MinIO），
// 默认监听 127.0.0.1:8787。FlowEdge 作为调用方，通过本代理把本地工作流中的
// 出片请求转发给 OpenMontage，并把结果原样返回给前端。
//
// 部署要求：先按 docs/OpenMontage-集成指南.md 把 OpenMontage 启起来（docker compose
// -f docker-compose.studio.yml up --build），再在 FlowEdge 侧设 OPENMONTAGE_BASE_URL
// （默认 http://127.0.0.1:8787）。
//
// 若 OpenMontage 未就绪，所有接口返回 502 并给出可操作的修复提示（不静默吞错误）。
type OpenMontageProxy struct {
	baseURL string
	client  *http.Client
}

// NewOpenMontageProxy 创建代理。baseURL 为空时回落默认值。
func NewOpenMontageProxy(baseURL string) *OpenMontageProxy {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8787"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenMontageProxy{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Health 探活 OpenMontage：GET /api/openmontage/health → GET {baseURL}/api/health
func (p *OpenMontageProxy) Health(c *gin.Context) {
	if ok, body := p.forward(c, http.MethodGet, "/api/health", nil); ok {
		c.Data(http.StatusOK, "application/json", body)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{
		"error":    "OpenMontage 不可达，请先部署并启动",
		"detail":   fmt.Sprintf("尝试连接 %s/api/health 失败，请确认 OpenMontage 已通过 docker compose 启动（端口 8787）", p.baseURL),
		"fix":      "docker compose -f docker-compose.studio.yml up --build",
		"base_url": p.baseURL,
	})
}

// ListProjects 列出项目：GET /api/openmontage/projects → GET {baseURL}/api/projects
func (p *OpenMontageProxy) ListProjects(c *gin.Context) {
	if ok, body := p.forward(c, http.MethodGet, "/api/projects", nil); ok {
		c.Data(http.StatusOK, "application/json", body)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// GetProject 获取单个项目：GET /api/openmontage/projects/:id → GET {baseURL}/api/projects/:id
func (p *OpenMontageProxy) GetProject(c *gin.Context) {
	id := c.Param("id")
	if ok, body := p.forward(c, http.MethodGet, "/api/projects/"+url.PathEscape(id), nil); ok {
		c.Data(http.StatusOK, "application/json", body)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// CreateProject 新建项目：POST /api/openmontage/projects → POST {baseURL}/api/projects
func (p *OpenMontageProxy) CreateProject(c *gin.Context) {
	body, _ := io.ReadAll(c.Request.Body)
	if ok, rb := p.forward(c, http.MethodPost, "/api/projects", body); ok {
		c.Data(http.StatusCreated, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// GenerateScript 生成剧本：POST /api/openmontage/projects/:id/script → POST {baseURL}/api/projects/{id}/script
func (p *OpenMontageProxy) GenerateScript(c *gin.Context) {
	id := c.Param("id")
	body, _ := io.ReadAll(c.Request.Body)
	if ok, rb := p.forward(c, http.MethodPost, "/api/projects/"+url.PathEscape(id)+"/script", body); ok {
		c.Data(http.StatusOK, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// GenerateStoryboard 生成分镜：POST /api/openmontage/projects/:id/storyboard → POST {baseURL}/api/projects/{id}/storyboard
func (p *OpenMontageProxy) GenerateStoryboard(c *gin.Context) {
	id := c.Param("id")
	body, _ := io.ReadAll(c.Request.Body)
	if ok, rb := p.forward(c, http.MethodPost, "/api/projects/"+url.PathEscape(id)+"/storyboard", body); ok {
		c.Data(http.StatusOK, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// GenerateVideo 生成分镜视频：POST /api/openmontage/projects/:id/generate → POST {baseURL}/api/projects/{id}/generate
func (p *OpenMontageProxy) GenerateVideo(c *gin.Context) {
	id := c.Param("id")
	body, _ := io.ReadAll(c.Request.Body)
	if ok, rb := p.forward(c, http.MethodPost, "/api/projects/"+url.PathEscape(id)+"/generate", body); ok {
		c.Data(http.StatusOK, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// GenerateAudio 生成配音/音频：POST /api/openmontage/projects/:id/audio → POST {baseURL}/api/projects/{id}/audio
func (p *OpenMontageProxy) GenerateAudio(c *gin.Context) {
	id := c.Param("id")
	body, _ := io.ReadAll(c.Request.Body)
	if ok, rb := p.forward(c, http.MethodPost, "/api/projects/"+url.PathEscape(id)+"/audio", body); ok {
		c.Data(http.StatusOK, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// SaveEdit 保存剪辑并导出：POST /api/openmontage/projects/:id/edits → POST {baseURL}/api/projects/{id}/edits
func (p *OpenMontageProxy) SaveEdit(c *gin.Context) {
	id := c.Param("id")
	body, _ := io.ReadAll(c.Request.Body)
	if ok, rb := p.forward(c, http.MethodPost, "/api/projects/"+url.PathEscape(id)+"/edits", body); ok {
		c.Data(http.StatusOK, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// AutoProduce 一键出片：POST /api/openmontage/auto-produce → POST {baseURL}/api/auto-produce
func (p *OpenMontageProxy) AutoProduce(c *gin.Context) {
	body, _ := io.ReadAll(c.Request.Body)
	if ok, rb := p.forward(c, http.MethodPost, "/api/auto-produce", body); ok {
		c.Data(http.StatusCreated, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// GetSettingsKeys 获取模型密钥配置：GET /api/openmontage/settings/keys → GET {baseURL}/api/settings/keys
func (p *OpenMontageProxy) GetSettingsKeys(c *gin.Context) {
	if ok, rb := p.forward(c, http.MethodGet, "/api/settings/keys", nil); ok {
		c.Data(http.StatusOK, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// SaveSettingsKeys 保存模型密钥：POST /api/openmontage/settings/keys → POST {baseURL}/api/settings/keys
func (p *OpenMontageProxy) SaveSettingsKeys(c *gin.Context) {
	body, _ := io.ReadAll(c.Request.Body)
	if ok, rb := p.forward(c, http.MethodPost, "/api/settings/keys", body); ok {
		c.Data(http.StatusOK, "application/json", rb)
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "OpenMontage 不可达", "base_url": p.baseURL})
}

// forward 把请求转发到 OpenMontage。返回 (成功, 响应体字节)。失败时会把错误写入 c。
func (p *OpenMontageProxy) forward(c *gin.Context, method, path string, body []byte) (bool, []byte) {
	target := p.baseURL + path
	req, err := http.NewRequestWithContext(c.Request.Context(), method, target, nil)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "构建 OpenMontage 请求失败", "detail": err.Error()})
		return false, nil
	}
	if len(body) > 0 {
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
	}
	// 透传 Authorization（OpenMontage 本地 Demo 通常不需要，但保留兼容性）
	if auth := c.GetHeader("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"error":  "OpenMontage 连接失败",
			"detail": err.Error(),
			"target": target,
			"fix":    "请先启动 OpenMontage（docker compose -f docker-compose.studio.yml up --build），或检查 OPENMONTAGE_BASE_URL 是否正确",
		})
		return false, nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "读取 OpenMontage 响应失败", "detail": err.Error()})
		return false, nil
	}

	// 透传 Content-Type
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	_, _ = c.Writer.Write(respBody)
	return true, respBody
}

// RegisterOpenMontageRoutes 在 /api 分组下注册 OpenMontage 代理路由。
func RegisterOpenMontageRoutes(api *gin.RouterGroup, baseURL string) {
	om := NewOpenMontageProxy(baseURL)
	omGroup := api.Group("/openmontage")
	{
		// 探活与配置
		omGroup.GET("/health", om.Health)
		omGroup.GET("/settings/keys", om.GetSettingsKeys)
		omGroup.POST("/settings/keys", om.SaveSettingsKeys)

		// 项目
		omGroup.GET("/projects", om.ListProjects)
		omGroup.POST("/projects", om.CreateProject)
		omGroup.GET("/projects/:id", om.GetProject)

		// 7 阶段工作流
		omGroup.POST("/projects/:id/script", om.GenerateScript)         // 剧本
		omGroup.POST("/projects/:id/storyboard", om.GenerateStoryboard) // 分镜
		omGroup.POST("/projects/:id/generate", om.GenerateVideo)        // 视频生成
		omGroup.POST("/projects/:id/audio", om.GenerateAudio)           // 配音
		omGroup.POST("/projects/:id/edits", om.SaveEdit)                // 剪辑导出

		// 一键出片
		omGroup.POST("/auto-produce", om.AutoProduce)
	}
}
