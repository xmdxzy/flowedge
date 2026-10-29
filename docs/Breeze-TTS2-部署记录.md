# Breeze TTS 2 部署记录（2026-10-26）

> 替换原 Audio8-TTS 0.6B 作为 FlowEdge 的本地语音后端。
> 所有数字都是本机（RTX 3090 24GB / Windows / Python 3.11.9）实测，不是抄的。

## 1. 现状一览

| 项 | 值 |
|---|---|
| 模型 | Breeze TTS 2（`BreezeBlue/Breeze-TTS-2`，3.47B BF16，2026-08-25 发权重） |
| 权重 | `D:\AI\Models\breeze_tts_2`（7.16GB / 13 文件，sha256 全绿） |
| 官方代码 | `D:\AI\breeze-tts`（github.com/breezeblue-ai/breeze-tts，clone 于 2026-10-26） |
| 独立 venv | `D:\AI\envs\breeze-tts-pt`（torch 2.9.1+cu128 / transformers 4.57.3 / qwen-tts 0.1.1 / triton-windows 3.5.1 / faster-whisper 1.2.1） |
| 服务脚本 | `运行/local-services/tts_breeze_server.py`，端口 **8103** |
| 启动脚本 | `运行/local-services/启动-Breeze-TTS.bat` |
| 配置 | `运行/data/deps.json` 的 `tts` 段（备份：`deps.json.bak-audio8`） |
| 许可证 | **权重仅限研究/非商用**（BreezeBlue Research & Non-Commercial）；官方 API 付费订阅才可商用。代码 Apache-2.0 |

## 2. 性能实测（同一段中文文案，A/B 只动一个变量）

| 配置 | RTF | 预热 | 峰值显存 |
|---|---|---|---|
| eager（全关图） | **10.81** | ~60s | 8.3GB |
| 快路径但深度图关（`BREEZE_FAST_DEPTH=0`） | **9.5** | 70s | 8.3GB |
| **快路径全开（默认）** | **0.66–0.70** | 90s（缓存热）/ 222s（冷编译） | ~16.4GB |

**结论：速度几乎全部来自深度解码器的图编译，必须开。**
关掉深度图只省预热时间，RTF 掉回 9.5（等于没用）。

引擎托管启动实测：**冷 245s / 缓存热 117s**。
≥2 次复现的规律：**启动时显存不足 22GB，预热会被拖到 300s 以上**（图捕获本身要吃满整卡），
所以起 TTS 前必须先腾显存（见 §7）。

它还依赖一条关键补丁：

```
TORCHINDUCTOR_USE_STATIC_CUDA_LAUNCHER=0
```

不加这条，Windows 上 `torch._inductor.runtime.static_cuda_launcher` 会抛
`OverflowError: Python int too large to convert to C long`（Windows 的 C `long` 是 32 位，
指针塞不进去）。triton 3.5.1 与 3.8 都复现，与版本无关，是 torch 在 Windows 的缺陷。
退回 Python 启动器后快路径完全可用，代价只是编译慢一点。

脚本里已用 `os.environ.setdefault` 在 `import torch` 之前兜底，bat 里也显式设了。

## 3. 验收记录

| 项 | 结果 |
|---|---|
| `/health` | 200，`loaded` 反映真实加载态；加载中 `?deep=1` 回 503 + 「正在预热」 |
| `/tts` 契约 | 200 / `audio/wav` / RIFF 头正确 / 24kHz 单声道 s16 |
| 波形 | 6.0s，峰值 0.475（不削波），RMS 0.083，六段均有能量（无漏字） |
| ASR 复核（faster-whisper large-v3） | 「你好,我是序册的语音测试,今天的风很轻,适合把故事慢慢讲完。」——「序策」被听成同音「序册」，其余逐字正确 |
| **FlowEdge 端到端（手动起服务）** | `POST /api/produce {pipeline:"voice"}` → run 251 **success**，16s 出片，节点 `backend:"service"`（不是 SAPI 兜底），产物 288,044 字节 / 6.0s |
| **FlowEdge 端到端（引擎托管）** | run 255 **success**，`backend:"service"`，7.36s / 24kHz / 峰值 0.508；ASR 转写「引擎现在可以自己把Breeze语音服务拉起来,也能停掉它,整条链路真正闭环了。」**逐字全对** |
| **修复后最终验证** | 新引擎（含 600s）托管启动 **111s** 就绪、`managed=True`、单进程、父进程就是引擎本体；run 262 **success**，`backend:"service"`，6.8s；ASR「这是新引擎托管下的最终验证。Breeze语音服务,由引擎自己拉起,自己监管。」（仅冒号变句号） |

