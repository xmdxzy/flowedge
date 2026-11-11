#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""把一段录音拆成「可写进 prompt 的风格参数」。

为什么不用现成的音频标签模型：这台机器上没有 CLAP/MERT 类权重，
而 ACE-Step 吃的是**标签式英文提示词 + bpm**，需要的正是
速度/调式/音色亮度/织体密度/动态曲线这几项——这些用 DSP 就能量出来，
而且比模型给的标签更可复现（同一段音频永远同一组数）。

产物：JSON，字段全部带「怎么算的」注释，方便回头质疑某个数。
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np

SR = 22050

# Krumhansl-Kessler 调性剖面（大调/小调）
MAJOR = np.array([6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88])
MINOR = np.array([6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17])
NAMES = ["C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"]


def detect_key(chroma_mean: np.ndarray) -> dict:
    best = None
    for i in range(12):
        for prof, mode in ((MAJOR, "major"), (MINOR, "minor")):
            corr = float(np.corrcoef(np.roll(prof, i), chroma_mean)[0, 1])
            if best is None or corr > best["corr"]:
                best = {"tonic": NAMES[i], "mode": mode, "corr": round(corr, 4)}
    # 小调倾向：同名大小调的相关差
    tonic_i = NAMES.index(best["tonic"])
    mj = float(np.corrcoef(np.roll(MAJOR, tonic_i), chroma_mean)[0, 1])
    mn = float(np.corrcoef(np.roll(MINOR, tonic_i), chroma_mean)[0, 1])
    best["minor_bias"] = round(mn - mj, 4)
    best["label"] = f"{best['tonic']} {best['mode']}"
    return best


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("audio")
    ap.add_argument("--out", default="")
    ap.add_argument("--start", type=float, default=0.0, help="只分析这一段（秒）")
    ap.add_argument("--end", type=float, default=0.0)
    ap.add_argument("--frame", type=float, default=0.5, help="动态包络帧长（秒）")
    args = ap.parse_args()

    import librosa

    y, sr = librosa.load(args.audio, sr=SR, mono=True, offset=args.start or 0.0,
                         duration=(args.end - args.start) if (args.end and args.start) else None)
    if y.size < sr:
        raise SystemExit(f"音频太短：{y.size} 采样")

    dur = y.size / sr
    y_h, y_p = librosa.effects.hpss(y)

    # ---- tempo：起步用 onset 自相关（快），再用 beat_track 修正
    onset_env = librosa.onset.onset_strength(y=y, sr=sr, hop_length=512)
    tempo, beats = librosa.beat.beat_track(onset_envelope=onset_env, sr=sr, hop_length=512)
    tempo = float(np.atleast_1d(tempo)[0])
    # 半速/双速折叠到 60–180 的常用区（写 prompt 时人对 bpm 的直觉在这区间）
    t_fold = tempo
    while t_fold > 180:
        t_fold /= 2
    while t_fold < 60:
        t_fold *= 2

    # ---- 调式 / 和声
    chroma = librosa.feature.chroma_cqt(y=y_h, sr=sr, hop_length=1024)
    chroma_mean = chroma.mean(axis=1)
    chroma_mean = chroma_mean / (chroma_mean.sum() or 1.0)
    key = detect_key(chroma_mean)

    # 每 2 秒的和声重心（看转调/和声走向）
    hop2 = int(2 * sr / 1024)
    chroma_blocks = []
    for i in range(0, chroma.shape[1], hop2):
        blk = chroma[:, i:i + hop2].mean(axis=1)
        if blk.sum() > 0:
            chroma_blocks.append(detect_key(blk / blk.sum())["label"])

    # ---- 动态：短时 RMS（dBFS），取有声帧
    frame = int(args.frame * sr)
    rms = librosa.feature.rms(y=y, frame_length=frame, hop_length=frame)[0]
    db = 20 * np.log10(np.maximum(rms, 1e-9))
    peak_db = float(20 * np.log10(max(np.max(np.abs(y)), 1e-9)))
    loud_mask = db > (np.percentile(db, 95) - 40)  # 相对高峰 40dB 以上算「有声」
    db_loud = db[loud_mask] if loud_mask.any() else db
    # 动态范围：有声帧 p95-p10（不用 max-min，避免单帧毛刺）
    dr = float(np.percentile(db_loud, 95) - np.percentile(db_loud, 10))

    # ---- 音色：谱质心 / 滚降 / 平坦度 / 低频占比
    S = np.abs(librosa.stft(y, n_fft=2048, hop_length=512)) ** 2
    freqs = librosa.fft_frequencies(sr=sr, n_fft=2048)
    mag_sum = S.sum(axis=1) + 1e-12
    centroid = float(np.sum(freqs * mag_sum) / mag_sum.sum())
    rolloff = float(np.percentile(freqs, 85))  # 粗估，下面用真值覆盖
    rolloff = float(librosa.feature.spectral_rolloff(S=np.sqrt(S), sr=sr)[0].mean())
    flatness = float(librosa.feature.spectral_flatness(S=np.sqrt(S))[0].mean())
    band_low = float(S[(freqs >= 20) & (freqs < 250)].sum() / S.sum())
    band_mid = float(S[(freqs >= 250) & (freqs < 4000)].sum() / S.sum())
    band_high = float(S[freqs >= 4000].sum() / S.sum())

    # ---- 织体：打击成分占比（鼓/击键）+ 起音密度
    perc_ratio = float(np.sum(y_p ** 2) / max(np.sum(y ** 2), 1e-12))
    onset_rate = float(len(librosa.onset.onset_detect(onset_envelope=onset_env, sr=sr, hop_length=512)) / dur)

    # ---- 分段：把动态曲线压成 8 段，看起伏形状
    nseg = 8
    seg_len = len(db) / nseg
    sections = []
    for i in range(nseg):
        sl = db[int(i * seg_len):int((i + 1) * seg_len)]
        if sl.size:
            sections.append({"t0": round(i * dur / nseg, 1), "t1": round((i + 1) * dur / nseg, 1),
                             "db": round(float(sl.mean()), 1)})

    # 「情绪弧」形状：算动态曲线与时间的相关（上升=推动，下降=收）
    x = np.arange(len(db))
    trend = float(np.polyfit(x, db, 1)[0] * len(db)) if len(db) > 2 else 0.0

    out = {
        "file": str(args.audio),
        "duration": round(dur, 2),
        "tempo": {"bpm_raw": round(tempo, 1), "bpm": round(t_fold, 1), "beats": int(len(beats))},
        "key": key,
        "key_blocks_2s": chroma_blocks[:40],
        "dynamics": {
            "peak_dbfs": round(peak_db, 1),
            "loud_mean_db": round(float(db_loud.mean()), 1),
            "loud_p95_p10_db": round(dr, 1),
            "sections": sections,
            "arc_db_per_track": round(trend, 1),
        },
        "timbre": {
            "centroid_hz": round(centroid, 0),
            "rolloff_hz": round(rolloff, 0),
            "flatness": round(flatness, 4),
            "band_low": round(band_low, 3),
            "band_mid": round(band_mid, 3),
            "band_high": round(band_high, 3),
        },
        "texture": {"percussive_ratio": round(perc_ratio, 3), "onset_per_sec": round(onset_rate, 2),
                    "harmonic_ratio": round(1 - perc_ratio, 3)},
    }
    text = json.dumps(out, ensure_ascii=False, indent=2)
    print(text)
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
