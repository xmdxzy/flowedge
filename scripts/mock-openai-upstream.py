"""Mock OpenAI-compatible upstream (zero GPU).

Purpose: verify that FlowEdge behaves correctly when acting as an
OpenAI-compatible *server* in front of a real OpenAI-compatible *upstream*.
No model is loaded, no VRAM is touched — this only checks protocol shape.

用法（GPU 被别的任务占着时也能量 OpenAI 兼容链路）:

    python scripts/mock-openai-upstream.py 8199
    cd dist && PORT=8099 OLLAMA_BASE_URL=http://127.0.0.1:8199 ./FlowEdge.exe
    curl --noproxy '*' localhost:8099/v1/models
    curl --noproxy '*' -X POST localhost:8099/v1/chat/completions \
         -H 'Content-Type: application/json' \
         -d '{"stream":true,"messages":[{"role":"user","content":"hi"}]}'
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8199


def send_json(h, obj, code=200):
    raw = json.dumps(obj).encode()
    h.send_response(code)
    h.send_header("Content-Type", "application/json")
    h.send_header("Content-Length", str(len(raw)))
    h.end_headers()
    h.wfile.write(raw)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        if self.path == "/api/tags":
            send_json(self, {"models": [{"name": "mock-llm:7b", "size": 4500000000}]})
        elif self.path == "/system_stats":
            send_json(self, {"devices": [{"vram_free": 20971520000, "vram_total": 25757220864}]})
        else:
            send_json(self, {"error": "not found"}, 404)

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            body = json.loads(raw)
        except Exception:
            body = {}

        if self.path == "/v1/chat/completions":
            if body.get("stream"):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Connection", "close")
                self.end_headers()
                for tok in ["你", "好"]:
                    chunk = {"choices": [{"delta": {"content": tok}}]}
                    self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
                    self.wfile.flush()
                fin = {"choices": [{"delta": {}, "finish_reason": "stop"}]}
                self.wfile.write(b"data: " + json.dumps(fin).encode() + b"\n\n")
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                return
            # Echo the incoming prompt so the test can prove it was forwarded.
            prompt = ""
            for m in body.get("messages", []):
                c = m.get("content")
                if isinstance(c, str):
                    prompt += c
            send_json(self, {
                "choices": [{"message": {"role": "assistant", "content": "MOCK-REPLY:" + prompt[:40]}}],
                "usage": {"prompt_tokens": 9, "completion_tokens": 4},
            })
            return

        if self.path == "/api/chat":
            send_json(self, {"message": {"role": "assistant", "content": "mock vision"},
                             "prompt_eval_count": 5, "eval_count": 2})
            return

        if self.path == "/api/generate":
            send_json(self, {"done": True})
            return

        send_json(self, {"error": "not found"}, 404)


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
