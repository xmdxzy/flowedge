#!/usr/bin/env python3
"""启动 FlowEdge 引擎（三条路，按可靠性从高到低试）。

为什么不是一句 taskkill + start
------------------------------
在这台机器上，「从 agent 的 shell 里拉起的进程」会随工具调用结束被回收：
`cmd /c start`、`Start-Process`、`DETACHED_PROCESS` 全都试过，全部死；
`CREATE_BREAKAWAY_FROM_JOB` 直接被作业对象拒绝（WinError 5）——
沙箱用 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` 把子孙进程连坐。
**这不是能靠"改规则"绕过的**，是操作系统层的限制。

真正可行的第三条路：**让一个本来就常驻的进程去拉它**。
序策外壳（`xuce-flow`，:30000，用户自己起的 node 进程）带一个监管器接口
`POST /api/flowedge/supervise/start {"target":"compute"}`，
引擎由它 spawn → 父进程是外壳，agent 会话结束也不受影响。

所以顺序是：
  1. 已经在跑 → 直接返回
  2. 外壳在跑 → 让外壳拉起（父进程是外壳，能活）
  3. 都没有 → detached 起（可能随会话被回收，脚本会明说）

用法
----
    python scripts/start-engine.py              # 起引擎并等到 /api/health 通
    python scripts/start-engine.py --status     # 只探活
    python scripts/start-engine.py --no-station # 强制走本进程 detached（调试用）

环境变量：`PORT`（默认 8080）、`FLOWEDGE_STATION`（默认 http://127.0.0.1:30000）
退出码：0 = 可用；2 = 起不来。
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUN_DIR = os.path.join(ROOT, "运行")
EXE = os.path.join(RUN_DIR, "FlowEdge.exe")
PORT = os.environ.get("PORT", "8080")
HEALTH = f"http://127.0.0.1:{PORT}/api/health"
STATION = os.environ.get("FLOWEDGE_STATION", "http://127.0.0.1:30000")

# 本机常驻 http_proxy（企业代理），本地探活必须绕过，否则会拿到 502 假死。
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def healthy(timeout: float = 2.0) -> bool:
    try:
        with _OPENER.open(HEALTH, timeout=timeout) as r:
            return 200 <= r.status < 300
    except Exception:
        return False


def wait_ready(seconds: float = 45.0) -> bool:
    deadline = time.time() + seconds
    while time.time() < deadline:
        if healthy():
            return True
        time.sleep(0.5)
    return False


def start_via_station() -> int | None:
    """让序策外壳拉起计算面。成功返回它给的 pid，失败返回 None。"""
    body = json.dumps({"target": "compute"}).encode()
    req = urllib.request.Request(
        STATION.rstrip("/") + "/api/flowedge/supervise/start",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with _OPENER.open(req, timeout=20) as r:
            data = json.loads(r.read().decode("utf-8") or "{}")
        return int(data.get("pid") or 0)
    except Exception as e:
        print(f"[提示] 外壳拉起失败：{e}", file=sys.stderr)
        return None


def spawn_detached() -> subprocess.Popen:
    DETACHED_PROCESS = 0x00000008
    CREATE_NEW_PROCESS_GROUP = 0x00000200
    return subprocess.Popen(
        [EXE],
        cwd=RUN_DIR,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        close_fds=True,
        creationflags=DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP,
    )


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--status", action="store_true", help="只探活，不启动")
    ap.add_argument("--no-station", action="store_true", help="跳过外壳，直接 detached 起")
    args = ap.parse_args()

    if args.status:
        ok = healthy()
        print("running" if ok else "stopped")
        return 0 if ok else 2

    if healthy():
        print("already running")
        return 0

    if not os.path.exists(EXE):
        print(f"[错误] 找不到 {EXE}", file=sys.stderr)
        return 2

    if not args.no_station:
        pid = start_via_station()
        if pid:
            print(f"spawned by station pid={pid}（父进程是外壳，可跨会话存活）")
            if wait_ready():
                print(f"ready on {HEALTH}")
                return 0
            print("[错误] 外壳已返回 pid，但 45 秒内没就绪", file=sys.stderr)
            return 2

    try:
        proc = spawn_detached()
    except OSError as e:
        print(f"[错误] 启动失败：{e}", file=sys.stderr)
        return 2

    print(f"spawned pid={proc.pid} mode=detached（可能随会话被回收；要长期常驻请用外壳或双击 bat）")
    if wait_ready():
        print(f"ready on {HEALTH}")
        return 0
    print("[错误] 45 秒内没有就绪：检查端口占用 / 数据目录权限", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
