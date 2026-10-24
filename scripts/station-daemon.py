#!/usr/bin/env python
"""工作站守护循环：保证 :30000 的序策工作站一直活着。

为什么需要它
------------
工作站是 agent 的**唯一** job 外派生出入口（沙箱会在每条命令结束时终止 job，
agent 自己 spawn 的常驻进程全被回收）。工作站一旦挂掉，整条链路就断了，
而 agent 无权把它拉回来 —— 只能等人。

所以让一个开机自启的守护进程盯着它：挂了就重拉。守护进程自己由 explorer
（启动项）拉起，天然在沙箱 job 之外。

用法
----
    python scripts/station-daemon.py            # 前台跑守护循环（自启项用这个）
    python scripts/station-daemon.py --once     # 只确保一次，用于脚本/排障
    python scripts/station-daemon.py --stop     # 按 pid 文件停掉守护

日志：运行/../data/station-daemon.log（超过 2MB 自动截断）
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DATA = os.path.join(ROOT, "data")
PID_FILE = os.path.join(DATA, "station-daemon.pid")
LOG = os.path.join(DATA, "station-daemon.log")
LOG_MAX = 2 * 1024 * 1024

STATION_JS = os.environ.get(
    "FLOWEDGE_STATION_JS",
    r"C:\Users\Administrator\Desktop\序策\xuce-flow\server\flowedge-station.js",
)
NODE_EXE = os.environ.get("FLOWEDGE_NODE", r"C:\Program Files\nodejs\node.exe")
STATION = os.environ.get("FLOWEDGE_STATION", "http://127.0.0.1:30000").rstrip("/")

_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def log(msg: str) -> None:
    line = "%s  %s\n" % (time.strftime("%Y-%m-%d %H:%M:%S"), msg)
    try:
        if os.path.exists(LOG) and os.path.getsize(LOG) > LOG_MAX:
            os.replace(LOG, LOG + ".1")
    except OSError:
        pass
    with open(LOG, "a", encoding="utf-8") as f:
        f.write(line)


def station_up(timeout=3.5) -> bool:
    try:
        with _OPENER.open(STATION + "/api/flowedge/supervise", timeout=timeout) as r:
            return 200 <= r.status < 500
    except Exception:
        return False


def pid_alive(pid: int) -> bool:
    try:
        out = subprocess.run(["tasklist", "/FI", "PID eq %d" % pid, "/FO", "CSV", "/NH"],
                             capture_output=True, text=True, timeout=10).stdout
        return str(pid) in out
    except Exception:
        return False


def running_daemon():
    if not os.path.exists(PID_FILE):
        return None
    try:
        pid = int(open(PID_FILE, encoding="utf-8").read().strip())
    except Exception:
        return None
    return pid if pid_alive(pid) else None


def spawn_station():
    if not os.path.exists(STATION_JS) or not os.path.exists(NODE_EXE):
        log("缺文件：node=%s station=%s" % (os.path.exists(NODE_EXE), os.path.exists(STATION_JS)))
        return None
    si = subprocess.STARTUPINFO()
    si.dwFlags |= subprocess.STARTF_USESHOWWINDOW
    si.wShowWindow = 0
    p = subprocess.Popen([NODE_EXE, STATION_JS],
                         cwd=os.path.dirname(os.path.dirname(STATION_JS)),
                         stdin=subprocess.DEVNULL,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         creationflags=0x08000000, startupinfo=si)  # CREATE_NO_WINDOW
    log("拉起工作站 pid=%d" % p.pid)
    return p


def ensure_once(wait=20) -> bool:
    if station_up():
        return True
    spawn_station()
    end = time.time() + wait
    while time.time() < end:
        time.sleep(0.5)
        if station_up():
            log("工作站就绪")
            return True
    log("工作站 %ds 内未就绪" % wait)
    return False


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--once", action="store_true")
    ap.add_argument("--stop", action="store_true")
    ap.add_argument("--interval", type=float, default=30.0)
    a = ap.parse_args()

    os.makedirs(DATA, exist_ok=True)

    if a.stop:
        pid = running_daemon()
        if pid:
            subprocess.run(["taskkill", "/F", "/PID", str(pid)], capture_output=True)
            log("守护已停止 pid=%d" % pid)
            print("stopped", pid)
        else:
            print("not running")
        try:
            os.remove(PID_FILE)
        except OSError:
            pass
        return 0

    if a.once:
        ok = ensure_once()
        print("station", "UP" if ok else "DOWN")
        return 0 if ok else 2

    me = running_daemon()
    if me:
        log("已有守护 pid=%d，本实例退出" % me)
        return 0
    with open(PID_FILE, "w", encoding="utf-8") as f:
        f.write(str(os.getpid()))
    log("守护启动 pid=%d interval=%.0fs" % (os.getpid(), a.interval))

    try:
        while True:
            if not station_up():
                log("工作站不在，重新拉起")
                ensure_once()
            time.sleep(a.interval)
    except KeyboardInterrupt:
        log("守护收到中断，退出")
    finally:
        try:
            os.remove(PID_FILE)
        except OSError:
            pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
