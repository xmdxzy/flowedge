#!/usr/bin/env python3
"""平滑度验收：一次生成的曲子有没有「塌下去 / 跳一下 / 换了个乐器」这类硬伤。

拼接版的问题在接缝，单次生成的问题在**后段塌陷**（实测 180s 请求 60s 后稳定掉到
-40~-45dB，每 8s 与开头 chroma 相似度 0.90–0.96 = 同一段 loop 越放越轻）。
本脚本只回答三件事：有没有突变、有没有塌陷、和声/音色是不是连续演进。

用法：python scripts/check-smoothness.py <wav>
退出码：0 平滑，1 有 WARN，2 有 FAIL
"""
from __future__ import annotations

import argparse
import json
import sys

import librosa
import numpy as np


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("wav")
    ap.add_argument("--json", default="")
    a = ap.parse_args()

    y, sr = librosa.load(a.wav, sr=22050, mono=True)
    dur = len(y) / sr
    rep: dict = {"file": a.wav, "duration_s": round(dur, 2)}
    fails, warns = [], []

    # 1) 1 秒级电平与突变
    h = sr
    n = len(y) // h
    rms = np.sqrt((y[: n * h].reshape(n, h) ** 2).mean(axis=1)) + 1e-9
    db = 20 * np.log10(rms)
    rep["level_1s_db"] = [round(float(v), 1) for v in db]
    live = db > -50
    rep["music_until_s"] = round(float((np.where(live)[0][-1] + 1)) if live.any() else 0.0, 1)
    if live.any():
        idx = np.where(live)[0]
        rep["music_from_s"] = round(float(idx[0]), 1)
        seg = db[idx[0]: idx[-1] + 1]
        # 结尾那几秒必然是一次「掉到静音」，那不是接缝缺陷；把它排除在突变统计外，
        # 单独用 ending_fade_s 量「收尾是否自然」。
        body = seg[:-3] if len(seg) > 6 else seg
        steps = np.abs(np.diff(body))
        rep["max_step_db"] = round(float(steps.max()), 1) if len(steps) else 0.0
        rep["p95_step_db"] = round(float(np.percentile(steps, 95)), 1) if len(steps) else 0.0
        if rep["max_step_db"] > 12:
            warns.append(f"相邻秒电平最大跳变 {rep['max_step_db']}dB（>12dB 可能听成断裂）")

        # 收尾：从「最后一次接近正常电平」到「落到 -50dB 以下」跨了几秒
        loud_idx = np.where(seg > -50)[0]
        if len(loud_idx) > 2:
            e = int(loud_idx[-1])
            win30 = seg[max(0, len(seg) - 40): max(1, len(seg) - 8)]
            med = float(np.median(win30)) if len(win30) else float(np.median(seg))
            ok = np.where(seg[: e + 1] >= med - 6)[0]
            s = int(ok[-1]) if len(ok) else 0
            rep["ending_fade_s"] = round(float(e - s), 1)
            rep["ending_median_db"] = round(med, 1)
            if rep["ending_fade_s"] < 3:
                warns.append(f"收尾过急：从正常电平掉到 −50dB 只用 {rep['ending_fade_s']}s（<3s 像硬切）")
        else:
            rep["ending_fade_s"] = 0.0

        # 2) 长程衰减：后半程相对前半程
        half = len(seg) // 2
        early = float(np.median(seg[:half]))
        late = float(np.median(seg[half:]))
        rep["median_db_first_half"] = round(early, 1)
        rep["median_db_second_half"] = round(late, 1)
        rep["decay_db"] = round(late - early, 1)
        if late - early < -6:
            fails.append(f"后段塌陷 {late - early:.1f}dB（前半 {early:.1f} / 后半 {late:.1f}）")
        elif late - early < -3:
            warns.append(f"后段偏轻 {late - early:.1f}dB")

        # 3) 是否存在整段掉到 -50 以下的「假收尾」
        tail_quiet = sum(1 for v in seg[int(len(seg) * 0.6):] if v < -50)
        rep["quiet_seconds_after_60pct"] = tail_quiet
        if tail_quiet > 6:
            warns.append(f"60% 之后有 {tail_quiet}s 低于 -50dB ⇒ 音乐提前收尾")

    # 4) 和声连续性 + 是否单 loop
    hop = 512
    chroma = librosa.feature.chroma_cqt(y=y, sr=sr, hop_length=hop)
    tps = sr / hop
    win = 8.0
    blocks = []
    t = 0.0
    while t + win < dur:
        b0, b1 = int(t * tps), int((t + win) * tps)
        blocks.append((t, chroma[:, b0:b1].mean(axis=1)))
        t += win
    adj, to_first = [], []
    for i, (_, v) in enumerate(blocks):
        nv = np.linalg.norm(v) + 1e-9
        if i:
            adj.append(float(np.dot(blocks[i - 1][1], v) / (np.linalg.norm(blocks[i - 1][1]) * nv)))
        to_first.append(float(np.dot(blocks[0][1], v) / (np.linalg.norm(blocks[0][1]) * nv)))
    if adj:
        rep["adjacent_8s_sim_min"] = round(min(adj), 3)
        rep["adjacent_8s_sim_mean"] = round(float(np.mean(adj)), 3)
        if min(adj) < 0.5:
            warns.append(f"相邻 8s 和声突变（min {min(adj):.3f}）")
    if to_first:
        rep["to_first_8s_sim_min"] = round(min(to_first), 3)
        rep["to_first_8s_sim_mean"] = round(float(np.mean(to_first)), 3)

    # 5) 音色漂移（每 10s 质心）
    cen = librosa.feature.spectral_centroid(y=y, sr=sr, hop_length=hop)[0]
    per10 = []
    for t in range(0, int(dur) - 10, 10):
        b0, b1 = int(t * tps), int((t + 10) * tps)
        per10.append((t, round(float(cen[b0:b1].mean()), 0)))
    rep["centroid_per_10s"] = per10
    if per10:
        vals = [v for _, v in per10]
        rep["centroid_min"], rep["centroid_max"] = min(vals), max(vals)
        # 「从一架钢琴推到弦乐齐鸣」本来就会把质心抬上去，3.5 倍以内算编排动态，
        # 超过才算音色跑偏（4 倍以上基本是换了编制/串了风格）。
        if max(vals) > min(vals) * 3.5:
            warns.append(f"音色大幅漂移（质心 {min(vals)}→{max(vals)}Hz）")

    rep["fails"], rep["warns"] = fails, warns
    print(json.dumps({k: v for k, v in rep.items() if k != "level_1s_db"}, ensure_ascii=False, indent=1))
    print("---")
    for f in fails:
        print("FAIL", f)
    for w in warns:
        print("WARN", w)
    if not fails and not warns:
        print("PASS 平滑，无突变/无塌陷/和声连续")
    if a.json:
        with open(a.json, "w", encoding="utf-8") as fh:
            json.dump(rep, fh, ensure_ascii=False, indent=1)
    return 2 if fails else (1 if warns else 0)


if __name__ == "__main__":
    sys.exit(main())
