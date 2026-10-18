package main

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
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
	"flowedge/backend/web"
	webservices "flowedge/backend/web/services"
)

// version 构建版本（发布时用 -ldflags "-X main.version=1.0.0" 注入）
var version = "dev"

func main() {
	// 以 exe 所在目录为工作目录：Windows 双击运行时 CWD 可能是 system32，
	// 相对路径（./data）会写到系统目录。所有数据都跟着 exe 走。
	baseDir := config.BaseDir()
	if wd, err := os.Getwd(); err == nil && !strings.EqualFold(filepath.Clean(wd), filepath.Clean(baseDir)) {
		if err := os.Chdir(baseDir); err == nil {
			log.Printf("工作目录切换到 %s（数据保存在此目录下的 data/）", baseDir)
		}
	}

	// 子命令：把内嵌的语音服务脚本导出到 exe 旁的 local-services/
	if len(os.Args) > 1 && (os.Args[1] == "--export-services" || os.Args[1] == "-export-services") {
		exportServices(baseDir)
		return
	}

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

	// 模型选择：环境变量优先；未指定则自动探测本地已装模型（换机器部署不用改配置）
	llmModel := cfg.OllamaModel
	if llmModel == "" {
		if picked := service.PickDefaultModel(cfg.OllamaBaseURL); picked != "" {
			llmModel = picked
			log.Printf("未指定 OLLAMA_MODEL，自动使用本地模型：%s", picked)
		} else {
			llmModel = "qwen2.5-coder:7b"
			log.Printf("提示：未检测到本地 Ollama 模型，暂用 %s（ollama pull %s 后重启即可）", llmModel, llmModel)
		}
	}

	// 初始化服务
	llmService := service.NewLLMService(cfg.OllamaBaseURL, llmModel)
	llmStreamService := service.NewLLMStreamService(cfg.OllamaBaseURL, llmModel)

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

	// 节点级缓存（节点声明 cache: true 时生效）
	nodeCache := service.NewNodeCache(200)
	engine.SetCache(nodeCache)

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

	// handlers（cron 调度器也要用到 wfHandler，故在路由组之前构造）
	wfHandler := handler.NewWorkflowHandler(db, engine)
	triggerHandler := handler.NewTriggerHandler(db, wfHandler)

	// 定时触发器：每 20 秒扫描一次启用了 cron 的工作流（分钟级去重）
	cronScheduler := service.NewCronScheduler(db, func(id uint) error {
		var wf model.Workflow
		if err := db.First(&wf, id).Error; err != nil {
			return err
		}
		wfHandler.StartRun(&wf)
		return nil
	})
	cronScheduler.Start(20 * time.Second)

	api := r.Group("/api")
	{
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
		api.POST("/runs/:runId/cancel", wfHandler.CancelRun)

		// 项目专属知识库
		kbHandler := handler.NewKBHandler(ragService)
		api.POST("/kb/documents", kbHandler.AddDocument)
		api.POST("/kb/import", kbHandler.ImportFile)
		api.POST("/kb/import-dir", kbHandler.ImportDir)
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

		// 产物管理（生成的图片 / 视频 / 音频）
		artifactHandler := handler.NewArtifactHandler(getEnv("GENERATED_DIR", "data/generated"))
		api.GET("/artifacts", artifactHandler.List)
		api.GET("/artifacts/:name", artifactHandler.Serve)
		api.DELETE("/artifacts/:name", artifactHandler.Delete)

		// 触发器：Webhook（外部调用）+ Cron（定时）
		api.GET("/workflows/:id/trigger", triggerHandler.Get)
		api.PUT("/workflows/:id/trigger", triggerHandler.Update)
		api.POST("/workflows/:id/trigger/rotate", triggerHandler.Rotate)
		api.POST("/hooks/:token", triggerHandler.Webhook)
		api.GET("/triggers", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"schedules": cronScheduler.CronSummary()})
		})

		// WebSocket 实时推送
		api.GET("/ws", wsHandler.HandleWS)

		// 健康检查与环境自检（部署排障用）
		healthHandler := handler.NewHealthHandler(gpuSched, baseDir, cfg.OllamaBaseURL, comfyURL, asrURL, ttsURL, version)
		api.GET("/health", healthHandler.Health)
		api.GET("/capabilities", healthHandler.Capabilities)
	}

	// 前端入口：外部目录优先（便于替换），否则用 exe 内嵌产物
	mountStatic(r)

	// 启动后打印一次环境自检（用户看控制台就知道还缺什么）
	go func() {
		time.Sleep(200 * time.Millisecond)
		caps := service.ProbeCapabilities(gpuSched, baseDir, cfg.OllamaBaseURL, comfyURL, asrURL, ttsURL)
		log.Printf("环境自检  GPU %s | LLM(Ollama) %s | 文生图(ComfyUI) %s | 视频(ffmpeg) %s | 音频(ASR+TTS) %s",
			mark(caps.GPU.OK), mark(caps.Ollama.OK), mark(caps.ComfyUI.OK), mark(caps.FFmpeg.OK), mark(caps.ASR.OK && caps.TTS.OK))
		if len(caps.Missing) > 0 {
			log.Printf("当前不可用的能力：%v —— 界面右上角「环境」或 /api/capabilities 可查看修复指引", caps.Missing)
		}
	}()

	browserURL := fmt.Sprintf("http://127.0.0.1:%s", cfg.Port)
	log.Printf("FlowEdge 已启动：%s（Ctrl+C 停止）", browserURL)
	if getEnv("FLOWEDGE_OPEN_BROWSER", "") == "1" {
		openBrowser(browserURL)
	}
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("启动失败：%v（端口 %s 可能已被占用，可用 PORT 环境变量改端口）", err, cfg.Port)
	}
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

