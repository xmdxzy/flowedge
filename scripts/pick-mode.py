#!/usr/bin/env python3
"""判定当前任务该走「台词」还是「氛围」模式 —— 序策图里的分支依据。

判定规则（确定性、0 token）：
  1. --mode dialogue / ambience 显式指定 → 直接用
  2. --mode auto（默认）：
     - 有台词文件且至少 1 条非空 text → dialogue
     - 否则 → ambience

为什么不让 LLM 判：这是可机械判定的事实（有没有台词文本），
用 LLM 既慢又可能飘；分支错一次要烧 3 分钟显卡。

输出：stdout 只打印 `dialogue` 或 `ambience`（供 router 的边 label 匹配）。
"""

from __future__ import annotations

import argparse
import json
import os
import sys


def has_lines(path: str) -> bool:
    if not path or not os.path.isfile(path):
        return False
    try:
        with open(path, encoding="utf-8") as f:
            data = json.load(f)
    except Exception:
        return False
    if isinstance(data, dict):
        data = data.get("lines", [])
    if not isinstance(data, list):
        return False
    return any(str((x or {}).get("text", "")).strip() for x in data if isinstance(x, dict))


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", default="auto", help="auto | dialogue | ambience（空值按 auto 处理）")
    ap.add_argument("--lines-file", default="", help="台词表 JSON")
    args = ap.parse_args()

    mode = (args.mode or "auto").strip().lower()
    if mode not in ("auto", "dialogue", "ambience"):
        mode = "auto"
    if mode in ("dialogue", "ambience"):
        print(mode)
        return 0
    if False:
        print(args.mode)
        return 0
    print("dialogue" if has_lines(args.lines_file) else "ambience")
    return 0


if __name__ == "__main__":
    sys.exit(main())
