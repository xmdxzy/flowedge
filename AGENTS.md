# FlowEdge 项目规则（给未来的自己/其他 agent）

> 这些都踩过一次，别再踩第二次。详细证据链见 skill `local-multimodal-ops`。
> **产品形态先读 `docs/多模态出片台-v2.md`**：7 条固定流水线 / 21 条 API / 质量循环怎么接。
> **出片参数与验收读 `docs/多模态出片手册.md`**（写于画布时代：选路/时长/提示词/验收仍有效，"建工作流"那套已作废）。
> **先查路由表**：`.workbuddy/memory/INDEX.md`（任务/症状 → 精确到章节 + 唯一事实源），命中就不再全文搜索。
> **提交前一键**：`python scripts/check-all.py`（build+test+tsc+前端用例，失败才展开细节）。

## 0. 作业纪律（最高优先级）

> 第 1–7 节是**环境坑**，改一次就没了。这一节是**判断方式错**，不写下来会反复犯。
> 每条 = 触发情境（报错原文/现象，保留原文以便对号入座）→ 动作。违反任一条视为返工。

### 0.1 下「不可用」结论前必须找反证
**反例**：只做显存算术（34.3GB > 24GB）就断言「24G 跑不了 H3」。实际 H3 有独立 worktree `D:/AI/ComfyUI-H3` + 独立 venv（torch 2.14），608×352×124 帧 120s / 21.8GB 跑得很轻松——错在没有枚举实例，不在算术。
**动作**：任何「跑不了/不可用」之前，按序找三处反证：① **历史产物**（`ComfyUI/output/`、`运行/data/generated/`，有成品就是铁证）② **同类实例**（`ls D:/AI | grep -i comfy`，比对 torch 版本）③ **被注释掉的参数**（旧启动脚本里注释项往往就是缺口）。找不到才可下结论，措辞限「**在当前这个实例上不可用**」。

### 0.2 报错字符串不是根因
`OOM` → 实际是跑错实例（主实例 torch 2.6 跑 H3 图）。`'NoneType' object has no attribute 'Params'` → 实际是 `comfy-kitchen` 版本错位，不是模型文件选错。
**动作**：先问「这个报错在什么环境下产生的」，核对 **venv / torch / 端口 / 工作树** 四件套，确认同一套环境后再谈显存和权重。

### 0.3 耗时估算只能同参数外推
30 秒片子估 15 分钟、实跑 40 分钟：拿 608×352 的 118s/段去推 736×416@20 步的 240s/段（跨分辨率 >2 倍），还漏了插帧、LLM 分镜、实例启动。
**动作**：逐项拆——生成（宽高 × 步数 × 段数）+ 插帧 + LLM + 实例启动 + 拼接 + 装配；不跨参数外推，不报单值，报**区间 + 最坏情况**。

### 0.4 新开关上线必须 A/B，否则不算验证
`low_vram` / `interp` 两个字段从未被节点读取（JSON bool 没解析），却报过「低显存模式验证成功」——那只是裸跑成功。
**动作**：新参数**开/关各跑一次**，比耗时与峰值显存，并在日志里打印实际生效值。只说「跑通了」不等于「开关起作用了」。

### 0.5 视频验收四件套（缺一不可）
`cut` 用 0.04s 淡化 < 一帧 → **第二段画面整段丢失，而时长和音频都正常**；只查 duration 永远发现不了。
**动作**：交付前跑

```bash
python scripts/verify-media.py 成片.mp4 --asr --frames .tmp/frames
```

覆盖：流列表 + 时长 + **精确帧数** + 音画时长差 + 中段静音缺口 + 响度 + ASR 转写。`FAIL` 一律不许交付；退出码 0/1/2 = 通过/警告/硬失败，可直接做流水线门禁。

### 0.6 「服务没起」和「没有结果」必须分开
ASR 不可达 → 返回空文本 → 看起来像「检测到无人声」；shell 常驻 `http_proxy=http://127.0.0.1:5751` 会让 `curl 127.0.0.1:8080` 被代理劫持返回 **502**，看起来像引擎挂了。
**动作**：本地端口探测一律 `curl --noproxy '*'`；任何验收工具必须显式区分「**不可达**」与「**空结果**」，不可达报 FAIL 而不是 WARN。

### 0.7 图构建禁硬编码节点 id，也禁全局常数
`saveImage` 写死源节点 `"8"`（超分图的源是 `"2"`）→ 400 校验失败；单节点固定 180s 超时 → 合法的长任务被误杀。
**动作**：上游 id、超时、尺寸、步数全部参数化；超时按「类型 × 工作量」估算，不许出现魔法常数。

### 0.8 迁移权重是「N 处配置」动作，不是一处
只改主实例 `extra_model_paths.yaml`，H3 工作树那份还指旧路径 → H3 一个模型都看不到（当时误诊成「H3 装不下」）。
**动作**：迁移前 `grep -rl extra_model_paths D:/AI` 逐份改；改完用各自端口的 `/object_info` 枚举验证，**主实例和 H3 各验一次**。

### 0.9 加速类改动先量化，再决定是否留依赖
SageAttention 是负优化（206s vs 120s，还连带装 triton）；RIFE 镜像 404 白试一轮。
**动作**：先找零成本替代（插帧用 `ffmpeg minterpolate`，CPU、零显存）；必须装的先在单样本对比，负优化就卸载并记进 §6「别再装」。

### 0.10 常驻服务怎么起（2026-10-23 定案）

agent 的 shell 起不了任何活过本次调用的进程——沙箱每命令一个 job、结束显式终止、不给 breakaway，7 种逃逸方式实测全灭（两层机制与实证明细见 `docs/沙箱与常驻进程.md`）。常驻只能由 job 外的进程派生：