## 4. 接口映射（与 Audio8 同一套契约，Go 端不用改）

| FlowEdge 字段 | 本服务行为 |
|---|---|
| `text` | 原文；>180 字按句切段后逐段合成、段间 120ms 静音 |
| `voice` | 音色名 → 内置设计预设（`zh_female_warm` 等 7 个）或 `voices-breeze/<name>/` 里注册的克隆音色；传一句话（≥8 字）则直接当**音色设计指令** |
| `ref_audio` | 克隆；**必须**同时有逐字稿，来源顺序：`ref_text` → 同名 `.txt` → `voices-breeze/<voice>/ref.txt`；都没有则 **400 显式报错**（不猜稿子，猜错等于换人） |
| `instruction` / `cfg_scale` / `seed` | 直通；设计/导演模式默认 `cfg_scale=4` |
| `speed` / `rate` | **不支持**，收到即忽略并在日志注明（Breeze 无语速控制） |

四种模式由官方模板自动选：有参考+指令=导演(`ref_edit_tata`)、有参考=克隆(`ref_clone_tata`)、
无参考+指令=设计(`tts_instruction`)、都没有=平读(`tts_plain`)。

## 5. 踩过的坑（按浪费的时间排序）

1. **代理把下载限到 1.6MB/s**。curl 继承 `http_proxy=127.0.0.1:23480`，ModelScope 直链只跑 1.6MB/s；
   加 `--noproxy '*'` 后 **25MB/s**（15.6×）。7.16GB 从 75 分钟变 5.5 分钟。
2. **pip 解析依赖会卡死**。`transformers==4.57.3 + qwen-tts==0.1.1` 一起装会回溯十几分钟不动
   （缓存零增长）。改 `--no-deps` 分开装，缺什么补什么。
3. **清华源给的是 `torch 2.9.1+cpu`**。Windows 上 `pip install torch` 默认拿 CPU 版，
   必须 `--index-url https://download.pytorch.org/whl/cu128`。
4. **`Start-Process` 在 PS 5.1 会因环境变量 `no_proxy`/`NO_PROXY` 大小写冲突直接报错**
   （"Item has already been added"），不是权限问题。
5. **`.ps1` 被执行策略挡**，要么 `-ExecutionPolicy Bypass`，要么别用脚本文件。
6. **「卡住」类问题别靠猜，直接抓栈**：`pip install py-spy` 然后
   `py-spy dump --pid <pid>`，再配合 `(Get-Process -Id <pid>).TotalProcessorTime` 看 CPU 是否在涨：
   - 栈不动 + CPU 100% = 空转/死循环（本次就是 numba 缓存路径）；
   - 栈不动 + CPU 不涨 = 真阻塞（网络/锁）。
   这条把「引擎托管为什么起不来」从几小时缩到两次 dump。

## 6. 引擎托管自动启动：已修复（三个独立根因）

一开始 `/api/deps/tts/start` 起了进程却永远到不了 `loaded=true`，而且**停不掉**
（报「不是由本进程拉起的」）。逐个挖出来是三个互不相干的问题：

### 6.1 venv 的 `python.exe` 是转发器（274KB），引擎会丢句柄

venv 生成的 `Scripts\python.exe` 不是真解释器，它会再拉起基础解释器的子进程、自己退出。
引擎 `exec.Command` 拿的是转发器的句柄，转发器一退，`cmd.Wait()` 返回 → 引擎把这条从
`procs` 里删掉 → `managed` 变 false，而真正干活的服务成了**孤儿进程**（还占着 16GB 显存），
于是「停不掉、再 start 又起一个」。

**修法**：用基础解释器覆盖 venv 的（`pyvenv.cfg` 还在，venv 语义不变）：

```powershell
Copy-Item 'C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe' `
          'D:\AI\envs\breeze-tts-pt\Scripts\python.exe' -Force
