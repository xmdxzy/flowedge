#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""ACE-Step 最高质量音乐生成流水线（2026-11-06 定稿）。

依据：docs/音乐生成规则.md §0–§5
- 时长是结果，不是约束；结构需要多长就多长
- 一次长生成，不拼接（§6）
- 纯音乐 24 步起步；旋律细节 32–48 步
- 提示词六段骨架（类型→配器→混音空间→情绪→禁止项→结构）
- 混音平衡靠分轨重混，不能靠提示词（实测：提示词写 piano forward 没用）
- 选段必须过「频段闸门」：sub≤20%、mid≥22%

端到端流程：
1) /release_task 多候选生成（batch=N）
2) metrics.json 筛选合格候选（无静音缺口 + 频段达标）
3) audiobox-aesthetics 审美分（CE/PQ ≥ 参考曲 -0.5）
4) htdemucs_6s 拆 stem（piano, drums, bass, other, guitar, vocals）
5) 自动重混（remix-stems.py）：把钢琴抬到 −10 上下、鼓压到 −8 左右
6) 母带（两遍 loudnorm + alimiter）
7) check-song 成品验收（响度/峰值/静音缺口/调性/情绪弧）

用法：
    # 生成一首 200s R&B 纯音乐，一次出 6 条候选，用当前目录 .tmp/ref.wav 作审美参考
    python scripts/song-pipeline.py --prompt "Instrumental cinematic ambient R&B ballad in F# minor at 84 BPM" \
        --duration 200 --steps 32 --batch 6 --tag rnb_fshminor --model acestep-v15-sft [--ref-wav .tmp/ref.wav]

    # 只用最高审美分的一条（跳过手动挑）
    python scripts/song-pipeline.py --auto-select --prompt ... --duration 180 --batch 4

