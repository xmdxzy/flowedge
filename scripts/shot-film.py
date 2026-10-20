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


def video_duration(path: str) -> float:
    """视频流自身时长。

    xfade 的 offset 必须按**视频流**算：容器时长可能比视频流长（AAC 编码有 priming
    delay，实测差 4 帧 = 0.08s）。按容器时长算出的 offset 会越过上一段视频流的终点，
    导致该次 xfade 直接落空、整段画面丢失——症状是成片帧数只剩一段，时长却正常。
    """
    try:
        info = probe(path)
        for s in info["streams"]:
            if s.get("codec_type") == "video" and s.get("duration"):
                return float(s["duration"])
        return float(info["format"]["duration"])
    except Exception:
        return 0.0


def has_audio(path: str) -> bool:
    try:
        return any(s.get("codec_type") == "audio" for s in probe(path)["streams"])
    except Exception:
        return False


def load_gates_module():
    """把 film-gates.py 当模块引入：门的判定逻辑只维护一份，避免两处漂移。"""
    import importlib.util
    path = os.path.join(HERE, "film-gates.py")
    spec = importlib.util.spec_from_file_location("film_gates", path)
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


def endpoint_alive(url: str, timeout: float = 2.0) -> bool:
    """探活本地端口。必须绕过 http_proxy，否则拿到 502 假死。"""
    import urllib.request
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(url, timeout=timeout) as r:
            return 200 <= r.status < 400
    except Exception:
        return False


def segment_gate(path: str, mode: str, expect: str, want_asr: bool,
                 max_silence: float = 0.0) -> tuple[bool, str]:
    """按门验**单段**：技术项 + ASR 策略。

    为什么要在每段后验、而不是等全片：一段不合门，越早发现越省时间——
    以前是全片跑完 20 分钟才在结尾发现台词念错，只能整批重来。
    """
    cmd = [PYEXE, os.path.join(HERE, "verify-media.py"), path, "--gate"]
    if max_silence > 0:
        # 静音缺口就是「声音断一下」：门给了上限就必须当硬失败，不能只告警
        cmd += ["--max-silence", f"{max_silence:g}"]
    if want_asr:
        cmd.append("--asr")
        if mode == "dialogue" and expect:
            cmd += ["--expect", expect]
        elif mode != "dialogue":
            cmd.append("--no-speech")
    code, out = run(cmd, 600)
    hits = [ln.strip() for ln in out.splitlines()
            if ln.strip().startswith(("[FAIL]", "[WARN]"))]
    return code == 0, ("；".join(hits[:2]) if hits else "通过")


def stitch(clips: list[str], out: str, fade: float = 0.2) -> None:
    """按 xfade + acrossfade 缝合；时长不足或段数=1 时退化为直接复制/拼接。"""
    if len(clips) == 1:
        run([FFMPEG, "-y", "-v", "error", "-i", clips[0], "-c", "copy", out])
        return

    durs = [video_duration(c) for c in clips]
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


def mean_luma(path: str, at_ratio: float = 0.5) -> float:
    """取片段中间那一帧的平均亮度（YAVG）。

    跨段光比是"重拍某一镜"最容易毁掉的一致性：定妆图锁住了角色，
    但锁不住时间感——实测重拍后段 2 从雪夜变成灰白阴天（68 → 107），
    肉眼一抽帧就看出来，可 verify-media 一个字都不会报。
    """
    d = duration(path)
    t = max(0.05, d * at_ratio)
    code, out = run([FFMPEG, "-hide_banner", "-ss", f"{t:.2f}", "-i", path,
                     "-frames:v", "1", "-vf", "signalstats,metadata=mode=print:key=lavfi.signalstats.YAVG",
                     "-f", "null", "-"], 120)
    for ln in out.splitlines():
        if "yavg" in ln.lower():
            try:
                return float(ln.rsplit("=", 1)[-1])
            except ValueError:
                continue
    return 0.0