# 原件备份在 python.exe.launcher-orig
```

修完：进程数 1，`managed=True`，`/api/deps/tts/stop` 真的能停。

### 6.2 引擎给的 TMP 触发 numba 空转（CPU 100%，13 分钟不返回）

引擎把子进程的 `TMP`/`TEMP` 指到 `…\Desktop\flowedge\运行\data\tmp`（带中文的深路径）。
在这个环境下 `qwen_tts → librosa → numba` 会卡死在
`numba.core.caching.ensure_cache_path` 的 `tempfile.NamedTemporaryFile` 上：
栈不动、CPU 100%、13 分钟不返回，日志只停在 `phase imports`。
手动跑（`TMP=%LOCALAPPDATA%\Temp`）同一份脚本 15 秒就过——对照组非常明确。

**修法**（脚本里已内置）：
- `NUMBA_DISABLE_JIT=1`：numba 走纯 Python 实现，整条 `ensure_cache_path` 路径不执行。
  它只被用来做音频重采样这类小函数，功能不受影响。想恢复 JIT 设 `BREEZE_NUMBA_JIT=1`。
- 同时把 `TMP`/`TEMP`/`NUMBA_CACHE_DIR` 钉到 `D:\AI\cache\{tmp,numba}`（纯 ASCII 短路径），
  顺带让 torch inductor 的编译缓存有稳定落点——缓存热的时候启动 117s，冷的时候 245s。

### 6.3 引擎传 `HF_ENDPOINT` 但没设离线

`deps.go` 会给 TTS 子进程传 `HF_ENDPOINT=https://hf-mirror.com`。权重全在本地，
但缺 `HF_HUB_OFFLINE` 时 `Qwen3TTSTokenizer.from_pretrained` 仍会走一次 hub 解析，
镜像不可达时没有超时。脚本已 `setdefault("HF_HUB_OFFLINE","1")` +
`TRANSFORMERS_OFFLINE=1`。

### 6.4 修复后的实测

| 项 | 结果 |
|---|---|
| `POST /api/deps/tts/start` | 202 → **117s**（缓存热）/ 245s（冷编译）后 `loaded=true` |
| `managed` | **true**（进程数 1） |
| `POST /api/deps/tts/stop` | **能真正停掉**，显存回收 |
| `POST /api/produce {pipeline:"voice"}` | **run 255 success**，`backend:"service"`，7.36s / 24kHz / 峰值 0.508 |
| ASR 复核 | 「引擎现在可以自己把Breeze语音服务拉起来,也能停掉它,整条链路真正闭环了。」**逐字全对（含 Breeze）** |

⚠️ 仍然保留的约束：**启动瞬间显存必须 ≥22GB**，否则预热会被拖到 300s+。
2026-10-26 23:00 已把 `deps.go` 的 `readyTimeout("tts")` 从 300s 提到 **600s**
（原来和 asr 共用 300s，只剩 55s 余量，实测被卡过两次），并加了回归测试
`backend/service/deps_readytimeout_test.go` 钉住这个值。

### 6.5 exe 已重编并部署（2026-10-26 23:19）

- `python scripts/build-windows.py --skip-ui` → `dist/FlowEdge.exe` 19.2MB
- 已用 `mv` 绕锁部署到 `运行/FlowEdge.exe`，旧版留作
  `运行/FlowEdge.exe.old-20261026-231919`（回滚就是 `mv` 回来）
- 顺带把 `tts_breeze_server.py` 加进 `build-windows.py` 的内嵌白名单（原来只有 3 个，
  连 Audio8 的脚本都没进，换目录重装后 `deps.json` 会指向不存在的脚本）
- ⚠️ 部署时运行中的引擎是旧代码，但它在 **23:36:58 自行重启**后已加载新 exe
  （`FlowEdge.exe` 23:17 落盘 → 引擎 23:36 重启 → 600s 已生效）。
  验证方式：`GET /api/health` 的 `uptime_seconds` 起点晚于 exe 的 `LastWriteTime`。

### 6.6 起 TTS 前先看显存

实测过两次踩坑：显存被 H3 生产任务占着（23.5GB）时启动 TTS，会卡在 `phase imports`
或预热里十几分钟，而且**不报错**。判断方法：

