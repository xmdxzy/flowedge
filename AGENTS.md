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
- [ ] 多段片：`prompt_warnings` 里没有 `scene_lock_missing` / `scene_lock_drift`
- [ ] ASR 转写确认无人声 / 台词正确
- [ ] 报耗时报的是**实测区间**，不是估算
- [ ] 告诉用户「改哪个参数可以重跑」，而不是只给一个成片

### 0.12 六类反复犯的失误（2026-10-20 复盘，每条都对应一次真实返工）

> 这一节不是自我批评，是把「下次怎么不问第二遍」写死。违反任一条 = 返工。

#### ① 把二手的当成一手的

**错**：拿到"优化 H3 提示词"的任务，直接沿用社区博客（reapi.ai / aifinitee）的摘要，把
`subject_definitions / retention_analysis` 那套当官方要求写进文档。真相是官方仓库
`MiniMax-AI/MiniMax-H3/skills/h3-prompt-writing/references/` 有两份原文，而且**本地就躺着**
官方工作流 JSON、`nodes_minimax_h3.py` 源码、`MINIMAX_H3_LOCAL.md`。

**根因**：搜索到了"看起来权威"的总结就停下，没有枚举本地已有资产。这就是 §0.1 那个错的原样复发。

**规则**：任何"标准/规范/最佳实践"类判断，按顺序取证——**本地源码与官方范例 → 官方仓库原文 →
官方文档页 → 社区博客（只能当索引）**。写进文档前必须能指到原文那一条；指不到就是在猜。

#### ② 验收维度选错，用"工具没报错"当"没问题"

**错**：`verify-media` 报 `av_drift=0`，就认为音画同步。用户听完说"不同"，一量 TTS 才发现
每句自带 0.44s 前导静音。

**根因**：av_drift 只验容器时长差，不验"语音实际起点 vs 计划时点"。**工具能测的维度 ≠ 用户能感知的维度。**

**规则**：验收清单必须包含"用户能感知的项"，并且都要有数值：
- 视频：帧数 / 时长 / 接缝（已有）**+ 语音起点 vs 计划时点（差 ≤0.1s）+ 跨段平均亮度 AvgY 光比**
- 音频：人声与底噪的 SNR（≥8dB）+ ASR 可懂度
- 提示词：`auditH3Prompt` 全绿 **+ 至少跑一次真实 LLM 并把它的错误记下来**

#### ③ 用调参数掩盖结构错误

**错**：混音顺序错了（先 amix 后 loudnorm），我先把 `--ambience-gain` 从 0.18 调到 1.2 硬顶，
结果底噪盖住人声、ASR 直接转不出字，比原版更糟。

**规则**：指标不对时先问"链路顺序/结构是否错了"，再问"参数该调多少"。
**参数化 ≠ 修根因**；连续两次调参没改善，必须停下来读代码。

#### ④ 一次改多个变量

**错**：重拍段 2/3 时同时改了提示词、lead/pause、混音链、环境声素材。出现"段 2 变成白天"时
分不清是重拍的锅还是调色的锅，多花一轮定位。

**规则**：一轮只动一个变量（这条用户早就定过，我违反了）。同一轮改 ≥2 项时，
必须在日志里列出变量清单并保留 A/B 版本，否则不许说"已验证"。

#### ⑤ 换轨/装配这类"看起来无害"的操作不验前提

**错**：换音轨用 `-shortest` 让容器比视频流长 4 帧，`stitch` 按容器时长算 xfade offset →
offset 越过上一段终点 → 整段画面被丢掉，而成片**时长仍然报 30s**，只看时长根本发现不了。

**规则**：任何"重封装/mux/拼接"之后，立刻 `ffprobe` 核对 **`nb_frames` 与视频流 duration**；
装配器一律用**视频流时长**算偏移，不用容器时长。

#### ⑥ 停服务前没先准备好恢复手段

**错**：为了让改动生效先杀了 8080 引擎，然后才发现 bat 在 Git Bash 下编码失败、PowerShell 拉起的
进程会被会话清理、COM 方式被安全策略拦。最后服务停着交回用户手动启动。

**规则**：§0.10 已经写明"agent 不负责挂着"。所以**动任何常驻服务之前，先确认恢复路径可用**
（谁启动、怎么启动、路径是否有中文/编码问题），再停。验证不了就别停。

### 0.13 汇报口径（避免"看起来都做完了"）

结论先行 → 给数值证据 → **把"没验证的部分"单独列一节** → 下一步。

不许把"代码已落地"和"效果已验证"写在同一个语气里。未跑过的路径就写"未验证"，不要用
"应该可以""理论上"糊过去。

## 1. 模型与目录

