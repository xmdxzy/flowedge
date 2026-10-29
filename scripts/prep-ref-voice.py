#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""参考音频预处理：去咔哒 + 归一化，供 Breeze 克隆使用。

为什么需要：
  录制窗口里"停止/关闭"那一下点击会打进 11~30 个满幅样本（实测 0.998）。
  这些样本在克隆前会变成参考包络的一部分，轻则刺一下，重则被当成音色特征。
  另外系统麦克风增益常常偏低（实测语音峰值只到 -16dBFS），不归一化的话
  参考音频整体偏小，克隆出来也是偏轻偏闷。

做法（顺序不能换）：
  1. 找 |x| > thresh 的连续段 → 用两侧各 pad 个样本的中位数插值替换（保字，不裁时段）
  2. 再整体增益到目标峰值
只处理"孤立尖峰"（长度 <= max_run），真正的爆破音会连续很长，不会被误插。

用法：
    python scripts/prep-ref-voice.py data/voice-input/ref-voice.wav
"""
from __future__ import annotations

import argparse
import sys
import wave
from pathlib import Path

import numpy as np


def read_wav(path: Path) -> tuple[np.ndarray, int]:
    with wave.open(str(path)) as w:
        if w.getsampwidth() != 2:
            raise SystemExit(f"只支持 16bit PCM，当前 {w.getsampwidth()*8}bit")
        sr = w.getframerate()
        n = w.getnframes()
        ch = w.getnchannels()
        raw = w.readframes(n)
    data = np.frombuffer(raw, dtype=np.int16)
    if ch > 1:
        data = data.reshape(-1, ch).mean(axis=1).astype(np.int16)
    return data.astype(np.float64) / 32768.0, sr


def write_wav(path: Path, data: np.ndarray, sr: int) -> None:
    pcm = np.clip(data, -1.0, 1.0)
    pcm = (pcm * 32767).astype(np.int16)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(pcm.tobytes())


def declick(x: np.ndarray, thresh: float, max_run: int, pad: int) -> tuple[np.ndarray, int]:
    y = x.copy()
    hits = np.abs(x) > thresh
    fixed = 0
    i = 0
    n = len(x)
    while i < n:
        if not hits[i]:
            i += 1
            continue
        j = i
        while j < n and hits[j]:
            j += 1
        run = j - i
        if run <= max_run:
            left = y[max(0, i - pad):i]
            right = y[j:min(n, j + pad)]
            repl = np.median(np.concatenate([left, right])) if left.size + right.size else 0.0
            y[i:j] = repl
            fixed += run
        i = j
    return y, fixed


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("wav")
    ap.add_argument("--out", default=None, help="默认 <stem>-clean.wav")
    ap.add_argument("--thresh", type=float, default=0.5, help="判定尖峰的幅度阈值")
    ap.add_argument("--max-run", type=int, default=64, help="最长当尖峰处理的连续样本数（~2.7ms@24k）")
    ap.add_argument("--pad", type=int, default=32)
    ap.add_argument("--peak-db", type=float, default=-3.0, help="归一化目标峰值")
    ap.add_argument("--trim-tail", type=float, default=0.0, help="尾巴裁掉多少秒（0=不裁）")
    args = ap.parse_args()

    src = Path(args.wav)
    x, sr = read_wav(src)

    def stats(a: np.ndarray) -> str:
        pk = 20 * np.log10(max(np.abs(a).max(), 1e-9))
        rms = 20 * np.log10(max(np.sqrt(np.mean(a**2)), 1e-9))
        return f"peak {pk:.1f}dBFS rms {rms:.1f}dBFS"

    before = stats(x)
    x, fixed = declick(x, args.thresh, args.max_run, args.pad)
    if args.trim_tail > 0:
        x = x[: len(x) - int(args.trim_tail * sr)]

    target = 10 ** (args.peak_db / 20.0)
    cur = float(np.abs(x).max())
    gain = target / cur if cur > 1e-9 else 1.0
    x = x * gain
    # 增益后再修一次（避免放大后重新触顶）
    if np.abs(x).max() > 1.0:
        x = np.clip(x, -1.0, 1.0)

    out = Path(args.out) if args.out else src.with_name(src.stem + "-clean.wav")
    write_wav(out, x, sr)

    print(f"源:   {src.name}  {len(read_wav(src)[0])/sr:.2f}s  {before}")
    print(f"处理: 修复尖峰 {fixed} 样本, 增益 x{gain:.2f} (+{20*np.log10(gain):.1f}dB)")
    print(f"产物: {out.name}  {len(x)/sr:.2f}s  {stats(x)}")
    if len(x) / sr < 3:
        print("警告：短于 3 秒，克隆会不稳", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
