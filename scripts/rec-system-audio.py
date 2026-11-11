#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""系统声音录音（WASAPI loopback）：抓电脑正在播放的声音，不依赖「立体声混音」。

为什么不用 ffmpeg dshow：这台机器没有启用「立体声混音」设备，
dshow 只能看到麦克风；而麦克风当前零信号。
soundcard 库走 WASAPI 环回，直接抓输出设备，链路实测可用（自播 440Hz → −16.5 dBFS）。

用法：
    python scripts/rec-system-audio.py                       # 默认设备，48k 立体声
    python scripts/rec-system-audio.py --out .tmp/rec --segment 30
    python scripts/rec-system-audio.py --device "27G5S-PRO+ (NVIDIA High Definition Audio)"

输出：<out>/part_000.wav, part_001.wav ...（每 segment 秒一段，中断只丢最后一段）
"""
from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path

import numpy as np
import soundcard as sc
import soundfile as sf


def main() -> int:
    ap = argparse.ArgumentParser(description="系统声音录音（WASAPI loopback）")
    ap.add_argument("--out", default=".tmp/rec-system")
    ap.add_argument("--segment", type=float, default=30.0, help="每段秒数")
    ap.add_argument("--samplerate", type=int, default=48000)
    ap.add_argument("--channels", type=int, default=2)
    ap.add_argument("--device", default="", help="扬声器名（默认用系统默认输出）")
    ap.add_argument("--max-seconds", type=float, default=7200, help="总时长上限")
    a = ap.parse_args()

    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)

    spk = sc.default_speaker() if not a.device else next(
        (s for s in sc.all_speakers() if a.device in s.name), None)
    if spk is None:
        print(f"[!] 找不到扬声器：{a.device}", flush=True)
        for s in sc.all_speakers():
            print("    可用:", s.name, flush=True)
        return 1

    print(f"[rec] loopback of: {spk.name}", flush=True)
    print(f"[rec] {a.samplerate}Hz x{a.channels}ch, 每段 {a.segment}s → {out}", flush=True)

    mic = sc.get_microphone(spk.name, include_loopback=True)
    frames = int(a.samplerate * a.segment)
    idx = 0
    t0 = time.time()
    total = 0.0

    with mic.recorder(samplerate=a.samplerate, channels=a.channels) as rec:
        while total < a.max_seconds:
            data = rec.record(numframes=frames)
            if data is None or len(data) == 0:
                break
            dur = len(data) / a.samplerate
            pk = float(np.abs(data).max())
            rms = float(np.sqrt((data ** 2).mean()))
            dst = out / f"part_{idx:03d}.wav"
            sf.write(str(dst), data, a.samplerate, subtype="PCM_24")
            print(f"[{time.time()-t0:7.1f}s] {dst.name}  {dur:5.1f}s  "
                  f"peak={20*np.log10(max(pk,1e-9)):6.1f}dB  rms={20*np.log10(max(rms,1e-9)):6.1f}dB",
                  flush=True)
            idx += 1
            total += dur

    print(f"[rec] done: {idx} segments, {total:.1f}s total", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
