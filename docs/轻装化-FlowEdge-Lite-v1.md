# 轻装化：FlowEdge Lite v1（2026-10-24）

> 目标：只服务两个核心能力，其余全部删除；界面保留自由画布。
> 本文是这次重写的唯一事实源：删了什么、留了什么、怎么验证的、还剩什么没做。

## 零、一句话结论

| 维度 | 之前 | 现在 |
|---|---|---|
| 后端 API | 112 条 | **44 条** |
| 节点类型 | 40 种 | **27 种**（媒体 13 + 控制 5 + 语言/门 9） |
| 后端 Go 文件 | 241 | 145 |
| 前端源文件 | 308（38 个面板） | 64（13 个面板） |
| 一次改动 | — | **165 文件 / -34,531 行 / +368 行**（不含新增的质量循环模块） |
| 交付形态 | 引擎 + 16 个功能面 | 极简运行台 + MCP（9 个工具） |

两个核心能力：

1. **本地多模态生成工作流**：文生图 / 图生图 / 局部重绘 / 超分 / 文生视频 / 分镜执行（H3）/ ComfyUI 工作流导入 / ASR / TTS / 合成 / 字幕 / 后期包装 + 质检门 / 一致性门。
2. **质量建议门控 + 循环提升**：`quality_loop` 节点 —— 生成 → 验收打分 → 诊断（哪项不过）→ 修复（改哪个字段改成多少）→ 重跑 → 择优。

## 一、删掉的（连同测试一起删，git 可回溯）

| 子系统 | 具体内容 | 为什么删 |
|---|---|---|
| 自进化 | `evolution.go` / `evidence*` / `promotion` / `variants` / `instrument` / `graph_score` / `regression` / `candidate` + 12 条 API + 面板 | 生产库 `promotions=0`，从未真正用过；它自己的判分又依赖另一套证据口径 |
| 工具链 | `toolchain.go` / `mcp_client` / `mcp_registry` / `plugin_*` / `agent_*` / `tools` / `exec_code` / `exec_command` + 14 条 API + 面板 | DSH 在外层编排，引擎不需要再当工具网关 |
| 能力卡 / 技能包 | `cards.go` / `skillpack.go` / 三段式 pipeline / 徽章 HOC | 卡片是"提示词封装"，节点参数已经够用；三段式让每个节点行为都变得不可预测 |
| KB / RAG | `rag_v3` / `bm25` / `vector_store` / `embedder` / `docparse` + 7 条 API + 面板 | 本地生成不需要检索；留着要养一套 embedding 服务 |
| 触发器 | `cron*` / `trigger*` / webhook + 6 条 API + 面板 | 由 DSH 调度，不需要引擎定时器 |
| 凭据仓库 | `credentials*` + 3 条 API | 端点清单里直接写 Bearer key，少一层加密/解密失败面 |
| 版本管理 | `WorkflowVersion` / 快照 / 回滚 + 4 条 API | 画布保存即版本，git 之外再存一份只是噪音 |
| 通用编排节点 | `agent_task` / `command` / `code` / `http_request` / `rag_query` / `data_transform` / `aggregate` / `sub_workflow` / `mcp_call` | 与"只保留两件事"直接冲突；它们的失败面（沙箱/网络/解释器）都不属于生成 |
| 镜头作业 | `ShotJob` / 单镜重跑 / `reroll` 参数 | 生产库 `shot_jobs=0`；重跑整条已足够（`quality_loop` 就是有界重跑） |
| 节点缓存 | `NodeCache` | 与质量循环冲突：缓存命中会让"重跑"没有真的重新生成 |
| OpenAI 兼容出口 | `/v1/*` + `openai_compat.go` | DSH 直连模型，不需要引擎冒充模型服务器 |
| 产物血缘 | `lineage` + 2 条 API | 报告里已经有逐轮路径，血缘是第二份账 |

## 二、质量循环（新增，核心②）

节点类型 `quality_loop`，实现在 `backend/service/quality_loop.go`。

### 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `workflow_id` | 必填 | 被反复重跑的**生成**工作流 id |
| `max_rounds` | 3 | 硬上限，修不动就停 |
| `target_score` | 100 | 0–100，按 fail 级检查项通过率算 |
| `repair` | `auto` | `auto` 数值项自动改参数 / `resample` 只重采样 / `off` 只跑不修 |
| `target_node` | 自动 | 被修补的节点；留空自动挑 shotlist/text2video/text2image… |
| 门规则 | — | 与 `quality_gate` **同一套键**：`min_frames` / `min_width` / `min_height` / `duration_min` / `require_audio` / `forbid_speech` / `loudness_min_db` / `max_silence_gap` / `av_drift` |

### 每轮做什么

1. 解析内层工作流 → 把上一轮诊断出的补丁写进目标节点 `data` → 执行；
2. 取产物 → `ffprobe` 探测（**探测失败 ≠ 不合格**，直接报错停止）；
3. `runGateChecks` 跑声明式门 → 打分 → 保留每项 `checks` 与修复建议；
4. 达标 → 结束；不达标 → `derivePatches` 把失败项翻译成补丁：
   - `min_frames` → `frames = max(当前, 阈值)`
   - `min_width/height` → 对齐到 16 的倍数
   - `duration_min` → `frames = ceil(秒×24)`
   - `require_audio` → `audio = true`
   - `forbid_speech` → 提示词末尾追加「纯环境音，无人声对白，无旁白」
   - 采样噪声/语义类问题 → 下一轮重采样（不硬编"改提示词"规则）
