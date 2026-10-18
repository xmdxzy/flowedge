# FlowEdge

面向消费级单 GPU 的私有化多模态 AI 工作流引擎。

## 核心能力

### 已实现
- ✅ 可视化拖拽编排（React Flow v12，支持侧边栏拖拽到画布）
- ✅ DAG 拓扑排序执行引擎（Go，已含条件分支与菱形 DAG 单元测试）
- ✅ 13 种节点类型：开始 / 结束 / LLM / 文生图 / 语音转写 / 语音合成 / 视频合成 / 条件分支 / **循环** / 子工作流 / HTTP 请求 / 数据转换 / 知识库检索
- ✅ Ollama 本地 LLM 集成（OpenAI 兼容接口 + 流式输出）
- ✅ SQLite 零依赖持久化（工作流、执行记录、版本快照），可选 PostgreSQL
- ✅ GPU 显存调度器（LRU 缓存 + 三级水位线 70%/85%/92%）
- ✅ POPS 流水线重叠预加载调度算法
- ✅ WebSocket 实时状态推送（节点状态实时更新）
- ✅ 工作流版本管理与回滚
- ✅ 预置模板库（文本总结、文生图、条件分支）
- ✅ 快捷键支持（Cmd+Enter 运行、Delete 删除、Cmd+Z 撤销）
- ✅ 右键菜单（节点复制/删除、画布快捷操作）
- ✅ 工作流导出/导入（JSON 文件）
- ✅ 执行历史面板
- ✅ 深色科技风 UI（三栏布局、节点状态指示灯、连线流动画）

## 技术栈

| 层 | 技术 |
|---|---|
| 前端 | React 18 + TypeScript + @xyflow/react + Tailwind CSS |
| 后端 | Go 1.22 + Gin + GORM + Gorilla WebSocket |
| 模型 | Ollama（LLM）+ ComfyUI（文生图）+ Whisper（ASR）+ TTS |
| 视频 | FFmpeg（NVENC 硬件编解码） |
| 数据库 | **SQLite（默认，零依赖开箱即用）**；可通过 `DB_DRIVER=postgres` 切换 PostgreSQL |

## 快速启动

### 前置要求
- Go 1.22+
- Node.js 18+
- NVIDIA GPU（目标硬件 RTX 3090 24GB，已验证）
- Ollama / ComfyUI / FFmpeg 按需（不装也能启动，仅对应节点会报错）

> 国内网络：Go 模块设置 `go env -w GOPROXY=https://goproxy.cn,direct`；npm 已默认淘宝镜像。

### 1. 启动后端（默认自动创建本地 SQLite，无需任何外部服务）
```bash
cd backend
go mod tidy
go run main.go
```
后端运行在 `http://localhost:8080`，数据库文件位于 `backend/data/flowedge.db`。

切换 PostgreSQL：设置环境变量 `DB_DRIVER=postgres` 及 `DB_HOST/DB_PORT/DB_USER/DB_PASSWORD/DB_NAME`。

### 2. 启动前端
```bash
cd frontend
npm install
npm run dev
```
前端运行在 `http://localhost:3000`，已配置 `/api` 代理到后端 8080。

### 3. 构建与测试
```bash
# 后端编译 + 静态检查 + 单元测试
cd backend && go build ./... && go vet ./... && go test ./...

# 前端生产构建
cd frontend && npm run build
```

### 4. 拉取本地模型
```bash
ollama pull qwen2.5:7b
```

## 架构设计

### 分层架构
```
┌─────────────────────────────────────┐
│ 前端交互层                           │
│  React + React Flow + Tailwind      │
├─────────────────────────────────────┤
│ 引擎控制层                           │
│  Go + Gin + DAG引擎 + 事件总线       │
├─────────────────────────────────────┤
│ 智能调度层                           │
│  GPU调度器 + POPS预加载算法          │
├─────────────────────────────────────┤
│ 推理执行层                           │
│  Ollama + ComfyUI + Whisper + FFmpeg │
├─────────────────────────────────────┤
│ 基础设施层                           │
│  PostgreSQL + MinIO + Redis        │
└─────────────────────────────────────┘
```

### 核心调度算法：POPS
**Pipeline Overlap Preloading Scheduling**

通过历史执行时长统计，预测当前节点剩余执行时间，在合适的时机预加载下一节点的模型，隐藏模型加载延迟，提升整体执行效率。

- 自适应触发：基于指数移动平均的执行时长估计
- 重叠计算：模型加载与节点执行并行进行
- 智能延迟：计算最佳预加载开始时刻

