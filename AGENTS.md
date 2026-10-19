# FlowEdge 项目规则（给未来的自己/其他 agent）

> 这些都踩过一次，别再踩第二次。详细证据链见 skill `local-multimodal-ops`。
> **出片先读 `docs/多模态出片手册.md`**：选路 / 时长分配 / 提示词模板 / 验收 / 排错，一页查完。

## 0. 作业纪律（最高优先级）

> 下面第 1–7 节是**环境坑**，改一次就没了。这一节是**判断方式错**，不写下来会反复犯。
> 每条都附了当时的真实错误；违反任一条视为返工。

### 0.1 下「不可用」结论前必须找反证

我在 H3 上犯过：只做显存算术（最小组合 34.3GB > 24GB），就断言「24G 跑不了，日常用 Wan 替代」。
真相是 H3 有独立 worktree `D:/AI/ComfyUI-H3` + 独立 venv（torch 2.14），608×352×124 帧 120s / 21.8GB
就跑得很轻松。整条判断错在**没有枚举实例**，不是错在算术。

**动作**：任何「跑不了 / 不可用」之前，按顺序找三个反证：

1. **历史产物**——`ComfyUI/output/`、`运行/data/generated/`。有成品就是铁证，比任何推断都强。
2. **同类实例**——`ls D:/AI | grep -i comfy`，看有没有第二个工作树/venv；比对它们的 torch 版本。
3. **被注释掉的参数**——旧启动脚本里注释掉的项，往往就是当前跑不起来的真正缺口。

找不到反证才允许下结论，且措辞必须是「**在当前这个实例上不可用**」，而不是「不可用」。

### 0.2 报错字符串不是根因

- `OOM` → 我归因显存。实际是跑错实例（主实例 torch 2.6 跑 H3 图）。
- `'NoneType' object has no attribute 'Params'` → 我归因模型文件选错。实际是 `comfy-kitchen` 版本错位。

**动作**：看到报错先问「这个报错是在什么环境下产生的」，核对 **venv / torch / 端口 / 工作树** 四件套，
确认是同一套环境之后，再谈显存和权重。

### 0.3 耗时估算只能同参数外推

30 秒片子我说 15 分钟，实跑 40 分钟。错因：拿 608×352 的 118s/段，去推 736×416 @20 步的 240s/段，
跨分辨率外推 >2 倍；还漏了插帧、LLM 分镜、实例启动。

**动作**：报时间必须逐项拆——生成（宽高 × 步数 × 段数）+ 插帧 + LLM + 实例启动 + 拼接 + 装配；
不跨参数外推，不报单个数字，报**区间 + 最坏情况**。

### 0.4 新开关上线必须 A/B，否则不算验证

`low_vram` / `interp` 两个字段从未被节点读取（JSON bool 没解析），我却报过「低显存模式验证成功」——
那只是裸跑成功。参数生效是独立命题。

**动作**：新参数必须**开/关各跑一次**，比较耗时与峰值显存，并在日志里打印实际生效值。
只说「跑通了」不等于「开关起作用了」。

### 0.5 视频验收四件套（缺一不可）

`cut` 用 0.04s 淡化 < 一帧 → **第二段画面整段丢失，而时长和音频都正常**。只查 duration 永远发现不了。

**动作**：交付前跑

```bash
python scripts/verify-media.py 成片.mp4 --asr --frames .tmp/frames
```

覆盖：流列表 + 时长 + **精确帧数** + 音画时长差 + 中段静音缺口 + 响度 + ASR 转写。
出现 `FAIL` 一律不许交付。脚本退出码 0/1/2 = 通过/警告/硬失败，可直接做流水线门禁。

### 0.6 「服务没起」和「没有结果」必须分开

两个真实案例：

- ASR 服务不可达 → 返回空文本 → 看起来像「检测到无人声」→ 验收等于没做。
- 本机 shell 常驻 `http_proxy=http://127.0.0.1:5751`，`curl 127.0.0.1:8080` 会被代理劫持返回 **502**，
  看起来像引擎挂了，其实服务好好的。

**动作**：本地端口探测一律 `curl --noproxy '*'`；任何验收工具都必须显式区分「**不可达**」与「**空结果**」，
不可达时报 FAIL 而不是 WARN。

### 0.7 图构建禁止硬编码节点 id，也禁止全局常数

- `saveImage` 写死源节点 `"8"`，超分图的源是 `"2"` → 400 校验失败。
- 单节点固定 180s 超时 → 合法的批量/长视频任务被误杀。

**动作**：上游 id、超时、尺寸、步数全部参数化；超时按「类型 × 工作量」估算，不许出现魔法常数。

