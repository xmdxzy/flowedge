# FlowEdge SOTA 达成度对照表

> 对标口径：**私有化 · 单 GPU · 本地多模态工作流**这一细分的 SOTA
> 统计时间：2026-10-19
> 基线：`baseline-pre-sota`（commit 4b098a3，33 测试，exe 18.3 MB）

---

## 一、结论

**在「单张消费级 GPU + 全离线 + LLM 编排 × 像素生成」这个细分里，FlowEdge 已经从「能跑通的 MVP」推进到「可日常使用、可复盘、可扩展」的水平。**

它现在不是 Dify 的对手（不做多租户/插件市场/RAG 全家桶），也不是 n8n 的对手（不做 400 个 SaaS 连接器）。但它占住了两家都没占的位置：**同时拥有 LLM 大脑与像素手脚，并且真正管理 GPU 显存**。

A 类（入场券）**基本追平**，C 类（反超标签）**已立起三面**，B 类（放弃项）**按计划放弃**。

---

## 二、A 类：必须追平项

| # | 维度 | 状态 | 证据 |
|---|---|---|---|
| A1 | 可执行性可诊断 | ✅ | 环检测（Kahn 入度法，`service/dag.go`）在保存与执行前拦截；节点级超时 `timeout_ms`（默认 180s，上限 30min）；结构化错误码 `timeout/config_missing/cycle/retry_exhausted/cancelled`；事件日志持久化 `run_events` 表 |
| A2 | 可恢复 | ✅ | 单节点重跑 `POST /api/runs/:id/rerun-from`（下游闭包重算、上游输出保持不变，有测试锁定）；持久化任务队列 `jobs` 表 + 启动 `RecoverInterrupted`（把残留 running 标记 interrupted）；续跑也可取消 |
| A3 | 表达力下限 | ✅ | 变量支持 JSONPath 数组索引 `{{node.list[0].field}}`；代码执行节点（yaegi + stdlib 白名单 + 超时）；**变量聚合器节点已补齐**（object/list/text/first/last 五种模式，支持显式顺序与字段选择） |
| A4 | 检索质量下限 | ✅ 部分 | BM25（中文 bigram）+ 向量双路 + RRF 融合 + MMR 去重 + min_score；批量写入把导入从 O(n²) 降到 O(n)；结构化引用 `source#chunkN`；**PDF/DOCX/CSV 解析已补齐**（PDF 优先 pdftotext，缺失时纯 Go 流解析器，乱码显式报错）；❌ 重排（rerank）未做（需额外常驻模型，收益递减） |
| A5 | 安全下限 | ✅ | 默认只 bind `127.0.0.1`；对外必须 `FLOWEDGE_API_TOKEN`；token 鉴权中间件；CORS 白名单；**凭据加密存储已做**（AES-256-GCM 落 `data/credentials.json`，列表接口只回 header 名，HTTP 节点按名称注入 header/query，导出工作流不再泄密） |

---

## 三、C 类：反超标签（三家都没做好的地方）

| # | 旗帜 | 状态 | 做了什么 |
|---|---|---|---|
| **C1** | 单 GPU VRAM 感知调度 | ✅ | 引用计数（`Refs>0` 禁止驱逐，不会把在跑的任务踢翻）；**真实卸载**（Ollama `keep_alive:0` / ComfyUI `POST /free`）——原实现只改内存状态、显存一点没释放；文生图串行门（并行跑两个 flux 只会互相 OOM）；遥测不可用时不阻断执行；`GET /api/gpu/timeline` + 环境面板可视化 |
| **C2** | LLM 编排 × 像素生成闭环 | ✅ | ComfyUI 工作流模板化（`data/comfy_workflows/*.json` + 占位符）→ ControlNet/LoRA/img2img 任意图可直接接入；视频多图轮播；**产物血缘已做**（`artifact_lineages` 表 + `/api/artifacts/:name/lineage`，只认真正的产出节点、跳过 end 等透传节点，画廊里点开即见「哪张图 + 哪段音合成这个 mp4」） |
| **C3** | 离线全栈「自证清白」 | ✅ | ASR/TTS 未配置改为显式失败（`config_missing`），不再伪造结果；前端节点库按 `/api/capabilities` 置灰；**TTS 零依赖已做**（Windows SAPI COM 直出 wav，实测 `backend=sapi`；也支持服务优先）；**依赖进程托管**：ComfyUI / ASR 一键启停（实测引擎拉起 ComfyUI 冷启 ~40s 即出图） |

