# ACE-Step 音乐生成最高质量流水线（2026-11-06 定稿）

## 一、环境安装（一次性）

```bash
# 核心依赖（系统 Python 3.11+，或你常用的 venv）
pip install soundfile librosa numpy

# Demucs 分轨（htdemucs_6s）
pip install demucs

# Audiobox-aesthetics 审美分
pip install audiobox-aesthetics torch

# 下载审美权重（首次跑会自动提示，但手动下更稳）
mkdir -p .tmp/aes
curl -L https://dl.fbaipublicfiles.com/audiobox-aesthetics/checkpoint.pt -o .tmp/aes/checkpoint.pt
```

**验证**：

```bash
python -c "import soundfile,librosa,demucs; print('OK')"
ffmpeg -hide_banner -version | head -1
```

---

## 二、使用方式（一条命令到底）

### 场景 A：为已有成片配乐（纯音乐）

```bash
python scripts/song-pipeline.py \
    --prompt "Instrumental cinematic ambient R&B ballad in F# minor at 84 BPM, soft brushed drums, intimate Rhodes piano, subtle bass, atmospheric pads, emotional arc from intro to lifted chorus, no vocals." \
    --duration 180 --steps 32 --batch 6 --tag rnb_fshminor --model acestep-v15-sft \
    --auto-select
```

**参数说明**：

| 字段 | 默认 | 推荐值 | 备注 |
|---|---|---|---|
| `--duration` | 200 | 120–240 | 一次长生成，不拼接 |
| `--steps` | 24 | 32–48（旋律细节） | 纯音乐≥24，有钢琴/人声建议 32–48 |
| `--batch` | 6 | 6–8（24GB） | tier6b 上限 8，建议 6–8 候选 |
| `--model` | sft | sft（高质） | turbo（快）适合快速试 prompt |
| `--key` | F# minor | 目标调性 | 影响审美评分差值对比 |

### 场景 B：A/B 测试不同提示词

```bash
for P in "cinematic jazz" "orchestral epic" "electronic chill"; do
  python scripts/song-pipeline.py --prompt "$P" --duration 120 --steps 24 --batch 4 --tag test_$P --auto-select
done
```

---

## 三、输出产物

```
.tmp/songs/<tag>/
├── candidates/
│   ├── c0_xxxx.wav         # 全部候选（含客观指标 metrics.json）
│   └── c1_xxxx.wav
├── stems/
│   └── <stemdir>/<drums,bass,other,guitar,piano,vocals>.wav  # Demucs 分轨
├── remixed.wav             # 自动重混版（钢琴抬到 -10dB、鼓压到 -8dB）
├── remixed_master.wav      # 母带版（-14 LUFS / ≤-1.0 dBTP）
├── metrics.json            # 各候选指标表（bands/gaps/dur/ae_scores）
└── audit.json              # 成品审计（验收通过/失败原因 + 审美分）
```

**audit.json 解读**：

```json
{
  "selected_candidate": "c0_xxxx.wav",
  "audited_remix": "remixed_master.wav",
  "passed": true/false,     // false = FAIL，需要调整
  "metrics": {
    "lufs": -14.2,
    "tp": -1.1,
    "gaps": []               // 中段静音缺口
  },
  "ae_scores": {"CE":7.8, "CU":7.6, "PC":7.9, "PQ":8.1}
}
```

---

## 四、显存加速现状（3090 24GB）

**确诊事实**（ACE 日志证据）：

- **tier6b**: `Max Batch=8`, `CPU offload 已关` ✓ 最佳
- **flash_attn 未装** → DiT 走 SDPA 回落（比 eager 好，但无 flash）
- **torch.compile 无效**：Windows 无 triton，compile_model 开关开了也白开
- **单条预检**: `~1.00 GB needed (batch=1)` → **batch=8 完全装得下**（空闲 11.9GB）

**结论**：**不用换模型、不用额外优化**。当前方案已经吃满你的显存：

- 一次 `batch=6` 生成 6 条候选，总耗时 ≈ 单条×6（固定开销摊薄）
- 分轨/重混/母带全部 CPU，不抢 GPU
- **唯一能做的效率提升**是加 batch 到 8（如果你要更多候选挑）

---

## 五、常见问题

**Q: 为什么一次生成这么长？不是只能出 10–12 秒吗？**

A: 这是旧版结论（turbo 短曲）。官方 `xl-sft` + 20 步下，240s 请求实得 236s 连续音乐，没有「10 秒天花板」。多段拼接反而接缝断气（用户原话「只有 1 分钟后能听」）。

**Q: 提示词写「piano forward」没用？**

A: 实测同一批 4 条里钢琴电平从 −12.7 到 −37.6 乱飘。**模型根本不按混音指令分配声部**。平衡必须靠分轨重混（把钢琴 stem 直接 +10dB）。

