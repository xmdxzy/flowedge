# FlowEdge

面向消费级单 GPU 的私有化多模态 AI 工作流引擎。

## 核心能力

### 已实现

**编排引擎**
- ✅ 可视化拖拽编排（React Flow v12，侧边栏拖拽 + 吸附网格 + 连线校验）
- ✅ 分层并行 DAG 执行引擎（Go），含菱形汇合、条件分支、`join: any`、`continue_on_error`
- ✅ **15 种节点**：开始 / 结束 / LLM / 文生图 / 语音转写 / 语音合成 / 视频合成 / 条件分支 / 循环 / 子工作流 / HTTP / 数据转换 / 知识库检索 / 代码执行 / **变量聚合**
- ✅ **单步调试 + 断点**：节点上右键打断点，运行到断点停住，可「单步 / 继续 / 停止」，进度实时落库（`status=paused` + `paused_at`）
- ✅ **凭据加密存储**：API Key 用 AES-256-GCM 加密落 `data/credentials.json`，HTTP 节点只写凭据名，导出工作流不再泄密
- ✅ **产物血缘**：每张图 / 每段音 / 每个视频都可溯源到「哪个工作流、哪个运行、哪个节点、上游用了什么」
- ✅ **依赖进程托管**：ComfyUI / ASR 在「环境」面板一键启停，无需另开终端
- ✅ **知识库支持 PDF / DOCX / CSV**（PDF 优先走 poppler，找不到时内置流解析器，乱码会显式报错）
- ✅ **TTS 零依赖**：Windows 内置 SAPI 直出 wav（无需 Python 服务）；配置 `TTS_BASE_URL` 时优先走服务
- ✅ **画布体验**：分层自动布局、分组框、批注、深色 / 浅色主题
- ✅ **视觉理解节点（VLM）**：图片 + 提问 → 中文描述，本地 qwen2.5vl:7b，图片不出本机
- ✅ **模型资产统一管理**：全部权重落在 `D://AI//Models`，`/api/models` 可查家族/体积/具体文件
- ✅ **显存单侧驻留**：扩散 / 语言 / 语音三类任务同一时刻只让一类驻留，换侧自动卸载并等显存真正归还
- ✅ 代码执行节点：yaegi 解释 Go 子集，stdlib 白名单（strings/json/fmt/sort/regexp/time…），危险 import 静态拒绝
- ✅ 图结构校验：环 / 自环 / 悬空边在**保存与执行前**拦截（Kahn 入度法，菱形 DAG 不误判）
- ✅ 节点级超时（默认 180s，`timeout_ms` 覆盖，上限 30min）+ 结构化错误码（timeout / config_missing / cycle / retry_exhausted / cancelled）
- ✅ 节点重试（上限 5）+ 节点级缓存（确定性节点 opt-in）
- ✅ 循环 count / foreach / until，硬上限 100；子工作流嵌套深度 4

**可观测与恢复**
- ✅ 事件日志持久化（`run_events` 表，node_token 不入库避免写放大）→ 可回放
- ✅ 运行 trace：节点耗时 + token 用量（`GET /api/runs/:id/trace`）
- ✅ 单节点重跑 / 从指定节点继续（`rerun-from`，上游输出保持不变）
- ✅ 断点续跑（分支感知，已成功节点复用时间戳不变）
- ✅ 持久化任务队列（`jobs` 表 + 启动识别中断任务）
- ✅ 全局运行列表（分页 + 状态过滤）
- ✅ 执行面板：事件流回放、token 汇总、每节点「重跑」按钮

**检索**
- ✅ 混合检索：BM25（中文 bigram，零词典）+ 向量，RRF 融合
- ✅ MMR 去重（硬切重叠产生的近似块不再挤占结果）
- ✅ 语义阈值过滤；批量写入（导入 O(n²) → O(n)）
- ✅ 结构化引用 `source#chunkN` + 语义相似度，可 `{{rag-1.sources[0].content}}` 精确引用
- ✅ embedding 后端探测缓存；降级时关键词路兜底（不再静默降质）
- ✅ 知识库目录批量导入（递归、自动跳过 node_modules/.git/.venv/data）

