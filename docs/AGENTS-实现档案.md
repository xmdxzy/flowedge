# FlowEdge 实现档案（从 AGENTS.md 移出，2026-10-23）

> **来源**：原 `AGENTS.md` 的 §5.1–5.14 与 §8，逐字搬迁，未删改任何内容。
> **为什么移出**：原文件 40.3k 字符，其中 73% 是实现记录而非作业纪律，每会话注入造成 token 压力。
> **怎么用**：`grep -n "^## 5.10" docs/AGENTS-实现档案.md` 定位后只读那一节，不要整篇加载。
> **判断纪律与硬约定仍在 `AGENTS.md`**（§0、§1–4、§5 判定纪律、§6 已知不可用、§7 构建验证）。

---

## 5.1 ComfyUI 依赖（血泪）

- `comfy-kitchen` 必须与 ComfyUI 版本匹配，且与 torch 兼容：
  - 0.2.22（0.29 自带）缺 `AsymW4A8Int8Layout` → **所有 fp8/量化文本编码器加载返回 None**
  - 0.2.28（0.31 requirements）要求 torch>2.6，本机 torch 2.6 会报 `list[int]` schema 错误
  - **本机可用组合：ComfyUI 0.31 + comfy-kitchen 0.2.27 + torch 2.6.0+cu124 + torchaudio 2.6.0+cu124**
- 症状特征：`'NoneType' object has no attribute 'Params'` 出现在 CLIPLoader → 先查这条，不要急着换模型文件。

## 5.1.1 出图性能铁律（2026-10-19 实测）

- **不要用 `--disable-smart-memory`**：它让每张图都重新装 17GB，实测 5 张 768² 要 199s；
  常驻权重同样 5 张只要 29s（6.9 倍）。默认参数已改为 `--reserve-vram 2`。
- 换侧（扩散 ↔ 语言 ↔ 语音）时由 `enterSide` 调 `/free` + 轮询 `vram_free`，这是唯一的卸载时机。
- 批量优先用「多提示词一次提交」（`prompts` 一行一条 / `split_lines`），而不是循环多次跑节点。
- **SageAttention 在本机是负优化**：608×352×124/20 步，开 206s / 23.8GB，不开 120s / 21.8GB。
  默认关闭，只有显式 `H3_SAGE=1` 才启用。
- **low_vram 分块不降峰值**：20GB 权重本身主导峰值，注意力分块只省 kernel 瞬时显存。
  实测 736×416×124：开 167s / 23.8GB，不开 130s / 23.8GB —— 留作逃生开关，别当常规优化。
- **视频插帧用 ffmpeg minterpolate（CPU）**：10.4s 视频 36s 完成，显存零占用；RIFE 镜像 404 不采用。

## 5.2 H3 双实例铁律（2026-10-19 实测）

- H3 不是「大一点的工作流」，是**另一套运行环境**：
  - 工作树 `D://AI//ComfyUI-H3`（git worktree）+ 自己的 `.venv`（torch 2.14.1+cu130 + 完整量化后端）
  - 端口 **8189**，主实例 8188；两者靠 `Exclusive` 互斥，**不能同时驻留**
  - 启动参数：`--reserve-vram 4 --cache-none`；**不要加 `--disable-smart-memory`**（会把权重挤进内存）
- `--use-sage-attention` 只在 sageattention 真正装好时才加。该 venv 没装时，ComfyUI 会直接 ERROR 退出
  （不是降级运行），表现就是「启动即退出」。
- 权重迁移后**必须同步改 `D://AI//ComfyUI-H3//extra_model_paths.yaml`**，否则 H3 一个模型都看不到。
- 实测能力：608×352×124 帧 20 步 ≈ 2 分钟，峰值 21.8GB；图生视频（首帧引导）峰值 22.6GB。
- 帧数必须落在 **17k+5** 网格（124 / 141 / 158），否则采样器报错。

## 5.2.1 文生图三实例 + MOSS 后端（2026-10-23 落地）

**为什么有第三个实例**：MOSS-Image 的节点（UNETLoader 认 moss 权重 / CLIPLoader `type=moss_image` /
`EmptyFlux2LatentImage`）是 ComfyUI **0.41** 才合入的（PR #17095，在 v0.41.0 发布之后）。
主实例停在 **0.31**（§5.1 的血泪组合：comfy-kitchen 0.2.27 + torch 2.6.0+cu124），
**为了 MOSS 升级主实例会把现有 flux 出图一起搞坏**——所以另开工作树，不动主实例。

| 实例 | 端口 | ComfyUI | venv | 跑什么 |
|---|---|---|---|---|
| main | 8188 | 0.31.0 | `ComfyUI/.venv`（torch 2.6.0+cu124） | flux / SDXL / Wan / 超分 |
| h3 | 8189 | 0.41.0 | `ComfyUI-H3/.venv`（torch 2.14.1+cu130） | MiniMax-H3 音视频 |
| **moss** | **8190** | 0.41.0 | **复用 H3 的 venv**（同 commit，省 ~10GB） | MOSS-Image 文字/布局 |

MOSS 用 `git worktree add --detach D:/AI/ComfyUI-MOSS e087d8f` 建，与 H3 **同一个 commit**。
H3 与 MOSS 同属 `gpu-exclusive` 组（11GB + flux 17GB = 28GB > 24GB，同卡必 OOM）。

**`extra_model_paths.yaml` 现在是三处**（§0.8 的一次实操）：主实例、H3、**MOSS**。
漏了 MOSS 那份，表现是「8190 起来了但一个模型都看不到」，报错不指向路径。

### 后端不是"换权重名"，是换整套构图

| | flux | MOSS |
|---|---|---|
| 加载 | `CheckpointLoaderSimple`（一个文件打包） | `UNETLoader` + `CLIPLoader(type=moss_image)` + `VAELoader` |
| latent | `EmptyLatentImage` | **`EmptyFlux2LatentImage`**（latent_format 是 Flux2） |
| 步数 | 4（schnell 蒸馏） | **50**（官方；三档实测见下） |
| CFG | 1.0 | 3.0 |
| 图内文字 | 画不出，只有像字的笔画 | **原生渲染中英文** |
| bbox 布局 | 无 | 有（layout JSON，见下） |
| 超时 | 240s + 75s/张 | **600s + 240s/张**（含 8190 冷启 ~130s） |

### MOSS 步数三档（2026-10-23 实测，1024²，同 seed 单变量）

| 步数 | 耗时（安静环境） | 显存峰值 | 文字 | 画面 |
|---|---|---|---|---|
| **8** | **9.7 / 9.9s** | 11.8GB | **全部正确** | 裁切偏紧、背景糊，适合草稿 |
| 25 | 24.5 / 29.9s | 13.5GB | 正确 | 与 50 步肉眼差距很小 |
| **50**（默认） | **43.7 / 47.7 / 43.9s** | 10.1–12.7GB | 正确 | 雾层与细节最完整 |

**消息：8 步就能把字写对。** 文字正确性不随步数显著变化（这是 bbox/字形条件在起作用），
步数影响的是画面整体质量。所以「只要字」用 8 步，「要成片级」用 50 步。

**两条测量纪律（都是本次踩的）**

1. **测耗时时不能有任何其他任务在跑**。同一台机器上同时做 Go 构建 / 前端测试 / 引擎重启，
   同一张 50 步图从 44s 变成 **120–204s**（2.4–4.6 倍误差），而且完全没有报错提示。
2. **显存峰值受碎片历史影响，单次测量不可信**。连续跑不同配置后基座在 9.5→11.7→13.5GB 之间漂移；
   25 步有一次峰值顶到 **23.5GB / 24GB**，触发 offload 后耗时从 25s 变 **96s**。
   判「跑不跑得动」看多轮最小值，判「稳不稳」看是否贴顶。

