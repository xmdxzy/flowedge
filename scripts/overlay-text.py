#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""给成片贴确定性文字（标题/招牌/字幕），零 GPU、逐帧精确。

为什么需要它：H3 是扩散类音视频模型，没有字形条件也没有版式控制，
让它"画字"只能赌。文字要清晰可读，只有两条确定性路线：
  ① 本脚本：成片之后用 ffmpeg 把字贴上去（推荐，字型/位置/时点全部可控）
  ② MOSS 出带字首帧 → H3 图生视频（字在景里，但要求镜头基本不动）

用法：
  python scripts/overlay-text.py --in 成片.mp4 --text "山海茶肆" --out 带字.mp4
  python scripts/overlay-text.py --in 成片.mp4 --text-file 标题.txt --position top --size 64
  python scripts/overlay-text.py --in 成片.mp4 --srt 台词.srt --out 带字幕.mp4
  python scripts/overlay-text.py --in 成片.mp4 --text "样板" --from 0 --to 3 --box

纪律（AGENTS §0.5 / §0.6）：
  - 贴完立刻 ffprobe 核「视频流帧数 + 时长 + 音频流」，不靠退出码判定；
  - 输入不可读 / 字体缺失 / 输出无视频流 → 一律 FAIL 退出 1，不静默给个坏文件。
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile

DEFAULT_FONTS = [
    r"C:/Windows/Fonts/msyh.ttc",      # 微软雅黑
    r"C:/Windows/Fonts/msyhbd.ttc",    # 微软雅黑 Bold
    r"C:/Windows/Fonts/simhei.ttf",    # 黑体
]


def die(msg, code=1):
    print("FAIL: " + msg, file=sys.stderr)
    sys.exit(code)


def find_ffmpeg(explicit=None):
    path = explicit or shutil.which("ffmpeg")
    if not path or not os.path.isfile(path):
        die("找不到 ffmpeg；用 --ffmpeg 指定绝对路径")
    return path


def find_ffprobe(ffmpeg):
    """ffprobe 通常在 ffmpeg 同目录。"""
    cand = os.path.join(os.path.dirname(ffmpeg), "ffprobe.exe" if os.name == "nt" else "ffprobe")
    if os.path.isfile(cand):
        return cand
    path = shutil.which("ffprobe")
    if path and os.path.isfile(path):
        return path
    die("找不到 ffprobe（贴完要核帧数与流列表，不能跳过）")


def pick_font(explicit=None):
    if explicit:
        if not os.path.isfile(explicit):
            die("指定的字体不存在：" + explicit)
        return explicit.replace("\\", "/")
    for f in DEFAULT_FONTS:
        if os.path.isfile(f):
            return f
    die("没找到中文字体（试过 msyh / msyhbd / simhei）；用 --font 指定")


def filter_escape_path(p):
    """ffmpeg 滤镜参数里的路径：正斜杠 + 盘符冒号转义 + 整体单引号。

    实测（本机 ffmpeg 8.x）：只转义冒号不加引号会失败（`fontfile=C\\:/...` → 把
    Windows/Fonts... 当成选项名）；`fontfile='C\\:/...'` 才是可用写法。
    """
    return "'" + p.replace("\\", "/").replace(":", r"\:") + "'"


def probe(ffprobe, path):
    out = subprocess.run(
        [ffprobe, "-v", "error", "-show_streams", "-show_format", "-of", "json", path],
        capture_output=True,
    )
    if out.returncode != 0:
        die("ffprobe 读不了输入：" + out.stderr.decode("utf-8", "replace")[:300])
    return json.loads(out.stdout.decode("utf-8", "replace"))


def stream_summary(info, path):
    vs = [s for s in info["streams"] if s.get("codec_type") == "video"]
    aus = [s for s in info["streams"] if s.get("codec_type") == "audio"]
    if not vs:
        die("输入没有视频流：" + path)
    v = vs[0]
    frames = v.get("nb_frames")
    if frames in (None, "N/A"):
        # 有些封装不写 nb_frames：用 -count_frames 精确数
        return {"width": v.get("width"), "height": v.get("height"),
                "duration": float(info["format"].get("duration") or 0),
                "nb_frames": None, "audio": len(aus)}
    return {"width": v.get("width"), "height": v.get("height"),
            "duration": float(info["format"].get("duration") or 0),
            "nb_frames": int(frames), "audio": len(aus)}


def count_frames(ffprobe, path):
    out = subprocess.run(
        [ffprobe, "-v", "error", "-select_streams", "v:0", "-count_frames",
         "-show_entries", "stream=nb_read_frames", "-of", "json", path],
        capture_output=True,
    )
    try:
        return int(json.loads(out.stdout.decode("utf-8", "replace"))["streams"][0]["nb_read_frames"])
    except Exception:
        return None


