#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""录系统回放（WASAPI loopback）——给「听一首歌，然后仿写一首」用。

为什么要自己写：
  本机没有「立体声混音」输入设备，ffmpeg 的 dshow 打不到系统声。
  soundcard 走 Windows Core Audio 的 loopback，不需要任何驱动/重启。

产物：
  <out>.f32     float32 交织原始采样（边录边 flush，进程被杀也只丢最后一小段）
  <out>.json    sr / channels / 实际秒数
  <out>.wav     正常结束时自动转（16bit）

用法：
  python scripts/record-loopback.py --seconds 300 --out .tmp/capture-song.f32
"""
from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path

import numpy as np
import soundcard as sc

SR = 48000
CH = 2
BLK = 0.1  # 100ms 一块，块内偏移可控，避免长录漂移


def pick_loopback() -> "sc._Microphone":
    spk = sc.default_speaker()
    name = spk.name
    try:
        return sc.get_microphone(id=str(name), include_loopback=True), name
    except Exception:
        pass
    for m in sc.all_microphones(include_loopback=True):
        if getattr(m, "isloopback", False):
            return m, name
    raise SystemExit("没有可用的 loopback 设备")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--seconds", type=float, default=300.0)
    ap.add_argument("--out", default=".tmp/capture-song.f32")
    args = ap.parse_args()

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    mic, spk_name = pick_loopback()
    frames_total = int(args.seconds * SR)

    print(f"[rec] speaker={spk_name} device={mic.name} sr={SR} ch={CH} sec={args.seconds}", flush=True)
    written = 0
    t0 = time.time()
    last_report = t0
    peak_all = 0.0
    with open(out, "wb") as f, mic.recorder(samplerate=SR, channels=CH) as rec:
        try:
            while written < frames_total:
                n = min(int(BLK * SR), frames_total - written)
                data = np.asarray(rec.record(numframes=n), dtype=np.float32)
                if data.ndim == 1:
                    data = data[:, None]
                f.write(data.tobytes())
                f.flush()
                written += data.shape[0]
                peak = float(np.max(np.abs(data))) if data.size else 0.0
                peak_all = max(peak_all, peak)
                now = time.time()
                if now - last_report >= 10.0:
                    last_report = now
                    el = now - t0
                    seg = np.asarray(data)
                    peak = float(np.max(np.abs(seg))) if seg.size else 0.0
                    db = 20 * np.log10(peak) if peak > 1e-9 else -120.0
                    print(f"[rec] t={el:6.1f}s peak={db:7.1f}dBFS", flush=True)
        except KeyboardInterrupt:
            print("[rec] interrupted", flush=True)

    dur = written / SR
    print(f"[rec] done frames={written} dur={dur:.2f}s peak={20*np.log10(peak_all) if peak_all>1e-9 else -120:.1f}dBFS", flush=True)

    raw = np.fromfile(out, dtype=np.float32)
    raw = raw[:(raw.size // CH) * CH].reshape(-1, CH)
    wav = out.with_suffix(".wav")
    try:
        import soundfile as sf

        sf.write(str(wav), np.clip(raw, -1.0, 1.0), SR, subtype="PCM_16")
        print(f"[rec] wav={wav}", flush=True)
    except Exception as e:  # noqa: BLE001
        print(f"[rec] wav convert failed: {e}", flush=True)

    out.with_suffix(".json").write_text(
        json.dumps({"sr": SR, "channels": CH, "seconds": round(dur, 3), "peak": peak_all}, ensure_ascii=False),
        encoding="utf-8",
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
