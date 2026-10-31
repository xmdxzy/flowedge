# HuggingFace 全域 · 多模态视频生成「编排工作流」调研

> 调研员：HuggingFace 泳道子智能体 ｜ 建立于 2026-10-31 ｜ 规范依据：`00-任务简报与规范.md`
> 取证方式：`huggingface.co/api/models|spaces|papers` + `<repo>/raw/main/README.md`（全部 2026-10-31 当日实取）
> **证据级别标记**：【API】HF API 一手实测 ｜【卡】模型卡/README 原文 ｜【作】作者自述 ｜【推】由权重体积推算（非实测）｜【未】未取到
> **本泳道不含**：单模型质量排行、排行榜刷分、与 FE 无关的通用视频模型。

---

## ① 一句话结论

**HF 全域里真正「能串成一条出片流水线的」不是榜单头部那几支单体大模型，而是三类编排件：**
① **MiniMax-H3 的多镜连续性节点包**（`context_pin` 潜空间接力 + 记忆库 + 全套 24GB 默认值，与 FE 现役 H3 同源）；
② **LTX-2.3 / JoyAI-Echo 这一支「音视频联合 + fp4/int8 分档 + 逐层 offload」的消费卡路线**（22B 有 20.03–22.81GB 的量化档，正好卡 24GB）；
③ **audio-driven 数字人里仅有的轻量落地档**（EchoMimicV3 1.3B/6.65GB；LongCat-Video-Avatar-1.5 INT8 走 2 卡官方 quickstart）。

**最值得接到 FE 的 3 条**：**(1) `joeygambino/MiniMax-H3-Multishot-Workflow` v2.7.0** —— 直接把 FE 的「逐镜生成 + xfade 拼接」换成潜空间接力，且是 H3 同源、24GB 默认值、有成套失败处理文档；**(2) `jdopensource/JoyAI-Echo` 1.5 fp4（22.81GB，8 步 DMD，≤7 记忆槽）** —— 给 FE 的 `text2video` 加第二条「一次出画面+声音+跨镜记忆」的后端；**(3) `BadToBest/EchoMimicV3`（apache-2.0，6.65GB）或 `LongCat-Video-Avatar-1.5` INT8** —— 补 FE 唯一的「口型由 TTS 驱动而非模型即兴」缺口。

**一条最重要的反向结论**：HF 上没有一个 Space 能替代 FE 的编排层。所有「编排型」Space 都是**单链路的**（一次跑一个模型或一条固定链），FE 的 7 条流水线 + 队列分道 + 单侧驻留 + 质量门控在 HF 上没有对应物。**抄它们的参数与失败处理，别抄它们的架构。**

---

## ② 主体

### 2.1 音频驱动 / 数字人（优先级 1）

| 仓库 id | 参数量 | License | 权重体积【API】 | 显存门槛 | 单模型 / 可编排组件 | 现成 Space |
|---|---|---|---|---|---|---|
| `meituan-longcat/LongCat-Video-Avatar-1.5` | 未标注（base_model 6 shard） | **MIT** | bf16 base **≈29.4GB**；**int8 base ≈14.78GB**；whisper-large-v3 ≈7.5GB；dmd_lora 2.35GB | 【卡】官方 quickstart 用 `torchrun --nproc_per_node=2 --context_parallel_size=2`＝**双卡**；单卡 24GB 未验证【未】 | 可编排组件（AT2V/ATI2V/视频续接三段 stage；`--use_int8`、`--use_distill`、`--resolution 480P/720P`） | 无官方 Space；社区 ComfyUI 版 `vantagewithai/LongCat-Video-Avatar-1.5-GGUF-ComfyUI`（25316 dl） |
| `tencent/HunyuanVideo-Avatar` | 未标注 | 【未】（卡内未见 license 字段） | **HF repo 只有 `ckpts/llava_llama_image` 6 文件 17.34GB**，DiT 权重不在此 repo | 【卡】README 给 8 GPU 并行示例 | 可编排组件（角色图注入 + 情绪模块 AEM + 面部分离适配器 FAA → 多角色对话） | 无（官方 Playground 在 hunyuan.tencent.com） |
| `Quark-Vision/Live-Avatar` | **14B** | **apache-2.0** | 【未】（tree 未取） | 【卡】领域宣称 **5×H800 下 20 FPS**，4 步采样 | 系统级框架（Block-wise 自回归 → 10,000+ 秒流式），base 是 `Wan-AI/Wan2.2-S2V-14B` | 无 |
| `jdopensource/JoyAI-Echo`（1.5） | 22B 级（based on LTX-2.3） | LTX-2 community | **BF16 46.14GB / FP8 27.62GB / FP4 22.81GB**【卡】 | 【卡】提供「消费卡 profile」：逐层 DiT offload + 分块 Video VAE 解码 | **强可编排**：≤7 个有序 memory slot（参考图+音频）+ 首帧条件 + 成对跨模态记忆库 + **Director Agent** | 无官方 Space；社区 ComfyUI 路径 `joeygambino/joyai-echo-multishot-workflow` |
| `BadToBest/EchoMimicV3` | **1.3B** | **apache-2.0** | **6.65GB**（transformer 3.18GB + flash-pro 3.47GB）【API】 | 【推】单卡 24GB 非常宽裕 | 单模型（统一多模态/多任务人体动画） | 无（GitHub 有 Gradio） |
| `Wan-AI/Wan2.2-S2V-14B` | 14B | **apache-2.0** | 42.58GB（含 umt5-xxl 文本编码器 10.58GB + DiT 4×≈9.28GB）【API】 | 【推】bf16 需量化才能进 24GB | 单模型（音频驱动电影级；论文含长视频与精确口型编辑） | `Wan-AI/Wan2.2-S2V`（436 like） |
| `digital-avatar/ditto-talkinghead` | 未标注 | 【未】 | 【未】 | 【卡】测试环境 A100 + TensorRT 8.6.1（实时路线） | 单模型（实时口型，走 TensorRT 优化链） | Colab |
| LTX 生态口型 LoRA | — | 随 LTX | `elix3r/LTX-2.3-22b-AV-LoRA-talking-head`（79 like）、`AviadDahan/LTX-2.3-ID-LoRA-TalkVid-3K`（63）、`JinhyukJang/lipforcing`（5） | 随底模 | **可编排组件（LoRA 层）** | `linoyts/LTX-2-3-sync`（177）、`fffiloni/tts-hallo-talking-portrait`（255）、`Skywork/skyreels-a1-talking-head`（200）、`multimodalart/MoDA-fast-talking-head`（139） |

