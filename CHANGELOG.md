# Changelog

All notable changes to FlowEdge are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
date-based versioning (`vYYYY.MM.DD`).

## [v2026.10.21]

> 主题：把「只在我这台机器上跑得通」变成「装得上、接得进、测得准」。
> 六个阶段（P0–P5）共用一块地基：模型不再写死在代码里。

### P0 地基：模型 bundle + 提交前预检 + 后端抽象

- **`data/models.json`（模型 bundle）** — 权重文件名与默认参数（steps/cfg/分辨率/帧数）
  从代码搬进 JSON。此前硬编码在 `comfy_media.go` 的 8 处：Wan 的 unet/clip/vae、H3 的
  fl2va/ref2va 两个 checkpoint 与两个 VAE、超分模型、flux 的 4 步蒸馏参数。
  换任何模型都要改代码重编 32MB 的 exe，而且**别人机器上没有同名文件时开箱即失败**，
  报错还是 ComfyUI 的 `400 prompt_outputs_failed_validation`。
  现在：`GET/PUT /api/models/bundles`，只写想覆盖的字段，其余继承内置默认。
  内置默认与改造前的硬编码**逐字相同**，有测试锁死（`TestBuiltinBundlesMatchLegacyHardcode`）。
- **`/object_info` 提交前预检** — 把「权重名写错 / 节点没装」拦在提交前，
  而不是等 ComfyUI 返回 400。**探不到 object_info 不阻断**（老实例、自定义节点包返回
  畸形 JSON、代理劫持都会让探活失败），但 400 时会附上「提交前预检未执行：<原因>」——
  「没预检」与「预检通过」必须能区分。
- **LLM 后端抽象** — 补全链路本来就是 OpenAI 兼容格式（`/v1/chat/completions`），
  但代码里处处号称 ollama：错误文案写 `ollama request failed`、视觉硬编码 `/api/chat`、
  卸载走 `/api/generate`。现在显式区分 `ollama` / `openai` 两种后端：
  vLLM / SGLang / LM Studio / llama.cpp server / 任意 OpenAI 兼容端点开箱可用；
  vision 按后端走 `images` 数组或 `image_url`；卸载能力不可用时显式报「该后端不支持」，
  不假装成功（`LLM_PROVIDER=openai|ollama` 可强制）。

### P1 广度：四个接入点（不是四百个连接器）

- **OpenAI 兼容出口** — `/v1/images/generations`、`/v1/audio/transcriptions`、
  `/v1/audio/speech`、`/v1/chat/completions`（含 SSE 流式）、`/v1/models`。
  Open WebUI / SillyTavern / Chatbox / LangChain 把 Base URL 指到
  `http://127.0.0.1:8080/v1` 即可用本地 GPU 对话、出图、转写、配音，客户端零改动。
  边界写在代码注释里：这些端点不做 DAG 编排、不经显存仲裁。
- **`comfy_workflow` 节点** — 直接执行 ComfyUI「导出 (API)」的 workflow JSON。
  UI 格式（含 nodes/links）会**明确拒绝并告知怎么导出**，而不是猜着转换。
  绑定语法逐条校验：目标节点不存在、改 `class_type`、变量缺失都报错，不静默跳过。
  数值字段自动转 number（ComfyUI 收到字符串 `"1024"` 会 400）。

### P2 深度：显存档案 + 提交前估算

- **`data/vram_profiles.json`（显存档案）** — 每次重节点执行时在**执行窗口内**局部采样
  `nvidia-smi`（1s 一次，空闲不烧 CPU），按「模型 × 分辨率 × 帧数 × 步数」累积实测峰值。
  相近档位合并（样本加权平均），避免档位爆炸。
- **`POST /api/estimate`** — 提交前给出峰值与耗时**区间 + confidence**。
  三条硬纪律：不跨参数外推；样本 <3 时降级 confidence；采样漏峰时标 `peak_underestimated`
  并用 `source=model_size` 区分「测到的」与「算出来的」。
  `peak_mb` 是期望值，`peak_max_mb` 是上界，`fits` 按上界判（保守）。

### P3 深度：多实例路由

- **`data/instances.json`** — 模型 → ComfyUI 实例路由。H3 需要 torch 2.14 的独立工作树，
  flux/Wan 在 torch 2.6 的主实例上，两者在同一张卡上互斥。这套知识此前只存在于
  `AGENTS.md`，用户得手动改 `H3_BASE_URL`、手动停一个实例，搞混了只得到一句 OOM。
  现在：通配符匹配模型名 → 实例；`exclusive_group` 相同的实例自动串行。
  默认配置与改造前行为逐字一致（8188 主 / 8189 H3），有测试锁死。

### P4 广度：MCP server + 运行期插件

