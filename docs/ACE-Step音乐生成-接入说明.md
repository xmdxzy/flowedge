# ACE-Step 1.5 XL 音乐生成接入说明（2026-11-03）

> 目标：本地生成音乐（BGM / 纯音乐 / 歌），由 FlowEdge 托管（启停、探活、显存仲裁），
> 并能作为出片流水线的「自动配乐」环节。许可证 MIT ⇒ 产出可商用（模型方提示
> 风格撞车风险自负）。

## 1. 部署形态

| 项 | 值 |
|---|---|
| 代码 | `D:/AI/ACE-Step-1.5`（git clone + **手工装的 venv**，见 §8）|
| 权重 | `D:/AI/Models/ace-step/`（`ACESTEP_CHECKPOINTS_DIR`；**另有 junction** `D:/AI/ACE-Step-1.5/checkpoints` → 指过去）|
| 服务 | `acestep/api_server.py`，端口 **8104**（`ACESTEP_API_PORT`）|
| Python | `D:/AI/ACE-Step-1.5/.venv/Scripts/python.exe`（torch **2.7.1+cu128**）|
| 协议 | `POST /release_task` → `POST /query_result` → `GET /v1/audio?path=`，探活 `GET /health` |

**模型选择**（`运行/data/deps.json` 的 `acestep.model`）：

- `acestep-v15-xl-turbo`（默认）：4B DiT，**8 步**，日常档；
- `acestep-v15-xl-sft`：质量更高，但 50 步（慢一个数量级），适合「主打曲」；
- LM 规划器 `acestep-5Hz-lm-1.7B`，`lm_backend=pt`（没有装 vllm/nano-vllm）。

**LM 默认不预载**（`init_llm=false`）：纯音乐走 BGM 用不到它，见 §8 的 CoT 坑。

## 2. 为什么单独开一个显存侧（sideMusic）

ACE-Step XL 权重约 9GB，运行时叠加 VAE / LM。它与 H3（20.5GB）**物理上装不下**
同一张 24GB 卡，与 GPU 上的 TTS（15–20GB）也装不下。

- `service/vram.go` 新增 `sideMusic`，进音乐侧会依次卸掉 **扩散 → 语言 → 语音**；
- 进扩散/语言/语音侧时反过来调 `releaseMusicSide`；
- **ACE-Step 没有 `/unload` 端点**，所以释放只能**停进程**（`deps.Stop("acestep")`）。
  别给它造假的 Unload —— Breeze TTS 的「假卸载」已经踩过一次（返回 ok 但显存没还）。
- 代价：下次生成要重新冷加载 1–3 分钟（实测：首冷 143s、再冷 77s、热 4.7s）。
  `autostart=false`，用到才起。

## 3. FlowEdge 托管

`运行/data/deps.json`：

```json
"acestep": {
  "home": "D:/AI/ACE-Step-1.5",
  "script": "acestep/api_server.py",
  "python": "D:/AI/ACE-Step-1.5/.venv/Scripts/python.exe",
  "port": "8104",
  "base_url": "http://127.0.0.1:8104",
  "autostart": false,
  "model": "acestep-v15-xl-turbo",
  "lm_model": "acestep-5Hz-lm-1.7B",
  "lm_backend": "vllm",
  "checkpoints_dir": "D:/AI/Models/ace-step"
}
```

- 面板：`GET /api/deps` 会多一项 `acestep`；`POST /api/deps/acestep/start|stop` 可启停；
- 就绪等待：`readyTimeout("acestep") = 600s`（可研，冷加载比 TTS 更贵）；
- 预检：`capabilities.go` 增加 `Music` 能力（`/health`），`preflight.go` 对
  `music` 节点只在「连拉都拉不起来」时才拦——单纯没在跑不拦（节点会按需拉起）。

## 4. 引擎节点 `music`

节点类型 `music`（`nodespec.go` + `nodecatalog.go` 双写，`nodecatalog_test` 会红）：

| 输入 | 字段 | 说明 |
|---|---|---|
| `text`（可选）| `prompt` | 风格/氛围描述；留空取上游文本 |
| | `lyrics` | 留空 = 纯音乐 |
| | `duration` | 秒（10–600） |
| | `model` / `steps` | 默认 `xl-turbo` / 8 |
| | `language` / `thinking` / `seed` | 歌词语言 / LM 增强 / 种子 |