```powershell
nvidia-smi --query-gpu=memory.free --format=csv,noheader   # 要 ≥22000 MiB
nvidia-smi --query-compute-apps=pid,process_name --format=csv,noheader   # 看谁在占
```

### 6.7 显存预检（2026-10-27 加，服务内自带）

上面那种「沉默地慢」是最难查的一类失败。现在服务自己会先判：

- **阈值按峰值给 22GB**，不是按权重 8.3GB —— 权重只是一半，CUDA Graph 捕获会把整卡吃满
  （实测峰值 ~23.4GB）。给 18GB 会放行一个注定失败的启动（实测过一次：22.0GiB 放行，
  随后权重加载花了 **334s**，正常只要 11–25s）。
- **采样 3 次取最小值**，间隔 1.5s —— ComfyUI/H3 是异步加载的，单次采样很容易撞上
  它们「刚好卸完」的空隙。
- 不够就 **当场抛错**，`/health?deep=1` 回 **503**，错误串里直接写清下一步怎么做
  （卸哪个端口的模型、要多少空闲、怎么强行跳过）。

验证（2026-10-27，两条都实跑）：

| 路径 | 触发 | 结果 |
|---|---|---|
| 失败 | `BREEZE_MIN_FREE_GB=99`（阈值覆盖，备用端口 8104） | 日志 `vram free=22.8GiB (min of 3 samples) need≈99.0GiB` → `RuntimeError: 显存不足…`，`/health?deep=1` = **503** |
| 成功 | 真实启动（显存 23.5GB 空闲） | `vram free=22.8GiB (min of 3) need≈22.0GiB` → **177.7s 就绪**，`managed=True`，合成 RTF **0.70** |

可调开关：`BREEZE_MIN_FREE_GB`（阈值）、`BREEZE_ALLOW_LOW_VRAM=1`（硬上，跳过预检）。

## 7. 显存纪律：不能和 H3 共存（启动前必须 ≥22GB）

- Breeze 快路径常驻 **~16.4GB**；H3 权重 **~20.5GB**；合计远超 24GB。
- 实测过两次：H3 模型驻留时 Breeze 只有 847MB~9.5GB 可用，预热会被拖到 300s 以上甚至永远不 ready
  （**不报错，只是卡在预热**——这是最难查的一种失败，别盯着端口看，看 `运行/logs/tts.log` 的 `phase` 行）。
- **顺序**：跑 TTS 前先 `POST http://127.0.0.1:8189/free {"unload_models":true,"free_memory":true}`
  （等 10 秒让 WDDM 真正归还），再 start TTS；跑 H3 前先 `POST /api/deps/tts/stop`。
- `idle_unload=0`（常驻）：卸载后重新预热要 117–245 秒，会让下一句台词超出 Go 侧的 HTTP 预算
  （`60s + 1.5s/字`），所以宁可常驻、显式停。

## 8. 日常操作

```powershell
# 启动（推荐：走引擎托管，能停能管）
curl.exe --noproxy '*' -X POST http://127.0.0.1:8080/api/deps/tts/start
# 立刻就绪约 117s（缓存热）/ 245s（冷编译），/health 会先回 200 + loaded=false

# 停止（回收显存，跑 H3 前必须）
curl.exe --noproxy '*' -X POST http://127.0.0.1:8080/api/deps/tts/stop

# 备选：直接起进程（引擎不在、或想自己盯日志时）
运行\local-services\启动-Breeze-TTS.bat

# 探活
curl.exe --noproxy '*' http://127.0.0.1:8103/health
curl.exe --noproxy '*' "http://127.0.0.1:8103/health?deep=1"   # 真合成一小段

# 手动合成一句（返回 wav）
curl.exe --noproxy '*' -X POST http://127.0.0.1:8103/tts -H "Content-Type: application/json" ^
  -d "{\"text\":\"测试一句\",\"voice\":\"zh_female_warm\"}" -o test.wav

# 释放显存（进程留着，不推荐——再预热要 2 分钟）
curl.exe --noproxy '*' -X POST http://127.0.0.1:8103/unload
```

## 9. 回滚到 Audio8（3 步）

改 `运行/data/deps.json` 的 `tts` 段，或直接把备份换回来：

