#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""make_audio_kit.py — 配乐节拍测量 + 程序化 SFX 生成

为什么有这一步：爆款视听课纲要求「切点落拍」「音效按事件逐拍计分」，
而盒子上那两个工具（vq-beat / knowledge-video beats.py）本机没有。

产出（默认 .tmp/audio-kit/）：
  beats.json   BPM / 拍点时间 / 小节头 / 能量包络
  whoosh.wav   impact.wav   riser.wav   tick.wav   （44.1k 立体声，峰值 −6dBFS）
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np

try:
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

SR = 44100


def write_wav(path: Path, y: np.ndarray):
    import wave
    y = np.clip(y, -1.0, 1.0)
    pcm = (y * 32767).astype("<i2")
    if pcm.ndim == 1:
        pcm = np.stack([pcm, pcm], axis=1)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(2)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(pcm.tobytes())


def env(n: int, a: float, d: float, curve: float = 2.0) -> np.ndarray:
    ai, di = int(SR * a), int(SR * d)
    e = np.ones(n)
    ai = min(ai, n)
    di = min(di, n - ai)
    if ai:
        e[:ai] = np.linspace(0, 1, ai) ** (1 / curve)
    if di:
        e[n - di:] = np.linspace(1, 0, di) ** curve
    return e


def lp(x: np.ndarray, k: int) -> np.ndarray:
    """一阶低通（滑窗均值），用于把噪声压成"风"而不是"嘶"。"""
    if k <= 1:
        return x
    c = np.cumsum(np.insert(x, 0, 0.0))
    y = (c[k:] - c[:-k]) / k
    return np.concatenate([np.full(k - 1, y[0]), y])


def make_whoosh(dur=0.55, f0=1800, f1=260, amp=0.5):
    n = int(SR * dur)
    t = np.arange(n) / SR
    noise = np.random.default_rng(7).standard_normal(n)
    # 频带随时间的"掠过"用两段不同低通交叉模拟
    a = lp(noise, 3) * np.linspace(1.0, 0.2, n)
    b = lp(noise, 22) * np.linspace(0.1, 1.0, n)
    s = (a + b) * env(n, 0.02, dur * 0.8, 2.2)
    s *= amp
    return s


def make_impact(dur=0.42, amp=0.55):
    n = int(SR * dur)
    t = np.arange(n) / SR
    thump = np.sin(2 * np.pi * (90 * np.exp(-t * 7) + 46) * t) * np.exp(-t * 9)
    click = lp(np.random.default_rng(3).standard_normal(n), 2) * np.exp(-t * 45) * 0.35
    s = (thump * 0.9 + click) * amp
    return s * env(n, 0.004, dur * 0.6, 1.6)


def make_riser(dur=0.85, amp=0.32):
    n = int(SR * dur)
    t = np.arange(n) / SR
    f = 180 * np.exp(t * 2.2)
    ph = 2 * np.pi * np.cumsum(f) / SR
    s = 0.6 * np.sin(ph) + 0.25 * np.sin(2.01 * ph)
    s *= np.linspace(0.05, 1.0, n) ** 2
    s += lp(np.random.default_rng(11).standard_normal(n), 8) * 0.25 * np.linspace(0, 1, n)
    return s * amp


def make_tick(dur=0.07, amp=0.3):
    n = int(SR * dur)
    t = np.arange(n) / SR
    s = np.sin(2 * np.pi * 2100 * t) * np.exp(-t * 90)
    s += 0.4 * np.sin(2 * np.pi * 3200 * t) * np.exp(-t * 140)
    return s * amp


def main() -> int:
    ap = argparse.ArgumentParser(description="配乐节拍 + SFX 素材")
    ap.add_argument("--bgm", required=True)
    ap.add_argument("--out", default=".tmp/audio-kit")
    ap.add_argument("--max-seconds", type=float, default=200.0)
    args = ap.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    import librosa
    y, sr = librosa.load(args.bgm, sr=SR, mono=True, duration=args.max_seconds)
    tempo, beats = librosa.beat.beat_track(y=y, sr=sr, units="time")
    tempo = float(np.atleast_1d(tempo)[0])
    beats = np.asarray(beats, dtype=float)
    onset = librosa.onset.onset_strength(y=y, sr=sr)
    hop = 512
    rms = librosa.feature.rms(y=y, frame_length=2048, hop_length=hop)[0]
    times = librosa.frames_to_time(np.arange(len(rms)), sr=sr, hop_length=hop)

    # 每 4 拍作为小节头（backbeat 落点）
    bar = beats[::4] if len(beats) >= 4 else beats
    data = {
        "bgm": str(args.bgm),
        "sr": SR,
        "duration": round(float(len(y) / SR), 3),
        "bpm": round(tempo, 2),
        "beat_count": int(len(beats)),
        "beats": [round(float(b), 3) for b in beats[:400]],
        "bars": [round(float(b), 3) for b in bar[:200]],
        "rms": [[round(float(t), 2), round(float(v), 4)] for t, v in zip(times[::20], rms[::20])],
        "onset_max": round(float(onset.max()), 4),
    }
    (out / "beats.json").write_text(json.dumps(data, ensure_ascii=False, indent=1), encoding="utf-8")

    write_wav(out / "whoosh.wav", make_whoosh())
    write_wav(out / "impact.wav", make_impact())
    write_wav(out / "riser.wav", make_riser())
    write_wav(out / "tick.wav", make_tick())

    print(f"BPM {tempo:.1f} · 拍点 {len(beats)} · 时长 {data['duration']}s · 小节头 {len(bar)}")
    print("前 12 拍: " + " ".join(f"{b:.2f}" for b in beats[:12]))
    print("前 6 小节: " + " ".join(f"{b:.2f}" for b in bar[:6]))
    for n in ("whoosh", "impact", "riser", "tick"):
        p = out / f"{n}.wav"
        print(f"  {n}.wav {p.stat().st_size/1024:.0f}KB")
    return 0


if __name__ == "__main__":
    sys.exit(main())
