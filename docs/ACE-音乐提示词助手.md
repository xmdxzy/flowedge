# ACE-Step 音乐创意生成助手（2026-11-06）

## 触发词
"帮我配个音乐"、"给这段视频配乐"、"ACE 提示词扩充"、"音乐创意引导"

---

## 任务流程

**当用户只给了简单描述时，自动执行：**

### Step 0 → 有源曲先做 DSP 分析（不可跳过）

用户给了参考曲（flac/mp3/wav）时，**先跑客观分析，不靠耳朵猜**：

```bash
python scripts/analyze-song.py "源曲.flac" --out .tmp/ref-<tag>.json
python -c "import json;d=json.load(open('.tmp/ref-<tag>.json',encoding='utf-8'));print(json.dumps({k:d[k] for k in ('tempo','key','timbre','texture')},ensure_ascii=False,indent=1))"
```

把 `bpm` / `key` / `band_low,mid,high` / `centroid_hz` / `percussive_ratio` / `arc_db` 作为生成参数**基线**。

**必须当面提出的冲突**（2026-11-06 实测教训）：
源曲若 `band_low ≥ 80%`、`band_mid ≤ 15%`（低频极厚、中频极薄），
照搬会**过不了质量门**（sub≤20% / mid≥22%），手机外放还会糊。
→ 必须让用户二选一：**"像源曲"（厚但过不了门）** 还是 **"过门"（压缩低频换清晰度）**。

### Step 1.5 → 第二轮细化选择题（源曲锚定时必做）

拿到源曲参数 + 第一轮选题后，**再问 4 道细化题**（每道都带实测数字做依据）：

| # | 问题 | 选项要点 |
|---|---|---|
| 1 | **低频策略**（核心冲突） | 平衡压 20–60Hz / 完全照搬源曲 / 干净优先 |
| 2 | **人声** | 纯器乐 / 无词哼唱 / 完整人声（写明返工风险） |
| 3 | **律动密度** | 保持克制（给源曲 percussive 值）/ 中等加鼓 / 明显律动 |
| 4 | **时长与结构** | 200–240s 完整段落 / 120–180s 精简 / 60–90s 抓耳 |

**提问纪律**：每题都引用实测值（如「源曲 percussive 0.145，非常克制」），
并说明代价（「会牺牲一点源曲的空灵感」）—— 不要只给选项不给后果。

### Step 1 → 五维度选择题引导
用**最多 5 道选择题**快速锁定核心需求（每问 3–4 个选项），绝不问开放式问题。

### Step 2 → 自动生成 200+ 字提示词
把用户的选择题答案 + 场景信息，按《音乐生成规则》的六段式骨架扩写成完整提示词：
1. **类型 + 调性 + 速度**（带具体 BPM）
2. **配器**（具体到乐器与频段）
3. **混音空间**（tape saturation / sidechain / wide stereo / reverb）
4. **情绪**（1–2 句氛围描述）
5. **禁止项**（No vocals 等）
6. **结构**（intro → verse → chorus → bridge → outro）

### Step 3 → 给出可执行命令
直接输出完整的 `song-pipeline.py` 命令行，包含所有参数。

---

## 五维度选择题模板（每次只问 1 道，不超过 5 道）

### Q1. 片种与用途？
A) 产品介绍片（功能讲解，节奏稳健）  
B) 品牌形象片（大气宏大，有史诗感）  
C) 社交媒体短视频（轻快活泼，抓耳前 3 秒）  
D) 纪录片旁白配乐（舒缓叙事，低频干净）  
E) 剧情短剧转场（情绪起伏大，冲突感强）  

### Q2. 目标曲风？
A) Cinematic/R&B 融合（电影感 + 律动，84–100BPM）  
B) Electronic/Chill（电子氛围，120–128BPM）  
C) Jazz/Neo-Soul（爵士律动，90–100BPM）  
D) Epic Orchestral（史诗管弦，60–80BPM）  
E) Acoustic/Folk（原声木吉他，70–90BPM）  

### Q3. 情感基调？
A) 温暖治愈（柔和明亮，小调中带希望）  
B) 紧张悬疑（压抑不安，低频主导）  
C) 轻快活力（跳跃感，打击乐突出）  
D) 深沉内省（慢速留白，钢琴为主）  
E) 激昂励志（逐步推进，副歌爆发）  

### Q4. 主要配器偏好？（可选填，默认 AI 自由发挥）
A) Rhodes 电钢琴 +  brushed drums（经典 R&B 感）  
B) 合成器 pad + 电子鼓机（现代感）  
C) 原声吉他 + 手碟（自然空灵）  
D) 弦乐四重奏 + 竖琴（古典优雅）  
E) 不选，AI 自由搭配最佳组合  

### Q5. 时长需求？
A) 短（60–90s，适合 TIKTOK/Reels）  
B) 中（120–180s，标准视频配乐）  
C) 长（200–240s，独立曲目级别）  

---

## 提示词生成算法（六段式骨架）

