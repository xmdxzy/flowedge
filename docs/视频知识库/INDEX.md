# 视频知识库 · 总索引（INDEX）

> 建立 2026-10-31。本目录是 FE（FlowEdge）视频工作流的**外部知识底座**：
> 每份文档都回答同一个问题——「这条知识能不能落到 FE 的某条流水线 / 某个节点 / 某个门」。
> **读法**：先看本页的「行动清单」，需要证据再点进对应文档的具体章节。**不要整目录通读。**

---

## 一、文件清单

| 编号 | 文档 | 覆盖 | 一手程度 |
|---|---|---|---|
| 00 | [任务简报与规范](00-任务简报与规范.md) | FE 上下文 / 交付规范 / 工具与渠道状态 | — |
| 01 | [HuggingFace 多模态视频生成编排](01-HuggingFace-多模态视频生成编排.md) | H3 同源多镜链、音视频联合、数字人口型、视频编辑、HF 编排型 Space | API 实时抓取（未下载权重） |
| 02 | [代码视频方案全景](02-代码视频方案全景.md) | Remotion / Motion Canvas / Revideo / Manim / HyperFrames / lemo 横向对比 | 源码 + 官方文档（零渲染） |
| 03 | [爆款科普剪辑编排实测](03-爆款科普剪辑编排实测.md) | YouTube 4 条官方字幕全量 + 17 条头部样本 + B站 7 条 | **一手**（agent-reach 实抓） |
| 04 | [论文前沿·多镜一致性与编排](04-论文前沿-多镜一致性与编排.md) | 端帧锚定、参考图筛选、AVC-Reward 三级口径 | arXiv 一手（已抽验 REEL） |
| 05 | [开源编排生态与 AI 剪辑](05-开源编排生态与AI剪辑.md) | ComfyUI 编排节点、AI 剪辑、VBench-2.0、CPU 道候选节点 | GitHub API 实时（零实验） |
| 07 | 视频生成模型 SOTA 全景 | 闭源/开放权重两条线、价格与时长、趋势 | 进行中 |
| 08 | 商业编排平台与 API 生态 | fal/Replicate/n8n/Dify、剪映草稿 JSON、一键出片产品 | 进行中 |
| 09 | 剪辑编排与时间线自动化 | OTIO/EDL/FCPXML、自动剪辑算法、字幕规范 | 进行中 |
| 10 | 平台算法与爆款分发 | 推荐机制、封面标题、AIGC 标注合规 | 进行中 |

---

## 二、行动清单（按性价比排序，**这是本目录最重要的产出**）

### A 级 · 零成本、纯文本可判、当天能落

| # | 动作 | 依据 | 落点 |
|---|---|---|---|
| A1 | 新增「开场钩子」审计：前 10 条 cue 内必须有绝对化/反常识断言，**0 次提问** | 03：4/4 样本提问数 0、反差标记 4/0/2/1 | `consistency_plan.go` 加 `plan.hook_no_contrast` / `plan.hook_question`（与 `plan.no_cold_open` 同处） |
| A2 | 新增「信息密度带」审计：**词/分 135–180**（实测均值 154.4） | 03：137.1–175.9 | `consistency_plan.go` 加 `plan.density_out_of_range` |
| A3 | **禁止跨频道比 cue/分**（14.7–34.2 受字幕断行粒度污染） | 03 | 写进审计注释，避免后人误用 |
| A4 | 新增「收束超长」审计：全片 0.1–2.6% 是一句致谢/CTA | 03：4/4 末句致谢 | `consistency_plan.go` 加 `plan.tail_overlong`（>8% 告警） |
| A5 | 参考图**按当前镜所需筛选**，而不是历史出现过的都塞 | 04 反证：VMem-Bench precision 0.977→0.915–0.938、禁止实体规避 0.968→0.845–0.898 | `consistency_plan.go` 加 `plan.ref_stale_state` / `plan.ref_over_supply`；现状 `require_ref_images_every_segment=true` 正踩反证 |
| A6 | 建 `data/comfy_workflows/` 空目录（现为悬空引用） | 05 + **已核实**：`multimodal.go:127` 的 `ComfyTemplateDir` 指它但目录不存在 | `data/comfy_workflows/` |
| A7 | 验收加「同图连跑 3 次，`memory_reserved` 基线不得单调上升」 | 05 | `scripts/verify-media.py` 或新探针 |

