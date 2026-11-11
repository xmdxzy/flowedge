# -*- coding: utf-8 -*-
"""分轨体检：量每个 stem 的电平与段落占比，回答「鼓是不是太重、钢琴是不是太轻」。

为什么用它：这是唯一能把「听感」翻译成可比数字的办法——
用户说「前面钢琴听不清」，在数据上就是「前 40 秒 piano stem 比 drums 低 X dB」。
"""
import glob
import json
import sys

import numpy as np
import soundfile as sf


def db(x):
    return float(20 * np.log10(max(np.sqrt(np.mean(x ** 2)), 1e-9)))


def analyse(pattern, label, sections):
    files = {}
    for f in glob.glob(pattern):
        stem = f.rsplit("_", 1)[-1].split(".")[0]
        files[stem] = f
    if not files:
        print(f"{label}: 没找到 stem（{pattern}）")
        return None
    data = {s: sf.read(p, dtype="float32", always_2d=True)[0].mean(axis=1) for s, p in files.items()}
    sr = sf.read(list(files.values())[0], dtype="float32")[0].shape[0]
    total = float(20 * np.log10(np.sqrt(sum(np.mean(v ** 2) for v in data.values())) + 1e-12))
    print(f"\n== {label} ==  全曲 {total:.1f} dB")
    rows = {}
    for s, v in sorted(data.items(), key=lambda kv: -np.mean(kv[1] ** 2)):
        rel = db(v) - total
        rows[s] = round(rel, 1)
        print(f"   {s:8s} {db(v):7.1f} dB   相对 {rel:+6.1f} dB")
    print("   分段（相对全曲 dB）：")
    for name, (a, b) in sections.items():
        seg = {s: (db(v[int(a * sr):int(b * sr)]) - total) for s, v in data.items()}
        top = sorted(seg.items(), key=lambda kv: -kv[1])[:4]
        print(f"     {name:6s} " + "  ".join(f"{k}={v:+.1f}" for k, v in top))
    return {"total": round(total, 1), "stems": rows}


def main():
    sections = {"0-40s": (0, 40), "40-80s": (40, 80), "80-120s": (80, 120), "120-160s": (120, 160)}
    a = analyse(".tmp/stems/htdemucs_6s/ace-ambient-rnb-piano-84bpm_*.wav", "这一版（84bpm）", sections)
    b = analyse(".tmp/stems/htdemucs_6s/如果回到初见那天_*.wav", "你认可的那首", sections)
    if a and b:
        print("\n== 差异（这一版 − 喜欢那首，dB）==")
        for s in ("drums", "bass", "piano", "other", "vocals", "guitar"):
            if s in a["stems"] and s in b["stems"]:
                print(f"   {s:8s} {a['stems'][s] - b['stems'][s]:+6.1f}")
    json.dump({"new": a, "ref": b}, open(".tmp/stems/balance.json", "w", encoding="utf-8"),
              ensure_ascii=False, indent=2)


if __name__ == "__main__":
    sys.exit(main())