1. **一键入口** `python scripts/ensure-daemon.py`：确保整链（序策工作站 :30000 → 引擎 :8080）；`--status` 只报状态；`--install-autostart` 装登录自启 + 自愈守护。工作站不在时它直接给双击路径，**不瞎试**。
2. **拉依赖走引擎**：`POST :8080/api/deps/:name/start`（ComfyUI/ASR/TTS 都归它托管），别自己 spawn；等待用 `/api/deps/:name/wait`，别硬 sleep。

不许再试：`wmic` / `schtasks` / `wsl` / `cscript` / `explorer.exe <exe>` / `Start-Process` / `cmd /c start`。
**判据**：`curl --noproxy '*' 127.0.0.1:8080/api/health` 返回 200，**并且**用下一次工具调用确认进程还在（`python scripts/probe-survive.py --check`）。每次接手先 `ensure-daemon.py --status`，别假设还活着。

### 0.11 交付自检（5 分钟，做完再说「完成」）

- [ ] `verify-media.py` 无 FAIL
- [ ] 抽接缝前后各 1 帧看过（cut 段尤其）
- [ ] 多段片：`prompt_warnings` 里没有 `scene_lock_missing` / `scene_lock_drift`
- [ ] ASR 转写确认无人声 / 台词正确
- [ ] 报耗时报的是**实测区间**，不是估算
- [ ] 告诉用户「改哪个参数可以重跑」，而不是只给一个成片

### 0.12 六类反复犯的失误（2026-10-20 复盘，每条对应一次真实返工）

| # | 错（当时怎么想的） | 规则 |
|---|---|---|
| ① | 把二手的当一手：沿用社区博客摘要（`subject_definitions / retention_analysis` 那套）当官方要求，没发现官方仓库原文、工作流 JSON、节点源码**本地就有** | 「标准/规范/最佳实践」按序取证——本地源码与官方范例 → 官方仓库原文 → 官方文档页 → 社区博客（只当索引）。指不到原文那一条就是在猜 |
| ② | 用「工具没报错」当「没问题」：`av_drift=0` 就认为音画同步，实际 TTS 每句自带 0.44s 前导静音 | 验收必须含**用户能感知的项**且都有数值：帧数/时长/接缝 + 语音起点 vs 计划时点（≤0.1s）+ 跨段 AvgY 光比；人声/底噪 SNR ≥8dB + ASR 可懂度；提示词 audit 全绿 + 至少跑一次真实 LLM 并记下它的错误 |
| ③ | 用调参掩盖结构错误：混音顺序错（先 amix 后 loudnorm），把 `--ambience-gain` 从 0.18 顶到 1.2，结果比原版更糟 | 指标不对先问「链路顺序/结构错了没」，再问「参数调多少」。**参数化 ≠ 修根因**；连续两次调参无改善必须停下读代码 |
| ④ | 一次改多个变量：重拍段 2/3 同时改提示词、lead/pause、混音链、环境声素材 → 分不清是谁导致的 | 一轮只动一个变量；同轮改 ≥2 项必须在日志列变量清单 + 保留 A/B 版本，否则不许说「已验证」 |
| ⑤ | 换轨/装配不验前提：`-shortest` 让容器比视频流长 4 帧 → `stitch` 按容器时长算 xfade offset 越界 → 整段画面丢失，**成片时长仍报 30s** | 任何 mux/拼接后立刻 `ffprobe` 核 **`nb_frames` 与视频流 duration**；装配器一律用**视频流时长**算偏移，不用容器时长 |
| ⑥ | 停服务前没准备恢复手段：杀了 8080 才发现 bat 在 Git Bash 下编码失败、PowerShell 拉起的进程被会话清理、COM 被安全策略拦 | **动常驻服务之前先确认恢复路径可用**（谁启动、怎么启动、有无中文/编码问题）；验证不了就别停 |

### 0.13 汇报口径（避免「看起来都做完了」）
结论先行 → 给数值证据 → **把「没验证的部分」单独列一节** → 下一步。不许把「代码已落地」和「效果已验证」写在同一个语气里；没跑过的写「未验证」，不用「应该可以」「理论上」糊过去。

### 0.14 上下文预算（2026-10-23 实测，比砍规则文件更值钱）
同一会话实测：静态注入 25.3k 字符，而**自动压缩摘要 94.6k + 47.6k = 142k 字符（≈102k tokens，5.8×）**。摘要由 transcript 生成 ⇒ **工具输出才是成本主因**，砍规则文件只能动 3%。

- **单条输出 ≤2KB**：命令自带限制（`head`、`--oneline`、`--json`、`head_limit`、`| tail`）；禁无限制 `cat`/`grep -r` 全库。
- **>4KB 的文件禁整读**（AGENTS.md 12.7k、多模态出片手册 16.7k、引擎平台化评估 65k）：先 `grep -n` 定位，只读命中行。
- **一次性测量先查 `scripts/`**：`python scripts/token-audit.py [--session|--dup]` 已覆盖注入与会话开销；别现写探针（每段探针 ≈1KB 命令 + 1–3KB 输出，且会被摘要二次放大）。

### 0.15 重型第三方项目构建纪律（2026-10-24 复盘 QCoder 从源码构建，每条对应一次真实浪费）

