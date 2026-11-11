# OpenMontage 集成指南（FlowEdge FE 工作流适配）

> 把 OpenMontage Drama Studio（独立部署的 AI 短剧/短视频 Web 工坊）接入 FlowEdge FE 工作流，
> 以后从 FlowEdge 界面直接发起 OpenMontage 的七阶段出片：剧本 → 角色 → 场景 → 道具 → 分镜 → 配音 → 剪辑。

## 一、架构概览

```
FlowEdge (本地出片引擎, :8080)  —— 代理转发 ——  OpenMontage Drama Studio (:8787)
                                              │
                          ┌───────────────────┼───────────────────┐
                          ▼                   ▼                   ▼
                       PostgreSQL         Redis                MinIO/S3
                       (项目/元数据)       (媒体任务队列)       (媒体 Blob 存储)
```

- **FlowEdge 负责**：作为统一入口与 FE 界面，通过 `/api/openmontage/*` 代理把请求转发给 OpenMontage，返回结果原样透传。
- **OpenMontage 负责**：7 阶段短剧/短视频工作流（剧本生成、角色/场景/道具、分镜、Seedream 出图、Seedance 出视频、edge-tts 配音、剪辑导出）。
- **两者关系**：并行共存，互不抢显存。OpenMontage 用云 API（Seedream/Seedance）或自建 ComfyUI；FlowEdge 用本地 H3/ComfyUI。

## 二、OpenMontage 部署（必做，FlowEdge 无法替代）

OpenMontage 需要 PostgreSQL + Redis + S3 兼容存储，推荐用 Docker Compose 一键启动：

```bash
# 1. 克隆仓库
git clone --depth 1 https://github.com/charliewwdev/OpenMontage.git
cd OpenMontage

# 2. 配置环境变量
cp studio/.env.example .env
# 编辑 .env，至少填写：
#   STUDIO_PUBLIC_URL=http://127.0.0.1:8787
#   DEEPSEEK_API_KEY=sk-...          # 文本 LLM（剧本/分镜用）
#   VOLCENGINE_API_KEY=...            # Seedream 出图 / Seedance 出视频（付费）
#   或 COMFYUI_SERVER_URL=...         # 自建 ComfyUI 出图（免 API Key）

# 3. 启动（需要 Docker；本机没有 Docker 时需先安装 Docker Desktop）
docker compose -f docker-compose.studio.yml up --build

# 4. 验证
curl http://127.0.0.1:8787/api/health   # 应返回 200
# 打开 http://127.0.0.1:8787 → 点 Demo 进入工作室
```

关键环境变量（`.env`）：

| 变量 | 说明 | 必填 |
|---|---|---|
| `STUDIO_PUBLIC_URL` | 本机访问地址 | 是 |
| `DEEPSEEK_API_KEY` / `OPENAI_API_KEY` 等 | 文本模型 Key，用于剧本/分镜生成 | 是（否则卡在剧本阶段） |
| `VOLCENGINE_API_KEY` / `ARK_API_KEY` | Seedream 出图 + Seedance 出视频 | 否（可改用 ComfyUI） |
| `COMFYUI_SERVER_URL` | 自建 ComfyUI 地址 | 否（与 Seedream 二选一） |
| `MINIO_ENDPOINT` / `MINIO_*` | 对象存储，Compose 已内置 drama-minio | 否 |

更多细节见 OpenMontage 仓库：`docs/SELF_HOSTING.md`、`studio/.env.example`。

> 本环境未安装 Docker，无法在此处跑通 OpenMontage 全套；上述步骤为部署脚本，部署完成后即可与 FlowEdge 对接。

## 三、FlowEdge 侧集成（已落地代码）

### 3.1 后端代理（已新增）

文件：`backend/handler/openmontage.go`

新增 `OpenMontageProxy`，把 FlowEdge 的 `/api/openmontage/*` 请求转发到 OpenMontage：

| FlowEdge 路由 | OpenMontage 目标 | 说明 |
|---|---|---|
| `GET /api/openmontage/health` | `GET {base}/api/health` | 探活 |
| `GET /api/openmontage/projects` | `GET {base}/api/projects` | 列项目 |
| `POST /api/openmontage/projects` | `POST {base}/api/projects` | 新建项目 |
| `GET /api/openmontage/projects/:id` | `GET {base}/api/projects/:id` | 项目详情 |
| `POST /api/openmontage/projects/:id/script` | `POST {base}/api/projects/{id}/script` | 生成剧本 |
| `POST /api/openmontage/projects/:id/storyboard` | `POST {base}/api/projects/{id}/storyboard` | 生成分镜 |
| `POST /api/openmontage/projects/:id/generate` | `POST {base}/api/projects/{id}/generate` | 生成视频 |
| `POST /api/openmontage/projects/:id/audio` | `POST {base}/api/projects/{id}/audio` | 生成配音 |
| `POST /api/openmontage/projects/:id/edits` | `POST {base}/api/projects/{id}/edits` | 保存剪辑 |
| `POST /api/openmontage/auto-produce` | `POST {base}/api/auto-produce` | 一键出片 |

默认 `baseURL = http://127.0.0.1:8787`，可通过环境变量 `OPENMONTAGE_BASE_URL` 覆盖。