// mountStatic 挂载前端资源：外部目录 > exe 内嵌 > 明确的排障页
func mountStatic(r *gin.Engine) {
	// 显式指定 > 同目录构建产物 > 内嵌 > 开发态的前端目录
	// （内嵌优先于 ../frontend/dist：否则打包后的 exe 放在 dist/ 里运行时，
	//   会意外加载仓库里的旧构建，而不是自己内嵌的那份）
	candidates := []string{}
	if dir := os.Getenv("STATIC_DIR"); dir != "" {
		candidates = append(candidates, config.Resolve(dir))
	}
	candidates = append(candidates, config.Resolve(filepath.Join("web", "static")))

	spaHandler := func(root fs.FS, fileServer http.FileSystem, label string) {
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			clean := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/")
			if clean == "" || clean == "." {
				clean = "index.html"
			}
			if st, err := fs.Stat(root, clean); err != nil || st.IsDir() {
				clean = "index.html" // SPA 前端路由回退
			}
			// index.html 必须直接写内容：http.ServeFile 见到 */index.html 会
			// 301 重定向到 "./"，与 SPA 回退叠加会形成无限重定向
			if clean == "index.html" {
				data, rerr := fs.ReadFile(root, "index.html")
				if rerr != nil {
					c.String(http.StatusInternalServerError, "index.html missing")
					return
				}
				c.Data(http.StatusOK, "text/html; charset=utf-8", data)
				return
			}
			c.FileFromFS(clean, fileServer)
		})
		log.Printf("前端资源：%s", label)
	}

	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
			continue
		}
		spaHandler(os.DirFS(dir), http.Dir(dir), "外部目录 "+dir)
		return
	}

	if embedded := web.Dist(); embedded != nil {
		spaHandler(embedded, http.FS(embedded), "已内嵌在 exe 中")
		return
	}

	// 开发态兜底：从 backend/ 目录运行时用仓库里的前端构建
	for _, dir := range []string{
		config.Resolve(filepath.Join("..", "frontend", "dist")),
		config.Resolve(filepath.Join("frontend", "dist")),
	} {
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
			spaHandler(os.DirFS(dir), http.Dir(dir), "开发目录 "+dir)
			return
		}
	}

	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(noUIBody))
	})
	log.Printf("前端资源：未打包，当前为「仅 API」模式")
}

const noUIBody = `<!doctype html><html lang="zh-CN"><meta charset="utf-8">
<title>FlowEdge · 未打包前端</title>
<style>body{background:#0A0D14;color:#E5E7EB;font-family:system-ui,"Microsoft YaHei UI",sans-serif;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{max-width:620px;padding:36px;border:1px solid #232B3D;border-radius:16px;background:#10141F}
h1{margin:0 0 14px;font-size:20px}p{color:#9CA3AF;line-height:1.75;font-size:14px}
code{background:#0A0D14;border:1px solid #232B3D;padding:2px 7px;border-radius:6px;color:#22D3EE;font-size:13px}
a{color:#818CF8}</style>
<div class="card"><h1>后端已启动，但前端还没打包</h1>
<p>这是开发模式的正常提示。以下任一方式即可看到界面：</p>
<p>① 运行 <code>python scripts/build-windows.py</code> 打包（界面会内嵌进 exe）</p>
<p>② 设置环境变量 <code>STATIC_DIR</code> 指向前端构建目录（含 index.html）</p>
<p>③ 开发时用 <code>npm run dev</code> 起 Vite，后端仅提供 /api</p>
<p>环境自检：<a href="/capabilities">/api/capabilities</a></p></div></html>`

// mark 自检结果的可读标记
func mark(ok bool) string {
	if ok {
		return "OK"
	}
	return "缺失"
}

// openBrowser 启动后自动打开默认浏览器（FLOWEDGE_OPEN_BROWSER=1 时）
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd, args = "explorer", []string{url}
	case "darwin":
		cmd, args = "open", []string{url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}

// exportServices 导出内嵌的语音服务脚本（供没有源码仓库的单文件用户使用）
func exportServices(baseDir string) {
	fsys := webservices.Files()
	if fsys == nil {
		fmt.Println("此构建未内嵌语音服务脚本，请用 scripts/build-windows.py 重新打包。")
		return
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		fmt.Println("读取内嵌脚本失败：", err)
		return
	}

	outDir := filepath.Join(baseDir, "local-services")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Println("创建目录失败：", err)
		return
	}

	count := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		data, readErr := fs.ReadFile(fsys, name)
		if readErr != nil {
			continue
		}
		dst := filepath.Join(outDir, name)
		if writeErr := os.WriteFile(dst, data, 0o644); writeErr != nil {
			fmt.Printf("  写入失败 %s: %v\n", dst, writeErr)
			continue
		}
		fmt.Printf("  已导出 %s\n", dst)
		count++
	}

	if count == 0 {
		fmt.Println("没有可导出的脚本（当前是未打包前端的开发构建）。")
		return
	}
	fmt.Printf("\n共导出 %d 个脚本到 %s\n", count, outDir)
	fmt.Println("这些是可选能力（语音转写/合成），不装也不影响其他功能。启用步骤：")
	fmt.Println("  1) 安装 Python 3.10+（python.org，安装时勾选 Add to PATH）")
	fmt.Printf("  2) cd \"%s\"\n", outDir)
	fmt.Println("  3) python -m venv .venv && .venv\\Scripts\\pip install -r requirements.txt")
	fmt.Println("  4) .venv\\Scripts\\python tts_server.py    （语音合成，端口 8102）")
	fmt.Println("  5) .venv\\Scripts\\python asr_server.py    （语音转写，端口 8101，首次会下载模型）")
	fmt.Println("  然后重启 FlowEdge.exe，界面右上角「环境」会显示这两项已就绪。")
}
