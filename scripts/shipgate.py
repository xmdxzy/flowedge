#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""shipgate.py — 出片版式/节奏/文案门禁（2026-11-07 重建）

原 grokbot 盒子上的 kepu-qa.py / kadr-frame-gate 不在本机。本脚本用 ffmpeg+opencv+PIL
重写其中**可机械判定**的部分，判据照 docs/出片接续/00-接续总纲.md：

  budget  shots.json → 每镜阅读预算是否够（hold >= event+0.5+字数/4+1.0 且 >= event+1.8+1.5）
  copy    shots.json → 文案机器闸（元标签/句号/BIG>12/钩子>8/全半角混排）
  boxes   boxes.jsonl → 五门 collision / clip / crop / empty / sparseMid
  dead    视频 → 死画面（>5s 无新信息 = FAIL）+ idle 占比（>=35% WARN）
  luma    视频 → 每秒 p99.5 亮度（聚光灯：英雄镜需 ≥200）
  sheet   视频 → 4×3 接触表 JPEG（高 ≤2400）
  strip   视频 → 接缝前后连续帧条带
  selftest 造已知坏样本，证明门会 FAIL（门必须能在坏片上红）
  all     一次跑 dead+luma+sheet

退出码：0 PASS（可含 WARN）· 1 FAIL · 2 用法/环境错误。
只做机械判定，不替代人眼通看与静音复述。
"""
from __future__ import annotations

import argparse
import json
import math
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

try:
    sys.stdout.reconfigure(encoding="utf-8")
    sys.stderr.reconfigure(encoding="utf-8")
except Exception:
    pass

# ---------- 主题硬数值（照 00-接续总纲） ----------
SAFE = (96, 80, 1824, 900)        # x0,y0,x1,y1 文字可落区（y>900 禁大字）
TITLE_MAX_Y = 720                  # 标题/论点只到 720
PLATE_ALPHA = 0.72
MIN_AREA = 14000                   # empty 门：字幕期间主视觉面积下限 px²
CLIP_PAD = 8                       # clip 门：字离父框 ≥8px
COLLIDE_TOL = 2                    # collision 容差
DEAD_SEC = 5.0                     # 无新信息上限
IDLE_WARN = 0.35                   # idle 占比告警线
MAX_LEN = 180.0                    # 成片硬上限

BAN_TAGS = [
    "钩：", "钩:", "题面：", "约束：", "用途：", "命名：", "确认：", "图：", "来源：",
    "承接：", "数据：", "预测：", "否证：", "中间因果：", "结论：", "新用途起：",
    "局限：", "记住：", "BIG", "SUB", "镜号", "分镜", "旁白：", "音效：",
]

FFMPEG = shutil.which("ffmpeg") or "ffmpeg"
FFPROBE = shutil.which("ffprobe") or "ffprobe"


def fail(msg: str) -> "NoReturn":  # type: ignore[valid-type]
    print(f"[shipgate] ERROR {msg}")
    sys.exit(2)


def run(cmd: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace")


def probe(path: Path) -> dict:
    r = run([FFPROBE, "-v", "error", "-select_streams", "v:0", "-count_frames",
             "-show_entries", "stream=width,height,r_frame_rate,nb_read_frames,duration",
             "-show_entries", "format=duration", "-of", "json", str(path)])
    if r.returncode != 0:
        fail(f"ffprobe 失败：{r.stderr.strip()[:300]}")
    d = json.loads(r.stdout or "{}")
    st = (d.get("streams") or [{}])[0]
    num, _, den = (st.get("r_frame_rate") or "30/1").partition("/")
    fps = float(num) / float(den or 1) if float(den or 1) else 30.0
    return {
        "width": int(st.get("width") or 0),
        "height": int(st.get("height") or 0),
        "fps": fps,
        "frames": int(st.get("nb_read_frames") or 0),
        "duration": float(d.get("format", {}).get("duration") or st.get("duration") or 0),
    }


# ============================ 文案 / 预算 ============================

def char_weight(text: str) -> int:
    """阅读预算的字数：CJK 逐字 1；连续拉丁/数字串整体计 2。"""
    n, i, s = 0, 0, text or ""
    while i < len(s):
        ch = s[i]
        if ch.isascii() and ch.isalnum():
            j = i
            while j < len(s) and s[j].isascii() and s[j].isalnum():
                j += 1
            n += 2
            i = j
        else:
            if not ch.isspace():
                n += 1
            i += 1
    return n


def load_shots(path: Path) -> list[dict]:
    data = json.loads(path.read_text(encoding="utf-8"))
    shots = data.get("shots") if isinstance(data, dict) else data
    if not isinstance(shots, list) or not shots:
        fail("shots.json 需要 {\"shots\":[...]} 或裸数组，且非空")
    return shots


def cmd_budget(args) -> int:
    shots = load_shots(Path(args.shots))
    rows, fails, warns = [], [], []
    total = 0.0
    for idx, s in enumerate(shots, 1):
        sid = str(s.get("id", idx))
        event = float(s.get("event", 0) or 0)
        hold = float(s.get("hold", 0) or 0)
        chars = char_weight(str(s.get("big", ""))) + char_weight(str(s.get("sub", "")))
        need1 = event + 0.5 + chars / 4.0 + 1.0
        need2 = event + 0.8 + 1.5
        need = max(need1, need2)
        overlap = float(s.get("overlap", 0) or 0)
        total += hold - overlap
        ok = hold + 1e-6 >= need
        rows.append((sid, event, chars, round(need, 2), round(hold, 2), "PASS" if ok else "FAIL"))
        if not ok:
            fails.append(f"shot {sid}: hold {hold:.2f}s < 需要 {need:.2f}s（event {event:.2f} + 字 {chars}）")
        if hold > 6.5:
            warns.append(f"shot {sid}: hold {hold:.2f}s > 6.5s 上限（横版可协商，需说明）")
    print(f"{'shot':>5} {'event':>6} {'chars':>5} {'need':>6} {'hold':>6}  verdict")
    for r in rows:
        print(f"{r[0]:>5} {r[1]:>6.2f} {r[2]:>5} {r[3]:>6.2f} {r[4]:>6.2f}  {r[5]}")
    print(f"\n合计时长（扣接缝重叠）: {total:.2f}s   镜数 {len(shots)}")
    if total > MAX_LEN:
        fails.append(f"总时长 {total:.2f}s 超过硬上限 {MAX_LEN:.0f}s")
    if len(shots) >= 2:
        half = len(shots) // 2
        a = sum(float(s.get("hold", 0) or 0) for s in shots[:half]) / max(1, half)
        b = sum(float(s.get("hold", 0) or 0) for s in shots[half:]) / max(1, len(shots) - half)
        if a and b and abs(a - b) / max(a, b) > 0.20:
            warns.append(f"前后半平均镜长差 {abs(a-b)/max(a,b)*100:.0f}% > 20%（{a:.2f}s vs {b:.2f}s）")
    for w in warns:
        print(f"WARN  {w}")
    for f in fails:
        print(f"FAIL  {f}")
    print("GATE=" + ("FAIL" if fails else "PASS"))
    if args.json:
        print(json.dumps({"gate": "budget", "fail": fails, "warn": warns,
                          "total": round(total, 2), "shots": len(shots)}, ensure_ascii=False))
    return 1 if fails else 0


def cmd_copy(args) -> int:
    shots = load_shots(Path(args.shots))
    fails, warns = [], []
    for idx, s in enumerate(shots, 1):
        sid = str(s.get("id", idx))
        for field in ("big", "sub", "tag"):
            txt = str(s.get(field, "") or "")
            if not txt:
                continue
            for tag in BAN_TAGS:
                if tag in txt:
                    fails.append(f"shot {sid}.{field}: 出现制作元标签「{tag}」→ 上屏即违规")
            if field in ("big", "sub") and ("。" in txt or "，" == txt.strip()):
                fails.append(f"shot {sid}.{field}: 屏显禁句号 → 「{txt}」")
            if re.search(r"[，。]{2,}", txt):
                fails.append(f"shot {sid}.{field}: 叠用标点 → 「{txt}」")
            if field == "big":
                w = char_weight(txt)
                if w > 12:
                    fails.append(f"shot {sid}.big: {w} 字 > 12 → 拆成「展示镜 + 意味着什么镜」")
                if not re.search(r"[\u4e00-\u9fff]", txt) and len(txt.strip()) > 24:
                    warns.append(f"shot {sid}.big: 无中文但 {len(txt)} 字符，确认手机可读")
        if s.get("hook"):
            w = char_weight(str(s.get("big", "")))
            if w > 8:
                fails.append(f"shot {sid} 是钩子镜但 BIG {w} 字 > 8")
    for w in warns:
        print(f"WARN  {w}")
    for f in fails:
        print(f"FAIL  {f}")
    print("GATE=" + ("FAIL" if fails else "PASS"))
    if args.json:
        print(json.dumps({"gate": "copy", "fail": fails, "warn": warns}, ensure_ascii=False))
    return 1 if fails else 0


# ============================ 五门（版式） ============================

def _rect(b: dict) -> tuple[float, float, float, float]:
    return (float(b["x0"]), float(b["y0"]), float(b["x1"]), float(b["y1"]))


def _overlap(a, b, tol=0.0) -> float:
    w = min(a[2], b[2]) - max(a[0], b[0]) - tol
    h = min(a[3], b[3]) - max(a[1], b[1]) - tol
    return w * h if w > 0 and h > 0 else 0.0


def load_boxes(path: Path) -> list[dict]:
    out = []
    for ln, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        try:
            out.append(json.loads(line))
        except json.JSONDecodeError as e:
            fail(f"boxes.jsonl 第 {ln} 行不是合法 JSON：{e}")
    if not out:
        fail("boxes.jsonl 为空")
    return out


def cmd_boxes(args) -> int:
    """五门：collision / clip / crop / empty / sparseMid（按 0.25s 采样帧 + 每镜出镜帧）。"""
    boxes = load_boxes(Path(args.boxes))
    if getattr(args, "safe", None):
        sx0, sy0, sx1, sy1 = (float(v) for v in args.safe.split(","))
    else:
        sx0, sy0, sx1, sy1 = SAFE
    tmax = float(getattr(args, "title_max_y", TITLE_MAX_Y) or TITLE_MAX_Y)
    by_t: dict[float, list[dict]] = {}
    for b in boxes:
        b.setdefault("kind", "text")
        b.setdefault("group", str(b.get("id", id(b))))
        b.setdefault("alpha", 1.0)
        by_t.setdefault(round(float(b.get("t", 0)), 3), []).append(b)

    v_coll, v_clip, v_crop, v_empty, v_sparse = [], [], [], [], []
    for t, items in sorted(by_t.items()):
        texts = [b for b in items if b["kind"] == "text" and float(b.get("alpha", 1)) > args.min_alpha]
        # collision：字与字两两互检，只有「字↔自己的底板」豁免
        for i in range(len(texts)):
            for j in range(i + 1, len(texts)):
                a, c = texts[i], texts[j]
                if a.get("parent") and a["parent"] in (c.get("id"), c.get("group")):
                    continue
                if c.get("parent") and c["parent"] in (a.get("id"), a.get("group")):
                    continue
                if _overlap(_rect(a), _rect(c), COLLIDE_TOL) > 0:
                    v_coll.append(f"t={t}s {a['group']} × {c['group']} 文字重叠")
        # clip：带 parent 的字离父框四边 ≥8px
        for b in texts:
            pid = b.get("parent")
            if not pid:
                continue
            par = next((p for p in items if p.get("id") == pid or p.get("group") == pid), None)
            if par is None:
                v_clip.append(f"t={t}s {b['group']} 的父框 {pid} 不在同帧登记里")
                continue
            bx, p = _rect(b), _rect(par)
            gaps = (bx[0] - p[0], bx[1] - p[1], p[2] - bx[2], p[3] - bx[3])
            if min(gaps) < CLIP_PAD:
                v_clip.append(f"t={t}s {b['group']} 离父框最小边距 {min(gaps):.1f}px < {CLIP_PAD}px")
        # crop：text/plate 必须落在安全区；大字（≥72px）不许压到 y>720
        for b in items:
            if b["kind"] not in ("text", "plate"):
                continue
            r = _rect(b)
            if r[0] < sx0 or r[1] < sy0 or r[2] > sx1 or r[3] > sy1:
                v_crop.append(f"t={t}s {b['group']}({b['kind']}) 越安全区 "
                              f"({r[0]:.0f},{r[1]:.0f})-({r[2]:.0f},{r[3]:.0f})")
            elif (b["kind"] == "text" and float(b.get("size", 0)) >= 72
                  and r[3] > tmax and b.get("role") != "subtitle"):
                v_crop.append(f"t={t}s {b['group']} 是 {b.get('size'):.0f}px 大字却压到 "
                              f"y>{tmax:.0f}（{r[3]:.0f}）")
        # empty / sparseMid：字幕显示期间
        subs = [b for b in items if b.get("role") == "subtitle"]
        if subs:
            vis_area = sum(_overlap(_rect(b), (0, 0, 1920, TITLE_MAX_Y))
                           for b in items if b["kind"] in ("viz", "plate"))
            if vis_area < MIN_AREA:
                v_empty.append(f"t={t}s 字幕期间主视觉面积 {vis_area:.0f}px² < {MIN_AREA}")
            others = [b for b in texts if b.get("role") != "subtitle"]
            if len(others) < 2:
                v_sparse.append(f"t={t}s 字幕期间除字幕外文字块只有 {len(others)} 个（需 ≥2）")

    gates = {
        "collision": v_coll,
        "clip": v_clip,
        "crop": v_crop,
        "empty": v_empty,
        "sparseMid": v_sparse,
    }
    print(f"采样帧 {len(by_t)} 个（t = {', '.join(f'{t:.2f}' for t in sorted(by_t)[:8])}"
          f"{' …' if len(by_t) > 8 else ''}）\n")
    bad = 0
    for name, viol in gates.items():
        print(f"--- {name} ---  违规 {len(viol)}")
        for v in viol[:20]:
            print("   " + v)
        if len(viol) > 20:
            print(f"   … 另有 {len(viol) - 20} 条")
        print("GATE=" + ("FAIL" if viol else "PASS"))
        bad += 1 if viol else 0
    print(f"\n汇总：{'FAIL' if bad else 'PASS'}（{bad}/5 门未过）")
    if args.json:
        print(json.dumps({"gate": "boxes", "violations": {k: len(v) for k, v in gates.items()},
                          "pass": bad == 0}, ensure_ascii=False))
    return 1 if bad else 0


# ============================ 视频门 ============================

def _frames(path: Path, fps: float, w: int, h: int):
    import cv2  # 延迟导入：纯文案门不需要 cv2
    cap = cv2.VideoCapture(str(path))
    if not cap.isOpened():
        fail(f"打不开视频：{path}")
    src_fps = cap.get(cv2.CAP_PROP_FPS) or 30.0
    step = max(1, int(round(src_fps / fps)))
    idx = 0
    while True:
        ok, frame = cap.read()
        if not ok:
            break
        if idx % step == 0:
            import numpy as np
            yield idx / src_fps, cv2.resize(frame, (w, h), interpolation=cv2.INTER_AREA)
        idx += 1
    cap.release()


def cmd_dead(args) -> int:
    import numpy as np
    path = Path(args.video)
    prev, idle, runs, cur = None, 0, [], None
    total = 0
    for t, small in _frames(path, args.fps, 48, 27):
        gray = small.mean(axis=2)
        total += 1
        if prev is not None:
            diff = float(np.abs(gray - prev).mean())
            quiet = diff < args.thresh
            if quiet:
                idle += 1
                cur = (cur or t)
            else:
                if cur is not None and t - cur >= DEAD_SEC:
                    runs.append((round(cur, 2), round(t, 2), round(t - cur, 2)))
                cur = None
        prev = gray
    last_t = t if total else 0.0
    if cur is not None and last_t - cur >= DEAD_SEC:
        runs.append((round(cur, 2), round(last_t, 2), round(last_t - cur, 2)))
    ratio = idle / max(1, total - 1)
    print(f"采样 {total} 帧（{args.fps}fps, 阈值 diff<{args.thresh}）· idle 占比 {ratio*100:.1f}%")
    for a, b, d in runs:
        print(f"FAIL  死画面 {d:.2f}s @ {a:.2f}–{b:.2f}s（> {DEAD_SEC:.0f}s 无新信息）")
    if ratio >= IDLE_WARN:
        print(f"WARN  idle {ratio*100:.0f}% ≥ {IDLE_WARN*100:.0f}%：高峰若只是字变画不动，算假峰")
    print("GATE=" + ("FAIL" if runs else "PASS"))
    if args.json:
        print(json.dumps({"gate": "dead", "runs": runs, "idle_ratio": round(ratio, 3)},
                         ensure_ascii=False))
    return 1 if runs else 0


def cmd_luma(args) -> int:
    import numpy as np
    path = Path(args.video)
    buckets: dict[int, list[float]] = {}
    for t, frame in _frames(path, 4.0, 480, 270):
        g = frame.mean(axis=2)
        buckets.setdefault(int(t), []).append((float(np.percentile(g, 99.5)), float(np.percentile(g, 99.95))))
    print(f"{'sec':>4}  p99.5")
    dark = []
    for sec in sorted(buckets):
        p = max(b[0] for b in buckets[sec]); q = max(b[1] for b in buckets[sec])
        print(f"{sec:>4}  {p:6.1f}  {q:6.1f}{'   ← p99.5 无全亮' if p < 200 else ''}")
        if p < 200:
            dark.append(sec)
    print(f"\n{len(dark)}/{len(buckets)} 秒没有 ≥200 的高亮元素（聚光灯闸：英雄镜必须每秒有）")
    print("GATE=" + ("WARN" if dark else "PASS"))
    if args.json:
        print(json.dumps({"gate": "luma", "dark_seconds": dark}, ensure_ascii=False))
    return 0 if not args.require else (1 if dark else 0)


def cmd_sheet(args) -> int:
    from PIL import Image, ImageDraw
    path = Path(args.video)
    info = probe(path)
    n = args.cols * args.rows
    dur = info["duration"]
    tmp = Path(args.out or (path.parent / f"{path.stem}_contact.jpg"))
    cell_w = 480
    cell_h = max(1, round(cell_w * info["height"] / info["width"])) if info["width"] else 270
    sheet = Image.new("RGB", (cell_w * args.cols, cell_h * args.rows), (8, 8, 15))
    draw = ImageDraw.Draw(sheet)
    times = [dur * (i + 0.5) / n for i in range(n)]
    for i, t in enumerate(times):
        f = tmp.with_name(f"_sg_{i}.jpg")
        r = run([FFMPEG, "-y", "-v", "error", "-ss", f"{t:.3f}", "-i", str(path),
                 "-frames:v", "1", "-vf", f"scale={cell_w}:-2", str(f)])
        if r.returncode != 0 or not f.exists():
            continue
        img = Image.open(f).convert("RGB")
        x, y = (i % args.cols) * cell_w, (i // args.cols) * cell_h
        sheet.paste(img, (x, y))
        draw.text((x + 8, y + 6), f"{t:.2f}s", fill=(0, 229, 255))
        draw.text((x + 8, y + cell_h - 20), f"#{i+1}", fill=(180, 190, 210))
        f.unlink(missing_ok=True)
    if sheet.height > 2400:
        ratio = 2400 / sheet.height
        sheet = sheet.resize((int(sheet.width * ratio), 2400), Image.LANCZOS)
    sheet.save(tmp, "JPEG", quality=82)
    print(f"接触表 {tmp}  {sheet.width}×{sheet.height}  {tmp.stat().st_size/1024:.0f}KB")
    print("GATE=PASS")
    return 0


def cmd_strip(args) -> int:
    from PIL import Image
    path = Path(args.video)
    info = probe(path)
    fps = info["fps"] or 30.0
    back, fwd = args.frames, args.frames
    out = Path(args.out or (path.parent / f"{path.stem}_strip_{args.t:g}s.png"))
    tiles = []
    for k in range(-back, fwd + 1):
        t = args.t + k / fps
        if t < 0 or t > info["duration"]:
            continue
        f = out.with_name(f"_ss_{k+back}.jpg")
        r = run([FFMPEG, "-y", "-v", "error", "-ss", f"{t:.4f}", "-i", str(path),
                 "-frames:v", "1", "-vf", "scale=320:-2", str(f)])
        if r.returncode == 0 and f.exists():
            tiles.append(Image.open(f).convert("RGB"))
            f.unlink(missing_ok=True)
    if not tiles:
        fail("条带一帧都没抽到（时间点越界？）")
    w = sum(t.width for t in tiles)
    h = max(t.height for t in tiles)
    strip = Image.new("RGB", (w, h), (0, 0, 0))
    x = 0
    for t in tiles:
        strip.paste(t, (x, 0))
        x += t.width
    strip.save(out)
    print(f"条带 {out}  {strip.width}×{strip.height}  {len(tiles)} 帧 @ {args.t:g}s ±{args.frames}f")
    print("GATE=PASS")
    return 0


def cmd_rhythm(args) -> int:
    """课纲口径：idle% / peaks/min / activity —— 判"像不像成片"的那组数。"""
    import numpy as np
    path = Path(args.video)
    f = args.fps
    prev, diffs, t_list = None, [], []
    for t, small in _frames(path, f, 48, 27):
        g = small.mean(axis=2) / 255.0
        if prev is not None:
            diffs.append(float(np.abs(g - prev).mean()))
            t_list.append(t)
        prev = g
    if not diffs:
        fail("抽不到帧")
    d = np.array(diffs)
    dur = t_list[-1] - t_list[0] + 1.0 / f
    quiet = d < args.idle_th
    spans, cur = [], None
    for i, q in enumerate(quiet):
        if q and cur is None:
            cur = i
        elif not q and cur is not None:
            spans.append((cur, i)); cur = None
    if cur is not None:
        spans.append((cur, len(quiet)))
    spans = [s for s in spans if (s[1] - s[0]) / f >= 2.0]
    idle_s = sum((b - a) / f for a, b in spans)
    p90 = float(np.percentile(d, 90))
    thr = max(p90, args.peak_th)
    peaks = sum(1 for i in range(1, len(d) - 1)
                if d[i] >= thr and d[i] >= d[i - 1] and d[i] >= d[i + 1])
    activity = float(d.mean())
    idle_pct = idle_s / dur * 100
    print(f"片长 {dur:6.2f}s · 采样 {f:g}fps · diff mean {activity:.4f} · p90 {p90:.4f}")
    print(f"idle（连续静止 ≥2s）: {idle_s:.2f}s = {idle_pct:.1f}%   红线 35%")
    print(f"peaks: {peaks} 个 = {peaks/dur*60:.1f}/min   标杆 9–22")
    if spans:
        top = sorted(spans, key=lambda s: s[1] - s[0], reverse=True)[:5]
        print("最长静止段: " + ", ".join(
            f"{t_list[a]:.2f}–{t_list[b-1]:.2f}s({(b-a)/f:.2f}s)" for a, b in top))
    ppm = peaks / dur * 60
    verdict = []
    if idle_pct >= 35:
        verdict.append("FAIL idle 超红线 35%")
    if activity < 0.008 and ppm > 15:
        verdict.append("FAIL 假峰：peaks 高但 activity 贴地（字变画不动）")
    elif activity < 0.008:
        verdict.append("WARN 稳推不足：activity < 0.008")
    if idle_pct < 20 and activity >= 0.008:
        verdict.append("PASS 可接受稳推")
    for v in verdict:
        print(v)
    ok = not any(v.startswith("FAIL") for v in verdict)
    print("GATE=" + ("PASS" if ok else "FAIL"))
    if args.json:
        print(json.dumps({"idle_pct": round(idle_pct, 2), "peaks_per_min": round(ppm, 2),
                          "activity": round(activity, 5), "duration": round(dur, 2)}, ensure_ascii=False))
    return 0 if ok else 1


def cmd_all(args) -> int:
    rc = 0
    print("=== dead ===")
    rc |= cmd_dead(argparse.Namespace(video=args.video, fps=args.fps, thresh=args.thresh, json=False))
    print("\n=== luma ===")
    cmd_luma(argparse.Namespace(video=args.video, json=False, require=False))
    print("\n=== sheet ===")
    cmd_sheet(argparse.Namespace(video=args.video, cols=4, rows=3, out=None))
    return 1 if rc else 0


# ============================ 坏帧控制（门必须能红） ============================

def cmd_selftest(args) -> int:
    work = Path(args.dir or ".tmp/shipgate-selftest")
    work.mkdir(parents=True, exist_ok=True)
    ok = True

    def check(name: str, got_fail: bool, want_fail: bool):
        nonlocal ok
        good = got_fail == want_fail
        ok = ok and good
        print(f"[{'ok ' if good else 'BAD'}] {name}: 期望 {'FAIL' if want_fail else 'PASS'}，"
              f"实得 {'FAIL' if got_fail else 'PASS'}")

    # --- boxes：已知坏样本（重叠 / 出区 / 贴边 / 空场 / 稀疏） ---
    bad = work / "bad.jsonl"
    lines = [
        # 文字互相重叠
        {"t": 1.0, "id": "a", "kind": "text", "group": "a", "x0": 200, "y0": 100, "x1": 700, "y1": 160},
        {"t": 1.0, "id": "b", "kind": "text", "group": "b", "x0": 600, "y0": 120, "x1": 1100, "y1": 180},
        # 出安全区 + 大字压到 y>720
        {"t": 2.0, "id": "c", "kind": "text", "group": "c", "x0": 40, "y0": 700, "x1": 1200, "y1": 800},
        # 字贴父框（0px 边距）
        {"t": 3.0, "id": "p", "kind": "plate", "group": "p", "x0": 200, "y0": 400, "x1": 900, "y1": 500},
        {"t": 3.0, "id": "d", "kind": "text", "group": "d", "parent": "p", "x0": 200, "y0": 400, "x1": 500, "y1": 460},
        # 字幕期间空场 + 稀疏
        {"t": 4.0, "id": "s", "kind": "text", "group": "s", "role": "subtitle",
         "x0": 300, "y0": 760, "x1": 900, "y1": 820},
    ]
    bad.write_text("\n".join(json.dumps(x, ensure_ascii=False) for x in lines), encoding="utf-8")

    good = work / "good.jsonl"
    glines = [
        {"t": 1.0, "id": "p1", "kind": "plate", "group": "p1", "x0": 200, "y0": 400, "x1": 1200, "y1": 520},
        {"t": 1.0, "id": "t1", "kind": "text", "group": "t1", "parent": "p1",
         "x0": 220, "y0": 420, "x1": 700, "y1": 470},
        {"t": 1.0, "id": "t2", "kind": "text", "group": "t2", "x0": 800, "y0": 200, "x1": 1100, "y1": 260},
    ]
    good.write_text("\n".join(json.dumps(x, ensure_ascii=False) for x in glines), encoding="utf-8")

    for label, f, want in (("五门/坏样本", bad, True), ("五门/好样本", good, False)):
        ns = argparse.Namespace(boxes=str(f), json=False, min_alpha=0.05)
        print(f"--- {label} ---")
        rc = cmd_boxes(ns)
        check(label, rc != 0, want)

    # --- 文案：已知坏（元标签 / 句号 / 超字 / 钩子超长） ---
    bsh = work / "bad_shots.json"
    bsh.write_text(json.dumps({"shots": [
        {"id": 1, "event": 1.0, "hold": 2.0, "hook": True, "big": "钩：一道画鹈鹕的题很长很长",
         "sub": "来源：某论文。"},
        {"id": 2, "event": 1.0, "hold": 1.0, "big": "一句话说完", "sub": "短"},
    ]}, ensure_ascii=False), encoding="utf-8")
    print("--- 文案+预算/坏样本 ---")
    rc1 = cmd_copy(argparse.Namespace(shots=str(bsh), json=False))
    rc2 = cmd_budget(argparse.Namespace(shots=str(bsh), json=False))
    check("copy/坏样本", rc1 != 0, True)
    check("budget/坏样本", rc2 != 0, True)

    gsh = work / "good_shots.json"
    gsh.write_text(json.dumps({"shots": [
        {"id": 1, "event": 1.2, "hold": 6.3, "hook": True,
         "big": "一道画鹈鹕的题", "sub": "笔记本赢了旗舰"},
    ]}, ensure_ascii=False), encoding="utf-8")
    print("--- 文案+预算/好样本 ---")
    check("copy/好样本", cmd_copy(argparse.Namespace(shots=str(gsh), json=False)) != 0, False)
    check("budget/好样本", cmd_budget(argparse.Namespace(shots=str(gsh), json=False)) != 0, False)

    # --- 视频：已知坏（8s 静止）vs 好（4s 有变化） ---
    badv = work / "bad_static.mp4"
    goodv = work / "good_motion.mp4"
    run([FFMPEG, "-y", "-v", "error", "-f", "lavfi", "-i", "color=c=0x0a0c14:s=640x360",
         "-t", "8", "-r", "30", "-pix_fmt", "yuv420p", str(badv)])
    run([FFMPEG, "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=s=640x360:r=30",
         "-t", "4", "-pix_fmt", "yuv420p", str(goodv)])
    print("--- dead/坏样本（8s 静止）---")
    if badv.exists():
        check("dead/坏样本", cmd_dead(argparse.Namespace(video=str(badv), fps=4.0,
                                                         thresh=1.0, json=False)) != 0, True)
    else:
        print("[skip] 没造出坏样本视频")
    print("--- dead/好样本（4s 变化）---")
    if goodv.exists():
        check("dead/好样本", cmd_dead(argparse.Namespace(video=str(goodv), fps=4.0,
                                                         thresh=1.0, json=False)) != 0, False)
    else:
        print("[skip] 没造出好样本视频")

    print("\nSELFTEST=" + ("PASS" if ok else "FAIL"))
    return 0 if ok else 1


# ============================ CLI ============================

def main() -> int:
    ap = argparse.ArgumentParser(description="出片版式/节奏/文案门禁")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("budget", help="阅读预算（shots.json）")
    p.add_argument("shots")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_budget)

    p = sub.add_parser("copy", help="文案机器闸（shots.json）")
    p.add_argument("shots")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_copy)

    p = sub.add_parser("boxes", help="五门 collision/clip/crop/empty/sparseMid")
    p.add_argument("boxes")
    p.add_argument("--json", action="store_true")
    p.add_argument("--min-alpha", type=float, default=0.05)
    p.add_argument("--safe", help="x0,y0,x1,y1 安全区覆盖（默认横版 1920 用 [96,80,1824,900]）")
    p.add_argument("--title-max-y", type=float, default=TITLE_MAX_Y, help="大字下限（竖版封面要放宽）")
    p.set_defaults(func=cmd_boxes)

    p = sub.add_parser("dead", help="死画面")
    p.add_argument("video")
    p.add_argument("--fps", type=float, default=4.0)
    p.add_argument("--thresh", type=float, default=1.0)
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_dead)

    p = sub.add_parser("luma", help="每秒 p99.5 亮度")
    p.add_argument("video")
    p.add_argument("--require", action="store_true", help="有任何一秒无高亮即 FAIL")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_luma)

    p = sub.add_parser("sheet", help="接触表")
    p.add_argument("video")
    p.add_argument("--cols", type=int, default=4)
    p.add_argument("--rows", type=int, default=3)
    p.add_argument("--out")
    p.set_defaults(func=cmd_sheet)

    p = sub.add_parser("strip", help="接缝连续帧条带")
    p.add_argument("video")
    p.add_argument("--t", type=float, required=True)
    p.add_argument("--frames", type=int, default=4)
    p.add_argument("--out")
    p.set_defaults(func=cmd_strip)

    p = sub.add_parser("rhythm", help="节奏课：idle% / peaks / activity")
    p.add_argument("video")
    p.add_argument("--fps", type=float, default=4.0)
    p.add_argument("--idle-th", type=float, default=0.004, help="小于此 diff 视为静止（0-1 归一）")
    p.add_argument("--peak-th", type=float, default=0.012, help="峰的最小 diff")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_rhythm)

    p = sub.add_parser("all", help="dead+luma+sheet")
    p.add_argument("video")
    p.add_argument("--fps", type=float, default=4.0)
    p.add_argument("--thresh", type=float, default=1.0)
    p.set_defaults(func=cmd_all)

    p = sub.add_parser("selftest", help="坏帧控制：证明门能在坏样本上 FAIL")
    p.add_argument("--dir")
    p.set_defaults(func=cmd_selftest)

    args = ap.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