输出：`{"audio": 绝对路径, "duration": 实测秒, "model", "prompt", "seed"}`。

超时 `musicTimeout = 8 分钟 + 每秒音频 1 秒`（冷加载 3–5 分钟的余量）。

## 5. produce 层「自动配乐」

film / film_from_plan / video / video_from_image 新增参数（**produce 层消费，不 bind 节点**）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `bgm_auto` | false | 勾了就生成并混入成片 |
| `bgm_prompt` | "" | 留空用默认氛围垫（无人声无鼓点） |
| `bgm_gain` | -18 | BGM 相对节目声电平 dB（0 合法，>0 压回 0） |
| `bgm_duck` | true | 旁链压缩：人声/强环境声时自动让位 |

实现：`service/music_bgm.go` 的 `ApplyAutoBGM`，在 **贴字之后** 执行
（BGM 要按最终成片时长铺），失败即 `run.status=failed`——不给「静默没配乐」的假成功。

**与已有的 `bgm` 文件参数的关系**：`film` 系列本来就有 `bgm`（用户自己的音频文件，
绑 `sl-1`）。**给了文件就以文件为准**，`bgm_auto` 直接跳过——参数化绝不覆盖显式输入，
也不会混两层 BGM。

## 6. 验收与排障

```bash
curl.exe --noproxy '*' 127.0.0.1:8104/health
python .tmp/ace_smoke.py "warm lo-fi piano" 12      # 提交→轮询→落盘→ffprobe
```

- 服务日志：`<部署目录>/logs/acestep.log`（托管进程的 stdout/stderr）；
- 换侧日志：`engine.log` 里 `[vram] 释放音乐侧显存（进扩散侧）`；
- 生成日志：`[music] 已落盘 ...`、`[bgm] 生成完成/已混入成片`。

常见问题：

| 现象 | 原因 |
|---|---|
| `music 节点不可用：引擎没有配置音乐服务` | 引擎未注入 MusicService（旧 exe）或 deps.json 缺 `acestep` 段 |
| 启动后 600s 未就绪 | 权重没下全（`checkpoints_dir` 少目录）或模型名与目录名不一致 |
| 生成完但产物 0 字节 | 服务磁盘满 / 下载环节被代理劫持（本机 `http_proxy` 需 bypass） |
| 出片时 OOM | 音乐侧与扩散侧同时驻留（检查是否用了旧 exe，没有 sideMusic） |

## 7. 已知限制

- 单次最长 600s（10 分钟）；更长的 BGM 由 `mixBGM` 的 `-stream_loop -1` 循环补齐；
- 无 `/unload`：换侧只能停进程，冷加载要 1–3 分钟（见 §8 实测）；
- 未做「歌词与画面对齐」（歌词是自由生成，不跟分镜台词）；
- 知识类/科普类片**默认不加** BGM（项目已拍板的规则），要加必须显式勾 `bgm_auto`。

## 8. 实测数据与踩过的坑（2026-11-03，3090 24GB）

**耗时（都是本机墙钟）**

| 场景 | 耗时 |
|---|---|
| 服务绑定端口 → `/health` 可探 | ~31s（导入 torch + uvicorn 启动）|
| 首次生成（XL 冷加载 + 12s 曲，LM 关） | **143.4s** |
| 再次冷启动（权重已在页缓存 + 20s 曲） | **77.4s** |
| 热态 12s 曲 | **4.7s** |
| 开着 LM + CoT 的 12s 曲 | **>15 分钟没出结果**（卡在 `Phase 1: Generating CoT metadata`）|

**产物实测**：`pcm_f32le / 48000Hz / 2ch`，20.000s，mean −17.1dB、max −1.0dB（不是静音、没削波）。

### 8.1 XL-turbo 8 步 · 冷/热/长曲三点台架（2026-11-03 复测，`scripts/bench-acestep.py`）

| 轮次 | 请求 | 墙钟 | RTF | 显存峰值 |
|---|---|---|---|---|
| 冷（含 9GB 权重加载） | 13s | **111.3s** | 8.565 | **22420 MiB**（基线 9650，**+12770**）|
| 热 | 13s | **6.1s** | 0.471 | 21588 MiB（+84）|
| 热 | 30s | **7.2s** | 0.240 | 22014 MiB（+428）|
| 生产（成片配乐） | 14s | **6.4s** | 0.457 | — |

