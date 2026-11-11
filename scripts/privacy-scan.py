#!/usr/bin/env python3
"""公开仓库体检：拦住个人信息、日志、以及不该发出去的文件。

为什么要这个
------------
这个仓库本来是本地工具，开发机上到处是 C:\\Users\\<名字>、模型目录、会话记录。
手工检查总会漏。发版前跑一遍，比事后撤 tag 便宜。

看什么
------
- 文本：用户名（只允许 C:\\Users\\you 或占位符）、邮箱、局域网 IP、密钥样式、
  以及助手会话的痕迹。
- 文件：.log、数据库、docs/internal/、AGENTS.md 这类不该入库的东西。
- 只看 git 跟踪的文件；本地未跟踪的东西不管。

用法
----
    python scripts/privacy-scan.py          # 有问题退出码 1
    python scripts/privacy-scan.py --list   # 只列出来，不判错
"""
from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

TEXT_EXT = {".md", ".go", ".ts", ".tsx", ".js", ".mjs", ".json", ".yml", ".yaml",
            ".py", ".sh", ".ps1", ".bat", ".sql", ".html", ".css", ".txt", ".example"}

RULES = [
    # C:/Users/<名字> 或 C:\Users\<名字>；you 与 <占位> 放行
    ("个人目录", re.compile(r"C:[\\/]Users[\\/](?!you\b|<)([A-Za-z0-9._-]+)")),
    ("邮箱", re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")),
    ("局域网 IP", re.compile(r"\b(?:192\.168|10\.|172\.(?:1[6-9]|2\d|3[01]))\.\d+\.\d+\b")),
    ("疑似密钥", re.compile(r"(?:sk-[A-Za-z0-9]{20,}|gh[pousr]_[A-Za-z0-9]{30,})")),
    ("助手会话痕迹", re.compile(r"(?:\.workbuddy|\.dsh-|/AGENTS\.md|会话记录\s*[：:]|memory/MEMORY)")),
]

FORBIDDEN_PATHS = re.compile(
    r"(^|/)(AGENTS|STATE|engine-gap-audit|SOTA-达成度对照表)\.md$"
    r"|^docs/internal/"
    r"|\.log$|\.db$|\.bak-"
)


def tracked_files() -> list[str]:
    out = subprocess.run(["git", "ls-files"], cwd=ROOT, capture_output=True, text=True,
                         encoding="utf-8", errors="replace")
    if out.returncode != 0:
        print("  [FAIL] 读不到 git 索引：这不是一个 git 仓库？")
        sys.exit(2)
    return [f for f in out.stdout.splitlines() if f.strip()]


LINK = re.compile(r"\[[^\]]*\]\(([^)]+)\)")


def broken_links(rel: str, text: str) -> list[str]:
    """文档里的相对链接指向了不存在的文件。移动文档时最容易漏。"""
    bad = []
    for target in LINK.findall(text):
        if target.startswith(("http://", "https://", "#", "mailto:")):
            continue
        path = target.split("#")[0].strip()
        if not path:
            continue
        resolved = os.path.normpath(os.path.join(os.path.dirname(rel), path))
        if not os.path.exists(os.path.join(ROOT, resolved)):
            bad.append(f"{rel}: 链接指向不存在的文件 → {target}")
    return bad


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args()

    files = tracked_files()
    problems: list[str] = []

    for rel in files:
        # 这个脚本自己的规则字符串里就有这些关键词，不跳过会把自己判死
        if rel.endswith("scripts/privacy-scan.py"):
            continue
        if FORBIDDEN_PATHS.search(rel):
            problems.append(f"{rel}: 不该进公开仓库（内部档案/日志/数据库）")
            continue
        if os.path.splitext(rel)[1].lower() not in TEXT_EXT:
            continue
        path = os.path.join(ROOT, rel)
        try:
            with open(path, encoding="utf-8", errors="replace") as fh:
                text = fh.read()
        except OSError:
            continue
        for name, rx in RULES:
            m = rx.search(text)
            if not m:
                continue
            line_no = text[: m.start()].count("\n") + 1
            snippet = text.splitlines()[line_no - 1].strip()[:120]
            problems.append(f"{rel}:{line_no}: {name} → {snippet}")
        if rel.endswith(".md"):
            problems.extend(broken_links(rel, text))

    if args.list:
        for p in problems:
            print(p)
        print(f"\n{len(problems)} 处")
        return 0

    if problems:
        print(f"PRIVACY-SCAN: FAIL（{len(problems)} 处）")
        for p in problems[:40]:
            print("  " + p)
        if len(problems) > 40:
            print(f"  …还有 {len(problems) - 40} 处")
        return 1

    print(f"PRIVACY-SCAN: PASS（{len(files)} 个跟踪文件，无个人信息/日志）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
