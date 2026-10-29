# -*- coding: utf-8 -*-
"""主播配音桥 —— 给直播互动用的最薄一层。

为什么需要它：FlowEdge 的 voice 流水线是「提交任务 → 等 run → 取产物」，
适合出片，不适合直播时一句一句即时发声（每次都要建 run、排队、查状态）。
直播要的是：给一句文本，几百毫秒到 2 秒后出声。

它做的事：
  POST /say  {"text":"...", "voice":"anime_mix_5050", "play":true}
      → 直接调本地 TTS 服务（不走队列）→ 落 wav → 可选立刻播放
  GET  /health   看 TTS 是否已加载、队列有几条
  GET  /last     最近一条的 wav 路径（OBS 想用文件源时读它）

设计约束（都踩过）：
  * TTS 服务是**单并发**，第二个请求会 503「服务忙」→ 这里用单 worker 串行化，不并发。
  * 模型被别的任务挤掉后第一次合成要**重新加载 3–4 分钟**→ 启动时先 /warm 预热，
    并在 /health 里把 loaded 报出来，别让主播在直播里发现第一句卡了两分钟。
  * 不要在这里重试无限次：503 时重试 3 次就报错回给调用方。

启动（要在 agent shell 之外跑，否则进程活不过本次调用）：
  python scripts/voice-bridge.py                 # 默认 127.0.0.1:8110
  python scripts/voice-bridge.py --port 8110 --no-play
"""
from __future__ import annotations

import argparse
import json
import os
import queue
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TTS_BASE = os.environ.get("FLOWEDGE_TTS_URL", "http://127.0.0.1:8103")
DEFAULT_VOICE = os.environ.get("VOICE_BRIDGE_VOICE", "anime_mix_5050")
OUT_DIR = os.environ.get("VOICE_BRIDGE_OUT", "data/voice-bridge")
FFPLAY = os.environ.get("VOICE_BRIDGE_FFPLAY", "ffplay")

_jobs: "queue.Queue[dict]" = queue.Queue()
_last = {"path": None, "text": None, "ms": None, "at": None}
_stats = {"done": 0, "failed": 0}
_playing = {"proc": None}


def tts_alive() -> dict:
    try:
        with urllib.request.urlopen(TTS_BASE + "/health", timeout=4) as r:
            return json.loads(r.read().decode("utf-8-sig"))
    except Exception as e:
        return {"ok": False, "error": f"{type(e).__name__}: {e}"}


def synth(text: str, voice: str, path: str, retries: int = 3) -> None:
    body = json.dumps({"text": text, "voice": voice}, ensure_ascii=False).encode("utf-8")
    last_err = ""
    for i in range(retries):
        req = urllib.request.Request(TTS_BASE + "/tts", data=body,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=600) as r:
                data = r.read()
            if len(data) < 1000:
                raise RuntimeError(f"音频过短（{len(data)} bytes）")
            os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
            with open(path, "wb") as fh:
                fh.write(data)
            return
        except urllib.error.HTTPError as e:
            last_err = f"HTTP {e.code}: {e.read()[:200].decode('utf-8', 'replace')}"
            if e.code == 503:                    # 服务忙：等一下再试，别放弃
                time.sleep(1.5 + i)
                continue
            break
        except Exception as e:
            last_err = f"{type(e).__name__}: {e}"
            time.sleep(1.0 + i)
    raise RuntimeError(last_err or "合成失败")