- **所有权重只放 `D:\AI\Models`**（约 102GB），项目目录不存权重。加新模型先放这里，再用配置指过去。
- ComfyUI 通过 `D:\AI\ComfyUI\extra_model_paths.yaml` 读这个根；换路径后必须用 `/object_info` 的枚举验证读到了。
- `D:\AI\hy3d`（混元3D）保持原位，不要动。
- 同盘搬模型用 `Move-Item`（秒级）；Git Bash 里 `robocopy` 的参数要写 `//MOVE`。

## 2. 显存（24GB 卡 + UE5 同时开着）

- **默认同一时刻只让一侧驻留**：扩散（flux 17GB）/ 语言（14B 9GB 或 VLM 6GB）/ 语音（whisper 3GB）。
  - **小模型例外（2026-10-21 实测后放宽）**：卸载后重载 flux 要 **183.3s**，生成只要 **3.0s**
    （61 倍）。所以「装得下就别卸载」比任何采样参数优化都值钱。
    `canCoexistWithLanguage` 在**真实空闲显存 > 模型体积×1.3 + 1GB** 时跳过卸载
    （flux 17GB + VLM 6GB ≈ 23GB，24G 卡放得开）。
  - **保守底线不变**：查不到显存、查不到模型体积、或开关关闭（`SetVRAMCoexist(false)`）
    一律退回卸载。判断偏乐观会把已驻留的扩散模型挤出去，代价是又一次 183s，
    比不优化更糟——**「不确定」永远等于「不共存」**。
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
- 场景「写了锁」不等于「锁住了」→ 多段锁句必须**字节全等**（`auditSceneConsistency`）；
  文字上"差不多"就等于没锁，接缝处会换场地而帧数时长全部正常。
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

## 5.13 加节点只改两处（2026-10-21 结构收口后生效）

背景：`docs/系统评估-2026-10-21.md` 实测「同一节点类型字符串出现在 7–13 个文件」，
加一个节点要逐处改 switch，编译器全程不报错。现在判定类元数据已收进
`backend/service/nodespec.go` 的 `NodeSpecs`。

**要做的两件事**

1. 在 `registerBuiltinNodes`（`engine_v2.go`）注册执行体；
2. 在 `NodeSpecs`（`nodespec.go`）登记 `Side / Heavy / Media / Timeout / MaxTimeout`。

前端再补 4 处配置（`config/graph.ts` 的 `PALETTE`、`NODE_OUTPUT_FIELDS`、`NODE_REQUIRES`，
`App.tsx` 的 `nodeTypes` 与 `defaultNodeData`），属性面板按需加一个区块。

**为什么必须两处都改**

- 漏 `NodeSpecs` → `TestNodeSpecsCoverRegisteredTypes` 直接红。
  运行期后果是调度器把它当"不吃显存"，和重任务一起并发起飞 → 24GB 卡 OOM。
- `Media` 决定有没有血缘，`Heavy` 决定采不采显存峰值，`Side` 决定侧别感知调度。
  这三项漏一个都不会报错，只会在真机上偶发——所以宁可让测试红。

**不许再新增第三份名单**

- 侧别、重节点、血缘、超时**不许**再写 switch/map；要加维度就加到 `NodeSpec` 里。
- `TestNodeRegistry_HasAllBuiltins` 现在与 `NodeSpecTypes()` 对账，不再手抄类型清单——
  手抄清单本身就是一处会忘的维护点。

**故意没进 NodeSpecs 的两类**

- 参数校验（`nodeparams.go`）：每条校验带人话修复建议，是逻辑不是数据；
- 依赖预检（`preflight.go`）：要区分 h3 / 主实例 / CPU 服务，且要产出「一键修」动作。
它们各自只有一处，不构成"改 N 个文件"的问题；`handler/estimate.go` 的类型分支同理（3 处）。
若这三处将来各自长出第二份副本，再收进 `NodeSpec`。

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

## 6. 已知不可用（别重复投入）

| 项 | 状态 | 原因 |
|---|---|---|
| ~~minimax_h3~~ | **可用** | 必须走 `D://AI//ComfyUI-H3` 独立工作树 + 独立 venv（torch 2.14）；默认实例（torch 2.6）跑同图会 OOM |
| qwen_image_2.1 出图 | 不可用 | VAE 被误判成 WanVAE（4 vs 12 通道），0.29/0.31 都没修 |
| ~~VLM 主题一致性质检~~ | **已补齐** | vision 节点支持「参考主题」入参（节点填，或自动取上游 prompt/original_prompt），不再按画面自评；新增 `select` 择优节点：K 个候选 → VLM 逐个打分 → 取最高。缺 VL 模型时报 `config_missing`（界面给「打开环境面板」） |

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
