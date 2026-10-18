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
| A3 | 表达力下限 | ✅ 部分 | 变量支持 JSONPath 数组索引 `{{node.list[0].field}}`；代码执行节点（yaegi + stdlib 白名单 + 超时）；❌ 变量聚合器节点未做 |
| A4 | 检索质量下限 | ✅ 部分 | BM25（中文 bigram）+ 向量双路 + RRF 融合 + MMR 去重 + min_score；批量写入把导入从 O(n²) 降到 O(n)；结构化引用 `source#chunkN`；❌ 重排（rerank）与 PDF/DOCX 解析未做 |
| A5 | 安全下限 | ✅ 部分 | 默认只 bind `127.0.0.1`；`FLOWEDGE_HOST=0.0.0.0` 必须同时设 `FLOWEDGE_API_TOKEN` 否则拒绝启动；token 鉴权中间件；CORS 不再无条件 `*`；❌ credentials 加密存储未做 |

---

## 三、C 类：反超标签（三家都没做好的地方）

| # | 旗帜 | 状态 | 做了什么 |
|---|---|---|---|
| **C1** | 单 GPU VRAM 感知调度 | ✅ | 引用计数（`Refs>0` 禁止驱逐，不会把在跑的任务踢翻）；**真实卸载**（Ollama `keep_alive:0` / ComfyUI `POST /free`）——原实现只改内存状态、显存一点没释放；文生图串行门（并行跑两个 flux 只会互相 OOM）；遥测不可用时不阻断执行；`GET /api/gpu/timeline` + 环境面板可视化 |
| **C2** | LLM 编排 × 像素生成闭环 | ✅ 部分 | ComfyUI 工作流模板化（`data/comfy_workflows/*.json` + `__PROMPT__/__WIDTH__/__SEED__` 占位符）→ ControlNet/LoRA/img2img 任意图可直接接入；视频多图轮播（原来只取第一张，其余静默丢弃）；❌ 产物血缘（mp4 由哪张图 + 哪段 TTS 构成）未做 |
| **C3** | 离线全栈「自证清白」 | ✅ | ASR/TTS 未配置改为显式失败（`config_missing`），不再返回 `[transcribed from xxx]` 与 `fake audio data`；前端节点库按 `/api/capabilities` 置灰（缺什么一目了然，仍可先编排）；❌ TTS 零依赖 SAPI 直调未做 |

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
| 变量聚合器节点 | 本轮优先做代码节点（表达力收益更大） | 复杂汇合仍需 code 节点手工处理 |
| Credentials 加密存储 | 需要密钥管理 UI | HTTP 节点仍需手填 header |
| Rerank（bge-reranker） | 需额外模型常驻显存 | 混合检索已可用的前提下收益递减 |
| PDF / DOCX 解析 | 纯 Go 解析质量风险高，且需新依赖 | 知识库仍仅 .md/.txt |
| 产物血缘 | 需要新的 artifact 模型 | 视频/图片溯源靠文件名 |
| TTS 零依赖路径（SAPI 直调） | 现路径已可用（Python 语音服务） | 裸 exe 需先 `--export-services` |
| App.tsx 拆分（1352 行） | **主动跳过**：纯工程整洁度，stale closure 回归风险高（编译能过、测试能过、功能可能是死的） | 后续改动需更小心 |
| electron-builder 实际打包 | 配置已就绪（`desktop/package.json`），未执行 `npm run dist`（需下载 100MB+ 打包链） | 交付形态仍是单 exe + 可选 Electron 壳 |
| 自动布局 / 分组子画布 / 批注 / 浅色主题 | 体验类，优先级低于正确性 | 大图仍需手动整理 |

---

## 六、硬数据

| 指标 | 基线 | 现在 |
|---|---|---|
| Go 测试 | 33 | **91**（0 失败） |
| 节点类型 | 13 | **14 + 3 插件**（start/end/llm/text2image/asr/tts/video_compose/if_else/loop/sub_workflow/http_request/data_transform/rag_query/**code**） |
| API 端点 | 33 | **39**（+runs 全局列表/events/trace/rerun-from/gpu timeline/…） |
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

### 本轮修掉的 3 个「文档说支持、代码不支持」

- `FFMPEG_PATH` 环境变量（能力自检提示文案宣称支持，代码从未读取）
- 快捷键保存（`onSave` 是 `console.log('save')`）
- 插件系统（`plugin_system.go` 完整实现但零调用 → 已接入节点注册表）
