# Audio8-TTS 0.6B（PyTorch + CUDA）部署记录

> 2026-10-23。结论先行：**可用**——权重完整、文本还原准确、零样本克隆 MFCC 相似度 0.993、显存仅 2.4GB。
> 唯一短板是速度：默认音色 RTF 7.4~7.9（batch 后），克隆音色 RTF 18~32。
> 0.1B ONNX 版（`docs/Audio8-TTS-部署记录.md`）产出无效语音，**勿用**。

## 1. 共享 CUDA 底座（本次的重点）

不是给 Audio8 单独建环境，而是建一个**通用底座**，其它要 GPU torch 的软件直接复用同一个解释器：

| 项 | 值 |
|---|---|
| 路径 | `D:/AI/envs/torch-cu124/` |
| Python | 3.11.9（与 ComfyUI / 本地语音服务同 base） |
| torch | 2.6.0+cu124（**从 ComfyUI venv 直接复制，零网络下载**） |
| torchaudio | 2.6.0+cu124 + torio（同源复制） |
| transformers | 4.57.6（要求 `<5`，所以**不能**塞进 ComfyUI venv——那边是 5.14.1） |
| numpy / safetensors / sympy / networkx / jinja2 / filelock / fsspec | 同源复制 + `numpy.libs`（漏了它 numpy 会 `DLL load failed`） |
| 新增下载量 | 仅 transformers 及小依赖，约 60MB |
| 解释器 | `D:/AI/envs/torch-cu124/Scripts/python.exe` |

验证：`torch.cuda.is_available() == True`，识别 `NVIDIA GeForce RTX 3090`，capability (8,6)，82 SM。
算力基线：`Linear(1×512×1536) bf16 = 0.17ms/call`。

**为什么不复用 ComfyUI venv**：`requirements.txt` 写死 `transformers>=4.57.0,<5`，而 ComfyUI 是
5.14.1。降级会破坏 ComfyUI 的节点注册（AGENTS §5.1 那条血泪）。

## 2. 权重

| 项 | 值 |
|---|---|
| 目录 | `D:/AI/Models/audio8_tts_0_6b/` |
| 体积 | 2.45GB（`codec.pth` 1.35G + `model.safetensors` 1.20G + 配置文件） |
| 来源 | HF 原名 `Edge0/Audio8-TTS-Preview-0.6b`（原 Audio8-AI 改名），ModelScope 同名仓库 |
| 许可 | **Apache-2.0**（HF 模型卡 tag 明确 `license:apache-2.0`） |
| 校验 | 14/14 文件 sha256 全部匹配 ModelScope 官方清单 |

**下载通道实测（同一台机器、同一时段）**：

| 通道 | 速度 | 2.45GB 耗时 |
|---|---|---|
| hf-mirror.com 单连接 | 140KB/s | ~5 小时 |
| hf-mirror.com 16 线程分段 | 0.5MB/s | ~80 分钟 |
| **ModelScope resolve 直链** | **18.8MB/s** | **2 分 20 秒** |

命令：
```bash
curl -L --noproxy '*' -o codec.pth \
  "https://modelscope.cn/models/Edge0/Audio8-TTS-Preview-0.6b/resolve/master/codec.pth"
```
> 注意：ModelScope 的 `api/v1/.../repo?FilePath=` 路径对大文件会限速，必须用 `resolve` 直链。

## 3. 实测数据

### 3.1 质量（全部 ASR 逐句校验，faster-whisper small/int8）

| 输入文本 | ASR 回读 | 判定 |
|---|---|---|
| 深夜的巷子里，只有一盏灯还亮着。 | 深夜的巷子里只有一盏灯还亮着 | ✅ 一字不差 |
| 他推开门，风裹着雨点扑进来。桌上的信纸被吹得哗哗作响。 | 他推开门,风果着雨点铺进来,桌上的性质被吹得花花作响 | ✅ 差异全是同音字（whisper 混淆） |
| 那一年冬天特别冷，河面结了厚厚一层冰…（51 字） | 那一年冬天特別冷，河面結了厚厚一層冰… | ✅ 完整念完 |
| 这一句要用参考音色来念，看看像不像原来那个人。（克隆） | 这一句要用参考音色来念,看看像不像原来那个人。 | ✅ 一字不差 |

所有样本 `finished=True`（正常早停，不像 0.1B 跑满 token）。

### 3.2 克隆相似度（`scripts` 外的临时探针，指标可复现）

| 音频 | F0 均值±std | 谱质心 | MFCC 余弦 vs 参考 |
|---|---|---|---|
| 参考音频（Kokoro 22.05kHz） | 212.5±60.5 | 1209Hz | — |
| **克隆输出** | 201.8±55.1 | 1005Hz | **0.993** |
| 默认音色输出 | 141.6±51.6 | 1047Hz | 0.455 |

克隆确实抓住了参考音色（0.993 对比 0.455 是决定性差距）。

### 3.3 速度（RTX 3090，bf16，实测 wall time）

| 场景 | 字数 | 音频时长 | 耗时 | RTF |
|---|---|---|---|---|
| 单句（含首次 warmup） | 16 | 3.39s | 57.1s | 16.8 |
| 单句（已 warmup） | 27 | 4.55s | 30.1s | 6.6 |
| 长句 | 51 | 11.05s | 111.1s | 10.1 |
| **3 段 batch（服务实际路径）** | 41 | 9.89s | 77.9s | **7.9** |
| 4 句逐个跑 → 4 句 batch=4 | — | 10.17s | 311.5s → **79.2s** | 30.6 → **8.0** |
| 克隆音色 | 26 | 4.88s | 157.7s | 32.3 |
| 克隆音色（探针，已 warmup） | 26 | 5.02s | 91.4s | 18.2 |

