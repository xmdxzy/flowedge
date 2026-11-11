#!/usr/bin/env python3
"""把一条 BGM 铺进已有成片（旁链 ducking，人声起伏时不抢口播）。

用法：
  python scripts/mix-bgm.py --video in.mp4 --bgm master.wav --out out.mp4 \
      [--gain -16] [--duck 5] [--offset 0] [--fade-out 3] [--fade-in 0.6] \
      [--no-highpass]

设计口径（与 backend/service/audio_master.go 的 mixBGM 同一套）：
  - BGM 用 -stream_loop -1 补长，再 atrim 到视频流时长，末段淡出；
  - 旁链压缩用原声轨做 key：人声一起 BGM 就让位；
  - amix normalize=0 + alimiter（level=false，防自动抬回 0dB）；
  - 视频流 -c:v copy，只重编码音轨，保证不引入帧数变化。

承诺：失败必须非零退出，不产出「时长对但没音乐」的假成功。
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys


def ffprobe(path: str) -> dict:
    out = subprocess.run(
        ["ffprobe", "-v", "error", "-print_format", "json",
         "-show_streams", "-show_format", path],
        check=True, capture_output=True, text=True,
    ).stdout
    return json.loads(out)


def video_duration(path: str) -> float:
    info = ffprobe(path)
    for s in info["streams"]:
        if s["codec_type"] == "video":
            # 装配器的铁律：用视频流时长，不用容器时长
            if s.get("duration"):
                return float(s["duration"])
            if s.get("nb_frames") and s.get("avg_frame_rate"):
                num, den = s["avg_frame_rate"].split("/")
                return float(s["nb_frames"]) * float(den) / float(num)
    raise SystemExit(f"读不到视频流时长：{path}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--video", required=True)
    ap.add_argument("--bgm", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--gain", type=float, default=-10.0, help="BGM 电平 dB（相对母带）")
    ap.add_argument("--duck", type=float, default=2.5, help="旁链压缩比；0 = 关闭")
    ap.add_argument("--duck-threshold", type=float, default=0.12, help="旁链阈值（线性幅度）")
    ap.add_argument("--duck-attack", type=float, default=20.0)
    ap.add_argument("--duck-release", type=float, default=500.0)
    ap.add_argument("--offset", type=float, default=0.0, help="从 BGM 第几秒开始取")
    ap.add_argument("--fade-in", type=float, default=0.6)
    ap.add_argument("--fade-out", type=float, default=3.0)
    ap.add_argument("--highpass", type=float, default=45.0)
    a = ap.parse_args()

    dur = video_duration(a.video)
    if dur <= 0:
        raise SystemExit("视频流时长 <= 0")
    fade_out = min(a.fade_out, max(0.0, dur - 1.0))
    fade_in = min(a.fade_in, dur - fade_out)

    chain = (
        f"[1:a]atrim=start={a.offset:.3f}:end={a.offset + dur:.3f},asetpts=PTS-STARTPTS"
        + (f",highpass=f={a.highpass:.0f}" if a.highpass > 0 else "")
        + f",volume={a.gain:.1f}dB"
        + (f",afade=t=in:st=0:d={fade_in:.3f}" if fade_in > 0 else "")
        + f",afade=t=out:st={dur - fade_out:.3f}:d={fade_out:.3f}[bg];"
    )
    if a.duck > 0:
        chain += (
            "[0:a]asplit=2[main][sc];"
            f"[bg][sc]sidechaincompress=threshold={a.duck_threshold:.3f}:ratio={a.duck:.1f}"
            f":attack={a.duck_attack:.0f}:release={a.duck_release:.0f}[bgd];"
            "[main][bgd]amix=inputs=2:duration=first:normalize=0,alimiter=limit=0.8:level=false[a]"
        )
    else:
        chain += (
            "[0:a][bg]amix=inputs=2:duration=first:normalize=0,"
            "alimiter=limit=0.8:level=false[a]"
        )

    args = [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
        "-i", a.video,
        "-stream_loop", "-1", "-i", a.bgm,
        "-filter_complex", chain,
        "-map", "0:v:0", "-map", "[a]",
        "-c:v", "copy",
        "-c:a", "aac", "-b:a", "192k", "-ar", "48000",
        "-t", f"{dur:.3f}",
        a.out,
    ]
    proc = subprocess.run(args, capture_output=True, text=True)
    if proc.returncode != 0:
        raise SystemExit(f"ffmpeg 失败：{proc.stderr.strip()[:800]}")

    # 混音后立刻复核：视频帧数、音轨时长、文件存在
    src, dst = ffprobe(a.video), ffprobe(a.out)
    sv = next(s for s in src["streams"] if s["codec_type"] == "video")
    dv = next(s for s in dst["streams"] if s["codec_type"] == "video")
    da = next((s for s in dst["streams"] if s["codec_type"] == "audio"), None)
    if da is None:
        raise SystemExit("混音产物没有音轨")
    if sv.get("nb_frames") and dv.get("nb_frames") and sv["nb_frames"] != dv["nb_frames"]:
        raise SystemExit(f"帧数变了：{sv['nb_frames']} -> {dv['nb_frames']}")
    dd = float(dv.get("duration") or dst["format"]["duration"])
    print(f"[ok] {os.path.basename(a.out)} {dd:.3f}s frames={dv.get('nb_frames')} "
          f"audio={float(da['duration']):.3f}s")


if __name__ == "__main__":
    main()
