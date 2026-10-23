# FlowEdge 项目规则（给未来的自己/其他 agent）

> 这些都踩过一次，别再踩第二次。详细证据链见 skill `local-multimodal-ops`。
> **出片先读 `docs/多模态出片手册.md`**：选路 / 时长分配 / 提示词模板 / 验收 / 排错，一页查完。
> **先查路由表**：`.workbuddy/memory/INDEX.md`（任务/症状 → 精确到章节 + 唯一事实源），命中就不再全文搜索。
> **提交前一键**：`python scripts/check-all.py`（build+test+tsc+契约测试，失败才展开细节）。

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

### 0.10 常驻服务怎么起（2026-10-22 修订）
agent 自己的 shell 拉不起来（沙箱给子孙进程设 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`，连 `CREATE_BREAKAWAY_FROM_JOB` 都被拒 WinError 5），但**让别人拉可以**。按可靠性排序：

1. **让外壳拉（首选）**：`POST http://127.0.0.1:30000/api/flowedge/supervise/start {"target":"compute"}` → 引擎父进程变成序策外壳（用户自己起的 node），**跨会话存活**。封装：`python scripts/start-engine.py`。
2. **用户双击**：`运行\0-打开客户端.bat`（探 8080 → 没起就拉起 → 开窗口）。
3. **本进程 detached**：`python scripts/start-engine.py --no-station`，能跑但随会话回收，只当临时用。

不许再试：`wmic` / `schtasks`（黑名单）、`cscript`（判 LOLBin）、`explorer.exe <exe>`（不生效）。
**判据**：`curl --noproxy '*' 127.0.0.1:8080/api/health` 返回 200 **且** `Get-CimInstance Win32_Process` 里 `ParentProcessId` 不是 agent 的 shell。命令返回 0 不算成功——`start` 也返回 0，进程照样死。每次接手先探活，别假设还活着。

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
| ① | 把二手的当一手：沿用社区博客摘要（`subject_definitions / retention_analysis` 那套）当官方要求，没发现官方仓库原文、工作流 JSON、节点源码（`nodes_minimax_h3.py`、`MINIMAX_H3_LOCAL.md`、`MiniMax-AI/MiniMax-H3/skills/h3-prompt-writing/references/`）**本地就有** | 「标准/规范/最佳实践」按序取证——本地源码与官方范例 → 官方仓库原文 → 官方文档页 → 社区博客（只当索引）。指不到原文那一条就是在猜 |
| ② | 用「工具没报错」当「没问题」：`av_drift=0` 就认为音画同步，实际 TTS 每句自带 0.44s 前导静音 | 验收必须含**用户能感知的项**且都有数值：视频帧数/时长/接缝 + 语音起点 vs 计划时点（≤0.1s）+ 跨段 AvgY 光比；音频人声/底噪 SNR ≥8dB + ASR 可懂度；提示词 `auditH3Prompt` 全绿 + 至少跑一次真实 LLM 并记下它的错误 |
| ③ | 用调参掩盖结构错误：混音顺序错（先 amix 后 loudnorm），把 `--ambience-gain` 从 0.18 顶到 1.2，结果比原版更糟 | 指标不对先问「链路顺序/结构错了没」，再问「参数调多少」。**参数化 ≠ 修根因**；连续两次调参无改善必须停下读代码 |
| ④ | 一次改多个变量：重拍段 2/3 同时改提示词、lead/pause、混音链、环境声素材 → 分不清是谁导致的 | 一轮只动一个变量；同轮改 ≥2 项必须在日志列变量清单 + 保留 A/B 版本，否则不许说「已验证」 |
| ⑤ | 换轨/装配不验前提：`-shortest` 让容器比视频流长 4 帧 → `stitch` 按容器时长算 xfade offset 越界 → 整段画面丢失，**成片时长仍报 30s** | 任何 mux/拼接后立刻 `ffprobe` 核 **`nb_frames` 与视频流 duration**；装配器一律用**视频流时长**算偏移，不用容器时长 |
| ⑥ | 停服务前没准备恢复手段：杀了 8080 才发现 bat 在 Git Bash 下编码失败、PowerShell 拉起的进程被会话清理、COM 被安全策略拦 | **动常驻服务之前先确认恢复路径可用**（谁启动、怎么启动、有无中文/编码问题）；验证不了就别停 |

