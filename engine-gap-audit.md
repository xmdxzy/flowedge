# FlowEdge 引擎层能力审计（vs 商业多模态工作流产品）

日期 2026-10-19 · 范围：`backend/` 全量（14.3k 行 Go）· 每条结论附 file:line，未找到的显式标注 "grep 未命中"。

> **状态更新（2026-10-19 13:45，读本文前必看）**：
> 本文 §1 能力矩阵中「参考图/角色锁定（ref2va）❌ 缺失」与「单镜 seed 指定/记录 ❌」**已过期**。
> 该链路已于当日 13:37 实测打通：`comfy_media.go` 的 `h3_r2v` 分支（`ref_audios`）、
> `media_nodes.go` 自动切 kind、`prompt_opt.go:ensureRef2VAPrompt` 六段式补全、`ref2va_test.go` 回归测试，
> 实测 ASR 由「枉年 赏我鸡肉海鲜」变为「这件事我其实早就知道了」（见 `AGENTS.md` §5.8）。
> 其余条目（镜头级作业缺失、30min 超时上限、血缘只覆盖 3 类、best-of-N 缺失、
> `AddSubtitles` 死代码、无 9:16/字幕/BGM）仍与代码一致。
方法：先读 `AGENTS.md`（项目自述已解决问题），再逐文件读 `service/*.go`、`handler/*.go`、`model/*.go`、`scripts/verify-media.py`。

---

## 0. 节点注册表（全部已注册类型 → 实现文件）

注册表 `backend/service/engine_v2.go:844-868`（23 个内置），插件另注册于 `backend/main.go:168-187`；经 `GET /api/capabilities` 下发（`backend/handler/health.go:60-62`）。注释里写"内置 15 种"（`engine_v2.go:151`）是过期描述，实际 23。

| 分类 | 类型 → 实现 |
|---|---|
| 控制 | start `engine_v2.go:910` · end `:1459` · if_else `:1416` · loop `:1711` · sub_workflow `:1653` |
| AI | llm `:924`（工具循环 `:1047`）· text2image `:1180` · rag_query `:1544` · vision `vision_node.go:19` · prompt_opt `prompt_opt.go:230` · storyboard `shotlist.go:65` · shotlist `shotlist.go:120` |
| 媒体 | text2video `media_nodes.go:194` · image2image `:85` · inpaint `:118` · upscale `:170` · video_compose `engine_v2.go:1373` · asr `:1291` · tts `:1322` |
| 数据 | http_request `:1466` · code `exec_code.go:96` · data_transform `:1517` · aggregate `aggregate_node.go:29` |
| 插件 | echo / string_length / uppercase `plugin_system.go:104-155` |
| 前端画布专用 | note / group `frontend/src/config/graph.ts:75-76`（无后端执行体，纯装饰） |

底层媒体全部收口到 `BuildMediaGraph`（`comfy_media.go:237-511`，5 个 kind：img2img/inpaint/upscale/t2v/h3）与 6 个 ffmpeg 函数（`video_edit.go`）。**H3 只有 fl2va 一条链路**（`comfy_media.go:464-501`），`ref2va` 参数在 `backend/` 内 grep 未命中（唯一命中是 `tts_sapi.go:32` 的 PowerShell 文本）。

---

## 1. 能力矩阵

状态：✅ 已实现并接线 · ⚠️ 部分/需人工 · ❌ 缺失（含死代码）