### GPU 显存调度
- **安全线（70%）**：正常缓存模型
- **预警线（85%）**：开始 LRU 卸载
- **红线（92%）**：强制卸载非活跃模型
- **LRU 策略**：按最近最少使用淘汰，避免频繁换入换出

## 节点类型

| 节点 | 类型 | 能力 |
|---|---|---|
| 开始 | control | 输入初始提示词 |
| 大语言模型 | ai | 调用本地 LLM 推理（支持流式） |
| 文生图 | ai | ComfyUI 生图（SDXL） |
| 语音转写 | multimodal | Whisper ASR |
| 语音合成 | multimodal | TTS 文字转语音 |
| 视频合成 | multimodal | FFmpeg 合成视频 |
| 条件分支 | control | IF/ELSE 条件判断 |
| 循环 | control | 迭代执行子工作流：固定次数 / 遍历列表 / 条件循环 |
| 子工作流 | control | 引用并内联执行另一个工作流（嵌套上限 4 层） |
| HTTP 请求 | data | 调用外部接口 |
| 数据转换 | data | 模板渲染（`{{input}}` / `{{a.b}}`） |
| 知识库检索 | ai | 本地 RAG 语义检索 |
| 结束 | control | 输出最终结果 |

## API 接口

### 工作流
- `GET /api/workflows` - 列出所有工作流
- `GET /api/workflows/:id` - 获取单个工作流
- `POST /api/workflows` - 创建工作流
- `PUT /api/workflows/:id` - 更新工作流
- `DELETE /api/workflows/:id` - 删除工作流

### 执行
- `POST /api/workflows/:id/run` - 执行工作流
- `GET /api/workflows/:id/runs` - 列出执行历史
- `GET /api/runs/:runId` - 获取执行状态

### 版本
- `GET /api/workflows/:id/versions` - 列出所有版本
- `POST /api/workflows/:id/versions` - 创建新版本快照
- `POST /api/workflows/:id/versions/:versionId/rollback` - 回滚到指定版本

### 系统
- `GET /api/gpu/status` - GPU 显存调度状态
- `GET /api/ws` - WebSocket 实时事件推送

## 项目结构

```
flowedge/
├── backend/
│   ├── config/          # 配置加载
│   ├── handler/         # HTTP 接口
│   │   ├── workflow.go     # 工作流 CRUD + 执行
│   │   ├── version.go      # 版本管理
│   │   └── ws.go           # WebSocket 推送
│   ├── model/           # 数据模型
│   │   ├── workflow.go     # 工作流 + 执行记录
│   │   └── version.go      # 版本快照
│   └── service/
│       ├── engine.go        # 基础 DAG 引擎
│       ├── engine_v2.go     # 增强版引擎（多模态+分支）
│       ├── llm.go           # LLM 服务
│       ├── llm_stream.go    # 流式 LLM 服务
│       ├── multimodal.go     # 多模态服务
│       ├── gpu_scheduler.go  # GPU 显存调度器
│       ├── pops_scheduler.go # POPS 预加载算法
│       └── ws_hub.go        # 事件广播中心
├── frontend/
│   ├── src/
│   │   ├── components/    # UI 组件
│   │   │   ├── ContextMenu.tsx    # 右键菜单
│   │   │   └── ExecutionPanel.tsx  # 执行历史面板
│   │   ├── hooks/        # 自定义 hooks
│   │   │   ├── useHotkeys.ts      # 快捷键
│   │   │   └── useWebSocket.ts   # WebSocket 客户端
│   │   ├── nodes/       # 8 种自定义节点组件
│   │   ├── templates/    # 预置工作流模板
│   │   ├── App.tsx      # 主画布
│   │   └── main.tsx     # 入口
│   └── package.json
├── .env.example         # 环境变量示例
└── README.md
```

## 路线图

- [x] MVP：基础画布 + 串行执行 + 本地 LLM
- [x] 显存调度器（LRU 缓存 + 水位线）
- [x] 多模态节点（文生图、ASR、TTS、视频）
- [x] 条件分支节点
- [x] POPS 预加载算法
- [x] 流式输出
- [x] 版本管理
- [x] WebSocket 实时推送
- [x] 执行历史面板
- [x] 节点拖拽 + 右键菜单
- [x] 工作流导出/导入
- [ ] 单步调试 + 断点
- [ ] Docker 一键部署
- [ ] 节点插件系统
- [x] 子工作流嵌套