- **MCP server（`FlowEdge.exe --mcp`）** — stdio JSON-RPC，5 个工具：
  `flowedge_list_workflows` / `run_workflow` / `run_status` / `estimate` / `generate_image`。
  Claude Desktop / Cursor 可直接调工作流。架构是**瘦客户端 + HTTP 转发**：
  在 stdio 进程里再起一套引擎会抢同一张卡的显存和同一个 sqlite 文件。
  引擎没起时返回「请先启动 FlowEdge」而不是裸的连接错误。
- **运行期插件** — `data/plugins/<name>/plugin.json`，`http` 型（任何语言）或
  `script` 型（yaegi 进程内解释）。**加节点不再需要重编译 exe**。
  坏插件只记日志，不影响其余插件加载。示例见 `examples/plugins/text_stats/`。

### P5 深度：单镜重跑 + 精度降级

- **`POST /api/runs/:runId/shots/:index/reroll`** — 只重拍某一镜。
  关键：**不修改用户保存的工作流**，reroll 只写进这次调用的内存节点图，
  否则一次临时操作会把 `reroll: 3` 永久留下，以后每次运行都重拍第 3 镜。
  配套 `GET /api/runs/:runId/shots` 查看每镜状态。
- **精度降级档** — OOM 降级链从「分辨率 → 帧数 → 步数」改为
  **「精度 → 分辨率 → 帧数 → 步数」**。换精度不改变构图与时长，降分辨率会——
  用户对后者的容忍度明显更低。阶梯由 `models.json` 的 `ladder` 声明，
  默认为空（本机权重多为 fp8/int8，没有更低档），当前权重不在阶梯里时不动（不猜）。

### Fixed

- **`checkCodeSafety` 假阳性** — 旧实现用 `strings.Contains(code, "\"os\"")` 匹配黑名单，
  于是**注释里提到 `import "os"` 也会被拦**。插件示例的说明文字直接把示例插件拒了。
  改为解析真正的 import 语句（含 `import (...)` 块、别名导入），先剥行注释。
- **nil `*http.Client` 导致进程崩溃** — 服务用结构体字面量构造时 `client` 为 nil，
  `s.client.Post` 会 nil pointer panic。三个服务（T2I / LLM / LLMStream）都加了兜底客户端，
  让装配失误退化成「正常请求」而不是「崩溃」。
- **`/v1/images/generations` 先出图后校验格式** — 参数错也会白烧几十秒 GPU 才报错，
  已把校验提前。

### UI：把后端能力接到界面（2026-10-21 16:20–16:50）

上一轮盘完成度时发现一个结构性偏差：**这轮抬高了引擎天花板，没抬用户能看见的地板**。
前端只调 12 个端点，新加的 5 个能力（estimate / models.bundles / instances / shots / reroll）
在前端代码里 **0 引用**——对你用 curl 不是问题，对任何别人等于不存在。

补齐 4 项：

| 新增 | 位置 | 解决什么 |
|---|---|---|
| `RunEstimate` | 运行按钮左侧 | 点运行前显示「≈17.6GB · 35–58min」，悬停看依据与 confidence；**撞墙时按钮变红，点运行先二次确认** |
| `ShotReroll` | 执行面板选中 run 后 | 镜头列表（缩略图 + 状态 + 耗时），每镜一个「重拍」按钮 |
| `ConfigPanel` | 更多菜单 → 「配置 · 模型 / 实例 / 接入」 | 模型角色表单（9 个）、ComfyUI 实例表单、MCP 配置一键复制 |
| `GET /api/mcp/config` | 后端新增 | 生成含 **exe 绝对路径**的 MCP 客户端配置——路径只有引擎自己知道，让用户手拼必然填错 |

设计上的两个取舍：

- **没有媒体节点就不显示估算**：纯 LLM/HTTP 流程显示「估算」只是噪音。同理，没样本时显示「无实测样本」而不是编一个数字。
- **估算只在媒体节点参数变化时重算**：拖节点位置也会触发 `nodes` 变化，不 debounce + 不比 key 的话后端每秒被算几十次。

**真机验证（16:25–16:30，RTX 3090，峰值 17878 MiB = 72.7%）**

| 项 | 结果 |
|---|---|
| 跑之前估算 | `confidence: none, peak_mb: 0` —— 没样本不编数 |
| 跑一次 flux 512²（46.7s，实采峰值 18050MB） | 档案自动写入 `image / flux1-schnell / 512x512 / peak 18050 / samples 1 / source sampled` |
| 跑之后估算 | `confidence: medium`（样本 <3 降级）、区间 13537–22562、wall 35.0–58.3s |
| 配置面板数据链路 | `GET /api/models/bundles` 9 角色、`GET /api/instances` main+h3(exclusive)、`PUT` 保存生效并持久化 |
| 单镜面板降级 | 无分镜的 run 返回 404 + 「该运行里没有分镜节点」→ 前端静默隐藏整块（不弹错） |
| exe 重建 | 32.2MB；bundle 里 6 个新文案全部存在 |

