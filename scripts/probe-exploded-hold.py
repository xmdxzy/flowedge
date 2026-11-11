#!/usr/bin/env python3
"""从成片实测「爆炸停留窗口」与「引线末端坐标」，给烧字提供锚点。

为什么必须实测：上一版编号是模型画的、位置不受控；这一版引线末端留白，
标签由 libass 烧，**标签 y 必须落在真实引线的延长线上**，否则线是线、字是字。
按经验值排轴 = 每条都偏（AGENTS §0.12②「用工具没报错当没问题」的镜像）。

为什么一次解码而不是逐帧起 ffmpeg：`select=eq(n,K)` 每次都从片头解到 K，
160 帧就是 O(n²) 次解码；这里一次读出整个区间的缩略灰度帧，只起一个进程。
"""
import subprocess
import sys
import numpy as np

VIDEO = sys.argv[1] if len(sys.argv) > 1 else "运行/data/generated/film-1793702692499791800-shots-master-roomtone.mp4"
FPS = 24
SW, SH = 184, 104  # 差分用缩略尺寸；引线检测用原尺寸


def decode_range(a: int, b: int, w: int, h: int) -> np.ndarray:
    """解码 [a,b] 闭区间为 (n,h,w) uint8 灰度。"""
    n = b - a + 1
    cmd = ["ffmpeg", "-v", "error", "-i", VIDEO,
           "-vf", f"select='between(n,{a},{b})',scale={w}:{h},format=gray",
           "-vsync", "0", "-f", "rawvideo", "-pix_fmt", "gray", "-"]
    raw = subprocess.run(cmd, capture_output=True, check=True).stdout
    expect = n * w * h
    if len(raw) < expect:
        n = len(raw) // (w * h)
        print(f"[warn] 只解出 {n} 帧（期望 {b-a+1}）", file=sys.stderr)
    return np.frombuffer(raw[: n * w * h], dtype=np.uint8).reshape(n, h, w).astype(np.float32)


def main() -> int:
    # 1) 静止窗口：镜 2 大致在 73–231 帧，两端各留余量
    a0, a1 = 66, 236
    seq = decode_range(a0, a1, SW, SH)
    d = np.abs(np.diff(seq, axis=0)).mean(axis=(1, 2))  # d[i] = 帧 a0+i → a0+i+1 的平均差
    quiet = d < 1.2
    runs, start = [], None
    for i, q in enumerate(quiet):
        if q and start is None:
            start = i
        elif not q and start is not None:
            runs.append((a0 + start, a0 + i))  # 静止覆盖帧 [start, i]
            start = None
    if start is not None:
        runs.append((a0 + start, a0 + len(quiet)))
    runs = [(x, y) for x, y in runs if y - x >= 8]
    print("静止窗口（帧 / 秒）：")
    for x, y in runs:
        print(f"  {x}–{y}  时长 {(y-x)/FPS:.2f}s  起于 {x/FPS:.3f}s 止于 {y/FPS:.3f}s")
    if not runs:
        print("没找到 ≥8 帧静止窗口 → 停留没生效，别烧字，先重拍")
        return 1

    # 2) 在最静止的窗口里找引线（原尺寸）
    #    判据不能要求「右侧 35% 全亮」——线长短不一，那样只检出 4/7 条。
    #    改成：逐行找**最长连续亮段**，≥40px 且明显亮于上下行即为一条线，并记录其终点 x。
    best = max(runs, key=lambda r: r[1] - r[0])
    mid = (best[0] + best[1]) // 2
    full = decode_range(mid, mid, 736, 416)[0]
    h, w = full.shape
    x0 = int(w * 0.45)
    lines = []
    for y in range(2, h - 2):
        seg = full[y, x0:]
        bright = seg > 96
        # 最长连续亮段
        best_len = cur = 0
        best_end = cur_start = 0
        for i, b in enumerate(bright):
            if b:
                if cur == 0:
                    cur_start = i
                cur += 1
                if cur > best_len:
                    best_len, best_end = cur, i
            else:
                cur = 0
        if best_len < 40:
            continue
        band = seg[best_len and (best_end - best_len + 1): best_end + 1]
        if band.mean() <= max(full[y - 1, x0:][best_end - best_len + 1: best_end + 1].mean(),
                               full[y + 1, x0:][best_end - best_len + 1: best_end + 1].mean()) + 4:
            continue
        lines.append({"y": float(y), "x_end": float(x0 + best_end), "x_start": float(x0 + best_end - best_len + 1), "len": best_len})
    # 合并相邻行（线宽 1–3px）
    merged = []
    for ln in sorted(lines, key=lambda l: l["y"]):
        if merged and ln["y"] - merged[-1]["y"] <= 3:
            g = merged[-1]
            g["y"] = (g["y"] * g["n"] + ln["y"]) / (g["n"] + 1)
            g["n"] += 1
            g["x_end"] = max(g["x_end"], ln["x_end"])
        else:
            ln["n"] = 1
            merged.append(ln)
    cents = [round(m["y"], 1) for m in merged]
    print(f"\n探针帧 {mid}（{mid/FPS:.3f}s）检出水平引线 {len(merged)} 条：")
    for i, m in enumerate(merged, 1):
        print(f"  L{i}: y={m['y']:6.1f} ({m['y']/h:.3f}高)  x {m['x_start']:.0f}→{m['x_end']:.0f}  长 {m['len']}px")
    print(f"\n画面 {w}x{h}")
    if len(merged) < 7:
        print(f"⚠ 只检出 {len(merged)} 条引线（预期 7）——标签按检出数排，缺的不硬凑")
    import json
    json.dump([{"y": m["y"], "x_end": m["x_end"]} for m in merged], open(".tmp/pc2-leaders.json", "w"))
    print("已存 .tmp/pc2-leaders.json")
    return 0


if __name__ == "__main__":
    sys.exit(main())
