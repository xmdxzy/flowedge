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
        if self.path == "/health":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok","service":"flowedge-tts"}')
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