def play(path: str) -> None:
    """上一个还在播就掐掉，保证口型/音频不叠。"""
    old = _playing["proc"]
    if old is not None and old.poll() is None:
        try:
            old.kill()
        except Exception:
            pass
    try:
        _playing["proc"] = subprocess.Popen(
            [FFPLAY, "-nodisp", "-autoexit", "-loglevel", "quiet", path],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except FileNotFoundError:
        _playing["proc"] = None


def worker(do_play: bool) -> None:
    while True:
        job = _jobs.get()
        t0 = time.time()
        try:
            synth(job["text"], job["voice"], job["path"])
            ms = int((time.time() - t0) * 1000)
            _last.update({"path": job["path"], "text": job["text"], "ms": ms,
                          "at": time.strftime("%H:%M:%S")})
            _stats["done"] += 1
            if do_play:
                play(job["path"])
            job["result"].update({"ok": True, "path": job["path"], "ms": ms})
        except Exception as e:
            _stats["failed"] += 1
            job["result"].update({"ok": False, "error": str(e)[:300],
                                  "ms": int((time.time() - t0) * 1000)})
        finally:
            job["event"].set()
            _jobs.task_done()


class Handler(BaseHTTPRequestHandler):
    server_version = "flowedge-voice-bridge/1.0"

    def log_message(self, fmt, *args):
        pass

    def _json(self, code: int, obj: dict) -> None:
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.startswith("/health"):
            h = tts_alive()
            self._json(200, {"ok": True,
                             "tts": {"base": TTS_BASE,
                                     "reachable": h.get("status") == "ok",
                                     "loaded": h.get("loaded"),
                                     "idle_unload": h.get("idle_unload"),
                                     "raw": h},
                             "queue": _jobs.qsize(), "stats": _stats, "last": _last})
            return
        if self.path.startswith("/last"):
            self._json(200, _last)
            return
        self._json(404, {"error": "unknown path"})

    def do_POST(self):
        if not self.path.startswith("/say"):
            self._json(404, {"error": "unknown path"})
            return
        try:
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n).decode("utf-8") or "{}")
        except Exception as e:
            self._json(400, {"error": f"bad json: {e}"})
            return
        text = (body.get("text") or "").strip()
        if not text:
            self._json(400, {"error": "text 不能为空"})
            return
        if len(text) > 500:
            self._json(400, {"error": "一次最多 500 字（直播一句话没必要这么长）"})
            return
        voice = body.get("voice") or DEFAULT_VOICE
        name = time.strftime("%H%M%S") + "-" + uuid.uuid4().hex[:6] + ".wav"
        path = os.path.join(OUT_DIR, name)
        job = {"text": text, "voice": voice, "path": path, "result": {},
               "event": threading.Event()}
        _jobs.put(job)
        timeout = float(body.get("timeout") or 180)
        if not job["event"].wait(timeout):
            self._json(504, {"ok": False, "error": "排队超时（前面还有没播完的）"})
            return
        r = job["result"]
        self._json(200 if r.get("ok") else 502, r)


def wait_ready(budget_sec: float) -> bool:
    t0 = time.time()
    while time.time() - t0 < budget_sec:
        h = tts_alive()
        if h.get("loaded"):
            return True
        time.sleep(3)
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=int(os.environ.get("VOICE_BRIDGE_PORT", 8110)))
    ap.add_argument("--no-play", action="store_true", help="只出 wav 不播放（用 OBS 的文件源时）")
    ap.add_argument("--warm", action="store_true", help="启动后等 TTS 加载完（可能要 3-4 分钟）")
    a = ap.parse_args()
    os.makedirs(OUT_DIR, exist_ok=True)

    threading.Thread(target=worker, args=(not a.no_play,), daemon=True).start()
    h = tts_alive()
    if not (h.get("status") == "ok" or "ok" in h):
        print(f"[bridge] 警告：TTS 不可达 {TTS_BASE}（先 POST :8080/api/deps/tts/start）", flush=True)
    elif not h.get("loaded"):
        print("[bridge] TTS 进程在，但模型没加载：第一句会等 3-4 分钟（autoexec 建议加 --warm）",
              flush=True)
    else:
        print(f"[bridge] TTS 已加载，声线默认 {DEFAULT_VOICE}", flush=True)
    if a.warm:
        print("[bridge] 预热中（等模型 loaded）…", flush=True)
        ok = wait_ready(420)
        print("[bridge] 预热%s" % ("完成" if ok else "超时（先去腾显存）"), flush=True)

    srv = ThreadingHTTPServer((a.host, a.port), Handler)
    print(f"[bridge] 听 http://{a.host}:{a.port}/say  （POST {{\"text\":\"...\"}}）", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
