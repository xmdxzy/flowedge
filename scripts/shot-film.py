#!/usr/bin/env python3
"""多段成片编排：角色定妆图锁脸 → 逐段出片（可带台词）→ 交叉淡化装配 → 客观验收。

为什么需要这一层
----------------
H3 单次最长约 15 秒（362 帧），30 秒必须分段。分段最大的风险是**角色换脸**：
每次生成都是独立采样，靠"重复描述外貌"只能做到形似。

这里的做法（2026-10-19 实测）：
  1. **先用文生图出角色定妆图**（正面全身、干净背景）
  2. 每一段都把这组定妆图作为 ref_images 传给 ref2va → 角色身份由图像锁定，不靠文字
  3. 每段内部用**多镜头单提示词**（[Shot 1] … [Shot 2] At 00:04.500 …），
     段内切机位但声音连续；段间用 xfade + acrossfade 0.2s 缝合
  4. 台词只在需要的段生成 wav（TTS），该段走 dialogue；其余段走 ambience（同样带参考图）

用法
----
    python scripts/shot-film.py --plan film.json [--frames 243] [--dry-run]

film.json:
{
  "title": "金丹斗法",
  "characters": ["still_a.png", "still_b.png"],
  "segments": [
    {"id": 1, "prompt": "...", "lines": [{"speaker":"S1","text":"...","pause":0.4}]},
    {"id": 2, "prompt": "...", "lines": []},
    {"id": 3, "prompt": "...", "lines": [{"speaker":"S2","text":"..."}]}
  ],
  "frames": 243,
  "out_dir": "...",
  "out": "film.mp4"
}
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
PYEXE = sys.executable


def find_ffmpeg() -> str:
    from shutil import which
    for n in ("ffmpeg", "ffmpeg.exe"):
        p = which(n)
        if p:
            return p
    for c in (r"C:\ffmpeg\bin\ffmpeg.exe", r"D:\ffmpeg\bin\ffmpeg.exe"):
        if os.path.isfile(c):
            return c
    return "ffmpeg"


FFMPEG = find_ffmpeg()
FFPROBE = FFMPEG.replace("ffmpeg", "ffprobe")


def run(cmd: list[str], timeout: int = 3600) -> tuple[int, str]:
    cp = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8",
                        errors="replace", timeout=timeout)
    return cp.returncode, (cp.stdout or "") + (cp.stderr or "")


def probe(path: str) -> dict:
    code, out = run([FFPROBE, "-v", "error", "-print_format", "json",
                     "-show_format", "-show_streams", path], 120)
    if code != 0:
        raise RuntimeError(f"ffprobe 失败：{out[:300]}")
    return json.loads(out)


def duration(path: str) -> float:
    try:
        return float(probe(path)["format"]["duration"])
    except Exception:
        return 0.0


def has_audio(path: str) -> bool:
    try:
        return any(s.get("codec_type") == "audio" for s in probe(path)["streams"])
    except Exception:
        return False


def stitch(clips: list[str], out: str, fade: float = 0.2) -> None:
    """按 xfade + acrossfade 缝合；时长不足或段数=1 时退化为直接复制/拼接。"""
    if len(clips) == 1:
        run([FFMPEG, "-y", "-v", "error", "-i", clips[0], "-c", "copy", out])
        return

    durs = [duration(c) for c in clips]
    inputs, filters = [], []
    for c in clips:
        inputs += ["-i", c]

    # 视频链
    vcur = "[0:v]"
    offset = 0.0
    for i in range(1, len(clips)):
        offset += durs[i - 1] - fade
        label = f"[v{i}]"
        filters.append(f"{vcur}[{i}:v]xfade=transition=fade:duration={fade}:offset={offset:.3f}{label}")
        vcur = label

    # 音频链（任一段无音轨就跳过音频链，避免 acrossfade 失败）
    aout = ""
    if all(has_audio(c) for c in clips):
        acur = "[0:a]"
        for i in range(1, len(clips)):
            label = f"[a{i}]"
            filters.append(f"{acur}[{i}:a]acrossfade=d={fade}:c1=tri:c2=tri{label}")
            acur = label
        aout = acur

    args = [FFMPEG, "-y", "-v", "error"] + inputs + ["-filter_complex", ";".join(filters),
                                                     "-map", vcur]
    if aout:
        args += ["-map", aout, "-c:a", "aac", "-b:a", "192k"]
    args += ["-c:v", "libx264", "-preset", "medium", "-crf", "18",
             "-pix_fmt", "yuv420p", "-movflags", "+faststart", out]
    code, msg = run(args)
    if code != 0:
        raise RuntimeError(f"装配失败：{msg[-600:]}")


def main() -> int:
    ap = argparse.ArgumentParser(description="多段成片：定妆图锁角色 + 逐段出片 + 装配")
    ap.add_argument("--plan", required=True, help="film.json")
    ap.add_argument("--frames", type=int, default=0, help="每段帧数（17k+5）；默认取 plan.frames")
    ap.add_argument("--steps", type=int, default=12)
    ap.add_argument("--dry-run", action="store_true", help="只打印将要执行的命令")
    ap.add_argument("--skip-existing", action="store_true", help="已有分段产物则跳过生成")
    args = ap.parse_args()

    with open(args.plan, encoding="utf-8") as f:
        plan = json.load(f)

    default_frames = args.frames or int(plan.get("frames") or 243)
    if (default_frames - 5) % 17 != 0:
        print(json.dumps({"ok": False, "error": f"frames={default_frames} 不在 17k+5 网格"}, ensure_ascii=False))
        return 2

    # 每段可以有自己的时长：按内容节奏给，不要为了凑整而均分。
    # H3 网格 17k+5：73/90/107/124/141/158/175/192/209/226/243/260/277/294/311/328/345/362
    seg_frames = []
    for i, seg in enumerate(plan.get("segments") or []):
        f = int(seg.get("frames") or default_frames)
        if (f - 5) % 17 != 0:
            print(json.dumps({"ok": False,
                              "error": f"第 {seg.get('id', i+1)} 段 frames={f} 不在 17k+5 网格"},
                             ensure_ascii=False))
            return 2
        seg_frames.append(f)

    overlap = 0.2
    est_total = round(sum(seg_frames) / 24.0 - overlap * max(0, len(seg_frames) - 1), 2)
    target = float(plan.get("target_seconds") or 0)
    if target > 0 and abs(est_total - target) > 0.75:
        print(f"[warn] 预计总长 {est_total}s 与目标 {target}s 相差超过 0.75s"
              f"（段帧数 {seg_frames}）", flush=True)

    plan_dir = os.path.dirname(os.path.abspath(args.plan))
    out_dir = plan.get("out_dir") or os.path.join(plan_dir, "film")
    if not os.path.isabs(out_dir):
        out_dir = os.path.join(os.getcwd(), out_dir)
    os.makedirs(out_dir, exist_ok=True)
    # 相对路径按 plan 文件所在目录解析（不是 cwd）
    chars = []
    for raw in (plan.get("characters") or []):
        if not raw:
            continue
        chars.append(os.path.abspath(raw) if os.path.isabs(raw) else os.path.abspath(os.path.join(plan_dir, raw)))
    for c in chars:
        if not os.path.isfile(c):
            print(json.dumps({"ok": False, "error": f"定妆图不存在：{c}"}, ensure_ascii=False))
            return 2

    images_arg = ";".join(chars)
    segments = plan.get("segments") or []
    if not segments:
        print(json.dumps({"ok": False, "error": "plan.segments 为空"}, ensure_ascii=False))
        return 2

    clips: list[str] = []
    report = []

    for idx, seg in enumerate(segments):
        sid = seg.get("id") or (idx + 1)
        frames = seg_frames[idx]
        prompt_path = os.path.join(out_dir, f"seg{sid}.txt")
        with open(prompt_path, "w", encoding="utf-8") as f:
            f.write(seg.get("prompt", "").strip() + "\n")

        lines = [x for x in (seg.get("lines") or []) if str(x.get("text", "")).strip()]
        audio_path = ""
        if lines:
            audio_path = os.path.join(out_dir, f"seg{sid}.wav")
            lines_path = os.path.join(out_dir, f"seg{sid}-lines.json")
            with open(lines_path, "w", encoding="utf-8") as f:
                json.dump({"lines": lines}, f, ensure_ascii=False)
            lead = float(seg.get("lead") or 0.25)
            cmd = [PYEXE, os.path.join(HERE, "dub-track.py"),
                   "--lines-file", lines_path, "--out", audio_path,
                   "--frames", str(frames), "--fps", "24", "--lead", f"{lead:.2f}"]
            print(f"[seg{sid}] 合成念稿音轨 …", flush=True)
            if not args.dry_run:
                code, out = run(cmd, 900)
                if code != 0:
                    print(json.dumps({"ok": False, "error": f"seg{sid} 配音失败", "detail": out[-400:]},
                                     ensure_ascii=False))
                    return 2

        cmd = [PYEXE, os.path.join(HERE, "run-flowedge.py"),
               "--mode", "dialogue" if lines else "ambience",
               "--prompt-file", prompt_path,
               "--frames", str(frames), "--steps", str(args.steps),
               "--report-dir", out_dir]
        if lines:
            cmd += ["--audio", audio_path]
        if images_arg:
            cmd += ["--images", images_arg]

        print(f"[seg{sid}] 出片（{'台词' if lines else '氛围'} / {frames} 帧）…", flush=True)
        if args.dry_run:
            print("  " + " ".join(cmd), flush=True)
            continue

        t0 = time.time()
        code, out = run(cmd, 3600)
        if code != 0:
            print(json.dumps({"ok": False, "error": f"seg{sid} 出片失败", "detail": out[-600:]},
                             ensure_ascii=False))
            return 2
        try:
            res = json.loads(out.strip().splitlines()[-1])
        except Exception:
            print(json.dumps({"ok": False, "error": f"seg{sid} 输出无法解析", "raw": out[-300:]},
                             ensure_ascii=False))
            return 2
        clips.append(res["video"])
        report.append({"segment": sid, "mode": res.get("mode"), "video": res["video"],
                       "seconds": round(duration(res["video"]), 3),
                       "wall_seconds": round(time.time() - t0, 1),
                       "lines": [x.get("text") for x in lines]})

    if args.dry_run:
        print(json.dumps({"ok": True, "dry_run": True, "segments": len(segments)}, ensure_ascii=False))
        return 0

    out_video = os.path.join(out_dir, plan.get("out") or "film.mp4")
    print(f"[stitch] {len(clips)} 段 → {out_video}", flush=True)
    stitch(clips, out_video)

    # 装配后验收
    vcode, vout = run([PYEXE, os.path.join(HERE, "verify-media.py"), out_video,
                       "--asr", "--gate"], 900)

    summary = {
        "ok": vcode == 0,
        "title": plan.get("title", ""),
        "target_seconds": target or None,
        "estimated_seconds": est_total,
        "segment_frames": seg_frames,
        "video": os.path.abspath(out_video),
        "seconds": round(duration(out_video), 3),
        "segments": report,
        "verify_exit": vcode,
        "verify_tail": vout.strip().splitlines()[-6:],
    }
    with open(os.path.join(out_dir, "film-report.json"), "w", encoding="utf-8") as f:
        json.dump(summary, f, ensure_ascii=False, indent=2)
    print(json.dumps(summary, ensure_ascii=False))
    return 0 if vcode == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