**两条比原表更重要的事实**：

1. **显存不是 9GB，实测 ~12.7GB**（峰值 22420 / 24576，剩余仅 2.1GB）。
   所以「不能与 H3 共存」不只是 20.5+9 的估算，是真的会贴边：
   H3 20.5GB + ACE 12.7GB = 33GB > 24GB。**换侧必须停进程**（上游无 `/unload`）。
2. **RTF 随长度下降 → 固定开销占主导**：拟合 `t ≈ 5.3s + 0.06s × 秒数`
   （13s→6.1s、30s→7.2s、14s→6.4s，三点的预测都对得上）。
   **推论：别抠曲子长度**——做一个 30s 的只比 13s 多 1 秒，宁可生成够长再裁。

**冷加载 ≈ 105s**（111.3 − 6.1）。混合出片时这条要算进预算：**每换一次音乐侧 = 停进程 + 下次 105s 冷启动**，
所以同一支片子要改配乐，最好一次定稿再跑。

### 8.2 替换成片音轨的两个坑（2026-11-03）

1. **`-c:v copy` 才能保证画面零损失**，并且必须用**视频流时长**（本例 12.458333s）
   定 `atrim`/`-t`/`afade st`，不能用容器时长（AGENTS §0.12⑤ 同源教训）。
   核验方式：混音前后 `ffprobe` 比 `codec_name + nb_frames + duration + bit_rate` 四项全等。
2. **`loudnorm` 会把采样率抬到 96kHz**（内部 192k 工作采样），链尾必须
   `aresample=48000` 钉回去，否则成片音轨与既有工程不一致。
3. PowerShell 拼滤镜串时 `$var:d=` 会被当成 `$var:drive` 语法解析（`afade st=$fadeSt:d=0.55`
   变成 `=0.55` → ffmpeg `exit -22`）。**一律写 `${var}`** 划清边界。
4. **`volumedetect` 的统计打在 info 级日志上**：跑成 `-v error` 会把它们全吞掉，
   `mean/max` 变 `None`——看起来像「音频静音」，其实是测量失败。
   这类「探测失败被当成结论」必须分开报（同 AGENTS §0.6）。
   要 LUFS 真值用 `ebur128`（`volumedetect` 的 mean 是 RMS，不是 LUFS，两者不可混比）。

**四个坑（每个都真实卡过一次）**

1. **`uv sync` 会卡死**：torch 3GB 走 uv（无论走代理还是直连）都会停在
   `Downloading torch (3.0GiB)` 不再动。改法：`curl -C -` 直接下
   `torch/torchvision/torchaudio` 的 cu128 轮子，`uv pip install --no-deps --reinstall`
   覆盖；之后**必须删掉残留的 `torch-2.15.0.dist-info`**——两个同名 dist-info 并存时
   `importlib.metadata.version("torch")` 返回 `None`，transformers 直接
   `InvalidVersion: Invalid version: None`。
2. **不要用 `uv sync` 装项目本体**：它会拉 `nano-vllm`（本地包）+ flash-attn + triton。
   LM 用 `pt` 后端时这三样都不需要（`llm_inference.py` 对缺 flash_attn/triton 有降级分支），
   改用 `uv pip install --no-deps -e D:/AI/ACE-Step-1.5` 只让 `acestep` 可 import。
3. **权重会被重复下载到仓库目录**：api_server 的 `_get_project_root()` 写死
   `dirname(dirname(__file__))`，`ACESTEP_PROJECT_ROOT` 对它无效 ⇒ 它认为
   `<repo>/checkpoints` 才是模型目录，缺文件就整仓库重下（实测 29 个文件 / 9 分钟）。
   改法：把 `<repo>/checkpoints` 做成 **junction** 指向 `D:/AI/Models/ace-step`。
4. **`.cache` 必须挪出 D:/AI**：引擎派生进程写建 sqlite 缓存会报
   `attempt to write a readonly database` → `Application startup failed`
   （同一个命令在 agent 的 job 里跑却正常，和 H3 写 `D:/AI/ComfyUI/output` 报
   PermissionError 是同一类：派生进程对 D:/AI 无写权限）。
   改法：`<repo>/.cache/acestep` 做成 junction 指向
   `运行/data/acestep/.cache/acestep`。

