#!/usr/bin/env python3
"""发布产物冒烟：用真的 exe 起一个临时实例，断言 HTTP 行为，再收干净。

为什么不用单元测试代替
----------------------
`go test` 覆盖的是处理器函数，覆盖不到「exe 内嵌的前端资源在不在」「ldflags 版本号有没有写进去」
「静态资源缓存头在真实 gin 管道里生效没有」这类**打包之后**才有的问题。发版前跑一遍这个，
就是拿下载区里那个二进制去验。

用法
----
    python scripts/release-check.py                 # 用 dist/FlowEdge.exe
    python scripts/release-check.py --build         # 先构建再验
    python scripts/release-check.py --expect 2026.11.11
    python scripts/release-check.py --keep          # 失败时保留临时目录，便于翻日志

退出码：0 = 全过；1 = 有断言失败；2 = 环境缺失（exe 不存在 / 端口起不来）。
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_EXE = os.path.join(ROOT, "dist", "FlowEdge.exe")
PIPELINES = os.path.join(ROOT, "data", "pipelines.json")

# 不走系统代理：这台机器常驻 http_proxy，会把 127.0.0.1 的请求劫持成 502
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def get(url: str, timeout: float = 5.0):
    """返回 (status, headers, body_bytes)。连接失败返回 (0, {}, b'')。"""
    req = urllib.request.Request(url, method="GET")
    try:
        with OPENER.open(req, timeout=timeout) as resp:
            return resp.status, dict(resp.headers), resp.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()
    except Exception:
        return 0, {}, b""


def wait_healthy(base: str, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        status, _, body = get(base + "/api/health", timeout=2)
        if status == 200:
            return True
        time.sleep(0.4)
    return False


class Checker:
    def __init__(self) -> None:
        self.failures: list[str] = []
        self.passes = 0

    def check(self, name: str, ok: bool, detail: str = "") -> None:
        if ok:
            self.passes += 1
            print(f"  [PASS] {name}")
        else:
            self.failures.append(name)
            print(f"  [FAIL] {name}" + (f"  ← {detail}" if detail else ""))

    def contains(self, name: str, haystack: bytes | str, needle: str) -> None:
        text = haystack.decode("utf-8", "replace") if isinstance(haystack, bytes) else haystack
        self.check(name, needle in text, f"响应里没有 {needle!r}")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--exe", default=DEFAULT_EXE)
    ap.add_argument("--build", action="store_true", help="先跑 scripts/build-windows.py")
    ap.add_argument("--expect", default="", help="期望的版本号（与 /api/health 的 version 比对）")
    ap.add_argument("--timeout", type=float, default=40.0, help="等健康检查的超时秒数")
    ap.add_argument("--keep", action="store_true", help="失败时保留临时目录")
    args = ap.parse_args()

    if args.build:
        print("[release-check] 构建…")
        rc = subprocess.run([sys.executable, os.path.join(ROOT, "scripts", "build-windows.py")]).returncode
        if rc != 0:
            print("  [FAIL] 构建失败（看上面的输出）")
            return 2

    exe = os.path.abspath(args.exe)
    if not os.path.exists(exe):
        print(f"  [FAIL] 找不到 {exe}：先跑 python scripts/build-windows.py")
        return 2
    if not os.path.exists(PIPELINES):
        print(f"  [FAIL] 缺 {PIPELINES}：引擎必须有流水线定义才能启动")
        return 2

    # 引擎启动后会把工作目录切到 exe 所在目录：所以要拷到临时目录里跑，
    # 否则测试数据、日志、sqlite 全会写进 dist/ 或仓库。
    work = tempfile.mkdtemp(prefix="flowedge-release-")
    os.makedirs(os.path.join(work, "data"), exist_ok=True)
    shutil.copy2(exe, os.path.join(work, os.path.basename(exe)))
    shutil.copy2(PIPELINES, os.path.join(work, "data", "pipelines.json"))

    port = free_port()
    base = f"http://127.0.0.1:{port}"
    env = dict(os.environ)
    env.update({"PORT": str(port), "FLOWEDGE_HOST": "127.0.0.1",
                "FLOWEDGE_MODE": "release", "FLOWEDGE_OPEN_BROWSER": "0"})

    print(f"[release-check] 启动 {os.path.basename(exe)} @ {base}")
    proc = subprocess.Popen([os.path.join(work, os.path.basename(exe))], cwd=work, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    c = Checker()
    ok_boot = False
    try:
        ok_boot = wait_healthy(base, args.timeout)
        c.check("引擎在 %.0fs 内就绪" % args.timeout, ok_boot)
        if ok_boot:
            _assertions(c, base, args.expect)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        if c.failures and args.keep:
            print(f"[release-check] 临时目录保留在 {work}")
        else:
            shutil.rmtree(work, ignore_errors=True)

    print()
    if c.failures:
        print(f"RELEASE-CHECK: FAIL（{len(c.failures)} 项：{', '.join(c.failures)}）")
        return 1
    if not ok_boot:
        return 2
    print(f"RELEASE-CHECK: ALL-PASS（{c.passes} 项）")
    return 0


def _assertions(c: Checker, base: str, expect: str) -> None:
    # 1) 健康检查 + 版本号（ldflags 没写进去的话这里会露馅）
    status, _, body = get(base + "/api/health")
    c.check("/api/health 200", status == 200, f"status={status}")
    if status == 200:
        try:
            health = json.loads(body.decode("utf-8"))
        except Exception:
            health = {}
        version = str(health.get("version", ""))
        c.check("version 非空", bool(version), "health.version 为空")
        if expect:
            c.check(f"version == {expect}", version == expect, f"实际 {version!r}")

    # 2) 壳：200 + 不缓存
    status, headers, body = get(base + "/")
    c.check("/ 返回 200", status == 200, f"status={status}")
    cc = headers.get("Cache-Control", "")
    c.check("壳 no-cache", "no-cache" in cc, f"Cache-Control={cc!r}")
    c.contains("壳里有应用挂载点", body, 'id="root"')
    c.contains("壳里有 favicon 声明", body, "favicon.svg")

    # 3) SPA 深链回退
    status, _, _ = get(base + "/runs/42")
    c.check("前端深链回退 200", status == 200, f"status={status}")

    # 4) favicon
    status, headers, _ = get(base + "/favicon.svg")
    c.check("favicon 200", status == 200, f"status={status}")
    c.check("favicon 是 svg", "image/svg+xml" in headers.get("Content-Type", ""),
            f"Content-Type={headers.get('Content-Type')!r}")

    # 5) 哈希产物长缓存 + 缺失资源必须真 404
    status, _, body = get(base + "/")
    asset = ""
    for token in body.decode("utf-8", "replace").split('"'):
        if token.startswith("/assets/") and token.endswith(".js"):
            asset = token
            break
    if asset:
        status, headers, _ = get(base + asset)
        c.check("哈希产物 200", status == 200, f"{asset} status={status}")
        cc = headers.get("Cache-Control", "")
        c.check("哈希产物 immutable", "immutable" in cc, f"Cache-Control={cc!r}")

    status, _, _ = get(base + "/assets/definitely-missing.js")
    c.check("缺失资源真 404（不回退成 HTML）", status == 404, f"status={status}")

    # 6) API：流水线目录能读出来，且固定流水线条数与 id 集合都在
    #    这里是「产品形态」的断言：删掉或改名一条流水线，发布检查必须红。
    EXPECTED_PIPELINES = {
        "image", "image_batch", "image_edit",
        "video", "video_from_image", "film", "film_from_plan", "knowledge_film",
        "voice", "storyboard", "text",
    }
    status, _, body = get(base + "/api/pipelines")
    c.check("/api/pipelines 200", status == 200, f"status={status}")
    if status == 200:
        try:
            data = json.loads(body.decode("utf-8"))
            pipes = data.get("pipelines") or []
            c.check("流水线数量 == %d" % len(EXPECTED_PIPELINES),
                    len(pipes) == len(EXPECTED_PIPELINES), f"实际 {len(pipes)} 条")
            ids = {p.get("id") for p in pipes}
            missing = EXPECTED_PIPELINES - ids
            extra = ids - EXPECTED_PIPELINES
            c.check("固定流水线齐备", not missing, f"缺 {sorted(missing)}")
            c.check("没有计划外的流水线", not extra, f"多出 {sorted(extra)}")
        except Exception as e:
            c.check("流水线 JSON 可解析", False, str(e))

    # 7) 未命中的 API 返回 JSON 404 而不是 HTML
    status, headers, _ = get(base + "/api/nope")
    c.check("未知 API 返回 404", status == 404, f"status={status}")
    c.check("未知 API 是 JSON", "json" in headers.get("Content-Type", "").lower(),
            f"Content-Type={headers.get('Content-Type')!r}")


if __name__ == "__main__":
    sys.exit(main())
