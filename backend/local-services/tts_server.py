# -*- coding: utf-8 -*-
"""FlowEdge 本地 TTS 服务 —— Windows SAPI（私有化，零外部调用）

POST /tts  {"text": "..."}  ->  audio/wav 字节流

用 pywin32 进程内 COM 调 System.Speech，不派生 PowerShell。
端口 8102；模型无关、离线可用。
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

import pythoncom
import win32com.client

PORT = int(os.environ.get("TTS_PORT", "8102"))


def selfcheck_missing():
    """深度自检：真的合成一小段，验证整条链路可用。

    为什么不能只查 import：历史故障里服务起得来、顶层 import 也过了，
    真正调 /tts 时才因为**延迟导入**的传递依赖缺失而 500
    （No module named 'ordered_set'）。只探端口/只查顶层导入都会放过它。

    代价：一次 SAPI 合成约几十毫秒，只在 deep=1 时执行（运行前预检一次），
    常规 /health 保持轻量。
    """
    try:
        wav = synth_to_wav("。")
        if len(wav) < 1000:
            return ["<sapi produced no audio>"]
        return []
    except Exception as exc:  # noqa: BLE001
        msg = str(exc)
        # 把 ModuleNotFoundError 里的模块名提出来，好让提示可以照着装
        for token in ("No module named '", 'No module named "'):
            idx = msg.find(token)
            if idx >= 0:
                rest = msg[idx + len(token):]
                name = rest.split("'")[0].split('"')[0]
                if name:
                    return [name]
        return [f"<{msg}>"]


def select_chinese_voice(synth):
    """SAPI 官方过滤器 Language=804（zh-CN 的十六进制 LCID）选中文声。"""
    try:
        tokens = synth.GetVoices("Language=804")
        if tokens.Count > 0:
            synth.Voice = tokens.Item(0)
            return tokens.Item(0).GetDescription(0)
    except Exception as exc:  # noqa: BLE001
        print(f"[tts] zh voice select failed: {exc}", file=sys.stderr)
    return "default"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # 安静模式
        pass

    def do_GET(self):
        path, _, query = self.path.partition("?")
        if path == "/health":
            payload = {"status": "ok", "service": "flowedge-tts"}
            # deep=1 时才走一遍真实合成：验证「服务活着」之外，还验证
            # 「真的能出声」。历史故障：服务能起、/health 也 200，但真正调
            # /tts 时才 500 —— No module named 'ordered_set'。
            # 只探端口永远抓不到这类延迟导入缺失。
            if "deep=1" in query:
                missing = selfcheck_missing()
                if missing:
                    payload = {
                        "status": "degraded",
                        "service": "flowedge-tts",
                        "missing": missing,
                        "hint": "缺 Python 依赖：pip install " + " ".join(missing),
                    }
                else:
                    payload["detail"] = "自检通过（已成功合成一段测试音频）"
            self._json(200, payload)
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        if self.path != "/tts":
            self.send_response(404)
            self.end_headers()
            return

        length = int(self.headers.get("Content-Length", "0"))
        try:
            payload = json.loads(self.rfile.read(length).decode("utf-8"))
            text = str(payload.get("text", "")).strip()
        except Exception as exc:  # noqa: BLE001
            self._json(400, {"error": f"bad json: {exc}"})
            return

        if not text:
            self._json(400, {"error": "text is empty"})
            return
        if len(text) > 2000:
            text = text[:2000]

        try:
            wav_bytes = synth_to_wav(text)
        except Exception as exc:  # noqa: BLE001
            self._json(500, {"error": f"sapi synthesize failed: {exc}"})
            return

        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Content-Length", str(len(wav_bytes)))
        self.end_headers()
        self.wfile.write(wav_bytes)

    def _json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def synth_to_wav(text: str) -> bytes:
    pythoncom.CoInitialize()
    tmp = os.path.join(os.environ.get("TEMP", "."), f"flowedge-tts-{os.getpid()}.wav")
    try:
        synth = win32com.client.Dispatch("SAPI.SpVoice")
        voice = select_chinese_voice(synth)

        # 关键：嵌套属性赋值（late-binding 下独立 SpAudioFormat 对象赋值会静默失效）
        stream = win32com.client.Dispatch("SAPI.SpFileStream")
        # 不自定义 Format（SAFT 定制会触发 0x80045003），用引擎默认 22kHz16BitMono
        stream.Open(tmp, 3)  # SSFMCreateForWrite
        synth.AudioOutputStream = stream
        try:
            synth.Speak(text)
        finally:
            stream.Close()

        with open(tmp, "rb") as fh:
            data = fh.read()
        if len(data) < 1000:
            raise RuntimeError(f"sapi produced no audio (voice={voice}, {len(data)}B)")
        return data
    finally:
        try:
            os.remove(tmp)
        except OSError:
            pass
        pythoncom.CoUninitialize()


if __name__ == "__main__":
    # 注意：启动块不要 Dispatch/枚举 SAPI——主线程预先持有 SpVoice 会污染
    # 后续请求的引擎激活（0x80045003）。语音选择在每次请求内完成。
    print(f"[tts] listening on :{PORT}", flush=True)
    # 单线程：请求处理始终在主线程，规避 SAPI COM 跨单元（MTA/STA）故障
    HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