### 0.8 迁移权重是「N 处配置」动作，不是一处

搬到 `D:/AI/Models` 只改了主实例的 `extra_model_paths.yaml`，H3 工作树那份还指着旧路径 →
H3 一个模型都看不到。**我把它误诊成「H3 装不下」。**

**动作**：迁移前 `grep -rl extra_model_paths D:/AI`，逐份改；改完用各自端口的 `/object_info`
枚举验证，主实例和 H3 实例**各验一次**。

### 0.9 加速类改动先量化，再决定是否保留依赖

SageAttention 装完才发现是负优化（206s vs 120s，还连带装 triton）；RIFE 镜像是 404，白试一轮。

**动作**：先找零成本替代（插帧用 `ffmpeg minterpolate`，CPU、显存零占用）；
必须装的话先在单条样本上对比，负优化就卸载并记录进「别再装」清单。

### 0.10 常驻服务不许挂在会话进程里

引擎以会话后台任务启动 → 会话结束即被回收（日志停在启动后 20 秒）。
托管子进程（ComfyUI / ASR / TTS）会跟着一起消失。

**动作**：交付型常驻服务只用 `运行\1-启动 FlowEdge.bat`（detached）或计划任务；
agent 只负责探活、配置、修复，不负责「帮你挂着」。每次接手先探活，不要假设还活着。

### 0.11 交付自检（5 分钟，做完再说「完成」）

- [ ] `verify-media.py` 无 FAIL
- [ ] 抽接缝前后各 1 帧看过（cut 段尤其）
- [ ] ASR 转写确认无人声 / 台词正确
- [ ] 报耗时报的是**实测区间**，不是估算
- [ ] 告诉用户「改哪个参数可以重跑」，而不是只给一个成片

## 1. 模型与目录

- **所有权重只放 `D:\AI\Models`**（约 102GB），项目目录不存权重。加新模型先放这里，再用配置指过去。
- ComfyUI 通过 `D:\AI\ComfyUI\extra_model_paths.yaml` 读这个根；换路径后必须用 `/object_info` 的枚举验证读到了。
- `D:\AI\hy3d`（混元3D）保持原位，不要动。
- 同盘搬模型用 `Move-Item`（秒级）；Git Bash 里 `robocopy` 的参数要写 `//MOVE`。

## 2. 显存（24GB 卡 + UE5 同时开着）

- **同一时刻只让一侧驻留**：扩散（flux 17GB）/ 语言（14B 9GB 或 VLM 6GB）/ 语音（whisper 3GB）。
- 换侧：先卸载 → 轮询 `vram_free` ≥ 16GB → 再继续。卸载返回不等于显存归还。
- ComfyUI `/free` 必须带 `{"unload_models":true,"free_memory":true}`。
- 语音服务默认 CPU（`ASR_DEVICE=cpu` / `TTS_DEVICE=cpu`），把显存留给图像与 LLM。
- 批量出图（>8 张且中间不切 LLM）可去掉 `--disable-smart-memory` 换速度，但要接受换侧变慢。
- 任何显存相关改动**必须实测峰值**（`nvidia-smi` 采样），不接受「应该没问题」。

## 3. 进程与参数

- 改 ComfyUI 启动参数前**先 stop 再 start**：依赖管理器会复用已在跑的进程，旧参数会一直生效。
- 引擎重启会连带杀掉托管的 ComfyUI/ASR/TTS（预期行为）；重启后要重新 `POST /api/deps/{name}/start`。
- **依赖启动等待要按类型给**：`readyTimeout()` —— ASR/TTS 300s、`comfy_h3` 180s、其余 120s。
  这里曾写死 3s，导致「先在面板点启动、再跑工作流」这条最常见路径必然报「3s 内未就绪」（H3 冷启 ~50s）。
- Ollama 不在托管范围，需要手动 `OLLAMA_MODELS=D:\AI\Models\ollama ollama serve`。

## 4. 下载与离线

- 本地已有权重时一律 `HF_HUB_OFFLINE=1`（否则企业代理下 502 → 服务启动即退出）。
- 走镜像下载：`HF_ENDPOINT=https://hf-mirror.com` + `HF_HUB_DISABLE_XET=1`（Xet 会 401）。
- 装 torchaudio 必须与 torch 版本对齐（`2.6.0+cu124` ↔ `2.6.0+cu124`），否则音频/视频节点整块不注册。

## 5. 判定纪律