**多模态**
- ✅ Ollama 本地 LLM（流式 + 工具调用 + 不支持 tool_calls 模型的文本兜底）
- ✅ ComfyUI 文生图：内置 flux 图 + **自定义工作流模板**（`data/comfy_workflows/*.json`，占位符替换，可接 ControlNet/LoRA/img2img）
- ✅ 视频合成：多图轮播（concat 一次转码）+ 10 分钟超时 + `image_duration`
- ✅ ASR（faster-whisper）/ TTS（Windows SAPI）本地服务；**未配置时显式失败，不伪造结果**
- ✅ `FFMPEG_PATH` 环境变量生效

**GPU 调度（本项目的差异化能力）**
- ✅ 引用计数：正在使用的模型禁止驱逐
- ✅ **真实卸载**：Ollama `keep_alive:0` / ComfyUI `POST /free`（不是只改内存状态）
- ✅ 文生图串行门：并行跑两个 flux 只会互相 OOM
- ✅ 显存时间线（`/api/gpu/timeline`）+ 环境面板可视化
- ✅ 遥测不可用时不阻断执行

**其他**
- ✅ 变量系统 `{{nodeId.path}}`，支持数组索引 `{{node.list[0].field}}`
- ✅ 触发器：Webhook（token 即凭据）+ Cron（五段式，分钟级去重）
- ✅ 工作流版本管理与回滚；导出/导入 JSON
- ✅ 产物画廊；命令面板（Ctrl/Cmd+K）
- ✅ 快捷键：Ctrl+S 保存、Ctrl+Enter 运行、Delete 删除、Ctrl+Z 撤销、Ctrl+Shift+Z / Ctrl+Y 重做
- ✅ 安全：默认只监听 127.0.0.1；对外必须显式 `FLOWEDGE_HOST=0.0.0.0` + `FLOWEDGE_API_TOKEN`
- ✅ 单文件 exe（前端与语音脚本内嵌）；SQLite 零依赖，数据始终跟随 exe 目录
- ✅ 插件注册表（新增节点类型不需改引擎分发逻辑）

## 在 Windows 上运行（推荐：不需要 Docker / WSL）

FlowEdge 的目标是「一台有显卡的 Windows 机器，拿到就双击用」。
整个引擎编译成**单个 exe**：前端界面、数据库、语音服务脚本全部内嵌，
不依赖 Docker、WSL、Node.js 或 Python。

### 构建（只需一次）

```bash
python scripts/build-windows.py
```

产出 `dist/FlowEdge.exe`（约 18 MB）。把它复制到任意目录（例如 `D://FlowEdge//`），
双击 `FlowEdge.exe` 或仓库里的 `FlowEdge.bat`，浏览器自动打开 `http://127.0.0.1:8080`。

### 运行后会生成什么

```
D://FlowEdge//
├── FlowEdge.exe
└── data\              ← 所有数据都在这里；卸载 = 删掉这个目录
    ├── flowedge.db    ← 工作流 / 执行记录 / 版本快照（SQLite，无需安装数据库）
    ├── kb\            ← 知识库向量
    └── generated\     ← 生成的图片与视频
```

exe 会把工作目录切到自己所在位置，所以从桌面快捷方式、计划任务启动都不会把数据写到奇怪的地方。

### 依赖分级（缺了不影响启动）