### 0.13 汇报口径（避免「看起来都做完了」）
结论先行 → 给数值证据 → **把「没验证的部分」单独列一节** → 下一步。不许把「代码已落地」和「效果已验证」写在同一个语气里；没跑过的写「未验证」，不用「应该可以」「理论上」糊过去。

### 0.14 上下文预算（2026-10-23 实测，比砍规则文件更值钱）
同一会话实测：静态注入 25.3k 字符，而**自动压缩摘要 94.6k + 47.6k = 142k 字符（≈102k tokens，5.8×）**。摘要由 transcript 生成 ⇒ **工具输出才是成本主因**，砍规则文件只能动 3%。

- **单条输出 ≤2KB**：命令自带限制（`head`、`--oneline`、`--json`、`head_limit`、`| tail`）；禁无限制 `cat`/`grep -r` 全库。
- **>4KB 的文件禁整读**（`AGENTS.md` 8.9k、`docs/多模态出片手册.md` 16.7k、`引擎平台化评估` 65k）：先 `grep -n` 定位，只读命中行。
- **一次性测量先查 `scripts/`**：`python scripts/token-audit.py [--session|--dup]` 已覆盖注入与会话开销；别现写探针（每段探针 ≈1KB 命令 + 1–3KB 输出，且会被摘要二次放大）。

## 1–4. 环境硬约束（权重 / 显存 / 进程 / 下载）

> 详表（含实测数字与历史坑）见 skill `local-multimodal-ops` 的 `references/environment.md`；这里只留**会做错事**的部分。

- **权重只放 `D://AI//Models`**（~102GB），项目目录不存；ComfyUI 经 `D://AI//ComfyUI//extra_model_paths.yaml` 读，换路径必须用 `/object_info` 验证。`D://AI//hy3d` 保持原位别动；同盘搬用 `Move-Item`（Git Bash 里 robocopy 参数写 `//MOVE`）。
- **显存单侧驻留**：换侧先卸载 → 轮询 `vram_free` ≥16GB → 再继续；卸载返回不等于显存归还；`/free` 必带 `{"unload_models":true,"free_memory":true}`。**「不确定」永远等于「不共存」**——判断偏乐观会把已驻留的扩散模型挤出去，代价是又一次 183s。语音服务默认 CPU。
- **共享 CUDA 底座 `D:/AI/envs/torch-cu124/`**（2026-10-23 建）：所有需要 GPU torch 的第三方推理都复用它，
  不再各自下载 2.4GB 的 torch。torch/torchaudio 是**从 ComfyUI venv 直接复制**的（同版本 2.6.0+cu124），
  只额外 pip 装了 `transformers<5` 与小依赖（约 60MB）。
  复制必须带的四样，漏一个就报错：`numpy.libs`（漏 → `DLL load failed while importing _multiarray_umath`）、
  `torio`（漏 → torchaudio `No module named 'torio'`）、`mpmath`（漏 → sympy 起不来）、`typing_extensions.py`。
  不复用 ComfyUI venv 本身：Audio8 要求 `transformers<5`，ComfyUI 是 5.14.1，降级会把节点整块打挂。
- **Windows 版 torch 不带 triton** ⇒ `torch.compile` / inductor 不可用（`import triton` 直接 ModuleNotFound）。
  想加速只能走 CUDA Graph 或换 Linux，别在 Windows 上反复试 compile。
- **改 ComfyUI 启动参数先 stop 再 start**（依赖管理器会复用旧进程）；引擎重启会连带杀掉托管的 ComfyUI/ASR/TTS，重启后要重新 `POST /api/deps/{name}/start`；`readyTimeout()` 按类型：ASR/TTS 300s、`comfy_h3` 180s、其余 120s——写死 3s 会让「先在面板点启动、再跑工作流」这条最常见路径必然报未就绪。
- **下载**：本地已有权重一律 `HF_HUB_OFFLINE=1`；镜像 `HF_ENDPOINT=https://hf-mirror.com` + `HF_HUB_DISABLE_XET=1`；torchaudio 必须与 torch 同版本（否则音视频节点整块不注册）；**大文件禁 `huggingface-cli download`**（静默失败、退出码仍 0，只剩 `.cache/*.incomplete`）→ 改 curl + sha256 校验。
- **HF 模型优先试 ModelScope 镜像（2026-10-23 实测，差 35 倍）**：同一时段同一台机器下
  Audio8-TTS 0.6B（2.45GB）——hf-mirror 单连接 140KB/s、16 线程分段 0.5MB/s（要 80 分钟），
  ModelScope `resolve` 直链 **18.8MB/s（2 分 20 秒）**。
  格式 `https://modelscope.cn/models/{owner}/{repo}/resolve/master/{path}`，
  文件清单与 sha256 走 `https://modelscope.cn/api/v1/models/{owner}/{repo}/repo/files?Recursive=true`。
  **注意**：`api/v1/.../repo?FilePath=` 那条路径对大文件会限速，必须用 `resolve` 直链。
  模型 ID 可能与 HF 不同（改了名/换了号），先查清单再下。
