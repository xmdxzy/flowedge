# 声线 `anime_mix_5050` 使用手册

> 你选定的声音：**调和 B · 正中**（`0C_合集` 里第二个），中位基频 258Hz，
> 由 `anime_moe`(296Hz 软甜) 与 `anime_healing`(239Hz 温柔气声) 按 5:5 重新设计。
> 本文件是「以后怎么长期用它」的唯一事实源，写于 2026-10-27。

## 1. 三种调用方式（按场景选）

| 场景 | 走哪条 | 延迟量级 |
|---|---|---|
| 前端点选、出片、留任务记录 | FlowEdge `voice` 流水线 | 端到端 2–20s（含建 run/排队/轮询） |
| 直播互动、即时发声 | **直连 TTS 服务** 或 `scripts/voice-bridge.py` | 短句 1.5–3s |
| 批量预生成一批台词 | 流水线（可追溯）或直连（快） | 按条数线性 |

### 1.1 前端（最直观）
出片台 → **文案转配音** → `声线` 下拉选 **anime_mix_5050** → 填台词 → 出片。

### 1.2 流水线 API（要 run 记录时用）
```bash
curl --noproxy '*' -X POST http://127.0.0.1:8080/api/produce \
  -H "Content-Type: application/json" \
  -d '{"pipeline":"voice","params":{"prompt":"欢迎麓进入直播间","voice":"anime_mix_5050"},"invoked_by":"dsh"}'
# → {"run_id":291,...}  然后 GET /api/runs/291 取 outputs 里的 wav 路径
```

### 1.3 直连 TTS 服务（直播就用这个，别走流水线）
```python
import json, urllib.request
body = json.dumps({"text": "欢迎麓进入直播间", "voice": "anime_mix_5050"},
                  ensure_ascii=False).encode()
req = urllib.request.Request("http://127.0.0.1:8103/tts", data=body,
                             headers={"Content-Type": "application/json"})
open("out.wav", "wb").write(urllib.request.urlopen(req, timeout=600).read())
```
返回的就是完整 wav（24kHz 单声道 16bit），没有任务记录、没有队列。

### 1.4 直播桥（推荐，见 §3）
```bash
python scripts/voice-bridge.py --port 8110 --warm     # 自己开个终端跑，别让它跟着 agent 会话死掉
curl -X POST http://127.0.0.1:8110/say -d "{\"text\":\"谢谢老板的火箭！\"}"
```

## 2. 实测数字（本机 RTX 3090 24GB，2026-10-27 同一会话内测得）

| 项 | 数值 | 备注 |
|---|---|---|
| 模型冷加载 | **215s / 245s**（两次） | 需空闲 ≥20.3GB；期间被抢卡会卡死（见 §4） |
| 合成速度 RTF | **0.665**（8 字 → 2.56s 音频 ≈ 1.7s 算力）；最慢一条 1.679 | 与文本长度近似线性 |
| 流水线端到端 | 2.0 / 4.1 / 6.1 / 8.1s（短句） | 含建 run、排队、轮询、产物复制 |
| 直连端到端 | 11 字 2.9s（模型已加载时） | 直播可用的量级 |
| 并发 | **1**（单并发） | 第二个请求 180s 内抢不到锁就 503「服务忙」 |
| 驻留显存 | 15–16GB | `idle_unload=0` 时一直占着 |

**结论**：直播能用的送气节奏是「一句 1.5–3 秒出声」。要更顺，把常用话术预生成成音频块（§3.3）。

## 3. 直播（与观众互动）怎么搭

### 3.1 最小可用链路
```
观众弹幕/礼物 → 你的机器人/脚本 → POST :8110/say {"text": "..."}
                                    → voice-bridge 串行调 TTS → wav → ffplay 播放
                                    → 系统音频进 OBS（虚拟声卡 / OBS 音频输出捕获）
```
`voice-bridge.py` 已经把三件脏活干掉了：单 worker 串行（不会 503）、503 自动重试 3 次、
新句打断旧句（不会叠音）。`--no-play` 则只落 wav，交给 OBS 读文件。

### 3.2 开播前必做（顺序别反）
1. 确认没有出片/3D 任务在跑：`nvidia-smi` 空闲应 ≥20GB。
2. `curl --noproxy '*' -X POST http://127.0.0.1:8080/api/deps/tts/start`
3. 预热一句短的（或 `voice-bridge.py --warm`），等 `/health` 里 `loaded=true`。
4. 再开播。**第一句千万别留给观众触发**——那就是一次 3–4 分钟的静默事故。