| 能力 | 需要 | 缺少时的影响 |
|---|---|---|
| LLM 推理 / 知识库向量化 | [Ollama](https://ollama.com) | 只有 LLM 与 RAG 节点不可用 |
| 文生图 | ComfyUI | 只有文生图节点不可用 |
| 视频合成 | ffmpeg | 只有视频节点不可用 |
| 语音转写 / 合成 | Python 3.10+ | 只有这两个语音节点不可用 |

界面右上角 **「环境」** 按钮（或 `GET /api/capabilities`）实时显示每项是否就绪，
缺失项会直接给出安装命令；启动时控制台也会打印一行环境摘要，例如：

```
环境自检  GPU OK | LLM(Ollama) OK | 文生图(ComfyUI) 缺失 | 视频(ffmpeg) OK | 音频(ASR+TTS) 缺失
```

### 可选：启用语音能力

```bat
FlowEdge.exe --export-services
```

把内嵌的 `asr_server.py` / `tts_server.py` / `requirements.txt` 导出到 `local-services\`，
按提示装好 Python 依赖后分别启动即可（TTS 走 Windows 自带语音引擎，不需要下载模型；
ASR 首次会下载 whisper 模型约 500 MB）。

### 常用环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | `8080` | 服务端口 |
| `FLOWEDGE_HOST` | `127.0.0.1` | 监听地址。设 `0.0.0.0` 对外时**必须**同时设 `FLOWEDGE_API_TOKEN`，否则拒绝启动（避免删库接口裸奔） |
| `FLOWEDGE_API_TOKEN` | 空 | 设置后除 `/api/hooks/*`、`/api/health`、`/api/capabilities` 外均需 `Authorization: Bearer <token>` |
| `FLOWEDGE_CORS_ORIGIN` | 空 | 跨域白名单来源。留空时仅放行本机 origin |
| `FLOWEDGE_QUEUE_CONCURRENCY` | `2` | 任务队列并发上限（单 GPU 上并行跑两个重任务只会互相抢显存） |
| `OLLAMA_BASE_URL` | `http://127.0.0.1:11434` | Ollama 地址 |
| `OLLAMA_MODEL` | 自动探测 | 留空时自动使用本地已装的第一个模型 |
| `COMFYUI_BASE_URL` | `http://127.0.0.1:8188` | ComfyUI 地址 |
| `COMFYUI_CHECKPOINT` | `flux1-schnell-fp8.safetensors` | 默认 checkpoint |
| `COMFYUI_TEMPLATE` | 空 | 用 `data/comfy_workflows/<name>.json` 自定义工作流（占位符 `__PROMPT__` / `__WIDTH__` / `__HEIGHT__` / `__SEED__` / `__CHECKPOINT__`） |
| `FFMPEG_PATH` | `ffmpeg` | ffmpeg 可执行文件路径 |
| `ASR_BASE_URL` / `TTS_BASE_URL` | 空 | 指向本地语音服务（导出脚本后自行启动）。**未配置时 ASR/TTS 节点显式失败**，不会伪造结果 |
| `COMFYUI_HOME` | 自动探测 | ComfyUI 安装目录（含 `main.py`）；用于「一键启动」 |
| `COMFYUI_ARGS` | `--listen 127.0.0.1 --port 8188` | 自定义 ComfyUI 启动参数（低显存可加 `--lowvram`） |
| `COMFYUI_AUTOSTART` | `0` | 设为 `1` 时引擎启动即拉起 ComfyUI |
| `ASR_HOME` | `./local-services` | ASR 服务脚本目录（含 `asr_server.py`） |
| `ASR_AUTOSTART` | `0` | 设为 `1` 时引擎启动即拉起 ASR |
| `FLOWEDGE_MODELS_DIR` | `D:/AI/Models` | 统一模型根目录（`/api/models` 与面板都读它） |
| `ASR_DEVICE` | `cpu` | whisper 设备；设为 `cuda` 更快但吃 3GB 显存 |
| `TTS_DEVICE` | `cpu` | Kokoro 设备 |
| `KOKORO_DIR` | `D:/AI/Models/tts` | Kokoro 权重与 HF 缓存 |
| `FLOWEDGE_MASTER_KEY` | 空 | 凭据主密钥（hex/base64/任意口令），留空则自动生成 `data/.master_key` |
| `FLOWEDGE_TTS_SAPI` | `1` | 设为 `0` 禁用 Windows 内置语音合成兜底 |
| `FLOWEDGE_OPEN_BROWSER` | `0` | 设为 `1` 启动后自动打开浏览器 |
| `STATIC_DIR` | 空 | 指定外部前端目录（想替换界面时用） |
| `DB_DRIVER` | `sqlite` | 设为 `postgres` 且配置 `DB_*` 可切换 PostgreSQL |

### Docker 只在服务器场景需要

仓库里的 `docker-compose.yml` 保留给 Linux / 服务器部署，**Windows 桌面环境不需要它**。
配置校验（无需 Docker 运行）：

```bash
python scripts/verify-docker.py
```

## 从源码开发

> 上面的单文件部署已经覆盖绝大多数使用场景；下面是改代码时才需要的。

## 技术栈

| 层 | 技术 |
|---|---|
| 前端 | React 18 + TypeScript + @xyflow/react + Tailwind CSS |
| 后端 | Go 1.25 + Gin + GORM + Gorilla WebSocket（编译为单文件，前端资源内嵌） |
| 模型 | Ollama（LLM）+ ComfyUI（文生图）+ Whisper（ASR）+ TTS |
| 视频 | FFmpeg（NVENC 硬件编解码） |
| 数据库 | **SQLite（默认，零依赖开箱即用）**；可通过 `DB_DRIVER=postgres` 切换 PostgreSQL |

## 快速启动

### 前置要求
- Go 1.25+
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

## 本地模型栈（2026-10 实测）

所有权重集中在 **`D://AI//Models`**（可用 `FLOWEDGE_MODELS_DIR` 改），项目目录只放代码与配置。

| 家族 | 模型 | 体积 | 用途 | 默认设备 |
|---|---|---|---|---|
| 文生图 | `flux1-schnell-fp8` | 16.1 GB | 出图（英文提示词效果最好） | GPU 17GB |
| 文生图（备用） | `qwen_image_2.1_turbo` + LoRA + VAE | 8.2 GB | 中文提示词友好（当前 ComfyUI 版本 VAE 识别不匹配，待升级） | GPU |
| 视频+音频 | `minimax_h3_fl2va / ref2va` + 视频/音频 VAE | 47 GB | 联合音视频生成（需装 ComfyUI-MiniMax-H3-Turbo 节点） | GPU |
| 文本编码 | `qwen3vl_8b_int8` / `qwen3vl_32b_nvfp4` | 23.3 GB | Qwen-Image / H3 的文本与视觉编码 | GPU |
| 大语言模型 | `qwen2.5:14b`（Ollama） | 9.0 GB | 中文推理 / 改写 / 翻译 | GPU 9GB |
| 视觉理解 | `qwen2.5vl:7b`（Ollama） | 6.0 GB | 看图回答（vision 节点） | GPU 6GB |
| 插帧 | ffmpeg minterpolate | 0 | 视频帧率翻倍，CPU | CPU |
| 语音识别 | `faster-whisper-large-v3` | 2.9 GB | 转写（默认 CPU int8，`ASR_DEVICE=cuda` 可上卡） | CPU |
| 语音合成 | `Kokoro-82M` | 0.3 GB | 中英日神经 TTS（默认 CPU） | CPU |
| 超分 | `RealESRGAN_x4plus` | 0.06 GB | 放大 | — |

### 显存策略（24GB 卡 + UE5 同时开着的现实约束）

```
扩散侧 flux  ≈17GB   ←→   语言侧 14B ≈9GB / VLM ≈6GB   ←→   语音 whisper≈3GB
```

三类同时驻留必然爆显存，所以引擎内置了**显存仲裁**：

1. 进入任一侧前先让其它侧卸载（Ollama `keep_alive=0` / ComfyUI `/free` + `free_memory`）
2. 卸载后轮询 `vram_free`，等显存真正回到 16GB 以上才继续（CUDA 释放是异步的）
3. 同类任务连续执行不重复卸载（避免每次 40 秒冷加载）
4. ComfyUI **常驻权重**（`--reserve-vram 2`，不加 `--disable-smart-memory`）
   同一批出图不重复装载；换到语言侧时由引擎调 `/free` 并等 `vram_free` 真回来
5. 语音服务默认跑 CPU：Kokoro 82M / whisper int8 足够快，把显存全部留给图像与 LLM
6. 视频平滑用 **ffmpeg minterpolate**（CPU），不用 RIFE：权重镜像 404 且要 cupy，
   实测 10.4 秒视频插帧翻倍只要 36 秒 CPU、显存零占用

> **实测对比（同一台 3090，5 条不同提示词 @768²）**：
> 每张卸载重装 = **199 秒**；常驻权重 = **29 秒（6.9 倍）**。
> 所以默认改成常驻；切到 LLM/VLM 时靠 `/free` 回收，最多等 12 秒，值得。

实测（3090 24GB，同时开着 UE5 编辑器）：文生图 + 视觉理解串起来跑，**峰值 15.0GB**，
任务结束后回落到 1.6GB；改造前同样流程峰值 24.0GB（贴着上限）。

## 节点类型

| 节点 | 类型 | 能力 |
|---|---|---|
| 开始 | control | 输入初始提示词 |
| 大语言模型 | ai | 本地 LLM 推理（流式打字机 + 工具调用） |
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
| 变量聚合 | data | 把多路分支收拢成对象 / 数组 / 拼接文本（替代手写 code 节点） |
| 图生图 | ai | 以输入图为底按强度重绘（flux，实测 0.85 明显改风格） |
| 局部重绘 | ai | 蒙版区域按提示词重画（蒙版须与原图同尺寸，白色为重绘区） |
| 超分放大 | ai | RealESRGAN 等放大模型（768→3072 实测） |
| 文生视频 | ai | 双后端：Wan2.1-1.3B（64 秒/2 秒片，无声）/ **MiniMax-H3**（~2 分钟/5 秒片，**自带立体声**） |
| 图生视频 | ai | H3 首帧引导（`MiniMaxH3AddGuide`）：给一张图 → 5 秒带声动态镜头 |
| 分镜规划 | ai | 剧本 → 结构化分镜表（景别/角度/轴线/运镜/动作/声音/台词/衔接），时长按内容给 |
| 分镜执行器 | ai | 逐镜生成 + ASR 音频抽检 + 按 cut/continue/dissolve 装配 + 响度归一 |
| 视频插帧 | ai | 出片后帧率翻倍（Wan 16→32 / H3 24→48），ffmpeg minterpolate，**CPU 完成、不占显存** |
| 视觉理解 | ai | 图片 + 提问 → 文字（本地 VLM，走 Ollama /api/chat） |
| 代码执行 | data | yaegi 跑 Go 子集，stdlib 白名单 |
| 结束 | control | 输出最终结果 |

> 另有仅存在于画布的 **批注（note）** 与 **分组（group）**：保存与运行时会自动剔除，不会进工作流定义。

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

### 触发器
- `GET /api/workflows/:id/trigger` - 读取触发器配置（token / cron / 上次触发）
- `PUT /api/workflows/:id/trigger` - 设置 cron 与启用状态
- `POST /api/workflows/:id/trigger/rotate` - 重新生成 token（旧链接失效）
- `POST /api/hooks/:token` - **公开**触发端点：外部系统 POST 即执行

### 运行控制与产物
- `POST /api/runs/:runId/cancel` - 中止进行中的运行
- `GET /api/artifacts` - 列出生成的图片 / 视频 / 音频
- `GET /api/artifacts/:name` - 获取单个产物
- `DELETE /api/artifacts/:name` - 删除产物
- `GET /api/capabilities` - 部署环境自检（GPU / Ollama / ComfyUI / ffmpeg / ASR / TTS）
- `GET /api/health` - 轻量探活

### 变量语法

节点数据里任何字符串字段都可以引用其他节点的输出：

```
{{llm-1.text}}          上游 LLM 的文本输出
{{http-2.status_code}}  HTTP 节点的状态码
{{loop-3.iterations}}   循环执行了几轮
{{loop-3.stopped_by}}   循环的停止原因
```

- 只替换「已完成节点」的引用，未完成的保留原文（便于排障）
- `{{input}}` 属于 `data_transform` 的模板语法，不会被变量解析动过

### 版本
- `GET /api/workflows/:id/versions` - 列出所有版本
- `POST /api/workflows/:id/versions` - 创建新版本快照
- `POST /api/workflows/:id/versions/:versionId/rollback` - 回滚到指定版本

### 调试 / 凭据 / 依赖 / 血缘
- `POST /api/runs/:runId/step` - 单步执行一个节点后暂停
- `POST /api/runs/:runId/continue` - 跑到下一个断点或结束
- `GET|POST /api/credentials` · `DELETE /api/credentials/:name` - 凭据（列表脱敏，值不可回读）
- `GET /api/deps` · `POST /api/deps/:name/start|stop` · `GET /api/deps/:name/wait` - 依赖进程托管
- `GET /api/artifacts/:name/lineage` · `GET /api/lineage` - 产物血缘

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
│   │   ├── nodes/       # 12 种自定义节点组件
│   │   ├── templates/    # 预置工作流模板
│   │   ├── App.tsx      # 主画布
│   │   └── main.tsx     # 入口
│   └── package.json
├── .env.example         # 环境变量示例
└── README.md
```

## 已知限制（诚实披露）

- **图生图对强度敏感**：flux-schnell 是蒸馏 t2i 模型，0.55 只有轻微润色，0.8 以上才明显改风格。
- **重绘质量有限**：flux 并非专门的重绘模型，蒙版内可能只做弱修补。要专业重绘需下 Flux-Fill（~11GB），
  权衡后暂缓：功能可用，下载与图谱改动留待真有高频重绘需求再做。
- **~~minimax_h3 不可用~~ → 已可用**：H3 必须跑在独立工作树 `D://AI//ComfyUI-H3`（自带 venv，torch 2.14 + 完整
  int8/nvfp4 量化后端），启动参数 `--reserve-vram 4 --cache-none`，**不要叠加** `--disable-smart-memory`。
  实测 608×352×124 帧 20 步：**约 2 分钟出片，峰值 21.8GB**，自带 32kHz 立体声。同机默认实例（torch 2.6）跑同一个图会 OOM，
  这是两套环境差异，不是显存不够。
- **qwen_image_2.1 暂不可用**：当前 ComfyUI 版本把它的 VAE 误判成 WanVAE（12 通道 vs 4 通道），
  出图会报 size mismatch；权重已就位，等 ComfyUI 升级或补相应节点即可切换。
- **flux 对中文提示词效果弱**：`flux1-schnell` 的文本编码器以英文为主，纯中文提示词经常只画出通用风景。
  建议在工作流里先接一个 LLM 节点把中文改写/翻译成英文，再喂给文生图。
- **ASR 精度取决于模型档位**：默认 `small`（CPU int8），中文专有名词可能听错（实测「闭环」被识别成「必还」）。
  要更准可设 `WHISPER_MODEL=medium`，代价是更慢、更吃内存。
- **重排 / VLM / 多租户** 仍按 SOTA 对照表放弃（原因见 `SOTA-达成度对照表.md`）。
- **Electron 壳（`desktop/`）目前只是可选打包路径**，日常客户端走 Edge/Chrome 的 `--app` 模式，
  两者不要同时维护界面逻辑。

## 路线图

- [x] MVP：基础画布 + 串行执行 + 本地 LLM
- [x] 显存调度器（LRU 缓存 + 水位线）
- [x] 多模态节点（文生图、ASR、TTS、视频）
- [x] 条件分支节点
- [x] 图结构校验（环 / 自环 / 悬空边拦截）+ 节点级超时 + 结构化错误码
- [x] 流式输出
- [x] 版本管理
- [x] WebSocket 实时推送
- [x] 执行历史面板
- [x] 节点拖拽 + 右键菜单
- [x] 工作流导出/导入
- [x] 单步调试 + 断点
- [x] 单文件 Windows 部署（前端内嵌，无需 Docker/WSL/Node）
- [ ] Docker 一键部署（服务器场景）
- [x] 节点插件系统
- [x] 子工作流嵌套
