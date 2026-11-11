#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""L4「读谱」：用 SheetSage2 把音频转成乐谱层信息（调性/和弦/节拍/段落/音符）。

为什么要有这个：我（agent）没有音频输入通道，只能靠数值和别人的描述判断音乐。
SheetSage2 把音频变成**结构化乐谱**——和弦进行、旋律音高、拍号、调性——
于是"听懂音乐"从玄学变成我能精确处理的数据。

用法：
    python scripts/hear-score.py <audio> [--out .tmp/score] [--json]

依赖：D:/AI/envs/sheetsage（独立 venv，见 docs/ACE-Step 音乐生成最高质量流水线.md §八）
权重缓存：~/.sheetsage/sheetsage2/
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

NOTE_NAMES = ["C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"]


def pitch_name(p: int) -> str:
    return f"{NOTE_NAMES[p % 12]}{p // 12 - 1}"


def merge_labels(intervals, min_dur: float = 0.4):
    """合并相邻同标签、丢掉过短的碎片，输出 (start, end, label) 序列。"""
    out = []
    for it in intervals:
        if not hasattr(it, "label"):
            continue
        lbl = str(it.label)
        dur = float(it.end) - float(it.start)
        if dur < min_dur:
            continue
        if out and out[-1][2] == lbl and abs(float(it.start) - out[-1][1]) < 0.15:
            out[-1] = (out[-1][0], float(it.end), lbl)
        else:
            out.append((float(it.start), float(it.end), lbl))
    return out


def analyze(audio: str, out_dir: Path, use_jukebox: bool = False) -> dict:
    from sheetsage import SheetSage2Options, SheetSageTranscriber

    tr = SheetSageTranscriber("sheetsage2", device="auto", require_structure=True)
    tr.load()
    # native_output_dir 才会让上游把 ABC / MIDI / 标注落盘（字段名不是 output_dir）
    out_dir.mkdir(parents=True, exist_ok=True)
    opts = SheetSage2Options(native_output_dir=str(out_dir))
    try:
        res = tr.infer(audio, options=opts)
    finally:
        pass

    rep: dict = {"audio": audio, "statuses": {}}

    # 各种输出的状态（ok / unsupported / not_requested / failed）
    st = getattr(res, "statuses", None)
    if isinstance(st, dict):
        rep["statuses"] = {k: getattr(v, "state", str(v)) for k, v in st.items()}

    # 调性
    key = getattr(res, "key", None) or getattr(res, "keys", None)
    if key is not None:
        rep["key"] = str(key)

    # 和弦进行
    chords = getattr(res, "chords", None) or []
    chord_seq = merge_labels(chords)
    rep["chord_count"] = len(chords)
    rep["chord_progression"] = [
        {"start": round(s, 2), "end": round(e, 2), "chord": l} for s, e, l in chord_seq
    ]

    # 拍号 / 速度
    meters = getattr(res, "meters", None) or []
    if meters:
        m = meters[0]
        rep["meter"] = f"{m.numerator}/{m.denominator}"
    beats = getattr(res, "beats", None) or []
    if len(beats) > 2:
        span = beats[-1].time - beats[0].time
        rep["bpm"] = round((len(beats) - 1) / span * 60, 1) if span > 0 else None
    rep["beat_count"] = len(beats)

    # 音符（旋律/和声）
    notes = getattr(res, "notes", None) or []
    rep["note_count"] = len(notes)
    if notes:
        roles: dict[str, int] = {}
        for n in notes:
            roles[n.role] = roles.get(n.role, 0) + 1
        rep["note_roles"] = roles
        # 旋律线（role 含 melody 的）
        mel = [n for n in notes if "melody" in str(n.role).lower()]
        if mel:
            rep["melody_preview"] = [
                {"t": round(n.start, 2), "pitch": pitch_name(n.pitch), "role": n.role}
                for n in mel[:40]
            ]
            rep["melody_pitch_range"] = [
                pitch_name(min(n.pitch for n in mel)),
                pitch_name(max(n.pitch for n in mel)),
            ]

    # 段落
    struct = getattr(res, "structure", None) or getattr(res, "sections", None)
    if struct:
        rep["structure"] = [str(s) for s in struct][:40]

    # 导出乐谱文件
    out_dir.mkdir(parents=True, exist_ok=True)
    for meth, suffix in (("as_midi", ".mid"), ("as_abc", ".abc"), ("as_lily", ".ly")):
        fn = getattr(res, meth, None)
        if callable(fn):
            try:
                data = fn()
                p = out_dir / (Path(audio).stem + suffix)
                if isinstance(data, bytes):
                    p.write_bytes(data)
                else:
                    p.write_text(str(data), encoding="utf-8")
                rep.setdefault("exports", []).append(str(p))
            except Exception as e:
                rep.setdefault("export_errors", []).append(f"{meth}: {e}")

    return rep


def main() -> int:
    ap = argparse.ArgumentParser(description="SheetSage2 读谱：音频 → 乐谱层结构化信息")
    ap.add_argument("audio")
    ap.add_argument("--out", default=".tmp/score")
    ap.add_argument("--json", action="store_true", help="只输出 JSON")
    a = ap.parse_args()

    rep = analyze(a.audio, Path(a.out))

    if a.json:
        print(json.dumps(rep, ensure_ascii=False, indent=2))
        return 0

    print(f"=== {a.audio} ===")
    print(f"状态: {rep.get('statuses')}")
    print(f"调性: {rep.get('key')}   拍号: {rep.get('meter')}   BPM: {rep.get('bpm')}")
    print(f"音符 {rep.get('note_count')} 个（角色 {rep.get('note_roles')}）  和弦 {rep.get('chord_count')} 个")
    if rep.get("melody_pitch_range"):
        print(f"旋律音域: {rep['melody_pitch_range']}")
    print("\n和弦进行（前 40 段）:")
    for c in rep.get("chord_progression", [])[:40]:
        print(f"  {c['start']:6.2f}-{c['end']:6.2f}s  {c['chord']}")
    if rep.get("melody_preview"):
        print("\n旋律（前 40 音）:")
        print("  " + " ".join(f"{m['pitch']}@{m['t']}" for m in rep["melody_preview"]))
    if rep.get("exports"):
        print(f"\n导出: {rep['exports']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