### 2.2 视频 + 音频联合生成（优先级 2，对标 FE 现役 H3）

| 仓库 id | 参数量 | License | 权重体积【API】 | 显存门槛 | 单模型 / 可编排组件 | 现成 Space |
|---|---|---|---|---|---|---|
| `Lightricks/LTX-2.3` | **22B** | LTX-2 community | bf16 **42.98GB**/变体；fp8 **27.5 / 27.14GB**；**nvfp4 20.21GB**；另有 unsloth GGUF | 【推】nvfp4 20.21GB 是唯一贴 24GB 的档；fp8 超 24GB 单独放不下 | 可编排组件：dev / distilled（8 步 CFG=1）/ distilled-lora-384 / **空间 x2、x1.5 + 时间 x2 上采样器** | `Lightricks/LTX-2-3`（486） |
| `Lightricks/LTX-2.5` | 22B | ltx-2.x community | bf16 39.13GB/变体；**comfy-int8-convrot 20.03GB**；**Gemma4-12B 文本编码器 bf16 24.46GB** | 【推】DiT 20.03GB 可进 24GB，但**文本编码器 24.46GB 必须量化/offload** | 可编排组件（tags 覆盖 8 种音频/视频组合，含 `image-text-to-audio-video`） | `Lightricks/LTX-2.5`（89） |
| `MiniMaxAI/MiniMax-H3`（FE 现役） | DiT **33.14B/分区**【卡】 | minimax-h3 community | 见 2.7 剪枝条目 | 20.5GB（FE 现役 W4A8，主会话实测） | 可编排组件（fl2va / ref2va / flf） | `multimodalart/minimax-h3-audio-to-video`（50） |
| `multimodalart/MiniMax-H3-Pruned` | DiT **20.11B/分区**（AdaLN 投影剪到可达秩） | minimax-h3 community | `transformer/` 66.28→**40.24GB**，`transformer_ref/` 同；`text_encoder/` 66.71→**52.48GB** | 【推】剪枝后叠 FE 的 W4A8 → 理论更省；**但必须 A/B**（FE 已知 PDD LoRA 叠 W4A8 是负优化） | **可编排组件（drop-in 替换 H3 分区）** | 无 |
| `tencent/HunyuanVideo-1.5` | **8.3B** | tencent-hunyuan-community | 【未】 | 【卡】宣称「消费者级 GPU 流畅运行」 | 单模型 —— **tags 只有 t2v/i2v，无音频**（不满足「一次出画面+声音」） | 无 |
| `Lightricks/LTX-2.3-22b-LoRA-Foley-V2A` | 随底模 | LTX-2 community | 【未】 | 随底模 | **可编排组件**：视频→音频副轨（画面已有，补 Foley） | — |
| `HKUSTAudio/Talker-T2AV` | 【未】 | 【未】 | 【未】 | 【未】 | 待考（8 like，证据不足） | — |

### 2.3 多镜 / 长视频一致性（优先级 3）

| 仓库 id | 类型 | License | 关键机制（【卡】原文） | 24GB 判定 | 值得接 FE 的理由 |
|---|---|---|---|---|---|
| **`joeygambino/MiniMax-H3-Multishot-Workflow`** v2.7.0（84 like） | ComfyUI 节点包 + 2 工作流 | apache-2.0 | **`context_pin`**：上一镜**末 22 帧以 raw latents 无缝注入**下一镜内部关键帧坐标（无 VAE 往返），重建的 0.92s 在解码时裁掉；配套 memory bank、boundary plate、`H3ChainNormalize` 色彩/纹理校平器 | 【作】「new defaults **tuned for 16-24 GB cards**」+「reserve-planning fixes from the **24 GB test lab**」；建议 checkpoint 为 ref2va `curve-Q5_1` GGUF（作者称 ~14GB） | **同源 H3、潜空间接力替代 xfade**、有成套失败模式文档 |
| `ethanfel/H3_Cinematic_Multishot_Coverage`（48 like） | 纯工作流 JSON | minimax-h3 community | 一张场景图 → **一次 H3 pass 出 8 个机位**（真剪辑点 + 精确切时码），抽稳定中段帧 + 拼 4×2 接触表；**只用 ComfyUI 核心节点**（`MiniMaxH3ReferenceToVideo` / `MiniMaxH3SigmaShift` / `ImageFromBatch` / `BatchImagesNode` / `ImageStitch`），124 帧 @24fps | 【卡】无自定义节点依赖 → 落地成本最低 | **零依赖**，可直接验证 FE 的「一镜多机位」 |
| `KlingTeam/MultiShotMaster` | 权重（框架） | 【未】 | 文本驱动镜间一致性 / 可变镜数与时长 / 定制主体+运动控制 / 背景驱动场景 | 【未】 | 论文级编排范式（SIGGRAPH Asia 方向） |
| `jdopensource/JoyAI-Echo` 的 memory slot | 模型内建 | LTX-2 community | ≤7 有序记忆槽 + 跨模态记忆库（外貌/**音色**/场景） | fp4 22.81GB【推】 | **音色与外貌一起跨镜携带** = FE「角色圣经 + 场景锁」的潜空间版 |
| `Lightricks/LTX-2.3-22b-IC-LoRA-Ingredients`（223 like） | IC-LoRA | LTX-2 community | 主体/场景成分条件 | 随底模 | 与 `linoyts/ltx2.3-ic-lora-ingredients-multishot`（267 dl）配套 |
| `Wan-AI/Wan2.2-Animate-14B`（1303 like） | video-to-video | **apache-2.0** | 角色替换/驱动（base `Wan2.2-I2V-A14B`）；GGUF 版 381343 dl | 【推】14B 需量化 | 换角/换人（FE 的 `image_edit` 只有图像级） |
| 相机控制族 | LoRA/IC-LoRA | 混合 | `Cseti/LTX2.3-22B_IC-LoRA-Cameraman_v1/v2`（62/52）、`Lightricks/LTX-2-19b-LoRA-Camera-Control-{Static,Dolly-In,Dolly-Out}`、`lovis93/Motion-Lora-Camera-Push-In`（100）、`stabilityai/stable-virtual-camera`（244）、`alibaba-pai/Wan2.2-Fun-A14B-Control-Camera` | 随底模 | 分镜表要「运镜」而非只「内容」时的最小改动 |
| 首尾帧控制 | Space/LoRA/节点 | 混合 | `linoyts/LTX-2-3-First-Last-Frame`（159）、`multimodalart/wan-2-2-first-last-frame`（676）、H3 multishot 的 **`flf_chain`** 模式 | 随底模 | FE 的 `shotlist` 已按「续接」思路设计（plan.consistency），flf 是其图像级替身 |