输出：
    .tmp/songs/<tag>/candidates/*.wav       # 全部候选（含指标）
    .tmp/songs/<tag>/selected.wav            # 最终交付（已重混+母带）
    .tmp/songs/<tag>/metrics.json            # 各候选指标表
    .tmp/songs/<tag>/audit.json              # 成品审计（check-song + 审美分）
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.parse
from pathlib import Path
from typing import Optional

import librosa
import numpy as np
import soundfile as sf


# ============ 服务地址（与引擎托管一致） ============
BASE_URL = "http://127.0.0.1:8104"


def post(path: str, payload: dict, timeout: float = 1800) -> dict:
    """调用 ACE API，返回 data 字段。失败抛 SystemExit。」"""
    req = urllib.request.Request(
        BASE_URL + path,
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=int(timeout)) as resp:
        body = resp.read().decode("utf-8-sig")
    data = json.loads(body)
    code = data.get("code", 0)
    if code not in (None, 0, 200):
        raise SystemExit(f"{path} 报错：{data}")
    return data.get("data", data)


def health() -> dict:
    with urllib.request.urlopen(BASE_URL + "/health", timeout=10) as r:
        return json.loads(r.read().decode("utf-8"))


# ============ 生成入口（复用 ace-gen 协议） ============


def submit_generation(prompt: str, duration: float, steps: int, batch_size: int,
                      model: str, bpm: int, key_scale: str, seed: int = None,
                      lyrics: str = "", thinking: bool = False) -> list[str]:
    """提交生成任务并等待完成，返回 wav 文件相对路径列表。」"""
    if not lyrics.strip():
        # 纯音乐关闭 CoT（否则卡在 Phase 1）
        use_cot_caption = False
        use_cot_language = False
    else:
        # 有歌词时开启 LM，让 CoT 改写 caption/猜语言
        use_cot_caption = thinking or True
        use_cot_language = True

    payload = {
        "prompt": prompt,
        "lyrics": lyrics,
        "audio_format": "wav",
        "audio_duration": duration,
        "inference_steps": steps,
        "model": model,
        "batch_size": batch_size,
        "thinking": thinking,
        "use_cot_caption": use_cot_caption,
        "use_cot_language": use_cot_language,
    }
    if bpm:
        payload["bpm"] = bpm
    if key_scale:
        payload["key_scale"] = key_scale
    if seed is not None and seed >= 0:
        payload["use_random_seed"] = False
        payload["seed"] = seed
    elif seed == -1:
        payload["use_random_seed"] = True

    t0 = time.time()
    print(f"[submit] duration={duration}s steps={steps} batch={batch_size} seed={seed}", flush=True)
    sub = post("/release_task", payload)
    task_id = sub.get("task_id") if isinstance(sub.get("data"), dict) else sub.get("task_id", sub.get("data"))
    if not task_id:
        raise SystemExit(f"未拿到 task_id: {sub}")

    last = ""
    while True:
        elapsed = time.time() - t0
        if elapsed > 1500:  # 25 分钟
            raise SystemExit("等待超时（25 分钟）")

        q = post("/query_result", {"task_id_list": [task_id]}, timeout=120)
        items = q if isinstance(q, list) else [q]
        it = next((x for x in items if x.get("task_id") == task_id), None)
        if it is None:
            time.sleep(2)
            continue

        st = it.get("status")
        if st == 1:
            files_raw = it.get("result", "[]")
            files = json.loads(files_raw) if isinstance(files_raw, str) else files_raw
            # 文件可能是完整 URL 或相对路径
            return [f["file"] if isinstance(f, dict) else f for f in files]

        if st == 2:
            result_str = it.get("result") or "未知错误"
            raise SystemExit(f"生成失败：{result_str}")

        prog = it.get("progress", "")
        msg = f"[{time.time()-t0:6.1f}s] status={st} progress={prog}"
        if msg != last:
            print(msg, flush=True)
            last = msg
        time.sleep(3)


def download(url_or_path: str, dest: Path, base: str = BASE_URL) -> None:
    """下载产物。

    ACE 的 result 可能是三种形态，早期实现把它们混为一谈，导致
    `/v1/audio?path=...` 被二次拼接+二次编码 → 403：
      ① 完整 http(s) URL
      ② 已带 query 的 `/v1/audio?path=...`
      ③ 裸文件路径（需要自己拼 /v1/audio?path= 并 quote）
    """
    u = url_or_path
    if u.startswith("http"):
        url = u
    elif u.startswith("/v1/audio"):
        url = base + u
    elif u.startswith("/"):
        url = base + u
    else:
        url = base + "/v1/audio?path=" + urllib.parse.quote(u, safe="")
    dest.parent.mkdir(parents=True, exist_ok=True)
    print(f"[download] {u[:80]} → {dest.name}", flush=True)
    with urllib.request.urlopen(url, timeout=300) as r, open(dest, "wb") as f:
        while True:
            chunk = r.read(1 << 20)
            if not chunk:
                break
            f.write(chunk)


# ============ 客观指标计算（复现 gen_multi.metrics） ============


def bands_of(mono: np.ndarray, sr: int) -> dict:
    """20–60 / 60–250 / 250–2k / 2k+ 的能量占比（%）。"""
    S = np.abs(librosa.stft(mono, n_fft=4096)) ** 2
    fr = librosa.fft_frequencies(sr=sr, n_fft=4096)
    tot = S.sum() + 1e-12
    out = []
    for lo, hi in ((20, 60), (60, 250), (250, 2000), (2000, 16000)):
        out.append(float(S[(fr >= lo) & (fr < hi)].sum() / tot * 100))
    return {"sub": round(out[0], 1), "low": round(out[1], 1), "mid": round(out[2], 1), "high": round(out[3], 1)}


KEY_NAMES = ["C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"]
_MAJOR = np.array([6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88])
_MINOR = np.array([6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17])


def _chroma_of(mono: np.ndarray, sr: int) -> np.ndarray:
    m = librosa.resample(mono, orig_sr=sr, target_sr=22050) if sr != 22050 else mono
    return librosa.feature.chroma_cqt(y=librosa.effects.harmonic(m), sr=22050,
                                      hop_length=1024).mean(axis=1)


def key_analysis(mono: np.ndarray, sr: int, target_tonic: str = "F",
                 target_mode: str = "minor") -> dict:
    """调性分析。

    为什么必须进选优：2026-11-06 实测 6 条候选里，AES 分最高的 c4 其实是 C major
    （与源曲 F minor 相关仅 0.326），而唯一对调的 c0 相关 0.825。
    只按频段门 + AES 挑会选出「好听但跑调」的那条。
    """
    ch = _chroma_of(mono, sr)
    fits = sorted(((float(np.corrcoef(np.roll(pr, k), ch)[0, 1]), KEY_NAMES[k], md)
                   for k in range(12) for pr, md in ((_MAJOR, "major"), (_MINOR, "minor"))),
                  reverse=True)
    corr, tonic, mode = fits[0]
    pr = _MAJOR if target_mode.startswith("maj") else _MINOR
    try:
        tgt_corr = float(np.corrcoef(np.roll(pr, KEY_NAMES.index(target_tonic)), ch)[0, 1])
    except ValueError:
        tgt_corr = 0.0
    # 必须「主音 + 调式」都对：F major 与 F minor 同名主音，只比相关度会把大调当小调收下
    want_mode = "major" if target_mode.startswith("maj") else "minor"
    on_target = (tonic == target_tonic and mode == want_mode)
    return {"best": f"{tonic} {mode}", "corr": round(corr, 3),
            "target_corr": round(tgt_corr, 3), "on_target": on_target}


def metrics_for_file(path: Path, sr_target: int = 48000,
                     target_tonic: str = "F", target_mode: str = "minor") -> dict:
    y, sr = sf.read(str(path), dtype="float32", always_2d=True)
    mono = y.mean(axis=1)
    dur = len(mono) / sr
    rms = float(np.sqrt(np.mean(mono ** 2)))
    peak = float(np.max(np.abs(mono)))

    # 中段静音缺口（只看中段；结尾渐弱是设计）
    win = int(0.25 * sr)
    n = len(mono) // win
    db_arr = np.array([20 * np.log10(max(np.sqrt(np.mean(mono[i*win:(i+1)*win]**2)), 1e-9)) for i in range(n)])
    floor = db_arr.max() - 42.0
    body = db_arr[:int(n * 0.93)]
    gaps_run = 0
    gap_list = []
    for i, v in enumerate(body):
        if v < floor:
            gaps_run += 1
        else:
            if gaps_run * 0.25 >= 0.6:
                gap_list.append((round((i - gaps_run) * 0.25, 2), round(gaps_run * 0.25, 2)))
            gaps_run = 0
    gaps = gap_list

    # 起音密度（用于「剥离段」探测）
    env = librosa.onset.onset_strength(y=mono, sr=sr, hop_length=512)
    onsets_pos = librosa.onset.onset_detect(onset_envelope=env, sr=sr, hop_length=512) * 512 / sr
    strip = None
    span = 8.0
    t = 0.25 * len(mono) / sr
    end_t = 0.85 * len(mono) / sr
    while t + span < end_t:
        dens = float(np.sum((onsets_pos >= t) & (onsets_pos < t + span)) / span)
        rms_seg = float(20 * np.log10(max(np.sqrt(np.mean(mono[int(t*sr):int((t+span)*sr)] ** 2)), 1e-9)))
        if abs(rms_seg - float(np.median(db_arr))) < 6.0:
            if strip is None or dens < strip[1]:
                strip = (round(t, 1), round(dens, 2))
        t += 2.0

    return {
        "dur": round(dur, 2),
        "rms_db": round(20 * np.log10(max(rms, 1e-9)), 1),
        "peak": round(20 * np.log10(max(peak, 1e-9)), 1),
        "bands": bands_of(mono, sr),
        "gaps": gaps,
        "strip": strip,
        "onset_density": round(len(onsets_pos) / dur, 2),
        "arc_span": round(float(np.percentile(db_arr, 90) - np.percentile(db_arr, 10)), 1),
        "key": key_analysis(mono, sr, target_tonic, target_mode),
    }


def ffmpeg_ebur128(path: Path) -> tuple[float | None, float | None]:
    """ebur128 测响度，返回 (LUFS, true_peak)。"""
    r = subprocess.run(["ffmpeg", "-hide_banner", "-nostats", "-i", str(path),
                        "-af", "ebur128=peak=true", "-f", "null", "-"],
                       capture_output=True, text=True)
    txt = r.stderr
    import re
    i = re.search(r"I:\s+(-?[\d.]+)\s+LUFS", txt.split("Summary:")[-1] if "Summary:" in txt else txt)
    tp = re.search(r"Peak:\s+(-?[\d.]+)", txt.split("Summary:")[-1] if "Summary:" in txt else txt)
    return float(i.group(1)) if i else None, float(tp.group(1)) if tp else None


# ============ 审美打分（audiobox-aesthetics） ============


def _segment_for_eval(p: Path, max_seconds: float) -> Path:
    """长音频截中间片段再评。

    为什么必须截：AES 是滑窗模型，CPU 上评 6 条 210s 实测超过 8 分钟还没完
    （GPU 被 ACE 占满时自动回落 CPU）。取中间 60s 代表段，结果与听感一致。
    """
    try:
        dur = float(sf.info(str(p)).duration)
    except Exception:
        return p
    if dur <= max_seconds:
        return p
    import tempfile
    work = Path(tempfile.gettempdir()) / "aes_seg"
    work.mkdir(parents=True, exist_ok=True)
    seg = work / f"{p.stem}_{int(max_seconds)}s.wav"
    if not seg.exists():
        start = max(0.0, (dur - max_seconds) / 2.0)
        subprocess.run(["ffmpeg", "-y", "-v", "error", "-ss", f"{start:.1f}",
                        "-t", f"{max_seconds:.1f}", "-i", str(p),
                        "-ar", "44100", "-ac", "1", str(seg)], check=True)
    return seg


def song_eval(files: list[Path], ref_wav: Optional[Path] = None,
              max_seconds: float = 60.0) -> dict:
    """Meta audiobox-aesthetics 四轴评分（CE/CU/PC/PQ）。"""
    ckpt = os.environ.get("AES_CKPT", ".tmp/aes/checkpoint.pt")
    if not Path(ckpt).exists():
        raise SystemExit(f"缺权重：{ckpt}（从 https://dl.fbaipublicfiles.com/audiobox-aesthetics/checkpoint.pt 下载）")

    # 检查依赖
    try:
        import torch
        from audiobox_aesthetics.infer import initialize_predictor
    except ImportError as e:
        raise SystemExit(f"缺少 audiobox_aesthetics：pip install audiobox-aesthetics") from e

    pred = initialize_predictor(ckpt)
    device = "cuda" if torch.cuda.is_available() else "cpu"
    pred.device = torch.device(device)
    print(f"[eval] device={device} max_seconds={max_seconds}", flush=True)

    eval_files = [_segment_for_eval(p, max_seconds) for p in files]
    ref_seg = _segment_for_eval(ref_wav, max_seconds) if ref_wav else None

    paths = [str(p) for p in eval_files] + ([str(ref_seg)] if ref_seg else [])
    rows = pred.forward([{"path": p} for p in paths])

    AXES = ["CE", "CU", "PC", "PQ"]
    out = {}
    refs = None
    if ref_seg:
        refs = {k: rows[len(eval_files)][k] for k in AXES}

    for orig, r in zip(files, rows[:len(eval_files)]):
        # key 用完整文件名（含扩展名），与调用方 Path(...).name 对齐
        out[orig.name] = {k: round(float(r[k]), 2) for k in AXES}
        if refs:
            out[orig.name]["Δvs_ref"] = {k: round(float(r[k] - refs[k]), 2) for k in AXES}
    return out


# ============ Demucs 分轨（htdemucs_6s） ============


def demucs_split(path: Path, out_dir: Path, stems_prefix: str = "ace") -> Path:
    """htdemucs_6s 拆六轨。GPU 不够时自动降级 CPU（ACE 驻留时会吃掉大半显存）。"""
    last_err = ""
    for device in ("cuda", "cpu"):
        cmd = [
            "demucs", "--device", device,
            "-n", "htdemucs_6s", "-o", str(out_dir), "--segment", "4", "--jobs", "2",
            str(path)
        ]
        print(f"[demucs] device={device} …", flush=True)
        r = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace")
        if r.returncode == 0:
            # demucs 实际产物在 <out>/htdemucs_6s/<trackname>/ 下
            actual = out_dir / "htdemucs_6s" / path.stem
            if not actual.is_dir():
                found = list(out_dir.rglob("drums.wav"))
                if not found:
                    raise RuntimeError(f"demucs 产物缺失：{out_dir}")
                actual = found[0].parent
            return actual
        last_err = r.stderr[-500:]
        print(f"[demucs] {device} 失败，降级下一个：{last_err[-200:]}", flush=True)
    raise RuntimeError(f"demucs cuda/cpu 都失败：{last_err}")


# ============ 自动重混（基于 remix-stems.py） ============


def remix_stems(stem_pattern: str, out: Path, target_piano_db: float = -10.0,
                target_drums_db: float = -8.0, bass_trim_db: float = 0.0,
                highpass_hz: float = 0.0) -> dict:
    """把钢琴抬到 target_piano_db 左右，鼓压到 target_drums_db 左右，再母带。

    bass_trim_db / highpass_hz 用于「调性对但低频糊」的候选：
    2026-11-06 实测 c0 是唯一 F minor（相关 0.825），但 sub 占比 38.4%，
    分轨里 bass+drums 贡献了大部分 20–60Hz ⇒ 削 bass 电平 + 母带高通可救回。
    """
    # 先体检当前分轨电平
    import glob as gb
    stems = {}
    sr = None
    pattern_glob = stem_pattern.replace("*stem*", "*")
    for f in gb.glob(pattern_glob):
        name = Path(f).stem.rsplit("_", 1)[-1]
        y, sr = sf.read(f, dtype="float32", always_2d=True)
        stems[name] = np.asarray(y, dtype=np.float64).reshape(-1, y.shape[-1])

    if not stems:
        raise SystemExit(f"没找到 stem：{pattern_glob}")

    # 全曲总功率 = 各 stem 功率之和（拆平写，避免嵌套类型歧义）
    total_power = float(sum(float(np.mean(v ** 2)) for v in stems.values()))
    total = float(20.0 * np.log10(max(np.sqrt(total_power + 1e-12), 1e-9)))

    # 估算当前相对电平
    current_rel = {k: round(db(v) - total, 1) for k, v in stems.items()}
    print(json.dumps({"current_rel_db": current_rel}, ensure_ascii=False, indent=2), flush=True)

    # 目标差值：target - current → 调整增益（带上下限，防爆推）
    MAX_LIFT, MAX_CUT = 12.0, 20.0
    gains = {}
    for name, current_db in current_rel.items():
        tgt = {"piano": target_piano_db, "drums": target_drums_db}.get(name, -11.0)  # other/guitar/vocals 默认 -11
        diff = tgt - current_db
        if name == "piano" and current_db < -25.0:
            # 实测：c1 的 piano stem 只有 -33dB（生成侧就没给钢琴），硬抬 +23dB 放大的只是噪声
            print(f"[remix] piano 原始 {current_db}dB 过低 → 判定无钢琴，不抬升", flush=True)
            diff = 0.0
        gains[name] = round(max(-MAX_CUT, min(MAX_LIFT, diff)), 1)

    # 低频超标时的补救：削 bass 电平（候选本身 sub 太高、但调性对，只能后期救）
    if bass_trim_db:
        gains["bass"] = round(gains.get("bass", 0.0) + bass_trim_db, 1)
        print(f"[remix] bass 额外 {bass_trim_db:+.1f}dB（低频超标补救）", flush=True)

    # 求和
    mixed = None
    for name, y in stems.items():
        g = 10 ** (gains.get(name, 0.0) / 20.0)
        mixed = y * g if mixed is None else mixed + y * g
    peak = float(np.max(np.abs(mixed))) or 1.0
    if peak > 0.98:
        mixed = mixed * (0.98 / peak)

    out.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(out), np.clip(mixed, -1, 1), sr, subtype="PCM_24", format="WAV")

    # 两遍 linear loudnorm + alimiter（必要时先高通压掉过重的 20–60Hz）
    e, tp = ffmpeg_ebur128(out)
    ln = f"loudnorm=I=-14:TP=-1.5:LRA=11:measured_I={e:.1f}:measured_TP={tp:.1f}:linear=true" if e else "loudnorm=I=-14:TP=-1.5:LRA=11:linear=true"
    chain = []
    if highpass_hz:
        chain.append(f"highpass=f={highpass_hz}:poles=2")
        print(f"[remix] 母带链加 highpass {highpass_hz}Hz", flush=True)
    chain.append(ln)
    chain.append("alimiter=limit=0.89:level=false")
    final = out.with_name(out.stem + "_master.wav")
    subprocess.run(["ffmpeg", "-y", "-v", "error", "-i", str(out),
                    "-af", ",".join(chain), "-ar", "48000", "-c:a", "pcm_s24le",
                    str(final)], check=True)

    final_e, final_tp = ffmpeg_ebur128(final)
    print(json.dumps({
        "out": str(final),
        "gains": gains,
        # 报告「应用增益后」的相对电平（用原始 stem 电平会误导判断）
        "stem_rel_db": {k: round(db((v * (10 ** (gains.get(k, 0.0) / 20.0))).mean(axis=1))
                                 - db(mixed.mean(axis=1)), 1) for k, v in stems.items()},
        "loudness_lufs": round(final_e, 1),
        "true_peak_db": round(final_tp, 1),
    }, ensure_ascii=False, indent=2))
    return {"out": str(final), "gains": gains, "loudness_lufs": final_e, "true_peak": final_tp}


def db(x):
    return float(20 * np.log10(max(np.sqrt(np.mean(x ** 2)), 1e-9)))


# ============ 成品验收（基于 check-song.py 简化版） ============


def check_song(path: Path, target_key: int = 6, lufs_target: float = -14.0,
               target_mode: str = "minor") -> tuple[bool, dict]:
    """简单版本成品验收：响度/峰值/静音缺口/调性近似。"""
    import librosa

    y, sr = sf.read(str(path), dtype="float32", always_2d=True)
    mono = y.mean(axis=1)
    dur = len(mono) / sr

    meta_ff = subprocess.run(["ffprobe", "-v", "error", "-show_streams", "-of", "json", str(path)],
                             capture_output=True, text=True, check=True).stdout
    meta = json.loads(meta_ff)
    st = meta["streams"][0]

    lufs, tp = ffmpeg_ebur128(path)
    fails = []
    info = {"lufs": lufs, "tp": tp, "duration": round(dur, 2), "codec": st.get("codec_name")}

    if lufs is not None and abs(lufs - lufs_target) > 1.0:
        fails.append(f"响度 {lufs:.1f} LUFS 偏离 {lufs_target} 超过 1LU")
    if tp is not None and tp > -1.0:
        fails.append(f"true peak {tp:.1f} dBTP > -1.0")

    # 静音缺口（中段）
    win = int(0.25 * sr)
    n = len(mono) // win
    db_arr = np.array([20 * np.log10(max(np.sqrt(np.mean(mono[i*win:(i+1)*win]**2)), 1e-9)) for i in range(n)])
    floor = db_arr.max() - 42.0
    body = db_arr[:int(n * 0.93)]
    gaps = []
    run = 0
    for i, v in enumerate(body):
        if v < floor:
            run += 1
        else:
            if run * 0.25 >= 0.6:
                gaps.append((round((i - run) * 0.25, 2), round(run * 0.25, 2)))
            run = 0
    info["gaps"] = gaps
    if gaps:
        fails.append(f"中段静音缺口 {len(gaps)} 处：{gaps[:4]}")

    # 调性（大小调都要判，否则 F major 会被当成它的关系小调 A minor）
    chroma = librosa.feature.chroma_cqt(y=librosa.effects.harmonic(mono), sr=sr, hop_length=1024).mean(axis=1)
    NAMES = KEY_NAMES
    matches = sorted(((float(np.corrcoef(np.roll(pr, i), chroma)[0, 1]), i, md)
                      for i in range(12) for pr, md in ((_MAJOR, "major"), (_MINOR, "minor"))),
                     reverse=True)
    best_corr, best_i, best_mode = matches[0]
    info["best_key"] = f"{NAMES[best_i]} {best_mode}"
    info["target_key_name"] = f"{NAMES[target_key]} {target_mode}"
    info["target_match"] = (best_i == target_key and best_mode == target_mode)
    if not info["target_match"]:
        fails.append(f"整曲调性 {info['best_key']}，不是目标 {info['target_key_name']}")

    passed = len(fails) == 0
    print(json.dumps(info, ensure_ascii=False, indent=2))
    for f in fails:
        print(f"FAIL  {f}")
    print(f"{'PASS' if passed else 'FAIL'}  验收通过")
    return passed, info


# ============ 主控流程 ============


def main() -> int:
    ap = argparse.ArgumentParser(description="ACE-Step 最高质量音乐生成流水线")
    ap.add_argument("--prompt", default="", help="主提示词（六段式更佳）")
    ap.add_argument("--prompt-file", default="", help="从 UTF-8 文件读提示词（长提示词/中文建议用这个）")
    ap.add_argument("--duration", type=float, default=200, help="请求时长（秒）")
    ap.add_argument("--steps", type=int, default=24, help="采样步数（纯音乐≥24，旋律细节 32–48）")
    ap.add_argument("--batch", type=int, default=6, help=f"候选条数（24GB 建议 6–8）")
    ap.add_argument("--model", default="acestep-v15-sft", choices=["acestep-v15-xl-turbo", "acestep-v15-sft"])
    ap.add_argument("--bpm", type=int, default=None)
    ap.add_argument("--key", default="F# minor", help="目标调性")
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--tag", required=True, help="输出目录标记名")
    ap.add_argument("--ref-wav", default="", help="参考曲（用于审美差值对比）")
    ap.add_argument("--auto-select", action="store_true", help="自动选最高 CE/PQ 候选，人工干预跳过审美挑选")
    ap.add_argument("--skip-eval", action="store_true", help="跳过 audiobox-aesthetics 审美分（没装/没权重时用）")
    ap.add_argument("--resume-from", default="", help="断点复用：跳过生成，直接用已有 wav（文件或目录）")
    args = ap.parse_args()

    prompt = args.prompt.strip()
    if args.prompt_file:
        prompt = Path(args.prompt_file).read_text(encoding="utf-8").strip()
    if not prompt and not args.resume_from:
        raise SystemExit("需要 --prompt 或 --prompt-file（断点复用时可省）")

    # ============ 预检 ============
    if args.resume_from:
        print("[precheck] 断点复用模式：不依赖 ACE 服务（可先停它腾显存）", flush=True)
    else:
        print("[precheck] /health", flush=True)
        health()

    # ============ 创建输出目录 ============
    root = Path(".tmp/songs") / args.tag
    root.mkdir(parents=True, exist_ok=True)
    cand_dir = root / "candidates"
    cand_dir.mkdir(exist_ok=True)
    stem_dir = root / "stems"
    stem_dir.mkdir(exist_ok=True)

    # ============ 生成多候选（或断点复用） ============
    candidates = []
    if args.resume_from:
        src = Path(args.resume_from)
        files = sorted(src.glob("*.wav")) if src.is_dir() else [src]
        if not files:
            raise SystemExit(f"--resume-from 没找到 wav：{src}")
        for f in files:
            dest = cand_dir / f.name
            if not dest.exists():
                shutil.copy2(f, dest)
            candidates.append(dest)
        print(f"[resume] 复用已有候选 {len(candidates)} 条（跳过生成）", flush=True)
    else:
        print("[generation] 提交生成任务", flush=True)
        urls = submit_generation(
            prompt=prompt,
            duration=args.duration,
            steps=args.steps,
            batch_size=args.batch,
            model=args.model,
            bpm=args.bpm,
            key_scale=args.key,
            seed=args.seed
        )
        for i, u in enumerate(urls):
            # 不能用 Path(u).name —— ACE 返回 `/v1/audio?path=D%3A...`，
            # 取 .name 会得到 `audio?path=...` 这种非法文件名（Windows Errno 22）。
            dest = cand_dir / f"c{i}.wav"
            download(u, dest)
            candidates.append(dest)

    # ============ 客观指标筛选 ============
    print("[metrics] 客观指标评估", flush=True)
    all_metrics = []
    _kp = (args.key or "F# minor").split()
    tgt_tonic = _kp[0] if _kp else "F#"
    tgt_mode = _kp[1] if len(_kp) > 1 else "minor"
    print(f"[metrics] 目标调性 {tgt_tonic} {tgt_mode}", flush=True)
    for c in candidates:
        m = metrics_for_file(c, target_tonic=tgt_tonic, target_mode=tgt_mode)
        m["file"] = str(c)
        m["bands_ok"] = m["bands"]["sub"] <= 20 and m["bands"]["mid"] >= 22
        m["gaps_ok"] = len(m["gaps"]) == 0
        m["key_ok"] = m["key"]["on_target"]
        m["passed_gates"] = m["bands_ok"] and m["gaps_ok"] and m["key_ok"]
        all_metrics.append(m)
        print(f"  {Path(m['file']).name}: key={m['key']['best']}(目标相关{m['key']['target_corr']}) "
              f"sub={m['bands']['sub']}% mid={m['bands']['mid']}% "
              f"{'✓' if m['passed_gates'] else '✗'} "
              f"[band:{'ok' if m['bands_ok'] else 'no'} key:{'ok' if m['key_ok'] else 'no'}]", flush=True)

    metrics_file = root / "metrics.json"
    metrics_file.write_text(json.dumps(all_metrics, ensure_ascii=False, indent=2), encoding="utf-8")

    valid_pool = [m for m in all_metrics if m["passed_gates"]]
    # 全达标 > 调性对（低频可在重混阶段压） > 全部
    keyed_pool = [m for m in all_metrics if m["key_ok"] and m["gaps_ok"]]
    pool = valid_pool or keyed_pool or all_metrics
    if not valid_pool:
        print(f"[selection] 无全达标候选，退到「调性对」池（{len(keyed_pool)} 条，低频靠重混压）", flush=True)
    # 选择动态跨度最大的一条作为候选
    selected = max(pool, key=lambda x: x["arc_span"] if "arc_span" in x else 0)

    # ============ 审美分 ============
    aes = {}
    if args.skip_eval:
        print("[eval] 已跳过审美分（--skip-eval）", flush=True)
    else:
        try:
            # 只对通过频段门的候选打分：6 条全评在 CPU 上要十几分钟，
            # 而没过门的候选本来就不会被选中。
            eval_pool = [Path(m["file"]) for m in pool]
            print(f"[eval] 评 {len(eval_pool)}/{len(candidates)} 条（客观池）", flush=True)
            aes = song_eval(eval_pool, Path(args.ref_wav) if args.ref_wav else None)
            print(json.dumps(aes, ensure_ascii=False, indent=2), flush=True)
        except Exception as e:  # 含 CUDA OOM：ACE 驻留时显存可能不够
            print(f"[eval] 审美分不可用，降级为只按客观指标选：{type(e).__name__}: {e}", flush=True)
            aes = {}

    # 结合客观 + 审美，选最终候选
    if args.auto_select and aes:
        # 只在「客观池」（过门 或 调性对）里按 AES 选——绝不跨过客观门
        allowed = {Path(m["file"]).name for m in pool}
        scored = [f for f in candidates if f.name in aes and f.name in allowed]
        if scored:
            ranked = sorted(scored, key=lambda f: aes[f.name]["CE"] + aes[f.name]["PQ"], reverse=True)
            selected_file = ranked[0]
            print(f"[selection] AES 在客观池内选：{selected_file.name}", flush=True)
        else:
            selected_file = Path(selected["file"])
            print(f"[selection] 客观池与 AES 无交集，用客观指标：{selected_file.name}", flush=True)
    else:
        # 退回客观指标（动态跨度最大）
        print("[selection] 客观指标推荐：", flush=True)
        selected_file = Path(selected["file"])
        print(f"  {selected_file.name}", flush=True)
        print(f"    dur={selected['dur']}s sub={selected['bands']['sub']}% "
              f"mid={selected['bands']['mid']}% gaps={len(selected['gaps'])} "
              f"arc={selected.get('arc_span')}dB", flush=True)
        if aes:
            print(f"    审美：CE={aes[selected_file.name]['CE']} PQ={aes[selected_file.name]['PQ']}", flush=True)

    # ============ Demucs 分轨 ============
    print("[demucs] 拆六轨分音轨", flush=True)
    demucs_root = stem_dir / selected_file.stem
    cached_stems = demucs_root / "htdemucs_6s" / selected_file.stem
    if cached_stems.is_dir() and (cached_stems / "drums.wav").exists():
        actual_dir = cached_stems
        print(f"[demucs] 复用已有分轨：{actual_dir}", flush=True)
    else:
        actual_dir = demucs_split(selected_file, demucs_root)
    print(f"[demucs] 产物目录：{actual_dir}", flush=True)

    # ============ 自动重混 ============
    print("[remix] 自动重混", flush=True)
    remix_dest = root / "remixed.wav"
    pattern = str(actual_dir / "*.wav")
    # 低频超标自动补救：sub 每超 1% 削 1.2dB（最多 8dB）+ 55Hz 高通
    _sub = float(selected["bands"]["sub"])
    _bass_trim = round(-min(8.0, 1.2 * max(0.0, _sub - 20.0)), 1)
    _hp = 55.0 if _sub > 20.0 else 0.0
    if _hp:
        print(f"[remix] 低频超标 sub={_sub}% → 自动补救 bass {_bass_trim:+.1f}dB + highpass {_hp}Hz",
              flush=True)
    remix_info = remix_stems(pattern, remix_dest, bass_trim_db=_bass_trim, highpass_hz=_hp)

    # ============ 成品验收（审母带，不是未母带的混音） ============
    print("[audit] 成品审计", flush=True)
    final_audio = Path(remix_info.get("out") or str(remix_dest))
    print(f"[audit] 审计对象：{final_audio}", flush=True)
    KEY_NAMES = ["C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"]
    try:
        tk = KEY_NAMES.index((args.key or "").split()[0])
    except (ValueError, IndexError):
        tk = 6
    passed, audit_info = check_song(final_audio, target_key=tk, target_mode=tgt_mode)
    # 复测母带频段：验证「低频补救」是否真的把 sub 压回门内
    _ym, _srm = sf.read(str(final_audio), dtype="float32", always_2d=True)
    final_bands = bands_of(_ym.mean(axis=1), _srm)
    _ok = final_bands["sub"] <= 20 and final_bands["mid"] >= 22
    print(f"[audit] 母带频段 sub={final_bands['sub']}% mid={final_bands['mid']}% "
          f"{'✓ 达门' if _ok else '✗ 未达门'}", flush=True)
    audit_json = {
        "selected_candidate": str(selected_file.name),
        "audited_remix": str(final_audio.name),
        "target_key": (args.key or "").strip(),
        "candidate_key": selected.get("key", {}),
        "candidate_bands": selected.get("bands", {}),
        "final_bands": final_bands,
        "bands_gate_after_remix": _ok,
        "passed": passed,
        "metrics": audit_info,
        "ae_scores": aes.get(selected_file.name, {})
    }
    audit_file = root / "audit.json"
    audit_file.write_text(json.dumps(audit_json, ensure_ascii=False, indent=2), encoding="utf-8")
    return 0 if passed else 2


if __name__ == "__main__":
    sys.exit(main())