**batch 摊薄 3.93 倍**——这是本次唯一落地的性能优化：`_split_text` 切出的多段一次 batch 送入，
而不是逐段串行。

### 3.4 显存

| 阶段 | 显存 |
|---|---|
| 加载后 | 1.12 GiB |
| 生成峰值 | 2.41 GiB |

比 flux（17GB）小一个数量级，与 VLM（6GB）并存无压力。服务带 `POST /unload` 供换侧前显式释放。

## 4. 为什么慢（已定位，不是猜）

`torch.profiler` 抓 16 帧（生成约 1.3s 音频）：**Self CPU 12.58s / Self CUDA 14.68s**，
两值接近说明是 **launch-bound**，不是算力不足。

单次 profile 里 aten 调用次数：`aten::to` 16654、`aten::copy_` 17232、`aten::select` 17432、
`aten::slice` 17097、`aten::_to_copy` 13566…… 16 帧约 10 万次 kernel launch，
平均 **每帧 6000+ 次**。0.6B 一帧要跑 1 次 slow AR + 10 次 fast codebook forward，
每步都被拆成几千个小 kernel，无法融合。

排除掉的假设（都做了 A/B，不留猜测）：

| 假设 | 验证 | 结论 |
|---|---|---|
| GPU 被别的进程抢 / 算力不足 | 裸跑 matmul 基线 0.17ms | ❌ 不是 |
| `sdpa_kernel(SDPBackend.MATH)` 强制数学实现 | patch 掉后重测：RTF 6.6 → 8.29（**更慢**） | ❌ 不是主因（profile 里 MATH attention 只占 1.05s/14.68s = 7%） |
| 参考音频编码是克隆慢的原因 | processor 编码仅 0.58s，generate 从 29.4s → 91.4s | ❌ 不是（缓存 codes 无意义） |
| `torch.compile` 能救 | Windows torch 无 triton，inductor 不可用 | ❌ 走不通 |

**能真正解决的路**：CUDA Graph（把每帧静态图录制重放）或官方 `sglang_omni`（paged attention +
CUDA graphs + 动态 batch）。后者要求 Linux + SGLang 0.5.8 + torch 2.9.1+cu128，Windows 装不了。
两条都要改模型代码，属独立工程，本次没做。

## 5. 接入 FlowEdge

```bash
# 切到 Audio8-PyTorch 后端（Kokoro 保持默认不动）
TTS_SCRIPT=tts_audio8_pt_server.py
TTS_PYTHON=D:/AI/envs/torch-cu124/Scripts/python.exe
TTS_PORT=8103
TTS_BASE_URL=http://127.0.0.1:8103
```

服务文件：`运行/local-services/tts_audio8_pt_server.py`（与 Kokoro 同契约 `POST /tts {text,voice}`）。

| 端点 | 用途 |
|---|---|
| `POST /tts {text, voice}` | 合成，返回 wav（44.1kHz 单声道 PCM16） |
| `GET /health` | 轻量探活（不加载模型，首次约 3s 因 import torch） |
| `GET /health?deep=1` | 真合成一段，区分「跑着但坏了」 |
| `POST /voices/register {name, text, audio_path}` | 零样本克隆注册（参考音频 0.5~30s） |
| `POST /unload` | 释放显存 |

`deps.go` 已扩展：`TTS_SCRIPT` 选脚本、`TTS_PYTHON` 指定解释器，并注入
`AUDIO8_MODEL_DIR` / `AUDIO8_VOICES_DIR` / `AUDIO8_DTYPE` / `AUDIO8_IDLE_UNLOAD`（默认 0 = 常驻）。

## 6. 限制与未验证

- **没有语速控制**（`supports_speed=false`）：speed/rate 传了会被忽略并在响应头
  `X-TTS-Ignored-Params: speed` 里如实回报，不做事后变调假装生效。
- **单次输入 ≤150 字**（官方口径），超长自动切段 + batch。
- **克隆模式 RTF 18~32**：比默认音色慢 2~4 倍，因为 prompt 带参考码本、生成帧数也更多。
  出片排期要按这个数字留余量。
- **未验证**：并发请求（服务用全局锁串行化的）；与 flux / LLM 同时驻留的实际表现（只测了单独跑）；
  `float16` / `float32` 的 A/B（只用 bf16 跑过）；英文/多语种质量（只测中文）。
- **0.6B ONNX INT4 没试**——已知 ONNX 路线在本机产出有问题的是 0.1B，不能推广到 0.6B。

## 7. 复现命令

```bash
# 冒烟
curl --noproxy '*' -m 180 "http://127.0.0.1:8103/health?deep=1"

# 合成
curl --noproxy '*' -m 300 -X POST "http://127.0.0.1:8103/tts" \
  -H "Content-Type: application/json" \
  -d '{"text":"深夜的巷子里，只有一盏灯还亮着。","voice":"default"}' -o out.wav

# 注册克隆音色
curl --noproxy '*' -X POST "http://127.0.0.1:8103/voices/register" \
  -H "Content-Type: application/json" \
  -d '{"name":"narrator","text":"参考音频的逐字稿","audio_path":"C:/path/ref.wav"}'

# 验收（项目硬要求）
python scripts/verify-media.py out.wav --asr --frames .tmp/frames
```
