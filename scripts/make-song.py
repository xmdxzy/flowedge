#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""多段 ACE-Step 生成 → 按键性/能量挑候选 → 交叉淡化拼装 → 响度归一。

为什么要多段：ACE-Step 单次有效乐句只有 ~10–12 秒（见
docs/ACE-Step音乐生成-接入说明.md §11.3），requested duration 只决定文件长度，
后面全是残响尾巴。要「有情感起伏」的曲子，只能按段落分别生成再拼。

为什么要挑候选：同 prompt 不同 seed 的调式会漂（小调漂成大调就全毁了）。
这里用 chroma 与首段做相关性，配合计划好的能量高低，从 batch 里选一条。

用法：
  python scripts/make-song.py --plan .tmp/song-plan.json --bpm 68 --steps 8 \
      --model acestep-v15-xl-turbo --batch 2 --out 运行/data/generated
"""
from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

import numpy as np
import soundfile as sf

BASE = os.environ.get("ACESTEP_BASE", "http://127.0.0.1:8104")
urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))
SR = 22050
# 目标调性（与源曲一致）：F# minor 剖面（索引 6 = F#）
TARGET_TONIC = 6
MINOR_PROFILE = np.roll(
    np.array([6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17]), TARGET_TONIC)


def key_fit(chroma: np.ndarray) -> float:
    """与目标小调剖面的相关：>0.6 才算「确实落在这个调上」。"""
    return float(np.corrcoef(chroma, MINOR_PROFILE)[0, 1])


def post(path: str, payload: dict, timeout: float = 120) -> dict:
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        data = json.loads(r.read().decode("utf-8"))
    if data.get("code") not in (None, 200, 0):
        raise RuntimeError(f"{path} 报错：{data}")
    return data.get("data", data)


def health() -> dict:
    with urllib.request.urlopen(BASE + "/health", timeout=10) as r:
        return json.loads(r.read().decode("utf-8"))


def submit(seg: dict, opt) -> str:
    payload = {
        "prompt": seg["prompt"],
        "lyrics": "",
        "audio_format": "wav",
        "audio_duration": opt.duration,
        "batch_size": opt.batch,
        "inference_steps": opt.steps,
        "model": opt.model,
        "bpm": opt.bpm,
        "use_cot_caption": False,
        "use_cot_language": False,
        "use_random_seed": False,
        "seed": opt.seed + seg["i"] * 7919,
    }
    data = post("/release_task", payload)
    tid = data.get("task_id") or (data if isinstance(data, str) else None)
    if not tid:
        raise RuntimeError(f"未返回 task_id：{data}")
    return str(tid)


def wait(tid: str, budget: float) -> str:
    t0 = time.time()
    while time.time() - t0 < budget:
        items = post("/query_result", {"task_id_list": [tid]})
        if isinstance(items, dict):
            items = [items]
        for it in items or []:
            if it.get("task_id") and it["task_id"] != tid:
                continue
            st = it.get("status")
            if st == 1:
                return it.get("result") or ""
            if st == 2:
                raise RuntimeError(f"生成失败：{str(it.get('result'))[:300]}")
        time.sleep(2.0)
    raise TimeoutError(f"task {tid} 超时")


def files_from_result(result: str) -> list[str]:
    result = (result or "").strip()
    if not result or result == "null":
        raise RuntimeError("result 为空")
    try:
        arr = json.loads(result)
    except json.JSONDecodeError:
        raise RuntimeError(f"result 不可解析：{result[:200]}")
    if isinstance(arr, dict):
        arr = [arr]
    out = [e["file"] for e in arr if isinstance(e, dict) and e.get("file")]
    if not out:
        raise RuntimeError(f"result 无 file 字段：{result[:200]}")
    return out


def download(remote: str, dest: Path) -> Path:
    url = remote if remote.startswith("http") else BASE + remote
    if not url.startswith("http"):
        url = BASE + "/v1/audio?path=" + urllib.parse.quote(remote, safe="")
    with urllib.request.urlopen(url, timeout=120) as r:
        dest.write_bytes(r.read())
    return dest


def load_audio(path: Path):
    """(frames, channels) 原样读出，**保留立体声**——ACE 输出是 2 声道，
    混成单声道会丢掉混响宽度，最后成品会发干。分析另取 mono。"""
    y, sr = sf.read(str(path), dtype="float32", always_2d=True)
    return y.astype(np.float32), int(sr)


def load_mono(path: Path):
    """读成单声道，**保持原采样率**。

    之前这里统一重采样到 22050 再做分析，结果连成品也按 22050 写出——
    高频被砍到 11kHz，听感发闷。分析用的降采样只在 chroma_of 里做。
    """
    y, sr = sf.read(str(path), dtype="float32", always_2d=True)
    return y.mean(axis=1), int(sr)


def end_ratio(y: np.ndarray, sr: int) -> float:
    """尾部 2s 能量 / 中段（30%–70%）能量。

    ACE 每段都可能自己「解决」掉（回到主音、长音收束）——拼接时这样的尾巴
    会变成两次结尾。非收尾段要挑「还在流动」的；最后一段反过来要挑真的收尾的。
    """
    n = len(y)
    tail = y[max(0, n - int(2 * sr)):]
    mid = y[int(0.3 * n):int(0.7 * n)]
    if mid.size == 0:
        mid = y
    r_tail = float(np.sqrt(np.mean(tail ** 2)))
    r_mid = float(np.sqrt(np.mean(mid ** 2))) or 1e-9
    return r_tail / r_mid


def content_end(y: np.ndarray, sr: int, rel_db: float = -32.0) -> float:
    """有效内容结束点：最后一个「不低于峰值 rel_db dB」的 50ms 帧。"""
    win = int(0.05 * sr)
    n = len(y) // win
    if n == 0:
        return len(y) / sr
    rms = np.array([np.sqrt(np.mean(y[i * win:(i + 1) * win] ** 2)) for i in range(n)])
    peak = rms.max() or 1e-9
    thr = peak * (10 ** (rel_db / 20.0))
    idx = np.where(rms >= thr)[0]
    if idx.size == 0:
        return len(y) / sr
    return (idx[-1] + 1) * 0.05


def chroma_of(y: np.ndarray, sr: int) -> np.ndarray:
    import librosa

    if sr != SR:
        y = librosa.resample(y, orig_sr=sr, target_sr=SR)
    c = librosa.feature.chroma_cqt(y=librosa.effects.harmonic(y), sr=SR, hop_length=1024)
    m = c.mean(axis=1)
    return m / (m.sum() or 1.0)


def corr(a: np.ndarray, b: np.ndarray) -> float:
    return float(np.corrcoef(a, b)[0, 1])


def lr_corr(y: np.ndarray) -> float:
    if y.ndim != 2 or y.shape[1] < 2:
        return 1.0
    a, b = y[:, 0], y[:, 1]
    if a.std() < 1e-7 or b.std() < 1e-7:
        return 1.0
    return float(np.corrcoef(a, b)[0, 1])


def width_match(y: np.ndarray, target: float = 0.72) -> np.ndarray:
    """把左右声道「收」到更像一件乐器。

    ACE 经常把同一个 prompt 生成成两份**不同的演奏**分别放左右：
    实测 intro 段 L/R 相关 0.25（有的候选是 0.02、−0.41），听感就是
    「两架钢琴同时在弹、还互相打架」。这里按 mid/side 缩放 side，
    用二分把相关度抬到 target —— 保留一点宽度，但不再是两份演奏。
    """
    if y.ndim != 2 or y.shape[1] < 2:
        return y
    cur = lr_corr(y)
    if cur >= target:
        return y
    mid = (y[:, 0] + y[:, 1]) / 2.0
    side = (y[:, 0] - y[:, 1]) / 2.0
    lo, hi = 0.0, 1.0
    best = y
    # 相关度随 g 单调**递减**（g=0 时两声道相同=1.0），所以「合法区」是 [0, g*]
    for _ in range(22):
        g = (lo + hi) / 2.0
        cand = np.stack([mid + g * side, mid - g * side], axis=1).astype(np.float32)
        if lr_corr(cand) >= target:
            lo = g
            best = cand
        else:
            hi = g
    return best


def tonic_tail(mono: np.ndarray, sr: int, seconds: float = 3.0, tonic: int = TARGET_TONIC) -> float:
    """尾部 3 秒里主音在 chroma 里的占比——结尾「落没落住」的直接读数。"""
    import librosa

    tail = mono[-int(seconds * sr):]
    if tail.size < sr // 2:
        return 0.0
    c = librosa.feature.chroma_cqt(y=librosa.effects.harmonic(tail), sr=sr, hop_length=1024).mean(axis=1)
    s = float(c.sum()) or 1e-9
    return float(c[tonic] / s)


def onsets_per_sec(mono: np.ndarray, sr: int) -> float:
    import librosa

    env = librosa.onset.onset_strength(y=mono, sr=sr, hop_length=512)
    n = len(librosa.onset.onset_detect(onset_envelope=env, sr=sr, hop_length=512))
    return float(n / max(len(mono) / sr, 1e-6))


def cut_after_decay(y: np.ndarray, sr: int, rel_db: float = -18.0, hold: float = 0.75,
                    tail: float = 1.5) -> np.ndarray:
    """收尾段：在**第一次真正的衰减**处裁断。

    实测 ACE 的收尾段常常是「终止 → 静一下 → 又来一句」，用户听到的就是
    「结束了又回来，很奇怪」。这里找第一个持续 hold 秒低于峰值 rel_db 的点，
    之后只留 tail 秒让混响自然走完。
    """
    mono = y.mean(axis=1) if y.ndim == 2 else y
    w = int(0.25 * sr)
    n = len(mono) // w
    if n < 8:
        return y
    rms = np.array([np.sqrt(np.mean(mono[i * w:(i + 1) * w] ** 2)) for i in range(n)])
    peak_i = int(np.argmax(rms))
    thr = rms[peak_i] * (10 ** (rel_db / 20.0))
    need = max(1, int(hold / 0.25))
    run = 0
    for i in range(peak_i, n):
        if rms[i] < thr:
            run += 1
            if run >= need:
                cut = int((i - run + 1) * w + tail * sr)
                return y[:max(cut, int(1.0 * sr))]
        else:
            run = 0
    return y


def cluster_rate(mono: np.ndarray, sr: int) -> float:
    """每帧最强三音里出现相邻半音的比例——用户说的「很不和谐」就是它。

    实测：旧的独奏段 0.43、弦乐段 0.14。挑候选时要把这个数压下去。
    """
    import librosa

    c = librosa.feature.chroma_cqt(y=librosa.effects.harmonic(mono), sr=sr, hop_length=1024)
    cn = c / (c.sum(axis=0, keepdims=True) + 1e-9)
    top = np.argsort(cn, axis=0)[-3:, :]
    bad = 0
    for i in range(top.shape[1]):
        t = sorted(top[:, i].tolist())
        if t[1] - t[0] == 1 or t[2] - t[1] == 1:
            bad += 1
    return float(bad / max(top.shape[1], 1))


def head_cut_to_peak(y: np.ndarray, sr: int, rel_db: float = -8.0, max_s: float = 25.0) -> np.ndarray:
    """把「慢起」的段落切到它自己最响处之前的 rel_db 处。

    有的段（尤其是要当高潮用的）模型会写 10–15s 的铺垫，接在前面会塌一个洞；
    但又不能一刀切到峰值，那样会丢掉最后一点上行。rel_db=-8 实测正好落在起势点上。
    """
    mono = y.mean(axis=1) if y.ndim == 2 else y
    w = int(0.5 * sr)
    n = len(mono) // w
    if n < 4:
        return y
    rms = np.array([np.sqrt(np.mean(mono[i * w:(i + 1) * w] ** 2)) for i in range(n)])
    peak = rms.max() or 1e-9
    thr = peak * (10 ** (rel_db / 20.0))
    idx = np.where(rms >= thr)[0]
    if idx.size == 0:
        return y
    start = int(idx[0] * w)
    start = min(start, int(max_s * sr))
    return y[start:]


def head_db(mono: np.ndarray, sr: int, seconds: float = 1.5) -> float:
    """段首 1.5s 的电平——接缝处新段太弱就会压出一个洞（实测 71s 掉 14dB）。"""
    head = mono[: int(seconds * sr)]
    if head.size == 0:
        return -120.0
    return float(20 * np.log10(max(np.sqrt(np.mean(head ** 2)), 1e-9)))


def head_trim(y: np.ndarray, sr: int, rel_db: float = -8.0, max_s: float = 3.0) -> np.ndarray:
    """切掉段首那段「弱起」——ACE 常给每段留 0.5–3s 的静/弱开头。

    不切的话，交叉淡化区里新段是 -28dB、旧段还在 -14dB，
    合完就是一个 1.5s 的洞（实测在 71s 接缝后掉了 14dB）。
    """
    mono = y.mean(axis=1) if y.ndim == 2 else y
    w = int(0.25 * sr)
    n = len(mono) // w
    if n < 4:
        return y
    rms = np.array([np.sqrt(np.mean(mono[i * w:(i + 1) * w] ** 2)) for i in range(n)])
    thr = np.median(rms) * (10 ** (rel_db / 20.0))
    idx = np.where(rms >= thr)[0]
    start = 0 if idx.size == 0 else int(idx[0] * w)
    start = min(start, int(max_s * sr))
    return y[start:]


def apply_level_curve(y: np.ndarray, sr: int, spans: list[tuple[float, float]], targets_db: list[float]) -> np.ndarray:
    """按段落中心插值出一条电平曲线，再乘上去。

    为什么必须自己写：三段的实测 RMS 是 −15.9 / −16.0 / −17.8 dB——
    模型给各段配的音量根本不随「编曲厚度」变化，高潮段甚至更轻。
    只靠 prompt 要不到「起伏」，电平曲线才是可控的那一层。
    """
    n = len(y)
    centers = [s + (e - s) / 2 for s, e in spans]
    t = np.arange(n) / sr
    curve = np.interp(t, centers, targets_db)
    g = (10 ** (curve / 20.0)).astype(np.float32)
    return y * (g[:, None] if y.ndim == 2 else g)


def crossfade(segs: list[np.ndarray], sr: int, d) -> np.ndarray:
    """等功率交叉淡化。d 可以是统一的秒数，也可以是「每段与前一段的重叠」列表。"""
    if not segs:
        return np.zeros(0, dtype=np.float32)
    out = segs[0].copy()
    for idx, nxt in enumerate(segs[1:], start=1):
        di = float(d[idx]) if isinstance(d, (list, tuple)) else float(d)
        n = int(di * sr)
        n_eff = min(n, len(out), len(nxt))
        if n_eff < 8:
            out = np.concatenate([out, nxt])
            continue
        a = out[-n_eff:]
        b = nxt[:n_eff]
        t = np.linspace(0.0, 1.0, n_eff, dtype=np.float32)
        fade_a = np.cos(t * np.pi / 2)
        fade_b = np.sin(t * np.pi / 2)
        if a.ndim == 2:  # 立体声：淡化曲线按帧广播
            fade_a, fade_b = fade_a[:, None], fade_b[:, None]
        out = np.concatenate([out[:-n_eff], a * fade_a + b * fade_b, nxt[n_eff:]])
    return out


def apply_limiter(y: np.ndarray, sr: int, work: Path, limit: float) -> np.ndarray:
    """只削掉高于 limit 的峰，不做动态压缩。

    为什么不用 acompressor：实测 ratio 2.2 / thr −20dB 会把 6.8dB 的起伏压到 4.0dB
    ——那正是用户要的东西。alimiter 只动超过阈值的少数峰，5 秒段的 RMS 曲线不变。
    """
    if limit <= 0 or limit >= 1.0:
        return y
    src = work / "_prelimit.wav"
    dst = work / "_postlimit.wav"
    sf.write(str(src), np.clip(y, -1, 1), sr, subtype="FLOAT")
    subprocess.run(["ffmpeg", "-y", "-hide_banner", "-loglevel", "error", "-i", str(src),
                    "-af", f"alimiter=limit={limit:.3f}:attack=10:release=150:level=false",
                    str(dst)], check=True)
    out, sr2 = load_audio(dst)
    src.unlink(missing_ok=True)
    dst.unlink(missing_ok=True)
    return out


def measure_lufs(y: np.ndarray, sr: int, tmp: Path) -> tuple[float, float]:
    sf.write(str(tmp), np.clip(y, -1, 1), sr, subtype="PCM_24")
    p = subprocess.run(["ffmpeg", "-hide_banner", "-nostats", "-i", str(tmp), "-af", "ebur128=peak=true",
                        "-f", "null", "-"], capture_output=True, text=True)
    txt = p.stderr
    summary = txt.split("Summary:")[-1] if "Summary:" in txt else txt
    m = re.search(r"I:\s+(-?[\d.]+)\s+LUFS", summary)
    tp = re.search(r"True peak:\s+Peak:\s+(-?[\d.]+)", txt) or re.search(r"Peak:\s+(-?[\d.]+)\s+dBFS", txt)
    if not m:
        raise RuntimeError("ebur128 测不出响度")
    return float(m.group(1)), float(tp.group(1)) if tp else -99.0


def master_static(y: np.ndarray, sr: int, work: Path, lufs: float, tp: float) -> np.ndarray:
    """静态增益 → 目标响度，峰值超了就整体压下来。

    为什么不用 ffmpeg loudnorm：单遍 loudnorm 是**动态**归一化，会把
    段落间写好的 8dB 起伏自己抹平成 5dB（实测：想要 8dB，只拿到 2.6dB）。
    这里只做一次常数增益，动态由我写的那条曲线决定。
    """
    tmp = work / "_measure.wav"
    for i in range(3):
        cur, cur_tp = measure_lufs(y, sr, tmp)
        gain = lufs - cur
        # 峰值余量：先按响度算，再确保不超 TP
        max_gain_tp = tp - cur_tp
        gain = min(gain, max_gain_tp)
        if abs(gain) < 0.2:
            break
        y = (y * (10 ** (gain / 20.0))).astype(np.float32)
        print(f"[ace] master pass{i + 1}: {cur:.1f}LUFS/{cur_tp:.1f}dBTP -> gain {gain:+.1f}dB", flush=True)
    tmp.unlink(missing_ok=True)
    return y


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--plan", required=True)
    ap.add_argument("--out", default="运行/data/generated")
    ap.add_argument("--bpm", type=float, default=0)
    ap.add_argument("--seed", type=int, default=20261105)
    ap.add_argument("--steps", type=int, default=8)
    ap.add_argument("--duration", type=float, default=15.0)
    ap.add_argument("--batch", type=int, default=2)
    ap.add_argument("--model", default="acestep-v15-xl-turbo")
    ap.add_argument("--xfade", type=float, default=1.4)
    ap.add_argument("--fade-out", type=float, default=2.5, help="结尾淡出秒数")
    ap.add_argument("--no-auto", dest="auto", action="store_false", help="关掉段落电平曲线")
    ap.add_argument("--assemble-only", action="store_true", help="复用已有清单里的段落，只重拼")
    ap.add_argument("--lufs", type=float, default=-14.0)
    ap.add_argument("--limit", type=float, default=0.0, help="先削峰到该线性幅度（0.5≈−6dB），再静态增益")
    ap.add_argument("--tp", type=float, default=-1.5)
    ap.add_argument("--tag", default="ace-song")
    ap.add_argument("--mp3", action="store_true", help="额外导一份 320k MP3（默认只出 WAV）")
    ap.add_argument("--no-width-fix", dest="width_fix", action="store_false",
                    help="关掉段内立体声收窄")
    ap.add_argument("--width-target", type=float, default=0.72,
                    help="L/R 相关度目标（默认 0.72；太低=像两架琴，太高=单声道）")
    args = ap.parse_args()

    plan = json.loads(Path(args.plan).read_text(encoding="utf-8"))
    segs = plan["segments"] if isinstance(plan, dict) else plan
    for i, s in enumerate(segs):
        s["i"] = i
    plan_segs = segs  # --assemble-only 会把 segs 清空，电平目标仍要按 plan 取
    if args.bpm <= 0:
        args.bpm = float(plan.get("bpm", 0) or 0)

    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)
    work = out_dir / f"{args.tag}-work"
    work.mkdir(exist_ok=True)

    print(f"[ace] health={health()} bpm={args.bpm} steps={args.steps} model={args.model}", flush=True)

    picked: list[np.ndarray] = []
    picked_sr = 0
    ref_chroma = None
    manifest = {"bpm": args.bpm, "steps": args.steps, "model": args.model, "segments": []}
    prev_manifest = None
    if args.assemble_only:
        # 只重拼不重生成：段落已经挑过了，改的只是电平曲线/交叉淡化/母带
        mf = out_dir / f"{args.tag}-manifest.json"
        if not mf.exists():
            raise SystemExit(f"--assemble-only 需要已有清单：{mf}")
        prev_manifest = json.loads(mf.read_text(encoding="utf-8"))
        if len(prev_manifest["segments"]) != len(segs):
            raise SystemExit("清单段数与 plan 不一致，拒绝混拼")
        for idx, (segm, plan_seg) in enumerate(zip(prev_manifest["segments"], plan_segs)):
            y2d, sr = load_audio(Path(segm["picked"]))
            if plan_seg.get("head_cut_peak") is not None:
                y2d = head_cut_to_peak(y2d, sr, rel_db=float(plan_seg["head_cut_peak"]))
            if plan_seg.get("head_cut"):
                y2d = y2d[int(float(plan_seg["head_cut"]) * sr):]
            if idx > 0 and not plan_seg.get("no_head_trim"):
                y2d = head_trim(y2d, sr, max_s=float(plan_seg.get("head_trim_max", 3.0)))
            if plan_seg.get("end") and not plan_seg.get("no_decay_cut"):
                y2d = cut_after_decay(y2d, sr)
            if args.width_fix and lr_corr(y2d) < 0.55:
                c0 = lr_corr(y2d)
                y2d = width_match(y2d, target=args.width_target)
                print(f"[ace]   width {c0:.2f} -> {lr_corr(y2d):.2f} ({segm['name']})", flush=True)
            mono = y2d.mean(axis=1)
            cut = int(content_end(mono, sr) * sr) + int(0.15 * sr)
            picked.append(y2d[:cut])
            picked_sr = sr
            print(f"[ace] reuse {segm['name']} {cut/sr:.2f}s @{sr}Hz ch={y2d.shape[1]}", flush=True)
        segs = []
    for s in segs:
        t0 = time.time()
        tid = submit(s, args)
        print(f"[ace] #{s['i']} {s['name']}: submitted {tid}", flush=True)
        result = wait(tid, budget=900)
        files = files_from_result(result)
        cands = []
        for k, f in enumerate(files):
            dest = work / f"s{s['i']:02d}-c{k}.wav"
            download(f, dest)
            y2d, sr = load_audio(dest)
            if s.get("head_cut_peak") is not None:
                y2d = head_cut_to_peak(y2d, sr, rel_db=float(s["head_cut_peak"]))
            if s.get("head_cut"):
                # 定点切头：ACE 的「高潮」段常在前 10–15s 慢起，自动阈值认不出来
                y2d = y2d[int(float(s["head_cut"]) * sr):]
            if s["i"] > 0 and not s.get("no_head_trim"):
                y2d = head_trim(y2d, sr, max_s=float(s.get("head_trim_max", 3.0)))
            if s.get("end") and not s.get("no_decay_cut"):
                y2d = cut_after_decay(y2d, sr)
            corr_before = lr_corr(y2d)
            if args.width_fix and corr_before < 0.55:
                y2d = width_match(y2d, target=args.width_target)
                print(f"[ace]   width {corr_before:.2f} -> {lr_corr(y2d):.2f} ({dest.name})", flush=True)
            mono = y2d.mean(axis=1)
            end = content_end(mono, sr)
            cut = int(end * sr) + int(0.15 * sr)
            y2d, mono = y2d[:cut], mono[:cut]
            cands.append({"note": s.get("note", ""), "file": str(dest), "y": y2d, "sr": sr,
                          "dur": len(mono) / sr, "chroma": chroma_of(mono, sr),
                          "end_ratio": end_ratio(mono, sr), "head_db": head_db(mono, sr),
                          "onsets": onsets_per_sec(mono, sr), "tonic_tail": tonic_tail(mono, sr),
                          "clusters": cluster_rate(mono, sr),
                          "lr": lr_corr(y2d),
                          "db": float(20 * np.log10(max(np.sqrt(np.mean(mono ** 2)), 1e-9)))})
        if ref_chroma is None:
            # 首段：先认调性（必须是 F# minor），再看别太吵
            best = max(cands, key=lambda c: key_fit(c["chroma"]) - c["clusters"] * 4.0
                       - max(0.0, c["onsets"] - float(s.get("onsets_max", 99))) * 1.5)
            ref_chroma = best["chroma"]
        else:
            # 后续段：调性对齐优先，其次能量贴近计划（避免突然一段炸开），
            # 再其次「像不像一个结尾」——非收尾段要还在流动，末段要真收住。
            target_db = float(s.get("target_db", np.mean([c["db"] for c in cands])))
            want_end = bool(s.get("end"))

            def score(c, target_db=target_db, want_end=want_end):
                v = corr(c["chroma"], ref_chroma) * 3.0 + key_fit(c["chroma"]) * 2.5
                v -= abs(c["db"] - target_db) / 6.0
                v -= c["clusters"] * 3.0  # 半音相撞越少越好（用户听到的「不和谐」）
                # 起音密度：计划里给了上限就按超出的幅度扣（钢琴独奏不该有 6/s 的音符流）
                if s.get("onsets_max"):
                    v -= max(0.0, c["onsets"] - float(s["onsets_max"])) * 1.2
                if want_end:
                    v -= abs(c["end_ratio"] - 0.10) * 4.0
                    # 结尾必须落在主音上：实测「怪」的那版尾部主音占比只有 0.49
                    v += c["tonic_tail"] * 5.0
                else:
                    v -= max(0.0, 0.55 - c["end_ratio"]) * 6.0
                    # 段首比整体弱 3dB 以上就要扣：接缝处会塌成一个洞
                    v -= max(0.0, c["db"] - c["head_db"] - 3.0) * 0.8
                return v

            best = max(cands, key=score)
        print(f"[ace] #{s['i']} {s['name']}: {len(cands)} cand -> {Path(best['file']).name} "
              f"dur={best['dur']:.2f}s keyconn={corr(best['chroma'], ref_chroma):.3f} "
              f"keyfit={key_fit(best['chroma']):.3f} end={best['end_ratio']:.2f} "
              f"onsets={best['onsets']:.2f} lr={best['lr']:.2f} tonic={best['tonic_tail']:.2f} "
              f"clash={best['clusters']:.2f} "
              f"{time.time()-t0:.1f}s", flush=True)
        picked.append(best["y"])
        picked_sr = best["sr"]
        manifest["segments"].append({"name": s["name"], "prompt": s["prompt"], "picked": best["file"],
                                     "candidates": [c["file"] for c in cands],
                                     "dur": round(best["dur"], 2),
                                     "keycorr": round(corr(best["chroma"], ref_chroma), 3)})

    seg_sr = picked_sr if picked_sr else SR
    # 每段与前一段的重叠：plan 里写了 xfade 就用它（收尾接缝要更长，不然高潮刚收完
    # 又突然进来一句，听感就是「结束了又没结束」）
    xfs = [0.0] + [float(plan_segs[k].get("xfade", args.xfade)) for k in range(1, len(picked))]
    y = crossfade(picked, seg_sr, xfs if len(xfs) > 1 else args.xfade)
    # 段落中心（在拼好的时间轴上），供电平曲线插值
    spans, pos = [], 0.0
    for k, seg in enumerate(picked):
        d = len(seg) / seg_sr
        spans.append((pos, pos + d))
        pos += d - (xfs[k] if k < len(xfs) else args.xfade)
    if args.auto:
        # 每段先归一化到**绝对 RMS 目标**（plan 的 level_db），再沿时间轴插值平滑。
        # 用相对偏移（target_db）踩过坑：A 段内容本身比 C 段响 8dB，
        # 各给 -8/0 dB 之后，独奏钢琴反而比高潮响——弧线直接反了。
        gains, want = [], []
        for k, seg in enumerate(picked):
            rms = float(20 * np.log10(max(np.sqrt(np.mean(seg ** 2)), 1e-9)))
            lv = plan_segs[k].get("level_db") if k < len(plan_segs) else None
            tgt = float(lv) if lv is not None else rms
            want.append(tgt)
            gains.append(tgt - rms)
        before = float(20 * np.log10(max(np.sqrt(np.mean(y ** 2)), 1e-9)))
        y = apply_level_curve(y, seg_sr, spans, gains)
        after = float(20 * np.log10(max(np.sqrt(np.mean(y ** 2)), 1e-9)))
        print(f"[ace] level: raw={[round(-g + w, 1) for g, w in zip(gains, want)]} "
              f"gain={[round(g, 1) for g in gains]} rms {before:.1f} -> {after:.1f}dB", flush=True)
    # 末尾淡出：指数曲线比线性更贴「琴音自然衰减」，硬切断尾音会显得没落住
    nf = min(int(args.fade_out * seg_sr), len(y))
    ramp = np.linspace(0.0, 1.0, nf, dtype=np.float32)
    ramp = np.clip((1.0 - ramp) ** 1.6, 0.0, 1.0)
    y[-nf:] *= (ramp[:, None] if y.ndim == 2 else ramp)
    y = apply_limiter(y, seg_sr, work, args.limit)
    y = master_static(y, seg_sr, work, args.lufs, args.tp)
    final = out_dir / f"{args.tag}-{int(time.time())}.wav"
    sf.write(str(final), np.clip(y, -1, 1), seg_sr, subtype="PCM_16")
    print(f"[ace] assembled {len(y)/seg_sr:.2f}s @{seg_sr}Hz -> {final}", flush=True)

    if args.mp3:  # 默认只出 wav（用户 2026-11-05 定的口径），需要时显式加 --mp3
        mp3 = final.with_suffix(".mp3")
        subprocess.run(["ffmpeg", "-y", "-hide_banner", "-loglevel", "error", "-i", str(final),
                        "-c:a", "libmp3lame", "-b:a", "320k", str(mp3)], check=True)
    if prev_manifest:
        manifest = prev_manifest
    manifest["out"] = str(final)
    manifest["seconds"] = round(len(y) / seg_sr, 2)
    manifest["sample_rate"] = seg_sr
    (out_dir / f"{args.tag}-manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2),
                                                      encoding="utf-8")
    print(f"[ace] done -> {final}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