**为什么纯音乐要关 LM CoT**：请求里 `use_cot_caption=false, use_cot_language=false`
（`music.go` 在「无歌词 且 未显式 thinking」时自动加）。不关的话 LM 会做
「CoT 元数据生成」，实测 12 秒的曲子 **15 分钟都出不来**（progress 钉在 0.1），
而 BGM 根本不需要它——CoT 只用来改写 caption / 猜歌词语言。

**当前状态**：`运行/FlowEdge.exe` 已更新（2026-11-03 19:2x），`/api/deps` 里
`acestep: running=true, managed=true, state=ok`（`POST /api/deps/acestep/start` 由引擎派生、
`/api/deps/acestep/stop` 与换侧时的 `releaseMusicSide` 会停它）；
`/api/node-catalog` 有 `music` 节点（side=music，共 15 个节点）。

## 9. 端到端实测（run 433，2026-11-03）

**输入**：`film_from_plan`，1 镜 124 帧 / 608×352 / 12 步，`bgm_auto=true`，
`bgm_prompt="warm lo-fi ambient piano, soft pads, no vocals, cinematic underscore"`，
`bgm_gain=-18`、`bgm_duck=true`。

**时间线**（墙钟）：

| 时刻 | 事件 |
|---|---|
| 19:11:00 | 提交（H3 冷启 → 出镜 → 装配 → 声床/响度归一）|
| 19:15:02 | **图形链路结束**（`end-1` 校验 124 帧、有音轨）|
| 19:15:31 | `[vram] 释放扩散侧模型（进音乐侧）` + `释放语音侧显存（进音乐侧）：TTS 模型已卸载` |
| 19:19:13 | BGM 落盘 `bgm-*.wav`（10.16s = 片长 5.17 + 5s 余量）|
| 19:19:15 | 混音完成 `film-*-shots-master-roomtone-bgm.mp4` |

**验收**（`scripts/verify-media.py`）：✓ 全部通过 —— 124 帧 / 5.167s / 608×352 / 24fps /
h264 + aac 48kHz 立体声 / 响度 mean −21.6、max −6.4 / 无 silence_gap。

**BGM 确实混进去了**（不是复制）：与「混音前」同源文件做差值信号的 RMS =
**−18.2dB**（同一段音轨复制的话差值应≈−∞）；同时 100–500Hz 频段 RMS 由 −32.8dB
升到 −31.98dB，符合「低电平 BGM 铺底」。

**顺带修掉的一个错报**：`run.EndedAt` 原来在贴字/配乐**之前**取，run 433 因此
DB 里记 4m02s，实际 8m15s。现在结束时间算在两步之后（`produce.go`），
ETA 与预算对账不再少报。

**产物**：`运行/data/generated/film-1793704498352403600-shots-master-roomtone-bgm.mp4`，
示例纯音乐 `运行/data/generated/music-sample-ambient-20s.wav`。

## 10. 让 H3 出无声视频、配乐全交 ACE-Step（2026-11-03 实测验证）

**为什么要拆开做**：H3 的画面与音轨是一次推理出来的，音效与画面**天然同步**
（风扇起转、部件到位咔嗒、玻璃轻鸣）。但它自带的音乐是自由发挥的，
不能指定曲风/速度/卡点。拆开 = H3 只出画面，ACE-Step 专做音乐。

**两步必须都做对，少一步就白做**：

1. **prompt 侧**：`overall_soundscape: N/A` + `non_diegetic_music: N/A`
   （官方 `VIDEO_PROMPT_WRITING_GUIDE_base_en.md` §4.6：
   `Use N/A only when the user explicitly requests complete silence throughout the video.`）
2. **produce 侧**：`room_tone=false` ← **这一步是坑**

### 10.1 陷阱：引擎会把你辛苦留的静音填满

引擎在响度归一之后默认铺一层 −36dBFS 声床（`mixRoomTone`，见 AGENTS §0.16）。
H3 的双 N/A 输出确实是数字静音，但那层床会在上面加 **+52dB**。

同一 plan、只动 `room_tone` 一个变量的实测对照：

