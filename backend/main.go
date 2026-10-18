package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"flowedge/backend/config"
	"flowedge/backend/handler"
	"flowedge/backend/model"
	"flowedge/backend/service"
)

func main() {
	cfg := config.Load()

	// 连接数据库（默认本地 SQLite，零外部依赖；DB_DRIVER=postgres 时使用 PostgreSQL）
	db := initDB(cfg)
	if db != nil {
		if err := db.AutoMigrate(&model.Workflow{}, &model.WorkflowRun{}, &model.WorkflowVersion{}); err != nil {
			log.Printf("Warning: auto migrate failed: %v", err)
		} else {
			log.Println("Database migrated")
		}
	}

	// 事件中心
	wsHub := service.NewWSHub()

	// 初始化服务
	llmService := service.NewLLMService(cfg.OllamaBaseURL, cfg.OllamaModel)
	llmStreamService := service.NewLLMStreamService(cfg.OllamaBaseURL, cfg.OllamaModel)

	// 多模态服务
	comfyURL := getEnv("COMFYUI_BASE_URL", "http://localhost:8188")
	asrURL := getEnv("ASR_BASE_URL", "")
	ttsURL := getEnv("TTS_BASE_URL", "")

	t2iService := service.NewText2ImageService(comfyURL, getEnv("COMFYUI_CHECKPOINT", "flux1-schnell-fp8.safetensors"))
	asrService := service.NewASRService(asrURL)
	ttsService := service.NewTTSService(ttsURL)
	videoService := service.NewVideoService()
	httpService := service.NewHTTPService()
	ragService := service.NewRAGServiceV3(getEnv("KB_DIR", "./data/kb"), cfg.OllamaBaseURL, getEnv("EMBED_MODEL", "bge-m3"))

	// GPU 调度器
	gpuSched := service.NewGPUScheduler(24576) // 3090 24GB

	// 注册模型
	gpuSched.RegisterModel("qwen2.5:7b", "llm", 4500)
	gpuSched.RegisterModel("qwen2.5:13b", "llm", 8500)
	gpuSched.RegisterModel("sd_xl_base_1.0", "text2image", 11000)
	gpuSched.RegisterModel("whisper-large-v3", "asr", 6000)

	// 引擎
	engine := service.NewEngineV2(llmService, t2iService, asrService, ttsService, videoService, httpService, ragService, gpuSched, wsHub)

	// 子工作流解析器（从 DB 读取嵌套工作流定义）
	engine.SetResolver(func(id uint) ([]byte, []byte, error) {
		var wf model.Workflow
		if err := db.First(&wf, id).Error; err != nil {
			return nil, nil, err
		}
		return []byte(wf.Nodes), []byte(wf.Edges), nil
	})

	// 流式 LLM（token 经 WS 推送前端打字机效果）
	engine.SetLLMStream(llmStreamService)

	// GPU 真实遥测轮询（缓存 nvidia-smi 结果，避免每请求拉子进程）
	gpuSched.StartTelemetry(5 * time.Second)

	if getEnv("FLOWEDGE_MODE", "release") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 初始化路由
	r := gin.Default()

	// CORS
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	api := r.Group("/api")
	{
		wfHandler := handler.NewWorkflowHandler(db, engine)
		versionHandler := handler.NewVersionHandler(db)
		wsHandler := handler.NewWSHandler(wsHub)

		api.GET("/workflows", wfHandler.List)
		api.GET("/workflows/:id", wfHandler.Get)
		api.POST("/workflows", wfHandler.Create)
		api.PUT("/workflows/:id", wfHandler.Update)
		api.DELETE("/workflows/:id", wfHandler.Delete)

		api.POST("/workflows/:id/run", wfHandler.Run)
		api.GET("/workflows/:id/runs", wfHandler.ListRuns)
		api.GET("/runs/:runId", wfHandler.GetRun)
		api.POST("/runs/:runId/resume", wfHandler.ResumeRun)

		// 项目专属知识库
		kbHandler := handler.NewKBHandler(ragService)
		api.POST("/kb/documents", kbHandler.AddDocument)
		api.GET("/kb/documents", kbHandler.ListDocuments)
		api.DELETE("/kb/documents/:id", kbHandler.DeleteDocument)
		api.POST("/kb/query", kbHandler.Query)
		api.GET("/kb/status", kbHandler.Status)

		// 版本管理
		api.GET("/workflows/:id/versions", versionHandler.ListVersions)
		api.POST("/workflows/:id/versions", versionHandler.CreateVersion)
		api.GET("/workflows/:id/versions/:versionId", versionHandler.GetVersion)
		api.POST("/workflows/:id/versions/:versionId/rollback", versionHandler.Rollback)

		// GPU 状态接口
		api.GET("/gpu/status", func(c *gin.Context) {
			c.JSON(200, gpuSched.GetSchedulerStatus())
		})

		// WebSocket 实时推送
		api.GET("/ws", wsHandler.HandleWS)
	}

	// SPA 静态资源（桌面客户端 / Docker 部署共用同一后端入口）
	distDir := getEnv("STATIC_DIR", "../frontend/dist")
	if _, err := os.Stat(filepath.Join(distDir, "index.html")); err == nil {
		r.Static("/assets", filepath.Join(distDir, "assets"))
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.JSON(404, gin.H{"error": "not found"})
				return
			}
			c.File(filepath.Join(distDir, "index.html"))
		})
		log.Printf("Serving frontend from %s", distDir)
	}

	log.Printf("FlowEdge backend running on :%s", cfg.Port)
	r.Run(":" + cfg.Port)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// initDB 初始化数据库；默认 SQLite（开箱即用），DB_DRIVER=postgres 时切换 PostgreSQL
func initDB(cfg *config.Config) *gorm.DB {
	driver := getEnv("DB_DRIVER", "sqlite")

	if driver == "postgres" {
		dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
			cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName)
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		if err != nil {
			log.Printf("Warning: postgres connect failed: %v", err)
			return nil
		}
		log.Println("Using PostgreSQL")
		return db
	}

	// SQLite（默认）
	dbPath := getEnv("SQLITE_PATH", "./data/flowedge.db")
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		log.Printf("Warning: sqlite open failed: %v", err)
		return nil
	}
	log.Printf("Using SQLite at %s", dbPath)
	return db
}
