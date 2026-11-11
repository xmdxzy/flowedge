#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""成品验收：时长/响度/削波/静音缺口/调性/情绪弧。

判据都写死在这里，别凭「听起来还行」交付：
  · 响度 I 与目标差 ≤1 LU（loudnorm 两遍做不到就是链路顺序错了）
  · true peak ≤ -1.0 dBTP（有削波就是 alimiter 没生效）
  · 中段无 >0.6s 的静音缺口（交叉淡化没盖住 = 接缝断气）
  · 整曲最准的调 = 目标调（跨段漂调最刺耳，任何「差不多」都不算）
  · 情绪弧：分段 RMS 的 p90-p10 ≥ 5dB（起伏够不够，用户要的就是这个）

用法：python scripts/check-song.py <audio> [--target-key 6] [--lufs -14]
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys

import numpy as np
import soundfile as sf

NAMES = ["C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"]
MAJOR = np.array([6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88])
MINOR = np.array([6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17])


def ffprobe(path: str) -> dict:
    out = subprocess.run(["ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", path],
                         capture_output=True, text=True, check=True).stdout
    return json.loads(out)


def ebur128(path: str) -> dict:
    p = subprocess.run(["ffmpeg", "-hide_banner", "-nostats", "-i", path, "-af", "ebur128=peak=true",
                        "-f", "null", "-"], capture_output=True, text=True)
    txt = p.stderr
    summary = txt.split("Summary:")[-1] if "Summary:" in txt else txt
    i = re.search(r"I:\s+(-?[\d.]+)\s+LUFS", summary)
    tp = re.search(r"True peak:\s+Peak:\s+(-?[\d.]+)", txt)
    if not tp:
        tp = re.search(r"Peak:\s+(-?[\d.]+)\s+dBFS", txt)
    return {"lufs": float(i.group(1)) if i else None,
            "true_peak": float(tp.group(1)) if tp else None}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("audio")
    ap.add_argument("--target-key", type=int, default=6, help="0=C ... 6=F#")
    ap.add_argument("--lufs", type=float, default=-14.0)
    ap.add_argument("--min-arc-db", type=float, default=5.0)
    args = ap.parse_args()

    import librosa

    fails, warns, info = [], [], {}
    meta = ffprobe(args.audio)
    st = meta["streams"][0]
    dur = float(meta["format"].get("duration") or 0)
    info["stream"] = {k: st.get(k) for k in ("codec_name", "sample_rate", "channels", "duration")}

    y, sr = sf.read(args.audio, dtype="float32", always_2d=True)
    y = y.mean(axis=1)
    if len(y) / sr - dur > 0.05:
        warns.append(f"容器时长 {dur:.2f}s 与采样数 {len(y)/sr:.2f}s 不符")

    # 响度
    e = ebur128(args.audio)
    info["loudness"] = e
    if e["lufs"] is None:
        fails.append("响度测不出（ebur128 无 Summary）")
    elif abs(e["lufs"] - args.lufs) > 1.0:
        fails.append(f"响度 {e['lufs']:.1f} LUFS 偏离目标 {args.lufs} 超过 1LU")
    if e["true_peak"] is not None and e["true_peak"] > -1.0:
        fails.append(f"true peak {e['true_peak']:.1f} dBTP > -1.0，有削波")

    # 静音缺口（只看中段；结尾渐弱是设计）
    win = int(0.25 * sr)
    n = len(y) // win
    rms = np.array([np.sqrt(np.mean(y[i * win:(i + 1) * win] ** 2)) for i in range(n)])
    db = 20 * np.log10(np.maximum(rms, 1e-9))
    floor = db.max() - 42.0
    body = db[: int(n * 0.93)]
    gaps, run = [], 0
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

    # 调性
    import librosa as lb

    chroma = lb.feature.chroma_cqt(y=lb.effects.harmonic(y), sr=sr, hop_length=1024).mean(axis=1)
    chroma = chroma / (chroma.sum() or 1.0)
    fits = []
    for i in range(12):
        for prof, mode in ((MAJOR, "major"), (MINOR, "minor")):
            fits.append((float(np.corrcoef(np.roll(prof, i), chroma)[0, 1]), i, mode))
    fits.sort(reverse=True)
    info["key"] = {"best": f"{NAMES[fits[0][1]]} {fits[0][2]}", "corr": round(fits[0][0], 3),
                   "target_corr": round(float(np.corrcoef(np.roll(MINOR, args.target_key), chroma)[0, 1]), 3)}
    if fits[0][1] != args.target_key or fits[0][2] != "minor":
        fails.append(f"整曲调性 {info['key']['best']}，不是目标 {NAMES[args.target_key]} minor")

    # 情绪弧：5 秒一段的 RMS
    seg = 5.0
    k = int(seg * sr)
    sec = [float(20 * np.log10(max(np.sqrt(np.mean(y[i:i + k] ** 2)), 1e-9))) for i in range(0, len(y) - k + 1, k)]
    arr = np.array(sec)
    arc = float(np.percentile(arr, 90) - np.percentile(arr, 10))
    info["arc"] = {"per_5s_db": [round(v, 1) for v in sec], "span_db": round(arc, 1),
                   "peak_at_s": round(float(np.argmax(arr) * seg), 1),
                   "end_delta_db": round(float(arr[-3:].mean() - arr.max()), 1)}
    if arc < args.min_arc_db:
        warns.append(f"起伏跨度 {arc:.1f}dB < {args.min_arc_db}dB，可能太平")

    info["duration_s"] = round(len(y) / sr, 2)
    print(json.dumps(info, ensure_ascii=False, indent=2))
    for w in warns:
        print(f"WARN  {w}")
    for f in fails:
        print(f"FAIL  {f}")
    if not fails:
        print("PASS  全部硬判据通过")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