| 能力 | 状态 | 证据 |
|---|---|---|
| **1 生成粒度/重试** | | |
| 节点级重跑（整图 DAG） | ✅ | `handler/workflow.go:288-299`；闭包 `engine_v2.go:371-391` |
| 单镜（镜/segment）重跑 | ❌ | shotlist 是**单个节点内的大循环** `shotlist.go:188-252`；无镜头级 API，前端也只有节点级"从该节点重跑" `frontend/src/App.tsx:749-773` |
| 单镜失败隔离 | ❌ | 第 N 镜失败即整片失败：`shotlist.go:205-208`（`return nil, ...`）；前 N−1 镜成片丢失于节点输出之外 |
| 镜头级断点落盘 | ❌ | 循环内无 persist；仅 DAG 层末落库 `engine_v2.go:817-820` → 20 分钟工作在第 5/6 镜失败时无法复用 |
| 批量出图逐条重试 | ❌ | 多提示词一次成图 `engine_v2.go:1237-1252` → `comfy_media.go:616-681`；任一 KSampler 报错即整图 error `comfy_media.go:166-168`，11 张成功结果一并丢弃 |
| 节点级 retry（整体重跑） | ✅ | `engine_v2.go:657-667`（上限 5），shotlist 上等于重拍全片 |
| 超时预算 | ⚠️ | `nodeTimeoutFor` shotlist = 镜数×3min+5min，硬上限 30min `engine_v2.go:22,57-73`。12 镜（`shotlist.go:167-169` 上限）× 实测 169s/镜 ≈ 34min + 装配 + 插帧 → **必然被 30min 上限杀掉** |
| 单镜 seed 指定/记录 | ❌ | h3 图 seed 取 `req.Seed`，0 则随机 `comfy_media.go:238-241`；shotlist 从不传 Seed `shotlist.go:194-199`；lineage 不记 seed `lineage.go:121` |
| **2 择优/质量** | | |
| best-of-N（出 K 选 1） | ❌ | 只有 `batch`（同 prompt K 张，`engine_v2.go:1256-1278`）；下游固定取 `images[0]` `media_nodes.go:58-66`。全库 grep `score/candidate/rank` 只命中 RAG/BM25 `bm25.go:128-143` 与显存 LRU `gpu_scheduler.go:347-392` |
| 自动质量打分 | ❌ | 无任何图像/视频打分 |
| VLM 质检节点 | ⚠️ | `vision_node.go:19-90` 存在但要人工连线；只读自身 `data.prompt`，拿不到生成它的 prompt（`AGENTS.md:278` 自认"部分"） |
| 抽卡重试（不合格自动重生成） | ⚠️ | `loop` 节点只能对**文本**做条件判定 `engine_v2.go:1711-1866`、`matchCondition :1443-1457` |
| **3 角色一致性** | | |
| 逐镜重复角色描述 | ⚠️ | 单个 `character` 字符串 `shotplan.go:43-49`，逐镜拼进 prompt `shotplan.go:149-159`；**不支持多角色** |
| 末帧续接（continue） | ✅ | `shotlist.go:190-203,246-251`、`media_nodes.go:333-362` |
| 参考图/角色锁定（ref2va） | ❌ | H3 只有 `MiniMaxH3ImageToVideo` + `MiniMaxH3AddGuide frame_idx=0` `comfy_media.go:404-407`；grep `ref2va/reference/subject/pulid/instantid` 均未命中 |
| 角色/道具资产库 | ❌ | 无；`data/` 下无角色桶 |
| **4 资产与血缘** | | |
| 血缘记录 | ✅ | `lineage.go:21-32`（name/kind/workflow_id/run_id/node_id/parents/meta） |
| 血缘覆盖范围 | ❌ | `mediaProducerTypes` 只含 text2image/tts/video_compose `lineage.go:77-81` → **shotlist/text2video/upscale/inpaint/image2image 全部无血缘**（视频管线主产物恰好不在内） |
| 参数留痕 | ❌ | meta 只收 prompt/text/width/height/image_duration/voice `lineage.go:121`；无 seed/steps/model/耗时 |
| 钉选/对比/多 take 版本 | ❌ | `handler/artifact.go:79-142` 只有 list/serve/delete；无 pin/tag/compare |
| 项目 / 资产桶 | ❌ | 产物扁平落在 `data/generated`，按 basename 唯一索引 `lineage.go:23`，跨项目同名互相覆盖；无 project 维度 |
| 工作流版本 | ✅ | `model/version.go:9-18`、`handler/version.go:29-61`（只版本化图，不版本化 take） |
| **5 长任务人机工程** | | |
| 持久化队列 + 并发上限 | ✅ | `queue.go:25-33`，默认 2 `main.go:459-468` |
| 崩溃恢复 | ⚠️ | 只把 running 标 interrupted `queue.go:64-71`；需人工 resume `handler/workflow.go:302-410` |
| 提交不阻塞 | ❌ | `Enqueue` 先 `q.sem <- struct{}{}` 再起 goroutine `queue.go:44`，且 `StartRun` 同步调用 `handler/workflow.go:246-253` → 队列满时 **`POST /run` 的 HTTP 请求被挂住** |
| 取消粒度 | ⚠️ | `CancelRun` 只在**层边界**检查 `engine_v2.go:315-323,556-568`；shotlist 循环体不读 ctx `shotlist.go:188-252`；不调 ComfyUI `/interrupt`（grep 未命中）→ 已在跑的 H3 任务继续烧卡 |
| 队列位置/ETA 可见性 | ❌ | 无队列视图 |
| **6 成本/吞吐可观测** | | |
| 单节点墙钟 | ✅ | `NodeInstance.StartedAt/EndedAt` `model/workflow.go:60-61`；trace 汇总 `handler/runs.go:89-96,119-129` |
| token 用量 | ✅ | `event_log.go:131-151` |
| 单节点 VRAM 峰值 | ❌ | `NodeInstance` 无任何显存/模型字段 `model/workflow.go:51-62`；GPU 时间线只存在内存、上限 200 条、不落库 `gpu_scheduler.go:102-111,135-149` |
| 采样 | ✅ | nvidia-smi 5s 轮询 `gpu_scheduler.go:151-181`、`main.go:198` |
| 提交前耗时/显存预测 | ❌ | 唯一"工作量模型"是超时估算 `engine_v2.go:25-82`；无 ETA/成本接口 |
| **7 模型/bundle 管理** | | |
| 启动期 checkpoint 切换 | ✅ | `COMFYUI_CHECKPOINT` `main.go:106` → `multimodal.go:29-45` |
| 节点级模型覆盖 | ⚠️ | 仅 llm `engine_v2.go:959`、vision `vision_node.go:60`、upscale `media_nodes.go:181`；text2image/text2video/shotlist **无 model 字段** |
| H3/Wan 权重参数化 | ❌ | 硬编码 `minimax_h3_fl2va_pruned_int8_convrot...` `comfy_media.go:466-476`、Wan `:344-361` |
| flux 步数参数化 | ❌ | 批量图写死 `steps: 4, cfg: 1.0` `comfy_media.go:589,653` → 换成非 schnell 权重会静默出废片 |
| ComfyUI 模板占位符 | ⚠️ | `__PROMPT__/__WIDTH__/__CHECKPOINT__` `multimodal.go:114-148`，但只在 `GenerateImage` 生效 `multimodal.go:227-233`；img2img/inpaint/upscale/t2v/h3 全走硬编码图 |
| 提交前 `/object_info` 校验 | ❌ | grep `object_info` 未命中；节点缺失/改名只能等 ComfyUI 400（正合 `AGENTS.md:72-77` 的告诫） |
| 模型切换 API | ❌ | `/api/models` 只读清点 `main.go:371-395`；无 switch/validate |
| **8 后期** | | |
| 逐 transition 装配（xfade/acrossfade） | ✅ | `video_edit.go:364-440`（cut .08 / continue .12 / dissolve .30） |
| 响度归一 | ⚠️ | 单遍 loudnorm `video_edit.go:418`（非 two-pass，无 TP 限幅器） |
| 插帧 | ✅ | `minterpolate` CPU `video_edit.go:334-351` |
| 中置相消 / 压音 | ✅ | `video_edit.go:477-497`、`:448-463` |
| 成片自检 | ✅ | `video_edit.go:179-269`（帧数/音画差）+ `scripts/verify-media.py:218-294`，退出码门禁 `:369` |
| 字幕烧录 | ❌ 死代码 | `AddSubtitles` 存在但**零调用** `multimodal.go:590-602`；无 srt/ass 生成 |
| 竖屏 9:16 重构图 | ❌ | grep `9:16/vertical/crop` 未命中 |
| BGM 混音/侧链闪避 | ❌ | 无；`amix` 只出现在测试 `video_edit_test.go:148` |
| 标题/贴纸/水印 | ❌ | grep `drawtext/overlay` 未命中 |
| 对口型 | ❌ | 无 lip-sync 节点（ASR 只用于质检 `video_edit.go:501-510`） |
| 视频延长/末帧可控输出 | ❌ | 取末帧只写临时文件 `video_edit.go:34-50`、`shotlist.go:247-249`，不作为节点输出暴露 |
| 视频超分 | ❌ | 只有图片超分 `media_nodes.go:170-191` |
| **9 批量自动化面** | | |
| 无头触发 | ✅ | `main.go:309`；Bearer 鉴权与 hooks 豁免 `main.go:235-251` |
| 入站 Webhook | ⚠️ | token 即凭据 `trigger.go:123-147`；**不接收任何参数** |
| Cron | ✅ | `cron.go`、`cron_scheduler.go`、`main.go:288-296` |
| 运行入参（参数化批量） | ❌ | `Run` 忽略请求体 `handler/workflow.go:273-281`；变量只能引用上游节点输出 `variables.go:24-78` |
| 幂等键 | ❌ | grep `idempot` 未命中 |
| 完成回调（webhook-out） | ❌ | 无 | 
| 进度 | ✅ | WS `main.go:414` + 轮询 `GET /runs/:id` |
| 产物清单（manifest） | ❌ | 产物列表扁平 `handler/artifact.go:79-112`；血缘覆盖不到视频节点 `lineage.go:77-81` |
| 批量接口 | ❌ | 无 `/batch`；`loop` 只能迭代文本 `engine_v2.go:1711-1866`，无逐项作业状态 |