### 2.4 视频编辑 / 重绘 / 超分 / 插帧（优先级 4）

| 仓库 id | 类型 | License | 体积/门槛 | 可编排性 |
|---|---|---|---|---|
| `Wan-AI/Wan2.1-VACE-14B`（522 like）/ `Wan2.1-VACE-1.3B`（156） | Any-to-any 视频编辑（重绘/参考/编辑一体） | apache-2.0（Wan 系） | 【未】；有 `QuantStack/Wan2.1_14B_VACE-GGUF`（123490 dl） | **强**：单模型多任务，替代 FE 的 `inpaint` |
| `alibaba-pai/Wan2.2-VACE-Fun-A14B`（131） | 同上，Wan2.2 版 | 【未】 | 有 GGUF（21476 dl） | 强 |
| `netflix/void-model`（983 like） | 视频物体**与交互**删除（quadmask：主物体/重叠/受影响/背景 4 值） | **apache-2.0** | 【卡】**需要 40GB+ VRAM（A100）**；base `CogVideoX-Fun-V1.5-5b-InP` | 单模型（Space `sam-motamed/VOID`）；24GB 不行 |
| `decart-ai/Lucy-Edit-Dev`（368 like） | 文本引导视频编辑（5B） | **非商用** license | 【未】 | 单模型；有 ComfyUI 仓库 |
| `Lightricks/LTX-2.3-22b-IC-LoRA-DubIt`（157 like） | **口型对齐的配音重绘** | LTX-2 community | 随底模 | **高价值**：FE 改一句台词不必重拍整镜 |
| LTX IC-LoRA 编辑族 | LoRA | LTX-2 community | 随底模 | `In-Outpainting`(60)、`Clean-Plate`(114)、`lens_remover`（`JanKanta/…` 20）、`Restore`、`Refine-Details`(78)、`Deblur`(54)、`Relight`(39)、`Day-To-Night`(32)、`SDR-To-HDR`(51)、`HDR`(131)、`Water-Simulation`、`Alpha-Gen`(228)、`Layout-To-Render`(120) |
| `ByteDance-Seed/SeedVR2-7B` / `SeedVR2-3B` | 视频超分 | **apache-2.0** | 7B：**30.7GB×2**（ema / ema_sharp）+ vae 0.93GB【API】 | 3B 档【未】；ComfyUI 落地见 `numz/SeedVR2_comfyUI`（**731703 dl**, 328 like，含「Multi-GPU 独立 CLI」与自己的 Requirements 段） |
| `Lightricks/LTX-2.5-22b-IC-LoRA-Pixel-Spatial-Upscaler`（143） | 视频超分（LTX 内） | LTX-2 community | 随底模 | 与 LTX 时间/空间上采样器同一链，**不用额外驻留第二个模型** |
| `Comfy-Org/Frame-Interpolation`（**86392 dl**, 54 like） | 插帧 | 见卡 | 【未】 | 单模型；**FE 侧已是 `ffmpeg minterpolate`（CPU、零显存）** —— 换它会占显存，与「单侧驻留」纪律冲突（见 ③） |
| `akhaliq/frame-interpolation-film-style`（10 like） | FILM 插帧 | 【未】 | 【未】 | 同上 |

### 2.5 HF Spaces 上可参考的「编排型」Demo（优先级 5）

| Space | like | 硬件【API】 | 它到底编了什么 | 可抄的干货 |
|---|---|---|---|---|
| `techfreakworm/LTX2.3-Studio` | 365 | zero-a10g | **六模式（text · image · audio · lipsync · keyframe · style）共享同一条 ComfyUI All-In-One 工作流**；MIT；本地 MPS/CUDA 均可 | **`preload_from_hub` 是一份可直接用的 ComfyUI 权重清单**：`Kijai/LTX2.3_comfy diffusion_models/ltx-2.3-22b-dev_transformer_only_bf16`、`loras/…distilled-lora-dynamic…rank_105_bf16`、`text_encoders/ltx-2.3_text_projection_bf16`、`vae/LTX23_audio_vae_bf16`、`vae/LTX23_video_vae_bf16`、`vae/taeltx2_3`、`Lightricks/LTX-2.3-22b-IC-LoRA-Union-Control`、`google/gemma-3-12b-it-qat-q4_0-unquantized` |
| `linoyts/LTX-2-3-sync` | 177 | zero-a10g | **Portrait animation & lipsync with LTX 2.3** — 手写 pipeline，不用 diffusers | `app.py`（50KB）**把 `Lightricks/LTX-2` clone 到 pinned commit**，直接用 `ltx_core/ltx_pipelines`：`DISTILLED_SIGMA_VALUES`、`STAGE_2_DISTILLED_SIGMA_VALUES`、`TilingConfig`、`upsample_video`、`decode/encode_audio` 的 audio VAE、pose/depth processor —— **口型链的可复现骨架** |
| `linoyts/LTX-2-3-First-Last-Frame` | 159 | zero-a10g | 首尾帧受控生成 | 与 FE `shotlist` 的「续接」语义对齐 |
| `multimodalart/ltx2-audio-to-video` | 81 | zero-a10g | LTX-2 distilled + **给定音轨** → 画面；含音视频合流与「音轨比视频长就裁到视频长度」 | `save_video_with_audio()` 的 ffmpeg 合流写法；fps=24 约定 |
| `multimodalart/minimax-h3-audio-to-video` | 50 | 【未取到 gpu 字段】 | **H3 fl2va + 锁定音轨**（`AUDIO_CONDITION_MODE="locked"`：音轨行被锁死、不允许模型改） | **H3 的硬约束与约定**（一手）：`FPS=24`、`FRAMES_PER_CHUNK=17`、`LATENTS_PER_CHUNK=5`、`snap_frames()` 求 **17n+5**；`num_inference_steps` **含终点（终点不做模型评估）→ NFE = steps−1**；`TURBO_STEPS=7`（=6 NFE）/ `BASE_STEPS=28`；拆分部署（denoising half）；默认仓库 `multimodalart/MiniMax-H3-Pruned` |
| `zerogpu-aoti/wan2-2-fp8da-aoti-faster` | **3783** | zero-a10g | fp8 + **AOTI 编译** 加速 Wan2.2 | 「量化 + AOTI」这套加速组合的参照 |
| `hugging-apps/sbgrid-storyboard` | 59 | zero-a10g | `peculiar/sbgrid` LoRA × `krea/Krea-2-Turbo` → **8 格连贯分镜**；`startup_duration_timeout: 1h` | **分镜前置**：先出连贯 8 格再 i2v，是 FE `storyboard` 节点的另一条实现 |
| `multimodalart/wan-2-2-first-last-frame` | 676 | — | 首尾帧 | — |
| `Wan-AI/Wan2.2-S2V` | 436 | — | 音频驱动 | — |