def main():
    ap = argparse.ArgumentParser(description="给成片贴确定性文字/字幕（零 GPU）")
    ap.add_argument("--in", dest="src", required=True, help="输入视频")
    ap.add_argument("--out", dest="dst", help="输出视频（默认 <原名>-text.mp4）")
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--text", help="要贴的文字（换行用 \\n）")
    g.add_argument("--text-file", help="文字文件（UTF-8，支持多行；推荐，免转义）")
    g.add_argument("--srt", help="字幕文件（.srt/.ass，按时间轴烧进画面）")
    ap.add_argument("--position", default="bottom", choices=["top", "center", "bottom"],
                    help="文字位置（默认 bottom）")
    ap.add_argument("--size", type=int, default=0, help="字号 px（默认按画面高度自动：h/22）")
    ap.add_argument("--font", help="字体文件（默认 msyh.ttc）")
    ap.add_argument("--color", default="white", help="字色（默认 white）")
    ap.add_argument("--border", type=int, default=0, help="描边宽度 px（默认 0）")
    ap.add_argument("--box", action="store_true", help="加半透明底板（提高可读性）")
    ap.add_argument("--from", dest="t_from", type=float, default=0.0, help="出现时间（秒）")
    ap.add_argument("--to", dest="t_to", type=float, default=-1.0, help="消失时间（秒，-1=到片尾）")
    ap.add_argument("--crf", type=int, default=18, help="重编码质量（默认 18）")
    ap.add_argument("--ffmpeg", help="ffmpeg 绝对路径")
    args = ap.parse_args()

    ffmpeg = find_ffmpeg(args.ffmpeg)
    ffprobe = find_ffprobe(ffmpeg)

    if not os.path.isfile(args.src):
        die("输入不存在：" + args.src)
    src_info = probe(ffprobe, args.src)
    src_sum = stream_summary(src_info, args.src)

    dst = args.dst or os.path.splitext(args.src)[0] + "-text.mp4"
    if os.path.abspath(dst) == os.path.abspath(args.src):
        die("输出不能覆盖输入（先写新文件再替换会更安全）")

    width, height = src_sum["width"] or 0, src_sum["height"] or 0
    size = args.size or max(16, int(round(height / 22.0)))
    # 中文一行字宽 ≈ 字号；留 8% 边距
    margin = max(10, int(round(height * 0.08)))
    if args.position == "bottom":
        y = "h-th-%d" % margin
    elif args.position == "top":
        y = "%d" % margin
    else:
        y = "(h-th)/2"

    tmpdir = tempfile.mkdtemp(prefix="overlay-")
    try:
        if args.srt:
            if not os.path.isfile(args.srt):
                die("字幕文件不存在：" + args.srt)
            style = "FontName=Microsoft YaHei,FontSize=%d,PrimaryColour=&H00FFFFFF,Outline=%d,BorderStyle=%d" % (
                size, max(1, args.border or 2), 1)
            vf = "subtitles=%s:force_style='%s'" % (filter_escape_path(os.path.abspath(args.srt)), style)
            mode_desc = "SRT 按时间轴"
        else:
            if args.text_file:
                if not os.path.isfile(args.text_file):
                    die("文字文件不存在：" + args.text_file)
                text_path = os.path.abspath(args.text_file)
            else:
                text_path = os.path.join(tmpdir, "text.txt")
                with open(text_path, "w", encoding="utf-8", newline="\n") as f:
                    f.write((args.text or "").replace("\\n", "\n"))
            font = filter_escape_path(pick_font(args.font))
            parts = [
                "textfile=%s" % filter_escape_path(text_path),
                "fontfile=%s" % font,
                "fontsize=%d" % size,
                "fontcolor=%s" % args.color,
                "x=(w-text_w)/2",
                "y=%s" % y,
                "line_spacing=%d" % max(4, size // 4),
            ]
            if args.border > 0:
                parts.append("borderw=%d" % args.border)
                parts.append("bordercolor=black")
            if args.box:
                parts.append("box=1")
                parts.append("boxcolor=black@0.45")
                parts.append("boxborderw=%d" % max(8, size // 3))
            if args.t_to < 0 and args.t_from <= 0:
                pass  # 全程显示
            else:
                end = args.t_to if args.t_to >= 0 else max(src_sum["duration"], 0) + 1
                parts.append("enable='between(t,%.3f,%.3f)'" % (args.t_from, end))
            vf = "drawtext=" + ":".join(parts)
            mode_desc = "drawtext（%s）" % args.position

        cmd = [
            ffmpeg, "-y", "-hide_banner", "-loglevel", "error",
            "-i", args.src,
            "-vf", vf,
            "-c:v", "libx264", "-preset", "medium", "-crf", str(args.crf), "-pix_fmt", "yuv420p",
            "-c:a", "copy",
            "-movflags", "+faststart",
            dst,
        ]
        print("[1/3] 贴字：%s  字高=%dpx  %dx%d" % (mode_desc, size, width, height))
        run = subprocess.run(cmd, capture_output=True)
        if run.returncode != 0:
            die("ffmpeg 失败：" + run.stderr.decode("utf-8", "replace")[:800])

        print("[2/3] 核验输出")
        out_info = probe(ffprobe, dst)
        out_sum = stream_summary(out_info, dst)
        frames_in = src_sum["nb_frames"] or count_frames(ffprobe, args.src)
        frames_out = out_sum["nb_frames"] or count_frames(ffprobe, dst)
        problems = []
        if frames_in is not None and frames_out is not None and frames_in != frames_out:
            problems.append("帧数变了：%s → %s" % (frames_in, frames_out))
        if abs(out_sum["duration"] - src_sum["duration"]) > 0.15:
            problems.append("时长漂移 %.2fs → %.2fs" % (src_sum["duration"], out_sum["duration"]))
        if src_sum["audio"] > 0 and out_sum["audio"] == 0:
            problems.append("音频流丢了")
        if (out_sum["width"], out_sum["height"]) != (width, height):
            problems.append("分辨率变了")
        if problems:
            die("输出不合格：" + "；".join(problems))

        print("[3/3] 通过：%s" % dst)
        print("      帧数 %s | 时长 %.2fs | 音频流 %d | %dx%d" % (
            frames_out, out_sum["duration"], out_sum["audio"], out_sum["width"], out_sum["height"]))
        print("      重跑只需改 --size/--position/--from/--to，不必重新出片")
        return 0
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
