#!/usr/bin/env python3
"""BGM 客观体检：响度 / 峰值 / 静音缺口 / 速度稳定 / 调性稳定 / 频谱重心。

用法：
  python scripts/bgm-analyze.py track.wav [--json]

判据（配乐底噪用，不追求音乐制作级）：
  - 无 ≥0.45s 的可听静音（-38dB 以下）
  - 峰值 < -0.1dBFS（不削波）
  - 调性主音在整曲内不漂（chroma 相似度 > 0.75）
  - 速度窗口间偏差 < 6 BPM（否则像拼贴）

这不是「好不好听」的判据——听感只能人耳判断。它只拦「技术性废片」。
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys

import numpy as np

SR = 16000


def decode(path: str, sr: int = SR) -> np.ndarray:
    raw = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", path, "-f", "f32le", "-ac", "1", "-ar", str(sr), "-"],
        check=True, capture_output=True,
    ).stdout
    return np.frombuffer(raw, dtype="<f4").astype(np.float64)


def loudness(path: str) -> dict:
    out = subprocess.run(
        ["ffmpeg", "-hide_banner", "-nostats", "-i", path,
         "-af", "ebur128=peak=true", "-f", "null", "-"],
        check=False, capture_output=True, text=True,
    ).stderr
    res = {}
    for key in ("I", "LRA", "Peak"):
        m = re.findall(rf"^\s*{key}:\s*(-?[\d.]+)", out, re.M)
        if m:
            res[key] = float(m[-1])
    return res


def silence_gaps(x: np.ndarray, sr: int = SR, thresh_db: float = -38.0,
                 min_gap: float = 0.45) -> list[tuple[float, float]]:
    win = int(0.1 * sr)
    n = len(x) // win
    rms = np.sqrt(np.mean(x[: n * win].reshape(n, win) ** 2, axis=1) + 1e-12)
    db = 20 * np.log10(rms)
    quiet = db < thresh_db
    gaps, start = [], None
    for i, q in enumerate(quiet):
        if q and start is None:
            start = i
        elif not q and start is not None:
            if (i - start) * 0.1 >= min_gap:
                gaps.append((start * 0.1, i * 0.1))
            start = None
    if start is not None and (n - start) * 0.1 >= min_gap:
        gaps.append((start * 0.1, n * 0.1))
    return gaps


def onset_env(x: np.ndarray, sr: int = SR, hop: int = 256) -> np.ndarray:
    # 简单的谱通量：STFT 幅度的一阶差分正向和
    win = 1024
    frames = max(0, (len(x) - win) // hop)
    spec = np.empty((frames, win // 2 + 1), dtype=np.float32)
    window = np.hanning(win).astype(np.float32)
    for i in range(frames):
        seg = x[i * hop: i * hop + win].astype(np.float32) * window
        spec[i] = np.abs(np.fft.rfft(seg))
    diff = np.diff(spec, axis=0)
    flux = np.maximum(diff, 0).sum(axis=1)
    return flux


def tempo_estimate(flux: np.ndarray, fps: float) -> float:
    f = flux - flux.mean()
    ac = np.correlate(f, f, mode="full")[len(f) - 1:]
    ac[: int(fps * 0.25)] = 0  # 忽略 240BPM 以上
    lo, hi = int(fps * 60 / 200), int(fps * 60 / 50)
    seg = ac[lo:hi]
    if len(seg) == 0 or seg.max() <= 0:
        return 0.0
    lag = lo + int(np.argmax(seg))
    return 60.0 * fps / lag


def chroma(x: np.ndarray, sr: int = SR) -> np.ndarray:
    win = 4096
    hop = 2048
    frames = max(1, (len(x) - win) // hop)
    acc = np.zeros(12)
    freqs = np.fft.rfftfreq(win, 1 / sr)
    # 只取 55–2000Hz 的乐音区
    band = (freqs >= 55) & (freqs <= 2000)
    for i in range(frames):
        seg = x[i * hop: i * hop + win]
        if len(seg) < win:
            break
        mag = np.abs(np.fft.rfft(seg * np.hanning(win)))[band]
        f = freqs[band]
        pc = np.mod(np.round(12 * np.log2(f / 440.0) + 9), 12).astype(int)
        np.add.at(acc, pc, mag)
    return acc / (acc.sum() + 1e-12)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("path")
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--windows", type=int, default=6)
    a = ap.parse_args()

    x = decode(a.path)
    dur = len(x) / SR

    loud = loudness(a.path)
    gaps = silence_gaps(x)

    flux = onset_env(x)
    fps = SR / 256
    win_frames = len(flux) // a.windows
    bpms = []
    for w in range(a.windows):
        seg = flux[w * win_frames:(w + 1) * win_frames]
        if len(seg) > fps * 3:
            bpms.append(round(tempo_estimate(seg, fps), 1))

    # 调性稳定度：整曲 chroma 与各段 chroma 的余弦相似度
    whole = chroma(x)
    seg_chromas = []
    seg_len = len(x) // a.windows
    for w in range(a.windows):
        seg_chromas.append(chroma(x[w * seg_len:(w + 1) * seg_len]))
    sims = [float(np.dot(whole, c) / (np.linalg.norm(whole) * np.linalg.norm(c) + 1e-12))
            for c in seg_chromas]

    # 频谱重心（整体亮度）
    spec = np.abs(np.fft.rfft(x[: SR * 60] * np.hanning(min(len(x), SR * 60))))
    freqs = np.fft.rfftfreq(min(len(x), SR * 60), 1 / SR)
    centroid = float((spec * freqs).sum() / (spec.sum() + 1e-12))

    report = {
        "file": a.path,
        "duration_s": round(dur, 3),
        "integrated_lufs": loud.get("I"),
        "lra": loud.get("LRA"),
        "true_peak_db": loud.get("Peak"),
        "silence_gaps": [[round(s, 2), round(e, 2)] for s, e in gaps],
        "bpm_windows": bpms,
        "bpm_spread": round(max(bpms) - min(bpms), 1) if bpms else None,
        "chroma_similarity_by_window": [round(s, 3) for s in sims],
        "spectral_centroid_hz": round(centroid, 1),
        "verdict": {
            "silence_ok": len(gaps) == 0,
            "clip_ok": (loud.get("Peak") or 0) < -0.1,
            "key_stable": min(sims) > 0.75 if sims else None,
            "tempo_stable": (max(bpms) - min(bpms) < 6) if bpms else None,
        },
    }
    if a.json:
        print(json.dumps(report, ensure_ascii=False, indent=1))
    else:
        for k, v in report.items():
            print(f"{k:28s} {v}")


if __name__ == "__main__":
    main()