5. 保留得分最高的一轮；`max_rounds` 用尽仍未达标 → **显式失败**并带完整报告。

### 输出

```json
{
  "gate": "pass", "score": 100, "target_score": 100,
  "rounds": 2, "best_round": 2, "path": "…/out.mp4",
  "checks": [...], "report": [
    {"round": 1, "score": 33, "checks": [...], "fixes": [...], "patches": [...]},
    {"round": 2, "score": 100, "checks": [...], "patches": [...]}
  ],
  "text": "质量循环 2 轮（R1=33 R2=100），取第 2 轮…"
}
```

## 三、保留的接口面

### API（44 条，含 WebSocket）

```
GET  /api/health | /api/capabilities | /api/modules | /api/models | /api/models/bundles
GET  /api/deps | POST /api/deps/reload | POST /api/deps/:name/start|stop | GET /api/deps/:name/wait
GET  /api/instances | PUT /api/instances
GET  /api/llm/profiles | PUT /api/llm/profiles
GET/POST/PUT/DELETE /api/workflows[/:id]
POST /api/preflight | GET /api/workflows/:id/spec | POST /api/workflows/:id/spec/apply
POST /api/workflows/:id/run | GET /api/workflows/:id/runs
GET  /api/runs | GET /api/runs/:runId | POST /api/runs/:runId/resume|rerun-from|cancel
POST /api/runs/:runId/step|continue | GET /api/runs/:runId/events|trace
GET  /api/artifacts | GET/DELETE /api/artifacts/:name
GET  /api/gpu/status | /api/gpu/timeline | /api/vram/profiles | POST /api/estimate
GET  /api/mcp/config | GET /api/ws
```

### MCP（9 个工具，给 DSH）

`flowedge_list_workflows` · `flowedge_run_workflow` · `flowedge_run_status` · `flowedge_estimate`
`flowedge_generate_image` · `flowedge_get_workflow` · `flowedge_get_spec`
`flowedge_apply_spec` · `flowedge_list_modules`

## 四、验证证据（全部实测）

| 项 | 命令 | 结果 |
|---|---|---|
| 后端编译 | `cd backend && go build ./...` | 通过 |
| 后端测试 | `cd backend && go test ./...` | 4 包全过 |
| 一键全检 | `python scripts/check-all.py` | **ALL-PASS**（build + test + tsc + 前端契约） |
| 质量循环闭环 | `go test ./service -run TestQualityLoop` | **R1 320×240/16 帧 33 分 → 补丁 → R2 720×240/48 帧 100 分** |
| 质量循环失败语义 | `TestQualityLoop_FailsWhenTargetUnreachable` | 修不动时显式报错并带报告 |
| 前端类型 | `cd frontend && npx tsc --noEmit` | 0 错误 |
| 前端测试 | `cd frontend && npm test`（跑批器已钉 `NODE_ENV=test`） | **74/74 通过** |
| 打包 | `python scripts/build-windows.py` | 19.9 MB 单文件 |
| 启动冒烟 | 临时目录跑 exe，`/api/health` + `/api/modules` | `{"status":"ok"}`；模块 27 个，含 `quality_loop` |
| MCP | `FlowEdge.exe --mcp` + `tools/list` | 9 个工具正常返回 |

> **已修**：本机 shell 带 `NODE_ENV=production`，曾让 73 个前端用例全部假红。
> 现在 `scripts/run-tests.mjs` 自己钉死 `NODE_ENV=test`（define + 子进程 env），不再依赖外部环境。

## 五、没做的 / 已知限制（诚实清单）

| 项 | 现状 |
|---|---|
| 真实 GPU 出片验证 | **未做**。质量循环的闭环用 ffmpeg 合成视频验证（真跑门、真改参数、真重跑），但没有在 ComfyUI/H3 上跑一条真实生成 |
| 生成类问题（乱语、画面不对） | 只能重采样，不能"自动改提示词"——那需要 LLM 参与且容易变成随机调参 |
| `duration_max` 类修复 | 不自动改（降时长会改内容），交给人 |
| 依赖页面 | ComfyUI/ASR/TTS 仍需先起（`POST /api/deps/:name/start` 或 `运行\1-启动 FlowEdge.bat`） |
| 旧工作流兼容 | #113 这类用了 `consistency_gate`/`post_production` 的图仍能跑；用了 `agent_task`/`http_request` 的旧图会报 unknown node type（预期内） |

## 六、怎么用（DSH 视角）

```bash
# 1. 起引擎（常驻，不在 agent 沙箱内）
运行\1-启动 FlowEdge.bat

# 2. 质量循环：模板「质量循环出片」→ 把 quality_loop 的 workflow_id 指向你的生成工作流
#    或者直接造图：
```

```json
{"id":"q1","type":"quality_loop","data":{
  "workflow_id": 113, "max_rounds": 3, "target_score": 100, "repair": "auto",
  "min_frames": 96, "min_width": 720, "require_audio": true
}}
```

DSH 侧调 `flowedge_run_workflow` 即可；运行中每轮通过 WS 推 `quality_round` 事件，
`GET /api/runs/:id` 里能看到逐轮得分与补丁。
