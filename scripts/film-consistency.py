#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""
film-consistency.py — 多段片的接缝一致性度量（配色 / 布局 / 亮度，三条独立判，不合成总分）

用法：
  python scripts/film-consistency.py --run 168
  python scripts/film-consistency.py --parts a.mp4 b.mp4 c.mp4
  python scripts/film-consistency.py --run 168 --json

判据与阈值来自 skill `flowedge-shot-consistency`（标定自本机真实成片）：
  layout  3x3 分块色相直方图巴氏系数均值  >= 0.94
  palette 全局 HSV 联合直方图巴氏系数      >= 0.92
  luma    整帧平均亮度差                   <= 0.07
为什么不用 dHash：色相旋转 120° / 亮度 -35% 它都几乎不动（实测 0.98/1.00），
抓不到「换了场地」；三条分开判才能定位是布局变了还是调色变了。

退出码：0 全过；1 有 FAIL；2 输入错误
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
import urllib.request

import numpy as np
from PIL import Image

API = "http://127.0.0.1:8080"
TH = {"layout": 0.94, "palette": 0.92, "luma": 0.07}


def fetch_json(path: str):
    with urllib.request.urlopen(API + path, timeout=10) as r:
        return json.loads(r.read().decode("utf-8"))


def run_parts(run_id: int):
    """从 run 的 end 节点拿有序分段（detail 里带 file）。"""
    run = fetch_json("/api/runs/%d" % run_id)
    end = next((n for n in run.get("node_states", []) if n.get("type") == "end"), None)
    if not end:
        raise SystemExit("run %s 没有 end 节点" % run_id)
    out = end.get("output") or {}
    files = []
    for key in ("parts", "detail"):
        raw = out.get(key)
        if not raw:
            continue
        try:
            data = json.loads(raw) if isinstance(raw, str) else raw
        except Exception:
            continue
        if isinstance(data, list):
            files = [d.get("file") for d in data if isinstance(d, dict) and d.get("file")]
            if files:
                break
        if isinstance(data, dict) and isinstance(data.get("parts"), list):
            files = [d.get("file") for d in data["parts"] if isinstance(d, dict) and d.get("file")]
            if files:
                break
    video = out.get("video")
    return [f for f in files if f and os.path.exists(f)], video, run


def grab_frame(src: str, dest: str, last: bool = False):
    if last:
        cmd = ["ffmpeg", "-y", "-v", "error", "-sseof", "-0.30", "-i", src,
               "-update", "1", "-q:v", "2", dest]
    else:
        cmd = ["ffmpeg", "-y", "-v", "error", "-i", src,
               "-vf", "select=eq(n\\,0)", "-frames:v", "1", "-q:v", "2", dest]
    subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)


def hsv_arrays(path: str):
    img = Image.open(path).convert("RGB").resize((288, 162))
    hsv = np.asarray(img.convert("HSV"), dtype=np.uint8)
    return hsv


def hist_bc(h1: np.ndarray, h2: np.ndarray) -> float:
    """巴氏系数：两直方图归一化后逐 bin 取几何平均再求和。"""
    p = h1.astype(np.float64).ravel()
    q = h2.astype(np.float64).ravel()
    sp, sq = p.sum(), q.sum()
    if sp <= 0 or sq <= 0:
        return 0.0
    return float(np.sqrt(p / sp * q / sq).sum())


def palette_hsv(hsv: np.ndarray):
    h = (hsv[:, :, 0].astype(np.int32) * 32) // 256
    s = (hsv[:, :, 1].astype(np.int32) * 8) // 256
    idx = (h * 8 + s).ravel()
    return np.bincount(idx, minlength=32 * 8)


def layout_blocks(hsv: np.ndarray):
    hh, ww = hsv.shape[0] // 3, hsv.shape[1] // 3
    out = []
    for by in range(3):
        for bx in range(3):
            blk = hsv[by * hh:(by + 1) * hh, bx * ww:(bx + 1) * ww]
            h = (blk[:, :, 0].astype(np.int32) * 32) // 256
            out.append(np.bincount(h.ravel(), minlength=32))
    return out


def compare(a: str, b: str):
    ha, hb = hsv_arrays(a), hsv_arrays(b)
    layout = float(np.mean([hist_bc(x, y) for x, y in zip(layout_blocks(ha), layout_blocks(hb))]))
    palette = hist_bc(palette_hsv(ha), palette_hsv(hb))
    luma = abs(float(ha[:, :, 2].mean()) - float(hb[:, :, 2].mean())) / 255.0
    verdict = ("FAIL" if layout < TH["layout"] else "WARN") if layout < TH["layout"] else "PASS"
    if palette < TH["palette"]:
        verdict = "FAIL"
    if luma > TH["luma"]:
        verdict = "FAIL"
    return {"layout": round(layout, 3), "palette": round(palette, 3),
            "luma": round(luma, 3), "verdict": verdict}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--run", type=int)
    ap.add_argument("--parts", nargs="*")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()

    video = ""
    if a.run:
        parts, video, _ = run_parts(a.run)
        if not parts:
            raise SystemExit("run %s 没有分段（parts 空）——可能是单镜头或没有落 detail" % a.run)
    elif a.parts:
        parts = a.parts
    else:
        raise SystemExit("用法：--run <id> 或 --parts a.mp4 b.mp4 ...")

    rows = []
    with tempfile.TemporaryDirectory() as td:
        for i in range(len(parts) - 1):
            last_a = os.path.join(td, "a%d_last.png" % i)
            first_b = os.path.join(td, "b%d_first.png" % i)
            grab_frame(parts[i], last_a, last=True)
            grab_frame(parts[i + 1], first_b, last=False)
            m = compare(last_a, first_b)
            rows.append({"seam": "%d->%d" % (i + 1, i + 2), "from": os.path.basename(parts[i]),
                         "to": os.path.basename(parts[i + 1]), **m})

    fails = [r for r in rows if r["verdict"] == "FAIL"]
    if a.json:
        print(json.dumps({"run": a.run, "video": video, "parts": len(parts),
                          "thresholds": TH, "seams": rows, "fails": len(fails)}, ensure_ascii=False, indent=2))
    else:
        print("=== %s · %d 段 · %d 条接缝 ===" % (video or (("run %d" % a.run) if a.run else "parts"), len(parts), len(rows)))
        for r in rows:
            print("  %-6s 配色 %5.3f %-4s  布局 %5.3f %-4s  亮度 %5.3f %-4s  → %s" % (
                r["seam"], r["palette"], "ok" if r["palette"] >= TH["palette"] else "低",
                r["layout"], "ok" if r["layout"] >= TH["layout"] else "低",
                r["luma"], "ok" if r["luma"] <= TH["luma"] else "大", r["verdict"]))
        print("阈值：配色 ≥%.2f · 布局 ≥%.2f · 亮度差 ≤%.2f" % (TH["palette"], TH["layout"], TH["luma"]))
        print("结论：%s" % ("全部接缝通过" if not fails else "%d 条接缝 FAIL（换场地/换光/换调色）" % len(fails)))
    return 0 if not fails else 1


if __name__ == "__main__":
    sys.exit(main())