---

## 2. 最高杠杆的 8 项引擎补强（按性价比排序）

### R1. 镜头级作业模型（断点 + 单镜重跑 + 可取消 + 解开 30min 上限）
- **解锁**：第 5/6 镜失败不再丢 20 分钟；"只重拍第 3 镜"；12 镜长片不再被超时杀；取消能真的停 ComfyUI。这是当前与商业产品差距最大的一条。
- **做法**：新增 `service/shot_job.go` + `model.ShotJob`（run_id, shot_index, prompt, seed, file, status, elapsed_ms, vram_peak_mb, error_code），`AutoMigrate` 追加 `main.go:56`；把 `shotlist.go:188-252` 循环抽成 `executeShot(ctx, plan, i)`，每镜结束 `persistShot()`；装配改为"从已完成镜头重建"的幂等函数（`assembleShots` 已可复用 `video_edit.go:364`）；新增 `POST /api/runs/:runId/shots/:index/reroll`（复用 `ResumeFrom` 思路，但只重跑该镜 + 重新装配）；给 `MediaRequest` 补 `Seed` 并在 shotlist 落库；把 shotlist 的超时从节点级改为"每镜独立预算"，或直接把镜头展开成子作业由 `JobQueue` 调度，避开 `maxNodeTimeout engine_v2.go:22`；`projHandler` 里 `q.sem` 改异步（`queue.go:44`）。
- **风险**：每镜多一次 DB 写（可忽略）；装配重建时末尾 xfade 需按 clip 实际时长重算（现有 offset 逻辑 `video_edit.go:398-405` 已参数化，安全）；VRAM 无变化。
- **工作量**：M+（4–6 人日，含回归：`video_edit_test.go` 的帧数断言必须继续过）。