### 2.6 HF Papers（只列能落到编排上的）

| arXiv id | 标题 | 落到哪条编排 | 来源 |
|---|---|---|---|
| **2601.03233** | LTX-2: Efficient Joint Audio-Visual Foundation Model | 「一次出画面+声音」的联合生成范式；LTX-2.3/2.5/JoyAI-Echo 的共同底 | `Lightricks/LTX-2.3` 卡内 `arxiv` 字段【API】 |
| **2608.23383** | JoyAI-Echo 1.5（Long-Horizon Audio-Visual） | **跨镜记忆槽**编排（外貌+音色+场景随镜携带） | `jdopensource/JoyAI-Echo` 卡【API】 |
| **2512.03041** | MultiShotMaster: A Controllable Multi-Shot Video Generation Framework | 镜间一致性 / 可变镜数与时长 / 主体运动控制 | `KlingTeam/MultiShotMaster` 卡【API】 |
| **2604.02296** | VOID: Video Object and Interaction Deletion | 交互感知 quadmask 重绘（删人连带删「被他碰倒的东西」） | `netflix/void-model` 卡【API】 |
| **2505.20156** | HunyuanVideo-Avatar | 多角色对话 + 情绪对齐（AEM/FAA） | `tencent/HunyuanVideo-Avatar` 卡【API】 |
| **2512.04677** | Live Avatar（Streaming Real-time Audio-Driven, Infinite Length） | 流式无限长数字人 + Block-wise 自回归 | `Quark-Vision/Live-Avatar` 卡【API】 |
| **2508.18621** | Wan-S2V: Audio-Driven Cinematic Video Generation | 电影级音频驱动 + 长视频 + 精确口型编辑 | `Wan-AI/Wan2.2-S2V-14B` 卡【API】 |
| **2411.19509** | Ditto: Motion-Space Diffusion for Controllable Realtime Talking Head | 实时口型的运动空间扩散 | `digital-avatar/ditto-talkinghead` 卡【API】 |
| **2507.03905** | EchoMimicV3: 1.3B Parameters are All You Need | **小参数多任务人体动画** | `BadToBest/EchoMimicV3` 卡【API】 |
| 2511.18870 | HunyuanVideo-1.5 | 轻量 T2V（**无音频**） | 卡内 `arxiv` 字段【API】 |

### 2.7 三条最值得接的路线（参数与失败处理）

#### (A) H3 多镜链节点包 —— `joeygambino/MiniMax-H3-Multishot-Workflow` v2.7.0

**它解决的问题与 FE 现在的做法正好对撞**：FE 是「逐镜生成 → xfade 重叠拼接 → 一致性门审 plan → 出片后抽帧测 U/V 漂移」。它是「上一镜末 22 帧 raw latents 直接接力 → 下一镜重建该重叠段 → 解码时裁掉 0.92s」。

【卡】关键参数表（原文提炼）：

| 参数 | 作者给的值/建议 | 对应 FE 的哪个痛点 |
|---|---|---|
| `continuity` | `context_pin`（默认）；`first_frame`；`seamless`/`seamless_tail` 是**遗留对比模式**（`seamless` 常被判读成硬切；`seamless_tail` 与 Motion-Context 冲突、装了就**采样前直接停**） | FE 的 `cut` 段丢画面、接缝跳变 |
| `seed_per_shot` | **保持 ON**（实测：单 seed 全链会让**脸和声音同时漂**） | FE 的 `shot-cache` 键与 seed 管理 |
| `chain_gain_control` | 链长 >≈5 镜设 `flatten`（否则纹理每个接缝**棘轮约 1.3×**，后段过锐） | FE 实测的接缝色彩/光比漂移 |
| `color_level` | `off` / `mvgd` / `scene`；用**第 1 镜已稳定尾部**做固定参考（邻镜对邻镜会累积漂移） | 与 FE「相邻两镜 U/V 漂移 >6 报 plan.color_shift」同构 |
| `self_anchor_voice` / `voice_ref` | 全链音色一致（v1.5 头号特性）；**v2.7.0 起支持 `voice_ref_2/3` 逐角色独立音色**，并**盲测过无串音** | FE 的 TTS 音色统一（f0 std < 5Hz）纪律 |
| `output_scale` | 解码后 Lanczos 缩放（分辨率≠细节；比原生快 **1.78×**，但更软） | FE 的 `width/height` |
| `upscale_model` | 可选 ESRGAN 类；**其合成纹理不进 memory bank** → 不会喂给锐化棘轮 | FE 的 `upscale` 节点 |
| `preview_first_shot` | 先只渲第 1 镜，可提前中止 | FE 的「首镜 3–4 分钟」估时模型 |
| `low_ram_master` | OFF；长链或系统 RAM <32GB 时 ON | 与 AGENTS 的「64GB 内存 commit 打满」同源 |
| `head_trim_frames` | 默认 14（**裁掉每镜最不稳的前 ~0.5s**，身份形变与口型误差集中在此） | FE 的 TTS 前导静音逐行裁（0.03–0.05s）是音频侧的同款思路 |