**Q: 审美分能不能替代人耳？**

A: 不能。audiobox-aesthetics 评的是制作质量/内容愉悦度，评不出「旋律是否动人」或「钢琴被埋」。但它能筛掉明显做坏的候选，省你听废片的时间。

**Q: 批量生成（batch=N）会不会 OOM？**

A: tier6b 明确给了 max_batch_size_without_lm=8，你的空闲显存足够。如果真遇到 OOM，把 `--batch` 降到 4–6 就行。

---

## 六、后续可选增强（P1/P2）

- **参考曲风格迁移**（reference_audio_path 驱动节奏锚）：喂 160BPM click，节拍跟上 ±4 BPM，音色不被污染
- **歌词生成**（开启 CoT）：纯音乐关闭 CoT，有歌词时 `use_cot_caption=True`
- **LoRA 训练专属风格**：`/v1/training/start` 上传 10 首样曲 → 训练专属风格 → `/v1/lora/*` 调用

---

## 七、实测记录（2026-11-06，We Never 源曲锚定，3090 24GB）

**源曲**：《秋成 - We Never(若如初见)Vibe氛围版.flac》→ DSP 实测 BPM 112.3 / F minor / 184.6s /
low 85.9% / percussive 0.145 / centroid 266Hz。

**生成**：F minor @112BPM、210s、32 步、batch=6、sft 模型。
**16:10:47 提交 → 16:13:50 出 6 条完整 210s 曲 = 183 秒**（batch 并行确实摊薄了固定开销，
平均 30s/条）。

**六条候选的客观判定**（关键教训在这里）：

| 候选 | 实际调性 | F-minor 相关 | sub% | mid% | 频段门 | 调性门 |
|---|---|---|---|---|---|---|
| **c0** | **F minor** | **0.843** | 38.4 | 17.5 | ✗ | ✓ |
| c1 | F major | 0.669 | 22.8 | 23.2 | ✗ | ✗ |
| c2 | C major | 0.283 | 5.9 | 41.9 | ✓ | ✗ |
| c3 | F major | 0.651 | 28.5 | 22.4 | ✗ | ✗ |
| c4 | C major | 0.344 | 9.0 | 45.7 | ✓ | ✗ |
| c5 | C minor | 0.364 | 20.8 | 20.2 | ✗ | ✗ |

**三条用血换来的结论**：

1. **调性门不能省，而且必须「主音 + 调式」都对**。只比相关度会把 **F major 当成 F minor**
   （同名主音，相关 0.65 轻松过 0.6 线）。更坑的是 F major 的关系小调是 A minor，
   只跑 minor profile 会输出「A minor」——两个坑叠在一起会让人以为模型跑调，其实是判据错。
2. **`auto_select` 绝不能绕过客观门**。第一版按 AES 分（CE+PQ）排序，直接选了 C major 的
   c1/c4（CE 7.5+，听感最好）——**好听但整首跑调，对源曲锚定的需求等于没用**。
   正确做法：只在「过门 或 调性对」的池子里按 AES 选。
3. **低频超标可以靠分轨后期救，调性错不行**。c0 是唯一对调但 sub 38.4% 的候选；
   分轨后 bass 电平 −2.8dB、piano −29.1dB，压 bass −16.3dB + 母带 55Hz 高通
   ⇒ **sub 38.4% → 4.1%、mid 17.5% → 51.7%，达标**；调性仍是 F minor。
   反过来 c2/c4 频段完美但调性错，后期无解（重调性 = 重生成）。

**最终成品**：`.tmp/songs/we-never-vibe/remixed_master.wav`
- 210.0s / 44.1k→48k / PCM 24bit
- 响度 **−14.1 LUFS**（目标 −14±1 ✓）、真峰 **−1.4 dBTP**（≤−1 ✓）、中段静音缺口 **0**
- 调性 **F minor**（目标匹配 ✓）
- 母带频段 sub **4.1%** / mid **51.7%**（达门 ✓）
- AES：CE **7.50**（vs 源曲 **−0.12**）、CU 7.75、PQ 7.52

**必须承认的缺陷**：
- **piano stem 只有 −29.1dB**，判定「生成侧几乎没给钢琴」——提示词里写的
  `Fender Rhodes as the main melodic voice` **没有被模型执行**。这与《音乐生成规则》§8
  的结论一致：模型不按提示词分配声部。若要钢琴，只能换候选或重生成，不能靠后期硬抬
  （+23dB 放大的是噪声，已实测）。
- 调性只有 1/6 命中，说明 `key_scale` 参数的约束力很弱；想稳拿对调，需要
  **多候选 + 调性门筛**（本流程已内置），或提高 batch 到 8。

---

**最终交付**：`.tmp/songs/<tag>/remixed_master.wav` — 已满足响度/峰值/静音缺口门，可直接交付。

