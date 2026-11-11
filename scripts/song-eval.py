# -*- coding: utf-8 -*-
"""审美打分：Meta audiobox-aesthetics（CE / CU / PC / PQ，0–10 分）。

为什么要它：我自己的「听起来还行」没有任何信息量。这个模型是拿人类标注训练的
四轴评估器（内容愉悦度 / 有用性 / 制作复杂度 / 制作质量），能把「明显做坏了」的
候选先筛掉，也能量化「这一版比参考曲差多少」。

用法：
    python scripts/song-eval.py a.wav b.wav --ref ref.wav
    python scripts/song-eval.py *.wav --json out.json
"""
from __future__ import annotations

import argparse
import json
import os
import sys

CKPT = os.environ.get("AES_CKPT", ".tmp/aes/checkpoint.pt")
AXES = ["CE", "CU", "PC", "PQ"]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("files", nargs="+")
    ap.add_argument("--ref", default="", help="参考曲：输出与它的差值")
    ap.add_argument("--ckpt", default=CKPT)
    ap.add_argument("--json", default="")
    args = ap.parse_args()

    if not os.path.exists(args.ckpt):
        raise SystemExit(f"缺权重：{args.ckpt}（从 https://dl.fbaipublicfiles.com/audiobox-aesthetics/checkpoint.pt 下载）")

    import torch
    from audiobox_aesthetics.infer import initialize_predictor

    device = "cuda" if torch.cuda.is_available() else "cpu"
    pred = initialize_predictor(args.ckpt)
    pred.device = torch.device(device)

    paths = args.files + ([args.ref] if args.ref else [])
    rows = pred.forward([{"path": p} for p in paths])

    out = {}
    for p, r in zip(paths, rows):
        out[p] = {k: round(float(r[k]), 2) for k in AXES}

    ref = out.get(args.ref) if args.ref else None
    print(f"{'file':52s} " + " ".join(f"{k:>5s}" for k in AXES) + ("   Δvs参考" if ref else ""))
    for p, r in out.items():
        line = f"{os.path.basename(p)[:50]:52s} " + " ".join(f"{r[k]:5.2f}" for k in AXES)
        if ref and p != args.ref:
            line += "   " + " ".join(f"{r[k] - ref[k]:+5.2f}" for k in AXES)
        print(line)

    if args.json:
        with open(args.json, "w", encoding="utf-8") as f:
            json.dump(out, f, ensure_ascii=False, indent=2)
    return 0


if __name__ == "__main__":
    sys.exit(main())