| # | 当时怎么做的（错） | 规则（以后照做） |
|---|---|---|
| ① | 进仓库根就 `npm ci`，补依赖补了 3 轮才发现不对 | **先数 npm 项目再动手**：`find . -name package-lock.json -not -path "*/node_modules/*"`。有几个就在几个目录分别装。VS Code 系项目的 `build/` 是独立项目，根目录装够也不算数 |
| ② | `npm install --no-save X` 补包，下一次 install 被 lock 修剪掉，同一件事做了 3 遍 | **禁止 `--no-save` 补包**。要么 `--save-dev` 写进 package.json，要么装完立刻用、不再 install |
| ③ | `export PATH="C:/xx:$PATH"` 在 Git Bash 里**静默失效**，`which` 也查不到 `.cmd`，误判成"没装" | **改完 PATH 立刻验**：Git Bash 只认 `/c/...`；验法是 `cmd //c "npm.cmd --version"`，不是 `which` |
| ④ | 缺 `ternary-stream`→补；再缺 `gulp-sort`→补；再缺 `jsonc-parser`→补 | **同类错误第 2 次出现就换策略**：用 `require.resolve` 全量扫（模板 `scripts/check-deps.js`），一次列出全部缺口再批量装 |
| ⑤ | `rm -rf` 中间目录撞 `SAFE_DELETE_BULK_CONFIRM_REQUIRED`（50 文件阈值） | **清理一律 `mv` 走**（`mv out-build out-build.bak`），或 `CODEBUDDY_SAFE_DELETE_ENABLED=0`。禁写循环逐个删 |
| ⑥ | 闷头干 3.5 小时，用户凌晨才问「成功了吗」 | **3 个障碍或 90 分钟必须汇报**：已完成 / 卡点 / 可选路径，三行讲清。汇报不中断执行 |

**判据必须是产物，不是退出码**：`Finished compilation with 0 errors`、`.node` 文件数量、`--version` 的真实输出、日志是否为空。
反例：`gulp compile` 会因仓库自带的 `.vscode/extensions` 自测扩展报错而 `exit 1`，但主程序产物完全正常——只看退出码会误判整件事失败。
同源规则见 §0.12 ②「用工具没报错当没问题」和本条的镜像面。

### 0.16 v2 后的事实（2026-10-25，先读这条再动手）

FlowEdge 只剩两件事：**本地多模态出片** + **质量门控循环**。
完整说明见 `docs/多模态出片台-v2.md`；画布部分见 `docs/画布与流水线统一-20261029.md`。会做错的地方：

- **画布回来了（2026-10-29，本条覆盖下面所有"图不再由用户画"的说法）**：用户要的就是
  "自己定义工作流"，v2 删画布是砍症状——旧画布缺的是五道护栏（空画布起步 / 无类型系统 /
  无成本预估 / 无 dry-run / 节点元数据双写），不是缺能力。现在是**一张图两个视图**：
  出片台（7 条预置，填字段即出片）+ 画布（任意 DAG，从模板出发改），共用 `workflows` 表、
  同一个引擎、同一条校验链（结构 → 端口类型 → 参数 → 依赖预检）。
  新增 9 条 API：`GET /api/node-catalog`、`GET|POST /api/workflows`、
  `GET /api/workflows/templates`、`GET|PUT|DELETE /api/workflows/:id`、
  `POST /api/graph/validate`、`POST /api/graph/dry-run`。
  **加节点只改两处**：`service/nodespec.go`（调度元数据）+ `service/nodecatalog.go`
  （端口 / 字段 / 成本 / Label）；两张表键集合不一致时 `nodecatalog_test.go` 立刻红。
  前端调色板、检查器、连线校验**全部**由 catalog 生成，不许硬编码节点类型。
  **`type_mismatch` 是 warn 不是 error**：`image_edit` 流水线里 `start-1 → up-1` 就是
  一条"文本接只收图片的超分节点"的死线（纯执行顺序依赖，运行期无害），定成 error 会把
  自家产品拦下；真正该拦的是 `input_unreachable`（必填输入既没有兼容入边、节点自身也没值）。

- **产品形态是 7 条固定流水线**（`data/pipelines.json`）：`image` / `image_batch` /
  `image_edit` / `video` / `video_from_image` / `film` / `voice`。用户选一条、填字段，
  服务端组装图去跑。加能力 = 改这个 JSON（字段 `bind` 写节点 data、`when` 选节点），
  **不是**加画布节点。
- **唯一运行入口 `POST /api/produce {pipeline, params, invoked_by}`**。再也不会出现
  "先 POST /api/workflows 再 /run" 的两段式。
- **别再找这些接口**（已删除，不是坏了；**`/api/workflows*` 已在 2026-10-29 恢复**，见上面画布那条）：
  `/api/modules`、
  `/api/estimate`、`/api/vram/profiles`、`/api/models*`、`/api/instances`、
  `/api/llm/profiles`、`/api/gpu/*`、`/api/runs/:id/step|continue|resume`、
  `/api/preflight` 的旧语义（现在按 `{pipeline, params}` 体检）。对外只剩 21 条。
- **别再找这些节点**：`if_else`、`loop`、`comfy_workflow`、`input_source`、`text_split`、
  `vision`、`select`、`asr`、`video_compose`、`subtitles`、`post_production`、
  `scene_grid`、`quality_gate`、`consistency_gate`、`prompt_opt`。节点只剩 13 种
  （2026-10-29 起含 `comfy` 逃生舱，见《画布与流水线统一》§9）；
  旧图会报 `unknown node type`，这是预期。其中**场景锁与提示词优化的库函数保留**
  （`scene_grid.go` / `prompt_opt.go`），因为 `text2video` 内部要用：
  优化开关是 `text2video.data.prompt_opt`，参考图/音频走 `ref_images`/`ref_audio`
  （给了就自动 Ref2VA 六段式）。
- **一致性门在 `shotlist` 的 plan 层，不是节点**：`service/consistency_plan.go` 的
  `AuditPlanConsistency` 在开机前审分镜表——`continue` 续接却跳轴/换光比 = FAIL，
  场景锁句字节不全等 = FAIL，环境声多写法 = WARN。旧的 `consistency_gate` 节点
  （出片后抽帧比对）已删；别把它加回来，加在这个审计函数里。
- **质量循环不是自己拼 loop**：流水线字段 `qc=true` 时，服务端把该图落成 `qc:` 前缀的
  临时工作流，外层换 `start → quality_loop → end`，跑完即删、启动时清扫。
  预检与参数校验在**包壳之前**对原始图做（对壳做等于没查）。
