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
| `POST /tts {text, ref_audio, ref_text}` | 直接用参考音频克隆（不注册，逐节点指定） |
| `GET /health` | 轻量探活（不加载模型，首次约 3s 因 import torch） |
| `GET /health?deep=1` | 真合成一段，区分「跑着但坏了」 |
| `POST /voices/register {name, text, audio_path}` | 零样本克隆注册（参考音频 0.5~30s，可复用） |
| `POST /unload` | 释放显存 |

`deps.go` 已扩展：`TTS_SCRIPT` 选脚本、`TTS_PYTHON` 指定解释器，并注入
`AUDIO8_MODEL_DIR` / `AUDIO8_VOICES_DIR` / `AUDIO8_DTYPE` / `AUDIO8_IDLE_UNLOAD`（默认 0 = 常驻）。

一键启动：`运行/5-启动 FlowEdge(接Audio8-TTS克隆).bat`（会先检查 8080 是否已被占用）。

### 5.1 节点级克隆（2026-10-23 下午补齐）

tts 节点新增两个参数，走完整链路 `node.Data → TTSOptions → POST /tts`：

| 参数 | 含义 |
|---|---|
| `ref_audio` | 参考音频绝对路径。**非空即启用克隆，并让 `voice` 失效**（克隆看的是参考码本，音色名对它没意义） |
| `ref_text` | 参考音频的逐字稿。参考音频非空时**必填**：空着直接报错，不降级 |

两种克隆方式并存：节点直填参考音频（一次性、逐节点）或先注册音色再填 `voice`（可复用）。

### 5.2 两个必须知道的坑（都已修，有测试钉住）

**坑一：TTS 客户端超时写死 60 秒。**
Audio8 克隆实测 RTF 18~32，58 字台词要 157s ⇒ 每次克隆都会超时 → 静默回退 SAPI →
**用户拿到的是 Windows 系统音色，节点却报成功**。现在超时按工作量算：
`60s + 每字 1.5s`，封顶 20 分钟（`synthesizeHTTP` 用 `context.WithTimeout` 精确控制，
`http.Client.Timeout` 只做 20 分钟兜底）。

**坑二：克隆失败会静默回退 SAPI。**
回退本身是既有契约（服务没起时用系统语音兜底），但对克隆是错误行为——等于换了个人在说话。
现在 `SynthesizeWith` 里判断：`RefAudio != ""` 时**不回退**，直接报错。

两条都有回归测试：`backend/service/tts_clone_test.go`
（`TestTTSRefAudioPassthrough` / `TestTTSCloneNeverFallsBackToSAPI` / `TestTTSNoRefStillAllowsFallback`）。

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

## 8. 引擎侧切换（2026-10-23 20:50 完成，已端到端验证）

### 8.1 为什么改配置文件而不是环境变量

引擎的 `TTS_BASE_URL` 原本只能来自环境变量（`getEnv`，无 `.env` 支持）。而引擎是由序策外壳
（`node server/flowedge-station.js`，:30000）spawn 的，外壳那段代码是：

```js
spawn(exe, [], { cwd: path.dirname(exe), detached: true, stdio: "ignore", windowsHide: true })
```

**没有 `env` 参数** ⇒ 子进程继承外壳自己的环境。也就是说：`setx` 之后必须连外壳一起重启，
而纯 `os.Getenv` 又让「改配置不重启」不可能。这条路太脆。

所以补了配置层：`data/deps.json` 增加 `tts` 段，优先级 **环境变量 > deps.json > 按脚本名推导**。
这样切后端只改一个 JSON，重启引擎即可（外壳不用动）。

### 8.2 配置格式

`运行/data/deps.json`：

```json
{
  "tts": {
    "script": "tts_audio8_pt_server.py",
    "python": "D:/AI/envs/torch-cu124/Scripts/python.exe",
    "port": "8103",
    "base_url": "http://127.0.0.1:8103",
    "autostart": false
  }
}
```

| 字段 | 作用 |
|---|---|
| `script` | tts 依赖启动哪个脚本（配合 `TTS_SCRIPT`） |
| `python` | 用哪个解释器（Audio8 必须指向共享 CUDA 底座） |
| `port` | 推导出的默认端口（旧逻辑是按脚本名猜 8102/8103） |
| `base_url` | **引擎连哪个地址**（新增，之前只认环境变量） |
| `autostart` | 引擎启动时是否自动拉起 TTS（**当前 false = 不常驻**，见 §8.6） |

改的代码：`deps.go` 的 `depsFile` 加 TTS 段 + 导出 `TTSBaseURL()`；`main.go` 的 `ttsURL` 解析加配置回退。

### 8.3 切回 Kokoro

```json
"tts": { "script": "tts_kokoro_server.py", "port": "8102",
         "base_url": "http://127.0.0.1:8102", "autostart": false }
```
（删掉 `python` 字段，让它回落到 ASR 那个 venv。）然后重启引擎。

### 8.4 切换过程与验证

1. 构建：`python scripts/build-windows.py --skip-ui`（前端已单独 `npm run build` 过）
2. 备份旧 exe：`运行/FlowEdge.exe.bak-20261023-2045`
3. 停引擎（只杀 8080 上确认名为 `FlowEdge` 的进程，外壳不动）
4. `cp dist/FlowEdge.exe 运行/`
5. `python scripts/start-engine.py` → 外壳 spawn，**跨会话存活**（pid 39816，父进程 node.exe）

`GET /api/deps` 结果：

```json
{"name":"tts","label":"TTS（audio8_pt）","running":true,"managed":true,"autostart":true,
 "endpoint":"http://127.0.0.1:8103",
 "command":"D:/AI/envs/torch-cu124/Scripts/python.exe ...\\tts_audio8_pt_server.py"}
```

