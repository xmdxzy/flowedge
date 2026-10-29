#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""一条命令切换 FlowEdge 的 TTS 后端（Breeze TTS 2 ⇄ Kokoro-82M）。

为什么不靠手改 data/deps.json：
  2026-10-27 早上，两个 agent 各自按自己的判断改同一个 tts 段（08:12 改 Breeze、
  08:21 又改回 Kokoro），谁都没错但互相覆盖，最后没人知道当前生效的是哪套。
  把「切换」做成一个带校验、会等就绪、会打印结果的脚本，争论就变成一条命令。

两个后端的真实差异（都实测过，别再凭印象）：

  Breeze TTS 2（GPU :8103）
    质量  开源权重榜 #2（Elo 1233），支持 音色设计 / 导演 / 零样本克隆
    速度  RTF ≈0.66（A/B 实测，快路径全开）
    代价  峰值 22-23GB，与 H3(20.5GB) / ComfyUI **不能共存**；
          冷启动 110-250s（CUDA Graph 预热）；CPU 模式走不通
          （fast streaming requires a CUDA device）
    许可  仅研究/非商用

  Kokoro-82M（CPU :8102）
    质量  Elo 1066，固定音色，**不支持克隆**（给 ref_audio 会显式 501）
    速度  RTF ≈0.95，首次 load 85s（要建 jieba 词典），之后常驻
    代价  零显存，可以走 CPU 道与出片并行 —— 这是它唯一但重要的优势
    许可  Apache 2.0

用法：
    python scripts/set-tts.py --status          # 只报当前生效的是哪套 + 两个端口状态
    python scripts/set-tts.py breeze            # 切到 Breeze（会重启引擎、等模型就绪）
    python scripts/set-tts.py kokoro            # 切到 Kokoro
    python scripts/set-tts.py breeze --no-restart   # 只改配置，不重启引擎
