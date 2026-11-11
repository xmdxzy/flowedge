#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""shiprender.py — 1080p 确定性排版渲染器（Playwright 逐帧 + ffmpeg 管道）

做两件事，缺一不可：
  1. 按固定时间轴把 HTML/JS 模板渲染成 30fps 画面（无 CSS 动画，全部由 t 驱动）
  2. 每帧把 [data-reg] 元素的**变换后外接框**写成 boxes.jsonl，供 shipgate 五门吃

用法：
  python scripts/shiprender.py projects/ice-slip --out outputs/ice-slip
  python scripts/shiprender.py projects/ice-slip --box-every 0.25 --samples 8 --no-audio

产物（--out 目录）：
  silent.mp4  master.mp4  play.mp4  boxes.jsonl  frames/*.jpg  render.json
"""
from __future__ import annotations

import argparse
import json
import math
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

try:
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

FFMPEG = shutil.which("ffmpeg") or "ffmpeg"
DEFAULT_BGM = Path(os.environ["FLOWEDGE_BGM"]) if os.environ.get("FLOWEDGE_BGM") else None


def run(cmd: list[str], **kw) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace", **kw)


def start_static_server(root):
    """随机端口起静态服务器。模块化模板（importmap / three.js / fetch）在 file:// 下
    会被 CORS 拦死，必须走 http。"""
    import functools
    import threading
    from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

    handler = functools.partial(SimpleHTTPRequestHandler, directory=str(root))
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, f"http://127.0.0.1:{srv.server_address[1]}"


def timeline(shots: list[dict]) -> tuple[list[float], float]:
    starts, t = [], 0.0
    for i, s in enumerate(shots):
        starts.append(t)
        hold = float(s.get("hold", 0))
        t += hold - (float(s.get("overlap", 0)) if i < len(shots) - 1 else 0.0)
    total = starts[-1] + float(shots[-1].get("hold", 0))
    return starts, total


def main() -> int:
    ap = argparse.ArgumentParser(description="1080p 排版渲染 + boxes.jsonl")
    ap.add_argument("project", help="含 shots.json 与 index.html 的项目目录")
    ap.add_argument("--out", required=True)
    ap.add_argument("--fps", type=float, default=None)
    ap.add_argument("--crf", type=int, default=16)
    ap.add_argument("--grain", type=float, default=5.0, help="胶片颗粒强度，0=关")
    ap.add_argument("--box-every", type=float, default=0.25, help="boxes.jsonl 采样间隔（秒）")
    ap.add_argument("--samples", type=int, default=6, help="另存几张 jpg 供人眼复看")
    ap.add_argument("--bgm", default=str(DEFAULT_BGM) if DEFAULT_BGM else "")
    ap.add_argument("--lufs", type=float, default=-14.0)
    ap.add_argument("--no-audio", action="store_true")
    ap.add_argument("--no-play", action="store_true", help="不生成 play 版")
    ap.add_argument("--gl", choices=["gpu", "sw"], default="sw",
                    help="WebGL 后端：gpu=D3D11 硬件 / sw=SwiftShader 软件")
    ap.add_argument("--probe", default="", help="只在这些时间点出图（逗号分隔秒），不编码")
    args = ap.parse_args()

    proj = Path(args.project).resolve()
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    (out / "frames").mkdir(exist_ok=True)

    spec = json.loads((proj / "shots.json").read_text(encoding="utf-8-sig"))
    shots = spec["shots"]
    w = int(spec.get("canvas", {}).get("w", 1920))
    h = int(spec.get("canvas", {}).get("h", 1080))
    fps = float(args.fps or spec.get("canvas", {}).get("fps", 30))
    starts, total = timeline(shots)
    frames_n = int(round(total * fps))

    print(f"项目 {proj.name} · {w}×{h} @ {fps:g}fps · {len(shots)} 镜 · {total:.2f}s = {frames_n} 帧")
    if total > 180:
        print(f"FAIL 总时长 {total:.2f}s 超过 180s 硬上限")
        return 1

    try:
        from playwright.sync_api import sync_playwright
    except Exception as e:
        print(f"ERROR 缺 playwright：{e}")
        return 2

    silent = out / "silent.mp4"
    vf = [f"noise=alls={args.grain:g}:allf=t+u"] if args.grain > 0 else []
    vf.append("eq=contrast=1.03:saturation=1.04")
    enc = [FFMPEG, "-y", "-v", "error",
           "-f", "image2pipe", "-framerate", f"{fps:g}", "-c:v", "png", "-i", "-",
           "-vf", ",".join(vf),
           "-c:v", "libx264", "-preset", "medium", "-crf", str(args.crf),
           "-pix_fmt", "yuv420p", "-r", f"{fps:g}", str(silent)]
    proc = subprocess.Popen(enc, stdin=subprocess.PIPE, stderr=subprocess.PIPE)

    box_lines: list[dict] = []
    sample_at = {round(total * (i + 0.5) / args.samples, 3) for i in range(args.samples)} if args.samples else set()
    box_every = max(args.box_every, 1.0 / fps - 1e-6)
    next_box = 0.0
    t0 = time.time()

    srv, base_url = start_static_server(Path.cwd())
    page_url = f"{base_url}/{proj.relative_to(Path.cwd()).as_posix()}/index.html"
    gl_args = (["--enable-gpu", "--use-angle=d3d11", "--ignore-gpu-blocklist"]
               if getattr(args, "gl", "sw") == "gpu" else
               ["--use-gl=angle", "--use-angle=swiftshader", "--enable-unsafe-swiftshader"])
    if args.probe:
        probe_dir = out / "frames"
        probe_dir.mkdir(parents=True, exist_ok=True)
        with sync_playwright() as pw:
            browser = pw.chromium.launch(args=["--force-color-profile=srgb", "--hide-scrollbars"] + gl_args)
            page = browser.new_page(viewport={"width": w, "height": h}, device_scale_factor=1)
            page.goto(page_url)
            page.wait_for_function("() => typeof window.__init === 'function'", timeout=30000)
            page.evaluate("s => window.__init(s)", spec)
            probe_rows = []
            for raw in args.probe.split(","):
                tt = float(raw)
                rows = page.evaluate("t => window.__frame(t)", tt)
                page.screenshot(path=str(probe_dir / f"probe_{tt:06.2f}s.png"), animations="disabled")
                probe_rows.extend(rows)
                bad = [r for r in rows if r["kind"] == "text"
                       and (r["x0"] < 96 or r["x1"] > 1824 or r["y0"] < 80 or r["y1"] > 900)]
                print(f"  probe {tt:6.2f}s  框 {len(rows)} 个" + (f"  出安全区 {[b['group'] for b in bad]}" if bad else ""))
            browser.close()
        (Path(args.out) / "probe_boxes.jsonl").write_text(
            "\n".join(json.dumps(b, ensure_ascii=False) for b in probe_rows), encoding="utf-8")
        proc.kill()
        print(f"probe 图在 {probe_dir}")
        return 0

    with sync_playwright() as pw:
        browser = pw.chromium.launch(args=["--force-color-profile=srgb", "--hide-scrollbars"] + gl_args + [
                                          "--disable-lcd-text", "--font-render-hinting=none"])
        page = browser.new_page(viewport={"width": w, "height": h}, device_scale_factor=1)
        page.goto((proj / "index.html").as_uri())
        meta = page.evaluate("s => window.__init(s)", spec)
        if meta.get("cues"):
            (out / "cues.json").write_text(json.dumps(meta["cues"], ensure_ascii=False, indent=1),
                                           encoding="utf-8")
            print(f"cues.json  {len(meta['cues'])} 个音效事件")
        if int(meta["frames"]) != frames_n:
            print(f"WARN 模板算的帧数 {meta['frames']} ≠ 脚本算的 {frames_n}，以模板为准")
            frames_n = int(meta["frames"])
        for i in range(frames_n):
            t = i / fps
            rows = page.evaluate("t => window.__frame(t)", t)
            if t + 1e-9 >= next_box:
                box_lines.extend(rows)
                next_box += box_every
            png = page.screenshot(type="png", animations="disabled")
            proc.stdin.write(png)  # type: ignore[union-attr]
            for s in list(sample_at):
                if abs(t - s) < 0.5 / fps:
                    (out / "frames" / f"sample_{s:06.2f}s.png").write_bytes(png)
                    sample_at.discard(s)
            if i % 150 == 0:
                el = time.time() - t0
                print(f"  帧 {i}/{frames_n}  {el:5.1f}s  ≈{el/max(i,1)*1000:.0f}ms/帧", flush=True)
        browser.close()

    proc.stdin.close()  # type: ignore[union-attr]
    err = proc.stderr.read().decode("utf-8", "replace") if proc.stderr else ""
    if proc.wait() != 0:
        print(f"ERROR ffmpeg 编码失败：{err[-800:]}")
        return 2
    el = time.time() - t0
    print(f"静音母片 {silent.name}  {silent.stat().st_size/1048576:.1f}MB  渲染 {el:.1f}s")

    (out / "boxes.jsonl").write_text(
        "\n".join(json.dumps(b, ensure_ascii=False) for b in box_lines), encoding="utf-8")
    print(f"boxes.jsonl  {len(box_lines)} 框 / {len({round(b['t'],2) for b in box_lines})} 采样帧")

    master = out / "master.mp4"
    if args.no_audio or not args.bgm or not Path(args.bgm).exists():
        if os_replace(silent, master):
            print(f"无音频：{master.name} = 静音母片")
    else:
        fade = max(0.4, total - 1.4)
        af = (f"atrim=0:{total:.3f},asetpts=N/SR/TB,"
              f"loudnorm=I={args.lufs}:TP=-3:LRA=11,"
              f"afade=t=out:st={fade:.3f}:d=1.4,"
              f"alimiter=limit=0.708:level=false")   # −3 dBTP 硬顶（level=false 否则被抬回 0dB）
        cmd = [FFMPEG, "-y", "-v", "error", "-i", str(silent), "-i", args.bgm,
               "-filter_complex", f"[1:a]{af}[a]",
               "-map", "0:v", "-map", "[a]", "-c:v", "copy",
               "-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-movflags", "+faststart",
               str(master)]
        r = run(cmd)
        if r.returncode != 0:
            print(f"ERROR 混音失败：{r.stderr[-600:]}")
            return 2
        print(f"母带 {master.name}  {master.stat().st_size/1048576:.1f}MB  BGM={Path(args.bgm).name}")

    if not args.no_play and master.exists():
        play = out / "play.mp4"
        r = run([FFMPEG, "-y", "-v", "error", "-i", str(master),
                 "-c:v", "libx264", "-profile:v", "main", "-level", "4.0", "-pix_fmt", "yuv420p",
                 "-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709",
                 "-color_range", "tv", "-crf", "20", "-preset", "medium",
                 "-movflags", "+faststart",
                 "-af", "alimiter=limit=0.708:level=false", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "44100", str(play)])
        if r.returncode != 0:
            print(f"WARN play 版失败：{r.stderr[-400:]}")
        else:
            print(f"交付版 {play.name}  {play.stat().st_size/1048576:.1f}MB (Main/yuv420p/bt709/tv/+faststart)")

    (out / "render.json").write_text(json.dumps({
        "project": spec.get("project"), "title": spec.get("title"),
        "canvas": {"w": w, "h": h, "fps": fps}, "shots": len(shots),
        "starts": [round(s, 3) for s in starts], "total": round(total, 3),
        "frames": frames_n, "box_samples": len({round(b["t"], 2) for b in box_lines}),
        "bgm": None if args.no_audio else args.bgm, "lufs": args.lufs,
        "render_seconds": round(el, 1),
    }, ensure_ascii=False, indent=2), encoding="utf-8")
    return 0


def os_replace(src: Path, dst: Path) -> bool:
    try:
        src.replace(dst)
        return True
    except Exception:
        return False


if __name__ == "__main__":
    sys.exit(main())