默认保持 50 步（44–48s 可预测、质量最好）。要快就显式设 `steps`——
`auto` 只管后端，不管步数。

三个易错点（都有测试盯着，`t2i_backend_test.go`）：
1. `CLIPLoader` 的 `type` 必须写 `moss_image`，漏了报「检测不到模型类型」
2. latent 必须用 `EmptyFlux2LatentImage`，用老的会 size mismatch
3. **不要手动插 `ModelSamplingShift`**——MOSSImage 模型定义自带 `sampling_settings.shift=5.0`，再设一遍是叠乘

### 后端怎么选（三态，可审计）

`backend = auto | flux | moss`。auto 的判据（写死在 `ResolveT2IBackend`，返回 reason 供前端/日志显示）：

1. 填了 `layout` → MOSS（只有 MOSS 支持摆位）
2. 提示词命中文字信号（写着/文字/标题/字幕/海报/招牌/标语/排版/字样/logo/对联/reads/says/typography/poster/signage/headline）→ MOSS
3. 其余 → flux（4 步最快）

### layout JSON（MOSS 专有）

节点参数或上游 JSON 端口都收，格式：

```json
[{"type":"text","text":"要渲染的字","bbox":[y0,x0,y1,x1]},
 {"type":"object","desc":"一只白鹤","bbox":[400,200,900,700]}]
```

- bbox 是 **0-1000 归一化** `[y0,x0,y1,x1]`，原点左上（改分辨率不用重画框）
- 引擎按官方算法（`nodes_moss_image.py:41-42`，floor/ceil 方向照抄）转成
  `abs_centralized` + `yxyx` 像素坐标，编译成 layout JSON 直接喂 `CLIPTextEncode`
- 不连 `MOSSImageLayoutPrompt` 节点：它只是把数组转成同样的字符串，引擎自己拼省两个节点

### 下载踩坑（2026-10-23）

- `hf-mirror` 单连接只有 **0.7–0.8 MB/s**，5.4GB 要两小时；`hf_transfer` 也救不了（服务端限速）
- 解决：`scripts/fetch-moss-fast.py`，**8 段 Range 并发**绕过单连接限速（~6MB/s）
  两个硬点：① 必须 `ProxyHandler({})` 绕开本机 http_proxy ② 段写入要 seek 到正确偏移，末尾用大小校验收尾
- 三件套：`moss_image_sft_int8_convrot`(5.4G) + `qwen3.5_9b_w4a8_gptq` + `flux2-vae_small`(0.34G)

## 5.3 H3 提示词优化（内置环节，2026-10-19 重写）

- 官方 prompt guide 规定**三段式英文结构**，不是「一自然段」：
  ```
  integrated_multimodal_description: [Shot 1] ...
  overall_soundscape: <环境声/音效/非语言，1–4 句>
  non_diegetic_music: <只写配乐；不要就 N/A>
  ```
- **字段空着 = 模型自己脑补**。空 `overall_soundscape` / `non_diegetic_music` 时 H3 会自行
  填背景音乐和即兴人声——这是「胡言乱语」最直接的来源，比换 seed 影响大得多。
- **cfg=1 没有负分支**（官方明说）：禁止写 "no dialogue / silent / 不要说话"，否定词反而会把
  概念带进来。压人声只用可见事实：`lips stay closed and still, jaw tight, breath audible through the nose`。
  要台词才用 `(S1) says <d>[English] ...</d>`，单句 ≤10 字。
- 引擎内置两条路径：
  - `text2video`（backend=h3）**默认先优化**（`prompt_opt` 字段可关），输出带 `original_prompt`
  - 独立 `prompt_opt` 节点：上游 text / 自身 text → 优化后 text；失败透传原文不中断
- **确定性兜底**（不依赖 LLM 是否听话）：`ensureH3AudioFields` 补字段、`ensureDialogueFree`
  补口型约束（提示词里已有 `<d>` 台词时不动）。
- `cleanOptimizedPrompt` 遇到三段式字段时**必须保留换行**，压成一段等于把字段边界抹掉。
- 规则源：`~/.workbuddy/skills/h3-prompt-optimizer/SKILL.md`，与 `service/prompt_opt.go` 同一份指令
- 顺序约束：优化是语言侧（LLM），必须在 enterSide(sideDiffusion) 之前做，反了会互相卸载权重

## 5.4 长片（分段拼接）四条铁律（2026-10-19 实战复盘）

30 秒成片暴露的四类缺陷与对应修法，改分段逻辑前先读：

1. **接缝闪跳**：段末帧被当下一段首帧 → 边界重复一帧 + 曝光微差。
   拼接必须走 `xfade=0.2s`（视频）+ `acrossfade=0.2s`（音频），两者时长一致保音画同步；
   实测 2 段成品 10.168s（2×5.17 减 0.17 重叠），而不是硬拼的 10.34s。
2. **音景硬切**：每段音频独立生成、独立重启。修法有两层：
   - 提示词：所有段共用一句「持续」的环境声描述（引擎已在分镜指令里强制）
   - 后期：`acrossfade` 平滑接缝
3. **台词乱语**：不给具体台词时，H3 会即兴生成含糊人声。
   **旧规则「写无对白」是错的**——cfg=1 没有负分支，否定词不生效甚至反效果。
   正确做法见 §5.7：填满声音字段 + 口型可见事实 + 12 步以上 + 中置相消。
4. **运镜反向**：段间无约束时镜头会掉头。分镜指令要求「下一段与上一段同向同速」。

## 5.5 长片耗时构成（别再拍脑袋估）

20 步 / 736×416：单段 ≈ 4 分钟（6 段 25 分钟 + 插帧 3 分钟 ≈ 30 分钟）
turbo LoRA / 608×352：**12 步 ≈ 169s / 单段**（2026-10-19 实测，含提示词优化 + 实例加载）；
8 步 ≈ 118s 但音频欠采样会乱语，不建议再压。
固定成本还有：H3 实例启动 ~50s、每次执行重载 20GB 权重（`--cache-none`）、LLM 优化+分镜 ~1 分钟。

## 5.6 分镜驱动流水线（2026-10-19 重构，替代「均分 5 秒 + 全部续接」）

参考：ai-short-film / aigc-production-flow / Moyin Creator / Jellyfish / ArcReel 五个开源短剧流水线，
共同骨架是「剧本 → 资产锁定 → 分镜表（每镜带景别·角度·轴线·衔接）→ 逐镜生成 → 按衔接装配」。
引擎已实现为两个节点：

- `storyboard`：LLM 输出结构化分镜表（JSON），字段固定：
  `seconds / shot_size / angle / axis / camera / action / light / ambience / sfx / music / dialogue / transition`
- `shotlist`：逐镜执行 + 音频治理 + 按 transition 装配

### 与旧版的关键差异

| 维度 | 旧（均分续接） | 新（分镜驱动） |
|---|---|---|
| 时长 | 全部 124 帧 | 每镜独立，吸附 17k+5 网格（73/90/107/124/141/158/175…） |
| 衔接 | 全部末帧续接 | `cut` 换机位 / `continue` 同镜延续 / `dissolve` 时间跳跃 |
| 角度 | 无控制 | 景别+角度+轴线逐镜写死（AI 无跨镜记忆，必须逐镜重复） |
| 台词 | 无约束（会乱语） | 默认「无对白」+ ASR 抽检 + 自动压音 |
| 装配 | 统一 xfade 0.2 | cut=0.08s、continue=0.12s、dissolve=0.30s，最后 loudnorm 归一 |