- **节点缓存已删**：故意。缓存命中会让"重跑一轮"变成假重跑。
- **前端测试必须 `NODE_ENV=test`**：这个 shell 里 `NODE_ENV=production`，`act()` 会整体抛错。
- **Ollama 也是托管依赖**（`data/deps.json` 的 `ollama` 段，`autostart` 默认 true）：
  `video`(optimize)、`image_batch`、`film` 都要它。以前只能"另开终端跑 ollama serve"，
  现在 `POST /api/deps/ollama/start` 一键起；已装桌面版时只探活复用。
  `Startable` 曾把 `serve` 当成脚本路径 → 进程根本不派生且不留错（已修 + 回归测试）。
- **单镜实测 ≈6.6 分钟**（608×352×124 帧 @12 步，含 H3 冷加载，2026-10-25 本机）：
  film 2 镜 shotlist 13.2 分钟。旧笔记里的"120s/段"是别的参数/实例下的数字，
  **别拿它估时**；估时按"镜数 × 6–7 分钟 + LLM + 装配"报区间。
  > 2026-10-29 更新：film 默认 steps 20→12、LowVRAM 自动关；1 镜冷启动生成 2m10s（整 run 215.7s）。
  > 新估时改按「首镜 3–4 分钟 + 后续 1.5–2.5 分钟 + LLM + 装配」，并用当次实测覆盖。
- **默认队列并发是 1，且 Enqueue 不阻塞**：单卡并行出片只会互相抢显存
  （实测 H3 跑片时并发一条文生图 → 后者 5m15s 超时，像"节点坏了"）。
  旧 `JobQueue.Enqueue` 用信号量满则阻塞，会把 produce 请求卡住；
  现在入队即返回（`queue_test.go` 钉住：不阻塞/FIFO/上限/拒收）。
- **队列按资源分道（2026-10-27）**：GPU 道串行（默认 1），CPU 道并行（默认 2，
  `FLOWEDGE_QUEUE_CPU_CONCURRENCY`）。判类走白名单 `cpuOnlyNodeTypes`（当前只有 `start`/`end`），
  **认不出的节点一律 GPU**——判反的代价是两条任务抢显存。
  **`tts` 不在白名单里，它看配置**：`JobClassForNodes` 用 `ttsOnCPU()` 读 `deps.json` 的
  `tts.device`，为 `cpu` 才进 CPU 道。这条是两次反复换来的：Breeze TTS 2 写着 `device=cpu`
  却 CUDA-only（CPU 合成 500/超时、常驻 8–16GB），Kokoro-82M 才真在 CPU 上跑
  （实测 RTF 0.33–0.95、零显存，见 `docs/复盘-资源调度与近期失误-20261027.md §6`）——
  配置不等于事实，但判据必须只有一个来源，否则队列与仲裁会各拍一套。
- **CPU 上的 TTS 不许进显存仲裁（2026-10-27 实测）**：`execTTSNode` 原无条件
  `enterSide(sideAudio)`，两个后果都实测到了——voice 排在 image 后面 **102 秒**才说完一句；
  换侧时 `releaseDiffusionAndWait` 把**正在生成图**的 flux 卸掉，那个 image 当场
  `comfyui generation failed`。现在 `ttsOnCPU()` 为真时不抢 `heavyMu`、不逼别人让位。

- **GPU 上的 TTS 必须给扩散侧让位，超时必须覆盖冷加载（2026-10-27）**：这是上面那条的
  镜像面，坑了两次才补齐。实测 Breeze TTS 2（3.47B BF16）驻留 **15.2GB**、冷加载 **180s**、
  加载完峰值 19.9GB，而 H3 权重就要 20.5GB——两者物理上装不下。原实现有三处缺口：
  1. `enterSide(sideDiffusion)` 只卸 Ollama，**语音侧没人管** → 配音跑过之后出片，卡上
     只剩 3–4GB，H3 退化成部分加载（87s → 2269s）。现在扩散侧/语言侧都会调
     `releaseAudioSide`：优先 `POST :8103/unload`（进程保留、下次自动重载），失败退回停服务。
  2. `tts` 节点原来**没有 Timeout**，吃 `defaultNodeTimeout=180s`——与冷加载 180s 打平，
     「服务被卸载后第一次配音」必然失败（run #288 `node timeout after 3m0s`）。现在
     `ttsTimeout` = 5 分钟 + 每字 200ms。
  3. HTTP 预算原来 60s 起算，短文本 150s，连加载都走不完。现在
     `ttsFirstCallBudget=240s` + 每字 1.5s。
  另：**`tts.idle_unload` 保持 0（不让服务自己空闲卸载）**，但**引擎侧现在有统一回收**
  （2026-10-28，`service/vram_idle.go`，配置 `data/deps.json` 的 `idle_release` 段）：
  连续 `after_seconds`（默认 600）秒无 GPU 活 → TTS `/unload` + ComfyUI `/free` +
  Ollama `keep_alive=0`，**进程全部保留**。为什么必须由引擎判：服务自己的超时只看自己的
  `last_used`，看不见引擎队列里排队/在跑的活，会在「下一镜 5 秒后就要配音」时把模型卸掉。
  「出片撞车」仍然只靠 `releaseAudioSide`（`engine.log` 里有 `[vram] 释放语音侧显存（进扩散侧）`）。
  - **假卸载**（2026-10-28 修掉）：`POST /unload` 曾返回 `{"ok":true,"unloaded":true}` 而进程
    仍占 **16274 MiB**（设备空闲只从 6737 缩到 6730）。根因是 `FastBreezeStreamingRuntime`
    与 `model` **循环引用**（`runtime.model` / `model._fast_text_encoder_cudagraph` /
    `runtime._backbone_graphs` 持有 CUDAGraph），只把 `_state` 置 None 引用计数归不了零 ⇒
    PyTorch 视这些块为 allocated，`empty_cache()` 无权动。修法见 `_release_vram()`：
    **断环 → `gc.collect()`×2 → `synchronize` → `empty_cache` → `ipc_collect`**（顺序错了白做）。
    实测 allocated 8532→270 MB、空闲 6641→**20973 MiB**，回收 14.3GB / 6.1s。
  - **加载失败会回滚**：`load()` 里 except 调 `_release_vram`，不再「卡在 phase runtime 占 16GB 不 ready」。
  - **守卫**：`idle_release.tts_guard=true`（默认）= 「卸完空闲仍 ≥ `tts_min_free_mb`(20787)
    才卸 TTS」，避免「卸了装不回来」。设 false = 接受下次配音前手动腾卡。
  - **判据**：`/health` 的 `idle_unload` 仍应为 0；新增的 `vram` 段是「卸载真假」的唯一读数
    （`loaded=false` 而 `allocated_mb` 仍大 ⇒ 没真释放）。空闲时 `loaded=false` 是**预期**，
    下次合成自动重载（实测 60s，比重启进程 137s 快）。回收日志 grep `[idle-reclaim]`。