### R2. 候选生成（best-of-N）+ VLM 打分择优节点
- **解锁**：付费用户默认预期的"抽 3 张挑一张 / 抽 3 条挑一条"，以及无人值守的"不合格自动重抽"。当前引擎连一张废片都无法自动识别。
- **做法**：新增 `select` 节点 + `MediaCandidate` 表（shot_id, seed, path, score, reason, chosen）；`comfy_media.go:616-681` 的 `GenerateImagesFromPrompts` 已支持一次加载出 N 张，扩展为"同 prompt 不同 seed ×K"；给 `vision` 节点补 `reference_prompt` / `reference_image` 入参（`vision_node.go:19-58` 目前只读自身 prompt，正是 `AGENTS.md:278` 记的缺口），打分输出结构化 `{score, issues[]}`；`select` 按阈值取最高分，全部低于阈值时触发 `loop` 重抽；H3 侧逐镜生成 2–3 条候选（成本 ×K）。
- **风险**：GPU 时间 ×K，必须配"低分低分辨率草稿 → 选中后高分辨率/12 步重出"的两级策略，否则 24GB 上不可接受；VLM 与扩散互斥（`vram.go:43-66` `enterSide` 会来回卸载 17GB，每次换侧 5–10s，须把打分批量集中到语言侧一次完成）。
- **工作量**：M（3–4 人日）。

