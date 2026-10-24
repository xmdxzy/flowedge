#!/usr/bin/env python3
"""提交前一键全检：把 5 条命令压成 1 条，输出压成 4 行。

为什么要有它
------------
以前每次改完代码要依次跑：go build、go test（挑包）、tsc --noEmit、前端契约测试、
有时再加在线验证。每条命令一次工具调用、一段输出进上下文——**判断没变，token 翻五倍**。
这个脚本在同一进程里串起来，只回「哪项 PASS / FAIL + 耗时」，失败才展开最后 12 行。

用法
----
    python scripts/check-all.py          # 后端 core 包 + 前端类型 + 契约测试（约 60s）
    python scripts/check-all.py --full   # 后端全包（含 engine_v2_test 等，约 120s）
    python scripts/check-all.py --fast   # 只编译 + 前端类型（约 25s，改前端时用）
    python scripts/check-all.py --live   # 追加起引擎 + 新接口在线冒烟（约 60s 额外）

退出码：0 = 全过；1 = 有 FAIL；2 = 环境缺失（找不到 go/node/tsc）。
"""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BACKEND = os.path.join(ROOT, "backend")
FRONTEND = os.path.join(ROOT, "frontend")
GO_CANDIDATES = [
    os.path.expanduser(r"~\AppData\Local\Programs\Go\bin"),
    r"C:\Program Files\Go\bin",
    "/usr/local/go/bin",
]
TAIL = 12  # 失败时最多展开的行数——够定位，不淹上下文


def log(msg: str) -> None:
    print(msg, flush=True)


def run(name: str, cmd: list[str], cwd: str, env: dict | None = None) -> bool:
    """跑一条检查，只回一行结论；失败才展开末尾。"""
    t0 = time.time()
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True,
                       encoding="utf-8", errors="replace", env=env)
    dt = time.time() - t0
    out = ((p.stdout or "") + (p.stderr or "")).strip()
    if p.returncode == 0:
        log(f"  [PASS] {name}  ({dt:.1f}s)")
        return True
    log(f"  [FAIL] {name}  ({dt:.1f}s, exit={p.returncode})")
    lines = [l for l in out.splitlines() if l.strip()]
    for l in lines[-TAIL:]:
        log(f"         {l}")
    return False


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--full", action="store_true", help="后端全包（慢但全）")
    ap.add_argument("--fast", action="store_true", help="只编译 + 前端类型")
    ap.add_argument("--live", action="store_true", help="追加起引擎 + canonical 在线验证")
    args = ap.parse_args()

    env = dict(os.environ)
    go = shutil.which("go")
    if not go:
        for d in GO_CANDIDATES:
            cand = os.path.join(d, "go.exe" if os.name == "nt" else "go")
            if os.path.exists(cand):
                env["PATH"] = d + os.pathsep + env.get("PATH", "")
                go = cand
                break
    node = shutil.which("node")
    tsc = os.path.join(FRONTEND, "node_modules", ".bin", "tsc"
                       + (".cmd" if os.name == "nt" else ""))
    if not (go and node):
        log("  [SKIP] 环境缺失：找不到 go 或 node（先确认工具链，再跑本脚本）")
        return 2

    log(f"[check-all] fast={args.fast} full={args.full} live={args.live}")
    ok = True

    ok &= run("go build ./...", [go, "build", "./..."], BACKEND, env)
    if not args.fast:
        pkgs = "./..." if args.full else "./service/ ./handler/"
        targets = [pkgs] if args.full else ["./service/", "./handler/"]
        ok &= run(f"go test {pkgs}", [go, "test"] + targets, BACKEND, env)

    if os.path.exists(tsc):
        ok &= run("tsc --noEmit", [tsc, "--noEmit"], FRONTEND, env)
    else:
        log("  [SKIP] tsc 未安装（frontend/node_modules 缺）")
    if not args.fast:
        ok &= run("前端用例（node:test）", [node, "scripts/run-tests.mjs"], FRONTEND, env)

    if args.live:
        py = sys.executable
        # 同一进程内起引擎再验证：沙箱会在工具调用结束时回收引擎，跨调用必失败。
        run("start-engine", [py, os.path.join(ROOT, "scripts", "start-engine.py")], ROOT, env)
        ok &= run("canonical 在线 35 断言",
                  [py, os.path.join(ROOT, "scripts", "verify-canonical-live.py")], ROOT, env)

    log("RESULT: " + ("ALL-PASS" if ok else "HAS-FAIL"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())