顺手修的一处显示缺陷：估算依据原本输出「512x512 / 0 帧 / 0 步」——出图节点没有帧数和步数，
全部照抄看起来像数据缺失。改成 0 值不显示（`dimsText()`，有回归测试）。

**未验证**：UI 的像素级布局。浏览器截图通道被 10MB 上限卡住（`clip` 参数在本机不生效，
160×120 的 PNG 也报超限），所以只验证了「代码已打包 + 类型检查通过 + 每个组件发的请求真实返回正确数据」，
**没有在浏览器里逐个点过按钮**。

### 结构收口：节点元数据单表化 + 字幕落地（2026-10-21 17:00–17:35）

> 起因：系统级结构评估（`docs/系统评估-2026-10-21.md`）指出「节点元数据没有单一来源」，
> 加一个节点要改 9–14 处、编译器不报错。这一轮把判定类元数据全部收进一张表，并顺手
> 补上二维能力图里标为「短视频准入缺口」的字幕。

- **`service/nodespec.go`：节点元数据单一来源（28 类）** — 侧别 / 是否重节点 / 是否产媒体 /
  超时算法 / 超时上限，原来散在 4 处 switch 与 1 处手写 map：
  `nodeSide`（vram.go）、`isHeavyNode`（vram_sampler.go）、`mediaProducerTypes`（lineage.go）、
  `nodeTimeoutFor`（engine_v2.go）、超时上限里的 `if node.Type == "shotlist"`。
  现在全部读 `NodeSpecs[type]`。**加一个节点 = 注册表 1 处 + NodeSpecs 1 处**。
  超时算法（文生图按张数、H3 按段数、分镜按镜数×帧数）逐条搬过去，有测试锁住系数。
- **顺带修掉一个真 bug**：`comfy_workflow` 在侧别映射表里**缺失**整整一轮。
  它的执行体会 `enterSide(sideDiffusion)`，但调度器的 `nodeSide()` 不认它 →
  侧别感知调度把它和语言节点排在一起，真机表现是多付一次 ~180s 冷加载。
  现在声明为扩散侧 + 重节点，并被 `TestNodeSideMappingRegression` 覆盖到（此前被跳过）。
- **`engine_v2.go` 2,283 行 → 1,172 行**：16 个节点执行体（`exec*`）整体搬到
  `engine_v2_nodes.go`（1,067 行）。内核（DAG 调度 / 超时 / 事件 / 取消 / 注册表）与
  节点实现分开，改一个节点不必再滚两千行上下文。
- **血缘白名单补全**：`shotlist / text2video / image2image / inpaint / upscale / comfy_workflow`
  此前**全部没有血缘**——视频管线的主产物恰好都不在名单里，只有 3 类出图/配音/合成被记录。
  白名单改由 `NodeSpecs.Media` 派生，并加一道结构判断：若本节点产出的媒体文件名全部已出现在
  上游生产者产出里，判为搬运、不覆盖真正的生产者记录（避免 `select` 之类选择器把「谁做的」指错）。
- **新增 `subtitles` 节点（第 28 个内置节点）** — 视频 + 字幕来源 → 硬字幕成片，另存 `.srt` 便于复查。
  来源优先级：现成 `.srt` → 分镜表 `plan`（按每镜 `seconds` 排时间轴，台词取「身份：原话」里的原话，
  「无对白」占时间轴但不显示）→ 逐行 `dialogues`（按成片总时长均分）。
  烧录用临时目录 + 裸文件名，绕开 `subtitles=` 滤镜里 Windows 盘符冒号与中文路径的转义坑；
  ffmpeg 缺 libass 时给明确提示而不是抛一段 filtergraph 报错。前端已接线：
  节点面板、输出字段、依赖（ffmpeg）、默认值、属性面板。
- **测试自身的一处维护债**：`TestNodeRegistry_HasAllBuiltins` 原本手抄 27 个类型名，
  加节点忘补一行就红。改为与 `NodeSpecTypes()` 对账（并保留「不能被删空」的下限断言）。

**Verified（Measured）**

- `go test ./...` 全绿，测试函数 **317 → 330**；`go vet` 干净；`tsc --noEmit` 干净；
  `dist/FlowEdge.exe` 32.3 MB（含前端与语音脚本）。
- 真机烧录（`FLOWEDGE_REAL_SUB=1`）：2s 母片 + 中文 srt → 产物时长 2.00s、音轨保留、
  与原片 **PSNR 31.46**（`inf` 才说明滤镜空转），证明像素真的被改了。