### 装配三个已修的坑（都有回归测试 `video_edit_test.go`）

1. `cut` 用 0.04s 淡化 < 一帧 → xfade 窗口装不下帧，**第二段画面整段丢失**（音频却是全长，
   时长看起来正常，极易漏检）。修法：d≥0.08 且 offset = total−d−0.05。
2. `loudnorm` 前多写了一个 `[afin]` 输入标签 → `More input link labels 2 > 1`。
3. 滤镜串里 `[ax3]` 单独成段 → `No such filter: ''`。

验收必须查**视频帧数**（`ffprobe -count_frames`），只查时长会被音轨骗过去。
已封装成 `scripts/verify-media.py`（帧数 + 音画时长差 + 静音缺口 + ASR），交付前直接跑它。
`shotlist` 节点装配完成后也会自动跑一次 `inspectMedia` 自检，输出 `check` / `check_ok` 两个字段；
`check_ok=false` 时日志会打印具体原因，**不要忽略**。

### 镜头级作业（2026-10-19，替代「单节点大循环」）

`shotlist` 每镜成功即写 `shot_jobs` 表，重跑按 `(run_id, node_id, shot_index, prompt_hash)` 复用：

- **失败不丢**：第 N 镜失败只丢这一镜，已完成镜头已落盘；报错里直接给镜号与续跑写法。
- **只重拍一镜**：节点填 `reroll=3`（或 `1,3` / `1-3`）。判据是提示词哈希——改了哪镜提示词就只重跑哪镜，
  反过来「改了提示词却复用旧画面」这种静默错误也不会发生。
- **预算按帧算**：`帧数×1.2s + 90s/镜 + 10min 装配`（基准实测 0.85–0.93 秒/帧），类型上限 8h。
  **旧的 30 分钟整节点封顶会让 12 镜长片必然超时**（12 × 169s ≈ 34min），别再加回来。
- **取消真的停**：镜间检查 ctx；`CancelRun` 会 `POST /interrupt` 到 8188 与 8189。
- **进度可观测**：每镜广播 `shot_progress`（index/total/phase/elapsed），输出带 `shots_reused` / `shot_jobs`。

## 5.7 音频「胡言乱语」四层治理（2026-10-19，已实测闭环）

用户最痛的缺陷。四层根因叠加，**缺一层就会复发**，别只修一层就宣布解决：

| 层 | 根因 | 修法 | 证据 |
|---|---|---|---|
| 采样步数 | turbo 压到 8 步；官方：音频比画面晚收敛，**台词/音色需 12+ 步，8 步时音频是短板** | turbo 默认 **12 步**，<8 步禁止 | 8 步旧片 ASR「枉年 赏我鸡肉海鲜」 |
| sigma 调度 | 图里没接 `MiniMaxH3SigmaShift`，只吃默认 12/3 | 接节点：turbo 12/6，base 12/3 | turbo LoRA 模型卡明写音频问题几乎都是调度没对齐 |
| 提示词 | 三段式字段空着 / 中文 / 用否定式禁言 | `overall_soundscape` 填满 + `non_diegetic_music: N/A` + 英文 + 口型可见事实 | 官方 prompt guide |
| 后处理 | 仍有约 20% 漏出人声 | **中置相消** `pan=stereo\|c0=c0-c1\|c1=c1-c0` + ASR 复核 | 「枉年 赏我提用海鲜」→ 空；响度 -14.1→-17.1dB |

**验收闭环**：同一提示词，修前某镜 ASR 转出乱语、修后 ASR 为空且平均响度 -14dB（不是静音），
才算真解决。只写"无对白"再重跑不算。

`shotlist` 已内置：ASR 检出 → 中置相消 → **再跑一次 ASR 复核** → 仍有人声才退回 `duckAudio`。
输出字段 `speech_detected / center_stripped / audio_verified / note` 记录每一步。

## 5.8 正常念稿：ref2va + 参考音频（2026-10-19 实测打通）

**结论先行**：H3 只有 `ref2va` 这条链路能"照着我的稿子念、口型跟着对"。
`fl2va`（T2V/I2V）是联合生成音频，台词质量不可控——上一轮我们把口型强制闭合，
方向是错的：用户要的是**可控的台词**，不是没有人声。

### 为什么是 ref2va
- `MiniMaxH3ReferenceToVideo` 是**唯一暴露 `ref_audios`** 的节点（最多 3 段）
- 提示词里写 `<Audio 1>: fully_copy - is reused 1:1 as the target video's complete final audio track.`
  → 这段音频作为成片**完整最终音轨**原样复用，模型不再重生成
- 于是「TTS 生成台词 → 送进去 → 口型对齐」成立，中文台词不再变伪中文
- 权重 `minimax_h3_ref2va_pruned_int8_convrot.safetensors`（20.97GB）**早就在 `D:/AI/Models`**，
  一直没被用过；端口仍是 8189，与 fl2va 互斥

### 六段式提示词（ref 模式专用，与 T2V 的三段式不同）
```
subject_definitions: <Picture 1> 定义角色外貌；<Audio 1> 是念稿音频
summary: [reference generation + audio reuse] ...
retention_analysis: <Picture 1> (appears in [Shot 1]): fully_preserved - ...
                    <Audio 1>: fully_copy - is reused 1:1 as the target video's complete final audio track.
detailed_description: [Shot 1] ...（替代 integrated_multimodal_description）
overall_soundscape: N/A - the complete final audio track is the copied reference audio.
non_diegetic_music: N/A
```
缺 `subject_definitions` / `retention_analysis` 时，模型把参考音频当"风格提示"重新生成 →
又变回伪中文。`ensureRef2VAPrompt` 会自动补齐。

### 声音要自己做，不要交给模型
`fully_copy` 意味着整条音轨就是那段 wav，所以**环境声必须在送进去之前混好**
（TTS 人声 + 环境底噪 → 一条 wav）。好处是完全可控、可重复；代价是要做一次混音。

### 实测对比（同一句台词，3090 24GB）
| 路径 | 参数 | ASR 转写 |
|---|---|---|
| fl2va（T2V） | 608×352 / 107~124 帧 / 12 步 turbo | 「枉年 赏我鸡肉海鲜」 |
| **ref2va + TTS 音频** | 608×352 / 107 帧 / 12 步 turbo / `fully_copy` | **「这件事我其实早就知道了」** |

耗时：ref2va 107 帧 ≈ 110s（权重热态）。口型帧人工抽查：说话时刻嘴部张开，与音轨对得上。

### ⚠ 复验：`fully_copy` 未复现（2026-10-19 22:30，独立实例 8092）

上表那条「音频 1:1 透传」在**独立构建 + 严格核验**下没有复现，先别当既定事实用。

核验方法：`scripts/audio-copy-check.py <成片> <参考.wav>` —— 比 10ms 包络互相关，
抗 AAC 编码噪声与毫秒级偏移（直接相减会把「同一段声音错了几十毫秒」误判成不同）。

| 实验 | 帧 | 参考图 | peak_corr | 判定 |
|---|---|---|---|---|
| 362 帧 + 音频 | 362 | 无 | 0.22 | DIFFERENT |
| 362 帧（修悬空 `<Picture 1>` 前） | 362 | 无 | 0.05 | DIFFERENT |
| 107 帧 + 音频 | 107 | 无 | 0.09 | DIFFERENT |
| 107 帧 + 音频 + 定妆图 | 107 | 1 张 | 0.18 | DIFFERENT |

- 判据 0.8；四组全部不达标 → **成片音轨是模型自己生成的，不是那段 wav**。
  旁证：run1 的 ASR 转出「湘田英臣想话…」这类乱语。
