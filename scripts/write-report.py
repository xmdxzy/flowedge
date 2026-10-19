#!/usr/bin/env python3
"""写本次出片的验收报告（落盘可复核，不只看引擎返回 ok）。

为什么单独一个脚本：序策的 `write` 执行器只允许写项目目录内的路径，
而产物在 FlowEdge 工作区（可能跨盘/跨目录）。shell 执行器没有这个限制。

用法：python scripts/write-report.py --work-dir DIR --mode dialogue \
        --dialogue-video PATH --dialogue-gate true [--ambience-video PATH] [--ambience-gate true]
"""

from __future__ import annotations

import argparse
import json
import os
import sys


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--work-dir", required=True)
    ap.add_argument("--mode", default="")
    ap.add_argument("--dialogue-video", default="")
    ap.add_argument("--ambience-video", default="")
    ap.add_argument("--dialogue-gate", default="")
    ap.add_argument("--ambience-gate", default="")
    ap.add_argument("--frames", default="")
    args = ap.parse_args()

    report = {
        "mode": (args.mode or "").strip(),
        "frames": args.frames,
        "dialogue_video": args.dialogue_video.strip(),
        "ambience_video": args.ambience_video.strip(),
        "dialogue_gate": args.dialogue_gate.strip(),
        "ambience_gate": args.ambience_gate.strip(),
    }
    video = report["dialogue_video"] or report["ambience_video"]
    report["video"] = video
    report["video_exists"] = bool(video) and os.path.isfile(video)

    if not report["mode"]:
        print(json.dumps({"ok": False, "error": "模式为空：上游路由可能没命中"}, ensure_ascii=False))
        return 2
    if not report["video_exists"]:
        print(json.dumps({"ok": False, "error": "产物文件不存在", "video": video}, ensure_ascii=False))
        return 2

    os.makedirs(args.work_dir, exist_ok=True)
    out = os.path.join(args.work_dir, "report.json")
    with open(out, "w", encoding="utf-8") as f:
        json.dump(report, f, ensure_ascii=False, indent=2)
    print(json.dumps({"ok": True, "report": os.path.abspath(out), **report}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