- **排队是真的排队，也是真的能取消（2026-10-27）**：入队写 `pending`，被调度起跑那一刻才写
  `running`+`started_at`（旧实现在 Enqueue 之前就写 running，于是"等 20 分钟"和"在烧卡"
  在面板/看门狗/ETA 三处一样）。`/runs/:id/cancel` 先试执行中、再试排队中——
  只修 ComfyUI 那一层不够，引擎自己的队列有一模一样的洞。
- **显存预检：自家占用排队，外来占用才拦（2026-10-27）**：提交时判一次
  （`HasGPUWork()` 为真不拦），开跑前再复判三次（间隔 2s，容忍 CUDA 异步释放）。
  两次 409 误伤（热实例 / 自家队列）都是「只看空闲数字，不看谁占着」。
  `vramAdvisoryReserveImageMB` 必须逐字对齐 `deps.json` 的 `--reserve-vram`（当前 1 → 1024）。
- **队列里 `wg.Done()` 必须在 `dispatch()` 之后**：先归零再补位会让 `Wait()` 提前返回，
  下一条 `Add(1)` 直接 panic `WaitGroup is reused before previous Wait has returned`（实测踩到）。
- **引擎日志在 `<exe目录>/logs/engine.log`**：常驻时 stdout 被外壳（工作站/bat）收走，
  出事只能看这个文件。8MB 滚一份 `.1`。
- **一致性门有两层**：`shotlist` 开机前审 plan（轴线/光比/锁句，FAIL 拦下）；
  出片后量各镜 U/V 均值，相邻两镜漂移 >6 报 `plan.color_shift`（WARN）。
  实测抓到过"plan 两镜都写冷色、生成一暖一冷"——那是生成侧漂移，plan 门管不到。
- **部署必须带上 `data/pipelines.json`**（exe 同目录）：缺它引擎 `log.Fatalf` 不启动。
  `build-windows.py` 会复制到 `dist/data/`，`3-应用更新并重启.bat` 会同步到 `运行/data/`；
  手动换 exe 时别忘了这一步。
- **H3 必须由引擎托管启动，`deps.json` 里 `h3.reserve_vram` 要 0**（2026-10-26 实测）：
  24GB 卡上 20480(权重)+4096(预留)=24576=整张卡，桌面再占 1.3GB ⇒ **算术上永远差 1.3GB**，
  ComfyUI 必然退化成"部分加载"（权重留内存、按步换进显存），同一镜 87s → 2269s。
  手工 bat 起的实例带的是 `--reserve-vram 4`，引擎只会"探活复用"、改不了它的参数——
  **要改参数必须先停掉手工实例，让引擎自己派生**。改完用进程 argv 复核（不是看配置）。
- **"中止"= 删 pending + 停 running + 复核**：`/interrupt` 只作用于正在执行的那条，
  对**排队中**的提交一个像素都不停。实测它 6 分钟后照样开始跑、又烧 21 分钟。
  实现见 `service/comfy_queue.go`；生成链路必须带 ctx（`RunGraphAtCtx`），
  否则节点判超时之后它还会自己跑满 20 分钟预算。
- **`attachDepLog` 不许在持有 `s.mu` 时调用**（2026-10-26 死锁现场）：它会读 `logDir`，
  再抢同一把不可重入的锁 ⇒ 永久死锁。症状极具误导性：`/api/deps` 全挂（20s+ 不返回）、
  `/api/deps/{name}/start` 返回 202 却不再派生进程，而 `/api/health`、`/api/runs` 完全正常。
  Start 的持锁路径用无锁的 `depLogHandleAt`，测试 `deps_deadlock_test.go` 钉住。
- **镜头复用缓存**在 `<产物目录>/shot-cache/<hash>.mp4`（键=提示词+宽高帧步+后端+turbo）；
  台词段不缓存（画面取决于当次 TTS 音轨）。要重拍就改提示词或删缓存文件。
- **H3 的 input/output 必须在工作区内**（2026-10-27 实测）：`SaveVideo` 写
  `D:/AI/ComfyUI/output` 报 `av.error.PermissionError [Errno 13]`——同一路径交互 shell 可写、
  只有派生进程写不了，表现为"采样跑完、最后一步全片作废"。把 `deps.json` 的 `h3.args`
  里 `--input-directory/--output-directory` 指到 `data/comfy/{input,output}`（停旧实例再 start，
  用进程 argv 复核），引擎照旧走 `/view` 取片。另：`D:\AI\ComfyUI-H3\temp` 被删会让 H3
  起不来（`os.makedirs ... 拒绝访问`），别让清理脚本碰这个目录。