- 端到端（真实 exe，`PORT=8099`，不碰用户 8080）：工作流 `start → subtitles → end` 两次运行
  - `dialogues` 路径：`source=dialogues`、2 条、`00:00:00,100 → 00:00:00,900` / `1,100 → 1,899`，
    产物 2.00s 且 PSNR 32.39；
  - `plan` 路径：`source=plan`、2 条（「无对白」镜被跳过但占时间轴），
    第二条起于 `00:00:09,150`（5s + 4s 之后），台词已去掉「身份：」前缀。
- 验证后已停 8099、删 `dist/data`；复查 8080 仍 200。

**未验证**

- 字幕时间轴取的是分镜**期望秒数**，不是成片实际帧数（帧数会吸附到 17k+5 网格，有百毫秒级偏差）。
  逐字级对齐需要 ASR 强制对齐，本轮未做。
- 目标机器是否编译了 libass：本机可用，别的 ffmpeg 若没有会报「未编译 libass」而不是静默失败——
  但这条降级路径**没有在缺 libass 的机器上真跑过**。
- 竖屏 9:16 与 BGM 侧链闪避仍未做（评估里同属「交付准入门槛」的另两项）。
- 属性面板只做了类型检查与组件级验证，像素级布局仍需人工点一眼。

### 交付门槛：竖屏 9:16 + BGM 侧链闪避（2026-10-21 17:17–17:35）

> 评估里「短视频三道准入门槛」的最后两道。字幕已在上一轮做完，
> 这三件事如果各做一个节点，就是三次 libx264 重编码——画质掉三轮。
> 所以做成一个 `post_production` 节点，一条 `filter_complex` 一次做完。

- **新节点 `post_production`（第 29 个内置节点）**
  - 构图：`aspect` 9:16 / 1:1 / 16:9 / keep，`fit` 可选 `blur`（模糊垫底，横屏改竖屏不丢内容）
    或 `crop`（中心裁切）；`width/height` 可显式覆盖
  - BGM：`-stream_loop -1` 把短于成片的音乐铺满，`bgm_gain` 默认 -12dB
  - 闪避：`sidechaincompress` 以人声为侧链，人声一响 BGM 自动退，`duck` 默认开
  - 字幕：可选，与竖屏/BGM 串在同一条链上（复用 `subtitles` 的三路来源）
  - 什么都不配时**不做重编码**，直接复制——无操作不该付一次画质损失
- **抽出共用逻辑**：`gatherSubtitleInputs` / `buildSubtitleFile` 由 `subtitles` 与
  `post_production` 共用。两份各写一遍必然漂移（一处改了「无对白」判定、另一处没改，
  同一份分镜会出两种字幕）。

**测试当场抓到的三个错（都不是推的，是跑出来的）**

1. **`amix` 默认 `normalize=1` 会把 BGM 压到听不见**：每路先被压低约 6dB，
   叠上侧链压缩后 BGM 只剩 -34dB——实测「660Hz 频段能量比母片还低」，即"混了但听不见"。
   改 `normalize=0` 后回到 -28dB。这条是 A/B 测出来的。
2. **`duck=false` 时保留 `asplit` 会直接失败**：旁路输出 `[sc]` 没人消费，
   ffmpeg 报 `Filter asplit has an unconnected output`（EINVAL）。
   不闪避时不再拆分音频流。
3. **侧链阈值不能拍脑袋**：660Hz 窄带扫了三组——
   `0.03/6` 压 8.4dB 但静音时也误压 3dB；`0.08/4` 只压 2.8dB（等于没闪避）；
   最终 `0.05/8`：有人声压 5.9dB、静音 0dB。

**Verified（Measured）**

- 测试函数 **330 → 338**，`go vet` 干净；`tsc --noEmit` 干净；exe 32.3 MB。
- 真机（`FLOWEDGE_REAL_POST=1`）：320×240 横屏 + 1s BGM + 字幕 →
  **1080×1920**、时长 2.00s（BGM 自动循环铺满）、音轨在、
  静音母片 660Hz 由 **-91.0dB → -36.1dB**（BGM 确实进来）；
  侧链 A/B：duck off **-33.0dB** / on **-38.8dB**（压了 5.8dB）。
- 端到端（真实 exe @8099，不碰 8080）：`start → post_production → end` 一次 success，
  产物 `1080x1920 / 2.000s / 双流`，660Hz 由 -46.4dB（母片底噪）升到 -38.8dB。
- `TestNodeSpecsCoverRegisteredTypes` 当场拦住"注册了节点但忘了登记元数据"——
  上一轮刚立的两处约定第一次生效就抓到了真漏项。
- 验证后已停 8099、删 `dist/data`；8080 复查 200。

**未验证**

- `blur` 垫底在真实 4K 素材上的耗时（测试片只有 320×240；模糊半径固定 24，未按分辨率缩放）。
- 侧链参数是按 440Hz 纯音 + 语音级电平扫的，**真实人声（含齿音、停顿）未测**。
- 竖屏裁切后主体是否居中：源片是测试图案，真实片子需要人眼确认构图。
- BGM 与成片的响度归一（`loudnorm`）仍未接入后期链，只做了相对增益。