### 3.3 预热短语库（把延迟藏起来）
把固定话术批量预生成到 `data/voice-bank/`，直播时直接播文件、不走合成：
```python
PHRASES = ["欢迎麓进入直播间～", "谢谢你的礼物，好开心！", "这个问题问得好，我来说说看。",
           "今天的直播就到这里啦，下次见～"]
# 逐条 POST :8103/tts → data/voice-bank/<hash>.wav
```
只有「没人说过的新句子」才走实时合成。

### 3.4 音频进 OBS 的两条路
- **虚拟声卡**：装 VB-Audio Cable，`voice-bridge` 播放到 CABLE Input，OBS 采集 CABLE Output。
- **文件源**：`--no-play`，OBS 用媒体源指向 `data/voice-bridge/` 最新文件（有 1–2s 缓冲，适合慢节奏）。

## 4. 最容易踩的三个坑（今天都实测过）

1. **别的任务抢卡 → TTS 被卸载 → 下一句等 3–4 分钟。**
   引擎在切到扩散侧/语言侧时会 `releaseAudioSide` 主动卸载语音侧。所以**直播期间不要跑
   出片、H3、hy3d 贴图**。
2. **加载被抢卡会卡死**：TTS 进 `phase runtime`（CUDA Graph 捕获）时若显存被别人拿走，
   会占着 16GB、`loaded=false`、十几分钟不 ready（今天 14:00 与 15:00 各踩一次）。
   处理：`POST /api/deps/tts/stop`，腾出显存后 `start` 重来。
3. **`voice` 参数拼错不会报错**：Breeze 服务对未知音色会**静默回落到默认音色** `zh_female_warm`，
   出来的是另一个人的声音。直播脚本里把 `anime_mix_5050` 写成常量，别手打。

## 5. 怎么保证「长期」还在（持久化与迁移）

声线 = **一行中文描述**，存在两个地方（都要有，缺一个就会在某次更新后消失）：

| 位置 | 作用 | 检查命令 |
|---|---|---|
| `运行/local-services/tts_breeze_server.py` | 引擎实际跑的那份（改完要重启 tts） | `Select-String -Path 运行\local-services\tts_breeze_server.py -Pattern anime_mix_5050` |
| `backend/local-services/tts_breeze_server.py` | 构建时打进 exe 的那份，`--export-services` 从它导出 | 同上换路径 |
| `运行/data/pipelines.json` + `data/pipelines.json` | 流水线「声线」下拉的枚举值 | `curl --noproxy '*' http://127.0.0.1:8080/api/pipelines` |

**备份这一行（抄走即可，任何机器复制进 `BUILTIN_VOICES` 就能复现同一把嗓子）**：
```python
"anime_mix_5050": "一位年轻少女，声音清甜而柔和，音调中等偏高，语速舒缓，语气温柔里带一点明快的笑意，既软又安静。",
```

**注意**：
- 生成是**采样**的：同一句同一音色两次跑，波形不一致（实测 sha256 不同），但音色一致。
  需要逐条一模一样时，把生成好的 wav 存成分库（§3.3），别再合成。
- 若哪天换了 TTS 后端（Kokoro / Audio8），这个预设无效——它只属于 Breeze TTS 2 的「音色设计」模式。
- 可选加固：把这把嗓子最好的那条 wav 用 `POST :8103/voices/register {"name":"anime_mix_5050","audio_path":"...","text":"..."}`
  注册成克隆音色（落盘在 `voices-breeze/`），脚本被覆盖也不会丢；但**克隆是二次逼近**，
  音色会和设计版有细微差别，建议只当备份。

## 6. 没验证的部分

- **与真人互动端到端延迟**：我只测了「合成出声」，弹幕链路（识别/LLM 回复/推流）没接，也没测。
- **OBS 音频路由**没实机验证（虚拟声卡未装、未采流）。
- 直播长时间连续合成的稳定性（温度/显存碎片）没跑过 1 小时以上压测。
- `anime_genki` 短句可懂度仍存疑（8 字时 ASR 转出「欢迎订阅我的频道」），本声线不受影响。