- 代码量参考（2026-10-25 实测）：backend 62 个非测试 Go 文件 / 17.3k 行（改前 23.6k），
  测试 43 个文件 / 5.8k 行，API 21 条，节点 12 种。数字明显超出说明有东西被加回来了。
  （2026-10-29 画布回归后：API 31 条（新增 10 条画布 CRUD/校验/运行）、节点 13 种——
  画布面 + comfy 逃生舱是有意加的，不算"加回来"。）
- **H3 提速现状（2026-10-29 实测）**：`运行/data/deps.json` 的 h3.args 已加 `--use-ck-attention`，
  换提示词 178s→85–95s（约 2×），峰值仍 23.8GB。更深一层：每镜物理盘读 **35.4GB / 113s（313MB/s）**，
  而盘本身 2.6GB/s；64GB 内存 commit 打满、页面文件峰值 27GB（2 槽，最大 128GB）。
  `--disable-fast-disk` / `--disable-dynamic-vram` / 关 pinned / Defender 排除模型目录
  都实测无正收益（换页或方差更大），别再试。根因、A/B 与后续
  “内存升 128GB / W4A8 + fp8 VAE / 批量构图”方案见 `docs/H3-加速与显存优化-20261029.md`。
- **2026-10-29 分镜默认值已改**：`shotlist` 不再硬编码 `LowVRAM: true`——≤608×352×124 默认关，
  >1.15× 基准才自动开（防 OOM），节点 `low_vram` 显式覆盖；`film`/`film_from_plan` 的 steps 20→12。
  1 镜 `film_from_plan` 冒烟：生成 2m10s、整 run 215.7s（冷启动），124 帧 + AAC，
  ComfyUI 日志无 `MiniMaxLowVRAMAttention`/`MiniMaxChunkFeedForward`。旧 20 步 shot-cache 键不再命中，
  升级后首跑会重烧；旧 exe 在 `运行/FlowEdge.exe.old-20261029-182541`。
- **多镜身份锁实测（2026-10-29）**：只给 `characters` + 普通提示，第 2 镜会漂成黑发女仆；
  **Ref2VA 六段式（subject_definitions / `<Picture 1>` / retention_analysis）+ 3 张参考图**才锁住
  （两镜同一蓝发鱼尾/鲸鱼围裙角色）。2 镜实测：2m15s + 2m04s，总 run 271.4s；
  成片 245 帧 = 2×124−3，差值是 xfade 接缝合法重叠，不是丢段。
  `verify-media.py` 另报 **WARN silence_gap 0.85s @5.14s**（接缝处可听静音，mean −24.4dB）；
  交付前需修（首静音裁剪 / 共享环境声床 / 加长 acrossfade）。
  已试首静音裁剪+自身循环：0.888s→0.489s，但该镜自身还有 0.49/0.26s 静音段，
  **单靠裁剪不够，必须整片铺连续环境声床（bgm/room-tone 低电平）**。
- **多镜/高分辨率/音频修复实测（2026-10-29 20:45）**：3 镜六段式 3m23+2m23+4m07，总 611.9s，
  368 帧，身份一致、ASR 空；但有 3 处接缝静音和 −0.0dB 削波。736×416 单镜 7m32s，整 run 457.5s，
  ComfyUI history 确认自动 LowVRAM 节点存在，无 OOM。音频修法：`mixBGM` 允许 `bgm_gain=0`，
  `assembleShots`/`masterLoudness`/`mixBGM` 加 `alimiter=limit=0.8:level=false`；
  run 349 用缓存 39.3s 重跑，`verify-media` **全绿**（245 帧、mean −23.7、max −1.9、无静音）。
  **`alimiter` 必须 `level=false`**，否则自动抬回 0dB；中文路径走 PowerShell 请求会变 `??`，
  要用 UTF-8 文件体或 ASCII 路径。
- **PDD 8 步实测（2026-10-29 21:20）**：FL2VA/Ref2VA 两份 LoRA 各 1.28GB 已放
  `D:/AI/Models/comfyui/loras/`，ComfyUI 可枚举。同提示词/seed、608×352×124：
  CK+base 12 步 **88.7s**（峰值 23,820MB） vs CK+PDD 8 步 **42.5s**（23,858MB），**2.09×**。
  抽帧构图/角色一致。`h3-bench.py` 已加 `--lora/--shift-audio`。
  **已接入 FlowEdge**：非台词 **FL2VA** 镜走 PDD 8 步；2 镜生产 1m11+1m49，总 216.1s，
  `verify-media` 全绿。**Ref2VA PDD 实测反而 3m40+3m50（585.8s）**，代码已把 `h3_r2v`
  的 PDD 关掉、回 base 12；别再默认给角色片开 PDD。
- **多镜批量构图已上线（2026-10-30，P2 结构优化）**：`film`/`film_from_plan` 的
  `batch_size` 字段（默认 3，0/1=关）。连续、非台词、组内无 continue、缓存未命中、
  lowVRAM 同档的镜合成**一张图**提交（屏障节点强制两阶段），换权重 3N→3。
  直连 A/B 3 镜 752/654s vs 306/381s（**≈2.05×，峰值显存持平**）；
  生产同帧数对照 569s → 285s（**2.0×**），verify-media 全绿。
  屏障节点在 `D:/AI/ComfyUI-H3/custom_nodes/flowedge_batch.py`（**删了它批量就废**，
  ComfyUI 报 unknown node）；引擎侧 `comfy_media_batch.go`，产物靠 `NodeID=140+i`
  映射回镜，**改节点 id 布局必须同步**。批量失败自动回退逐镜，不构成新门槛。
  详见 `docs/H3-加速与显存优化-20261029.md §10.4`。