**失败处理（【卡】作者已归档）**：漏装任一依赖节点 → **ComfyUI 会校验整图所有节点类才愿意入队**（所以「删包」比「留空节点」更可行）；无 RES4LYF → `scheduler=beta`（作者标注了实测代价）；H3 与 LLM 抢卡 → writer 节点上的 `unload_model_after` 开关（Ollama 的 OpenAI 兼容端点**没有 `keep_alive` 字段**，不关的话模型会占满你的第 1 镜）；纯 GGUF 需 `ComfyUI-GGUF` + 跑一次 `apply_gguf_arch_patch.py`。

**已知缺陷（作者自述，2.6.0）**：长片**纹理棘轮未完全解决** —— 736×1280、抗漂全开时每接缝约 **+13% 细纹理**；<4 窗口（30–40s）轻微，7 窗口肉眼可见。**建议 extend take 暂限 ~4 窗口**。

---

## ③ 与 FE 的对接点

> FE 现状（一手，读 `backend/service/nodespec.go` + `data/pipelines.json`）：
> 节点 **14 种** = `start` / `end` / `quality_loop` / `llm` / `storyboard` / `text2video` / `text2image` / `image2image` / `inpaint` / `upscale` / `shotlist` / `comfy` / `code_video` / `tts`。
> 流水线 **11 条** = `image` / `image_batch` / `image_edit` / `video` / `video_from_image` / `film` / `knowledge_film` / `voice` / `storyboard` / `film_from_plan` / `text`。
> 相关实现文件：`comfy_media.go`（H3 构图与 media 节点分派）、`comfy_media_batch.go`（多镜合批）、`media_nodes.go:190`（`execUpscaleNode`＝RealESRGAN 图像级）、`media_nodes.go:392-505`（`interp` → `ffmpeg minterpolate`）、`shotlist.go`（逐镜循环 + `interp`）、`shotplan.go`、`shot_cache.go`、`video_edit.go`、`engine_v2.go:1124`（节点注册表）。

| # | 能力 | 接到 FE 哪条流水线 / 哪个节点 | 要改哪个文件 | 24GB 吃不吃得下 | 缺什么依赖 |
|---|---|---|---|---|---|
| 1 | **H3 多镜潜空间接力**（context_pin + memory bank + plate） | `film` / `film_from_plan` 的 **`shotlist` 节点**（替换其内部的 xfade 拼接语义）；开关走 `shotlist.data` 新增字段（照抄相邻字段的 `bind: sl-1`，**没有 bind 的字段永远不会落到节点 data**） | 构图：`backend/service/comfy_media.go`、`comfy_media_batch.go`；逐镜循环与 `transitionOverlapD`：`shotlist.go`；缓存键：`shot_cache.go`；字段：`data/pipelines.json` + `nodecatalog.go` | 【作】**是**（作者称默认值 tuned for 16–24GB，且有 24GB test lab）；但作者推荐的 `Q5_1` checkpoint 实测文件 **24.14GB** → 需靠 activation reserve/offload，**未验证**；FE 现役 W4A8（≈20.5GB）更稳 | **`ComfyUI-H3-Motion-Context`（FE 现在没有）**；`RES4LYF`（`beta57` 调度器）；`sol-attn` + `minimax-h3-blockcache-T8`；`ComfyUI-Custom-Scripts`；GGUF 路径另需 `ComfyUI-GGUF`。装在 `D:/AI/ComfyUI-H3/custom_nodes/`（先例：`flowedge_batch.py` 被删过一次，批量能力当场全废——自定义节点包属于「删了能力就没了」的硬依赖，不是可选优化） |
| 2 | **JoyAI-Echo 1.5 fp4**（8 步 DMD + ≤7 记忆槽 + 音色/外貌跨镜） | `video` / `film` 的 **`text2video` 节点**做**第二后端**（`video.h3.*` 旁边加 `video.echo.*`）；`film` 的 `characters` 字段语义升级为 memory slot 播种 | 后端分派 `comfy_media.go`；模型配置 `运行/data/models.json`（现存 `video.h3.ref2va.unet` 即此处）；字段 `data/pipelines.json` | 【推】fp4 权重 **22.81GB** → 单卡 24GB 需 `sequential_offload` + `resident_blocks`≈20–24（【作】给的 24GB 档位），**未实测**；官方另有「逐层 DiT offload + 分块 Video VAE 解码」配置档 | 官方 `configs/inference.fp4.yaml`（非 ComfyUI 原生）；**文本编码器必须是真 `Gemma-3-12B-it`**（用社区的 "joyecho" Gemma GGUF 会**出两个人**）；社区 ComfyUI 路径 `joeygambino/joyai-echo-multishot-workflow` 的 `INSTRUCTIONS.md` |
| 3 | **数字人口型**（EchoMimicV3 / LongCat-Avatar-1.5 INT8） | **新增节点**（建议名 `talking_head`），插在 `tts` 之后、`shotlist` 用 H3 出镜之前；也可先做成独立流水线（`film` 的旁支），不碰现役台词镜路径 | **两处**：`backend/service/nodespec.go`（进 GPU 道；**别**加进 `cpuOnlyNodeTypes` 白名单，否则会跟 H3 抢显存而不受仲裁）+ `backend/service/nodecatalog.go`（端口/字段/成本） | 【推】EchoMimicV3 **6.65GB 很宽裕**；LongCat-Avatar INT8 base 14.78GB + whisper 7.5GB + lora 2.35GB **≈24.6GB 单卡放不下**，且官方 quickstart 是 2 卡 | EchoMimicV3 走 diffusers；**复用 `D:/AI/envs/torch-cu124` 共享底座、不要塞进 ComfyUI venv**（ComfyUI 是 transformers 5.14.1，Audio8 就栽在版本冲突上）。必须走 FE 的 `releaseDiffusionAndWait` / `vram_idle.go` 回收（与 TTS 的 15.2GB 占卡是同一类问题） |
| 4 | **视频级重绘/编辑**（Wan2.1-VACE-14B / LTX IC-LoRA In-Outpainting / Clean-Plate / lens_remover / DubIt） | **`inpaint` 节点**从图像级扩到视频级；`DubIt` 单独对应「改台词只重绘口型」 | `backend/service/media_nodes.go`、`video_edit.go`、`comfy_media.go:462` | 【推】VACE-14B 需 GGUF；**LTX IC-LoRA 系最省**（LoRA 随底模，不额外驻留） | **DubIt 优先级最高**：它把 FE「TTS 改一句 → 重拍整镜（6–7 分钟/镜）」降成局部重绘 |
| 5 | **视频超分**（SeedVR2-3B / LTX Pixel-Spatial-Upscaler） | **`upscale` 节点**（现为 RealESRGAN 图像级，`media_nodes.go:190`）加视频分支 | `media_nodes.go`、`nodecatalog.go`、`vram_sampler.go:99/162` 与 `oom_degrade.go:34`（**这两处按节点类型估显存，加分支必须同步**，否则峰值估错） | 【API】SeedVR2-**7B** 每档 30.7GB **放不下**；3B 档体积【未取到】。LTX Pixel-Spatial-Upscaler **随 LTX 底模，不新增驻留模型 → 24GB 最友好** | `numz/SeedVR2_comfyUI`（ComfyUI 节点 + 多卡 CLI）；LTX 上采样器需 LTX 后端在位 |
| 6 | **LTX-2.3/2.5 做第二 T2V 后端** | `video` / `text2video` | 同第 2 条 | 【API】LTX-2.3 **nvfp4 20.21GB** 最贴；LTX-2.5 **int8-convrot 20.03GB**；fp8 27.5GB **放不下**；**文本编码器**（LTX-2.5 的 Gemma4-12B bf16 **24.46GB**）必须量化或 offload | LTX-2 官方 monorepo（`ltx-core`/`ltx-pipelines`，Python ≥3.12 / CUDA >12.7 / torch ≈2.7）；**帧数与分辨率约束不同**：LTX 要求宽高被 **32** 整除、帧数 **8n+1**，而 FE 现用的 **124 = 17×7+5** 是 H3 VAE 的 17n+5 约束（一手，见 `multimodalart/minimax-h3-audio-to-video`）→ `shotlist` 的 frames 参数**必须按后端分支**，不能共用一套常数 |
| 7 | **H3 剪枝分区**（`multimodalart/MiniMax-H3-Pruned`） | **不是新能力，是给现役 H3 省显存** | `运行/data/models.json` 的 `video.h3.*` 指向剪枝分区 | 【API】DiT **33.14B→20.11B**、`transformer/` 66.28→**40.24GB**、`text_encoder/` 66.71→**52.48GB**（截到 Qwen3-VL 第 50 层，位级等同）；【卡】作者称输出**无可辨变化**、已发布 LoRA 全部可加载 | **必须先 A/B 再上**：FE 已有 `scripts/h3-bench.py` / `scripts/h3-r2v-bench.py` 台架；AGENTS 的既有判负是「PDD LoRA 叠 W4A8 = 84.7s vs 42.5s」——**剪枝叠 W4A8 属同类交叉，不测不许上** |
| 8 | **插帧**（Comfy-Org/Frame-Interpolation） | `film` 的 **`interp` 字段**（现走 `ffmpeg minterpolate`，CPU、零显存） | `media_nodes.go:392-505`、`shotlist.go:886` | 换 GPU 插帧会**抢显存**，与 FE「单侧驻留 / GPU 道串行」纪律直接冲突 | **不建议换**：AGENTS 已记「RIFE 镜像 404；插帧改 ffmpeg minterpolate（CPU、零显存）」。要换先在 `interp` 开关上做 A/B（FE 有 `--low_vram`/`--interp` 曾被证明「从未被节点读取」的前科） |
| 9 | **一镜多机位**（`ethanfel/H3_Cinematic_Multishot_Coverage`） | `shotlist` 的**低成本验证路径** | 无需改代码 —— 走 **`comfy` 逃生舱** 提交其 JSON（只用核心节点，零自定义依赖） | 【卡】无自定义节点依赖，风险最低 | 仅需 ComfyUI 版本够新（含原生 `MiniMaxH3ReferenceToVideo` / `MiniMaxH3SigmaShift`） |