- 已排除：帧长（107 也失败）、缺参考图（加了也失败）。
- **视频侧是达标的**：362 帧 / 15.083s / 608×352 / 24fps，`ffmpeg select='gt(scene,0.30)'`
  在 **7.875s** 检出切点（提示词写 `[Shot 2] At 00:07.500`）→ 「一条提示词多镜头」成立。
- 已修的真 bug：纯音频时 `ensureRef2VAPrompt` 会写悬空的 `<Picture 1>`（图里没有 LoadImage），
  现在按实际引用描述，`ref2va_prompt_test.go` 锁住。
- **已查 /history（2026-10-19 22:40）**：接线是对的——`ref_audios: {"ref_audio_1": ["60",0]}`
  已到节点 5，节点 60 是 `LoadAudio`，`D:\AI\ComfyUI\input\dub107.wav` 557KB / 4.458s 完好，
  prompt 里 `retention_analysis: <Audio 1>: fully_copy` 也在。**接线、文件、提示词三样都对，音频仍未被复用。**
- **再排除两个**：字段重复（源文本自带 `overall_soundscape` 会与追加值冲突，已修，
  `/history` 里出现次数 2→1，仍 corr 0.166）；步数（12 步 turbo 0.166 vs 20 步非 turbo 0.092）。
- **结论**：本实例上 `fully_copy` 不生效，不要再指望「模型自己把音轨照抄」。
- **可行替代（已实测，且已进引擎）**：视频侧照旧用 ref2va 出多镜头，音轨在后期 1:1 替换。
  引擎已内置为 `text2video` 的 **`audio_track`** 字段（`33d507d`）：出片后自动 `apad + -t` 覆盖音轨、
  返回前自己数帧，掉帧直接报错。手工等价命令：
  ```bash
  # 关键：不要用 -shortest（音频比视频短 5ms 就会吃掉视频最后一帧，362 -> 361）
  ffmpeg -y -i in.mp4 -i dub.wav -map 0:v -map 1:a -c:v copy -af apad -c:a aac -b:a 192k -t <视频时长> out.mp4
  python scripts/audio-copy-check.py out.mp4 dub.wav   # 期望 peak_corr 1.0 / lag 0ms
  ffprobe -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames out.mp4
  ```
  实测：362 帧保住了、`peak_corr=1.0`、时长 15.084s。代价是**口型不由这段 wav 驱动**
  （模型是照自己生成的音频动的嘴），台词与口型的一致性无保证——这条要如实写进交付说明。

### 节点用法
`text2video` 节点填：
- `ref_audio`：一段 wav 路径（念稿音频）
- `ref_images`：参考图路径，换行分隔（≤9，锁角色外貌）
两者任一非空即自动切到 `h3_r2v`，不能与首帧引导（`image_path`）同时用。

### 已落地：一条提示词多镜头（2026-10-19 23:06 实测通过）

`text2video` 把 `[Shot 1] … [Shot 2] At 00:07.500 …` 排进同一条提示词，一次生成内含换机位；
台词靠 `audio_track` 后期 1:1 铺上去（不是靠 ref2va 的 `fully_copy`，那个在本实例不生效，见上）。

完整验收（单一产物，608×352 / 12 步 / turbo / 362 帧）：

| 项 | 结果 |
|---|---|
| 帧数 / 时长 | **362** / 15.083s / 24fps / 32kHz 立体声 |
| 片内切点 | `scene>0.30` 在 **7.75s**（提示词写 7.500）→ 两个镜头确实在一条提示词里 |
| 音轨 | `audio_track` 1:1 覆盖，`peak_corr 1.0 / lag 0ms`，`silence_gaps: []` |
| ASR | 「这件事我其实早就知道了**你身后那位也听得很清楚**」→ 两句台词、顺序正确、**跨切点连续** |
| 门禁 | `verify-media --asr --expect … --require-expect --max-silence 0.8 --gate` → **exit 0 全部通过** |

复现：
```bash
# 节点字段：backend=h3, frames=362, steps=12, turbo=true,
#           ref_audio=<念稿wav>, audio_track=<同一条wav>
```

为什么不再逐镜生成：逐次生成做不到「台词跨镜连续、音景不重启」——
每段音频独立生成、独立重启，接缝处必然断一次；单提示词多镜头则是一整条采样。

## 5.9 纳入序策引擎（2026-10-19，双模出片已跑通）

H3 出片不再靠对话里手工串步骤，已编译成序策图：
`content/01-H3音视频-台词氛围双模`（14 节点 / 13 边）。

### 图的骨架
```
goal → trigger → S1 模式判定(pick-mode) → router ─┬─ dialogue: dub-track → run-flowedge --mode dialogue
                                                  │            → extract → verify --asr --expect → 门
                                                  └─ ambience: run-flowedge --mode ambience
                                                               → extract → verify --asr --no-speech → 门
                                                                                     → hitl 人工签收
```

### 切换逻辑（"根据任务实际情况"）
- `mode=auto`：有台词表（非空 text）→ 台词；否则 → 氛围。**确定性判定，0 token**
- `mode=dialogue`：强制念稿（ref2va + TTS，音频 1:1 透传）
- `mode=ambience`：强制无台词（fl2va，且会**剥掉剧本里的 `<d>` 台词**、把 "says:" 换成 "stays silent"、补齐声音字段）
- 同一份剧本可以出两个版本：这是"切换"的实际含义

### 调用
```bash
node <序策>/scripts/flow-cli.cjs run graph "content/01-H3音视频-台词氛围双模" '{
  "brief_file":".../brief.txt","lines_file":".../lines.json","mode":"auto",
  "frames":107,"work_dir":"...","expect_line":"这件事我其实早就知道了"}'
```

### 六个必踩的坑（都已在图/脚本里规避）
1. **传参要平铺**：`'{"brief_file":...}'`，不要包一层 `{"input":{...}}`——否则 `run.input` 多一层，插值全空，路由静默走兜底。
2. **下游节点不能用 `{{input.x}}`**：那只在触发节点有效，后续拿到的是上游输出。一律用 `{{vars.x}}`（吸收输入里的标量）。
3. **shell 节点别传中文路径**：`cmd.exe` 转码会把 `运行\...` 弄坏。路径处理放进 Python 脚本内部（`--report-dir`），不要 shell 再转一手。
4. **`write` 执行器有路径沙箱**（只能写序策项目内）：跨目录落盘用 shell + 脚本。
5. **别在 cmd 里传可能为空的数值参数**：`--steps "{{vars.steps}}"` 在没传 steps 时渲染成 `--steps ""`，
   argparse 直接 `invalid int value: ''`，整步失败。可选参数一律由 plan 声明，图里不传。
6. **plan 里的相对路径按 plan 文件所在目录解析**：把 plan 复制到别的层级，`characters`
   会指到不存在的目录。跨目录复用 plan 时改绝对路径。

### 验收门
- 台词模式：`verify-media --asr --expect "<原话>" --gate` —— ASR 必须转出脚本原话
- 氛围模式：`verify-media --asr --no-speech --gate` —— 检出人声即 FAIL
- 两道门都通过才到 `hitl` 人工签收

## 5.10 多段成片与角色一致性（2026-10-19 实战：30 秒金丹斗法）

### 先设计门，再开机（2026-10-19 定，覆盖一切"固定秒数"建议）

**「每镜多少秒」没有恒定答案。** 我犯过两次同类错误（均分 5 秒、均分 10 秒），
根因都是拿一个固定数字去凑总长。现在改成机制：**每条片子开工前必须先写 `plan.gates`，
由 `scripts/film-gates.py` 机械校验，不自洽就不许出片。**

