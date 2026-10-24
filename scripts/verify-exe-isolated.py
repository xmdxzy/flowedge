#!/usr/bin/env python3
"""成品 exe 隔离验活：不碰线上引擎、不启动任何 GPU 服务。

为什么需要它
------------
`check-all.py --live` 验的是**正在跑的那个实例**（`运行/FlowEdge.exe`）。它证明不了
"我刚构建出来的这个 exe 能不能用"——而发布前真正想知道的是后者。

本脚本做的事：把成品 exe 复制到 `.tmp/exe-check/`，配一份 `deps.json`（全部关掉
自动启动，避免顺手把 ComfyUI/TTS 拉起来占显存），在**独立端口**起进程 → 跑
`verify-canonical-live.py` 的接口断言 + 内嵌前端首页可读 → 杀掉进程。

一条命令跑完是硬要求：agent 的 shell 每次调用是一个 job，调用结束显式终止子进程树；
分两次调用去连，第二次引擎已经没了（看起来像"接口挂了"）。

用法
----
    python scripts/verify-exe-isolated.py                       # 验 dist/FlowEdge.exe
    python scripts/verify-exe-isolated.py --exe 某路径.exe        # 验指定产物
    python scripts/verify-exe-isolated.py --port 18081          # 换端口
    python scripts/verify-exe-isolated.py --keep                 # 保留现场（排查用）

退出码：0 = 全过；1 = 有断言失败；2 = 引擎没起来。
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WORK = os.path.join(ROOT, ".tmp", "exe-check")
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def prepare(exe_src: str) -> str:
    os.makedirs(os.path.join(WORK, "data"), exist_ok=True)
    exe = os.path.join(WORK, "FlowEdge.exe")
    shutil.copy(exe_src, exe)

    # 流水线定义（缺它引擎会拒绝启动——这本身也是一条要验的行为）
    shutil.copy(os.path.join(ROOT, "data", "pipelines.json"),
                os.path.join(WORK, "data", "pipelines.json"))

    # 依赖全部不自动启动：验活不该占显存
    deps_src = os.path.join(ROOT, "data", "deps.json")
    deps = {}
    if os.path.exists(deps_src):
        with open(deps_src, encoding="utf-8-sig") as f:
            deps = json.load(f)
    for key in ("deps", "items"):
        block = deps.get(key) if isinstance(deps, dict) else None
        if isinstance(block, dict):
            for spec in block.values():
                if isinstance(spec, dict):
                    spec["autostart"] = False
    with open(os.path.join(WORK, "data", "deps.json"), "w", encoding="utf-8") as f:
        json.dump(deps, f, ensure_ascii=False, indent=2)
    return exe


def wait_health(base: str, timeout: int = 90) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with OPENER.open(base + "/api/health", timeout=2) as r:
                if r.status == 200:
                    return True
        except Exception:
            time.sleep(1)
    return False


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--exe", default=os.path.join(ROOT, "dist", "FlowEdge.exe"))
    ap.add_argument("--port", type=int, default=18080)
    ap.add_argument("--keep", action="store_true", help="保留 .tmp/exe-check 现场")
    ap.add_argument("--expect-version", default="",
                    help="期望的版本号（构建时 --version 写进二进制的那串）；给了就断言 /api/health 一致")
    args = ap.parse_args()

    if not os.path.exists(args.exe):
        print(f"[FAIL] 找不到产物 {args.exe}（先跑 python scripts/build-windows.py）")
        return 2

    base = f"http://127.0.0.1:{args.port}"
    exe = prepare(args.exe)
    print(f"[exe] {args.exe} → {WORK}，端口 {args.port}", flush=True)

    env = dict(os.environ)
    env.update({
        "PORT": str(args.port),
        "FLOWEDGE_PORT": str(args.port),
        "FLOWEDGE_HOST": "127.0.0.1",
        "GENERATED_DIR": os.path.join(WORK, "generated"),
        "GIN_MODE": "release",
    })
    log_path = os.path.join(WORK, "engine.log")
    log = open(log_path, "w", encoding="utf-8")
    proc = subprocess.Popen([exe], cwd=WORK, env=env, stdout=log, stderr=subprocess.STDOUT)
    try:
        if not wait_health(base):
            log.flush()
            tail = open(log_path, encoding="utf-8", errors="replace").read()[-1500:]
            print("[FAIL] 引擎 90s 内没起来\n" + tail)
            return 2
        print("[ok] 引擎已起", flush=True)

        # 内嵌前端是否真的被服务出来（打包漏静态资源时这里会 404/500）
        try:
            with OPENER.open(base + "/", timeout=5) as r:
                html = r.read().decode("utf-8", "replace")
        except Exception as e:
            print(f"  [FAIL] 首页不可读：{e}")
            return 1
        ok_ui = 'id="root"' in html and len(html) > 200
        print(("  [PASS] " if ok_ui else "  [FAIL] ") + f"内嵌前端首页可读（{len(html)} 字节）")
        if not ok_ui:
            return 1

        child_env = dict(os.environ)
        if args.expect_version:
            child_env["FLOWEDGE_EXPECT_VERSION"] = args.expect_version
        child_env["FLOWEDGE_BASE"] = base
        return subprocess.run(
            [sys.executable, os.path.join(ROOT, "scripts", "verify-canonical-live.py")],
            cwd=ROOT, env=child_env,
        ).returncode
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()
        if not args.keep:
            shutil.rmtree(WORK, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