**一句话给主会话的取舍**：**先做 1 和 9（同源 H3、零/低依赖、能直接对照现有接缝缺陷），再做 3（补唯一的真缺口，但要独立 venv），2/6 留作后端替换的长期项（都要动 `models.json` + 帧数约束分支）。**

---

## ④ 可复现动作

```bash
# 0) 全部证据可用同一套 API 复现（无 token；本机 shell 有 http_proxy，外网请求走代理是正常的）
curl -s "https://huggingface.co/api/models?filter=audio-to-video&sort=likes&limit=20"
curl -s "https://huggingface.co/api/spaces?search=lipsync&sort=likes&limit=15"
curl -s "https://huggingface.co/api/models/<repo_id>/tree/main?recursive=true"   # 取 weights 体积（blobs=true 需要鉴权，会 401）
curl -s "https://huggingface.co/<repo_id>/raw/main/README.md"                    # 模型卡原文

# 1) H3 多镜链（路线 A）
#    ComfyUI-Manager 里搜 "H3 Multishot"，或：
git clone https://github.com/jlucasmcrell/ComfyUI-H3-Multishot  D:/AI/ComfyUI-H3/custom_nodes/ComfyUI-H3-Multishot
#    + ComfyUI-H3-Motion-Context（context_pin 必需）/ RES4LYF / sol-attn / blockcache-T8 / Custom-Scripts
#    装完按节点包 INSTALL.md 走 apply_gguf_arch_patch.py（仅 GGUF 路径）
#    注意：节点包 GitHub 在 jlucasmcrell 名下，HF 仓库在 joeygambino 名下（同一作者的两个发布面）

# 2) H3 量化档体积（一手实测，用于判断 24GB）
#    joeygambino/MiniMax-H3-GGUF → fl2va / ref2va 各：Q5_1 24.14GB、Q4_0 18.50GB
#    FE 现役 W4A8 ≈20.5GB（主会话实测）→ Q4_0 才是同量级；Q5_1 需 offload

# 3) JoyAI-Echo 1.5（路线 B）
huggingface-cli download jdopensource/JoyAI-Echo --include "echo15_fp4/*" --local-dir <目标>
#    官方 inference: python inference.py --config configs/inference.fp4.yaml

# 4) 24GB 档位参考（【作】，非实测）
#    JoyAI-Echo ComfyUI 路径：24GB 卡 → fp8 或 Q8_0，sequential_offload ON，resident_blocks ≈20–24
#    16GB → Q5_0；32GB+ → bf16。系统 RAM / Windows pagefile 往往才是瓶颈。

# 5) FE 侧不跑管线，只读现役事实（本次已做，可复核）
rg -n 'Type: "' backend/service/nodespec.go
python -c "import json;d=json.load(open('data/pipelines.json'));print([p['id'] for p in d['pipelines']])"
```