- 节点「装了」不等于「注册了」→ 查 `/object_info` 的节点 id 与总数。
- 视频「生成了」不等于「有画面」→ `ffprobe` 看 stream 列表（video + audio 都要有）。
- 模型「迁完了」不等于「读得到」→ 查 loader 的枚举列表。
- 报错原文优先于任何文档；同一错误在两个不同权重上都出现，就别再换文件试。

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

### 节点用法
`text2video` 节点填：
- `ref_audio`：一段 wav 路径（念稿音频）
- `ref_images`：参考图路径，换行分隔（≤9，锁角色外貌）
两者任一非空即自动切到 `h3_r2v`，不能与首帧引导（`image_path`）同时用。

### 下一步（还没做）
对话场次应该**一条提示词多镜头**：`[Shot 1] ... [Shot 2] At 00:04.500, ...`，
一次生成 ≤15s（362 帧以内），而不是现在的逐镜多次生成。
逐次生成做不到：台词跨镜连续、音景不重启、跨镜音效（杯响/呼吸）自然衔接。

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

### 四个必踩的坑（都已在图/脚本里规避）
1. **传参要平铺**：`'{"brief_file":...}'`，不要包一层 `{"input":{...}}`——否则 `run.input` 多一层，插值全空，路由静默走兜底。
2. **下游节点不能用 `{{input.x}}`**：那只在触发节点有效，后续拿到的是上游输出。一律用 `{{vars.x}}`（吸收输入里的标量）。
3. **shell 节点别传中文路径**：`cmd.exe` 转码会把 `运行\...` 弄坏。路径处理放进 Python 脚本内部（`--report-dir`），不要 shell 再转一手。
4. **`write` 执行器有路径沙箱**（只能写序策项目内）：跨目录落盘用 shell + 脚本。

### 验收门
- 台词模式：`verify-media --asr --expect "<原话>" --gate` —— ASR 必须转出脚本原话
- 氛围模式：`verify-media --asr --no-speech --gate` —— 检出人声即 FAIL
- 两道门都通过才到 `hitl` 人工签收

## 5.10 多段成片与角色一致性（2026-10-19 实战：30 秒金丹斗法）

### 分段：按内容给时长，不要均分

**不要为了凑总时长把每段都设成同一个数**——这是我犯过的省事错误，代价是节奏被削平：
对峙和收招用不了 10 秒，交锋却需要更长。

做法：先按叙事功能定时长 → 吸附 `17k+5` 网格 → 核对 `sum(frames)/24 − 0.2×(段数−1)`。

| 叙事功能 | 时长 | 帧数 |
|---|---|---|
| 建立/定场 | 6–8s | 158 / 175 / 192 |
| 对话单回合 | 6–10s | 158 / 192 / 226 |
| 交锋主体 | 11–15s | 277 / 294 / 328 / 362 |
| 收招定格 | 9–11s | 226 / 243 / 260 |

30 秒三段的实际分配（优于均分）：192 + 277 + 260 = **29.98s**。
`shot-film.py` 支持每段 `frames`，并会核对总长（差 >0.75s 告警）。

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

### 耗时（实测，608×352 / 12 步 / 2 张参考图 / 243 帧）
| 段 | 类型 | 墙钟 |
|---|---|---|
| 1 | 台词（1 句） | 311s |
| 2 | 氛围（无台词） | 258s |
| 3 | 台词（2 句，提示词最长） | 556s |
合计 ≈ 18.8 分钟 + 装配 1 分钟。**按 5–9 分钟/段估，不要按 2 分钟估。**

### 一键复现
```bash
python scripts/shot-film.py --plan data/film/jindan/plan.json
```
序策图：`content/02-H3多段成片-定妆图锁角色`（trigger → shot-film → 门 → 人工签收）

## 6. 已知不可用（别重复投入）

| 项 | 状态 | 原因 |
|---|---|---|
| ~~minimax_h3~~ | **可用** | 必须走 `D://AI//ComfyUI-H3` 独立工作树 + 独立 venv（torch 2.14）；默认实例（torch 2.6）跑同图会 OOM |
| qwen_image_2.1 出图 | 不可用 | VAE 被误判成 WanVAE（4 vs 12 通道），0.29/0.31 都没修 |
| VLM 主题一致性质检 | 部分 | 视觉节点拿不到上游 start 的 prompt；需加「参考主题」入参 |

## 7. 构建与验证

```bash
export PATH="$PATH:/c/Users/Administrator/AppData/Local/Programs/Go/bin"
python scripts/build-windows.py            # 前端 + 单文件 exe → dist/FlowEdge.exe（31.4MB）
python scripts/build-windows.py --skip-ui  # 只重编后端（不复制前端，见下方警告）
cd backend && go test ./...                # 112 个测试
```

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
