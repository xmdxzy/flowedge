#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""
fix-av-desync.py — 把「音轨比视频流长」的成片修到音画同长（不重编码画面）。

背景：多段装配（xfade + acrossfade）后实测会出现 0.1–0.3s 的音画时长差，
verify-media 报 `av_desync` 硬失败。根因在装配侧（偏移按哪个时长算），
这里只做**交付前的确定性收口**：以视频流时长为准，音轨超长就截、偏短就补静音。

用法：
  python scripts/fix-av-desync.py in.mp4 [--out out.mp4] [--verify]
退出码：0 修好且（若 --verify）复核通过；1 无需修/失败；2 输入错误
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys


def probe(path: str):
    out = subprocess.run(
        ["ffprobe", "-v", "error", "-print_format", "json", "-show_streams", "-show_format", path],
        capture_output=True, text=True, check=True).stdout
    data = json.loads(out)
    v = next((s for s in data["streams"] if s.get("codec_type") == "video"), None)
    a = next((s for s in data["streams"] if s.get("codec_type") == "audio"), None)
    if not v:
        raise SystemExit("没有视频流：%s" % path)
    vdur = float(v.get("duration") or data["format"].get("duration") or 0)
    adur = float(a.get("duration") or 0) if a else 0.0
    return vdur, adur, bool(a)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("src")
    ap.add_argument("--out", default="")
    ap.add_argument("--verify", action="store_true")
    a = ap.parse_args()
    if not os.path.exists(a.src):
        print("找不到文件：%s" % a.src)
        return 2

    vdur, adur, has_audio = probe(a.src)
    delta = adur - vdur
    print("视频 %.3fs · 音频 %.3fs · 差 %+.3fs" % (vdur, adur, delta))
    if not has_audio or abs(delta) < 0.02:
        print("无需修复（差 < 0.02s）")
        return 1

    out = a.out or os.path.splitext(a.src)[0] + "-sync.mp4"
    # 视频流直接 copy（不重编码）；音轨按视频时长裁齐，偏短则补静音
    afilter = "atrim=0:%.6f,asetpts=PTS-STARTPTS,apad=whole_dur=%.6f" % (vdur, vdur)
    cmd = ["ffmpeg", "-y", "-v", "error", "-i", a.src,
           "-map", "0:v:0", "-map", "0:a:0", "-c:v", "copy",
           "-af", afilter, "-c:a", "aac", "-b:a", "160k",
           "-t", "%.6f" % vdur, "-movflags", "+faststart", out]
    subprocess.run(cmd, check=True)
    v2, a2, _ = probe(out)
    print("修后：视频 %.3fs · 音频 %.3fs · 差 %+.3fs → %s" % (v2, a2, a2 - v2, out))

    if a.verify:
        r = subprocess.run([sys.executable, os.path.join(os.path.dirname(__file__), "verify-media.py"),
                            out, "--asr"], capture_output=True, text=True)
        tail = "\n".join([ln for ln in r.stdout.splitlines() if ln.strip()][-6:])
        print(tail)
        return 0 if r.returncode == 0 else 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