### 前端测试从 0 到 28（2026-10-21 17:34–17:55）

> 评估里 S4 是"全项目最薄的一环"：前端 9.7k 行、0 测试，改 UI 只能靠手点。
> 这一轮先建安全网，**再**拆文件——反过来会没有回退依据。

- **零新增运行时依赖的测试通道**：`frontend/scripts/run-tests.mjs` 用 vite 自带的 esbuild
  把 `src/**/*.test.ts` 打成单个 ESM 包，交给 `node:test` 跑。只加了 `@types/node`（devDependency，
  不进 exe）。`npm test` 一条命令，已接进 CI 的 frontend job。
- **27 个用例分三组**
  - `graphUtils.test.ts`（9 个）：批注/分组剔除、连线过滤、分层布局（含"取最长路径"、
    环内节点不丢不堆原点）、包围盒（含缺测量值不产生 NaN）、ID 生成。
  - `graph.test.ts`（9 个）：**前后端契约测试**——从 `backend/service/nodespec.go` 解析节点类型，
    断言每一个都出现在前端调色板里；反向断言"调色板没有幽灵节点"；输出字段/依赖表只登记已知节点；
    依赖键必须有中文提示；预置模板的节点与连线两端都真实存在、模板 id 不重复。
  - `nodeDefaults.test.ts`（5 个）：调色板每种节点都有默认 data、默认值不出现 `undefined`
    （唯一例外 `workflow_id` 有注释说明）、字幕/后期节点的关键字段与默认值（后期默认 9:16 + 模糊垫底 + duck 开）。
  - `errorHints.test.ts`（5 个）：从 `backend/model/workflow.go` 解析 `ErrorCode*` 常量，
    逐个断言有中文提示而不是落到兜底；`isRetryable` 与 `retry-node` 动作自洽。
- **顺手拆掉 App.tsx 里 97 行纯数据**：`getDefaultNodeData` 移到 `src/config/nodeDefaults.ts`。
  `App.tsx` 3,226 → **3,129** 行。抽它的理由不是行数好看，而是它现在是**可测的纯数据**
  （见上面第三组用例）；属性面板与画布逻辑没动，重构风险为零。
- README/AGENTS 的构建命令同步：`go test` 338、`npm test` 28、`tsc --noEmit` 均列出。

**Verified（Measured）**

- `npm test`：**28 passed / 0 failed**；`tsc --noEmit` 干净；`vite build` 通过（553KB bundle）。
- `go test ./...` 仍全绿（338 个）；`go vet` 干净。
- 冒烟：新 exe（32.3MB）起在 8099，`/` 200、bundle 200 且含「字幕烧录」「后期包装」文案；
  验完停服、删 `dist/data`，8080 复查 200；`运行/FlowEdge.exe` 已替换。

**未验证 / 未做**

- 组件级测试（React 渲染）一个都没有——本轮只覆盖纯函数与配置契约。
  真正会腐坏的是那 3,129 行 JSX，这需要 @testing-library + jsdom，是下一轮的事。
- 属性面板仍然只能人眼确认；本机截图通道不可用（见本文件 16:20 的记录）。
- S3（`handler/workflow.go` 的 123 行编排逻辑）未动。

### Verified

- `go test ./...` 全绿；测试函数 **306**（改造前 212）；`go vet ./...` 干净；
  前端 `tsc --noEmit` 干净。
- 用真实 exe 在 8099 端口启动验证（不干扰用户 8080 实例）：
  `/api/health`、`/v1/models`、`/api/models/bundles`（9 个角色）、`/api/instances`、
  `/api/vram/profiles`、`POST /api/estimate`（空档案时 confidence=none，不编数字）、
  `/api/capabilities`（30 个节点类型，含 comfy_workflow）。
- 模型热改验证：`PUT /api/models/bundles` 改 Wan 的 unet + steps，
  读回时 clip/vae 正确继承内置默认。
- MCP 用真实 exe 验证：`initialize` → 2024-11-05、`tools/list` → 5 个工具、
  `tools/call flowedge_list_workflows` 成功转发。

### 验证轮（2026-10-21 15:45–16:10，零 GPU 占用）

用户当时在跑其他 GPU 任务，所以这一轮**不启动任何模型**：全部用 mock ComfyUI（httptest）
与 mock OpenAI 上游（Python stdlib）在 HTTP 层验证引擎决策，再用真实数据打自己。

**验证中发现并修掉的 3 个问题**

1. **绑定到已有连线的输入会静默破坏图**。API 图里上游连接是 `["6", 0]`；`ApplyBindings`
   无条件覆盖成字符串后，ComfyUI 把它当字面量，上游节点变孤儿——图提交成功、输出完全不对。
   现在检测链接形态并拒绝，错误里说明「要断开连线请先从工作流里删掉该连线」。