"""

from __future__ import annotations

import argparse
import io
import json
import os
import shutil
import sys
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEPS = os.path.join(ROOT, "运行", "data", "deps.json")
SERVICES = os.path.join(ROOT, "运行", "local-services")
ENGINE = os.environ.get("FLOWEDGE_BASE", "http://127.0.0.1:8080")
WORKSTATION = os.environ.get("FLOWEDGE_WORKSTATION", "http://127.0.0.1:30000")

PRESETS = {
    "breeze": {
        "script": "tts_breeze_server.py",
        "python": "D:/AI/envs/breeze-tts-pt/Scripts/python.exe",
        "port": "8103",
        "base_url": "http://127.0.0.1:8103",
        "autostart": False,
        # 常驻（0），**不要改成空闲卸载**。2026-10-27 实测：Breeze 重载要 ~20.3GB 空闲，
        # 而 UE 编辑器/浏览器常驻就占 4-6GB；当天 8 次 need≈22.0GiB 全被拒、只有
        # free=22.8GiB 那一次成功，且失败会卡在 phase runtime 占着 16GB 却不就绪。
        # 「出片撞车」不由这里解决，而是 service/vram.go 的 releaseAudioSide——
        # 进扩散侧/语言侧时引擎主动 POST /unload，把 16GB 交出来。
        "idle_unload": 0,
        "device": "cuda",
    },
    "kokoro": {
        "script": "tts_kokoro_server.py",
        "python": "C:/Users/Administrator/Desktop/flowedge/运行/local-services/.venv/Scripts/python.exe",
        "port": "8102",
        "base_url": "http://127.0.0.1:8102",
        "autostart": False,
        # CPU 后端零显存占用，卸载只会白白增加下一次的冷启动 —— 保持常驻。
        "idle_unload": 0,
        "device": "cpu",
    },
}

NOTES = {
    "breeze": "Breeze TTS 2（GPU）：质量最高、可克隆；冷加载 180s、驻留 16GB，"
              "与 H3 互斥。常驻（重载需 ~20.3GB 空闲，容易被 UE 挤掉）；"
              "出片进扩散侧时引擎会主动让它让位（service/vram.go releaseAudioSide）。",
    "kokoro": "Kokoro-82M（CPU）：质量一般、不能克隆；零显存，可与出片并行。",
}


def log(msg):
    print("[set-tts] %s" % msg, flush=True)


def http(url, method="GET", body=None, timeout=60):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json; charset=utf-8"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode("utf-8", "replace")
            try:
                return r.status, json.loads(raw) if raw else None
            except Exception:
                return r.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(raw) if raw else None
        except Exception:
            return e.code, raw
    except Exception as e:
        return 0, str(e)


def load_deps():
    with io.open(DEPS, encoding="utf-8-sig") as f:
        return json.load(f)


def save_deps(d):
    # 先备份，出问题一条 mv 就能回去
    bak = DEPS + ".bak-settts-%s" % time.strftime("%Y%m%d-%H%M%S")
    shutil.copyfile(DEPS, bak)
    with io.open(DEPS, "w", encoding="utf-8-sig") as f:
        json.dump(d, f, ensure_ascii=False, indent=4)
    return bak


def which(d):
    t = d.get("tts") or {}
    if t.get("script") == "tts_breeze_server.py":
        return "breeze"
    if t.get("script") == "tts_kokoro_server.py":
        return "kokoro"
    return "自定（%s）" % t.get("script")


def _kill_stragglers(services_dir):
    """杀掉不在引擎托管里的 TTS 遗留进程（切换后端时最容易留下的坑）。

    判据是命令行里含 tts_breeze_server.py / tts_kokoro_server.py。
    只动这两个脚本，不碰别的 python（MCP、ASR 等）。
    """
    import subprocess

    ps = (
        "Get-CimInstance Win32_Process -Filter \"Name='python.exe'\" | "
        "Where-Object { $_.CommandLine -match 'tts_breeze_server|tts_kokoro_server' } | "
        "ForEach-Object { $_.ProcessId }"
    )
    try:
        out = subprocess.run(["powershell", "-NoProfile", "-Command", ps],
                             capture_output=True, text=True, timeout=30).stdout
    except Exception as e:
        log("列举遗留进程失败：%s" % e)
        return
    for line in out.split():
        line = line.strip()
        if line.isdigit():
            try:
                subprocess.run(["taskkill", "/PID", line, "/F"],
                               capture_output=True, timeout=20)
                log("已清理遗留 TTS 进程 pid=%s" % line)
            except Exception:
                pass


def probe(port):
    code, body = http("http://127.0.0.1:%s/health" % port, timeout=4)
    if code != 200 or not isinstance(body, dict):
        return "down"
    ready = body.get("loaded", body.get("warm", None))
    return "backend=%s ready=%s" % (body.get("backend"), ready)


def status():
    d = load_deps()
    cur = which(d)
    log("当前生效：%s —— %s" % (cur, NOTES.get(cur, "")))
    log("deps.json tts：script=%s port=%s device=%s base_url=%s"
        % (d["tts"].get("script"), d["tts"].get("port"),
           d["tts"].get("device"), d["tts"].get("base_url")))
    for name, p in (("breeze", "8103"), ("kokoro", "8102")):
        log("  :%s -> %s" % (p, probe(p)))
    code, body = http(ENGINE + "/api/deps", timeout=15)
    if isinstance(body, dict):
        for it in body.get("items") or []:
            if it.get("name") == "tts":
                log("引擎视角：endpoint=%s state=%s managed=%s pid=%s"
                    % (it.get("endpoint"), it.get("state"), it.get("managed"), it.get("pid")))
    return 0


def switch(target, restart=True):
    pre = PRESETS[target]
    for key in ("script", "python"):
        if key == "script":
            path = os.path.join(SERVICES, pre["script"]) if not os.path.isabs(pre["script"]) else pre["script"]
        else:
            path = pre["python"]
        if not os.path.exists(path):
            log("拒绝切换：%s 不存在（%s）" % (key, path))
            return 2

    d = load_deps()
    old = (d.get("tts") or {}).get("base_url")
    d["tts"] = dict(d.get("tts") or {})
    d["tts"].update(pre)
    d["tts"]["_comment"] = NOTES[target] + " 由 scripts/set-tts.py 设置；切换只需再跑一次。"
    bak = save_deps(d)
    log("已写入 deps.json（备份 %s）" % os.path.basename(bak))

    # 端口变了 → 必须重启引擎（deps.go:941 的既定事实）
    if restart:
        if str(old) != pre["base_url"]:
            log("base_url %s -> %s，需要重启引擎" % (old, pre["base_url"]))
            code, body = http(WORKSTATION + "/api/flowedge/restart-engine", method="POST", timeout=120)
            if code != 200:
                log("工作站重启失败（%s）——请手动跑 运行/3-应用更新并重启.bat" % code)
                return 3
            for _ in range(40):
                time.sleep(3)
                if http(ENGINE + "/api/health", timeout=4)[0] == 200:
                    log("引擎已重启")
                    break
        else:
            http(ENGINE + "/api/deps/reload", method="POST", timeout=30)
            log("base_url 未变，已 reload")

    # 起服务并等到真就绪
    # 先把「上一个后端」的进程收干净：2026-10-27 实测切换后 Kokoro 与 Breeze
    # 同时常驻，两个都在占资源、端口也各听各的，排查时极易误判成"新后端有问题"。
    code, _ = http(ENGINE + "/api/deps/tts/stop", method="POST", timeout=60)
    log("停掉旧 tts -> HTTP %s" % code)
    _kill_stragglers(SERVICES)
    time.sleep(3)

    code, _ = http(ENGINE + "/api/deps/tts/start", method="POST", timeout=30)
    log("启动 tts -> HTTP %s（冷启动可能要 1-4 分钟）" % code)
    ep = pre["base_url"]
    # 等待上限 620s：实测 Breeze 预热在显存有竞争时会到 425s（干净卡 87-125s），
    # 引擎侧 readyTimeout 已按同样的理由提到 600s。
    deadline = time.time() + 620
    last_note = 0
    while time.time() < deadline:
        c, b = http(ep + "/health", timeout=4)
        if c == 200 and isinstance(b, dict):
            ready = b.get("loaded", b.get("warm", None))
            if ready is True:
                log("就绪：backend=%s device=%s" % (b.get("backend"), b.get("device")))
                return 0
        waited = int(time.time() - (deadline - 620))
        if waited - last_note >= 30:
            last_note = waited
            log("  等模型进显存… %ds（loaded=%s）" % (waited, (b or {}).get("loaded") if isinstance(b, dict) else "?"))
        time.sleep(5)
    log("等待超时——查 运行/logs/tts.log；GPU 后端还要 nvidia-smi 空闲 ≥22GB")
    return 4


def main():
    ap = argparse.ArgumentParser(description="切换 FlowEdge 的 TTS 后端")
    ap.add_argument("target", nargs="?", choices=["breeze", "kokoro"])
    ap.add_argument("--status", action="store_true", help="只看当前生效的是哪套")
    ap.add_argument("--no-restart", action="store_true", help="只改配置，不重启引擎")
    args = ap.parse_args()
    if args.status or not args.target:
        return status()
    return switch(args.target, restart=not args.no_restart)


if __name__ == "__main__":
    sys.exit(main())