```python
def generate_prompt(choices: dict) -> str:
    # 从选择题答案推断参数
    bpm_map = {
        "A": 84, "B": 120, "C": 95, "D": 70, "E": 110
    }
    key_map = {
        "A": "F# minor", "B": "C# minor", "C": "G major", 
        "D": "Am", "E": "D major"
    }
    
    prompt_parts = []
    
    # ① 类型 + 调性 + 速度
    genre_desc = choices["genre"]  # "cinematic R&B fusion"
    bpm = bpm_map[choices["tempo"]]
    key = key_map[choices["key"]]
    prompt_parts.append(f"Instrumental {genre_desc} ballad in {key} at {bpm} BPM with a half-time feel.")
    
    # ② 配器（按片种推断）
    if choices["type"] == "product":
        instruments = "Fender Rhodes seventh chords, clear fingerstyle bass, airy strings, glassy vibraphone"
    elif choices["type"] == "brand":
        instruments = "wide string pads, timpani rolls, solo piano motif, layered synths"
    else:
        instruments = "soft brushed drums, intimate Rhodes, subtle bass, atmospheric pads"
    prompt_parts.append(f"{instruments}, tight mix with punchy kick and crisp hi-hats.")
    
    # ③ 混音空间
    space = "tape saturation, sidechain compression, wide stereo image, lush reverb on snares"
    prompt_parts.append(space)
    
    # ④ 情绪（1–2 句）
    mood = {
        "warm": "intimate and melancholy, like a quiet night drive through city lights",
        "tense": "suspended tension, unresolved chords creating forward momentum",
        "energetic": "uplifting groove with driving rhythm and melodic hooks"
    }
    prompt_parts.append(mood[choices["mood"]])
    
    # ⑤ 禁止项
    prompt_parts.append("No vocals, no copyright samples, professional master quality.")
    
    # ⑥ 结构（明确段落）
    structure = "Intro (sparse, build anticipation) → Verse (groove enters) → Chorus (lifted melody, wider) → Bridge (stripped, emotional peak) → Final Chorus (full band, triumphant) → Outro (fade on piano)"
    prompt_parts.append(structure)
    
    return " ".join(prompt_parts)  # 约 180–220 词
```

---

## 交付格式示例

```
[你的需求] "给我做个产品介绍片的 BGM，要轻快一点，长度大概 2 分钟"

→ [我问你] 
好的，我来帮你生成一个完整的配乐方案。先回答几个选择题：

Q1. 片种与用途？
   A) 产品介绍片（功能讲解，节奏稳健）
   B) 品牌形象片（大气宏大，有史诗感）
   C) 社交媒体短视频（轻快活泼，抓耳前 3 秒）
   D) 纪录片旁白配乐（舒缓叙事，低频干净）
   E) 剧情短剧转场（情绪起伏大，冲突感强）
   
   [请填写 A/B/C/D/E]

（等用户回复后再问下一题，最多 5 题）

→ [最终生成]
根据你的选择，我生成了完整提示词并准备好以下命令：

✅ 生成参数：
• 曲风：Electronic/Chill Fusion（120 BPM，C# Minor）
• 情绪：轻快活力，跳跃感，打击乐突出
• 配器：Synth Pad + Arp Bass + Box Drum Machine + Glitch FX
• 结构：Intro → Drop → Build → Drop → Outro
• 时长：150s
• 采样步数：32 steps（高质量档）

📝 生成的提示词：
"Instrumental electronic chill fusion track in C# minor at 120 BPM with a four-on-the-floor kick pattern, featuring wide synth pads for atmospheric bed, plucky arpeggiated bassline locked to the kick drum, crisp box drum machine patterns with glitchy effects and staccato hits, sidechain compression pumping the pads on every snare hit, wide stereo image from −90° left to +90° right, warm tape saturation coloring the master bus, soft reverb tails on synth leads. Upbeat and energetic vibe perfect for tech product demonstration, conveying innovation and forward motion without overwhelming clarity. No vocals, no copyright samples, broadcast-ready master quality throughout. Structure: Sparse intro with filtered arp rising in intensity → First drop with full groove and driving bassline → Half-time build removing drums but keeping arp energy → Second drop with added lead melody and wider pad texture → Quick outro cutting all elements except arp decaying into silence."

💻 一键生成命令：
```bash
python scripts/song-pipeline.py \
    --prompt "Instrumental electronic chill fusion track in C# minor at 120 BPM..." \
    --duration 150 --steps 32 --batch 6 --tag product_electronic --model acestep-v15-sft \
    --auto-select
```

需要我现在跑这条命令吗？[是/否]
```

---

## 技能使用纪律

1. **绝不问开放式问题** → 每个问题都是选择题（≤5 题）
2. **绝不省略参数** → 最终必须输出完整命令
3. **提示词必须 200+ 字** → 按六段式骨架写满
4. **给出可解释性** → 告诉用户为什么选这个 BPM/调性/配器

---

## 后续增强方向（P1/P2）

- 参考曲风格迁移（reference_audio_path 驱动节奏锚）
- 歌词生成（开启 CoT）
- LoRA 训练专属风格（上传 10 首样曲）

---

## 实战案例：源曲锚定（2026-11-06）

**需求**：以《秋成 - We Never(若如初见)Vibe氛围版.flac》风格为基调；用户首轮选 cdc。

**Step 0 实测源曲**（`analyze-song.py`）：

| 项 | 值 |
|---|---|
| BPM | 112.3 |
| 调性 | F minor（corr 0.639） |
| 时长 | 184.6s（8 段，最后 23s 衰到 −21dB） |
| 频段 | low 85.9% / mid 12.4% / high 1.6% |
| 音色 | centroid 266Hz（很暗）、harmonic 0.855 |
| 织体 | percussive 0.145、onset 3.97/s（打击极克制） |
| 动态 | p95−p10 = 7.1dB、arc −4.9dB |

**Step 1.5 用户细化选择**：平衡低频 / 纯器乐 / 中等律动加轻鼓 / 200–240s 完整结构。

**产出**：F minor @112BPM、210s、32 步、batch 6、`--skip-eval` 可选，
命令模板见 `docs/ACE-Step 音乐生成最高质量流水线.md`。

---

**文件路径**：`skills/ace-music-gen-prompt.md`  
**触发词**："帮我配个音乐"、"给这段视频配乐"、"ACE 提示词扩充"、"按这首歌的风格"  
**依赖**：`librosa` `soundfile` `demucs` `audiobox_aesthetics` + `.tmp/aes/checkpoint.pt`（已在本机验证齐备）