2. **降级档位虚报一档**。`generateMediaWithDegrade` 在最后一次重试失败后仍执行 `level++`，
   但那一档并没有机会执行；用户按 level 判断画质会判断错。已改为「用掉全部重试次数就不再降」。
3. **`comfy_workflow` 的「吃社区 workflow」说法不成立**（见下）。已修正对外表述。

**关键事实：ComfyUI 官方模板库全是 UI 格式**

本机 `D:/AI/ComfyUI/blueprints` 共 **89 个**官方模板，实测：

| 格式 | 数量 |
|---|---|
| UI 格式（含 `nodes`/`links`） | **89** |
| API 格式（可执行） | **0** |

而且它们全部带 `definitions.subgraphs`——节点 `type` 是 UUID 而非节点类名，需要递归展开子图
才能变成可执行的 API 图。因此：

- 「直接把社区的几万个 workflow JSON 吃进来」**不成立**，必须由用户在 ComfyUI 里
  Export (API)。已从 README 删除该表述。
- 检测器对真实数据有效：`TestParseRealBlueprints` 跑完 89 个文件，**89/89 正确拒绝、0 泄漏**
  （泄漏 = 把 UI 格式当 API 格式放行 → 提交空图 → 得到一句看不懂的成功）。
- 错误信息现在区分「普通 UI 格式」与「含子图定义的 UI 格式」——后者不是换个导出方式
  就能解决，得先展开子图，提示必须说清。

**已验证（mock / 真实数据，零 GPU）**

| 项 | 证据 |
|---|---|
| Wan 全链路：构图 → 提交 → 轮询 → 下载 → 落盘 | 提交的图含 bundle 里的 `unet_name`、正确 `width`、`SaveVideo` 节点 |
| OOM 降级（无精度阶梯时） | mock 返回 `CUDA out of memory` → 重试 1 次，宽度 608 → 456（×0.75 对齐 8），帧数不变，note 写清降了什么 |
| 精度阶梯降级 | 配两档 ladder → 重试时 `unet_name` 换成低精度档，**分辨率不变**（换精度不改构图） |
| 降到底 | 一直 OOM → 尝试 `level+1` 次后如实返回原始 OOM 错误，`level` 不超过 3 |
| 多实例路由 | 两个 mock 实例，`minimax_h3*` 命中 h3、`wan*` 命中主实例；同时把 `H3BaseURL` 指向死端口，证明路由按配置走而不是靠那个字段 |
| 互斥组串行 | 同 group 两个实例，第二次进入等待 ≥140ms（第一次持锁 150ms） |
| OpenAI 兼容（真机 exe + mock 上游） | `/v1/models` 返回真实模型；非流式 chat 把 system+user 压平后转发；流式 SSE 首块带 `role`、增量 content、`finish_reason:stop`、`[DONE]`；空 messages / `n>8` / 空 input 均返回标准错误体 |

**真机端到端（2026-10-21 16:00–16:06，RTX 3090，显存峰值 19677 MiB = 80.0%）**

`comfy_workflow` 用真实 ComfyUI 0.31（8188，833 个节点）跑通两次：

| 次 | 绑定 | 产物 | 结果 |
|---|---|---|---|
| run 1 | `6.text={{prompt}}` + `3.seed=42`，prompt = "a red apple on a wooden table" | `flowedge-e2e_00001_.png` 512×512 327KB | 红苹果木桌 |
| run 2 | 改 prompt = "a blue ceramic teapot, studio lighting" + `3.seed=77` | `flowedge-e2e_00002_.png` 512×512 268KB | 蓝色陶瓷茶壶 |

**两张图内容完全不同**——这是绑定真的生效的铁证，不是「跑通了但用的 JSON 原值」。
节点输出 `bindings_used: ["3.seed", "6.text"]`，`count: 1`，产物已下载到 `data/generated`。

顺带确认：提交前预检在真实 `/object_info`（833 节点）下放行了这张图（没误拦），
第二次运行 8 秒完成（模型已驻留，验证了「少一次驱逐 ≈ 省 180 秒」的实际效果）。

显存全程 1–2 秒采样：**峰值 19677 MiB / 24576 = 80.0%**，无一次超过 90% 阈值。
完成后回落到 2528 MiB，用户原有进程不受影响。

**仍未验证**

- **带自定义节点包的社区 workflow**：真机跑的是标准节点（flux t2i），未拿到带自定义包的样本。
- **第三方客户端 UI 联调**：协议层已通，未在 Open WebUI / SillyTavern 界面里点过。
- **精度降级在真实多精度权重上**：本机只有单档权重，mock 用的是虚构的第二档文件名。

## [v2026.10.20]