---

## 四、B 类：明确放弃（按计划）

| 放弃项 | 理由 |
|---|---|
| 节点生态广度（n8n 400+） | 纯体力活，价值来自 SaaS 生态，离线用户没有 SaaS 账号可接 → 用代码节点替代 |
| 多租户 / RBAC / 审计 | 目标用户单人单机 |
| Prometheus / OTel / Grafana | 单进程单机，SQLite 表 + 时间线足够 |
| K8s / 分布式队列 | 与单文件 exe 交付红线冲突 |
| 自研推理后端 / fine-tune | Ollama / ComfyUI / faster-whisper 已是各自领域 SOTA |
| 多模态输入（VLM 节点） | 24GB 上 VLM 与 flux/LLM 抢不过 |
| 代码签名证书 | 个人分发收益低 |

---

## 五、未完成清单（诚实披露）

| 项 | 原因 | 影响 |
|---|---|---|
| ~~变量聚合器节点~~ | 已补齐 | — |
| ~~视觉理解（VLM）节点~~ | 已补齐：本地 qwen2.5vl:7b，图片 base64 走 Ollama /api/chat，输出中文描述 | — |
| ~~图生图 / 重绘 / 超分~~ | 已补齐并实测：img2img（0.85 明显改风格）、inpaint（蒙版尺寸校验）、upscale（768→3072） | flux 非专用重绘模型，蒙版内修补偏弱 |
| ~~生成式视频~~ | 已补齐：Wan2.1-T2V-1.3B，832×480×33 帧实测 64 秒出片 | 只有 2 秒/段；H3 因 36GB 权重超显存仍不可用 |
| ~~批量出图~~ | 已补齐：`batch` 参数一次提交 1–8 张，共用一次模型加载 | — |
| ~~Credentials 加密存储~~ | 已补齐（AES-GCM + 脱敏列表 + 节点注入） | — |
| Rerank（bge-reranker） | 需额外模型常驻显存 | 混合检索已可用的前提下收益递减 |
| ~~PDF / DOCX 解析~~ | 已补齐：DOCX 走 archive/zip（零依赖），PDF 优先 poppler、退化纯 Go | 扫描件仍无法解析（会显式报错） |
| ~~产物血缘~~ | 已补齐（含透传节点过滤） | — |
| ~~TTS 零依赖路径~~ | 已补齐（SAPI COM，免 Python） | — |
| App.tsx 完全拆分 | 已抽出配置表（`config/graph.ts`）与图算法（`lib/graphUtils.ts`）、主题（`lib/theme.ts`），主文件仍偏大；继续拆属性面板收益递减 | 剩余耦合集中在属性面板 |
| electron-builder 实际打包 | 配置已就绪（`desktop/package.json`），未执行 `npm run dist`（需下载 100MB+ 打包链） | 交付形态仍是单 exe + 可选 Electron 壳 |
| ~~自动布局 / 分组 / 批注 / 浅色主题~~ | 已补齐（分层拓扑布局、分组框 + 解散、画布批注、深/浅主题切换） | — |

---

## 六、硬数据