五层门：`timing`（总长/容差/单镜最短最长/各段权重）、`identity`（定妆图数/每段带参考图/<Picture N>）、
`audio`（台词必须转出、氛围必须无人声、响度区间、静音缺口、段级重试）、
`technical`（分辨率/帧率/音画漂移/接缝）、`budget`（总墙钟/单段墙钟/超限动作）。

```bash
# 1) 只给「总时长 + 各段叙事功能与权重」，脚本按比例分配并吸附 17k+5
python scripts/film-gates.py plan --seconds 30 \
    --beats setup=1.0,clash=1.35,finish=1.2 \
    --modes dialogue,ambience,dialogue --lines-per-seg 1,0,2 --stills 2 --budget 1500
# 2) 按本片内容改写草案（改门，不要直接改帧数）
# 3) 验门（shot-film.py 会自动跑；不过则拒绝开机）
python scripts/film-gates.py check --plan data/film/jindan/plan.json
```

门能抓的（实测反例）：段缩短但切点没前移 → `shot_too_short`（某镜 −0.2s）；
台词表与 `<d>` 不一致 → `line_mismatch`；`<Picture 5>` 但只有 2 张 → `picture_overflow`。

权重怎么定：问「这段承担多少叙事重量」，不问「该多少秒」。
内容写不进去（镜头挤在一段）→ 加权重；观众会走神（一段只有环境声）→ 减权重。

起点参考（默认值，**不是结论**）：建立 6–8s / 对话 6–10s / 交锋 11–15s / 收招 9–11s。
30 秒实战取值：192 + 277 + 260 = **29.98s**（`min_shot=1.2`、`max_shot=4.5`、预算 1500s，
实际墙钟 640s）。改任何一条都要能说出依据，写进 gates 的注释或 profile 名。

**段级门**：`shot-film.py` 每段出片后立刻验（台词段 `--asr --expect`、氛围段 `--asr --no-speech`），
不合门按 `audio.segment_retry` 重跑，仍不过才放弃——早失败只浪费一段，不是整片。

### 角色一致性：靠文生图定妆，不靠文字
1. flux 出角色定妆图（正面、干净背景、露法器与服装特征）
2. **每一段**都把这组图作为 `ref_images` 传进 ref2va（≤9 张）
3. 提示词里用 `<Picture 1>/<Picture 2>` 指认，并在 `retention_analysis` 写 `fully_preserved`
4. 段内再靠"逐镜重复外貌"兜底

实测（608×352）：三段之间青袍/黑袍、束发玉簪、白骨法杖、红瞳全部守住；
**锁得住服装·法器·剪影·配色，锁不住高相似度五官**——低分辨率下脸本来就没多少像素。
要更稳就提高分辨率，但 243 帧 + 2 张参考图在 24GB 上已贴顶（实测剩 ~0.6GB），没有余量。

### 段内多镜头单提示词
一条提示词里排 `[Shot 1] … [Shot 2] At 00:04.100, the camera cuts to …`，
一次生成内含多次换机位。这样段内声音连续、不需要拼接，比"一镜一生成"省一半时间且接缝为零。

### 台词时序
每段单独跑 `dub-track.py`，用 `lead` 把台词对齐到镜头切点（例：seg1 第 4.1 秒开口）。
`fully_copy` 下这条 wav 就是该段最终音轨，所以段内所有声音（含环境声）都要在 wav 里混好。

### 装配
`xfade 0.2` + `acrossfade 0.2`（等长保音画同步）。三段成品 720 帧 / 30.032s / 24fps，
`verify-media` 全过；整片 ASR 依次转出「让开 / 好剑 / 魔道无归」，与剧本一致。

### 耗时（实测，608×352 / 12 步 / 2 张参考图）
| 段 | 帧数 | 类型 | 墙钟 |
|---|---|---|---|
| 1 | 192 | 台词（1 句） | 178s |
| 2 | 277 | 氛围（无台词） | 236s |
| 3 | 260 | 台词（2 句） | 226s |
合计 729 帧 ≈ **10.7 分钟** + 装配 1 分钟。单帧约 0.85–0.93s。
同一批曾出现单段 556s（异常争用），**按 3–5 分钟/段估并留一次异常余量**。
`budget.wall_seconds_max` 就是拿这组数当基准定的，超了按 `on_exceed` 处理。

### 门必须有牙齿（2026-10-19 冒烟验证）

只有"能拦住"的门才算门。实测三条：
- 同一片段 `--max-silence 1.0` → WARN 通过；`--max-silence 0.8` → `FAIL silence_gap 0.92s（门上限 0.8s）`
- 台词模式漏传 `--expect` → `--require-expect` 直接 FAIL（以前会静默通过）
- plan 段缩到 141 帧但切点没前移 → `FAIL shot_too_short`（某镜 −0.2s）

**门不要设成你以为的"高标准"**：3 秒测试片只有一句「站住」，剩余静音 0.92s，
门设 0.8 会判它不合格——这是误判，正解是给 `dub-track.py` 混环境底噪，不是放宽门。
已交付的 30 秒片在 0.8s 门下 OK（有音效填满），说明门值要按片子解释，不能跨片照搬。

### 一键复现
```bash
python scripts/film-gates.py check --plan data/film/jindan/plan.json   # 先验门（不自洽就不必开机）
python scripts/shot-film.py --plan data/film/jindan/plan.json          # 内部会再验一次
```

### 预置门 profile（`film-gates.py plan --profile <name>`）

| profile | min_shot | max_shot | 容差 | 重试 | 静音缺口 | 超预算 |
|---|---|---|---|---|---|---|
| `narrative` 默认 | 1.2 | 4.5 | 0.75 | 1 | 1.0 | warn |
| `quick-cut` 快切 | 0.8 | 2.5 | 0.4 | 1 | 0.6 | warn |
| `long-take` 长镜 | 2.0 | 不限 | 1.0 | 1 | 1.2 | warn |
| `dialogue` 对话戏 | 1.5 | 6.0 | 0.75 | 2 | 0.8 | warn |
| `batch-night` 夜间批跑 | 1.0 | 5.0 | 0.75 | 0 | 1.0 | **fail** |
| `ambience-only` 纯氛围 | 1.5 | 6.0 | 0.75 | 1 | 0.7 | warn |

叠加顺序 `DEFAULT < PROFILE < 本片 gates < 命令行显式参数`，所以 `--min-shot` 之类默认必须是
`None`——给个 1.2 的默认值会把 profile 静默盖掉（当场踩过）。
序策图：`content/02-H3多段成片-定妆图锁角色`（trigger → shot-film → 门 → 人工签收）

## 5.11 提示词引擎对照官方指南重写（2026-10-20）

**事实源变了**：不再用社区博客。官方仓库 `github.com/MiniMax-AI/MiniMax-H3/skills/h3-prompt-writing`
里有 `references/base-en.txt`（T2VA/I2VA/FL2VA/L2VA）与 `references/ref-en.txt`（Ref2VA）两份原文，
本地官方工作流 `D:/AI/ComfyUI/user/default/workflows/MiniMax-H3/*.json` 的 prompt 字段是范例。

### 与旧实现的六处实质差异