### R3. 模型 bundle 抽象 + 提交前 `/object_info` 预检
- **解锁**：把 flux-schnell 换成更好的 checkpoint、换 H3 权重、换 turbo LoRA 不再需要改 JSON/改代码或重编 exe；同时终结"400 validation failed 才发现节点 id/权重名错"。
- **做法**：新增 `service/model_bundle.go` + `data/models.json`（逻辑角色 → 具体文件 + 默认参数：`t2i.primary{ckpt,steps,cfg}`、`video.wan{unet,clip,vae,frames}`、`video.h3{unet,clip,video_vae,audio_vae,lora}`、`upscale.model`）；`BuildMediaGraph` 里所有硬编码点（`comfy_media.go:344-361,464-476,589,653`）改为读 bundle；节点加 `bundle` 字段（缺省用 bundle 默认）；启动与保存时拉 `GET {base}/object_info` 校验 class_type 与 `unet_name/ckpt_name` 枚举是否存在，缺失即报 `config_missing` 级错误；`GET/PUT /api/models/bundles`（`main.go:371` 旁）暴露读写并用 `ScanModels`（`model_inventory.go:63-199`）枚举候选项。
- **风险**：换更大 checkpoint 触发 OOM —— 必须在切换时用 `GPUScheduler.AcquireModel` 预检（`gpu_scheduler.go:270-325`），并按实测值注册 VRAM 档案；`/object_info` 首次调用可能 1–3s（缓存 60s）。
- **工作量**：M（3–5 人日）。

### R4. 批量/自动化 API 面（入参 + 幂等 + 批量 + 回调 + 清单 + 不阻塞提交）
- **解锁**：外部脚本真正能无头跑 50 条不同视频：一条 `POST /api/batch {workflow_id, items:[{vars…}]×50}` 即可，且能拿到每条的产物清单与完成回调。
- **做法**：`Run`/`Webhook` 接受 `{"vars": {...}}`，在 `resolveData` 前把它并进 `runCtx`（`engine_v2.go:639-647`，变量语法 `variables.go:24` 已支持 `{{x.y}}`，只需允许非节点命名空间）；新增 `POST /api/batch` 生成 N 个 run（复用 `StartRun handler/workflow.go:153`）；`WorkflowRun` 加 `idempotency_key`（唯一索引，重复提交返回既有 run_id）；run 结束回调：`StartRun.exec` 末尾 POST 调用方给的 `callback_url`（HMAC 签名）；新增 `GET /api/runs/:id/manifest`，数据源改为新表 `ArtifactRecord`（见 R5），返回 `{shot/节点 → 文件 → 参数 → 状态}`；把 `queue.go:44` 的信号量获取移入 goroutine，HTTP 立即 202 并返回 `queue_position`。
- **风险**：批量 50 条会把队列堵死（单卡串行）——必须限制单批大小并暴露排队位置；回调需设 3 次重试与超时。
- **工作量**：M（3–4 人日，`runs.go`/`workflow.go`/`trigger.go` 改动集中）。