| 指标 | 基线 | 现在 |
|---|---|---|
| Go 测试 | 33 | **108**（0 失败） |
| 节点类型 | 13 | **20 + 3 插件**（新增 vision / image2image / inpaint / upscale / text2video）（start/end/llm/text2image/asr/tts/video_compose/if_else/loop/sub_workflow/http_request/data_transform/rag_query/**code**） |
| API 端点 | 33 | **50**（+step/continue、credentials、deps、lineage、runs 全局列表/events/trace/rerun-from…） |
| exe 体积 | 18.3 MB | **~31 MB**（+12.9MB 来自 yaegi stdlib 符号表） |
| `go vet` | 通过 | 通过 |
| `tsc --noEmit` | 通过 | 通过 |
| 数据落位 | 曾误写 CWD | **修复：始终跟随 exe 目录**（原 `BaseDir` 用「路径含 Temp」判断 go run，误伤放在临时目录的真实 exe） |

### 本轮修掉的 5 个真 bug

1. **成环静默零执行**：`remaining` 永不归零 → 主循环一次都不跑、不报错
2. **撒谎式降级**：ASR/TTS 未配置返回假数据且 `error=nil`；embedding 降级到哈希向量不报错
3. **向量库 ID 碰撞**：`generateID` 只有 3380 个取值，Delete 会误删无关记录
4. **GPU 调度器自死锁**：`AcquireModel` 持锁时调用 `GetGPUInfo`（内部再 Lock），`sync.Mutex` 不可重入
5. **BM25 df 统计错误**：按出现次数而非文档数累计，长文档重复词会让 idf 变负、排序完全错乱

### 本地模型栈与显存治理（2026-10-19 第二轮）

- **资产集中**：ComfyUI 的 99GB 权重从 `D://AI//ComfyUI//models` 迁到 `D://AI//Models//comfyui`，
  用 `extra_model_paths.yaml` 接管；Ollama / ASR / TTS 权重也落在同一根下。项目目录零权重（C 盘不背 100GB）。
- **模型升级**：`qwen2.5:14b`（LLM）+ `qwen2.5vl:7b`（VLM）+ `faster-whisper-large-v3` + `Kokoro-82M` 全部落地并实测。
- **显存仲裁**：扩散 / 语言 / 语音三类同一时刻只驻留一类；换侧调卸载接口并轮询 `vram_free` 等真正归还；
  同侧连续调用不重复卸载；ComfyUI 以 `--disable-smart-memory` 启动；语音服务默认 CPU。
  同一流程峰值从 **24.0GB → 15.0GB**，结束回落到 1.6GB（3090 + UE5 同时开着）。
- **新增 `/api/models`**：按家族列出根目录、数量、体积与具体文件，环境面板直接呈现。
- **补齐生成侧全链路**：图生图 / 局部重绘 / 超分 / 文生视频 / 批量出图，20 种内置节点。
- **发现并修复 `comfy-kitchen` 版本错位**：0.29 带的 0.2.22 缺 `AsymW4A8Int8Layout`，导致
  **所有 fp8/量化文本编码器加载后返回 None**（表现为 `NoneType object has no attribute 'Params'`）。
  升到与 0.31 匹配的 0.2.27（0.2.28 需要 torch>2.6，本机 torch 2.6 会报 `list[int]` 不支持）。

### 本轮修掉的多模态真 bug

- **单图 + 配音产出「无声画面」的 mp4**：只有多图才 `-map [v]`，单图又用 `-map 1:a` 指音轨，
  ffmpeg 一旦见到显式 `-map` 就只输出被映射的流 → 视频流被静默丢弃。已统一显式映射并加回归测试
  （`TestComposeVideoSingleImageKeepsVideoStream` 直接问 ffprobe 要流列表）。
- **产物路径相对/绝对混用**：TTS / 视频输出改成绝对路径，画廊复制出来的路径可以直接粘回节点。
- **ASR 服务被拉起却仍显示「未启用」**：`ASR_BASE_URL` 为空时端点为空 → 探活失败、节点报 config_missing。
  现在检测到本地 `local-services` 就默认 `http://127.0.0.1:8101`。

### 本轮修掉的 3 个「文档说支持、代码不支持」

- `FFMPEG_PATH` 环境变量（能力自检提示文案宣称支持，代码从未读取）
- 快捷键保存（`onSave` 是 `console.log('save')`）
- 插件系统（`plugin_system.go` 完整实现但零调用 → 已接入节点注册表）
