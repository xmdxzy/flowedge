# -*- coding: utf-8 -*-
"""专辑编曲走向表：每 5s 一段的响度 / 频段 / 亮度 / 瞬态密度 / 立体声。

用途：给「风格卡」提供可核对的编排证据（哪里起、哪里顶、哪里撤）。
用法：python scripts/song-arc.py <音频> [<音频2> ...] [--step 5] [--json out.json]
"""
import json
import subprocess
import sys

import numpy as np

SR = 22050
N = 4096
HOP = 1024


def decode(path, ch=1):
    raw = subprocess.run(["ffmpeg", "-v", "error", "-i", path, "-ac", str(ch), "-ar", str(SR),
                          "-f", "f32le", "-"], capture_output=True, check=True).stdout
    return np.frombuffer(raw, dtype=np.float32).reshape(-1, ch)


def frames(x):
    cnt = 1 + (len(x) - N) // HOP
    idx = np.arange(N)[None, :] + HOP * np.arange(cnt)[:, None]
    return x[idx]


def analyze(path, step=5.0):
    x = decode(path)[:, 0]
    st = decode(path, 2)
    dur = len(x) / SR
    fr = frames(x)
    win = np.hanning(N).astype(np.float32)
    mag = np.abs(np.fft.rfft(fr * win, axis=1))
    freqs = np.fft.rfftfreq(N, 1 / SR)
    power = mag ** 2
    tot = power.sum(axis=1) + 1e-12
    bands = {
        "sub": (0, 60), "low": (60, 250), "mid": (250, 2000),
        "hi": (2000, 6000), "air": (6000, 11025),
    }
    ratio = {k: power[:, (freqs >= a) & (freqs < b)].sum(axis=1) / tot
             for k, (a, b) in bands.items()}
    centroid = (power * freqs).sum(axis=1) / tot

    # 瞬态密度：谱通量超过局部中位数 2 倍的帧数 / 秒
    lm = np.log1p(mag * 10)
    flux = np.maximum(0, np.diff(lm, axis=0)).sum(axis=1)
    fps = SR / HOP
    roll = np.convolve(flux, np.ones(int(fps)) / int(fps), mode="same")
    onsets = flux > np.median(flux) * 2.5

    # 立体声
    L, R = st[:, 0], st[:, 1]
    rows = []
    for t0 in np.arange(0, dur, step):
        i0, i1 = int(t0 * fps), int(min(t0 + step, dur) * fps)
        if i1 - i0 < 2:
            continue
        seg_rms = 20 * np.log10(np.sqrt(np.mean(x[int(t0 * SR):int((t0 + step) * SR)] ** 2)) + 1e-9)
        n0, n1 = int(t0 * SR), int(min(t0 + step, dur) * SR)
        rows.append({
            "t": round(float(t0), 1),
            "rms": round(float(seg_rms), 1),
            "sub": round(100 * float(ratio["sub"][i0:i1].mean()), 1),
            "low": round(100 * float(ratio["low"][i0:i1].mean()), 1),
            "mid": round(100 * float(ratio["mid"][i0:i1].mean()), 1),
            "hi": round(100 * float(ratio["hi"][i0:i1].mean()), 1),
            "cent": int(float(centroid[i0:i1].mean())),
            "ons": round(float(onsets[i0:i1].mean() * fps), 1),
            "lr": round(float(np.corrcoef(L[n0:n1], R[n0:n1])[0, 1]), 3) if n1 - n0 > 100 else None,
        })
    return {"file": path, "duration_s": round(dur, 2), "step_s": step, "rows": rows}


def main():
    argv = sys.argv[1:]
    args, i = [], 0
    step = 5.0
    out = None
    while i < len(argv):
        a = argv[i]
        if a == "--step":
            step = float(argv[i + 1]); i += 2
        elif a == "--json":
            out = argv[i + 1]; i += 2
        elif a.startswith("--"):
            i += 1
        else:
            args.append(a); i += 1
    res = [analyze(p, step) for p in args]
    if out:
        with open(out, "w", encoding="utf-8") as f:
            json.dump(res, f, ensure_ascii=False, indent=1)
    for r in res:
        print(f"\n== {r['file'].split(chr(92))[-1]}  {r['duration_s']}s ==")
        print("  t     rms   sub  low  mid   hi  cent  ons   lr")
        for row in r["rows"]:
            print(f"{row['t']:5.0f} {row['rms']:6.1f} {row['sub']:5.1f} {row['low']:4.1f} "
                  f"{row['mid']:4.1f} {row['hi']:4.1f} {row['cent']:5d} {row['ons']:4.1f} "
                  f"{row['lr'] if row['lr'] is not None else '-':>4}")


if __name__ == "__main__":
    main()
