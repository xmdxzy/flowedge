#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""一条命令完成「腾卡 → 起 H3 → 出片 → 收卡」。

为什么需要它（2026-10-26 一整天的现场结论）：
  1. **GPU 版 TTS 会常驻 7GB**，H3 需要 ~20.5GB，一挤就变成「部分加载」——
     同一镜从 87s 退化到 2269s，然后撞 12 分钟节点超时，看起来像「模型坏了」。
  2. **引擎派生的 H3 起不来**：引擎进程对 %LOCALAPPDATA%\\Temp 是 Access denied，
     torch 启动时要写 Temp 直接 FileNotFoundError。
  3. 手工起 H3 又必须带 `--reserve-vram 0`，带 4 就是 20.5+4 > 24GB，永远部分加载。

所以把这段一次性编排固化下来：本脚本自己当 H3 的父进程（活到收工为止），
出片结束就把它收掉，显存还回去。

用法：
  python scripts/produce-with-gpu.py --pipeline video --params-file p.json
  python scripts/produce-with-gpu.py --pipeline video --params '{"prompt":"...","frames":107}'
  python scripts/produce-with-gpu.py --probe          # 只看当前能不能跑，不提交

退出码：0 成功；2 环境不满足（没腾出显存/起不来）；1 出片失败。
"""

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

ENGINE = os.environ.get("FLOWEDGE_BASE", "http://127.0.0.1:8080")
H3 = os.environ.get("H3_BASE_URL", "http://127.0.0.1:8189")
H3_HOME = os.environ.get("H3_HOME", r"D:\AI\ComfyUI-H3")
COMFY_HOME = os.environ.get("COMFY_HOME", r"D:\AI\ComfyUI")
# H3 权重 20480MB（int8 pruned）；低于这个数就会退化成按步换页。
H3_NEED_MB = 20480


def log(msg):
    print("[gpu-slot] " + msg, flush=True)


def http(url, method="GET", body=None, timeout=15):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read().decode("utf-8-sig", "replace")
        return r.status, (json.loads(raw) if raw.strip().startswith(("{", "[")) else raw)


def try_http(url, method="GET", body=None, timeout=8):
    try:
        return http(url, method, body, timeout)
    except Exception:
        return None, None


def gpu_free_mb():
    """返回 (free_mb, total_mb)。读不到就 (None, None)——不许猜。"""
    exe = "nvidia-smi"
    try:
        out = subprocess.run(
            [exe, "--query-gpu=memory.free,memory.total", "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=15)
        if out.returncode != 0:
            return None, None
        free, total = out.stdout.strip().splitlines()[0].split(",")
        return int(free), int(total)
    except Exception:
        return None, None


def gpu_holders(min_mb=500):
    """按进程列出显存占用（Windows 性能计数器）。拿不到就给空表。"""
    ps = (
        "$s=(Get-Counter '\\GPU Process Memory(*)\\Dedicated Usage').CounterSamples | "
        "Where-Object { $_.CookedValue -gt %dMB } | Sort-Object CookedValue -Descending;"
        "foreach($x in $s){ $proc=($x.InstanceName -split '_')[1];"
        "$p=Get-Process -Id $proc -ErrorAction SilentlyContinue;"
        "if($p){ '{0}|{1}|{2}' -f $proc,$p.ProcessName,[int]($x.CookedValue/1MB) } }"
    ) % min_mb
    try:
        out = subprocess.run(["powershell", "-NoProfile", "-Command", ps],
                             capture_output=True, text=True, timeout=40)
        rows = []
        for line in out.stdout.splitlines():
            parts = line.strip().split("|")
            if len(parts) == 3:
                rows.append({"pid": parts[0], "name": parts[1], "mb": int(parts[2])})
        return rows
    except Exception:
        return []


def dep_state(name):
    st, body = try_http(ENGINE + "/api/deps", timeout=25)
    if not isinstance(body, dict):
        return None
    for it in body.get("items") or []:
        if it.get("name") == name:
            return it
    return None


def restart_engine(timeout=90):
    """H3 换了 pid 之后必须重启引擎：引擎侧的 ComfyUI 客户端不会自己重连。

    2026-10-26 现场：手工起了新 H3，引擎 5-6 分钟后就被外部按"换了 H3，必须重启引擎"
    这条运维规则重启一次，把我正在采样的 run 判成「进程异常退出」。
    与其等别人来重启，不如自己先把这一步做掉 —— 顺序变成「H3 就绪 → 重启引擎 → 提交」。
    """
    st, body = try_http("http://127.0.0.1:30000/api/flowedge/restart-engine",
                        method="POST", timeout=60)
    if st != 200:
        log("重启引擎失败（%s）：%s" % (st, body))
        return False
    t0 = time.time()
    while time.time() - t0 < timeout:
        time.sleep(3)
        ok, _ = try_http(ENGINE + "/api/health", timeout=4)
        if ok == 200:
            log("引擎已重启（%.0fs）" % (time.time() - t0))
            return True
    log("引擎重启后 %ds 内未就序" % timeout)
    return False


def stop_dep(name):
    st, _ = try_http(ENGINE + "/api/deps/%s/stop" % name, method="POST", timeout=25)
    return st == 200


def _service_loaded(st):
    """服务是不是**真的**能干活了（模型已进显存）。

    为什么不能只看引擎的 `state=ok`：TTS 这类服务一启动就绑端口、/health 立刻回 200
    （`loaded=false` 表示模型还在预热），引擎据此判 `running+ok`。2026-10-27 实测：
    `start_dep('tts')` 3 秒就返回 True，而模型还要 110-180s 才就绪——紧接着跑 voice
    就会撞上预热、把请求拖到超时。
    """
    ep = (st or {}).get("endpoint") or ""
    if not ep:
        return True
    code, body = try_http(ep + "/health", timeout=6)
    if code != 200 or not isinstance(body, dict):
        return False
    # Breeze 用 loaded 明确表达「模型已进显存」；Kokoro 用 warm。两个都认，
    # 都没有这个字段的服务按 HTTP 200 算就绪。
    if "loaded" in body:
        return bool(body.get("loaded"))
    if "warm" in body:
        return bool(body.get("warm"))
    return True


def start_dep(name, wait_s=300):
    """启动依赖并等它**真的**就绪（引擎的 /start 是 202 异步，不能只看返回码）。

    2026-10-27 加：原来只有 stop_dep，出片前的「停了 TTS 出片后恢复」根本没有实现，
    只有一句提示文案——结果每跑一次视频，TTS 就一直停着，voice 流水线直接失败。
    Breeze TTS 2 的冷启动含 CUDA Graph 预热（缓存热 ~110-180s），所以默认等 300s。
    """
    code, _ = try_http(ENGINE + "/api/deps/%s/start" % name, method="POST", timeout=30)
    if code not in (200, 202):
        return False
    deadline = time.time() + wait_s
    while time.time() < deadline:
        st = dep_state(name)
        if st and st.get("running") and st.get("state") == "ok" and _service_loaded(st):
            return True
        time.sleep(3)
    return False


def free_instance(base):
    return try_http(base + "/free", method="POST",
                    body={"unload_models": True, "free_memory": True}, timeout=20)[0] == 200


def h3_ready():
    st, _ = try_http(H3 + "/system_stats", timeout=4)
    return st == 200


def takeover_h3():
    """把已有 :8189 实例停掉，改由本脚本自己拉起。

    为什么不能随便复用：2026-10-26 23:15 复用了一个由引擎派生的实例，
    出片时 ComfyUI 报 `SaveVideo: [Errno 13] Permission denied`（引擎派生的子进程
    没有写 D:/AI/ComfyUI/output 的权限，同源问题还有它连 %TEMP% 都写不了）。
    自己拉起的实例跑在正常上下文里，这两类坑都不会有。
    """
    if not h3_ready():
        return True
    ps = (
        "$c = Get-NetTCPConnection -LocalPort 8189 -State Listen -ErrorAction SilentlyContinue | "
        "Select-Object -First 1; if ($c) { $c.OwningProcess }"
    )
    try:
        out = subprocess.run(["powershell", "-NoProfile", "-Command", ps],
                             capture_output=True, text=True, timeout=25)
        pid = out.stdout.strip().splitlines()[0].strip() if out.stdout.strip() else ""
    except Exception:
        pid = ""
    if not pid.isdigit():
        log("8189 在监听但拿不到 pid，无法接管")
        return False
    log("接管：停掉已有 H3 实例 pid=%s" % pid)
    subprocess.run(["taskkill", "/PID", pid, "/T", "/F"], capture_output=True, timeout=30)
    for _ in range(30):
        time.sleep(1)
        if not h3_ready():
            return True
    log("旧实例 30s 内没退出")
    return False


def start_h3():
    py = os.path.join(H3_HOME, ".venv", "Scripts", "python.exe")
    if not os.path.isfile(py):
        log("找不到 H3 解释器：%s" % py)
        return None
    args = [
        py, os.path.join(H3_HOME, "main.py"),
        "--listen", "127.0.0.1", "--port", "8189",
        "--base-directory", H3_HOME,
        "--extra-model-paths-config", os.path.join(H3_HOME, "extra_model_paths.yaml"),
        "--input-directory", os.path.join(COMFY_HOME, "input"),
        "--output-directory", os.path.join(COMFY_HOME, "output"),
        "--user-directory", os.path.join(COMFY_HOME, "user"),
        "--database-url", "sqlite:///" + os.path.join(COMFY_HOME, "user", "comfyui-h3.db").replace("\\", "/"),
        # 关键：预留 4GB 会让「20.5GB 权重 + 4GB 预留 > 24GB」恒成立 → 永远部分加载。
        "--reserve-vram", "0",
        "--cache-lru", "2",
    ]
    log("拉起 ComfyUI-H3（reserve-vram=0）：pid 稍后打印")
    proc = subprocess.Popen(args, cwd=H3_HOME,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    log("H3 pid=%d" % proc.pid)
    return proc


def wait_h3(proc, timeout=240):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if proc is not None and proc.poll() is not None:
            log("H3 进程已退出（code=%s）——看 运行/logs/comfy_h3.log" % proc.returncode)
            return False
        if h3_ready():
            log("H3 就绪（%.0fs）" % (time.time() - t0))
            return True
        time.sleep(4)
    log("H3 在 %ds 内未就绪" % timeout)
    return False


def ensure_vram(need_mb, stop_tts):
    """腾显存：先让所有 ComfyUI 实例 /free，再按需停 GPU 版 TTS。"""
    stopped = []
    free, _ = gpu_free_mb()
    log("当前空闲显存：%s MB（需要 ≥ %d MB）" % (free, need_mb))
    if free is not None and free >= need_mb:
        return True, stopped

    for base in (H3, "http://127.0.0.1:8190", "http://127.0.0.1:8188"):
        if h3_ready() if base == H3 else True:
            free_instance(base)
    time.sleep(4)
    free, _ = gpu_free_mb()
    log("腾 ComfyUI 后：%s MB" % free)
    if free is not None and free >= need_mb:
        return True, stopped

    if stop_tts:
        st = dep_state("tts")
        if st and st.get("running"):
            if stop_dep("tts"):
                stopped.append("tts")
                log("已停 GPU 版 TTS（出片结束后会尝试恢复）")
                time.sleep(5)
                free, _ = gpu_free_mb()
                log("停 TTS 后：%s MB" % free)

    # 仍有别的进程占着就如实报，不硬上（硬上=部分加载=12 分钟超时）
    if free is not None and free < need_mb:
        for h in gpu_holders(400):
            log("  占用：pid=%s %s %dMB" % (h["pid"], h["name"], h["mb"]))
        return False, stopped
    return True, stopped


def submit(pipeline, params, invoked_by):
    st, body = http(ENGINE + "/api/produce", method="POST", body={
        "pipeline": pipeline, "params": params, "invoked_by": invoked_by}, timeout=60)
    if st != 202 or not isinstance(body, dict) or not body.get("run_id"):
        log("提交失败：%s %s" % (st, body))
        return None
    return int(body["run_id"])


def poll(run_id, timeout):
    t0 = time.time()
    last = None
    while time.time() - t0 < timeout:
        st, body = try_http("%s/api/runs/%d" % (ENGINE, run_id), timeout=10)
        if isinstance(body, dict):
            status = body.get("status")
            if status != last:
                log("run %d -> %s" % (run_id, status))
                last = status
            if status in ("success", "failed", "cancelled"):
                return body
        time.sleep(5)
    log("等待超时（%ds）" % timeout)
    return None


def main():
    ap = argparse.ArgumentParser(description="腾卡→起 H3→出片→收卡")
    ap.add_argument("--pipeline")
    ap.add_argument("--params", help="JSON 字符串")
    ap.add_argument("--params-file", help="从文件读字段（中文参数更稳）")
    ap.add_argument("--invoked-by", default="manual",
                    choices=["manual", "workbuddy", "dsh", "mcp"])
    ap.add_argument("--timeout", type=int, default=900, help="等出片的秒数")
    ap.add_argument("--need-mb", type=int, default=H3_NEED_MB)
    ap.add_argument("--no-stop-tts", action="store_true", help="不要动 TTS")
    ap.add_argument("--keep-h3", action="store_true", help="跑完保留 H3（默认收掉还显存）")
    ap.add_argument("--reuse-h3", action="store_true",
                    help="复用已在运行的 H3（默认接管：停掉旧实例自己拉起，避免权限坑）")
    ap.add_argument("--no-restart-engine", action="store_true",
                    help="不在 H3 就绪后重启引擎（默认重启：换了 H3 进程引擎必须重连）")
    ap.add_argument("--probe", action="store_true", help="只体检，不提交")
    args = ap.parse_args()

    if not args.probe and not args.pipeline:
        log("需要 --pipeline（或用 --probe 只体检）")
        return 2

    st, _ = try_http(ENGINE + "/api/health", timeout=5)
    if st != 200:
        log("引擎不可达：%s（先 python scripts/ensure-daemon.py）" % ENGINE)
        return 2

    ok, stopped = ensure_vram(args.need_mb, stop_tts=not args.no_stop_tts)
    h3_proc = None
    try:
        if not args.reuse_h3:
            if not takeover_h3():
                return 2
        if not h3_ready():
            h3_proc = start_h3()
            if h3_proc is None:
                return 2
            if not wait_h3(h3_proc):
                return 2
        else:
            log("复用已在运行的 H3（--reuse-h3 指定）")

        if not args.no_restart_engine:
            if not restart_engine():
                return 2

        # 这里的判定必须用**此刻**的读数：ensure_vram 是在 H3 起来之前算的，
        # 而 H3 自己启动也会占掉几 GB（实测 ~4GB），拿旧结论会把能跑的判成不能跑。
        free, total = gpu_free_mb()
        log("出片前空闲显存：%s / %s MB（门槛 %d MB）" % (free, total, args.need_mb))
        if free is not None and free < args.need_mb:
            log("显存仍不够，拒绝硬跑（硬跑 = 部分加载 = 12 分钟超时白烧）")
            return 2

        if args.probe:
            log("体检通过（--probe 不提交）")
            return 0

        params = {}
        if args.params_file:
            with open(args.params_file, encoding="utf-8") as f:
                params = json.load(f)
        elif args.params:
            params = json.loads(args.params)
        if not params:
            log("需要 --params 或 --params-file")
            return 2

        run_id = submit(args.pipeline, params, args.invoked_by)
        if run_id is None:
            return 2

        body = poll(run_id, args.timeout)
        if not body:
            return 1
        if body.get("status") != "success":
            log("出片失败：%s" % (body.get("error") or "")[:300])
            return 1

        # 产物路径：优先看节点 output.video（贴字后会被改写成 -text.mp4）
        art = ""
        for n in body.get("node_states") or []:
            out = n.get("output") or {}
            if isinstance(out, dict) and out.get("video"):
                art = out["video"]
        log("成功；产物：%s" % (art or "（见 /api/artifacts）"))
        return 0
    finally:
        if h3_proc is not None and not args.keep_h3:
            log("收掉 H3 子进程 pid=%d，归还显存" % h3_proc.pid)
            h3_proc.terminate()
            try:
                h3_proc.wait(timeout=20)
            except Exception:
                h3_proc.kill()
        if stopped:
            # 真的恢复，而不只是喊一句「需要时自己重启」。
            # 2026-10-27 修：原实现只 log 提示，于是每跑一次视频 TTS 就一直停着，
            # 下一条 voice 流水线直接失败，而日志看起来像「本可以恢复」。
            for dep in stopped:
                log("恢复 %s …（冷启动可能要 1-3 分钟）" % dep)
                if start_dep(dep):
                    log("已恢复 %s" % dep)
                else:
                    log("恢复 %s 失败 —— 请在「环境」面板手动启动，"
                        "或 curl -X POST %s/api/deps/%s/start" % (dep, ENGINE, dep))


if __name__ == "__main__":
    sys.exit(main())