| # | 旧实现 | 官方要求 | 后果 |
|---|---|---|---|
| 1 | `<Picture 1> defines the visual identity` | 图片只定身份时写 **`<Subject N>`**，图片源引在定义里；`<Picture N>` 只在它是帧/构图锚点时单列 | 标签语义错，身份锁不住 |
| 2 | LLM 猜 `summary` 任务前缀 | 前缀从固定枚举里选且必须与实际资产一致（无源视频就不能写 video continuation） | 模型把参考当错用的东西 |
| 3 | 说话人只写 `(S1) says:` | `(S1)` 前必须有可念的**音色/年龄/性别/语速**；身份与动作在 `<d>` 外，`<d>` 内只放语言标签与原文 | 语气与口型失控 |
| 4 | `outcome_soundscape: N/A`（fully_copy 时） | 不要拿 N/A 当垃圾桶：写「复制的环境层来自 `<Audio 1>` 持续全片」 | 声景字段失效 |
| 5 | 无风格开场句 | `[Shot 1]` 内第一句先立风格（Live-action, cinematic, ...）；Ref2VA 则放在 `[Shot 1]` **之前** | 画面质感掉档 |
| 6 | 正文 200 多词 | Ref2VA 的 `detailed_description` **350-500 词** | 细节不足，模型自由发挥 |

另：官方 4.3 明确「幅度与速度只在有意义时写，中等幅度常速通常省略」——
所以**不能**因为缺 `amplitude` 就报警，只查 `moves forward / camera follows` 这类非官方表达。

### 引擎落实点（`backend/service/prompt_opt.go`）

- `h3PromptInstruction` / `h3Ref2VAInstruction`：两套指令，按是否有参考资产分派（`media_nodes.go`）
- `auditH3Prompt`：确定性体检（字段缺失 / Picture 身份误用 / 说话人无音色 / 有音频没 fully_copy /
  非官方镜头词 / 无风格句 / 词数不足 / summary 前缀与实际资产矛盾 / 标签裸写）
- `normalizeSummaryPrefix`：用资产清单直接改写前缀，**不让模型猜**
- `patchRefAudioRetention`：多行块安全追加 `fully_copy`（`h3FieldBlock` 支持多行取值）
- `protectDialogue`：模型改字、翻译或丢 `<d>` 时**退回原文**重搭结构——台词不可让渡

### 实测结论（qwen2.5:14b，2026-10-20）

同一份指令跑三次，输出结构都在，但**每次错的地方不同**：把 `summary` 写成 `video continuation`、
`<Subject 1>` 丢尖括号、台词改写成 `"十年了." (Ten years have passed.)`。
**结论：14B 级模型不能作为唯一保障，必须有上面那套确定性兜底。**
`qwen3.8:27b` 在当前配置下返回空 `response`（推测思考链吃掉 num_predict），暂不作为默认。

## 5.12 与序策引擎合一（工作站，2026-10-20 实测）

FlowEdge 不再单打：**CORE（序策引擎 :30002）编排/门禁/预算 + COMPUTE（本 exe :8080）媒体/模型/工件 + STATION（:30000）单入口**。
完整评估与证据见 `序策/03-编排内核/FlowEdge-工作站合一-评估与优化-2026-10-20.md`。

**五条必须守的规则（都是本轮踩过的）**

1. **同一件事只能有一个答案**：能力探测判 `tts.ok=true`（SAPI 兜底），合成路径就必须能回退到 SAPI。
   实测踩坑：配了 `TTS_BASE_URL` 但服务没起 → 预检放行、节点 `connection refused`。
   现在回退并标 `backend=sapi-fallback` + 日志（真音频，不是假成功）。
2. **能力要分项**：`ready.audio = asr && tts` 会把「只有 TTS」的活挡死。要单边就查 `ready.tts` / `ready.asr`。
3. **媒体节点判定以执行器为准，但控制面执行器优先**：`type=step + executor=tts` 算媒体；
   `upscale` 两边同名，归控制面。子图类型必须归一（`step+tts`→`tts`），否则计算面报「不支持节点类型」。
4. **常驻进程从会话里拉必然会出问题**：孤儿引擎的 stdout 指向没人读的管道，写满就阻塞事件循环
   （实测每 ~8 秒 1.3s 卡顿 → governor 进 emergency → 拒绝一切新任务）。
   四平面一律 `node scripts/start-flowedge.cjs` 分离拉起，stdout 落文件。
5. **门禁不许「看起来全绿」**：复合任务可选阶段失败要落 `warnings[]`；governor 的 loop p99 要减掉测量地板
   （100ms 分辨率空转实测 111.4ms），否则空闲引擎常年 warn、`effectiveMax` 打折。

**一分钟自检**（四平面都在跑时）：

```bash
curl -s http://127.0.0.1:30000/api/flowedge/health          # 两个平面 ok
curl -s -X POST http://127.0.0.1:30000/api/flowedge/run \
  -H 'content-type: application/json; charset=utf-8' \
  -d '{"graph":"flowedge/27-工作站自检-配音到质检门","input":{"text":"两个平面已经合上。"}}'
```

期望：`status=success`，产物 `tts-*.wav` + `master-*.wav` + `qa-*.json`（verdict=pass）。

**本机 go 可用**（`%LOCALAPPDATA%\Programs\Go\bin\go.exe`，1.22.10，不在 PATH）：
改后端后 `python scripts/build-windows.py --skip-ui` → 停旧 compute → 覆盖 `运行\FlowEdge.exe` → `node scripts/start-flowedge.cjs`。

## 5.13 加模块只改三处（2026-10-22 模块契约上线后生效）

背景：`docs/系统评估-2026-10-21.md` 实测「同一节点类型字符串出现在 7–13 个文件」，
加一个节点要逐处改 switch，编译器全程不报错。判定类元数据已收进
`backend/service/nodespec.go` 的 `NodeSpecs`；表达类元数据收进
`backend/service/nodecontract.go` 的 `Modules`。

**要做的三件事**

1. 在 `registerBuiltinNodes`（`engine_v2.go`）注册执行体；
2. 在 `NodeSpecs`（`nodespec.go`）登记 `Side / Heavy / Media / Timeout / MaxTimeout`；
3. 在 `Modules`（`nodecontract.go`）登记 `Title / Role / Inputs / Outputs / KeyParams / Requires / GateLayers / Agent`。

前端再补 4 处配置（`config/graph.ts` 的 `PALETTE`、`NODE_OUTPUT_FIELDS`、`NODE_REQUIRES`，
`App.tsx` 的 `nodeTypes` 与 `defaultNodeData`），属性面板按需加一个区块。

**为什么必须三处都改**

- 漏 `NodeSpecs` → `TestNodeSpecsCoverRegisteredTypes` 直接红。
  运行期后果是调度器把它当"不吃显存"，和重任务一起并发起飞 → 24GB 卡 OOM。
- 漏 `Modules` → `TestModuleContractCoversRegistry` 直接红。
  运行期后果是任务书缺一段、依赖预检跳过它（跑进去才报连接错误）。
- `Media` 决定有没有血缘，`Heavy` 决定采不采显存峰值，`Side` 决定侧别感知调度。
  这三项漏一个都不会报错，只会在真机上偶发——所以宁可让测试红。

**模块契约的两条硬约束**

- **`Modules` 不许按类型写 switch**：端口、参数标签、门层、依赖全从表里读。
  `spec.go` 有测试扫描"不许出现任何节点类型字面量"；`preflight.go` 有测试扫描
  "不许 `switch n.Type` / `n.Type ==`"。谁破谁红。
- **模态只是 `PortKind`**（text/image/video/audio/json/any），不是节点分类。
  加一个新模态 = 加一个 PortKind 值 + 一条契约，调度/预检/任务书自动跟上。
  不许为"视频类节点""音频类节点"再建名单。

**不许再新增第四份名单**

- 侧别、重节点、血缘、超时**不许**再写 switch/map；要加维度就加到 `NodeSpec` 里。
- 端口、参数标签、门层、依赖**不许**写在别处；要加就加到 `ModuleContract` 里。
- `TestNodeRegistry_HasAllBuiltins` 现在与 `NodeSpecTypes()` 对账，不再手抄类型清单——
  手抄清单本身就是一处会忘的维护点。