### Measured (2026-10-21, RTX 3090 24GB / flux1-schnell-fp8 @512×512)

These are measured numbers, not estimates. They decide where optimisation effort is worth
spending, so they are recorded here rather than left in a session log.

| Scenario | Time | Note |
|---|---|---|
| First image after model eviction | **183.3 s** | includes loading 17 GB of weights |
| Subsequent images, weights resident | **3.0 s** | same 512×512, same 4-step sampler |
| Side-switch benchmark, 2 diffusion + 2 language, grouped | 166.6 s, 1 logical switch | |
| Side-switch benchmark, same graph, ungrouped | 158.4 s, 2 logical switches | |

**Cold load is 98% of the first image's wall time (61× the actual generation).** That
single ratio reorders the whole optimisation backlog: saving one model eviction is worth
~180 s, while tuning sampler steps or resolution moves single-digit seconds. It also means
a "logical side switch" is not the same thing as a real eviction — the engine now counts
both separately (`SideSwitchCount` vs `SideUnloadCount`), because only the latter costs
that 180 s.

The 2+2 benchmark above did **not** get faster with grouping (−8.2 s is inside GPU
warm/cold noise at this size). That is the honest result: at two nodes per side there is
only one eviction to save, and the noise floor is larger than the saving. Grouping pays off
as `2NM/(N+M)` grows — with six nodes per side the ungrouped expectation is ~6 switches vs
a fixed 1 when grouped. Reproduce with
`FLOWEDGE_REAL_BENCH=1 go test ./service/ -run TestReal(SideSwitchBenchmark|ImageLatencySplit) -v`.

### Added
- **VRAM coexistence: skip the eviction when the model actually fits** — the arbiter used to
  evict the diffusion model on *every* switch to the language side. Measured: evicting flux
  costs **183.3 s** to reload while generating an image takes **3.0 s**, so each unnecessary
  eviction burns ~180 s. `canCoexistWithLanguage` now checks the real free VRAM
  (ComfyUI `/system_stats`) against the model's size (Ollama `/api/tags`) and skips the
  eviction when `free > size × 1.3 + 1 GB`. flux 17 GB + a 6 GB VLM ≈ 23 GB, which does fit
  in 24 GB — that combination no longer pays the reload.
  The conservative rule is unchanged: **unknown VRAM, unknown model size, or the kill switch
  (`SetVRAMCoexist(false)`) all fall back to evicting.** A wrong "it fits" pushes the resident
  diffusion model out and costs another 183 s — strictly worse than not optimising, so
  "uncertain" always means "don't coexist".
- **`CoexistCount()`** — how many evictions were avoided. Each hit is worth ~183 s, so this
  is the number to watch when judging whether the optimisation is doing anything.
- **Reproducible real-hardware benchmarks** — `realbench_test.go` measures (a) grouped vs
  ungrouped scheduling on the same fan-out graph and (b) the cold-load/generation split.
  Both skip unless `FLOWEDGE_REAL_BENCH=1`, so CI stays hermetic.
- **`SetSideAwareScheduling(bool)`** — escape hatch for the side-aware scheduler; turning it
  off restores the previous "launch everything at once" behaviour, which is also what the
  A/B benchmark uses as its control.
- **Deep health checks for the local speech services** — `/health?deep=1` no longer
  just reports "the process is alive". TTS synthesises a throwaway sample end to end;
  ASR verifies that every module it needs can actually be imported, including
  transitively-required ones. Previously a service could answer `/health` with 200 and
  still return 500 on the first real call (`No module named 'ordered_set'`), so a run
  wasted 100+ seconds before surfacing an error that was knowable up front.
  Old service builds that do not understand the query string fall back cleanly to plain
  `/health`, so upgrading the engine alone does not break a running service.
- **`Degraded` is now a first-class dependency state** — separate from "not running".
  A degraded dependency reports the exact `pip install` command and is deliberately
  *not* marked one-click-fixable: restarting a process that is already alive never
  fixes a missing package.
- **`ordered_set` pinned in `requirements.txt`** — it is a transitive dependency of
  faster-whisper that is not always installed automatically; the service starts fine
  without it and only fails at transcription time.
- **P0 Observability** — long multimodal nodes used to render as a spinner with no
  signal. The engine now emits a `node_progress` heartbeat every 2s (`elapsed_ms` /
  `eta_ms`) while a node runs, and stamps `duration_ms` on `node_finished`,
  `node_failed` and cache hits. ETAs come from a per-node-type EWMA of observed
  durations and return `0` when there is no history — never a fabricated number.
- **P1 Orchestration depth** — `SetMaxParallel(n)` caps how many nodes of one
  topological layer may run at once (0 = unlimited). Independent branches still run
  concurrently; heavy nodes stay serialized by the VRAM arbiter.