```powershell
Copy-Item 运行\data\deps.json.bak-audio8 运行\data\deps.json -Force
```

等价的字段改动：
- `script`: `tts_breeze_server.py` → `tts_audio8_pt_server.py`
- `python`: `D:/AI/envs/breeze-tts-pt/Scripts/python.exe` → `D:/AI/envs/torch-cu124/Scripts/python.exe`
- `port` / `base_url`: 8103 → 8103（同端口，无需改）
- `idle_unload`: 0 → 600

然后重启引擎（`base_url` 改了必须重启，见 `deps.go:941`）。

## 10. 许可提醒

权重是 **BreezeBlue Research & Non-Commercial License**：研究/个人非商用免费，
商用需要另外授权（官方托管 API 的付费订阅也只覆盖 API 产出，不覆盖自托管产出）。
代码 Apache-2.0 不改变权重的许可。做商业交付请换模型或买授权。

---

## 11. 2026-10-27 修订：设备写法、请求堵死、后端切换器

### 11.1 `device: cuda` 会让启动「看起来成功、其实永远不就绪」

配置里写 `cuda`（人话）时，`torch.cuda.set_device("cuda")` 直接抛：

```
ValueError: Expected a torch.device with a specified index or an integer, but got:cuda
→ RuntimeError: Failed to set CUDA device device=cuda
```

表面症状极具误导性：日志出现「预加载失败，转为懒加载」，`/health` 一直 `loaded=false`，
但不把这个错报给客户端。**修法**：脚本里把 `cuda` 归一成 `cuda:0`（已内置），
`device` 现在也接受 `cuda:N`。

### 11.2 一个卡住的请求会把整个服务永久堵死

Breeze 单并发（一把合成锁）。2026-10-27 实测：出片节点 3 分钟超时、客户端放弃，
但**服务端仍在生成**，锁被它一直占着；后续每个请求都排队，然后一起超时，
看起来像「服务死了」。

**修法**：抢锁加超时（`BREEZE_BUSY_TIMEOUT`，默认 180s），等不到立刻回 **503** 并说明原因，
上游能很快拿到「忙」而不是干等 3 分钟。

### 11.3 切换后端用脚本，别手改 deps.json

2026-10-27 早上两个 agent 各自改同一个 `tts` 段（08:12 改 Breeze、08:21 改回 Kokoro），
互相覆盖，最后没人知道生效的是哪套。改用：

```powershell
python scripts/set-tts.py --status        # 当前生效哪套 + 两个端口状态
python scripts/set-tts.py breeze          # 切 Breeze（停旧后端→清孤儿→重启引擎→等就绪）
python scripts/set-tts.py kokoro          # 切 Kokoro
```

脚本会备份 deps.json、校验 python/脚本存在、切换前**停掉旧后端并清理遗留进程**
（否则 Kokoro 与 Breeze 会同时常驻，排查时极易误判成「新后端有问题」）。

### 11.4 出片脚本现在真的会恢复 TTS

`scripts/produce-with-gpu.py` 原来只写了一行「出片结束后会尝试恢复」，**没有任何恢复代码** ——
每跑一次视频 TTS 就一直停着，下一条 voice 直接失败。现在补齐：`start_dep()` 会等到
`loaded/warm` 为真（不是只看引擎的 `state=ok`，那只是进程活着），并认 Breeze 的 `loaded`
与 Kokoro 的 `warm` 两种字段。

### 11.5 实测：Breeze 必须独占 GPU

| 场景 | 就绪 | 合成 |
|---|---|---|
| **GPU 干净**（只剩桌面 2.8GB） | **152s** | 首请求 5.8s / RTF **0.873**；稳态 4.4s / **0.855** |
| 与 ComfyUI+H3 共存 | 425–560s | **挂住**（>200s 无响应，空闲显存只剩 0.6–1.4GB） |

结论：**Breeze 不是「慢一点」，是「共存时就不可用」**。正确姿势：
`停 ComfyUI/H3 → set-tts breeze → 用完停它 → 再起 ComfyUI/H3`；
`produce-with-gpu.py` 就是按这个顺序编排的。

若希望 voice 与出片并行、完全不动显存，只能用 Kokoro（CPU，质量低一档且不能克隆）。