### 3.2 路由注册（已在 main.go）

在 `/api` 分组下注册：

```go
omBaseURL := getEnv("OPENMONTAGE_BASE_URL", "")
RegisterOpenMontageRoutes(api, omBaseURL)
```

### 3.3 前端页面（已新增）

- `frontend/src/pages/OpenMontagePage.tsx` —— OpenMontage 工作室页面（项目列表 + 7 阶段按钮 + 一键出片）
- `frontend/src/api/types.ts` —— 新增 `OmProject`、`OmProjectDetail`、`OmAutoProduceBody` 等类型
- `frontend/src/api/client.ts` —— 新增 `fetchOmHealth`、`fetchOmProjects`、`createOmProject`、`generateOmScript`、`generateOmStoryboard`、`generateOmVideo`、`generateOmAudio`、`saveOmEdit`、`autoProduceOm`
- `frontend/src/App.tsx` —— 新增 `openmontage` 标签页（五段导航：出片 / OpenMontage / 画布 / 运行 / 产物）

## 四、如何调用（以后从 FlowEdge 调用 OpenMontage）

### 方式 A：通过 FE 界面（推荐）

1. 启动 FlowEdge 引擎，并设置 `OPENMONTAGE_BASE_URL`（或保持默认 `:8787`）。
2. 启动 OpenMontage（`docker compose -f docker-compose.studio.yml up --build`）。
3. 打开 FlowEdge 界面，点顶部导航的 **OpenMontage** 标签页。
4. 页面自动探活 `/api/openmontage/health`，显示「已连接」后：
   - 点「新建」创建项目（输入项目名称）。
   - 点项目卡片进入详情，依次点击「生成剧本」「生成分镜」「生成视频」「生成配音」「保存剪辑」。
   - 或直接在底部「一键出片」输入一句话灵感，全流程自动完成。

### 方式 B：通过 API 直接调用

```bash
# 1. 探活 OpenMontage 是否可达
curl http://127.0.0.1:8080/api/openmontage/health

# 2. 新建项目
curl -X POST http://127.0.0.1:8080/api/openmontage/projects \
  -H 'Content-Type: application/json' \
  -d '{"name":"我的第一个短剧","genre":"复仇","aspect_ratio":"9:16"}'

# 3. 生成剧本（项目 id 替换为实际返回的 id）
curl -X POST http://127.0.0.1:8080/api/openmontage/projects/1/script \
  -H 'Content-Type: application/json' \
  -d '{"idea":"一个职场反转故事","genre":"复仇","aspect_ratio":"9:16"}'

# 4. 一键出片
curl -X POST http://127.0.0.1:8080/api/openmontage/auto-produce \
  -H 'Content-Type: application/json' \
  -d '{"idea":"10集复仇短剧，9:16，前3秒钩住观众"}'
```

### 方式 C：通过 MCP（给 DSH / Claude Desktop 调用）

FlowEdge 的 MCP 服务端已有 `flowedge_produce` 等工具；OpenMontage 的代理路由走 HTTP，可在 MCP 客户端里通过 `curl` 或 FlowEdge 的 `/api/openmontage/*` 调用。

## 五、验收标准

- [ ] `GET /api/openmontage/health` 返回 200（OpenMontage 可达）
- [ ] FlowEdge 界面顶部出现 **OpenMontage** 标签页，点击可进入
- [ ] 项目列表页能拉取项目（或显示「暂无项目」）
- [ ] 创建项目 → 进入详情 → 各阶段按钮点击后，OpenMontage 侧有对应任务产生（查 OpenMontage 日志）
- [ ] 一键出片提交后返回任务 ID，状态可查
- [ ] OpenMontage 不可达时，FlowEdge 返回 502 并给出可操作修复提示（不静默失败）

## 六、注意事项（避坑）

1. **OpenMontage 需 Docker**。本机无 Docker 时需先安装 Docker Desktop，或自行手动部署 PostgreSQL/Redis/MinIO + FastAPI 后端。
2. **Seedream / Seedance 是付费第三方 API**。生成图片/视频会产生费用；若不想付费，在 `.env` 里配 `COMFYUI_SERVER_URL` 指向自建 ComfyUI。
3. **显存不冲突**。OpenMontage 用云 API 或自己的 ComfyUI 实例；FlowEdge 用本地 H3/ComfyUI（8188/8189），两者不共享显存，可并行。
4. **代理只转发，不缓存**。所有请求直接透传，OpenMontage 侧返回什么，FlowEdge 就返回什么。
5. **环境变量修改后需重启 FlowEdge**。`OPENMONTAGE_BASE_URL` 的改动在引擎重启后生效。

## 七、目录

| 路径 | 说明 |
|---|---|
| `backend/handler/openmontage.go` | 后端代理处理器 |
| `backend/main.go` | 路由注册（已加 `/api/openmontage/*`） |
| `frontend/src/pages/OpenMontagePage.tsx` | FE 工作室页面 |
| `frontend/src/api/types.ts` | OpenMontage 类型定义 |
| `frontend/src/api/client.ts` | OpenMontage API 客户端 |
| `frontend/src/App.tsx` | OpenMontage 标签页导航 |
| `docs/OpenMontage-集成指南.md` | 本文档 |
