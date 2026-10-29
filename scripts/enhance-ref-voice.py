#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""参考音频增强（给 Breeze 克隆用）：高通 → 谱减降噪 → 轻压缩 → 峰值归一化。

为什么这么排：
  1) 高通 80Hz：笔记本麦常见的低频轰鸣/桌面震动，克隆会把它学成"声音发闷"。
  2) 谱减：先用最静的那一帧估底噪谱，再逐帧减。这里**不做维纳滤波**——过度平滑
     会把齿音和气息抹掉，而气息恰恰是"像不像"的一部分。所以保留 15% 噪声底
     （over_subtraction=0.85），宁可留一点底噪也不要塑胶人声。
  3) 轻压缩（2:1, 慢起音）：把"偶尔声音小"的段落抬起来，让克隆学到的发声强度更稳定。
  4) 峰值归一化最后做，避免前面各步互相改变电平判据。

顺序换过会怎样：先归一化再降噪 → 噪声底被一起放大，谱减阈值估偏。

用法：
    python scripts/enhance-ref-voice.py data/voice-input/ref-12s.wav
"""
from __future__ import annotations

import argparse
import sys
import wave
from pathlib import Path

import numpy as np
from scipy.signal import butter, istft, sosfilt, stft


def read_wav(path: Path) -> tuple[np.ndarray, int]:
    with wave.open(str(path)) as w:
        if w.getsampwidth() != 2:
            raise SystemExit("只支持 16bit PCM")
        sr, n, ch = w.getframerate(), w.getnframes(), w.getnchannels()
        raw = w.readframes(n)
    d = np.frombuffer(raw, dtype=np.int16)
    if ch > 1:
        d = d.reshape(-1, ch).mean(axis=1).astype(np.int16)
    return d.astype(np.float64) / 32768.0, sr


def write_wav(path: Path, x: np.ndarray, sr: int) -> None:
    pcm = (np.clip(x, -1.0, 1.0) * 32767).astype(np.int16)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(pcm.tobytes())


def highpass(x: np.ndarray, sr: int, cutoff: float = 80.0, order: int = 4) -> np.ndarray:
    sos = butter(order, cutoff, btype="highpass", fs=sr, output="sos")
    return sosfilt(sos, x)


def spectral_subtract(x: np.ndarray, sr: int, over: float = 0.85, n_fft: int = 1024) -> np.ndarray:
    hop = n_fft // 4
    f, t, Z = stft(x, fs=sr, nperseg=n_fft, noverlap=n_fft - hop)
    mag, phase = np.abs(Z), np.angle(Z)
    # 用能量最低的 8% 帧估底噪谱（不是只看开头——开头可能是呼吸声）
    frame_e = mag.sum(axis=0)
    quiet = np.argsort(frame_e)[: max(1, int(len(frame_e) * 0.08))]
    noise = mag[:, quiet].mean(axis=1, keepdims=True)
    clean = np.maximum(mag - over * noise, 0.15 * noise)
    # 保留少量原始幅度，避免过度门控造成的"水声"
    clean = np.maximum(clean, mag * 0.12)
    _, rec = istft(clean * np.exp(1j * phase), fs=sr, nperseg=n_fft, noverlap=n_fft - hop)
    return np.asarray(rec.real[: len(x)])


def compress(x: np.ndarray, sr: int, ratio: float = 2.0, thresh_db: float = -28.0,
             attack_ms: float = 12.0, release_ms: float = 120.0) -> np.ndarray:
    """简易前馈压缩：跟帧包络，超过阈值按 ratio 压。"""
    frame = int(sr * 0.01)
    n = len(x) // frame
    if n < 4:
        return x
    env = np.array([np.sqrt(np.mean(x[i * frame:(i + 1) * frame] ** 2)) for i in range(n)])
    env_db = 20 * np.log10(np.maximum(env, 1e-9))
    thr = np.maximum(env_db, thresh_db)
    gain_db = (thr - env_db) * (1 - 1 / ratio)  # 超过阈值的部分被压
    gain_db = np.clip(gain_db, -12.0, 12.0)
    # 平滑增益曲线，防止抽吸
    a_att = np.exp(-1.0 / (attack_ms / 10.0))
    a_rel = np.exp(-1.0 / (release_ms / 10.0))
    sm = np.zeros_like(gain_db)
    prev = 0.0
    for i, g in enumerate(gain_db):
        a = a_att if g < prev else a_rel
        prev = a * prev + (1 - a) * g
        sm[i] = prev
    gain = 10 ** (np.repeat(sm, frame) / 20.0)
    out = x.copy()
    out[: len(gain)] *= gain
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("wav")
    ap.add_argument("--out", default=None)
    ap.add_argument("--hp", type=float, default=80.0)
    ap.add_argument("--over", type=float, default=0.85)
    ap.add_argument("--no-compress", action="store_true")
    ap.add_argument("--peak-db", type=float, default=-3.0)
    args = ap.parse_args()

    src = Path(args.wav)
    x, sr = read_wav(src)

    def st(a: np.ndarray) -> str:
        pk = 20 * np.log10(max(np.abs(a).max(), 1e-9))
        rms = 20 * np.log10(max(np.sqrt(np.mean(a**2)), 1e-9))
        # 用最低 10% 帧当底噪估计
        fr = int(sr * 0.02)
        n = len(a) // fr
        e = np.array([np.sqrt(np.mean(a[i * fr:(i + 1) * fr] ** 2)) for i in range(max(n, 1))])
        noise = 20 * np.log10(max(np.percentile(e, 10), 1e-9))
        return f"peak {pk:6.1f}dB rms {rms:6.1f}dB noisefloor {noise:6.1f}dB snr≈{rms-noise:4.1f}dB"

    print(f"源:     {src.name}  {st(x)}")
    x = highpass(x, sr, args.hp)
    print(f"高通后: {st(x)}")
    x = spectral_subtract(x, sr, args.over)
    print(f"降噪后: {st(x)}")
    if not args.no_compress:
        x = compress(x, sr)
        print(f"压缩后: {st(x)}")
    peak = 10 ** (args.peak_db / 20.0)
    x *= peak / max(np.abs(x).max(), 1e-9)
    print(f"归一化: {st(x)}")

    out = Path(args.out) if args.out else src.with_name(src.stem + "-enh.wav")
    write_wav(out, x, sr)
    print(f"产物:   {out.name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