### R5. Take 资产管理 + 血缘补全
- **解锁**：一条片子的第 3 镜有 5 个 take，可钉选、对比、回滚到某一版；画廊能按项目聚合；视频产物终于可溯源。
- **做法**：先做**一行级修复**——`mediaProducerTypes` 补 `shotlist/text2video/image2image/inpaint/upscale`（`lineage.go:77-81`），并把 `parents` 判定改为"上游产物字段"而非白名单类型；再把 `ArtifactLineage` 升格为 `ArtifactRecord`（加 `project_id, take_of, pinned, seed, steps, model, elapsed_ms, vram_peak_mb`），`AutoMigrate` 追加 `main.go:56`；`handler/artifact.go` 支持子目录与按项目过滤（`:79-112`）、`POST /api/artifacts/:name/pin`、`GET /api/takes/compare?a=&b=`；新增 `model.Project` + CRUD。
- **风险**：`name` 唯一索引（`lineage.go:23`）迁移到 `(project_id,name)` 唯一；历史数据 backfill 需容错（缺字段按旧记录处理）。
- **工作量**：S–M（2–3 人日；仅血缘补全半天）。

### R6. 角色/参考资产锁定（走 H3 ref2va）
- **解锁**：跨镜同一张脸/同一件衣服。当前只能靠"逐镜重复文字 + 末帧续接"，换机位（cut）就断。
- **做法**：先按 `AGENTS.md:10-22` 的纪律**枚举实例**：对该实例 `GET http://127.0.0.1:8189/object_info` 确认 ref2va 相关节点的 class_type 与输入槽（**不能只做显存算术就断言不可用**——这正是 AGENTS 0.1 的事故）；`MediaRequest` 加 `ReferenceImages []string`，`BuildMediaGraph` 增 `h3_ref` 分支（复用 `UploadImageAt(H3BaseURL)` `comfy_media.go:60-108`）；新增 `service/character.go` + `data/characters/<id>.json`（正面/侧面参考图、LoRA 可选、特征描述），`ShotPlan` 的 `character` 从字符串升级为 `characters[]`（`shotplan.go:43-49`），`CompileShotPrompt` 逐镜注入（`:149-159`）；shotlist 逐镜带参考图。
- **风险**：ref2va 权重（R2V）可能未落盘、帧网格/步数与 fl2va 不同（turbo 4 步 用于 R2V）；H3 峰值 21.8–22.6GB（`AGENTS.md:176`），多一张参考图会再涨，需实测峰值而非估算。**若枚举后确认该实例确实不具备，结论必须写成"在当前这个实例上不可用"。**
- **工作量**：M–L（5–8 人日，取决于实例枚举结果）。

