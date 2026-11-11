#!/usr/bin/env python3
"""实测爆炸态各结构层的纵向中心，给中文标签找落点。

引线只有 5 条（模型画的），但用户要 7 个模块全部标出。
所以必须知道每一层**实际在哪个 y**，缺线的两层由我自己补画引线——
而不是凭感觉排，那样标签会浮在半空。
"""
import subprocess
import sys
import numpy as np

VIDEO = sys.argv[1] if len(sys.argv) > 1 else "运行/data/generated/film-1793702692499791800-shots-master-roomtone.mp4"
FRAME = int(sys.argv[2]) if len(sys.argv) > 2 else 170


def gray_frame(n: int) -> np.ndarray:
    raw = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", VIDEO, "-vf", f"select=eq(n\\,{n})",
         "-vsync", "0", "-f", "rawvideo", "-pix_fmt", "gray", "-"],
        capture_output=True, check=True).stdout
    return np.frombuffer(raw, dtype=np.uint8).reshape(416, 736).astype(np.float32)


def main() -> None:
    img = gray_frame(FRAME)
    h, w = img.shape
    # 结构堆叠在画面左半（引线从中部向右），取 x∈[110,350] 统计每行亮度
    band = img[:, 110:350]
    row = band.mean(axis=1)
    # 找「有硬件的行」：明显亮于全片黑底
    floor = np.percentile(row, 15)
    on = row > floor + 9
    segs, start = [], None
    for y, v in enumerate(on):
        if v and start is None:
            start = y
        elif not v and start is not None:
            if y - start >= 6:
                segs.append((start, y - 1))
            start = None
    if start is not None:
        segs.append((start, h - 1))
    print(f"探针帧 {FRAME}，x∈[110,350] 检出结构层 {len(segs)} 段：")
    for i, (a, b) in enumerate(segs, 1):
        c = (a + b) / 2
        print(f"  S{i}: y {a:3d}–{b:3d}  中心 {c:6.1f} ({c/h:.3f}高)  厚 {b-a+1}px  峰值亮度 {row[a:b+1].max():.0f}")
    leaders = [73.0, 132.0, 181.0, 248.0, 303.0]
    print("\n模型已画的引线 y：", leaders)
    print("每层与最近引线的差：")
    for a, b in segs:
        c = (a + b) / 2
        near = min(leaders, key=lambda L: abs(L - c))
        print(f"  层中心 {c:6.1f} → 引线 {near:5.1f}  差 {c-near:+6.1f}")


if __name__ == "__main__":
    main()