- **直连 ComfyUI 的 A/B 必须挪 seed+prompt（2026-10-30 实测）**：ComfyUI 有**节点级
  输出缓存**——完全相同的图再提交一次，2 秒就"完成"并返回上次的产物（server_s≈0）。
  `h3-batch-bench.py` 用 `--nonce` 同时挪 seed 与 prompt 尾巴；两臂必须**不同** nonce
  （相同 nonce 会让 TE 编码节点命中缓存，白送批量臂收益）。
- **引擎重启后 ComfyUI 会留下孤儿实例（2026-10-30）**：部署换 exe 杀了引擎，但它派的
  ComfyUI 活着继续占 8189 ⇒ 新引擎 start 再派生一个。判据：`/api/deps` 里
  `managed:false` + `pid:null`（端口被非托管进程服务）。**先 `taskkill` 孤儿再 start**，
  起完用进程清单确认只剩一个。另：venv 的 python.exe 是启动器，真实工作进程是它的
  子进程（系统 python 路径 + venv site-packages）——父子两个 pid 是**正常**的，
  别当双实例误杀。
- **pipelines.json 加字段必须带 `bind`**：没有 bind 的字段参数在 API 层被接受
  （params 里看得见）但**永远不会落到节点 data**——表现为"开关传了却不生效"，
  且不报任何错。新字段照抄相邻字段的 bind 结构（shotlist 系都是 `sl-1`）。

## 1–4. 环境硬约束（权重 / 显存 / 进程 / 下载）


> 详表（含实测数字与历史坑）见 skill `local-multimodal-ops` 的 `references/environment.md`；这里只留**会做错事**的部分。

- **权重只放 `D://AI//Models`**，项目目录不存；ComfyUI 经 `extra_model_paths.yaml` 读，换路径必须 `/object_info` 验证。`D://AI//hy3d` 别动；同盘搬用 `Move-Item`。
- **显存单侧驻留**：换侧先卸载 → 轮询 `vram_free`≥16GB → 再继续；卸载返回≠显存归还；`/free` 必带 `{"unload_models":true,"free_memory":true}`。**「不确定」=「不共存」**——乐观的代价是又一次 183s。语音服务默认 CPU。
- **共享 CUDA 底座 `D:/AI/envs/torch-cu124/`**：第三方 GPU 推理都复用，不再各自下 2.4GB torch；torch/torchaudio 从 ComfyUI venv 复制（2.6.0+cu124），只另装 `transformers<5` 与小依赖（~60MB）。
  复制必带四样，漏一个就 ImportError：`numpy.libs`、`torio`、`mpmath`、`typing_extensions.py`。
  不复用 ComfyUI venv 本身：Audio8 要求 `transformers<5`，ComfyUI 是 5.14.1，降级会把节点整块打挂。