### R7. 后期滤镜包（字幕/竖屏/BGM/two-pass/视频超分）
- **解锁**：交付级成片。字幕烧录与 9:16 是短视频场景的**准入门槛**，现在都没有。
- **做法**：在 `video_edit.go` 内加 `buildPostFilters(spec)` 与一个 `post_production` 节点（注册 `engine_v2.go:844`）：① 字幕：接线 `AddSubtitles`（`multimodal.go:590-602`）+ 由 `plan.Shots[].dialogue` 或 ASR 输出生成 `.ass`（样式/位置参数化）；② 竖屏：`crop=ih*9/16:ih,scale=1080:1920` 或"模糊垫底 + 居中缩放"两档；③ BGM：`[music]volume` + `sidechaincompress` 以人声轨为侧链闪避，再 `amix`；④ two-pass `loudnorm` + `alimiter` 替代单遍（`:418`）；⑤ 视频超分：走 ComfyUI 图（已有 `upscale` 分支 `comfy_media.go:309-323`，需改为逐帧/逐段 + 音轨 copy）。
- **风险**：全部 CPU 时间。参考 `AGENTS.md:165`：minterpolate 10.4s 素材耗 36s；字幕/竖屏/超分叠加后，1 分钟成片后期可能到分钟级，必须放进可取消的作业（依赖 R1）。
- **工作量**：每项 S（0.5–1 人日），整包 4–5 人日。

### R8. 逐节点指标持久化 + 提交前 ETA
- **解锁**：用户提交前就知道"这条 30 秒 8 镜大约 25–35 分钟、峰值 ~22GB"；运维能看到每镜真实墙钟与显存峰值，而不是靠 `AGENTS.md` 手抄的区间。
- **做法**：新增 `model.NodeMetric`（run_id, node_id, type, wall_ms, vram_peak_mb, model, seed, output_bytes）——峰值由 telemetry 缓存 + 节点起止时间在采样流上取 max（`gpu_scheduler.go:151-181` 已有 5s 采样，需扩为 1s 并在节点窗口内取峰）；`Trace` 返回 `vram_peak_mb/model/seed`（`handler/runs.go:89-146`）；GPU 时间线落库（现在内存 200 条 `gpu_scheduler.go:102-111`）；`POST /api/estimate {workflow_id}` 用同类型 × 参数的历史中位数回归给出区间（复用 `nodeTimeoutFor engine_v2.go:25-82` 的系数作为先验，但输出区间 + 最坏情况，遵守 `AGENTS.md:32-38`）。
- **风险**：1s 采样增加 nvidia-smi 子进程开销（合并为单次 `--query-gpu` 轮询即可）；估算初期样本少，必须显示"样本不足"而非假精确值。
- **工作量**：M（3–4 人日）。

---

## 3. 已实现 vs 营销话术（避免误判）

**真做了的**：逐 transition 装配与音画同步（含帧数回归测试 `video_edit_test.go:31-78`）、成片自检落输出字段（`shotlist.go:276-281`）、H3 三段式提示词与四层音频治理（`prompt_opt.go:26-44`、`shotplan.go:171-213`）、显存单侧驻留仲裁（`vram.go:43-88`）、依赖进程托管与按类型冷启超时（`deps.go:570-579`）、DAG 校验防静默空转（`engine_v2.go:409-419`）、事件回放与 token 统计（`event_log.go:121-151`）、鉴权与 hooks 边界（`main.go:226-251`）。

**是话术/命名大于实质**：① `App.tsx:2020` 仍写"涡轮 LoRA（8 步）"，后端已是 12 步（`h3_turbo.go:30`）——UI 落后于实现；② `AddSubtitles` 看着像字幕能力，实际零调用（`multimodal.go:590`）；③ "模型资产清点"是只读枚举（`model_inventory.go`），不等于可切换；④ `engine_v2.go:151` 注释"内置 15 种"与 23 种注册不符；⑤ `trace` 只有耗时，把它当"可观测"会掩盖显存/参数/成本三类缺口；⑥ `AGENTS.md` 自认的"VLM 主题一致性质检（部分）"核实无误（`vision_node.go:19-58`）。

**证伪记录（grep 未命中）**：`ref2va`、`object_info`、`drawtext/overlay`、`9:16|vertical|crop`、`lipsync`、`idempot`、`callback`（出站）、`vram_peak`、`eta/predict`、`/interrupt`、`/api/models` 之外的模型切换端点、镜头级重跑路由。
