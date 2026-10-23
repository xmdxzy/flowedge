package main

import (
	"context"
	"encoding/json"
	"fmt"
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

	// 子命令：MCP server（stdio）。给 Claude Desktop / Cursor 这类客户端接工作流用。
	//
	// 刻意做成"瘦客户端转发"而不是在 stdio 进程里再起一套引擎：
	// 两个引擎会抢同一张卡的显存和同一个 sqlite 文件。
	// 代价是引擎必须先起来——所以每个工具在不可达时返回能照做的错误。
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

	// 连接数据库（默认本地 SQLite，零外部依赖；DB_DRIVER=postgres 时使用 PostgreSQL）
	db := initDB(cfg)
	if db != nil {
		if err := db.AutoMigrate(&model.Workflow{}, &model.WorkflowRun{}, &model.WorkflowVersion{}, &model.RunEvent{}, &model.Job{}, &model.ShotJob{}, &model.Promotion{}, &service.ArtifactLineage{}); err != nil {
			log.Printf("Warning: auto migrate failed: %v", err)
		} else {
			log.Println("Database migrated")
		}
	}

	// 事件中心
	wsHub := service.NewWSHub()

	// 事件日志持久化：事件不再「广播即丢」，落库后可回放、统计 token、生成 trace。
	// 批量异步写入，落库失败只记日志，不影响引擎执行。
	var eventLogger *service.EventLogger
	if db != nil {
		eventLogger = service.NewEventLogger(db)
		defer eventLogger.Close()
	}

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
	// H3 是独立工作树 + 独立 venv，端口 8189；与主实例互斥。
	// 能力面板必须分开探测，否则「H3 在跑」会被显示成「文生图缺失」（AGENTS §0.6）。
	h3URL := getEnv("H3_BASE_URL", "http://127.0.0.1:8189")
	asrURL := getEnv("ASR_BASE_URL", "")
	ttsURL := getEnv("TTS_BASE_URL", "")

	// 本地 ASR 服务目录存在时，默认端点指向它（asr_server.py 固定监听 8101）。
	// 否则会出现「引擎把服务拉起来了、能力面板却显示未启用、ASR 节点仍报 config_missing」
	// 这种自相矛盾的状态——配置为空的语义应该是「用本地默认」，不是「不可用」。
	if asrURL == "" && service.DetectASRHome() != "" {
		asrURL = "http://127.0.0.1:8101"
	}
	// 同理：本地有 Kokoro 脚本就默认用神经 TTS；服务没起时引擎自动落回 Windows SAPI
	if ttsURL == "" && service.DetectKokoroScript() != "" {
		ttsURL = "http://127.0.0.1:8102"
	}

	// 模型 bundle：逻辑角色 → 权重文件 + 默认参数，落 data/models.json。
	// 首次运行自动写一份默认配置出来——用户需要看到「原来模型名在这里改」。
	bundleStore := service.LoadBundleStore(filepath.Join(baseDir, "data", "models.json"))
	if err := bundleStore.LoadErr(); err != nil {
		log.Printf("Warning: %v（继续使用内置默认模型配置）", err)
	}
	// 优先级：显式 COMFYUI_CHECKPOINT > models.json 里配置的 ckpt > 内置默认
	ckpt := getEnv("COMFYUI_CHECKPOINT", "")
	if ckpt == "" {
		ckpt = bundleStore.ConfiguredFile(service.RoleT2IPrimary, "ckpt")
	}
	if ckpt == "" {
		ckpt = service.DefaultCheckpoint
	}
	t2iService := service.NewText2ImageService(comfyURL, ckpt)
	t2iService.Bundles = bundleStore
	// 多实例路由：模型 → 实例（H3 的 torch 2.14 独立工作树 vs 主实例 torch 2.6）。
	// 首次运行落一份默认配置，等价于原来的「h3 走 8189、其余走 8188」。
	instRegistry := service.LoadInstanceRegistry(filepath.Join(baseDir, "data", "instances.json"), h3URL)
	if note := instRegistry.LoadNote(); note != "" {
		log.Printf("Warning: %v", note)
	}
	t2iService.Instances = instRegistry
	// 可选：指定 data/comfy_workflows/<name>.json 作为自定义工作流模板
	t2iService.Template = getEnv("COMFYUI_TEMPLATE", "")
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

	// 显存档案：每次重节点执行后累积实测峰值，供提交前预测（data/vram_profiles.json）
	profileStore := service.LoadProfileStore(filepath.Join(baseDir, "data", "vram_profiles.json"), 24576)
	engine.SetProfiles(profileStore)

	// 凭据仓库：AES-256-GCM 加密落 data/credentials.json，HTTP 节点按名称引用
	credStore, credErr := service.NewCredentialStore("data")
	if credErr != nil {
		log.Printf("Warning: 凭据仓库初始化失败（HTTP 节点将无法使用 credential 字段）：%v", credErr)
	} else {
		engine.SetCredentials(credStore)
	}

	// 自定义模型端点（data/llm-profiles.json）：节点 llm_profile 引用。
	// 与 models.json（扩散权重 bundle）是两张表：一个管"跑什么权重"，一个管"调哪个端点"。
	llmProfileStore := service.LoadLLMProfiles(filepath.Join(baseDir, "data", "llm-profiles.json"))
	engine.SetLLMProfiles(llmProfileStore)

	// 外部智能体名册（data/agents.json）：agent_task 节点引用。
	// 与 llm-profiles 是两张表：那张管"调哪个模型"，这张管"把活儿交给哪个智能体"。
	agentStore := service.LoadAgents(filepath.Join(baseDir, "data", "agents.json"))
	engine.SetAgents(agentStore)
	// 技能包扫描根：agent_task 的 skill_refs 靠它把包名换成真实路径。
	// 两类根分开传：本机技能生态（用户级/冷存储/DSH/agents/项目级/额外根文件）与
	// 专家插件市场（plugins/<插件>/skills/<技能>）——布局与来源语义都不同。
	engine.SetSkillRoots(service.SkillRoots(baseDir))
	engine.SetPluginRoots(service.PluginSkillRoots(baseDir))

	// 工具链：skill / MCP / 插件 / 智能体的统一发现层（/api/toolchain）。
	// 四类工具共用一张表、一套启停语义，前端才能用一套面板管理；
	// 也是 mcp_call 节点按名字找服务器的入口。
	toolchain := service.NewToolchain(baseDir, engine, agentStore)
	engine.SetToolchain(toolchain)

	// 能力卡片（data/cards.json）：内置一份，用户可覆盖/追加。
	// agent/skill 只改提示词；workflow 卡会改执行结构，见 executeNode 的拦截。
	cardStore := service.LoadCards(filepath.Join(baseDir, "data", "cards.json"))
	engine.SetCards(cardStore)
	// 任务书编译不持有 Engine，靠它反查卡片名字
	service.SetDefaultCards(cardStore)
	// 任务书里的工作流卡要显示流水线名，而不是 "#12"
	service.SetWorkflowNameResolver(func(id uint) string {
		var w model.Workflow
		if err := db.Select("name").First(&w, id).Error; err != nil {
			return ""
		}
		return w.Name
	})

	// 产物血缘
	lineageStore := service.NewLineageStore(db)

	// 视觉模型：本地装了 VL 模型就自动启用 vision 节点
	visionModel := service.PickVisionModel(cfg.OllamaBaseURL)
	if visionModel != "" {
		engine.SetVisionModel(visionModel)
		log.Printf("视觉理解已启用：%s", visionModel)
	}
	modelsRoot := service.ModelsRoot(baseDir)
	log.Printf("模型根目录：%s", modelsRoot)

	// 依赖进程管理器：ComfyUI / ASR 可一键托管（先探活，已在跑则复用）
	depSupervisor := service.NewSupervisor(comfyURL, asrURL, ttsURL)
	engine.SetSupervisor(depSupervisor)
	depSupervisor.AutoStartAll()

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

	// 插件注册表：把内置示例插件接进节点注册表。
	// 这段代码此前从未被调用（plugin_system.go 是完整实现但零接线），
	// 接入后新增节点类型确实可以不改引擎分发逻辑。
	pluginRegistry := service.NewPluginRegistry()
	service.RegisterBuiltinPlugins(pluginRegistry)

	// 运行期插件（data/plugins/<name>/plugin.json，script 或 http 型）走**工具链热注册表**，
	// 与「工具台 → 重新扫描」完全同一条路径（见 service.Toolchain.Reload）。
	//
	// 为什么不在这里再写一遍加载逻辑：两套实现必然漂移——启动时能加载的插件，
	// 热重载后不认，或者反过来；且停用的插件在启动路径里被漏掉就会诈尸。
	// 一个入口，两种时机。
	pluginCount := 0
	for _, plugin := range pluginRegistry.All() {
		p := plugin // 闭包捕获
		exec := func(_ context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
			out, err := p.Execute(upstream, node.Data)
			if err != nil {
				return nil, err
			}
			if out == nil {
				return map[string]interface{}{}, nil
			}
			return out, nil
		}
		side := ""
		if rp, ok := plugin.(*service.RuntimePlugin); ok {
			side = rp.Side()
		}
		if side != "" {
			engine.RegisterNodeTypeWithSide(p.Type(), side, exec)
		} else {
			engine.RegisterNodeType(p.Type(), exec)
		}
		pluginCount++
	}
	if tcRes := toolchain.Reload(); tcRes.Plugins > 0 || len(tcRes.Problems) > 0 {
		if len(tcRes.Problems) > 0 {
			for _, perr := range tcRes.Problems {
				log.Printf("Warning: 运行期插件加载失败 %s", perr)
			}
		}
		if tcRes.Plugins > 0 {
			log.Printf("已热注册 %d 个运行期插件：%v", tcRes.Plugins, tcRes.NodeTypes)
		}
	}
	if pluginCount > 0 {
		log.Printf("已注册 %d 个内置插件节点类型", pluginCount)
	}

	// 流式 LLM（token 经 WS 推送前端打字机效果）
	engine.SetLLMStream(llmStreamService)

	// 事件日志：广播与落库并行（node_token 不入库，避免写放大）
	if eventLogger != nil {
		engine.SetEventLog(eventLogger)
		// 镜头级作业：每镜落盘，失败重跑时复用已完成镜头（见 model.ShotJob）
		engine.SetShotJobs(service.NewShotJobStore(db))
	}

	// GPU 真实遥测轮询（缓存 nvidia-smi 结果，避免每请求拉子进程）
	gpuSched.StartTelemetry(5 * time.Second)

	// 真实卸载钩子：没注入之前，调度器只会把内存里的状态改成 idle，
	// 模型仍占着显存，下一次 Acquire 照样失败。这里把「卸载」接到推理服务自己身上。
	gpuSched.SetUnloader(func(name, modelType string) error {
		switch modelType {
		case "llm":
			return llmService.UnloadModel(name)
		case "text2image":
			return t2iService.UnloadModels()
		}
		return nil
	})

	// 注册显存占用档案：flux fp8 实测约 11GB、whisper-large-v3 约 6GB
	gpuSched.RegisterModel("flux1-schnell-fp8", "text2image", 11000)

	if getEnv("FLOWEDGE_MODE", "release") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 初始化路由
	r := gin.Default()

	// 安全边界：
	//  - 默认只监听 127.0.0.1。其他机器要访问必须显式 FLOWEDGE_HOST=0.0.0.0，
	//    且必须同时设置 FLOWEDGE_API_TOKEN，否则任何拿到地址的人都能删除全部工作流
	//  - CORS 不再无条件放 *：仅本机源或显式配置的 FLOWEDGE_CORS_ORIGIN
	bindAddr := getEnv("FLOWEDGE_HOST", "127.0.0.1")
	apiToken := getEnv("FLOWEDGE_API_TOKEN", "")
	openToLan := bindAddr != "127.0.0.1" && bindAddr != "localhost"
	if openToLan && apiToken == "" {
		log.Println("⚠️  FLOWEDGE_HOST 暴露到局域网但未设置 FLOWEDGE_API_TOKEN：所有 /api 接口（含删除）对外裸奔，已自动拒绝启动")
		log.Println("    请补上 FLOWEDGE_API_TOKEN=<随机长字符串>，或把 FLOWEDGE_HOST 改回 127.0.0.1")
		os.Exit(2)
	}

	if apiToken != "" {
		r.Use(func(c *gin.Context) {
			// 这几个端点维持开放：公开触发器吃的是自己的随机 token，健康检查用于探活
			p := c.Request.URL.Path
			if p == "/api/health" || p == "/api/capabilities" ||
				(len(p) > 10 && p[:10] == "/api/hooks") {
				c.Next()
				return
			}
			if c.GetHeader("Authorization") != "Bearer "+apiToken {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid API token"})
				return
			}
			c.Next()
		})
		log.Println("🔒 API token 已启用：除 /api/hooks/*、/api/health、/api/capabilities 外均需 Authorization: Bearer <token>")
	}

	corsAllow := getEnv("FLOWEDGE_CORS_ORIGIN", "")
	r.Use(func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		allow := corsAllow
		if allow == "" {
			// 未显式配置时仅放行本机源（file:// 的 Origin 为空也算本机）
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

	// handlers（cron 调度器也要用到 wfHandler，故在路由组之前构造）
	// 任务队列：默认并发 2（单 GPU 上并行跑两个 flux 只会互相抢显存）
	jobQueue := service.NewJobQueue(db, getQueueConcurrency())
	if n := jobQueue.RecoverInterrupted(); n > 0 {
		log.Printf("上次异常退出的运行任务 %d 个，已标记为 interrupted（可在执行记录里续跑）", n)
	}
	wfHandler := handler.NewWorkflowHandler(db, engine, jobQueue)
	wfHandler.SetLineage(lineageStore)
	// 运行前依赖预检：Ollama/ASR/TTS/视觉模型没就绪时在入口显式失败，
	// 而不是让节点跑进去报一句连接错误（ComfyUI 两实例由 ensureComfy 自动拉起，不拦）
	wfHandler.SetPreflight(depSupervisor, cfg.OllamaBaseURL, comfyURL, h3URL, asrURL, ttsURL)
	// 资源准入闸：执行前问一句序策治理器「现在能不能跑」（fail-open，问不到就放行）。
	// FLOWEDGE_GOVERNOR=off 可关；GOVERNOR_BASE_URL 默认 127.0.0.1:30002。
	govGate := service.NewGovernorGate()
	wfHandler.SetGovernor(govGate)
	if govGate.Enabled() {
		log.Printf("[Governor] 资源准入已启用（kind=%s）", getEnv("GOVERNOR_KIND", "flowedge"))
	} else {
		log.Printf("[Governor] 资源准入已关闭（FLOWEDGE_GOVERNOR=off）")
	}
	triggerHandler := handler.NewTriggerHandler(db, wfHandler)
	debugHandler := handler.NewDebugHandler(db, engine, lineageStore)

	// 定时触发器：每 20 秒扫描一次启用了 cron 的工作流（分钟级去重）
	cronScheduler := service.NewCronScheduler(db, func(id uint) error {
		var wf model.Workflow
		if err := db.First(&wf, id).Error; err != nil {
			return err
		}
		wfHandler.StartRun(&wf, "cron")
		return nil
	})
	cronScheduler.Start(20 * time.Second)

	api := r.Group("/api")
	{
		versionHandler := handler.NewVersionHandler(db)
		wsHandler := handler.NewWSHandler(wsHub)

		api.POST("/preflight", wfHandler.PreflightCheck) // 只体检不运行：返回缺失依赖与可执行修复动作
		api.GET("/workflows", wfHandler.List)
		api.GET("/workflows/:id", wfHandler.Get)
		api.POST("/workflows", wfHandler.Create)
		api.PUT("/workflows/:id", wfHandler.Update)
		api.DELETE("/workflows/:id", wfHandler.Delete)

		// 任务书：把画布编译成可执行规格（md 给人/智能体读，json 给程序读）
		api.GET("/workflows/:id/spec", wfHandler.Spec)
		// 任务书回写（Compose 的另一半）：编辑 → 校验 → 快照 → 落图
		api.POST("/workflows/:id/spec/apply", wfHandler.SpecApply)
		// 模块目录：逻辑与模态同构的积木清单（端口 + 参数标签 + 门层 + 执行元数据）
		moduleHandler := handler.NewModuleHandler()
		api.GET("/modules", moduleHandler.List)

		// 自定义模型端点：LLM 节点的「我的模型」下拉读这里
		lpHandler := handler.NewLLMProfileHandler(llmProfileStore)
		api.GET("/llm/profiles", lpHandler.List)
		api.PUT("/llm/profiles", lpHandler.Replace)

		// 能力卡片目录：智能体卡 / 技能卡 / 工作流封装卡
		cardHandler := handler.NewCardHandler(db, cardStore)
		api.GET("/cards", cardHandler.List)
		api.PUT("/cards", cardHandler.Save)

		// 技能包索引：把机器上的 SKILL.md 全部盘一遍，给归属判定 + 可核验证据。
		// 只读，不产生可执行能力——能贴到节点上的东西一律走 /api/cards。
		skillPackHandler := handler.NewSkillPackHandler(baseDir)
		api.GET("/skills", skillPackHandler.List)
		api.GET("/skills/:name", skillPackHandler.ReadDoc)

		// 工具链：skill / MCP / 插件 / 智能体四类工具的统一清单与治理。
		//
		// 与 /api/skills 的关系：/api/skills 是「技能包的归属审计」（只读判定），
		// /api/toolchain 是「这台机器上有什么工具、怎么用、能不能用」（可操作）。
		// 两者数据源重叠但目的不同，不做合并——合并会让审计口径被"启停"污染。
		toolchainHandler := handler.NewToolchainHandler(toolchain)
		api.GET("/toolchain", toolchainHandler.List)
		api.POST("/toolchain/reload", toolchainHandler.Reload)
		api.GET("/toolchain/doc", toolchainHandler.Doc)
		api.POST("/toolchain/adopt", toolchainHandler.AdoptAgent)
		api.POST("/toolchain/agents/:id/enabled", toolchainHandler.SetAgentEnabled)

		// MCP 服务器：增删改启停 + 探活 + 工具清单 + 试跑。
		// 试跑与 mcp_call 节点共用 service.Toolchain.CallMCP，不存在两套行为。
		api.GET("/mcp/servers/:name/tools", toolchainHandler.Tools)
		api.POST("/mcp/servers/:name/probe", toolchainHandler.ProbeMCP)
		api.POST("/mcp/servers/:name/call", toolchainHandler.CallMCP)
		api.POST("/mcp/servers/:name/enabled", toolchainHandler.SetMCPEnabled)
		api.PUT("/mcp/servers", toolchainHandler.UpsertMCP)
		api.DELETE("/mcp/servers/:name", toolchainHandler.DeleteMCP)

		// 引擎自有插件启停（改 plugin.json 后立刻热重载，节点类型同步出现/消失）
		api.POST("/plugins/:name/enabled", toolchainHandler.SetPluginEnabled)

		// 外部智能体名册：agent_task 节点的「智能体」下拉读这里。
		// 列表默认带体检结果（配置了 ≠ 装好了），probe=0 可跳过探测。
		agentHandler := handler.NewAgentHandler(engine)
		api.GET("/agents", agentHandler.List)
		api.PUT("/agents", agentHandler.Upsert)
		api.DELETE("/agents/:id", agentHandler.Delete)
		api.POST("/agents/:id/probe", agentHandler.Probe)

		// 图结构评分：不执行就能判「这张图能不能跑」，compose 的准入指标（docs §5.9.4）
		graphHandler := handler.NewGraphHandler(db)
		api.POST("/graph/score", graphHandler.Score)

		// 构图：一句话目标 → 可执行的图 + 任务书（docs §3.6.5，唯一缺的半条腿）
		// 挂在 /api/compose 而不是 /api/workflows/compose：后者会和 :id 路由混在一起，
		// 读代码时要多想想"compose 是不是一个工作流 id"。
		composeHandler := handler.NewComposeHandler(engine, db)
		api.POST("/compose", composeHandler.Compose)

		// 证据聚合与登记：把散落的 evidence 变成「哪里在漏」+ 可登记的候选（docs §8.4 P0-1）
		evHandler := handler.NewEvidenceHandler(db, govGate)
		evHandler.SetWorkflowHandler(wfHandler)
		api.GET("/evidence/summary", evHandler.Summary)
		api.POST("/evidence/register", evHandler.Register)
		// 候选生成 → 试验：把诊断翻成"能试的改动"，试用后自动快照可回滚
		api.POST("/evidence/candidates", evHandler.Candidates)
		api.POST("/evidence/trial", evHandler.Trial)

		// 自进化闭环：漏在哪 → 改哪 → 是不是作弊 → 该不该留（docs 自进化方案 v1）
		//
		// 与 /evidence/* 的分工：evidence 那组回答"这批 run 成不成"（聚合视角），
		// 这组回答"下一版引擎该动哪个参数、动了算不算进化"（变化视角）。
		// 真跑仍复用 /evidence/trial，不新增第二条写图路径。
		evoHandler := handler.NewEvolutionHandler(db, engine)
		evoHandler.SetWorkflowHandler(wfHandler)
		api.GET("/evolution/status", evoHandler.Status)
		api.POST("/evolution/propose", evoHandler.Propose)
		api.POST("/evolution/audit", evoHandler.Audit)
		api.POST("/evolution/decide", evoHandler.Decide)
		// 闭环补全：接尺子 → 跑配对 → 提变体 → 盯回归 → 回滚
		api.POST("/evolution/instrument", evoHandler.Instrument)
		api.POST("/evolution/evaluate", evoHandler.Evaluate)
		api.GET("/evolution/batch", evoHandler.Batch)
		api.POST("/evolution/variants", evoHandler.Variants)
		api.GET("/evolution/monitor", evoHandler.Monitor)
		api.POST("/evolution/rollback", evoHandler.Rollback)
		// 晋升台账：人按下晋升/保留 + 台账查询
		api.POST("/evolution/promote", evoHandler.Promote)
		api.GET("/evolution/promotions", evoHandler.Promotions)

		api.POST("/workflows/:id/run", wfHandler.Run)
		api.GET("/workflows/:id/runs", wfHandler.ListRuns)
		api.GET("/runs/:runId", wfHandler.GetRun)
		api.POST("/runs/:runId/resume", wfHandler.ResumeRun)
		api.POST("/runs/:runId/rerun-from", wfHandler.ResumeFromRun)
		api.POST("/runs/:runId/cancel", wfHandler.CancelRun)

		// 单步调试与断点：step 执行一个节点后暂停，continue 跑到下一个断点/结束
		api.POST("/runs/:runId/step", debugHandler.Step)
		api.POST("/runs/:runId/continue", debugHandler.Continue)

		// 镜头级作业：按镜查看 / 只重拍某一镜（不修改用户保存的工作流）
		api.GET("/runs/:runId/shots", wfHandler.ListShots)
		api.POST("/runs/:runId/shots/:index/reroll", wfHandler.RerollShot)

		// 运行维度：全局列表（分页/状态过滤）、事件回放、trace 与 token 统计
		runHandler := handler.NewRunHandler(db, eventLogger)
		api.GET("/runs", runHandler.ListAll)
		api.GET("/runs/:runId/events", runHandler.Events)
		api.GET("/runs/:runId/trace", runHandler.Trace)

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
		// 显存时间线：谁在什么时候占了显存、有没有被驱逐
		api.GET("/gpu/timeline", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"events": gpuSched.Timeline(parseLimit(c.Query("limit"), 60)),
				"status": gpuSched.GetSchedulerStatus(),
			})
		})

		// 产物管理（生成的图片 / 视频 / 音频）
		artifactHandler := handler.NewArtifactHandler(getEnv("GENERATED_DIR", "data/generated"))
		artifactHandler.SetLineage(lineageStore)
		api.GET("/artifacts", artifactHandler.List)
		api.GET("/artifacts/:name", artifactHandler.Serve)
		api.GET("/artifacts/:name/lineage", artifactHandler.Lineage)
		api.DELETE("/artifacts/:name", artifactHandler.Delete)
		// 血缘总表（画廊批量展示来源）
		api.GET("/lineage", artifactHandler.LineageList)

		// 凭据（值永不下发，只回名称与 header 名）
		credHandler := handler.NewCredentialHandler(credStore)
		api.GET("/credentials", credHandler.List)
		api.POST("/credentials", credHandler.Set)
		api.DELETE("/credentials/:name", credHandler.Delete)

		// 模型资产清点（根目录 / 各家族体积 / 具体文件）
		api.GET("/models", func(c *gin.Context) {
			inv := service.ScanModels(service.ModelsRoot(baseDir))
			ollamaModels := []string{}
			// 模型体积：下拉里要能看出"这个模型多大"，否则用户只能靠名字猜
			ollamaSizes := map[string]float64{}
			if body, err := http.Get(cfg.OllamaBaseURL + "/api/tags"); err == nil {
				var tags struct {
					Models []struct {
						Name string `json:"name"`
						Size int64  `json:"size"`
					} `json:"models"`
				}
				if json.NewDecoder(body.Body).Decode(&tags) == nil {
					for _, m := range tags.Models {
						ollamaModels = append(ollamaModels, m.Name)
						if m.Size > 0 {
							ollamaSizes[m.Name] = float64(m.Size) / (1 << 30)
						}
					}
				}
				body.Body.Close()
			}
			c.JSON(http.StatusOK, gin.H{
				"inventory":     inv,
				"ollama_models": ollamaModels,
				"vision_model":  service.PickVisionModel(cfg.OllamaBaseURL),
				"models_dir":    modelsRoot,
				"resident_side": engine.ResidentSide(),
				"llm_backend":   llmService.BackendLabel(),
				// 具体名字要能看见：面板里不能只说"自动探测"
				"default_llm":     llmService.Model,
				"ollama_sizes_gb": ollamaSizes,
				"t2i_checkpoint":  t2iService.Checkpoint,
				// 出图后端列表（flux / moss / auto）。放在接口里而不是让前端硬编码，
				// 将来加第三个后端时前端下拉不会静默漏项。
				"t2i_backends": service.T2IBackends,
				// 可选的出图权重清单（来自 models.json 的各 bundle），
				// 节点里可以逐个覆盖，不必改全局配置
				"checkpoints":  checkpointsFromBundles(bundleStore),
				"bundles_path": bundleStore.Path(),
			})
		})

		// 模型 bundle：逻辑角色 → 权重文件 + 参数。
		// 只读视图（GET）与就地修改（PUT）分开，改完必须重新预检 object_info。
		api.GET("/models/bundles", func(c *gin.Context) {
			roles := bundleStore.Roles()
			out := make(map[string]interface{}, len(roles))
			for _, r := range roles {
				out[r] = bundleStore.Resolve(r)
			}
			resp := gin.H{"path": bundleStore.Path(), "roles": out}
			if err := bundleStore.LoadErr(); err != nil {
				resp["load_error"] = err.Error()
			}
			c.JSON(http.StatusOK, resp)
		})
		api.PUT("/models/bundles", func(c *gin.Context) {
			var req struct {
				Role   string             `json:"role"`
				Files  map[string]string  `json:"files"`
				Params map[string]float64 `json:"params"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON：" + err.Error()})
				return
			}
			if strings.TrimSpace(req.Role) == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 role（如 video.h3.fl2va）"})
				return
			}
			if len(req.Files) == 0 && len(req.Params) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "files 与 params 至少给一个"})
				return
			}
			bundleStore.Merge(req.Role, req.Files, req.Params)
			if err := bundleStore.Save(); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "保存模型配置失败：" + err.Error()})
				return
			}
			// 权重可能换了，旧 object_info 缓存立刻作废
			service.InvalidateObjectInfoCache()
			c.JSON(http.StatusOK, gin.H{"ok": true, "role": req.Role, "resolved": bundleStore.Resolve(req.Role)})
		})
		api.POST("/models/bundles/reload", func(c *gin.Context) {
			err := bundleStore.Reload()
			service.InvalidateObjectInfoCache()
			resp := gin.H{"ok": err == nil, "path": bundleStore.Path()}
			if err != nil {
				resp["error"] = err.Error()
			}
			c.JSON(http.StatusOK, resp)
		})

		// 多实例：模型 → ComfyUI 实例路由（data/instances.json）
		api.GET("/instances", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"path":      instRegistry.Path(),
				"default":   instRegistry.DefaultInstance().Name,
				"instances": instRegistry.All(),
				"note":      instRegistry.LoadNote(),
			})
		})
		api.PUT("/instances", func(c *gin.Context) {
			var req struct {
				Default   string             `json:"default"`
				Instances []service.Instance `json:"instances"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON：" + err.Error()})
				return
			}
			if len(req.Instances) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "instances 不能为空"})
				return
			}
			if err := instRegistry.Replace(req.Instances); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if strings.TrimSpace(req.Default) != "" {
				instRegistry.SetDefault(req.Default)
			}
			if err := instRegistry.Save(); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "保存实例配置失败：" + err.Error()})
				return
			}
			// 实例地址可能变了，object_info 缓存必须作废
			service.InvalidateObjectInfoCache()
			c.JSON(http.StatusOK, gin.H{"ok": true, "instances": instRegistry.All()})
		})

		// 依赖进程托管（ComfyUI / ASR 一键启停）
		depHandler := handler.NewDepHandler(depSupervisor)
		api.GET("/deps", depHandler.List)
		api.POST("/deps/:name/start", depHandler.Start)
		api.POST("/deps/:name/stop", depHandler.Stop)
		api.GET("/deps/:name/wait", depHandler.Wait)

		// 提交前估算：按实测档案预测峰值显存与墙钟（区间 + confidence，不是承诺）
		estHandler := handler.NewEstimateHandler(db, profileStore, 24576)
		api.POST("/estimate", estHandler.Estimate)
		api.GET("/vram/profiles", estHandler.ListProfiles)

		// 对外接入配置生成（MCP 客户端配置 / OpenAI Base URL）：
		// exe 绝对路径与端口只有引擎自己知道，让用户手拼必然填错
		integHandler := handler.NewIntegrationHandler(baseDir, cfg.Port)
		api.GET("/mcp/config", integHandler.MCPConfig)

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
		healthHandler := handler.NewHealthHandler(gpuSched, baseDir, cfg.OllamaBaseURL, comfyURL, h3URL, asrURL, ttsURL, version)
		healthHandler.SetNodeTypeLister(engine)
		api.GET("/health", healthHandler.Health)
		api.GET("/capabilities", healthHandler.Capabilities)
	}

	// ---------- OpenAI 兼容出口 ----------
	//
	// 让 FlowEdge 直接出现在「模型服务器」位置：Open WebUI / SillyTavern /
	// Chatbox / LangChain 把 Base URL 指向 http://127.0.0.1:8080/v1 即可
	// 用本地 GPU 对话、出图、转写、配音，客户端零改动。
	//
	// 与 /api 的关系：/api 是「编排」接口（工作流、运行、血缘），
	// /v1 是「能力」接口（单一模型调用）。两者共用同一批底层服务。
	oaiHandler := handler.NewOpenAICompatHandler(t2iService, asrService, ttsService,
		llmService, llmStreamService, filepath.Join(baseDir, "data", "generated"))
	v1 := r.Group("/v1")
	{
		v1.GET("/models", func(c *gin.Context) {
			// 客户端启动时会探测模型列表；返回本地真实可用的模型，
			// 而不是编一个 "gpt-4"。谎报模型名会让用户以为换模型生效了。
			data := []gin.H{{"id": llmModel, "object": "model", "owned_by": "flowedge"}}
			if t2iService.Checkpoint != "" {
				data = append(data, gin.H{"id": t2iService.Checkpoint, "object": "model", "owned_by": "flowedge"})
			}
			c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
		})
		v1.POST("/images/generations", oaiHandler.ImagesGenerations)
		v1.POST("/audio/transcriptions", oaiHandler.AudioTranscriptions)
		v1.POST("/audio/speech", oaiHandler.AudioSpeech)
		v1.POST("/chat/completions", oaiHandler.ChatCompletions)
	}

	// 前端入口：外部目录优先（便于替换），否则用 exe 内嵌产物
	mountStatic(r)

	// 启动后打印一次环境自检（用户看控制台就知道还缺什么）
	go func() {
		time.Sleep(200 * time.Millisecond)
		caps := service.ProbeCapabilities(gpuSched, baseDir, cfg.OllamaBaseURL, comfyURL, h3URL, asrURL, ttsURL)
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
	// 绑定地址：默认 127.0.0.1（仅本机）。FLOWEDGE_HOST=0.0.0.0 时按上面的规则强制要求 API token
	if err := r.Run(bindAddr + ":" + cfg.Port); err != nil {
		if openToLan {
			log.Printf("启动失败：%v\n    如需局域网访问，请检查 FLOWEDGE_HOST=%s 是否为合法地址（0.0.0.0 或本机网卡 IP）", err, bindAddr)
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
// openBrowser 用系统默认浏览器打开 URL。
//
// Windows 上用 rundll32 url.dll,FileProtocolHandler 而不是 explorer：
// `explorer <url>` 在部分精简系统或默认浏览器为 Edge 的机器上会打开「资源管理器」
// 窗口而不是浏览器（用户看到的就是「双击了但什么都没打开」）。
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
	// 显式丢弃输出：子进程若继承当前控制台句柄，关闭窗口时可能连带失败
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
	fmt.Println("这些是可选能力（语音转写/合成），不装也不影响其他功能。启用步骤：")
	fmt.Println("  1) 安装 Python 3.10+（python.org，安装时勾选 Add to PATH）")
	fmt.Printf("  2) cd \"%s\"\n", outDir)
	fmt.Println("  3) python -m venv .venv && .venv\\Scripts\\pip install -r requirements.txt")
	fmt.Println("  4) .venv\\Scripts\\python tts_server.py    （语音合成，端口 8102）")
	fmt.Println("  5) .venv\\Scripts\\python asr_server.py    （语音转写，端口 8101，首次会下载模型）")
	fmt.Println("  然后重启 FlowEdge.exe，界面右上角「环境」会显示这两项已就绪。")
}

// parseLimit 解析查询串里的条数上限
func parseLimit(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 && v <= 200 {
		return v
	}
	return def
}

// checkpointsFromBundles 把 models.json 里各 bundle 的 ckpt 去重成一份可选清单。
// 节点级覆盖只允许在这份清单里选——乱填一个不存在的文件名，
// 报错会发生在 ComfyUI 构图时，用户根本看不懂。
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