**故意没进两张表的**

- 参数校验（`nodeparams.go`）：每条校验带人话修复建议，是逻辑不是数据；
- 估算分类（`handler/estimate.go`）：3 处类型分支，收益低于改动风险。
它们各自只有一处，不构成"改 N 个文件"的问题。若将来长出第二份副本，再收进契约。

## 5.13.1 画布 → 任务书（Spec）

`GET /api/workflows/:id/spec?format=md|json` 把节点图编译成**可执行规格**：
执行顺序（拓扑分层）、每个模块吃什么吐什么、参数真实取值、验收门 + FAIL 条件、
产物清单、以及**图里没说清的地方**（无契约/孤立节点/必填输入无来源/成环/声明了门却无阈值）。

消费方三处，共用一份编译结果：前端「导出任务书」按钮、MCP `flowedge_get_spec`、脚本/CI。

写任务书时的三条铁律：

1. **只报事实**：参数值取 `node.data` 里真实写的，不补默认值。默认值住在引擎与
   `data/models.json` 里，复制一份就是第二份真相（「涡轮 8 步 vs 实际 12 步」的教训）。
2. **不确定就写出来**：任何"看起来齐了但其实没说清"的情况进 `Warnings` 并渲染到 §5。
   任务书的价值在于暴露空洞，不是把空洞糊过去。
3. **门必须有牙齿**：声明了门层却没写阈值 = 等于没设门，必须告警（`TestSpecWarnsWhenGatesDeclaredWithoutThresholds`）。


## 5.13.2 能力卡片与节点绑定（2026-10-22）

三类卡：**agent（角色）/ skill（确定性变换）/ workflow（整条流水线）**。
贴在节点上，存在 `node.Data["binding"] = {kind, id}`。

**卡片是内容，不是结构**——这一条决定了后面所有实现：

- 同一张「短剧导演」卡可以贴在任何文本节点、也可以同时贴三处；
- 做成节点类型 = "节点数 × 卡片数" 的组合爆炸，且 spec / 端口 / 调度都被迫认识每个内容变体；
- 所以贴卡**只改参数与提示词**，不改端口、不改侧别、不改依赖。

**2026-10-22 起改为四段管线**：`node.Data["bindings"] = [{kind, id, stage, order}]`
（旧单选 `binding` 读取时自动迁移，不写回）。

| stage | 语义 | 上限 | 落点 |
|---|---|---|---|
| `input` | 上游文本预处理（翻译/清洗/抽字段） | 多张串行 | `executeNode` 派发**前**统一做（`applyInputStage`），**所有节点生效** |
| `body` | 换角色设定 | **1 张** | 仅 AI 推理节点（`execLLMNode`）；其他节点面板置灰并说明 |
| `output` | 产出整形（只改 `text`） | 多张串行 | `executeNode` 派发**后**统一做（`applyOutputStage`），**所有节点生效** |
| `replace` | 整节点换成一条工作流 | **1 张** | `executeNode` 拦截 → `executeInnerWorkflow`（有嵌套深度保护） |

`replace` 与 `body` 互斥（本体都被替换了，角色没有落点）；`replace` 与 input/output 可共存。

**输入/输出段为什么放在 `executeNode` 而不是各执行体里**：20 个节点各写一遍必然漏，
且新加节点默认没有；预处理只关心"文本进节点前长什么样"，与节点内部逻辑无关。
- 输入段**不改原 upstream map**——同层并行时它可能还被别的节点读着；
- 输出段只改 `text` 字段，`image` / `tool_calls` / `_usage` 原样保留。

**卡片必须留痕**：每张卡执行后往 `output._cards` 写 `{stage,kind,id,ok,error}`。
没有它，卡片是黑盒——贴了"中译英"跑完只看到一段英文，不知道是卡翻的还是模型本来就会；
某张卡报错会被静默降级吞掉（预处理失败不阻断主流程，这是有意的）。

**保存期三条硬拦**（`handler/workflow.go: validateBindings`）：
工作流卡指向不存在的工作流 → 400；指向自己（无限递归）→ 400；
`sub_workflow`/`loop` 节点再贴工作流卡（两套 workflow_id 打架，用户以为"再套一层"实际是"换掉"）→ 400。

**两条不许破的约束**

1. `BindingOf` 必须校验 kind ∈ {agent, skill, workflow}；未知 kind 当未绑定。
   （否则它会一路透传到执行期，症状是"贴了卡但什么都没发生"。）
2. 画布徽章用 `withCardBadge` HOC 统一包装，**不许逐个改 20 个节点组件**——
   改组件会得到 20 份略有差异的实现，新加节点必然漏一次。

**视觉约定**（形状先于颜色，色弱也能分辨）：agent=圆+外环 · skill=六边形 · workflow=三层堆叠。

## 5.13.3 自定义模型端点（LLM Profile）

`data/llm-profiles.json` 与 `data/models.json` 是**两张表**：
一个管"调哪个 HTTP 端点"，一个管"跑什么权重"。混在一起迟早互相污染。

- profile 只存**凭据名**，密钥在 `credentials.json`（已加密）——导出工作流不会带 key；
- 鉴权用 `authTransport` 在 http.Client 上统一注入。**不要**在每个 `.Post()` 前面加头：
  LLM 有 6 处请求构造点，逐处加等于 6 个漏写点；
- 自定义端点**不走流式**（协议未必一致，硬套会得到空结果）。

## 5.14 后期链三个会静默出错的坑（2026-10-21 实测）

涉及 `post_production` / `subtitles` / `video_edit.go` 的所有 ffmpeg 拼接。

1. **`amix` 默认 `normalize=1` 会把你混进去的音轨压到听不见。**
   它是按输入路数做归一化，两路各 -6dB；再叠侧链压缩，BGM 只剩 -34dB，
   而时长、流列表、音量检测「有音频」全部正常。
   **混音一律 `normalize=0`**，音量自己用 `volume=` 控制。
2. **`duck=false` 时不能保留 `asplit`。** 旁路输出没人消费 → ffmpeg 直接失败：
   `Filter asplit has an unconnected output`（退出码 -22）。
   开关型滤镜必须让图上**每个标签都有人消费**，分支要真的分支。
3. **路径不许进滤镜字符串。** `subtitles=C://...`、`fontfile=D:/...` 这类写法会被
   盘符冒号与中文路径打爆。做法固定为：**临时目录 + `cmd.Dir` + 裸文件名**。

判定纪律补一条：**「有音轨」不等于「听得见」**。
验收混音要看**目标频段能量**（如 BGM 是 660Hz 就测 `bandpass=f=660:w=30` 的 mean_volume），
宽带 `volumedetect` 会被 AAC 底噪和主音轨泄漏污染，做出「没变化」的假结论。

同理「侧链开了」不等于「侧链起作用」：必须 **duck on/off 各渲一次** 比目标频段，
差值 <3dB 就等于没做。参数（threshold/ratio）要扫，不要拍。

## 8. 配置即数据（2026-10-21，P0–P5 改造后的新契约）

> 这一节是给「未来的我」看的：模型名不在代码里了，别再去 `comfy_media.go` 里改字符串。

### 8.1 三份配置文件（都在 `data/`，首次启动自动落默认）

| 文件 | 管什么 | 代码入口 |
|---|---|---|
| `models.json` | 逻辑角色 → 权重文件 + 默认参数 + 精度降级阶梯 | `service/model_bundle.go` |
| `instances.json` | 模型 → ComfyUI 实例路由；`exclusive_group` 互斥 | `service/instances.go` |
| `plugins/<name>/plugin.json` | 运行期节点（http / script 两种） | `service/plugin_loader.go` |

