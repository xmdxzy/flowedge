# Audio8-TTS 部署记录（2026-10-23）

> 一句话结论：**装好了、能跑、但产出不合格。** 0.1B INT8 ONNX 在本机生成的音频
> 无法还原输入文本（ASR 只识别出「咦咦咦…」/「明明」），权重、codec 解码、ASR 管线
> 三者的正确性都已单独验证过。**不要把它接进出片流水线。**

## 一、Audio8-TTS 是什么

Edge0（品牌 Audio8）开源的**小参数零样本音色克隆 TTS**，DualAR 架构（慢分支 24 层
出语义 token、快分支 4 层出 10 层声学码本、独立 44.1kHz 神经 codec）。

| 项 | 值 |
|---|---|
| 仓库 | `github.com/Edge0-AI/Audio8_TTS`（原 `Audio8-AI/Audio8_TTS`，改名过） |
| 许可 | **代码与权重均为 Apache-2.0**（LICENSE + NOTICE + HF 模型卡三处一致） |
| 版本 | 0.1B（170M，Falcon-H1 混合骨干）/ 0.6B（601M，Qwen 骨干） |
| 语言 | 11 种，含普通话、粤语 |
| 采样率 | 44.1kHz |
| 能力 | 零样本克隆（参考音频 0.5~30s + 逐字稿）、**无语速、无情绪标签** |

> ⚠️ 网传「权重是 Audio8 Community License、年营收 200 万美元封顶」是**错的**——
> 那是第三方博客口径。以仓库 LICENSE 与 HF 模型卡 `license: apache-2.0` 为准。

## 二、部署了什么

| 位置 | 内容 |
|---|---|
| `D:/AI/Audio8_TTS/` | 上游仓库（`git clone --depth 1`） |
| `D:/AI/Audio8_TTS/onnx_runtime_0_1b_int8/.venv/` | 独立 venv（python 3.11，onnxruntime 1.22.1，无 torch） |
| `D:/AI/Models/audio8_tts_0_1b_int8/` | 权重 686MB（含 registration 编码器），**sha256 已逐文件校验** |
| `运行/local-services/tts_audio8_server.py` | FlowEdge 契约适配层（新文件，与 Kokoro 同形状） |
| `运行/local-services/voices-audio8/default/` | 默认音色（由模型自带 reference_codes 生成，110 帧） |
| `backend/service/deps.go` | 新增 `TTS_SCRIPT` 环境变量，可切换启动哪个 TTS 脚本 |
| `backend/service/nodecontract.go` | tts 节点契约文案改为 Kokoro / Audio8 双后端 |
| `frontend/src/components/NodeInspector.tsx` | 面板文案与占位符同步 |

**没有替换 Kokoro**。Audio8 是并行存在的第二后端，两者共享 `POST /tts {text, voice}` 契约。

## 三、怎么切换（当前不建议）

```bash
# 切到 Audio8（端口 8103）
TTS_SCRIPT=tts_audio8_server.py
TTS_PORT=8103
TTS_BASE_URL=http://127.0.0.1:8103
TTS_PYTHON=D:/AI/Audio8_TTS/onnx_runtime_0_1b_int8/.venv/Scripts/python.exe

# 切回 Kokoro（默认，什么都不设）
```

适配层端点：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 轻量探活；`acceptance=failed` 明示未通过验收 |
| GET | `/health?deep=1` | 真合成一小段，区分「跑着但坏了」 |
| GET | `/voices` | 已注册音色 |
| POST | `/tts` | `{text, voice}` → wav（44.1kHz） |
| POST | `/voices/register` | `{name, text, audio_path}` → 从本地参考音频注册克隆音色 |

## 四、实测数据（Xeon E5-2686 v4 / 36 线程 / 纯 CPU）

| 指标 | 实测 |
|---|---|
| 模型加载 | 8.0 ~ 21.4s（3 个 ONNX session） |
| prompt 长度 | **181 帧**（参考音色 110 帧 + 71 个文本 token），逐 token prefill |
| 单步 slow AR | 59ms（4 线程）/ 104ms（16 线程）/ 84ms（32 线程） |
| 生成 128 帧（5.94s 音频） | 39s（ORT 1.23）/ 49s（ORT 1.22） |
| 生成 512 帧（23.78s 音频） | 95 ~ 109s |
| **RTF** | **4.0 ~ 6.0**（生成 1 秒音频要 4~6 秒 CPU） |
| 进程 RSS | 约 10GB（三 session 同时驻留） |

