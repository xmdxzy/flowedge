# -*- coding: utf-8 -*-
"""按分轨重混：把被埋掉的钢琴抬起来、鼓收一点，再走母带链。

依据（demucs 实测相对电平，单位 dB，相对全曲）：
  这一版  bass -2.2 / drums -5.9 / other -10.4 / guitar -13.7 / piano -20.8
  认可版  drums -4.4 / bass -4.6 / guitar -8.0 / other -10.7 / vocals(melody) -13.6
=> 旋律层（钢琴）比鼓低 15 dB，等于没有；目标把钢琴做到 -10 上下、鼓压到 -8 左右。

用法: python .tmp/remix.py <stemdir前缀> <out.wav> --piano 10 --drums -3 ...
"""
import argparse
import glob
import json
import re
import subprocess
from pathlib import Path

import numpy as np
import soundfile as sf


def loudness(path: Path):
    r = subprocess.run(["ffmpeg", "-hide_banner", "-nostats", "-i", str(path), "-af", "ebur128=peak=true",
                        "-f", "null", "-"], capture_output=True, text=True)
    t = r.stderr
    i = re.findall(r"I:\s+(-?\d+\.\d+) LUFS", t)
    tp = re.findall(r"Peak:\s+(-?\d+\.\d+) dBFS", t)
    return (float(i[-1]) if i else None), (float(tp[-1]) if tp else None)


def db(x):
    return float(20 * np.log10(max(np.sqrt(np.mean(x ** 2)), 1e-9)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("pattern")
    ap.add_argument("out")
    ap.add_argument("--gains", default="piano=10,drums=-3,bass=-1,other=1,guitar=1,vocals=-6")
    ap.add_argument("--lufs", type=float, default=-14.0)
    ap.add_argument("--tp", type=float, default=-1.5)
    args = ap.parse_args()

    gains = {}
    for kv in args.gains.split(","):
        k, v = kv.split("=")
        gains[k.strip()] = float(v)

    stems = {}
    sr = None
    for f in glob.glob(args.pattern):
        name = Path(f).stem.rsplit("_", 1)[-1]
        y, sr = sf.read(f, dtype="float32", always_2d=True)
        stems[name] = y
    if not stems:
        raise SystemExit(f"没找到 stem：{args.pattern}")

    mixed = None
    for name, y in stems.items():
        g = 10 ** (gains.get(name, 0.0) / 20.0)
        mixed = y * g if mixed is None else mixed + y * g
    # 混合后过一遍软限幅防溢出，再写盘
    peak = float(np.max(np.abs(mixed))) or 1.0
    if peak > 0.98:
        mixed = mixed * (0.98 / peak)

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(out), np.clip(mixed, -1, 1), sr, subtype="PCM_24", format="WAV")

    # 两遍线性 loudnorm + 限幅
    tmp = out.with_suffix(".premaster.wav")
    r = subprocess.run(["ffmpeg", "-hide_banner", "-nostats", "-i", str(out),
                        "-af", "loudnorm=I=-14:TP=-1.5:LRA=11:print_format=json", "-f", "null", "-"],
                       capture_output=True, text=True)
    m = re.search(r"\{[^{}]*\"input_i\"[^{}]*\}", r.stderr, re.S)
    js = json.loads(m.group(0))
    ln = (f"loudnorm=I={args.lufs}:TP={args.tp}:LRA=11:measured_I={js['input_i']}:measured_TP={js['input_tp']}:"
          f"measured_LRA={js['input_lra']}:measured_thresh={js['input_thresh']}:offset={js['target_offset']}:linear=true")
    final = out.with_name(out.stem + "_master.wav")
    subprocess.run(["ffmpeg", "-y", "-v", "error", "-i", str(out),
                    "-af", f"{ln},alimiter=limit=0.89:level=false", "-ar", "48000", "-c:a", "pcm_s24le",
                    str(final)], check=True)

    mono = mixed.mean(axis=1)
    total = db(mono)
    print(json.dumps({
        "out": str(final),
        "gains": gains,
        "stem_rel_db": {k: round(db(v.mean(axis=1)) - total, 1) for k, v in stems.items()},
        "loudness": loudness(final),
    }, ensure_ascii=False, indent=2))
    tmp.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
