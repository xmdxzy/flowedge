#!/usr/bin/env python
"""多线程分段下载 MOSS-Image 权重（hf-mirror 单连接只有 ~0.8MB/s，8 段可到 ~6MB/s）。

为什么自己写而不用 hf_transfer：装了 hf_transfer 实测仍只有 0.7MB/s
（2026-10-23，hf-mirror 对单文件限速），而 curl 直连测出 0.84MB/s/连接，
说明瓶颈在单连接而非总带宽 —— 分段就能吃满。

两条硬要求：
  1. 必须绕开本机 http_proxy（代理会把 127 段请求劫持成 502，且更慢）
  2. 必须写进目标文件的**正确偏移**，最后用大小校验收尾——
     分段下载最容易出的错是段拼错位置，文件照样打开只是内容废了。
"""
import os
import sys
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = "https://hf-mirror.com/Comfy-Org/MOSS-Image/resolve/main/"
ROOT = "D:/AI/Models/comfyui"
PARTS = 8

FILES = [
    ("diffusion_models/moss_image_sft_int8_convrot.safetensors", 5_400_000_000),
    ("text_encoders/qwen3.5_9b_w4a8_gptq.safetensors", 5_000_000_000),
    ("vae/flux2-vae_small.safetensors", 340_000_000),
]

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
opener.addheaders = [("User-Agent", "flowedge-fetch/1.0")]

lock = threading.Lock()
done_bytes = [0]


def head_size(url):
    req = urllib.request.Request(url, method="HEAD")
    with opener.open(req, timeout=60) as r:
        return int(r.headers["Content-Length"])


def fetch_range(url, start, end, path, retries=4):
    for attempt in range(retries):
        try:
            req = urllib.request.Request(url, headers={"Range": "bytes=%d-%d" % (start, end)})
            with opener.open(req, timeout=120) as r:
                data = r.read()
            if len(data) != end - start + 1:
                raise IOError("段长 %d != %d" % (len(data), end - start + 1))
            with lock:
                with open(path, "r+b") as f:
                    f.seek(start)
                    f.write(data)
                done_bytes[0] += len(data)
            return
        except Exception as e:
            if attempt == retries - 1:
                raise
            time.sleep(2 * (attempt + 1))


def download(rel, expect):
    dest = os.path.join(ROOT, rel.replace("/", os.sep))
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    url = BASE + rel
    size = head_size(url)
    print("[%s] size=%d (%.2f GB)" % (rel, size, size / 1e9), flush=True)
    tmp = dest + ".part"
    if not os.path.exists(tmp) or os.path.getsize(tmp) != size:
        with open(tmp, "wb") as f:
            f.truncate(size)
    step = (size + PARTS - 1) // PARTS
    ranges = []
    for i in range(PARTS):
        s = i * step
        e = min(size - 1, s + step - 1)
        if s > e:
            continue
        ranges.append((s, e))
    t0 = time.time()
    with ThreadPoolExecutor(max_workers=PARTS) as ex:
        list(ex.map(lambda r: fetch_range(url, r[0], r[1], tmp), ranges))
    got = os.path.getsize(tmp)
    dt = time.time() - t0
    print("[%s] 完成 %d bytes，用时 %.0fs（%.1f MB/s）" % (rel, got, dt, got / dt / 1e6), flush=True)
    if got != size:
        sys.exit("[%s] 大小不符 %d != %d" % (rel, got, size))
    if os.path.exists(dest):
        os.remove(dest)
    os.rename(tmp, dest)
    print("[ok] %s" % dest, flush=True)


if __name__ == "__main__":
    only = sys.argv[1] if len(sys.argv) > 1 else None
    for rel, exp in FILES:
        if only and only not in rel:
            continue
        download(rel, exp)
    print("ALL_OK", flush=True)