### B 级 · 半天到两天、改一处逻辑

| # | 动作 | 依据 | 落点 |
|---|---|---|---|
| B1 | **端帧锚定**：上一镜末帧 + 首镜 N_A 帧回填下一镜参考图（`continue` 现在只是标签、无像素锚，这正是 `plan.color_shift` 穿门的原因） | 04：CineWeaver 消融 N_A=0→0.368 vs N_A=5→0.429；Transition Dev 2.89→2.02 | `shotlist.go` 的 ref_images 装配 + `shotplan.go:49` 的 `Shot.RefImages`（现仅布尔，需扩成可寻址集合）；受 `filmgates.go:61` `max_ref_images=9` 约束 |
| B2 | **续接升级成显式契约**：区分「同潜空间续接（复用原生 TAKE 潜态）」与「外来素材接边（只 VAE 编码尾部窗口当 frame-0）」 | 05：`ComfyUI-MiniMax-H3-LongMedia` v0.6.63（309★） | `shotlist*.go` + `comfy_media_batch.go` + `subtitles.go`（重叠值须与 `transitionOverlapD` 同源共用） |
| B3 | **补 lemo 三处未用能力**：自托管 CJK 字体（`kit.js` 现在用系统字体栈，违反 lemo 官方规则、缺字静默回退）/ `window.TEXTS` + `readcheck.mjs`（CJK 4.5 字/秒阅读时长门）/ code_video 旁白 ASR 验词 | 02 | `codetpl/shared/kit.js` + `code_video.go` |
| B4 | **加 CPU 道节点**（不抢显存）：PySceneDetect cut 审计 / whisperX 词级对齐 / DeepFilterNet 降噪 | 05 | 三处同上：`nodespec.go` + `nodecatalog.go` + `queue.go:59` 白名单；依赖走 `data/deps.json` 托管，**不编进 exe** |
| B5 | **补一个当前零覆盖的验收盲区**：`cut` 用 <1 帧淡化导致第二段画面整段丢失（时长与音频全对，现无工具能发现） | 05 D5 + AGENTS §0.5 | `scripts/verify-media.py` |

### C 级 · 需要实验（一轮只动一个变量 + 开关各跑一次）

| # | 动作 | 依据 | 风险 |
|---|---|---|---|
| C1 | **H3 多镜潜空间接力**：`context_pin` 上镜末 22 帧 raw latents 注入下镜内部关键帧（无 VAE 往返），取代逐镜生成 + xfade | 01：`joeygambino/MiniMax-H3-Multishot-Workflow` v2.7.0（apache-2.0） | 缺依赖 `ComfyUI-H3-Motion-Context` + RES4LYF + sol-attn/blockcache-T8 + ComfyUI-GGUF；**未本机实测** |
| C2 | **H3 剪枝权重**：DiT 33.14B→20.11B、transformer 66.28→40.24GB | 01：`multimodalart/MiniMax-H3-Pruned` | **必须先跑 `scripts/h3-bench.py` A/B**（FE 有「PDD LoRA 叠 W4A8 是负优化」前科，属同类交叉） |
| C3 | **第二条「一次出画面+声音+跨镜记忆」后端**：JoyAI-Echo 1.5 fp4（22.81GB、8 步 DMD、≤7 记忆槽含音色记忆） | 01 | 24GB 卡档位按社区 INSTRUCTIONS（fp8/Q8_0 + sequential_offload + resident_blocks≈20–24），**未实测** |
| C4 | **补口型缺口**（FE 唯一真缺口：口型由模型即兴而非 TTS 驱动）：EchoMimicV3 1.3B（6.65GB、apache-2.0）优先 | 01 | 数字人节点要独立 venv 复用 `D:/AI/envs/torch-cu124`，**别塞 ComfyUI venv**；LongCat-Avatar 官方是 2 卡 |
| C5 | **VBench-2.0 `Human_Identity`** 自动判定「第 2 镜是否漂角色」 | 05：唯一「接进来就自动补 AGENTS §0.16 已知缺口」的开源实现 | **抢显存（大），绝不能与 H3 并跑**，只能放 `quality_loop` 后处理段 |