def exposure_spread(clips: list[str]) -> dict:
    """装配前测各段平均亮度，给出极差。极差过大说明同一场戏的时间感不一致。"""
    vals = [round(mean_luma(c), 1) for c in clips]
    if not vals or min(vals) <= 0:
        return {"segment_yavg": vals}
    spread = round(max(vals) - min(vals), 1)
    return {"segment_yavg": vals, "spread": spread, "warn": spread > 25}


def main() -> int:
    ap = argparse.ArgumentParser(description="多段成片：定妆图锁角色 + 逐段出片 + 装配")
    ap.add_argument("--plan", required=True, help="film.json")
    ap.add_argument("--frames", type=int, default=0, help="每段帧数（17k+5）；默认取 plan.frames")
    ap.add_argument("--steps", type=int, default=0,
                    help="采样步数；0 = 取 plan.steps（再缺省 12）")
    ap.add_argument("--dry-run", action="store_true", help="只打印将要执行的命令")
    ap.add_argument("--skip-existing", action="store_true", help="已有分段产物则跳过生成")
    ap.add_argument("--ambience", default="",
                    help="环境底噪 wav，混进每段念稿音轨（fully_copy 下这是唯一能加环境声的时机）")
    ap.add_argument("--ambience-gain", type=float, default=0.55, help="环境声增益（默认 0.55）")
    ap.add_argument("--only", default="",
                    help="只重跑这些段（逗号分隔），其余段沿用 film-report.json 里的既有产物。"
                         "改某一段的提示词时用，不必为一段重拍整片")
    ap.add_argument("--no-stitch", action="store_true",
                    help="只生成分段不装配（配合 --only 重跑后统一换轨/装配时用）")
    ap.add_argument("--no-gates", action="store_true",
                    help="跳过控制门（仅调试用；正常任务必须先设计门）")
    args = ap.parse_args()

    with open(args.plan, encoding="utf-8") as f:
        plan = json.load(f)

    # 参数优先级：命令行 > plan > 默认。图里不要传空串（argparse 会直接报错）
    steps = int(args.steps or plan.get("steps") or 12)

    # ---- 控制门：开机前先设计、先校验 ----
    # 「每镜多少秒」不由脚本规定，而由本条片子的 gates 声明；这里只负责拦住不自洽的门。
    gates_mod = load_gates_module()
    if args.frames:
        for seg in (plan.get("segments") or []):
            seg["frames"] = args.frames
    gverdict, gates, gstats = gates_mod.check_plan(plan, require_gates=not args.no_gates)
    print(f"[gates] {gverdict.worst()}  段帧数 {gstats.get('frames')}  "
          f"总长 {gstats.get('total_seconds')}s  镜头 {gstats.get('shots')}", flush=True)
    for it in gverdict.items:
        if it["level"] in ("FAIL", "WARN"):
            print(f"[gates]   {it['level']} {it['gate']}: {it['msg']}", flush=True)
    if not gverdict.ok():
        print(json.dumps({"ok": False, "error": "控制门未通过，未开机",
                          "gates": gverdict.items}, ensure_ascii=False))
        return 2

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

    # 只重跑指定段时，其余段的产物来自上一次的报告（省掉为一段改动重拍整片）
    only = {int(x) for x in re.split(r"[,，]", args.only) if x.strip().isdigit()}
    prev_segments: dict[int, dict] = {}
    if only:
        prev_path = os.path.join(out_dir, "film-report.json")
        if os.path.isfile(prev_path):
            with open(prev_path, encoding="utf-8") as f:
                for r in (json.load(f).get("segments") or []):
                    prev_segments[int(r.get("segment") or 0)] = r
            print(f"[only] 重跑 {sorted(only)}；沿用 {sorted(set(prev_segments) - only)}", flush=True)
        else:
            print("[only] 找不到上一份 film-report.json，全部段都会重跑", flush=True)

    # 段级 ASR 门需要本地 ASR；不可达时降级为"只验技术项"并明说，不能静默当作通过
    audio_gate = gates.get("audio") or {}
    asr_url = os.environ.get("ASR_ENDPOINT", "http://127.0.0.1:8101") + "/health"
    want_asr = endpoint_alive(asr_url)
    if not want_asr:
        print("[gates] WARN ASR 不可达：段级只验技术项，台词政策无法核对", flush=True)
    retry_max = int(audio_gate.get("segment_retry", 1) or 0)
    on_fail = str(audio_gate.get("on_segment_fail") or "retry")
    max_silence = float(audio_gate.get("max_silence_gap") or 0)
    per_seg_cap = int((gates.get("budget") or {}).get("per_segment_wall_max") or 0)

    for idx, seg in enumerate(segments):
        sid = seg.get("id") or (idx + 1)
        if only and sid not in only:
            rec = prev_segments.get(sid)
            if rec and rec.get("video") and os.path.isfile(rec["video"]):
                print(f"[seg{sid}] 沿用 {os.path.basename(rec['video'])}", flush=True)
                clips.append(rec["video"])
                report.append(rec)
                continue
            print(f"[seg{sid}] 无可沿用产物，按计划重跑", flush=True)
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
            # 环境声：段级 > plan 级 > 命令行；相对路径按 plan 目录解析
            amb = seg.get("ambience") or plan.get("ambience") or args.ambience
            if amb:
                amb_path = amb if os.path.isabs(amb) else os.path.abspath(os.path.join(plan_dir, amb))
                if os.path.isfile(amb_path):
                    gain = float(seg.get("ambience_gain") or args.ambience_gain)
                    cmd += ["--ambience", amb_path, "--ambience-gain", f"{gain:.2f}"]
                else:
                    print(f"[seg{sid}] WARN 环境声不存在，跳过：{amb_path}", flush=True)
            print(f"[seg{sid}] 合成念稿音轨 …", flush=True)
            if not args.dry_run:
                code, out = run(cmd, 900)
                if code != 0:
                    print(json.dumps({"ok": False, "error": f"seg{sid} 配音失败", "detail": out[-400:]},
                                     ensure_ascii=False))
                    return 2
                # 首句实际起点必须贴着计划时点：TTS 自带前导静音（实测 0.44s），
                # 不校验的话每句都比画面晚开口，而 verify-media 的 av_drift 看不出来
                try:
                    dub = json.loads(out.strip().splitlines()[-1])
                    lines_out = dub.get("lines") or []
                    if lines_out:
                        first = float(lines_out[0].get("start") or 0)
                        if abs(first - lead) > 0.12:
                            print(f"[seg{sid}] WARN 首句实际起点 {first:.2f}s ↔ 计划 {lead:.2f}s"
                                  f"（偏差 {abs(first - lead):.2f}s，音画会错位）", flush=True)
                except Exception:
                    pass

        mode = "dialogue" if lines else "ambience"
        cmd = [PYEXE, os.path.join(HERE, "run-flowedge.py"),
               "--mode", mode,
               "--prompt-file", prompt_path,
               "--frames", str(frames), "--steps", str(steps),
               "--report-dir", out_dir]
        if lines:
            cmd += ["--audio", audio_path]
        if images_arg:
            cmd += ["--images", images_arg]

        print(f"[seg{sid}] 出片（{'台词' if lines else '氛围'} / {frames} 帧）…", flush=True)
        if args.dry_run:
            print("  " + " ".join(cmd), flush=True)
            continue

        expect_line = str(lines[0].get("text") or "").strip() if lines else ""
        attempt, video, wall, seg_note = 0, "", 0.0, ""
        while True:
            attempt += 1
            t0 = time.time()
            code, out = run(cmd, 3600)
            wall = round(time.time() - t0, 1)
            if code != 0:
                print(json.dumps({"ok": False, "error": f"seg{sid} 出片失败",
                                  "detail": out[-600:]}, ensure_ascii=False))
                return 2
            try:
                res = json.loads(out.strip().splitlines()[-1])
            except Exception:
                print(json.dumps({"ok": False, "error": f"seg{sid} 输出无法解析",
                                  "raw": out[-300:]}, ensure_ascii=False))
                return 2
            video = res["video"]

            # 段级门：早失败才省时间（否则整片跑完 20 分钟才发现某段念错）
            passed, note = segment_gate(video, mode, expect_line, want_asr, max_silence)
            seg_note = note
            print(f"[seg{sid}] 段级门 {'通过' if passed else '未过'}"
                  f"{'' if passed else '：' + note}", flush=True)
            if passed or on_fail != "retry" or attempt > retry_max:
                break
            print(f"[seg{sid}] 第 {attempt} 次未过，重跑（采样有随机性）…", flush=True)

        if not passed:
            print(json.dumps({"ok": False, "error": f"seg{sid} 段级门未通过",
                              "note": seg_note, "attempts": attempt,
                              "video": video}, ensure_ascii=False))
            return 2

        if per_seg_cap and wall > per_seg_cap:
            print(f"[gates] WARN 预算 seg{sid} 用了 {wall}s，超过单段上限 {per_seg_cap}s",
                  flush=True)

        clips.append(video)
        report.append({"segment": sid, "beat": seg.get("beat"), "mode": mode,
                       "video": video,
                       "seconds": round(duration(video), 3),
                       "wall_seconds": wall,
                       "attempts": attempt,
                       "gate": seg_note,
                       "lines": [x.get("text") for x in lines]})

    if args.dry_run:
        print(json.dumps({"ok": True, "dry_run": True, "segments": len(segments)}, ensure_ascii=False))
        return 0

    out_video = os.path.join(out_dir, plan.get("out") or "film.mp4")
    if args.no_stitch:
        summary = {"ok": True, "no_stitch": True, "title": plan.get("title", ""),
                   "segment_frames": seg_frames, "segments": report}
        with open(os.path.join(out_dir, "film-report.json"), "w", encoding="utf-8") as f:
            json.dump(summary, f, ensure_ascii=False, indent=2)
        print(json.dumps(summary, ensure_ascii=False))
        return 0
    exposure = exposure_spread(clips)
    if exposure.get("warn"):
        print(f"[gates] WARN 跨段光比极差 {exposure['spread']}（各段 AvgY {exposure['segment_yavg']}）："
              f"同一场戏的时间感不一致，先调色统一再装配，或重拍该段", flush=True)
    print(f"[stitch] {len(clips)} 段 → {out_video}", flush=True)
    stitch(clips, out_video)

    # 装配后验收
    verify_cmd = [PYEXE, os.path.join(HERE, "verify-media.py"), out_video,
                  "--asr", "--gate"]
    if max_silence > 0:
        verify_cmd += ["--max-silence", f"{max_silence:g}"]
    vcode, vout = run(verify_cmd, 900)

    total_wall = round(sum(r["wall_seconds"] for r in report), 1)
    budget = gates.get("budget") or {}
    cap = int(budget.get("wall_seconds_max") or 0)
    budget_note = ""
    if cap and total_wall > cap:
        budget_note = f"总墙钟 {total_wall}s 超过预算 {cap}s"
        lvl = "FAIL" if budget.get("on_exceed") == "fail" else "WARN"
        print(f"[gates] {lvl} {budget_note}", flush=True)
        if lvl == "FAIL":
            vcode = vcode or 2

    summary = {
        "ok": vcode == 0,
        "title": plan.get("title", ""),
        "gates": {
            "profile": gates.get("profile"),
            "preflight": gverdict.worst(),
            "items": gverdict.items,
            "stats": gstats,
            "budget_note": budget_note,
        },
        "total_wall_seconds": total_wall,
        "target_seconds": target or None,
        "estimated_seconds": est_total,
        "segment_frames": seg_frames,
        "exposure": exposure,
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
