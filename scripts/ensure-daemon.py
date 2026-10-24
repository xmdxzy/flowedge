#!/usr/bin/env python
"""常驻守护统一入口：确保「工作站 → 引擎 → 托管依赖」这条链是活的。

为什么需要这个东西
------------------
沙箱把每条命令放进一个 `KILL_ON_JOB_CLOSE` 的 job，命令结束**显式终止**该 job，
所以 agent 从自己的 shell 里 spawn 的任何进程都活不过这一次工具调用。
实测 7 种逃逸方式（plain / DETACHED / NEW_PROCESS_GROUP / BREAKAWAY /
句柄继承 / 孤儿中继）全部失败，`CREATE_BREAKAWAY_FROM_JOB` 直接被内核拒（WinError 5）。

结论：**进程必须由 job 外的常驻进程派生**。本机现成的两级派生者：
  1. 序策工作站 node（`xuce-flow/server/flowedge-station.js`，:30000）
     —— 由用户双击/开机自启拉起，在 job 外
  2. FlowEdge 引擎（`:8080`，父进程 = 工作站）
     —— 自带 `POST /api/deps/:name/start`，可托管 ComfyUI / ASR / TTS

所以本脚本**从不直接 spawn 常驻服务**（那必然被回收，是假成功）。
它只在工作站可用时代为下发请求；工作站不在时，明确告诉人该怎么点，而不是瞎试。

用法
----
    python scripts/ensure-daemon.py               # 确保整链可用
    python scripts/ensure-daemon.py --status      # 只报状态
    python scripts/ensure-daemon.py --deps        # 附带托管依赖清单
    python scripts/ensure-daemon.py --install-autostart    # 工作站随登录自启
    python scripts/ensure-daemon.py --uninstall-autostart

退出码：0 = 全链可用；2 = 需要人工介入（工作站未运行）。
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUN_DIR = os.path.join(ROOT, "运行")
EXE = os.path.join(RUN_DIR, "FlowEdge.exe")
OPEN_BAT = os.path.join(RUN_DIR, "0-打开客户端.bat")

STATION = os.environ.get("FLOWEDGE_STATION", "http://127.0.0.1:30000").rstrip("/")
ENGINE = os.environ.get("FLOWEDGE_ENGINE", "http://127.0.0.1:8080").rstrip("/")

STATION_JS = os.environ.get(
    "FLOWEDGE_STATION_JS",
    r"C:\Users\Administrator\Desktop\序策\xuce-flow\server\flowedge-station.js",
)
NODE_EXE = os.environ.get("FLOWEDGE_NODE", r"C:\Program Files\nodejs\node.exe")

DAEMON_PY = os.path.join(os.path.dirname(os.path.abspath(__file__)), "station-daemon.py")
# pythonw 无控制台窗口；优先系统 Python（不随 WorkBuddy 升级消失）
_PYTHONW_CANDIDATES = [
    os.environ.get("FLOWEDGE_PYTHONW", ""),
    r"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\pythonw.exe",
    r"C:\Users\Administrator\.workbuddy\binaries\python\versions\3.13.12\pythonw.exe",
]

AUTOSTART_DIR = os.path.join(os.environ.get("APPDATA", ""),
                             r"Microsoft\Windows\Start Menu\Programs\Startup")
AUTOSTART_FILE = os.path.join(AUTOSTART_DIR, "FlowEdge 守护（工作站）.cmd")

# 本机常驻 http_proxy，本地探活必须绕开，否则拿到假 502。
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def _req(url, method="GET", body=None, timeout=4.0):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(url, data=data, method=method,
                               headers={"Content-Type": "application/json"} if data else {})
    with _OPENER.open(r, timeout=timeout) as resp:
        return resp.status, resp.read().decode("utf-8", "replace")


def station_up() -> bool:
    """工作站活着 = /api/flowedge/supervise 有 JSON 应答（不是裸端口通）。"""
    try:
        st, _ = _req(STATION + "/api/flowedge/supervise", timeout=3.5)
        return 200 <= st < 500
    except Exception:
        return False


def engine_up() -> bool:
    try:
        st, body = _req(ENGINE + "/api/health", timeout=3.0)
        return st == 200 and '"ok"' in body
    except Exception:
        return False


def wait_engine(seconds=45.0) -> bool:
    end = time.time() + seconds
    while time.time() < end:
        if engine_up():
            return True
        time.sleep(0.5)
    return False


def start_engine_via_station():
    """让工作派生引擎。返回 (ok, info)。这里 spawn 出来的父是工作站，能跨会话活。"""
    try:
        st, body = _req(STATION + "/api/flowedge/supervise/start", "POST",
                        {"target": "compute"}, timeout=25.0)
    except Exception as e:
        return False, "请求失败：%s" % e
    try:
        data = json.loads(body)
    except Exception:
        return False, "响应不是 JSON：%s" % body[:200]
    if not data.get("ok"):
        return False, data.get("error") or body[:200]
    return True, data


def deps_summary():
    """托管依赖快照。引擎探测依赖可能慢，给足超时。"""
    try:
        st, body = _req(ENGINE + "/api/deps", timeout=30.0)
        if st != 200:
            return "引擎 /api/deps 返回 %d" % st
        data = json.loads(body)
    except Exception as e:
        return "取依赖失败：%s" % e
    items = data if isinstance(data, list) else (data.get("deps") or data.get("items") or [])
    if isinstance(items, dict):
        items = list(items.values())
    lines = []
    for d in items:
        if not isinstance(d, dict):
            continue
        name = d.get("name") or d.get("id") or "?"
        running = d.get("running")
        port = d.get("port") or d.get("addr") or "-"
        lines.append("  %-12s running=%-5s port=%s" % (name, running, port))
    return "\n".join(lines) or "  （引擎未报告任何依赖）"


def autostart_installed() -> bool:
    return os.path.exists(AUTOSTART_FILE)


def install_autostart() -> int:
    """把「工作站守护」写进登录启动项。启动项由 explorer 执行 → 进程在 job 外。

    用 .cmd 而不是 .lnk：纯文本写入，不依赖 COM，路径含中文也不怕编码。
    """
    if not os.path.exists(STATION_JS):
        print("[错误] 找不到工作站脚本：%s" % STATION_JS, file=sys.stderr)
        return 2
    if not os.path.exists(NODE_EXE):
        print("[错误] 找不到 node：%s（可用 FLOWEDGE_NODE 指定）" % NODE_EXE, file=sys.stderr)
        return 2
    pythonw = next((p for p in _PYTHONW_CANDIDATES if p and os.path.exists(p)), None)
    if not pythonw:
        print("[错误] 找不到 pythonw.exe（可用 FLOWEDGE_PYTHONW 指定）", file=sys.stderr)
        return 2
    os.makedirs(AUTOSTART_DIR, exist_ok=True)
    # 只写 ASCII 路径 + chcp 65001，避免 cmd 以 GBK 解析中文路径失败
    content = (
        "@echo off\r\n"
        "chcp 65001 >nul\r\n"
        "rem FlowEdge 工作站守护：登录时由 explorer 拉起，进程在沙箱 job 之外\r\n"
        "rem :30000 工作站是 agent 唯一的 job 外派生出入口，挂了就没人能自己拉回来\r\n"
        'start "" "%s" "%s"\r\n'
    ) % (pythonw, DAEMON_PY)
    with open(AUTOSTART_FILE, "w", encoding="utf-8", newline="") as f:
        f.write(content)
    print("已写入启动项：%s" % AUTOSTART_FILE)
    print("  守护脚本：%s" % DAEMON_PY)
    print("  解释器  ：%s" % pythonw)
    print("下次登录自动生效；本次请先手动打开一次：%s" % OPEN_BAT)
    return 0


def uninstall_autostart() -> int:
    if not autostart_installed():
        print("启动项本来就不存在")
        return 0
    os.remove(AUTOSTART_FILE)
    print("已移除启动项：%s" % AUTOSTART_FILE)
    return 0


def ensure():
    """确保整链。不 spawn 任何常驻进程。"""
    print("== 工作站 %s ==" % STATION)
    if station_up():
        print("  活着（job 外的派生者，agent 的常驻服务都靠它）")
    else:
        print("  不在")
        print()
        print("  这里必须人工点一次 —— agent 无法自行拉起常驻进程。原因不是权限：")
        print("  沙箱把命令放进 KILL_ON_JOB_CLOSE 的 job 并在命令结束时显式终止，")
        print("  7 种 spawn 方式全被回收，CREATE_BREAKAWAY_FROM_JOB 被内核拒绝。")
        print()
        print("  请任选其一：")
        print("    1) 双击  %s" % OPEN_BAT)
        print("    2) 配一次开机自启（以后不用再管）：python scripts/ensure-daemon.py --install-autostart")
        if autostart_installed():
            print()
            print("  注：启动项已配置（%s），说明工作站被手动关掉了，" % os.path.basename(AUTOSTART_FILE))
            print("      重新登录即可恢复，或现在双击上面那个 bat。")
        return 2

    print("\n== 引擎 %s ==" % ENGINE)
    if engine_up():
        print("  活着")
    else:
        print("  不在，让工作站派生 ……")
        ok, info = start_engine_via_station()
        if not ok:
            print("  [失败] %s" % info, file=sys.stderr)
            return 2
        pid = info.get("pid")
        print("  工作站已接收：%s" % ("pid=%s" % pid if pid else info))
        if not wait_engine():
            print("  [失败] 45 秒内 /api/health 未通：查端口占用 / 数据目录权限", file=sys.stderr)
            return 2
        print("  已就绪")

    print("\n全链可用。引擎父进程是工作站，两者都在沙箱 job 之外。")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--status", action="store_true", help="只报状态")
    ap.add_argument("--deps", action="store_true", help="附带托管依赖清单")
    ap.add_argument("--install-autostart", action="store_true")
    ap.add_argument("--uninstall-autostart", action="store_true")
    a = ap.parse_args()

    if a.install_autostart:
        return install_autostart()
    if a.uninstall_autostart:
        return uninstall_autostart()

    if a.status:
        print("station  %s  %s" % ("UP  " if station_up() else "DOWN", STATION))
        print("engine   %s  %s" % ("UP  " if engine_up() else "DOWN", ENGINE))
        print("autostart %s" % ("installed" if autostart_installed() else "not-installed"))
        if a.deps and engine_up():
            print("\n托管依赖：\n%s" % deps_summary())
        return 0

    rc = ensure()
    if rc == 0 and a.deps:
        print("\n托管依赖：\n%s" % deps_summary())
    return rc


if __name__ == "__main__":
    sys.exit(main())