### D 级 · 已明确否决（别再试）

| 项 | 否决理由 | 依据 |
|---|---|---|
| 换掉 lemo / 换渲染后端求吞吐 | FE 实测 1080p/432 帧 21.3s（≈20.3fps）vs HyperFrames 官方 300 帧 9.8s（≈30.6fps，**官方自述**）→ **同量级，无数量级收益**；收益在确定性与排版质检 | 02 |
| Motion Canvas / Revideo / editly | 渲染依赖编辑器 GUI 或已停更（2026-07 / 2025-05） | 02 |
| moviepy / ffmpeg-python / vidgear / pireel | 给 Go 单 exe 引 Python 依赖；零增量；AGPL | 02 |
| Remotion（作为默认后端） | 含真实素材的产品介绍片才用；自动出片产品 $0.01/次 + 5.0 起强制 telemetry | 02 |
| CineWeaver 的注意力改造照抄 | 它是**单次扩散轨迹内**产多镜；FE 每镜是独立采样节点，**没有共享轨迹可打断** | 04 |
| H3 六段式新开第七段 | `filmgates.go` 的 `countShots`/`pictureTags`/`dialogueBlocks` 按段解析，加段要连带改解析器 | 04 |
| 按 `tencent/HunyuanVideo-Avatar` 排期 | HF repo 只有 LLaVA 编码器 17.34GB，DiT 权重与 license 都不在 | 01 |

---

## 三、冲突与待拍板（3 条，需要你定）

1. **BGM**：`AGENTS.md` 定的是「知识/科普片不加 BGM」，但 03 实测 **3/4 条英文头部科普铺 BGM**，A 流派「基地」是 500 秒无静音能量床、LRA 压到 2.0。
   → 建议：**维持不加 BGM**，把可懂度代偿成两条数：`speech_snr ≥8dB` + 「每分钟语音段数 vs plan cue 数偏差 ≤15%」，补进 `verify-media.py`。
2. **Remotion 能不能进方案**：取决于 FE 是「本地自用」（免费）还是「对外/批量出片产品」（落 Automators 计费）。
3. **「旁白提示画面」不成立**：03 实测宽口径视觉提示 0–0.9 次/分 → `knowledge_film`/`code_video` 必须走 **beat + 固定时长（建议 6–12s/次）驱动**，门判「视觉事件」而非 scene cut。这改变了 `code_video` 的编排假设。

---

## 四、跨文档未验证汇总（硬边界，别当已验证用）

- **零实验**：01–05 全部**没有**下载权重、启动 ComfyUI、跑任何管线、做任何 A/B。「24GB 能不能跑」在 01 里一律是**按权重体积推算**。
- **零渲染**：02 全部结论来自源码与官方文档。
- **画面层未取到**：03 拿到的是**字幕文本**，镜头/字幕样式/BGM 电平未取到（没下视频）。
- **阈值样本极小**：03 的门阈值 n=4，落地前必须用 FE 自家片子回归，否则会误杀。
- **中文口径缺失**：03 的 135–180 词/分是**英文口径**，中文须按字/分重新标定。
- **gated 仓库**：`Lightricks/LTX-2.5`、IC-LoRA-Ingredients 的 README 是 401，关键字段未取到。
- **论文**：04 未跑过任何论文代码；多数论文不报显存，**不要跨参数反推**；9 条仅标题级证据，不得据以决策。
- **lemo 平台风险**：官方原文写「on Windows use WSL」，而 FE 在原生 Windows 跑（官方未支持路径）；3D/shader 风格要 GPU，FE 4 个模板全是 Canvas 2D，**该路径从未触发、未验证**。

---

## 五、下一步（我会按此推进，你可改优先级）

1. 07–10 四条泳道落地后并入本索引（模型选型 / 平台机制 / 时间线格式 / 分发合规）。
2. 小红书 + 抖音站内取数（Chrome 装好后登录一次），补「7 日内增量爆款」。
3. 把 A 级 7 条做成一个可提交的补丁（含测试），而不是只停在建议。