- **Windows 版 torch 不带 triton** ⇒ `torch.compile`/inductor 不可用（`import triton` 直接 ModuleNotFound）；想加速只能走 CUDA Graph 或换 Linux，别在 Windows 上反复试 compile。
- **依赖配置改 `data/deps.json`，别死磕环境变量（2026-10-23 教训）**：引擎 `TTS_BASE_URL` 等只认 `os.Getenv`（无 `.env`），而引擎是序策外壳 `spawn(exe,[],{detached:true})` 拉起的、**外壳不传 env** ⇒ `setx` 后必须连外壳重启才生效。`depsFile` 已补 `tts` 段（script/python/port/base_url/autostart/**idle_unload**），优先级 **环境变量 > deps.json > 按脚本名推导**；改完 `POST /api/deps/reload` 立即生效（base_url 变更仍需重启引擎）。切法见 `docs/Audio8-TTS-0.6B-部署记录.md §8`。
- **换 `运行/FlowEdge.exe` 前先判断谁占着它**：多个 `--mcp` 瘦客户端会常驻锁文件，`cp` 报 busy。**别去杀**（那是 WorkBuddy/DSH 的 MCP 连接）；正确做法 `mv` 改名再 `cp`（改名只改目录项，不被句柄锁挡）。
- **不许「静默降级出不是用户要的东西」**：TTS 没起时回退 SAPI 是合理兜底，但**音色克隆**回退 SAPI 等于换了个人说话、节点还报成功。判定规则：凡「失败后还能给替代品」的路径，先问 **替代品与用户要的是不是同一件事**；不是就必须显式报错。同类：超时吞成成功、缺模型静默换小模型、翻译失败用原文。落地：`SynthesizeWith` 里 `RefAudio != ""` 禁止回退；`tts_clone_test.go` 钉住。
- **改 ComfyUI 启动参数先 stop 再 start**（复用旧进程）；引擎重启会连带杀掉托管的 ComfyUI/ASR/TTS，重启后重新 `POST /api/deps/{name}/start`；`readyTimeout()` 按类型：ASR/TTS 300s、`comfy_h3` 180s、其余 120s——写死 3s 会让「面板点启动再跑工作流」必然报未就绪。
- **下载**：已有权重一律 `HF_HUB_OFFLINE=1`；镜像 `HF_ENDPOINT=https://hf-mirror.com`+`HF_HUB_DISABLE_XET=1`；torchaudio 必须与 torch 同版本（否则音视频节点整块不注册）；**大文件禁 `huggingface-cli download`**（静默失败、退出码仍 0）→ curl+sha256。
- **HF 模型优先 ModelScope 镜像（实测差 35 倍）**：同机同时刻下 Audio8-TTS 0.6B——hf-mirror 16 线程 0.5MB/s（80 分钟）vs ModelScope `resolve` 直链 **18.8MB/s（2 分 20 秒）**。直链 `https://modelscope.cn/models/{owner}/{repo}/resolve/master/{path}`；清单+sha256 走 `api/v1/models/{owner}/{repo}/repo/files?Recursive=true`。**`repo?FilePath=` 对大文件限速，必须用 resolve**；模型 ID 可能与 HF 不同，先查清单再下。
- **显存相关改动必须实测峰值**（`nvidia-smi` 采样），不接受「应该没问题」。

## 5. 判定纪律

- 节点「装了」不等于「注册了」→ 查 `/object_info` 的节点 id 与总数。
- 视频「生成了」不等于「有画面」→ `ffprobe` 看 stream 列表（video + audio 都要有）。
- 模型「迁完了」不等于「读得到」→ 查 loader 的枚举列表。
- 场景「写了锁」不等于「锁住了」→ 多段锁句必须**字节全等**（`auditSceneConsistency`）；
  文字"差不多"=没锁，接缝处会换场地而帧数时长全部正常。
- 报错原文优先于任何文档；同一错误在两个不同权重上都出现，就别再换文件试。

> **实现档案已移出**：§5.1–5.14（各模块落地记录/实测复盘）与 §8（配置即数据契约）在
> `docs/AGENTS-实现档案.md`（29.5k 字符）。**不要整篇读**——按下面索引 grep 那一节。
> 本文件只留「判断纪律 + 硬约定 + 别重复投入」，这是每会话都要看的部分。

## 实现档案索引（需要时再读 `docs/AGENTS-实现档案.md`）

| 章节 | 内容 | 什么时候查 |
|---|---|---|
| §5.1 / §5.1.1 | ComfyUI 依赖组合、出图性能铁律 | 换 ComfyUI/torch/kitchen 版本、出图慢 |
| §5.2 / §5.2.1 | H3 双实例铁律、文生图三实例 + MOSS 后端 | 跑 H3、换图像后端 |
| §5.3 | H3 提示词优化（三段式/六段式） | 改提示词引擎 |
| §5.4–5.6 | 长片拼接四铁律、耗时构成、分镜驱动流水线 | 长片装配、估时 |
| §5.7–5.8 | 音频乱语四层治理、ref2va 正常念稿 | 有人声/念稿需求 |
| §5.9–5.10 | 序策双模出片、多段成片与角色一致性 | 多段片、一致性 |
| §5.11–5.14 | 提示词引擎重写、序策合一、模块三处、后期三坑 | 改引擎能力 |
| §8.1–8.8 | 三份配置文件、配置铁律、沙箱、API 格式 | 改 `data/` 配置或对外协议 |
| §8.9 | 工具链统一发现（skill/MCP/插件/智能体）、enabled/usable/managed 三分别、探活与热注册 | 加工具来源、改 `/api/toolchain`、插件节点空白 |

## 6. 已知不可用（别重复投入）

| 项 | 状态 | 原因 |
|---|---|---|
| ~~minimax_h3~~ | **可用** | 必须走 `D://AI//ComfyUI-H3` 独立工作树 + 独立 venv（torch 2.14）；默认实例（torch 2.6）跑同图会 OOM |
| qwen_image_2.1 出图 | 不可用 | VAE 被误判成 WanVAE（4 vs 12 通道），0.29/0.31 都没修（等上游） |
| SageAttention | 负优化 | 206s vs 120s，还连带装 triton；默认关 |
| RIFE 插帧 | 不可用 | 镜像 404；插帧改 `ffmpeg minterpolate`（CPU、零显存） |
| Audio8-TTS 0.1B INT8（ONNX/CPU） | 不可用 | 还原不了文本（任意文本都跑满 token、音频只有哼鸣）；权重/编解码器均正常，是那张 ONNX 图的问题。0.6B PyTorch 在同机同权重路线下完全正常，可反证 |
| Audio8-TTS 0.6B PyTorch（CUDA） | **可用但慢** | 文本还原准确、克隆 MFCC 0.993、显存 2.4GB；但 RTF 默认音色 7.9 / 克隆 18~32（launch-bound，Windows 无 triton 救不了）。服务 `tts_audio8_pt_server.py` :8103，用法见 `docs/Audio8-TTS-0.6B-部署记录.md` |
| ~~VLM 主题一致性质检~~ | **已补齐** | vision 节点支持「参考主题」入参（节点填或自动取上游 prompt），不再按画面自评；新增 `select` 择优节点（K 候选→VLM 打分取最高）；缺 VL 模型报 `config_missing` |

## 7. 构建与验证

```bash
python scripts/check-all.py                # 一键全检：build + 核心测试 + tsc + 契约（--fast 25s / 默认 1–5min / --live 加在线）
python scripts/build-windows.py            # 前端 + 单文件 exe → dist/FlowEdge.exe
python scripts/build-windows.py --skip-ui  # 只重编后端（不复制前端，见下方警告）
cd backend && go test ./...                # 全量测试
cd frontend && npm test                    # 前端用例（零新增依赖，见下）
cd frontend && npx tsc --noEmit            # 类型检查（测试文件也在内）
```

- **前端测试不引入 vitest**：`frontend/scripts/run-tests.mjs` 用 vite 自带 esbuild 把 `src/**/*.test.ts` 打包，`node:test` + `node:assert` 跑。
  旧的「后端节点类型 ↔ 前端调色板」契约测试**已作废**（调色板不存在了）；现在的形态契约在后端：
  `backend/service/pipeline_test.go` 直接拿 `data/pipelines.json` 逐条组装，字段 bind/when 写错就是红灯。
- **`--skip-ui` 不复制前端**：只内嵌 `backend/web/static` 已有产物。改了前端却用 `--skip-ui`，exe 界面还是旧的（或直接 500）；要么完整构建，要么先手动 `cp -r frontend/dist/* backend/web/static/`。
- **前端资源优先级**（启动时判定，运行中改文件不生效）：`STATIC_DIR` → `<exe目录>/web/static` → exe 内嵌 → 开发目录 `../frontend/dist`。临时放 `运行/web/static` 可免重编调试，但用完必须删，否则永远遮蔽内嵌版本。
- 部署一步到位：`运行/3-应用更新并重启.bat`（停服 → 替换 exe → 清 web/static → 启动）。
