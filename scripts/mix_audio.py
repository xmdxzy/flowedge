#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""mix_audio.py — 母带音频：BGM + 逐事件 SFX + 响度归一 + 真峰限幅

为什么单独一步：课纲要求「音效按事件逐拍计分」（图表出现/数字落定/结论压字各留 cue），
而渲染只出画面。分开做的好处是改音效不用重渲 16 分钟。

用法：
  python scripts/mix_audio.py outputs/ice-slip2
  python scripts/mix_audio.py outputs/ice-slip2 --bgm 路径 --kit .tmp/audio-kit --lufs -14
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

try:
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

FFMPEG = shutil.which("ffmpeg") or "ffmpeg"
DEF_BGM = Path(os.environ["FLOWEDGE_BGM"]) if os.environ.get("FLOWEDGE_BGM") else None
DEF_KIT = Path(".tmp/audio-kit")


def run(cmd):
    return subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace")


def main() -> int:
    ap = argparse.ArgumentParser(description="BGM + 事件音效混音")
    ap.add_argument("out", help="渲染产物目录（含 silent.mp4 与 cues.json）")
    ap.add_argument("--bgm", default=str(DEF_BGM) if DEF_BGM else "")
    ap.add_argument("--kit", default=str(DEF_KIT))
    ap.add_argument("--lufs", type=float, default=-14.0)
    ap.add_argument("--sfx-gain", type=float, default=0.55, help="音效总电平（相对 1.0）")
    args = ap.parse_args()

    out = Path(args.out)
    silent, cues_f = out / "silent.mp4", out / "cues.json"
    if not silent.exists():
        print(f"ERROR 缺 {silent}")
        return 2
    cues = json.loads(cues_f.read_text(encoding="utf-8-sig")) if cues_f.exists() else []
    kit = Path(args.kit)
    dur = float(run([shutil.which("ffprobe") or "ffprobe", "-v", "error", "-show_entries",
                     "format=duration", "-of", "default=nw=1:nk=1", str(silent)]).stdout.strip() or 0)

    inputs = [FFMPEG, "-y", "-v", "error", "-i", str(silent), "-i", args.bgm]
    filters = [f"[1:a]atrim=0:{dur:.3f},asetpts=N/SR/TB,loudnorm=I={args.lufs}:TP=-4:LRA=11[bed]"]
    labels = ["[bed]"]
    used = {}
    for i, c in enumerate(cues):
        kind = c.get("kind", "whoosh")
        wav = kit / f"{kind}.wav"
        if not wav.exists():
            continue
        idx = inputs.count("-i")  # 下一个输入索引
        inputs += ["-i", str(wav)]
        g = float(c.get("gain", 1.0)) * args.sfx_gain
        ms = max(0, int(float(c["t"]) * 1000))
        tag = f"s{i}"
        filters.append(f"[{idx}:a]volume={g:.2f},adelay={ms}|{ms}[{tag}]")
        labels.append(f"[{tag}]")
        used[kind] = used.get(kind, 0) + 1

    if len(labels) > 1:
        filters.append("".join(labels) + f"amix=inputs={len(labels)}:duration=first:normalize=0[mix]")
        tail = f"[mix]afade=t=out:st={max(0.4, dur-1.4):.3f}:d=1.4,alimiter=limit=0.5:level=false[a]"
    else:
        tail = f"[bed]afade=t=out:st={max(0.4, dur-1.4):.3f}:d=1.4,alimiter=limit=0.5:level=false[a]"
    filters.append(tail)

    master = out / "master.mp4"
    r = run(inputs + ["-filter_complex", ";".join(filters), "-map", "0:v", "-map", "[a]",
                      "-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-ar", "48000",
                      "-movflags", "+faststart", str(master)])
    if r.returncode != 0:
        print("ERROR 混音失败：\n" + r.stderr[-900:])
        return 2

    play = out / "play.mp4"
    r = run([FFMPEG, "-y", "-v", "error", "-i", str(master),
             "-c:v", "libx264", "-profile:v", "main", "-level", "4.0", "-pix_fmt", "yuv420p",
             "-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709",
             "-color_range", "tv", "-crf", "20", "-preset", "medium", "-movflags", "+faststart",
             "-af", "alimiter=limit=0.5:level=false",
             "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "44100", str(play)])
    if r.returncode != 0:
        print("WARN play 版失败：\n" + r.stderr[-400:])

    m = run([shutil.which("ffprobe") or "ffprobe", "-v", "error", "-show_entries",
             "format=duration,size", "-of", "json", str(play)]).stdout
    print(f"时长 {dur:.2f}s · 音效 {len(cues)} 个（{', '.join(f'{k}×{v}' for k, v in used.items())}）")
    print(f"母带 {master.name} {master.stat().st_size/1048576:.1f}MB · 交付 {play.name} {play.stat().st_size/1048576:.1f}MB")
    return 0


if __name__ == "__main__":
    sys.exit(main())
