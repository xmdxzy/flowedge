#!/usr/bin/env python3
"""生成中文标注 ASS —— 三个坑都踩过一次，全部钉在这里。

坑 1 · 时间格式：ASS 是 `H:MM:SS.cc`（**厘秒 2 位**），不是毫秒 3 位。
      写 `0:00:04.833` 会被 libass 读成 8.33 秒，事件时间轴整体错位，
      表现为「7 条标签只出 1 条，而线全在」——时长/帧数/音频全部正常，
      只有抽帧能发现。`.000` 两种解析都等于 0，所以用统一时间的对照实验
      **看不出这个 bug**（差点误诊成 \\fad 的问题）。

坑 2 · 锚点方向：`\\an6` 是右对齐，文字从锚点向左铺 → 正好压在引线末端。
      要文字在引线右侧，必须 `\\an4`（左对齐 + 垂直居中）。

坑 3 · 引线只画了 5 条，结构有 7 层。缺线的层补画；补画的线起点必须实测该层
      右缘，窗口不能卡在 52%（那样会把宽扁的底壳截断成假边缘）。
"""
import subprocess
import sys
import numpy as np

VIDEO = sys.argv[1] if len(sys.argv) > 1 else "运行/data/generated/film-1793702692499791800-shots-master-roomtone.mp4"
OUT = sys.argv[2] if len(sys.argv) > 2 else "exports/labels.ass"
W, H = 736, 416
PROBE = 170
T0, T_END = 4.833, 9.10        # 静止窗口实测 4.833–9.375，退场留 0.27s 余量
STEP = 0.28                     # 逐条错峰入场
LX, TX = 545.0, 556.0           # 引线末端 x / 标签左边界 x（留 11px 气口）

# (中文, 英文, 层 y, 模型是否已画这条引线)
ROWS = [
    ("机箱顶盖",   "TOP PANEL",      73.0,  True),
    ("电源与硬盘", "PSU & SSD",     100.0,  False),
    ("独立显卡",   "GRAPHICS CARD", 132.0,  True),
    ("CPU 散热器", "CPU COOLER",    181.0,  True),
    ("内存条",     "MEMORY",        248.0,  True),
    ("主板",       "MOTHERBOARD",   303.0,  True),
    ("机箱底壳",   "CHASSIS BASE",  345.0,  False),
]


def cs(t: float) -> str:
    """ASS 时间戳：厘秒 2 位。写成 3 位就是坑 1。"""
    cent = int(round(t * 100))
    h, rem = divmod(cent, 360000)
    m, rem = divmod(rem, 6000)
    s, c = divmod(rem, 100)
    return f"{h}:{m:02d}:{s:02d}.{c:02d}"


def gray(n: int) -> np.ndarray:
    raw = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", VIDEO, "-vf", f"select=eq(n\\,{n})",
         "-vsync", "0", "-f", "rawvideo", "-pix_fmt", "gray", "-"],
        capture_output=True, check=True).stdout
    return np.frombuffer(raw, dtype=np.uint8).reshape(H, W).astype(np.float32)


def right_edge(img, y: float) -> float:
    """该层右缘：在 x<0.72W 内找**连续 ≥4 行都亮**的最右列——
    排除细引线本身（它只有 1–2 行高），也避免把右缘截断。"""
    lo, hi = max(0, int(y) - 4), min(H, int(y) + 5)
    strip = img[lo:hi, : int(W * 0.72)]
    solid = (strip > 88).sum(axis=0) >= 4      # 竖直方向成块 = 实体，不是线
    xs = np.where(solid)[0]
    return float(xs.max()) if len(xs) else 400.0


def main() -> int:
    img = gray(PROBE)
    # 版式：单行内联字号（中文 17 + 英文 10 同行），块高 ≈17px。
    # 为什么不用 \N 双行：实测顶盖引线 y=73、电源层中心 y=84，**两层只差 11px**，
    # 双行块高 27px 必然重叠（成片上就是两行挤成一团）。单行才容得下近邻层。
    head = f"""[Script Info]
ScriptType: v4.00+
PlayResX: {W}
PlayResY: {H}
WrapStyle: 2
ScaledBorderAndShadow: yes

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Label,Microsoft YaHei,17,&H00F2F2F2,&H00F2F2F2,&H00000000,&H00000000,-1,0,0,0,100,100,0.6,0,1,0.7,0,4,0,0,0,134
Style: Lead,Microsoft YaHei,1,&H009A9A9A,&H00000000,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,1.0,0,4,0,0,0,134

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
"""
    # 强制最小间距：块高 17px → 相邻标签至少隔 19px，否则往下推。
    # 引线仍指向**实测层位**，只有标签文字错开（工程图的常规做法）。
    MIN_GAP = 19.0
    placed = []
    for (zh, en, y, has_line) in ROWS:
        yy = y if not placed else max(y, placed[-1] + MIN_GAP)
        placed.append(yy)

    ev = []
    for i, ((zh, en, y, has_line), yy) in enumerate(zip(ROWS, placed)):
        t = T0 + STEP * i
        if not has_line:
            x0 = right_edge(img, y)
            # 引线画到**实测层位 y**，不是标签位 yy：线贴部件才有指代关系
            ev.append(f"Dialogue: 1,{cs(t)},{cs(T_END)},Lead,,0,0,0,,"
                      f"{{\\p1\\fad(180,180)}}m {x0:.0f} {y:.0f} l {LX:.0f} {y:.0f}{{\\p0}}")
            note = f"补画线 x {x0:.0f}→{LX:.0f}"
        else:
            note = "吸附模型线"
        shift = f"  ⚠让位 +{yy-y:.0f}px" if yy - y > 0.5 else ""
        print(f"  {zh:8s} 层 y={y:5.1f} 标签 y={yy:5.1f}  {note}{shift}")
        # \an4 = 左对齐 + 垂直居中；\fs10 只压英文，中文保持 17（坑 2）
        ev.append(f"Dialogue: 0,{cs(t)},{cs(T_END)},Label,,0,0,0,,"
                  f"{{\\an4\\pos({TX:.0f},{yy:.0f})\\fad(180,180)}}"
                  f"{zh}{{\\fs10\\fnArial \\c&H9A9A9A&}} {en}{{\\r}}")

    with open(OUT, "w", encoding="utf-8-sig") as f:
        f.write(head + "\n".join(ev) + "\n")
    print(f"\n写出 {OUT}：{len(ROWS)} 组标签，时间列样例 {cs(T0)}–{cs(T_END)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