两条独立结论：

1. **线程越多越慢**。batch=1 逐 token 自回归，多线程只增加争抢。`ARKTTS_THREADS` 默认已改 4。
2. **固定成本是 prefill**。181 步 × 59ms ≈ 11s，与文本长短无关；短句特别亏。

## 五、产出不合格的证据链

**现象**：任意文本都跑满 `max_new_tokens` 不早停，音频有能量（RMS 0.18~0.27）但不含语音。

| # | 检查项 | 结果 |
|---|---|---|
| 1 | 11 个权重文件 sha256 vs HF 官方 | **全部匹配**（4 个大 `.data` + reference_codes + tokenizer 尺寸精确） |
| 2 | 用官方 `reference_codes.npy` 直接过 codec 解码 → ASR | **`至今為止,原據火券總共發行了兩艘專輯`**，与 manifest 里 `reference_text`「至今为止，元气火箭总共发行了两张专辑。」一致 ⇒ **codec 正常** |
| 3 | ASR 管线自身可信度 | Kokoro 历史产物被正确转写（「这是Flow Edge在本地生成的一段旁白…」）⇒ **ASR 判据可信** |
| 4 | onnxruntime 1.22.1 vs 1.23.2 | 同一输入生成的 semantic 序列**逐位相同** ⇒ 不是 ORT 版本 |
| 5 | 10 层码本熵 | 4.95 ~ 6.57，无塌缩、无复读（max_run=2）⇒ 生成侧「看起来正常」 |
| 6 | 生成音频谱平坦度 | **0.0000**（Kokoro 是 0.0020），ZCR 0.064 ⇒ 持续哼鸣，缺音素变化 |
| 7 | ASR 转写生成音频 | `咦咦咦咦咦咦…` / `明明` ⇒ 未还原文本 |
| 8 | A/B：去掉 `<|speaker:0|>` 前缀 | tokenizer.json 里**没有** `<|speaker:N|>` 特殊 token，被拆成 6 个普通 token；去掉后仍不合格 ⇒ **不是唯一根因**（但确实是上游缺陷，PyTorch 版走 AutoProcessor 才有这项） |

**定位**：问题在 **slow AR / fast AR 的 ONNX 图产出**（或 prompt 中参考码本的嵌入方式），
不在权重、不在 codec、不在 ORT、不在 ASR。

**措辞边界**：这只能说明「**0.1B INT8 ONNX 导出在这台机器上不可用**」，
不能说「Audio8-TTS 不可用」——PyTorch 版、0.6B 版、0.6B ONNX INT4 版都还没试。

## 六、顺手踩到的环境坑

**`huggingface-cli download --local-dir` 在本机下大文件会静默失败**：
下载完成后 `.data` 文件没有落到目标路径，只剩 `.cache/**/*.incomplete`，命令退出码仍是 0。
本次 4 个大文件（248MB/133MB/36MB/414MB）全部中招，日志里还混着
`[safe-delete][SAFE_DELETE_BULK_CONFIRM_REQUIRED]`。

**处置**：改 `curl -L --noproxy '*'` 直接打 `hf-mirror.com/.../resolve/main/<path>`，
下完用 HF API 的 `lfs.oid`（sha256）逐文件校验。校验脚本 `.tmp/verify_a8_weights.py`。

## 七、下一步（按性价比排序）

| 选项 | 成本 | 预期 |
|---|---|---|
| **A. 0.6B PyTorch + CUDA** | 新增 venv + torch（~2.5GB）+ 权重 1.7GB，约 40 分钟 | 3090 上 RTF 预计 0.1~0.3（官方 SGLang 在 H20 是 0.116），占显存约 1.5GB。**最可能可用** |
| B. 0.6B ONNX INT4（另一套 runtime） | 权重 ~1.4GB + 独立 runtime | 仍是 CPU 路径，RTF 问题不会改善，只是换模型质量 |
| C. 给上游报 issue | 10 分钟 | 附本页证据链；等作者修 0.1B ONNX 图 |
| D. 放弃 Audio8，继续用 Kokoro | 0 | Kokoro 没有克隆能力，但音质可用、RTF ≪1 |

选 A 的唯一代价与现有硬约定冲突：**TTS 从 CPU 侧挪到 GPU 侧**，需要走显存单侧驻留
（与 flux 17GB 抢卡）。若只在「无视频任务」的时段配音，实际影响可控。