| 参数 | 产物 | mean | max | LUFS |
|---|---|---|---|---|
| `room_tone=true`（默认）| `film-*-shots-master-**roomtone**.mp4` | −39.0dB | −26.1dB | −37.7 |
| `room_tone=false` | `film-*-shots-master.mp4` | **−91.0dB** | **−84.3dB** | **−70.0** |
| H3 裸分镜（无任何后处理）| `video-*-h3_*.mp4` | −91.0dB | −84.3dB | — |

**只看成品会把这件事误判成「N/A 压不住」**——我第一次测的就是成品，差点得出错结论。
判据：`max < −80dB` 属**数字静音**（AAC 量化残差；16bit 理论动态范围 96dB，
任何回放链路底噪都高于它），不要当"有底噪"去修。

验收工具：`python scripts/check-h3-silence.py <run_id>`（等完 run、取 end-1 产物、
三重测量并按上面的阈值判定）。

### 10.2 代价：会丢掉 H3 的音效

风扇起转、每层金属咔嗒、蓝宝石玻璃轻鸣都与画面对齐，删了就没有——
**ACE-Step 不支持"在第 3.2 秒放一声咔嗒"**。所以按需求二选一：

| 想要 | 做法 |
|---|---|
| 纯音乐片 | 双 N/A + `room_tone=false` + 后期整条替换音轨 |
| 音效 + 音乐 | **别关** H3 音频，后期把音乐叠在低电平上（`amix`，保留原音效层）|

### 10.3 配乐侧的质量旋钮（源码 `release_task_models.py`，比文档原表更全）

| 字段 | 作用 |
|---|---|
| `bpm` | 指定节拍速度（控制快慢，**不等于卡点**）|
| `reference_audio_path` + `audio_cover_strength` | 参考音频锚风格/结构（**语义未验证**，见 10.4）|
| `guidance_scale`（默认 7.0）| 提示词贴合度 |
| `inference_steps`（8 日常 / 20–50 高质量）| 画质档 |
| `seed` + `use_random_seed=false` | 可复现 |
| `repainting_start` / `repainting_end` | 只重绘某一段（改局部不重出全曲）|
| `batch_size` | 一次多出几条挑一条 |
| `task_type` | 默认 `text2music`，另有 cover/repaint 系 |
| `/v1/lora/*`、`/v1/training/start` | 可训练专属风格 LoRA |

### 10.4 未验证

- `bpm` 与 `guidance_scale` 的实际影响没做 A/B（只按默认 7.0 跑过）。
- 冷加载 105s 是单次测量，未取方差。

## 11. reference 语义 / 路径规则 / 有效长度（2026-11-03 三组实测）

这三条都是「不实测就会做错」的，工具一律不报错。

### 11.1 `reference_audio_path` 是**节奏锚**，不是风格锚

单变量对照（同 prompt、同 seed、同 steps，只多一个 reference）：

| 臂 | reference | 输出 BPM | 距 160 目标 | 4.4kHz 占比 |
|---|---|---|---|---|
| A | 无 | 197.4 | 37.4 | 0.00% |
| B | 160BPM click + 4.4kHz 正弦 | **156.2** | **3.8** | 0.001% |

喂进去的是「160BPM 规律 click + 4.4kHz 持续正弦」，出来**节拍跟上了（差 3.8）、音色完全没被带走**。
⇒ **可以按画面事件排一条 click 轨来驱动配乐节奏，而不污染音色。** 这是「让配乐踩画面节奏」的唯一可行路径
（`bpm` 只能定快慢，定不了"第 3.2 秒有重音"）。

### 11.2 路径只认**相对路径**（绝对路径几乎全拒）

源码 `release_task_audio_paths.py`：
- 绝对路径 → 必须落在**服务进程自己的** `tempfile.gettempdir()` 内，否则 `400 absolute audio file paths are not allowed`；
- 相对路径 → 不含 `..` 即放行，**相对服务 cwd**（= ACE 仓库根）。

