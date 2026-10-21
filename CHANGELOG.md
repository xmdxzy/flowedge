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

### 未验证（诚实披露）

- **精度阶梯降级**：本机权重只有一档（fp8/int8），`ladder` 为空时该档被跳过。
  逻辑有单测（`TestDegradeMediaRequest_PrecisionFirst` 等），但**没有在真实多精度权重上跑过**。
- **真实 OOM 降级全链**：降级顺序与留痕有单测，未在真机上制造 OOM 复验。
- **`comfy_workflow` 对真实社区 workflow 的兼容度**：只验证了自造 API 格式 JSON 的解析、
  绑定与拒绝路径；未拿社区真实 workflow（含自定义节点包）跑通端到端。
- **OpenAI 兼容端点的第三方客户端实测**：协议形状、SSE 分块、错误体有单测，
  未用 Open WebUI / SillyTavern 真机联调。
- **多实例互斥的真实收益**：逻辑有测试（同组串行），未在双实例真机上测「避免了几次 OOM」。

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
