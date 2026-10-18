package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

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

	// 多模态服务
	comfyURL := getEnv("COMFYUI_BASE_URL", "http://localhost:8188")
	asrURL := getEnv("ASR_BASE_URL", "")
	ttsURL := getEnv("TTS_BASE_URL", "")

	t2iService := service.NewText2ImageService(comfyURL, getEnv("COMFYUI_CHECKPOINT", "flux1-schnell-fp8.safetensors"))
	asrService := service.NewASRService(asrURL)
	ttsService := service.NewTTSService(ttsURL)
	videoService := service.NewVideoService()
	httpService := service.NewHTTPService()
	ragService := service.NewRAGService("./data/rag")

	// GPU 调度器
	gpuSched := service.NewGPUScheduler(24576) // 3090 24GB

	// 注册模型
	gpuSched.RegisterModel("qwen2.5:7b", "llm", 4500)
	gpuSched.RegisterModel("qwen2.5:13b", "llm", 8500)
	gpuSched.RegisterModel("sd_xl_base_1.0", "text2image", 11000)
	gpuSched.RegisterModel("whisper-large-v3", "asr", 6000)

	// 引擎
	engine := service.NewEngineV2(llmService, t2iService, asrService, ttsService, videoService, httpService, ragService, gpuSched, wsHub)

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