**坑**：引擎派生进程的 TEMP 与交互 shell 的不是同一个，所以把文件放进「我这边」的
`%TEMP%` 再传绝对路径仍然 400——看起来像「不支持这个参数」，实际只是路径形式不对。
**做法**：参考音频放 `D:\AI\ACE-Step-1.5\` 下，传裸文件名 `_ref.wav`（实测通过）。

### 11.3 有效音乐长度只有 **~10–12 秒**（`audio_duration` 只管文件长度）

这是一个容易让人白忙的特性：`audio_duration=16` 确实给 16.00s 的文件，但**内容在 9.5–12s 就自然收尾**，
之后是 −50~−61dB 的残响尾巴。三条候选的包络实测：

| 候选 | 进入静音时刻 | 尾部静音 |
|---|---|---|
| A 纯 prompt | 10.5s | 2.10s |
| B 引导 0.6 | 12.0s | 1.00s |
| C 引导 0.35 | 11.5s | 2.10s |

生成 12.46s 也一样（尾部 1.8–2.4s）⇒ **不是 duration 参数的问题，是模型固有的乐句长度**。

> **2026-11-06 更正（实测推翻上表的适用范围）**：10–12 s 收尾只发生在**短请求**上。
> `audio_duration=45` 实得 **43.2 s** 连续音乐，`audio_duration=180` 实得 **178.6 s**。
> 但长请求**不等于长曲结构**：180 s 那条 60 s 之后稳定衰减到 −40~−45 dB，
> 每 8 s 与开头 8 s 的 chroma 相似度 0.90–0.96，等于同一段 loop 越放越轻，
> 没有副歌/桥段。⇒ 整曲仍须**分段生成 + 拼接**，只是不必再假设“一次只能出 10 秒”。
> 详见 `docs/ACE-Step-最后的水族馆-学习与创作方案.md` §4.1/§4.2。
两个后果：
1. **别用「生成更长再裁」来凑时长**——窗口搜索会全部落在 0.00s，裁完还是带尾巴；
2. 12s 级成片的正确做法是**长淡出**：本片 12.458s，画面 9.375s 起归位，
   音乐设 `afade=t=out:st=9.9:d=2.558`（2.56s 渐弱），让「音乐先收」对应「结构合拢」。
   这样 `verify-media` 报**无 silence_gap**，而 0.55s 短淡出会留下可听空档。

### 11.4 质量档的实际收益（steps 8→24，16s 曲）

> 修掉一个误判：我最初把「音乐能量与画面运动曲线」整条求相关，得到负值就以为「不配」。
> **本片 57% 画面是静止的，配乐本就不该跟着静止**，整条求相关必然为负。
> 正确问法是分段对比（运动段 vs 静止段的平均能量差）。

最终用于成片的候选（`scripts/make-film-music.py`）：
- 画面运动曲线（帧差）→ 按运动量生成 click 密度 → 作 reference（strength 0.6）
- `steps=24`（日常 8），生成 5.1–5.5s
- 客观指标：`mean −16.8dB / max −1.0dB`（无削波）、`低 1.42% 中 65.55% 高 0.00%`、尾部静音 1.00s
- 混音后 `I: −16.0 LUFS`（精确命中）、`max −4.0dB`（无削波）、ASR 空、无 silence_gap

**仍未验证**：好不好听。上面全是技术指标，主观听感只能由人判断。

### 11.5 长时长在 `xl-sft` 上成立，11.3 只适用于 turbo 短曲（2026-11-06 复测）

11.3 的「有效长度只有 10–12 秒」是 `xl-turbo` + 8 步 + 时长 ≤16s 的结论，**不能外推**。
同机复测（3090 24GB，`scripts/ace-gen.py`，prompt 含 `no vocals`、关 CoT）：

| 请求时长 | 模型 / 步数 | 文件时长 | 有效内容到 | 尾部 |
|---|---|---|---|---|
| 60s | `acestep-v15-xl-sft` / 20 | 60.00s | **≈52s** | 8s 淡出 |
| 240s | `acestep-v15-xl-sft` / 20 | 240.00s | **≈236s** | 4s 淡出 |

240s 曲的客观体检：121 BPM 六个窗口零漂移、chroma 相似度 0.97+、无中段静音、
−14.0 LUFS / 真峰 −1.0dB。⇒ **4 分钟 BGM 可以一次生成，不需要拼贴循环**；
拼接反而会在接缝处换调/换音色。turbo 短曲仍按 11.3 处理（长淡出）。
单次 240s 热生成约 18s（模型已在显存时），冷启动另算。


