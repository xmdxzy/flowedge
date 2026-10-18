# -*- coding: utf-8 -*-
"""FlowEdge 本地 ASR 服务 —— faster-whisper（私有化，CPU int8）

POST /asr  multipart/form-data (field: file)  ->  {"text": "..."}

模型经 WHISPER_MODEL 配置（默认 small，首次自动经 HF_ENDPOINT 下载）。
端口 8101。
"""
import json
import os
import sys
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("ASR_PORT", "8101"))
MODEL_NAME = os.environ.get("WHISPER_MODEL", "small")

_model = None


def get_model():
    global _model
    if _model is None:
        from faster_whisper import WhisperModel
        print(f"[asr] loading faster-whisper '{MODEL_NAME}' (cpu int8)...", flush=True)
        _model = WhisperModel(MODEL_NAME, device="cpu", compute_type="int8")
        print("[asr] model ready", flush=True)
    return _model


def extract_multipart(body: bytes, content_type: str) -> bytes:
    """从 multipart 请求里取第一个文件 part 的字节（够用且零依赖）。"""
    marker = "boundary="
    idx = content_type.find(marker)
    if idx < 0:
        raise ValueError("no boundary in content-type")
    boundary = content_type[idx + len(marker):].strip().strip('"').encode()
    delim = b"--" + boundary
    parts = body.split(delim)
    for part in parts[1:-1]:  # 首尾是前导/收尾
        seg = part.strip(b"\r\n")
        if not seg or seg == b"--":
            continue
        head_split = seg.find(b"\r\n\r\n")
        if head_split < 0:
            continue
        headers = seg[:head_split].decode("utf-8", "replace").lower()
        if "filename=" in headers and "name=\"file\"" in headers:
            data = seg[head_split + 4:]
            if data.endswith(b"\r\n"):
                data = data[:-2]
            return data
    # 兜底：没有 filename 标记就取第一个带内容的 part
    for part in parts[1:-1]:
        head_split = part.find(b"\r\n\r\n")
        if head_split > 0:
            data = part[head_split + 4:]
            return data[:-2] if data.endswith(b"\r\n") else data
    raise ValueError("no file part found")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok","service":"flowedge-asr"}')
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        if self.path != "/asr":
            self.send_response(404)
            self.end_headers()
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            audio = extract_multipart(body, self.headers.get("Content-Type", ""))
            if not audio:
                self._json(400, {"error": "empty audio"})
                return

            tmp = tempfile.NamedTemporaryFile(suffix=".wav", delete=False)
            tmp.write(audio)
            tmp.close()

            try:
                model = get_model()
                segments, info = model.transcribe(tmp.name, beam_size=5, vad_filter=True)
                text = "".join(seg.text for seg in segments).strip()
                lang = getattr(info, "language", "")
            finally:
                try:
                    os.remove(tmp.name)
                except OSError:
                    pass

            self._json(200, {"text": text, "language": lang})
        except Exception as exc:  # noqa: BLE001
            self._json(500, {"error": str(exc)})

    def _json(self, code, obj):
        payload = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


if __name__ == "__main__":
    # 预加载模型（首次会下载，需 HF_ENDPOINT=https://hf-mirror.com）
    get_model()
    print(f"[asr] listening on :{PORT}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