---

## ⑤ 没验证的部分（不许含糊）

1. **所有「显存吃不吃得下」都是我按权重文件体积推算的（级别【推】），没有一条在本机 24GB 卡上实测过。** 未跑任何 HuggingFace Space、未下载任何新权重、未启动/修改 ComfyUI（任务规范禁止）。
2. **`LongCat-Video-Avatar-1.5` 单卡 24GB 可行性未验证**，而且官方 quickstart 用 `--nproc_per_node=2 --context_parallel_size=2`（2 卡），我无法从 README 判断单卡 INT8 是否能跑。它的 bf16 base ≈29.4GB 是我从 6 个 shard 求和得到的（【API】）—— 但**参数量未标注**，我没有从 `config.json` 反推。
3. **`HunyuanVideo-Avatar` 的 HF repo 只有 LLaVA 图像编码器 17.34GB**，DiT 权重不在该 repo；**它的 license 我没取到**（卡内无 license 字段，README 只说 "will be released"）。任何「用 HunyuanVideo-Avatar」的说法在权重层面目前不成立。
4. **`Lightricks/LTX-2.5` 与 `Lightricks/LTX-2.3-22b-IC-LoRA-Ingredients` 的 README 返回 401（gated）**，我只有 `cardData.tags`/`license_name`；LTX-2.5 的**参数量、帧数约束、音视频条件写法全部未取到**。2.2 表里 LTX-2.5 的字段是从 tags 推的。
5. **H3 多镜链的 24GB 保证只是作者自述**（「tuned for 16-24 GB」「24 GB test lab」）。作者推荐的 `curve-Q5_1`（称 ~14GB）**不在** `joeygambino/MiniMax-H3-GGUF` 的 tree 里（该仓库只有 Q5_1 24.14GB / Q4_0 18.50GB 四个文件）→ 「~14GB」这条我**没有找到对应文件**，属未取到。「Q5_1 面向 24–32GB」与「文件本身 24.14GB」之间的差额靠 activation reserve 吸收，**未验证**。
6. **`KlingTeam/MultiShotMaster` 权重体积与可用性未取**；它的 license 字段缺失 → 商用性未知。
7. **`netflix/void-model` 40GB+ 门槛是官方 README 原文（A100）**，我没有验证 24GB 上的任何降级路径（base 是 5B 的 CogVideoX-Fun-InP，理论上量化可能可行 —— 属猜测，**不写成结论**）。
8. **HF Papers 我只读了模型卡里的 `arxiv` 字段与标题**，没有读任何一篇论文全文，因此 2.6 表的「落到哪条编排」是从标题+模型卡摘要归纳的，非论文精读。
9. **`EchoMimicV3` 6.65GB 能否直接在本机跑没测**；它的 transformers 版本要求我没查（一个 `bad2best` 仓库可能要求 transformers<5，与 ComfyUI 的 5.14.1 冲突 —— 这是从 Audio8 的历史教训外推的**猜测**，需实测）。
10. **FE 现有 `comfy` 逃生舱能否直接承载上述工作流 JSON 未验证**：`nodespec.go` 里有 `comfy` 节点类型，但我没有读它的执行实现，不知道它是否支持自定义节点包、是否受 `flowedge_batch.py` 那类屏障节点约束。
11. **「ComfyUI 会校验全图所有节点类才入队」这条是我从节点包 README 的依赖说明读到的（【卡】）**，与 FE 自身的图校验链（结构→端口→参数→依赖预检）是否等价、冲突，**未验证**。
12. 本文件里**没有任何播放量/星数/评分**是从二手文章抄的；likes/downloads 全是 2026-10-31 当日 HF API 实测值（会随时间变）。

---

## ⑥ 来源清单

**FE 内部一手（本地源码）**
- `backend/service/nodespec.go`、`backend/service/nodecatalog.go`、`backend/service/media_nodes.go`、`backend/service/comfy_media.go`、`backend/service/shotlist.go`、`data/pipelines.json`、`AGENTS.md`