- **任何显存相关改动必须实测峰值**（`nvidia-smi` 采样），不接受「应该没问题」。

## 5. 判定纪律

- 节点「装了」不等于「注册了」→ 查 `/object_info` 的节点 id 与总数。
- 视频「生成了」不等于「有画面」→ `ffprobe` 看 stream 列表（video + audio 都要有）。
- 模型「迁完了」不等于「读得到」→ 查 loader 的枚举列表。
- 场景「写了锁」不等于「锁住了」→ 多段锁句必须**字节全等**（`auditSceneConsistency`）；
  文字上"差不多"就等于没锁，接缝处会换场地而帧数时长全部正常。
- 报错原文优先于任何文档；同一错误在两个不同权重上都出现，就别再换文件试。

> **实现档案已移出**：§5.1–5.14（各模块落地记录/实测复盘）与 §8（配置即数据契约）在
> `docs/AGENTS-实现档案.md`（29.5k 字符）。**不要整篇读**——先看下面索引，按 § 编号 grep 那一节。
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
| ~~VLM 主题一致性质检~~ | **已补齐** | vision 节点支持「参考主题」入参（节点填，或自动取上游 prompt/original_prompt），不再按画面自评；新增 `select` 择优节点：K 个候选 → VLM 逐个打分 → 取最高。缺 VL 模型时报 `config_missing`（界面给「打开环境面板」） |

## 7. 构建与验证

```bash
export PATH="$PATH:/c/Users/Administrator/AppData/Local/Programs/Go/bin"
python scripts/check-all.py                # 一键全检：build + 核心测试 + tsc + 契约（--fast 25s / 默认 1–5min / --live 加在线）
python scripts/build-windows.py            # 前端 + 单文件 exe → dist/FlowEdge.exe（32.3MB）
python scripts/build-windows.py --skip-ui  # 只重编后端（不复制前端，见下方警告）
cd backend && go test ./...                # 338 个测试
cd frontend && npm test                    # 28 个前端用例（零新增依赖，见下）
cd frontend && npx tsc --noEmit            # 类型检查（测试文件也在内）
```

- **前端测试不引入 vitest**：`frontend/scripts/run-tests.mjs` 用 vite 自带的 esbuild
  把 `src/**/*.test.ts` 打成一个 ESM 包，再用 `node:test` 跑。测试用 `node:test` + `node:assert`。
  其中包含**前后端契约测试**：从 `backend/service/nodespec.go` 解析出节点类型，
  断言每一个都出现在前端调色板里——「后端有、UI 点不到」这类漂移从此是红灯。
- **`--skip-ui` 不复制前端**：它只内嵌 `backend/web/static` 里已有的产物。改了前端却用 `--skip-ui`，
  打出来的 exe 界面还是旧的（或直接 500）。要么不带该参数完整构建，要么先手动
  `cp -r frontend/dist/* backend/web/static/`。
- **前端资源优先级**（启动时判定，运行中改文件不生效）：
  `STATIC_DIR` → `<exe目录>/web/static` → exe 内嵌 → 开发目录 `../frontend/dist`。
  临时放 `运行/web/static` 可免重编调试，但用完必须删，否则永远遮蔽内嵌版本。
- 部署一步到位：`运行/3-应用更新并重启.bat`（停服 → 替换 exe → 清 web/static → 启动）。

**成片验收（交付前必跑，见 §0.5）**：

```bash
python scripts/verify-media.py 成片.mp4 --asr --frames .tmp/frames
# 退出码 0=通过 1=警告 2=硬失败；--json 可接流水线门禁
# ASR 需要 8101 在跑；不可达时报 FAIL（不会伪装成「无人声」）
```

**本地端口探活（必须加 `--noproxy`，见 §0.6）**：

```bash
curl -s --noproxy '*' -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/api/capabilities
```

部署：`Copy-Item dist\FlowEdge.exe 运行\FlowEdge.exe`（先停旧进程），从 `运行\` 启动。
