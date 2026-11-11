#!/usr/bin/env python3
"""整曲验收：一段音乐到底是不是「能交付的曲子」。

光看时长会漏掉三类真实缺陷（本项目都踩过）：
  1. 中段真静音（一段尾巴全静音 → 接缝处 0.4–0.6s 空洞，时长完全正常）
  2. 接缝音量凹陷（没到静音门限，但听感是"掉了一下"）
  3. 削波 / 过冲（真峰值 > -1 dBFS）

用法：
  python scripts/verify-song.py <wav> [--ref <参考wav>] [--json out.json]
退出码：0 全过，1 有 WARN，2 有 FAIL。
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys

import librosa
import numpy as np


def ff(args: list[str]) -> str:
    p = subprocess.run(args, capture_output=True, text=True, encoding="utf-8", errors="replace")
    return (p.stdout or "") + (p.stderr or "")


def probe_duration(path: str) -> float:
    out = ff(["ffprobe", "-v", "error", "-show_entries", "format=duration",
              "-of", "default=nw=1:nk=1", path])
    return float(out.strip().splitlines()[-1])


def loudness(path: str) -> tuple[float, float, float]:
    out = ff(["ffmpeg", "-hide_banner", "-nostats", "-i", path,
              "-af", "ebur128=peak=true", "-f", "null", "-"])
    i = re.findall(r"I:\s*(-?\d+\.?\d*)\s*LUFS", out)
    pk = re.findall(r"Peak:\s*(-?\d+\.?\d*)\s*dBFS", out)
    lra = re.findall(r"LRA:\s*(-?\d+\.?\d*)\s*LU", out)
    return (float(i[-1]) if i else float("nan"),
            float(pk[-1]) if pk else float("nan"),
            float(lra[-1]) if lra else float("nan"))


def gaps(path: str, thr_db: int, min_d: float) -> list[tuple[float, float, float]]:
    out = ff(["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-i", path,
              "-af", f"silencedetect=n={thr_db}dB:d={min_d}", "-f", "null", "-"])
    starts = [float(m) for m in re.findall(r"silence_start:\s*(-?\d+\.?\d*)", out)]
    ends = [float(m) for m in re.findall(r"silence_end:\s*(-?\d+\.?\d*)", out)]
    durs = [float(m) for m in re.findall(r"silence_duration:\s*(-?\d+\.?\d*)", out)]
    res = []
    for i, s in enumerate(starts):
        e = ends[i] if i < len(ends) else float("nan")
        d = durs[i] if i < len(durs) else (e - s if e == e else float("nan"))
        res.append((round(s, 2), round(e, 2), round(d, 2)))
    return res


def dips(y: np.ndarray, sr: int, win: float = 0.2, span: float = 3.0, drop_db: float = 12.0):
    """局部凹陷：0.2s 窗的 rms 比 ±3s 中位数低 12dB 以上 ⇒ 接缝掉音。"""
    h = int(sr * win)
    n = len(y) // h
    if n < 8:
        return []
    r = np.sqrt((y[: n * h].reshape(n, h) ** 2).mean(axis=1)) + 1e-9
    db = 20 * np.log10(r)
    half = int(span / win)
    out = []
    for i in range(n):
        lo, hi = max(0, i - half), min(n, i + half + 1)
        med = float(np.median(db[lo:hi]))
        if db[i] < med - drop_db and med > -45:
            out.append((round(i * win, 2), round(float(db[i]), 1), round(med, 1)))
    # 只留每一簇的代表
    squeezed = []
    for t, d, m in out:
        if squeezed and t - squeezed[-1][0] < 1.0:
            continue
        squeezed.append((t, d, m))
    return squeezed


def bands(y: np.ndarray, sr: int) -> dict:
    stft = np.abs(librosa.stft(y, n_fft=2048, hop_length=512))
    f = librosa.fft_frequencies(sr=sr, n_fft=2048)
    tot = stft.sum() + 1e-9

    def b(lo, hi):
        m = (f >= lo) & (f < hi)
        return round(float(stft[m].sum() / tot), 4)

    return {"centroid_hz": round(float(np.mean(librosa.feature.spectral_centroid(y=y, sr=sr))), 1),
            "sub": b(20, 80), "bass": b(80, 250), "lowmid": b(250, 800),
            "mid": b(800, 2000), "himid": b(2000, 6000), "air": b(6000, 16000)}


def similarity_curve(y: np.ndarray, sr: int, win: float = 8.0) -> list[tuple[float, float]]:
    hop = 512
    chroma = librosa.feature.chroma_cqt(y=y, sr=sr, hop_length=hop)
    tps = sr / hop
    cache = {}

    def blk(t0, t1):
        key = (t0, t1)
        if key in cache:
            return cache[key]
        a, b = int(t0 * tps), int(t1 * tps)
        v = chroma[:, a:b].mean(axis=1) if b - a > 4 else None
        cache[key] = v
        return v

    ref = blk(0, win)
    ry, sx = [], []
    t = 0.0
    dur = len(y) / sr
    while t + win < dur:
        v = blk(t, t + win)
        if v is not None and ref is not None:
            sim = float(np.dot(ref, v) / (np.linalg.norm(ref) * np.linalg.norm(v) + 1e-9))
            a, b = int(t * sr), int((t + win) * sr)
            sx.append((round(t, 1), round(sim, 3)))
            ry.append(round(float(20 * np.log10(np.sqrt(np.mean(y[a:b] ** 2)) + 1e-9)), 1))
        t += win
    return sx, ry


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("wav")
    ap.add_argument("--ref", default="")
    ap.add_argument("--json", default="")
    a = ap.parse_args()

    rep: dict = {"file": a.wav}
    fails, warns = [], []

    dur = probe_duration(a.wav)
    rep["duration_s"] = round(dur, 2)
    lufs, peak, lra = loudness(a.wav)
    rep["lufs"], rep["true_peak_dbfs"], rep["lra"] = lufs, peak, lra
    if peak > -1.0:
        fails.append(f"true peak {peak} dBFS > -1（削波风险）")
    if not (dur > 100):
        warns.append(f"时长 {dur:.1f}s 偏短")

    g45 = gaps(a.wav, -45, 0.4)
    g35 = gaps(a.wav, -35, 0.25)
    rep["gaps_-45dB_0.4s"], rep["gaps_-35dB_0.25s"] = g45, g35
    if g45:
        fails.append(f"中段静音缺口 {len(g45)} 处: {g45[:4]}")
    elif g35:
        warns.append(f"-35dB/0.25s 静音 {len(g35)} 处: {g35[:4]}")

    y, sr = librosa.load(a.wav, sr=22050, mono=True)
    d = dips(y, sr)
    rep["dips"] = d
    if d:
        warns.append(f"接缝/局部凹陷 {len(d)} 处: {d[:4]}")

    bd = bands(y, sr)
    rep["bands"] = bd
    sim, rms = similarity_curve(y, sr)
    rep["sim_curve"], rep["rms_curve"] = sim, rms
    if sim:
        s = [v for _, v in sim]
        rep["sim_min"], rep["sim_max"] = min(s), max(s)
        # 整曲都跟开头一样 = 一段 loop 反复，不是结构
        if min(s) > 0.92:
            warns.append(f"全程与开头相似（min {min(s):.3f}）⇒ 疑似单一 loop，无段落变化")
    if rms:
        rep["rms_min"], rep["rms_max"] = min(rms), max(rms)
        if max(rms) - min(rms) < 2.0:
            warns.append(f"响度动态仅 {max(rms)-min(rms):.1f}dB ⇒ 没有强弱起伏")

    if a.ref:
        ry, rsr = librosa.load(a.ref, sr=22050, mono=True)
        rb = bands(ry, rsr)
        rep["ref_bands"] = rb
        rep["band_delta"] = {k: round(bd[k] - rb[k], 4) for k in bd if k in rb and k != "centroid_hz"}
        if abs(bd["air"] - rb["air"]) > 0.08:
            warns.append(f"air 占比 {bd['air']} vs 参考 {rb['air']}（差 >0.08，亮度不匹配）")

    rep["fails"], rep["warns"] = fails, warns
    print(json.dumps({k: v for k, v in rep.items() if k not in ("sim_curve", "rms_curve")},
                     ensure_ascii=False, indent=1))
    print("---")
    for f in fails:
        print("FAIL", f)
    for w in warns:
        print("WARN", w)
    if not fails and not warns:
        print("PASS 无 FAIL 无 WARN")

    if a.json:
        with open(a.json, "w", encoding="utf-8") as fh:
            json.dump(rep, fh, ensure_ascii=False, indent=1)
    return 2 if fails else (1 if warns else 0)


if __name__ == "__main__":
    sys.exit(main())
