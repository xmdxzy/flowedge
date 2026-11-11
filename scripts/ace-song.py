#!/usr/bin/env python3
"""分段生成 + 拼接一首完整曲（ACE-Step 1.5）。

为什么必须分段：实测一次 180s 请求会给 178.6s 文件，但 60s 之后稳定衰减到
-40~-45dB、每 8s 与开头 chroma 相似度 0.90–0.96 —— 是同一段 loop 越放越轻，
没有副歌/桥段。所以结构必须由「多段生成 + 拼接」显式做出来。

每段：同 BPM / 同调 / 独立 seed，各自目标响度（intro 轻、副歌重），
拼接用 acrossfade（默认 1.5s，等于 120BPM 的 3/4 小节，接缝不硬切）。

用法：
  python scripts/ace-song.py --out-dir .tmp/song --title aquarium
      [--dry-run]            只打印计划，不调用服务
      [--bpm 120 --key "C major" --model acestep-v15-sft --steps 16]
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent

# 载入同目录的 ace-gen.py（文件名带连字符，不能直接 import）
_spec = importlib.util.spec_from_file_location("ace_gen", HERE / "ace-gen.py")
ace = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ace)

BASE_PROMPT = ("bright modern Mandarin jazz-pop R&B instrumental, {bpm} BPM, {key}, "
               "punchy crisp mix, Fender Rhodes seventh and ninth chords, "
               "clear fingerstyle electric bass, airy strings, glassy vibraphone, "
               "aquarium night atmosphere")

# bars：120BPM 时 1 小节 = 2s。段首尾静音会被裁掉（实测每段 2–7s），
# 所以按 122 小节 = 244s 下料，裁完 + 交叉淡化后约 3:28。
PLAN = [
    dict(key="intro", bars=12, target=-18.0,
         text="sparse intro, solo Rhodes chords, soft pad swells, no drums, underwater ambience, gentle lift"),
    dict(key="verse1", bars=24, target=-15.5,
         text="verse groove, walking fingerstyle bass, brushed drums entering, sparse vibraphone accents, intimate and restrained"),
    dict(key="chorus1", bars=24, target=-12.5,
         text="full chorus, tight drum kit with crisp closed hi-hats, layered Rhodes, string pad swell, brighter and wider, singing lead line"),
    dict(key="verse2", bars=24, target=-15.0,
         text="second verse, add muted electric guitar comping, keep it restrained, dubby bass, night club feel"),
    dict(key="chorus2", bars=24, target=-12.0,
         text="final chorus, full band, shimmering ride cymbal, double-tracked keys, wide airy top end, uplifting"),
    dict(key="outro", bars=14, target=-19.0,
         text="outro, drums drop out, Rhodes and pad decay slowly, underwater ambience, long fade"),
]

XFADE = 2.0
TRIM = ("silenceremove=start_periods=1:start_threshold=-45dB:start_duration=0.05,"
        "areverse,"
        "silenceremove=start_periods=1:start_threshold=-45dB:start_duration=0.05,"
        "areverse")


def sh(cmd: list[str]) -> str:
    p = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace")
    if p.returncode != 0:
        raise RuntimeError(f"{cmd[0]} failed: {(p.stderr or p.stdout)[-800:]}")
    return p.stderr + p.stdout


def integrated_lufs(path: Path) -> float:
    out = sh(["ffmpeg", "-hide_banner", "-nostats", "-i", str(path),
              "-af", "ebur128=peak=true", "-f", "null", "-"])
    vals = re.findall(r"I:\s*(-?\d+\.?\d*)\s*LUFS", out)
    if not vals:
        raise RuntimeError(f"拿不到 LUFS：{path}")
    return float(vals[-1])


def duration(path: Path) -> float:
    out = sh(["ffprobe", "-v", "error", "-show_entries", "format=duration",
              "-of", "default=nw=1:nk=1", str(path)])
    return float(out.strip().splitlines()[-1])


def build_graph(n: int, gains: list[float], shelf: str, out_label: str = "aout") -> str:
    parts = []
    for i, g in enumerate(gains):
        parts.append(f"[{i}:a]volume={g:.2f}dB,aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo[v{i}]")
    cur = "[v0]"
    for i in range(1, n):
        nxt = f"[x{i}]"
        parts.append(f"{cur}[v{i}]acrossfade=d={XFADE}:c1=tri:c2=tri{nxt}")
        cur = nxt
    tail = "loudnorm=I=-14:TP=-1.5:LRA=11"
    if shelf:
        tail = shelf + "," + tail
    parts.append(f"{cur}{tail},alimiter=limit=0.8:level=false[{out_label}]")
    return ";".join(parts)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out-dir", required=True)
    ap.add_argument("--title", default="song")
    ap.add_argument("--bpm", type=int, default=120)
    ap.add_argument("--key", default="C major")
    ap.add_argument("--model", default="acestep-v15-sft")
    ap.add_argument("--steps", type=int, default=16)
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()

    out = Path(a.out_dir)
    out.mkdir(parents=True, exist_ok=True)
    sec_dir = out / "sections"
    sec_dir.mkdir(exist_ok=True)

    plan = []
    for i, s in enumerate(PLAN):
        d = s["bars"] * 4 * 60.0 / a.bpm  # 4/4：1 小节 = 4 拍；120BPM ⇒ 1 小节 = 2s
        plan.append({**s, "index": i, "seconds": round(d, 2),
                     "prompt": BASE_PROMPT.format(bpm=a.bpm, key=a.key) + ", " + s["text"],
                     "seed": 1000 + i * 17})
    print(f"[plan] {len(plan)} 段，预计 {sum(s['seconds'] for s in plan):.1f}s，"
          f"拼完约 {sum(s['seconds'] for s in plan) - XFADE * (len(plan) - 1):.1f}s", flush=True)
    for s in plan:
        print(f"  {s['key']:8s} {s['seconds']:5.1f}s seed={s['seed']} target={s['target']}dB  {s['text'][:60]}")

    (out / "plan.json").write_text(json.dumps(plan, ensure_ascii=False, indent=1), encoding="utf-8")
    if a.dry_run:
        return 0

    files, gains, measured = [], [], []
    for s in plan:
        f = sec_dir / f"{s['index']:02d}-{s['key']}.wav"
        if not f.exists() or f.stat().st_size < 10000:
            print(f"[gen] {s['key']} ({s['seconds']:.0f}s) …", flush=True)
            url = ace.generate(s["prompt"], s["seconds"], a.model, a.steps,
                               False, s["seed"], a.bpm, a.key)
            ace.download(url, f)
        # 段首尾静音必须当场裁掉：acrossfade 只叠音量，补不了「一段的尾巴是全静音」，
        # 实测会在接缝处留 0.58s 的真静音（133.5–134.0s）。
        tf = sec_dir / f"{s['index']:02d}-{s['key']}.trim.wav"
        sh(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", str(f),
            "-af", TRIM, "-c:a", "pcm_f32le", str(tf)])
        raw_dur, dur = duration(f), duration(tf)
        lufs = integrated_lufs(tf)
        g = s["target"] - lufs
        files.append(tf)
        gains.append(g)
        measured.append({"key": s["key"], "file": str(f), "trimmed": str(tf),
                         "raw_seconds": round(raw_dur, 2), "seconds": round(dur, 2),
                         "trimmed_out_s": round(raw_dur - dur, 2),
                         "lufs": lufs, "gain_db": round(g, 2)})
        print(f"[sec] {s['key']:8s} {raw_dur:6.2f}s → {dur:6.2f}s (裁掉 {raw_dur-dur:4.2f}s)"
              f"  {lufs:6.1f} LUFS  gain {g:+.1f}dB", flush=True)

    (out / "sections.json").write_text(json.dumps(measured, ensure_ascii=False, indent=1), encoding="utf-8")

    variants = {"raw": "", "bright": "highshelf=f=6000:g=4.0:t=q:w=0.7"}
    made = {}
    for name, shelf in variants.items():
        dst = out / f"{a.title}-{name}.wav"
        cmd = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-y"]
        for f in files:
            cmd += ["-i", str(f)]
        cmd += ["-filter_complex", build_graph(len(files), gains, shelf),
                "-map", "[aout]", "-c:a", "pcm_s24le", "-ar", "48000", str(dst)]
        sh(cmd)
        made[name] = dst
        print(f"[out] {dst}  {duration(dst):.2f}s", flush=True)

    (out / "build.json").write_text(json.dumps(
        {"title": a.title, "bpm": a.bpm, "key": a.key, "model": a.model, "steps": a.steps,
         "xfade_s": XFADE, "sections": measured,
         "outputs": {k: str(v) for k, v in made.items()},
         "total_seconds": round(sum(m["seconds"] for m in measured) - XFADE * (len(measured) - 1), 2)},
        ensure_ascii=False, indent=1), encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