引擎自动拉起了 TTS，8 秒内 `/health` 就绪。

### 8.5 端到端验收（工作流 #122，可重复跑）

`POST /api/workflows/122/run`，节点 `tts` 只填 `text` + `ref_audio` + `ref_text`（无 voice）：

| 项 | 值 |
|---|---|
| run 状态 | **success**，`backend: "service"`（不是 `sapi-fallback`，证明真走了 Audio8） |
| 耗时 | 84.5s 生成 5.29s 音频（**RTF 16.0**，克隆模式） |
| 产出 | `运行/data/generated/tts-1792759849771422900.wav`，pcm_s16le 44100 mono |
| ASR 回读 | 「这是Flow Edge引擎托管的Audio 8音色克龙端道端跑通了」 |
| 判定 | ✅ 差异只有同音字（克龙/克隆、端道端/端到端） |

工作流 #122 名 `tts-clone-smoke-20261023`，留着当这条链路的冒烟样板。

## 9. 已知遗留

- 测试工作流 #122 留在库里当样板，不需要可 `DELETE /api/workflows/122`。
- `运行/` 下累积了 15 个 `FlowEdge.exe.bak-*`（历史备份），最新那个是本次切换前的版本。

### 8.6 取消常驻（2026-10-23 20:58，用户要求）

改成 `autostart: false` 并停掉当前实例。**这是三件事，缺一不成**：

1. **改配置**：`deps.json` → `"autostart": false`（只影响引擎下次启动时的行为）
2. **停当前进程**：`POST /api/deps/tts/stop`（配置改了不会自动停已在跑的）
3. **重启引擎验证**：因为 `autostart` 只在 `NewSupervisor` 时读一次，不重启的话
   `/api/deps` 还会显示一直缓存着的 `true`——**改了没验证等于没改**。
   实测重启后：`running=false managed=false autostart=false pid=None`，8103 无响应，
   显存不再被占。

实测数据：

| 时点 | 显存 |
|---|---|
| 停之前（模型已加载） | 14341 MiB |
| 停之后 | 12197 MiB（**释放 2.1GB**） |
| 引擎重启后（确认没自动起） | 12858 MiB |

按需使用的两种方式：

| 方式 | 行为 |
|---|---|
| 面板点启动 / `POST /api/deps/tts/start` | 202 → **6 秒**就绪（懒加载：进程起来了但**不占显存**，首次 `/tts` 才加载模型约 30s、占 1.8GB） |
| 直接跑 tts 节点 | 不会自动起服务：没填 `ref_audio` 会回退 SAPI（音色变系统音），填了则直接报错 |

> 懒加载是默认行为（`AUDIO8_PRELOAD` 不设即 0）。所以「进程在跑」不等于「显存被占」，
> 真正的 1.8GB 只在第一次合成之后才付。

## 9. 已知遗留

- 测试工作流 #122 留在库里当样板，不需要可 `DELETE /api/workflows/122`。
- `运行/` 下累积了 15 个 `FlowEdge.exe.bak-*`（历史备份），最新那个是本次切换前的版本。

### 8.7 彻底解决「不常驻」与「不用记得点启动」（2026-10-23 21:15）

§8.6 只是「不自动起」，还留了三个靠人记住的坑：跑节点前要记得点启动、用完要记得停、
改配置要重启引擎。这三条现在都由代码兜住。

| 机制 | 解决什么 | 实测 |
|---|---|---|
| **节点按需启动** | 忘了点启动 → 静默回退 SAPI（音色变系统音） | TTS 完全未运行时直接跑工作流 #122 → 自动拉起 → `success / backend=service`，80s 完成 |
| **`idle_unload`** | 用过一次就一直占 1.8GB | 设 50s 测试：合成后 `loaded=True vram=1.79GB` → 50s 后 `loaded=False vram=0.15GB`，显存 14137 → **11935 MiB** |
| **`POST /api/deps/reload`** | 改配置要重启引擎，且改了没生效也没人知道 | 改 `idle_unload 50→600` 后调它，返回 `{"tts.env.AUDIO8_IDLE_UNLOAD":"50 -> 600"}` |

**按需启动的代码位置**：`engine_v2_nodes.go` 的 `execTTSNode`，在 `SynthesizeWith` 之前：

```go
if e.deps != nil && !e.deps.Healthy("tts") && e.deps.Startable("tts") {
    if err := e.deps.Start("tts"); err == nil {
        _ = e.deps.WaitReady("tts", 300*time.Second)
    }
}
```
用 6 秒起服务，换掉「产出错音色但报成功」，这笔账很划算。

**`Reload` 的边界**（别误解它的能力）：
- 覆盖：script / python / port / 参数 / autostart / **TTS 相关环境变量**（`TTS_` `AUDIO8_` `ARKTTS_` `KOKORO_` 前缀）
- **不覆盖**：`base_url` —— TTSService 是独立对象，改地址仍需重启引擎，`changes` 里会标「需重启引擎才生效」
- 已在跑的进程不受影响（`procs` / `started` / `lastErr` 都保留）

> 坑中坑：`Reload` 一开始只比较 binary/args/autostart/endpoint，**没比 Env**，导致
> 改 `idle_unload` 时返回 `changes: {}`（看起来没变化，实际 spec 已更新）。已补
> `ttsEnvDigest` 只抽 TTS 相关前缀比较，避免把整份 `os.Environ` 的噪音算成变更。

**最终常态**（`deps.json`）：`autostart: false` + `idle_unload: 600`
——引擎启动不拉起；跑 tts 节点时自动起；闲置 10 分钟自动卸显存；进程留着下次懒加载。