- **P2 Scene lock for image nodes** — `text2image` / `image2image` now resolve and inject
  the SCENE LOCK compiled by an upstream `scene_grid` node (or a hand-filled
  `scene_lock` / `scene_grid` field). Video was already covered; storyboards and
  character sheets were not, which is why the same character drifted across shots.
  Injection is pass-through: a prompt that already carries a lock is left byte-identical,
  so the `h3.scene_lock_missing` / `h3.scene_lock_drift` gates stay meaningful.
- **P2 Actionable gate fixes** — every failing `quality_gate` check now carries a `fix`
  string naming the exact knob to turn (raise `width` from 640 to ≥1280; raise `frames`
  to ≥124; add `-shortest`; drop the empty TTS segment…), and the blocking error text
  includes them. A gate that only reports a code hands the debugging cost right back.
- **P3 VRAM degrade-and-retry** — OOM is a capacity signal, not bad luck. Generation now
  releases diffusion VRAM (waiting for the asynchronous CUDA free), then steps down
  one notch at a time — resolution ×0.75 (8-aligned) → frames ×0.7 → steps ×0.6 — up to
  twice. Retrying in-process is an order of magnitude cheaper than failing and
  reloading 34GB of weights. Degradation is recorded in the node output
  (`degraded` / `degrade_level` / `degrade_note`).
- **P3 Dependency health check with one-click fix** — `POST /api/preflight` checks the
  current canvas without running it and returns each missing dependency with a concrete
  fix action and whether the engine can start it for you (`fixable` + `fix_dep`).
  The UI shows the report with a 一键修复 button per fixable item.
- **Text-to-image Chinese auto-translation** — when a node enables `auto_translate`,
  the engine translates CJK prompts to English with an LLM *before* they enter the
  diffusion stage (keeps GPU-side model loads from thrashing). Output carries
  `translated` / `translate_error` fields. Default off.
- **ASR tiering via node fields** — the `asr` node now forwards `model` / `device`
  to faster-whisper. The Python ASR service keeps a per-(model,device) cache and
  enforces a device allowlist (rejects `rocm`, etc.).
- **Inpaint model override** — the `inpaint` node accepts a `model` field to swap
  the default checkpoint (e.g. a dedicated Flux-Fill inpainter) instead of the
  weak default修补 path; empty = default.
- **Linux / macOS one-click run** — `scripts/run-local.sh` builds on demand
  (only when source is newer than the binary), launches in the background, health
  polls `/api/health` (bypassing the local proxy to avoid false 502s), and supports
  `--stop` / `--logs` / `--foreground` / `--port`.
- **`quality_gate` node** — five-layer gate (timing / identity / audio / technical /
  budget) that fails the run when any hard check is violated, so bad outputs never
  ship silently.
- **Parameter validation & dependency preflight** — node inputs are validated before
  execution; missing external services (ComfyUI / Ollama / ASR / TTS) are detected up
  front with actionable messages instead of mid-run crashes.
- **VAE compatibility matrix** — `comfy_compat.go` maps checkpoints to the correct VAE
  so cross-framework loads don't silently produce broken tensors.
- **English README, CONTRIBUTING, CI workflow, LICENSE (Apache-2.0)**.
- **`build-windows.py` cross-platform** — `--goos` / `--goarch` / `--output` reuse the
  same frontend + local-service embedding for Linux/macOS cross builds.

### Fixed
- **VRAM arbiter could be created twice under parallel layers** — `arbiter()` did an
  unlocked lazy init, so the *first* batch of heavy nodes after engine start
  (which the scheduler launches concurrently) could each allocate their own
  arbiter and each acquire their own `heavyMu`. Two diffusion nodes would then
  enter ComfyUI simultaneously and OOM a 24GB card. The lazy init is now
  mutex-guarded. Caught by `TestHeavyNodesSerializeOnVRAMArbiter`, whose
  concurrency probe made the bug surface as an intermittent failure —
  the scheduler sometimes got lucky and interleaved without overlap.

- **Stale `running` runs never recovered** — `queue.go` `RecoverInterrupted` now also
  sweeps the `workflow_runs` table, not just the task table, so interrupted runs are
  correctly marked failed instead of hanging forever.
- **Empty smoke-test database** — the 0-row `data/flowedge.db` shell was renamed;
  51 redundant smoke workflows purged from the real DB.
- **Dependency mismatch crashes** — preflight now fails fast with a clear message.

### Verified
- `go test ./...` green (189 in `service`, 192 total across packages); frontend `tsc --noEmit` clean.
- `TestHeavyNodesSerializeOnVRAMArbiter` / `TestParallelBranchesOverlap` pass under `-count=5`
  (concurrency probes are timing-based; repeated runs were used to rule out flakiness).
- Linux `GOOS=linux GOARCH=amd64` cross build produces a 31 MB CGO-free ELF.