**HuggingFace API（2026-10-31 一手）**
- 模型列表/标签：[api/models?filter=audio-to-video](https://huggingface.co/api/models?filter=audio-to-video&sort=likes&limit=20)、[api/models?search=multi-shot](https://huggingface.co/api/models?search=multi-shot&sort=likes&limit=15)
- 权重体积：[api/models/&lt;id&gt;/tree/main](https://huggingface.co/api/models/meituan-longcat/LongCat-Video-Avatar-1.5/tree/main?recursive=true)
- Spaces：[api/spaces?search=lipsync](https://huggingface.co/api/spaces?search=lipsync&sort=likes&limit=15)、[api/spaces?search=ltx](https://huggingface.co/api/spaces?search=ltx&sort=likes&limit=15)

**关键仓库与模型卡**
- [meituan-longcat/LongCat-Video-Avatar-1.5](https://huggingface.co/meituan-longcat/LongCat-Video-Avatar-1.5)（MIT / DMD2 8 NFE / AT2V·ATI2V·续接 / Whisper-Large）
- [vantagewithai/LongCat-Video-Avatar-1.5-GGUF-ComfyUI](https://huggingface.co/vantagewithai/LongCat-Video-Avatar-1.5-GGUF-ComfyUI)
- [tencent/HunyuanVideo-Avatar](https://huggingface.co/tencent/HunyuanVideo-Avatar) ｜ [arXiv 2505.20156](https://arxiv.org/pdf/2505.20156.pdf)
- [Quark-Vision/Live-Avatar](https://huggingface.co/Quark-Vision/Live-Avatar) ｜ [arXiv 2512.04677](https://arxiv.org/abs/2512.04677)
- [jdopensource/JoyAI-Echo](https://huggingface.co/jdopensource/JoyAI-Echo) ｜ [arXiv 2608.23383](https://arxiv.org/abs/2608.23383) ｜ [Director Agent](https://github.com/jd-opensource/JoyAI-Echo/tree/main/echo_longvideo/Director_Agent)
- [BadToBest/EchoMimicV3](https://huggingface.co/BadToBest/EchoMimicV3) ｜ [arXiv 2507.03905](https://arxiv.org/abs/2507.03905)
- [Wan-AI/Wan2.2-S2V-14B](https://huggingface.co/Wan-AI/Wan2.2-S2V-14B) ｜ [arXiv 2508.18621](https://huggingface.co/papers/2508.18621)
- [digital-avatar/ditto-talkinghead](https://huggingface.co/digital-avatar/ditto-talkinghead) ｜ [arXiv 2411.19509](https://arxiv.org/abs/2411.19509)
- [Lightricks/LTX-2.3](https://huggingface.co/Lightricks/LTX-2.3) ｜ [Lightricks/LTX-2.5](https://huggingface.co/Lightricks/LTX-2.5) ｜ [LTX-2 Community License](https://github.com/Lightricks/LTX-2/blob/main/LICENSE-2)
- [tencent/HunyuanVideo-1.5](https://huggingface.co/tencent/HunyuanVideo-1.5)
- [multimodalart/MiniMax-H3-Pruned](https://huggingface.co/multimodalart/MiniMax-H3-Pruned)
- [joeygambino/MiniMax-H3-GGUF](https://huggingface.co/joeygambino/MiniMax-H3-GGUF)

**多镜 / 长片编排（本泳道核心）**
- [joeygambino/MiniMax-H3-Multishot-Workflow](https://huggingface.co/joeygambino/MiniMax-H3-Multishot-Workflow) ｜ [ComfyUI-H3-Multishot（GitHub）](https://github.com/jlucasmcrell/ComfyUI-H3-Multishot) ｜ [ComfyUI-H3-Motion-Context](https://github.com/NikoDemon80/ComfyUI-H3-Motion-Context)
- [ethanfel/H3_Cinematic_Multishot_Coverage](https://huggingface.co/ethanfel/H3_Cinematic_Multishot_Coverage)
- [joeygambino/joyai-echo-multishot-workflow](https://huggingface.co/joeygambino/joyai-echo-multishot-workflow)（含 `INSTRUCTIONS.md` 的 per-VRAM 表与失败模式表）
- [KlingTeam/MultiShotMaster](https://huggingface.co/KlingTeam/MultiShotMaster) ｜ [arXiv 2512.03041](https://arxiv.org/abs/2512.03041)
- [Wan-AI/Wan2.2-Animate-14B](https://huggingface.co/Wan-AI/Wan2.2-Animate-14B)

**编辑 / 重绘 / 超分**
- [Wan-AI/Wan2.1-VACE-14B](https://huggingface.co/Wan-AI/Wan2.1-VACE-14B) ｜ [alibaba-pai/Wan2.2-VACE-Fun-A14B](https://huggingface.co/alibaba-pai/Wan2.2-VACE-Fun-A14B)
- [netflix/void-model](https://huggingface.co/netflix/void-model) ｜ [arXiv 2604.02296](https://arxiv.org/pdf/2604.02296) ｜ [VOID Space](https://huggingface.co/spaces/sam-motamed/VOID)
- [decart-ai/Lucy-Edit-Dev](https://huggingface.co/decart-ai/Lucy-Edit-Dev) ｜ [lucy-edit-comfyui](https://github.com/DecartAI/lucy-edit-comfyui)
- [ByteDance-Seed/SeedVR2-7B](https://huggingface.co/ByteDance-Seed/SeedVR2-7B) ｜ [numz/SeedVR2_comfyUI](https://huggingface.co/numz/SeedVR2_comfyUI)
- [Lightricks/LTX-2.3-22b-IC-LoRA-DubIt](https://huggingface.co/Lightricks/LTX-2.3-22b-IC-LoRA-DubIt) ｜ [In-Outpainting](https://huggingface.co/Lightricks/LTX-2.3-22b-IC-LoRA-In-Outpainting) ｜ [Clean-Plate](https://huggingface.co/Lightricks/LTX-2.3-22b-IC-LoRA-Clean-Plate) ｜ [Relight](https://huggingface.co/Lightricks/LTX-2.3-22b-IC-LoRA-Relight)
- [Lightricks/LTX-2.3-22b-IC-LoRA-Ingredients](https://huggingface.co/Lightricks/LTX-2.3-22b-IC-LoRA-Ingredients) ｜ [Union-Control](https://huggingface.co/Lightricks/LTX-2.3-22b-IC-LoRA-Union-Control) ｜ [Motion-Track-Control](https://huggingface.co/Lightricks/LTX-2.3-22b-IC-LoRA-Motion-Track-Control)
- [Comfy-Org/Frame-Interpolation](https://huggingface.co/Comfy-Org/Frame-Interpolation)

**Spaces（编排型）**
- [techfreakworm/LTX2.3-Studio](https://huggingface.co/spaces/techfreakworm/LTX2.3-Studio) ｜ [ltx2.3-AIO-generator（GitHub, MIT）](https://github.com/techfreakworm/ltx2.3-AIO-generator)
- [linoyts/LTX-2-3-sync](https://huggingface.co/spaces/linoyts/LTX-2-3-sync) ｜ [linoyts/LTX-2-3-First-Last-Frame](https://huggingface.co/spaces/linoyts/LTX-2-3-First-Last-Frame)
- [multimodalart/ltx2-audio-to-video](https://huggingface.co/spaces/multimodalart/ltx2-audio-to-video) ｜ [multimodalart/minimax-h3-audio-to-video](https://huggingface.co/spaces/multimodalart/minimax-h3-audio-to-video)
- [zerogpu-aoti/wan2-2-fp8da-aoti-faster](https://huggingface.co/spaces/zerogpu-aoti/wan2-2-fp8da-aoti-faster) ｜ [hugging-apps/sbgrid-storyboard](https://huggingface.co/spaces/hugging-apps/sbgrid-storyboard)
- [multimodalart/wan-2-2-first-last-frame](https://huggingface.co/spaces/multimodalart/wan-2-2-first-last-frame) ｜ [Wan-AI/Wan2.2-S2V](https://huggingface.co/spaces/Wan-AI/Wan2.2-S2V)

**论文（HF Papers 页）**
- [LTX-2 (2601.03233)](https://huggingface.co/papers/2601.03233) ｜ [JoyAI-Echo 1.5 (2608.23383)](https://huggingface.co/papers/2608.23383) ｜ [MultiShotMaster (2512.03041)](https://huggingface.co/papers/2512.03041) ｜ [VOID (2604.02296)](https://huggingface.co/papers/2604.02296)