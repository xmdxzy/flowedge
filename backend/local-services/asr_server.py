# -*- coding: utf-8 -*-
"""FlowEdge 本地 ASR 服务 —— faster-whisper（私有化，CPU int8）

POST /asr  multipart/form-data (fields: file[, model][, device])  ->  {"text": "..."}

模型优先级：请求字段 model > 环境变量 WHISPER_MODEL > 默认 small。
device 同理：请求字段 > WHISPER_DEVICE > 默认 cpu。
多档模型按需加载并缓存（faster-whisper 各档体积 small≈0.5GB / medium≈1.5GB /
large-v3≈3GB，都跑在内存里，64GB 机器可共存；首次加载会自动经 HF_ENDPOINT 下载）。
端口 8101。
"""
import json
import os
import sys
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("ASR_PORT", "8101"))
MODEL_NAME = os.environ.get("WHISPER_MODEL", "small")
DEVICE = os.environ.get("WHISPER_DEVICE", "cpu")

# 多档缓存：key = (model_name, device)。切档不必重启服务——节点可以按镜头
# 挑精度（台词镜头用 large-v3，氛围镜头用 small），代价只是内存。
_models = {}

# device 白名单：这个值直接进 WhisperModel(device=...)，不收白等于让请求方
# 决定加载行为。只放 faster-whisper 真实支持的。
_ALLOWED_DEVICES = ("cpu", "cuda", "auto")


def get_model(name=None, device=None):
    name = (name or MODEL_NAME).strip() or MODEL_NAME
    device = (device or DEVICE).strip() or DEVICE
    if device not in _ALLOWED_DEVICES:
        raise ValueError(f"不支持的 device：{device}（可选 {', '.join(_ALLOWED_DEVICES)}）")
    key = (name, device)
    model = _models.get(key)
    if model is None:
        from faster_whisper import WhisperModel
        compute = "int8" if device == "cpu" else "float16"
        print(f"[asr] loading faster-whisper '{name}' ({device} {compute})...", flush=True)
        model = WhisperModel(name, device=device, compute_type=compute)
        print("[asr] model ready", flush=True)
        _models[key] = model
    return model


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


def extract_field(body: bytes, content_type: str, field: str) -> str:
    """从 multipart 请求里取指定文本字段（model / device 用）。零依赖，够用即可。"""
    marker = "boundary="
    idx = content_type.find(marker)
    if idx < 0:
        return ""
    boundary = content_type[idx + len(marker):].strip().strip('"').encode()
    delim = b"--" + boundary
    parts = body.split(delim)
    for part in parts[1:-1]:
        seg = part.strip(b"\r\n")
        if not seg or seg == b"--":
            continue
        head_split = seg.find(b"\r\n\r\n")
        if head_split < 0:
            continue
        headers = seg[:head_split].decode("utf-8", "replace").lower()
        want = f'name="{field}"'
        if want in headers and "filename=" not in headers:
            return seg[head_split + 4:].strip(b"\r\n").decode("utf-8", "replace").strip()
    return ""


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
            content_type = self.headers.get("Content-Type", "")
            audio = extract_multipart(body, content_type)
            if not audio:
                self._json(400, {"error": "empty audio"})
                return

            # 节点级模型档位：没带就用服务默认（env 或 small）
            model_name = extract_field(body, content_type, "model")
            device = extract_field(body, content_type, "device")

            tmp = tempfile.NamedTemporaryFile(suffix=".wav", delete=False)
            tmp.write(audio)
            tmp.close()

            try:
                model = get_model(model_name, device)
                segments, info = model.transcribe(tmp.name, beam_size=5, vad_filter=True)
                text = "".join(seg.text for seg in segments).strip()
                lang = getattr(info, "language", "")
            finally:
                try:
                    os.remove(tmp.name)
                except OSError:
                    pass

            self._json(200, {"text": text, "language": lang})
        except ValueError as exc:
            # 参数类错误（device 白名单外等）：400，别伪装成 500 服务故障
            self._json(400, {"error": str(exc)})
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
    # 预加载默认模型（首次会下载，需 HF_ENDPOINT=https://hf-mirror.com）
    get_model()
    print(f"[asr] listening on :{PORT}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
