## [v2026.11.11-v3.3] 发布前收拾了一遍：白屏兜底、静态资源语义、日志降噪、Docker 能起来了

这次没加功能。做的事是：把「别人 clone 下来能不能跑、下载到的 exe 开起来对不对」逐个验一遍，
验出问题就修。

前端
- 加了顶层错误边界。渲染抛错以前是白屏，现在给一张说明页，带「重新加载」和「复制报错」，
  最近一次崩溃记进 localStorage，方便回传。
- index.html 补齐发布需要的元信息：description、两套 theme-color、color-scheme、favicon、
  noscript 提示。
- 修 theme.ts 的一个小毛病：偏好只在解析后主题变化时才写 localStorage，于是深色系统下点「深色」
  不会被记住，重载又退回旧偏好。
- 修首帧闪屏。index.html 的内联脚本写死了 `localStorage || 'dark'`，而 theme.ts 首帧后按系统切
  浅色，浅色系统用户会先黑一帧再变白。两处判断顺序现在完全一致。
- 运行页补了「从断点继续」。后端 /api/runs/:id/resume 一直在，界面没入口，interrupted 的运行
  只能从零重跑。
- 依赖抽屉的 3 秒轮询在标签页隐藏时暂停，回前台补一次。

后端
- 静态资源换了缓存策略：带哈希的 /assets/* 走 immutable，index.html 走 no-cache。缺文件时
  带扩展名的路径返回真 404，不再回退成 index.html（浏览器会把 HTML 当 JS 解析，报
  Unexpected token '<'）。
- 不再用 gin.Default() 的逐请求日志。探活、依赖 /wait 轮询、媒体流 Range、静态资源这些成功
  请求不写了；4xx/5xx 照写。日志里剩下的都是能办事的行。
- 上面这些约定补了三条测试：静态缓存头、缺失资源 404、噪声白名单。

发布与仓库
- README 改回实话，删掉已经不存在的东西（/v1 OpenAI 面、yaegi 插件、quality_gate 节点、
  /api/estimate、breakpoint、AES 凭据库），MCP 工具名换成实际注册的五个。
- 新增 scripts/release-check.py。它拿 dist/FlowEdge.exe 在临时目录真起一个实例，断言 20 项：
  健康检查、ldflags 版本号、壳不缓存、favicon MIME、哈希产物 immutable、缺失资源 404、SPA 深链、
  11 条流水线的 id 集合、未知 API 返回 JSON。打包之后才有的问题只能这样验。
- 新增 scripts/privacy-scan.py，并接进 CI：用户名、邮箱、局域网 IP、密钥、日志、内部档案
  都不许进仓库。
- 新增 RELEASE.md（发版步骤）、SECURITY.md（威胁模型和不算漏洞的几种情况）、
  .gitattributes、.editorconfig。
- Docker：后端镜像补上 data/pipelines.json（缺它 log.Fatalf 直接退出）；两个镜像加健康检查，
  后端改非 root；nginx.conf 修好 /api/ws 的握手，之前升级头挂在没人用的 /ws 上；
  .dockerignore 排掉 15GB 的运行目录。
- 清掉 17 项临时残留和 32.2GB 本地产物，源码和 data/pipelines.json 没动。
- 内部档案（助手会话记录、机器部署记录、交付与调研笔记）移到 docs/internal 并不再跟踪，
  文件还留在本地。

验证
- check-all.py --full：全过（go build、514 个测试、tsc、64 个前端用例）。
- release-check.py --expect 2026.11.11：20 项全过，跑的是 dist/FlowEdge.exe。
- Playwright 打开出片台、画布、运行三页，零 pageerror、零 console error、零请求失败。

没验证的部分
- Docker 只做了 YAML 校验和配置审查。这台机器没装 docker，镜像没真构建过。
- 截图是人工看的，没有像素基线比对。

## [v2026.10.31-v3.2] 代码知识片：竖屏（9:16 / 3:4 / 1:1）

上一版四套风格只有横屏。真机反馈「竖屏也要做」——不是把横屏挤扁，四套风格各写了一套竖版排版。

**新增**
- `code_video` 节点新增 `aspect`（`16:9` / `9:16` / `3:4` / `1:1`），**优先于 width/height**：
  一个下拉框切画幅，不用让用户同时填两个数。`aspectDims()` 映射到 1920×1080 / 1080×1920 /
  1080×1440 / 1080×1080。
- `knowledge_film` 新增 `画幅` enum 字段（默认 16:9），绑定 `code-1.aspect`。
- 四套风格各自的竖版分支（`PORTRAIT`，视口推导设计尺寸 `DW=1080, DH=round(1080×VH/VW)`）：
  - `darkcard`：卡片 940×0.66H，走廊间距按卡高推；下半部用 220px 超大序号当版面锚点。
  - `studio`：HUD → 92~60px 五档自适应标题 → 要点 → 数据区（0.545H）→ 分段章节条（0.775H）。
  - `atlas`：按「一页图鉴」排（眉标 → 宋体标题 → 剖面图版 → 图例 → 页码点 → 页脚），
    图版高度改成**按剩余空间预算**，不再固定比例（固定比例在 3:4 上会把图例顶到页脚上）。
  - `blueprint`：图版重排（标题 74px 最多 3 行、按标点断行；要点 42px；图区 0.54H），
    右下标题栏随画布自适应。
- **安全区**：竖屏顶部 ~13%、底部 ~18% 会被平台 UI 压住（头像/文案/点赞条）。四套都收在安全区内
  —— 实测过版式排到 0.82 以下，成片上线后章节条直接被盖掉。

**测试**
- 新增 `code_video_portrait_test.go`（live）：6 个组合（三套 9:16、atlas/darkcard 3:4、blueprint 9:16）
  逐一出片，判据是 `nb_read_frames == duration×fps` **且 ffprobe 尺寸等于传入的 --size**
  （尺寸传错时帧数完全正常，只查帧数发现不了）。

**实测（24fps / 每卡 4s / 3 卡 = 288 帧）**
- 台架：6 组合全 PASS，空闲机器 15–20s/条；竖屏比横屏贵约 3 倍（JPEG 截图为竖幅时更贵）。
- 真机：`{style:darkcard, aspect:9:16}` → run 391 success，1080×1920 / 144 帧；
  `{style:atlas, aspect:3:4}` → run 393 success，1080×1440 / 144 帧，`verify-media` 仅 `no_audio` WARN。

**未做**：竖屏只改布局不改内容密度（要点超 3–4 条会被截断）；竖屏下的字幕安全区没做；
`1:1` 只有分支逻辑，没逐套看过成片。

---
## [v2026.10.31-v3.1] 代码知识片：四套视觉风格 + 设计 token 层

`code_video` 原来只有一套「工程蓝图」模板，成片风格被用户判为「太差劲」。本版把调研到的
头部科普号视觉签名做成可复用模板体系（调研见 `docs/调研-Vibe知识科普前沿方案-20261030.md`，
体系说明见 `docs/代码知识片-视觉风格体系.md`）。

**新增**（`service/codetpl/`）
- `shared/kit.js`：确定性数学/排版/纹理。含 `wrapByPunct`（中文按标点断行，不再把「为什么」
  拆成「为什/么」）与 `wrapBalanced`（均衡断行）、`makeGrain`（纸张噪点）。
- `shared/tokens.js`：设计 token（四套配色 + 字号阶）。**改色只改这里**。
  调研结论是「好看不是审美，是可复用的约束」——头部账号 15 分钟不出现第四种强调色，
  靠的是写死 token，不是每帧现调。
- 三套新风格（同一份 CodeSpec，四种渲染）：
  - `darkcard` 暗色 3D 卡片世界 + 准一镜到底（对标抖音「基地」）：DOM + CSS 3D，
    走廊式卡片、相机飞行代替剪辑、三色纪律（白 / 薄荷绿 / 紫）。
  - `studio` 黑画布 MG 信息图 + 分段章节进度条（对标 anything2explainer）：纯 canvas。
  - `atlas` DK 图鉴 / 剖面图解（对标小红书 3D 剖面爆款）：羊皮纸 `#F0EDE6`、思源宋体、
    地层剖面 + 细折线引线标注 + 页码序列 + 「概念示意」免责标注。
- `code_video.go`：风格注册表 `codeVideoStyles` + `materializeCodeTemplate(demoDir, style)`，
  共用层落 `demo/shared/`，风格目录落 demo 根。

**接口**
- `knowledge_film` 新增 `视觉风格` enum 字段（`darkcard` 默认 / `studio` / `atlas` / `blueprint`），
  绑定 `code-1.style`；节点默认 `style` 从 `blueprint` 改为 `darkcard`。

**测试**
- `code_video_styles_test.go`（live，`FLOWEDGE_LIVE_CODEVIDEO=1`）：逐风格真渲染，核对
  `index.html`/`render.js`/`shared/kit.js` 全部落地，且 `nb_read_frames == duration×fps`（±2 帧）。
  实测 15s / 360 帧：blueprint 21.5s、darkcard 23.6s、atlas 23.9s、studio 22.0s，全走 CPU 道。

**踩坑（会再踩）**
- `//go:embed` 的目录模式**跳过以 `_` 或 `.` 开头的目录**：共用层最初叫 `_shared`，
  编译通过、运行时文件根本不存在 → 已改名 `shared`。
- DOM 型风格的卡片容器忘挂 class，柱子会按块级元素竖着堆（已修）。

**未做**：对比面板卡型只有 darkcard 有骨架；竖屏画幅需另做布局；真 3D（Blender）路线未做。

**真机联调抓到并修掉**（台架全绿、上产品就露的两类）
- **中文逗号把要点切碎**：`points` 原来走 `splitLoose`（按 `\n , ， ; ；` 全切），
  「信息密度一样，观感差距来自版式与节奏」被静默拆成两条，成片是被腰斩的两句话。
  现在要点只按换行切（新增 `splitPoints`），`labels`/`values` 仍走 `splitLoose`；
  `TestSplitPointsKeepsChineseCommas` 钉住。
- **超时预算按台架定，真机误杀**：台架 0.065s/帧，真机 0.27s/帧（冷启动 + 抢 CPU）；
  旧公式 `60s + 帧数×0.25`（未除 workers）算出 1m48s，把正常片子判成
  `node timeout after 1m48s`（run 386）。改为 `90s + 帧数×0.55/workers`。
- 真机链路：`POST /api/produce {knowledge_film, style:darkcard}` → run 388 success 21s →
  `verify-media` 192 帧/8.0s/1920×1080（仅 `no_audio` WARN，无声片属预期）。

**顺带修复**：`vlm_gate.go` 有个未使用变量 `actx`，导致 `go test ./service/` 整包编译失败
（工作区里的半成品改动）。按现状直连并留 TODO：`VisionChat` 没有 ctx 入参，
`vlmJudgeTimout` 暂时接不上——不要用空变量糊过去。

---
## [v2026.10.31-v3.0-p4] VLM 判词接真机 + 提示词诊断注入 + 参考帧模块

把 p1/p3 的「协议层」接到真实链路，并补上参考帧的自动收集。

**VLM 判词接真机**（`service/vlm_gate.go`，4 个测试）
- `ExtractFramesUniform`：一次 ffmpeg 均匀抽 3 帧（512 宽），避开首尾黑帧；失败报 ffmpeg 真实 stderr 首行。
- `judgeMedia`：图片直接送，视频抽帧后送；`VisionChat` → `ParseVLMVerdict`。
- 接进 film 终检：`gates.vlm.enabled=true` 才开（默认关，一次调用 10–30s 不能悄悄拖慢所有人）。
- **三种失败分开报**：内容问题（fair/poor/bad）、协议问题（判词不可解析）、环境问题（VLM 不可达）——修复动作完全不同，混成一句"质量不佳"等于没报。
- `TestAppendVLMCheck_EndToEndWithFakeOllama` 用假 Ollama 跑通「送图 → 判词 → 门结论」整链。

**提示词诊断注入**（`prompt_opt.go` + `DiagnoseGuide`，3 个测试）
- 改写前先诊断，把「本次只允许修这几处」拼进 user prompt。
- **诊断 ok 时返回空串 ⇒ 不注入 ⇒ 旧行为逐字不变**（prompt_opt 默认开启，不能让本来没问题的提示词换一种写法）。
- 踩到一个坑并修掉：诊断指导原先拼在 `sceneLock` 分支**之前**，会被 `user = fmt.Sprintf(...)` 覆盖掉——现在拼在之后，并有注释钉住。

**参考帧自动收集**（`service/auto_refs.go`，4 个测试）
- `rankAutoRefShots`：无台词 > 有台词、非 continue > continue、时长长者优先（短镜常是运动模糊的过渡态）。
- `CollectAutoRefs`：从已完成镜抽帧凑够 Max（默认 3，上限 6），每镜一个子目录（同目录连抽会互相覆盖）。
- **未接 shotlist**：动多镜生成主循环前必须真机 A/B（Ref2VA 六段式是多镜一致性的命脉，接错代价高）。

---

## [v2026.10.31-v3.0-p3] 提示词诊断：分型 + 定点算子（TARA 的工程化）

`prompt_opt` 一直是「扩写式重写」：把提示词交给 LLM 重写一遍。两个后果——改好改坏分不清
（没有"改之前缺什么"的结论），且一次改动同时动了主体/场景/镜头/音频四件事，违反
「一轮只动一个变量」。本版补的是**诊断层**：先判缺什么，再只动一处。

**新增**（`service/prompt_diagnose.go`，9 个测试）
- `DiagnosePrompt(text, isVideo)`：判 7 类缺陷——缺动作 / 缺场景 / 缺镜头语言 / 缺音频段 /
  模糊修饰 / 过长 / 空。每条带**原文证据 + 一句可执行修复 + 算子名**。
  - 图片不要求镜头与音频（图片没有运镜、没有音轨）
  - 模糊修饰只报 1 条（同类问题报 5 条会淹没真正的缺口）
  - 严重度：缺动作/场景 = fail，缺镜头/音频/模糊/过长 = warn，全齐 = ok
- `ApplyOperator(text, op, arg)`：只做纯规则算子（trim 按句子边界、replace_ambiguous、
  normalize_space）；**创作性算子（add_scene/add_action/add_camera/add_audio）一律拒绝**，
  因为那要 LLM 生成内容，纯规则硬拼等于编造。
- 词表刻意短而明确：**宁可漏判，不要误判**——误判会让人去改一个本来没问题的提示词。

**未接线**：`prompt_opt` 主流程仍走原来的重写路径。接线方式已定：先 `DiagnosePrompt` 出结论，
按 `NeedsLLM` 分派（false 走纯规则算子，true 才调 LLM 且一次只补一类），最后用
`ApplyOperator` 应用并记录算子名——这样"这一轮动了什么"永远可追溯。

---

## [v2026.10.31-v3.0-p2] 预算门：跑之前给区间、跑之中能停、跑完对账

补上 M4 护城河（成本/时间承诺）的最小闭环。现状是「能估但没人管」：
`EstimateGraph` 早就产出 P50/P90/worst，却只挂在画布的 `/graph/dry-run` 上，
流水线入口的 preflight 只有 missing/warnings——于是"30 秒片要多久"在产品里没有答案。

**新增**
- `service/budget.go`：`Budget{MaxWallSeconds, OnExceed}`，`ParseBudget` 支持
  `params.budget={...}` 与简写 `params.max_wall_seconds`；`OnExceed` 默认 **warn**
  （误杀一条跑了 20 分钟的长片，代价远大于多烧几分钟）。
- **引擎心跳式预算检查**（`EngineV2.SetBudgetGuard` + `emit` 的 `node_progress` 分支）：
  不挂在 `persist` 上，因为 persist 只在**层完成**时调用，而 film 只有一个 shotlist 节点
  ⇒ 层屏障形同虚设，预算要等整片跑完才判一次（那时已经晚了）。心跳是 5 秒级的。
- 超限动作：`stop` → 广播 `budget_exceeded` 事件 + `CancelRun`（会同时打 ComfyUI `/interrupt`
  并清 pending）；`warn` → 只记一条日志（只记一次，否则每 5 秒刷一条）。
- `POST /api/preflight` 响应新增 `estimate`（p50/p90/worst/cold_load/vram_peak + 逐节点 lines + caveat）。
- `manifest` 新增 `budget` 段：`{predicted_p50/p90/worst, actual_sec, verdict, within_budget}`，
  `verdict` 分 under_p50 / in_p50_p90 / over_p90 / over_worst / unknown。

**刻意不做**：GPU 秒预算。引擎目前没有实时采集 GPU 秒（只有节点耗时与峰值显存），
没有数据就不做字段——否则又是"配了却不生效"那一类。

**未验证**：`stop` 的真机行为（要跑一条真片并设一个很短的预算）；单测覆盖了
解析、边界（等于上限不算超）、对账四档、preflight 返回区间。

---

## [v2026.10.31-v3.0-p1] VLM 判词协议：先证据、后等级（质量层的第一步）

接上一版血缘层。这次落**质量层的协议**，不接真机门——理由是先把"判词长什么样、怎么解析、
解析失败算什么"定死，再谈调用，否则又是"看起来加了质检、其实静默通过"。

**为什么做这个**（论文深读的结论，见 `docs/调研-前沿论文深读-20261031.md`）：
> FE 的真瓶颈不是模型不够强，是缺一层把「画面内容对不对」变成可执行诊断的东西。

现有门只量**存在性**（帧数/响度/静音/色温/ASR 验词），量不到"人物换了脸"。
Q-Align（2312.17090）用离散等级代替分数以降抖动；FIRM-Video（2608.21839）要求先给可核验证据。

**新增**（`backend/service/vlm_judge.go`，5 个测试）
- `BuildJudgePrompt`：模板写死三条约束——先列证据再给等级、证据必须带帧号、禁止「可能/似乎」。
  模板是契约，`TestBuildJudgePrompt_Contract` 逐条断言（改模板会红）。
- `ParseVLMVerdict`：宽容解析（加粗/英文键/全角冒号/`3)` 编号行都能认）；
  **认不出等级就 `ParseOK=false`**，绝不猜默认值——猜成 good 等于把门关掉。
- `VLMVerdict.GateCheck`：good/excellent 通过，fair 记 warn，poor/bad 记 fail，
  **解析失败记 warn 且明确说"判词不可解析"**（不按通过处理）。
- `VLMGradeRank`：等级可比较序号，未识别返回 -1（聚合时排除，不当"最差"）。

**没做（明确边界）**：抽帧与 Ollama 调用、`gates.vlm.enabled` 接线、真机稳定性标定。
缺这三件不能说"质量门已升级"。

---

## [v2026.10.31-v3.0-p0] 架构升级 V3 立项 + 血缘层落地（内容寻址 + Manifest）

第一次把 FE 的**底层定位**写清楚（`docs/架构升级-FlowEdge-V3-总纲-20261031.md`），
并落地 P0 第一项：**内容寻址与产物血缘**。这是"从抽卡到生产线"的地基——
没有它，换权重/换参考图/换 seed 都不会让缓存失效，三个月后也无法复现任何一版。

**定位（写进文档，成为后续所有取舍的判据）**
> FE = 本地、单卡、**可验收、成本可承诺**的 AI 制片流水线。
> 不是节点工具（没验收），不是云平台（管线不可改），不是通用编排（不懂媒体）。

**新增能力**
- **`ArtifactKey` 内容寻址键**（`service/artifact_store.go`）：参数 + 模型指纹 + seed +
  上游产物摘要 + 代码版本 → 32 位 hash。8 个维度各自改变 key（`TestArtifactKey_Completeness`）。
- **模型指纹**：首尾各 1MB 的快速摘要（20GB 权重 <1s），纳秒 mtime 参与缓存判定，
  同尺寸替换也能检出；指纹落盘 `data/generated/model-fingerprints.json`，重启不重算。
- **`Manifest` 血缘清单**：每次出片落 `data/generated/manifests/run-<id>.json`，
  记模型/参数/产物 sha256/耗时/资源；失败运行也写（失败的血缘同样是证据）。
- **`GET /api/runs/:runId/manifest`**：有清单 200，旧运行 404 并**说明原因**（不返回空对象冒充成功）。

**修掉的坑**
- `data/pipelines.json` 带 UTF-8 BOM → handler 测试全红，报错误导性极强
  （`invalid character 'ï'`）。`LoadPipelines` 现在容错去 BOM **并记日志**，
  `TestLoadPipelines_ToleratesBOM` 钉住"容错但不吞错"。

**未验证（诚实清单）**
- `writeRunManifest` 的真机路径未跑（需 GPU + 几分钟出片）；单测覆盖了构造与 API。
- `shotCacheKey` 仍缺 seed/权重/参考图（已移到 P1：改键会让现有缓存全失效，须与真机 A/B 一起做）。

---


## 更早的版本

v2.5 及以前（画布时代、轻装化、自进化那套）的条目留在了 git 历史里：
`git log --oneline CHANGELOG.md` 能翻到。那些功能大多已经删掉，留着只会误导。
