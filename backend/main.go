// FlowEdge Lite —— 本地多模态生成引擎
//
// 只做两件事：
//  1. 本地多模态（图片 / 视频 / 音频）的高质量生成工作流；
//  2. 质量建议门控 + 循环提升：不过门就诊断、修复、重跑，直到达标或到轮次上限。
//
// 对外接口只有三个面：出片台 UI（选流水线/填字段/看进度）、/api（出片与产物）、MCP（给 DSH 调用）。
// 图由服务端按 data/pipelines.json 组装，用户不画图。
package main

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"flowedge/backend/config"
	"flowedge/backend/handler"
	"flowedge/backend/mcp"
	"flowedge/backend/model"
	"flowedge/backend/service"
	"flowedge/backend/web"
	webservices "flowedge/backend/web/services"
)

// version 构建版本（发布时用 -ldflags "-X main.version=1.0.0" 注入）
var version = "lite"

func main() {
	// 以 exe 所在目录为工作目录：Windows 双击运行时 CWD 可能是 system32，
	// 相对路径（./data）会写到系统目录。所有数据都跟着 exe 走。
	baseDir := config.BaseDir()
	if wd, err := os.Getwd(); err == nil && !strings.EqualFold(filepath.Clean(wd), filepath.Clean(baseDir)) {
		if err := os.Chdir(baseDir); err == nil {
			log.Printf("工作目录切换到 %s（数据保存在此目录下的 data/）", baseDir)
		}
	}

	// 日志同时落盘。常驻引擎多由外壳（序策工作站 / 启动 bat）拉起，
	// stdout 进了父进程的管道；出事后 agent 与人**读不到任何一行**，只能猜。
	// 落一份到 <exe目录>/logs/engine.log，才有事后可查的证据。
	closeLog := setupLogFile(baseDir)
	defer closeLog()

	// 子命令：把内嵌的语音服务脚本导出到 exe 旁的 local-services/
	if len(os.Args) > 1 && (os.Args[1] == "--export-services" || os.Args[1] == "-export-services") {
		exportServices(baseDir)
		return
	}

	// 子命令：MCP server（stdio）。给 DSH / Claude Desktop 这类客户端接工作流用。
	//
	// 瘦客户端转发：stdio 进程里不再起一套引擎（两个引擎会抢同一张卡的显存
	// 和同一个 sqlite 文件）。代价是引擎必须先起来——每个工具在不可达时
	// 返回能照做的错误。
	if len(os.Args) > 1 && (os.Args[1] == "--mcp" || os.Args[1] == "-mcp") {
		srv := mcp.NewServer(
			getEnv("FLOWEDGE_MCP_BASE_URL", "http://127.0.0.1:"+getEnv("PORT", "8080")),
			os.Getenv("FLOWEDGE_API_TOKEN"), os.Stdin, os.Stdout)
		if err := srv.Serve(); err != nil {
			log.Printf("MCP server 退出：%v", err)
		}
		return
	}

	cfg := config.Load()

	db := initDB(cfg)
	if db != nil {
		if err := db.AutoMigrate(&model.Workflow{}, &model.WorkflowRun{}, &model.RunEvent{}, &model.Job{}); err != nil {
			log.Printf("Warning: auto migrate failed: %v", err)
		} else {
			log.Println("Database migrated")
		}
	}

	wsHub := service.NewWSHub()

	var eventLogger *service.EventLogger
	if db != nil {
		eventLogger = service.NewEventLogger(db)
		defer eventLogger.Close()
	}

	// 模型选择：环境变量优先；未指定则自动探测本地已装模型
	llmModel := cfg.OllamaModel
	if llmModel == "" {
		if picked := service.PickDefaultModel(cfg.OllamaBaseURL); picked != "" {
			llmModel = picked
			log.Printf("未指定 OLLAMA_MODEL，自动使用本地模型：%s", picked)
		} else {
			llmModel = "qwen2.5-coder:7b"
			log.Printf("提示：未检测到本地 Ollama 模型，暂用 %s", llmModel)
		}
	}

	// 配置的模型必须真的存在：不然后面每个 LLM 节点都会以「404 model not found」
	// 的形式失败，而人会先去怀疑端口/代理。这里提前一次说清（但**不**替用户换模型，
	// 静默换模型等于悄悄改了创作质量）。
	if missing, available, err := service.CheckOllamaModel(cfg.OllamaBaseURL, llmModel); err == nil && missing {
		log.Printf("⚠️  OLLAMA_MODEL=%q 不在本机 Ollama 里；响应里会报 404。可用的：%s", llmModel, available)
		log.Printf("    修法：setx OLLAMA_MODEL <上面某个> 后重启，或把运行/data/llm-profiles.json 的 default 改成它")
	}
	log.Printf("语言模型：%s（%s）", llmModel, cfg.OllamaBaseURL)

	llmService := service.NewLLMService(cfg.OllamaBaseURL, llmModel)

	comfyURL := getEnv("COMFYUI_BASE_URL", "http://localhost:8188")
	h3URL := getEnv("H3_BASE_URL", "http://127.0.0.1:8189")
	asrURL := getEnv("ASR_BASE_URL", "")
	ttsURL := getEnv("TTS_BASE_URL", "")

	// 环境变量为空时先看 data/deps.json 的 tts.base_url。
	if ttsURL == "" {
		ttsURL = service.TTSBaseURL()
	}
	if asrURL == "" && service.DetectASRHome() != "" {
		asrURL = "http://127.0.0.1:8101"
	}
	if ttsURL == "" && service.DetectKokoroScript() != "" {
		ttsURL = "http://127.0.0.1:8102"
	}

	// 模型 bundle：逻辑角色 → 权重文件 + 默认参数，落 data/models.json。
	bundleStore := service.LoadBundleStore(filepath.Join(baseDir, "data", "models.json"))
	if err := bundleStore.LoadErr(); err != nil {
		log.Printf("Warning: %v（继续使用内置默认模型配置）", err)
	}
	ckpt := getEnv("COMFYUI_CHECKPOINT", "")
	if ckpt == "" {
		ckpt = bundleStore.ConfiguredFile(service.RoleT2IPrimary, "ckpt")
	}
	if ckpt == "" {
		ckpt = service.DefaultCheckpoint
	}
	t2iService := service.NewText2ImageService(comfyURL, ckpt)
	t2iService.Bundles = bundleStore

	// 多实例路由：模型 → 实例（H3 的 torch 2.14 独立工作树 vs 主实例 torch 2.6）
	instRegistry := service.LoadInstanceRegistry(filepath.Join(baseDir, "data", "instances.json"), h3URL)
	if note := instRegistry.LoadNote(); note != "" {
		log.Printf("Warning: %v", note)
	}
	t2iService.Instances = instRegistry
	t2iService.Template = getEnv("COMFYUI_TEMPLATE", "")

	asrService := service.NewASRService(asrURL)
	ttsService := service.NewTTSService(ttsURL)
	videoService := service.NewVideoService()

	gpuSched := service.NewGPUScheduler(24576)
	gpuSched.RegisterModel("qwen2.5:7b", "llm", 4500)
	gpuSched.RegisterModel("qwen2.5:13b", "llm", 8500)
	gpuSched.RegisterModel("sd_xl_base_1.0", "text2image", 11000)
	gpuSched.RegisterModel("whisper-large-v3", "asr", 6000)
	gpuSched.RegisterModel("flux1-schnell-fp8", "text2image", 11000)

	engine := service.NewEngineV2(llmService, t2iService, asrService, ttsService, videoService, gpuSched, wsHub)

	// 显存档案：每次重节点执行后累积实测峰值，供提交前预测（data/vram_profiles.json）
	profileStore := service.LoadProfileStore(filepath.Join(baseDir, "data", "vram_profiles.json"), 24576)
	engine.SetProfiles(profileStore)

	// 自定义模型端点（data/llm-profiles.json）：节点 llm_profile 引用。
	llmProfileStore := service.LoadLLMProfiles(filepath.Join(baseDir, "data", "llm-profiles.json"))
	engine.SetLLMProfiles(llmProfileStore)

	// 视觉模型：本地装了 VL 模型就自动启用 vision 节点
	if visionModel := service.PickVisionModel(cfg.OllamaBaseURL); visionModel != "" {
		engine.SetVisionModel(visionModel)
		log.Printf("视觉理解已启用：%s", visionModel)
	}

	// 依赖进程管理器：ComfyUI / ASR / TTS 可一键托管（先探活，已在跑则复用）
	depSupervisor := service.NewSupervisor(comfyURL, asrURL, ttsURL)
	engine.SetSupervisor(depSupervisor)
	depSupervisor.AutoStartAll()

	// 循环节点解析器（从 DB 读取被循环的工作流定义）
	engine.SetResolver(func(id uint) ([]byte, []byte, error) {
		var wf model.Workflow
		if err := db.First(&wf, id).Error; err != nil {
			return nil, nil, err
		}
		return []byte(wf.Nodes), []byte(wf.Edges), nil
	})

	// 事件日志：广播与落库并行
	if eventLogger != nil {
		engine.SetEventLog(eventLogger)
	}

	gpuSched.StartTelemetry(5 * time.Second)
	gpuSched.SetUnloader(func(name, modelType string) error {
		switch modelType {
		case "llm":
			return llmService.UnloadModel(name)
		case "text2image":
			return t2iService.UnloadModels()
		}
		return nil
	})

	if getEnv("FLOWEDGE_MODE", "release") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.Default()

	// 安全边界：默认只监听 127.0.0.1；对外开放必须带 token。
	bindAddr := getEnv("FLOWEDGE_HOST", "127.0.0.1")
	apiToken := getEnv("FLOWEDGE_API_TOKEN", "")
	openToLan := bindAddr != "127.0.0.1" && bindAddr != "localhost"
	if openToLan && apiToken == "" {
		log.Println("⚠️  FLOWEDGE_HOST 暴露到局域网但未设置 FLOWEDGE_API_TOKEN：已自动拒绝启动")
		os.Exit(2)
	}
	if apiToken != "" {
		r.Use(func(c *gin.Context) {
			p := c.Request.URL.Path
			if p == "/api/health" || p == "/api/capabilities" {
				c.Next()
				return
			}
			if c.GetHeader("Authorization") != "Bearer "+apiToken {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid API token"})
				return
			}
			c.Next()
		})
		log.Println("🔒 API token 已启用")
	}

	corsAllow := getEnv("FLOWEDGE_CORS_ORIGIN", "")
	r.Use(func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		allow := corsAllow
		if allow == "" {
			if origin == "" || strings.HasPrefix(origin, "http://127.0.0.1") || strings.HasPrefix(origin, "http://localhost") {
				allow = origin
			}
		}
		if allow != "" {
			c.Header("Access-Control-Allow-Origin", allow)
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// 任务队列：默认并发 2（单 GPU 上并行跑两个 flux 只会互相抢显存）
	jobQueue := service.NewJobQueue(db, getQueueConcurrency())
	if n := jobQueue.RecoverInterrupted(); n > 0 {
		log.Printf("上次异常退出的运行任务 %d 个，已标记为 interrupted", n)
	}

	pipesPath := filepath.Join(baseDir, "data", "pipelines.json")
	pipes, perr := service.LoadPipelines(pipesPath)
	if perr != nil {
		log.Fatalf("流水线定义加载失败：%v\n"+
			"  它现在是产品本身（7 条固定出片流水线），没有它引擎不启动。\n"+
			"  修法：把仓库 data/pipelines.json 复制到 %s（dist/data/ 里构建时已带一份）", perr, pipesPath)
	}

	producer := handler.NewProducer(db, engine, jobQueue, pipes)
	// 运行前依赖预检：Ollama/ASR/TTS/ComfyUI 没就绪时在入口显式失败并给修复动作
	producer.SetPreflight(depSupervisor, cfg.OllamaBaseURL, comfyURL, h3URL, asrURL, ttsURL)
	if n := producer.SweepTransient(); n > 0 {
		log.Printf("清理上次残留的质量循环临时图 %d 条", n)
	}

	api := r.Group("/api")
	{
		wsHandler := handler.NewWSHandler(wsHub)

		// 流水线目录：产品形态本身（界面按它渲染向导，字段与默认值都在 JSON 里）
		api.GET("/pipelines", producer.ListPipelines)
		api.POST("/pipelines/reload", producer.ReloadPipelines)
		// 出片前体检（只探依赖、不跑）
		api.POST("/preflight", producer.PreflightPipelines)
		// 出片：唯一运行入口
		api.POST("/produce", producer.Produce)

		// 运行看板
		runHandler := handler.NewRunHandler(db, eventLogger)
		api.GET("/runs", runHandler.ListAll)
		api.GET("/runs/:runId", producer.GetRun)
		api.POST("/runs/:runId/cancel", producer.CancelRun)
		api.POST("/runs/:runId/rerun", producer.Rerun)
		api.GET("/runs/:runId/events", runHandler.Events)
		api.GET("/runs/:runId/trace", runHandler.Trace)

		// 产物管理（生成的图片 / 视频 / 音频）
		artifactHandler := handler.NewArtifactHandler(getEnv("GENERATED_DIR", "data/generated"))
		api.GET("/artifacts", artifactHandler.List)
		api.GET("/artifacts/:name", artifactHandler.Serve)
		api.DELETE("/artifacts/:name", artifactHandler.Delete)

		// 依赖进程托管（ComfyUI / ASR / TTS 一键启停）
		depHandler := handler.NewDepHandler(depSupervisor)
		api.GET("/deps", depHandler.List)
		api.POST("/deps/reload", depHandler.Reload)
		api.POST("/deps/:name/start", depHandler.Start)
		api.POST("/deps/:name/stop", depHandler.Stop)
		api.GET("/deps/:name/wait", depHandler.Wait)

		// 对外接入配置生成（MCP 客户端配置）：exe 绝对路径与端口只有引擎自己知道
		integHandler := handler.NewIntegrationHandler(baseDir, cfg.Port)
		api.GET("/mcp/config", integHandler.MCPConfig)

		api.GET("/ws", wsHandler.HandleWS)

		healthHandler := handler.NewHealthHandler(version)
		api.GET("/health", healthHandler.Health)
	}

	mountStatic(r)

	go func() {
		time.Sleep(200 * time.Millisecond)
		caps := service.ProbeCapabilities(gpuSched, baseDir, cfg.OllamaBaseURL, comfyURL, h3URL, asrURL, ttsURL)
		log.Printf("环境自检  GPU %s | LLM(Ollama) %s | 文生图(ComfyUI) %s | 视频(ffmpeg) %s | 音频(ASR+TTS) %s",
			mark(caps.GPU.OK), mark(caps.Ollama.OK), mark(caps.ComfyUI.OK), mark(caps.FFmpeg.OK), mark(caps.ASR.OK && caps.TTS.OK))
		if len(caps.Missing) > 0 {
			log.Printf("当前不可用的能力：%v —— /api/capabilities 可查看修复指引", caps.Missing)
		}
	}()

	browserURL := fmt.Sprintf("http://127.0.0.1:%s", cfg.Port)
	log.Printf("FlowEdge Lite 已启动：%s（Ctrl+C 停止）", browserURL)
	if getEnv("FLOWEDGE_OPEN_BROWSER", "") == "1" {
		openBrowser(browserURL)
	}
	if err := r.Run(bindAddr + ":" + cfg.Port); err != nil {
		if openToLan {
			log.Printf("启动失败：%v\n    如需局域网访问，请检查 FLOWEDGE_HOST=%s 是否为合法地址", err, bindAddr)
		} else {
			log.Printf("启动失败：%v（端口 %s 可能已被占用，可用 PORT 环境变量改端口）", err, cfg.Port)
		}
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getQueueConcurrency 队列并发上限（默认 2：本地多模态任务多为显存密集，并行反而慢）
func getQueueConcurrency() int {
	c := 2
	if v := os.Getenv("FLOWEDGE_QUEUE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c = n
		}
	}
	return c
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
<p>① 运行 <code>python scripts/build-windows.py</code> 打包（界面会内嵌进 exe）</p>
<p>② 设置环境变量 <code>STATIC_DIR</code> 指向前端构建目录（含 index.html）</p>
<p>环境自检：<a href="/api/capabilities">/api/capabilities</a></p></div></html>`

// mark 自检结果的可读标记
func mark(ok bool) string {
	if ok {
		return "OK"
	}
	return "缺失"
}

// openBrowser 用系统默认浏览器打开 URL。
//
// Windows 上用 rundll32 url.dll,FileProtocolHandler 而不是 explorer：
// `explorer <url>` 在部分精简系统上会打开「资源管理器」而不是浏览器。
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd, args = "open", []string{url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	c := exec.Command(cmd, args...)
	c.Stdout, c.Stderr = nil, nil
	_ = c.Start()
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
	fmt.Printf("启用步骤：cd \"%s\" && python -m venv .venv && .venv\\Scripts\\pip install -r requirements.txt\n", outDir)
	fmt.Println("然后 .venv\\Scripts\\python tts_server.py（8102）/ asr_server.py（8101）")
}

// parseLimit 解析查询串里的条数上限
func parseLimit(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 && v <= 200 {
		return v
	}
	return def
}

// checkpointsFromBundles 把 models.json 里各 bundle 的 ckpt 去重成一份可选清单。
func checkpointsFromBundles(store *service.BundleStore) []string {
	if store == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, role := range store.Roles() {
		b := store.Resolve(role)
		if b == nil {
			continue
		}
		ckpt := b.Files["ckpt"]
		if ckpt == "" || seen[ckpt] {
			continue
		}
		seen[ckpt] = true
		out = append(out, ckpt)
	}
	sort.Strings(out)
	return out
}

// setupLogFile 把日志同时写到 <baseDir>/logs/engine.log。
//
// 为什么必须落盘：引擎常驻时由外壳（序策工作站 / 1-启动 bat）拉起，stdout 被
// 父进程的管道吃掉了。于是"跑挂了/跑得慢"的时候，agent 和人手里一行日志都没有，
// 只能靠猜——这正是 §0.2「报错字符串不是根因」最容易踩空的地方。
//
// 大小上限 8MB，超了滚一份 .1（不引入轮转库：一个文件足够定位问题）。
func setupLogFile(baseDir string) func() {
	dir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}
	}
	path := filepath.Join(dir, "engine.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 8<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return func() {}
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	log.Printf("=== FlowEdge %s 启动，日志写入 %s ===", version, path)
	return func() { _ = f.Close() }
}