**改模型只改 JSON**：`PUT /api/models/bundles {"role":"video.h3.fl2va","files":{"unet":"..."}}`，
未写的字段继承内置默认。改完 `object_info` 缓存自动作废。

### 8.2 三条铁律（违反会静默跑坏）

1. **内置默认必须与历史硬编码逐字相同**。`TestBuiltinBundlesMatchLegacyHardcode` 锁着这件事。
   任何「顺手换个默认模型」都会让升级变成静默换模型，用户只会看到「画风变了」。
2. **探不到 `object_info` ≠ 预检通过**。预检失败不阻断（老实例/畸形 JSON/代理都会失败），
   但 400 时会把「预检未执行」附在错误里。别把这条降级逻辑删掉，也别把它当成通过。
3. **跨参数不给数字**。`/api/estimate` 跨档位时只给上界并标 `extrapolated`；
   样本 <3 降 confidence；采样漏峰标 `peak_underestimated` + `source=model_size`。
   加新估算维度前先问：这个数字是测到的还是算出来的？说不清就别输出。

### 8.3 显存采样只看执行窗口

`vram_sampler.go` 在**重节点自己的执行窗口内**以 1 秒间隔采 `nvidia-smi`，节点结束即停。
不要改成全局常驻轮询：空闲时烧 CPU，而且 5 秒全局间隔本来就采不到 3 秒的出图峰值。

### 8.4 单镜重跑不许改持久化工作流

`POST /api/runs/:runId/shots/:index/reroll` 只把 `reroll` 写进**这次调用的内存节点图**
（`resumeFromWithOverride`）。写回 DB 会让 `reroll: 3` 永久留下——以后每次正常运行都重拍第 3 镜，
而且没人记得为什么。

### 8.5 新增的对外协议面

- `/v1/*`：OpenAI 兼容（出图/转写/配音/对话）。**不做 DAG 编排、不经显存仲裁**，
  与编排任务并跑时仍可能互相挤显存——这是刻意取舍，端点要能当无状态服务器用。
- `FlowEdge.exe --mcp`：stdio JSON-RPC，**瘦客户端转发**到本机引擎。
  不要在 stdio 进程里初始化全栈（会抢显存和 sqlite 文件）。

### 8.6 沙箱黑名单要按 import 解析，不能按字符串包含

`checkCodeSafety` 曾用 `strings.Contains(code, "\"os\"")`，结果注释里提到 `import "os"`
就把整个插件拒了。现在走 `importedPackages()`：解析 `import "x"` / `import alias "x"` /
`import (...)` 块，先剥行注释。**扩展黑名单时不要再退回字符串包含**。

### 8.7 comfy_workflow 只吃 API 格式，这是权衡不是疏忽

**实测（2026-10-21）**：`D:/AI/ComfyUI/blueprints` 89 个官方模板，**89 个都是 UI 格式，
0 个 API 格式**，且全部带 `definitions.subgraphs`（节点 `type` 是 UUID）。

所以「吃社区 workflow」这个说法**不成立**，对外必须写「需要 Export (API)」。
不做 UI→API 转换的理由：要递归展开 subgraph + 用 `/object_info` 的 schema 把
`widgets_values` 数组映射回输入名，猜错会产出「跑得通但接错线」的图——
那比直接拒绝难查十倍。

`TestParseRealBlueprints` 用这 89 个真实文件守着检测器：**必须 89/89 拒绝、0 泄漏**。
泄漏 = 把 UI 格式当 API 格式放行 → 提交空图 → 用户得到一句看不懂的成功。

### 8.8 已验证 vs 仍未验证（2026-10-21 验证轮，零 GPU）

**已验证（mock ComfyUI + mock OpenAI 上游 + 真实 blueprint 数据）**：
Wan 全链路落盘、OOM 降级（分辨率 ×0.75 且帧数不动）、精度阶梯（换 unet 不改构图）、
降到底如实报原错、多实例路由（含把 H3BaseURL 指向死端口仍正确的反证）、
互斥组串行、OpenAI 非流式/流式/错误体。

**真机已验证（2026-10-21 16:00，ComfyUI 0.31 / 833 节点 / flux 512×512）**：
`comfy_workflow` 端到端出图两次，改 `data.prompt` + `6.text={{prompt}}` 后画面从
「红苹果木桌」变成「蓝色陶瓷茶壶」——**内容不同才是绑定生效的铁证**，
不是「跑通了但用 JSON 原值」。`bindings_used` 回传 `["3.seed","6.text"]`。
显存峰值 **19677 MiB = 80.0%**（1–2 秒采样，0 次超 90%）；第二次运行 8 秒（模型已驻留）。

**仍未验证**：带自定义节点包的真实 workflow、第三方客户端 UI 点击联调、真实多精度权重。

**做真机 GPU 验证时的红线**：显存峰值必须 < 90%（22118 MiB）。做法是全程
1–2 秒采样 `nvidia-smi` 并统计峰值，而不是跑完再回看。flux 512² 实测 19.7GB 是安全档；
H3 608×352 实测 21.8GB = 89%，已经在红线边上，不要拿它做验证。

验证方法记在这里，下次照做：**真机验证一律 `PORT=8099` 起第二个实例**，
不要动用户的 8080；验完 TaskStop + 删 `dist/data` + 确认 8080 仍 200。

---

## 8.9 工具链：四类工具一处发现（原 AGENTS §7.1，2026-10-23 迁入）

> 用户那句「我 GitHub 之前装了很多啊，怎么这里一个都没有」的通用解。

**一条铁律**：新增工具不许再各写一套发现逻辑。skill / MCP / 插件 / 智能体统一走
`service.Toolchain`（`GET /api/toolchain`），每次请求重扫，不缓存目录树。

**三个必须分开的概念**（混了就一定出现假状态）：

| 概念 | 含义 | 混掉的后果 |
|---|---|---|
| `enabled` | 配置层开没开 | 停用的条目从列表里消失 → 用户以为被删了 |
| `usable` | 现在真能不能用 | 「配置了」被当成「装好了」，跑到节点上才炸 |
| `managed` | 引擎能不能写 | 越界改 WorkBuddy/Cursor 的配置 → 整台机器的工具链一起坏 |

**来源与可写性**：`data/mcp.json`、`data/plugins/`、`data/agents.json` 可写；
其余（`~/.workbuddy/mcp.json`、`~/.cursor/mcp.json`、`~/.claude.json`、
Claude Desktop、`~/.dsh/mcp.json`、项目级 `.mcp.json`）只读，启停写
`data/mcp-overrides.json` 覆盖层，原文件零改动（`TestToolchain_ExternalMCPToggleDoesNotTouchSource` 盯着）。

**探活不是探端口**：本地 MCP 服务器是 stdio（没有端口）。
「能连上」= 握手成功 + `tools/list` 返回。结果缓存 5 分钟——每次探活都要 spawn
一个 node/npx，全量重探等于把用户机器上的 node 起一遍。

**插件热注册**：`engine.hotNodes` 是独立表（RWMutex）。内置节点表启动后只读，
往里写会和 `executeNode` 的读并发 → Go 直接 fatal（不是"偶发不一致"那么轻）。
启动与「重新扫描」共用 `Toolchain.Reload`，语义是**整体替换**：停用的插件节点必须立刻消失。

**插件的最后一步**：前端 `nodeTypes` 是静态表，未知类型要落到 `GenericNode`，
否则插件装好、引擎能跑，拖到画布上是空白——生态在最后一步断掉。
