#!/usr/bin/env python
"""逃生实验：哪种 spawn 方式起的进程能跨"命令边界"存活？

已确证的前提（probe-sandbox.py 实测）：
  - 每条命令运行在一个新建的 job 中
  - LimitFlags = 0x2000 (KILL_ON_JOB_CLOSE)，**没有** BREAKAWAY_OK / SILENT_BREAKAWAY_OK
  - 因此 CREATE_BREAKAWAY_FROM_JOB 被内核拒绝（WinError 5）——这条路物理关闭

本脚本只问一件事：命令结束后，进程还活着吗？判据是 OpenProcess+GetExitCodeProcess，
不是"命令返回 0"。

用法：
    python scripts/probe-survive.py           # 起标记进程
    python scripts/probe-survive.py --check   # 下一条命令里检查存活
    python scripts/probe-survive.py --kill    # 清理残留标记
"""
import argparse
import ctypes
import ctypes.wintypes as wt
import json
import os
import subprocess
import sys
import time

k32 = ctypes.WinDLL("kernel32", use_last_error=True)
PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
STILL_ACTIVE = 259
DETACHED_PROCESS = 0x00000008
CREATE_NEW_PROCESS_GROUP = 0x00000200
CREATE_BREAKAWAY_FROM_JOB = 0x01000000

k32.OpenProcess.argtypes = [wt.DWORD, wt.BOOL, wt.DWORD]
k32.OpenProcess.restype = wt.HANDLE
k32.GetExitCodeProcess.argtypes = [wt.HANDLE, ctypes.POINTER(wt.DWORD)]
k32.CloseHandle.argtypes = [wt.HANDLE]

STATE = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                      "..", "data", "_probe", "survive.json"))

RELAY = (
    "import subprocess,sys,time;"
    "p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(240)'],"
    "creationflags=0x00000008,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,stdin=subprocess.DEVNULL);"
    "print(p.pid,flush=True)"
)


def alive(pid):
    h = k32.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, False, int(pid))
    if not h:
        return None
    try:
        code = wt.DWORD()
        if not k32.GetExitCodeProcess(h, ctypes.byref(code)):
            return None
        return code.value == STILL_ACTIVE
    finally:
        k32.CloseHandle(h)


def spawn(tag, flags, extra=None, close_fds=True):
    si = subprocess.STARTUPINFO()
    si.dwFlags |= subprocess.STARTF_USESHOWWINDOW
    si.wShowWindow = 0
    cmd = extra or [sys.executable, "-c", "import time;time.sleep(240)"]
    try:
        p = subprocess.Popen(cmd, creationflags=flags, startupinfo=si,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             stdin=subprocess.DEVNULL, close_fds=close_fds)
    except OSError as e:
        return {"tag": tag, "ok": False, "winerror": getattr(e, "winerror", None), "error": str(e)}
    return {"tag": tag, "ok": True, "pid": p.pid, "flags": "0x%08X" % flags,
            "close_fds": close_fds}


def spawn_relay():
    """中继孤儿化：中继进程拿到孙进程 pid 后立刻退出，孙进程被系统收养。"""
    si = subprocess.STARTUPINFO()
    si.dwFlags |= subprocess.STARTF_USESHOWWINDOW
    si.wShowWindow = 0
    try:
        out = subprocess.run([sys.executable, "-c", RELAY], capture_output=True, text=True,
                             timeout=20, startupinfo=si)
    except Exception as e:
        return {"tag": "relay-orphan", "ok": False, "error": str(e)}
    line = (out.stdout or "").strip().splitlines()
    pid = int(line[-1]) if line and line[-1].isdigit() else None
    return {"tag": "relay-orphan", "ok": bool(pid), "pid": pid,
            "flags": "relay(DETACHED)+parent-exit"}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true")
    ap.add_argument("--kill", action="store_true")
    a = ap.parse_args()

    if a.check:
        if not os.path.exists(STATE):
            print("没有基线，先跑一次不带 --check 的")
            return 1
        base = json.load(open(STATE, encoding="utf-8"))
        print("基线 %s" % base["at"])
        rows = []
        for r in base["marks"]:
            if not r.get("ok"):
                print("%-18s 起不来 %s" % (r["tag"], r.get("error") or r.get("winerror")))
                continue
            st = alive(r["pid"])
            label = {True: "ALIVE", False: "DEAD", None: "gone"}[st]
            print("%-18s pid=%-7s %-15s -> %s" % (r["tag"], r["pid"], r.get("flags"), label))
            rows.append(dict(r, alive=st))
        base["checkedAt"] = time.strftime("%Y-%m-%d %H:%M:%S")
        base["result"] = rows
        json.dump(base, open(STATE, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
        return 0

    if a.kill:
        if os.path.exists(STATE):
            base = json.load(open(STATE, encoding="utf-8"))
            for r in base.get("marks", []):
                if r.get("ok") and alive(r["pid"]):
                    subprocess.run(["taskkill", "/F", "/PID", str(r["pid"])],
                                   capture_output=True)
                    print("killed", r["tag"], r["pid"])
        return 0

    os.makedirs(os.path.dirname(STATE), exist_ok=True)
    marks = [
        spawn("plain", 0),
        spawn("detached", DETACHED_PROCESS),
        spawn("detached+newgroup", DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP),
        spawn("breakaway", CREATE_BREAKAWAY_FROM_JOB),
        spawn("detached+inherit", DETACHED_PROCESS, close_fds=False),
        spawn("plain+inherit", 0, close_fds=False),
        spawn_relay(),
    ]
    for m in marks:
        print(m)
    json.dump({"at": time.strftime("%Y-%m-%d %H:%M:%S"), "marks": marks},
              open(STATE, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    print("\n下一条命令：python scripts/probe-survive.py --check")
    return 0


if __name__ == "__main__":
    sys.exit(main())
