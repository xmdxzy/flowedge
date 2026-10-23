#!/usr/bin/env python
"""测 MOSS 单张出图的耗时与显存峰值（每次换 seed，避免 ComfyUI 缓存命中）。

为什么要在脚本内采样：放在脚本外面采样会被会话中断（实测 SIGTERM 两次），
而且 start/end 时间点对不齐——用缓存命中的 3 秒当成真实耗时是这里的典型陷阱。
"""
import json
import random
import subprocess
import sys
import threading
import time
import urllib.request

BASE = "http://127.0.0.1:8190"
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
STEPS = int(sys.argv[1]) if len(sys.argv) > 1 else 50
PROMPT = "一张中式茶馆的木质招牌挂在门楣上，招牌上用黑色毛笔字写着「山海茶肆」四个大字，清晨薄雾，写实摄影"

samples = []
stop = False


def sampler():
    while not stop:
        try:
            out = subprocess.run(
                ["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"],
                capture_output=True, text=True, timeout=5)
            v = out.stdout.strip().split("\n")[0].strip()
            if v.isdigit():
                samples.append(int(v))
        except Exception:
            pass
        time.sleep(1)


link = lambda i, s: [i, s]
graph = {
    "1": {"class_type": "UNETLoader", "inputs": {"unet_name": "moss_image_sft_int8_convrot.safetensors", "weight_dtype": "default"}},
    "2": {"class_type": "CLIPLoader", "inputs": {"clip_name": "qwen3.5_9b_w4a8_gptq.safetensors", "type": "moss_image"}},
    "3": {"class_type": "VAELoader", "inputs": {"vae_name": "flux2-vae_small.safetensors"}},
    "4": {"class_type": "CLIPTextEncode", "inputs": {"text": PROMPT, "clip": link("2", 0)}},
    "5": {"class_type": "CLIPTextEncode", "inputs": {"text": "", "clip": link("2", 0)}},
    "6": {"class_type": "EmptyFlux2LatentImage", "inputs": {"width": 1024, "height": 1024, "batch_size": 1}},
    "7": {"class_type": "KSampler", "inputs": {"seed": random.randint(1, 2 ** 40), "steps": STEPS, "cfg": 3.0,
                                              "sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
                                              "model": link("1", 0), "positive": link("4", 0),
                                              "negative": link("5", 0), "latent_image": link("6", 0)}},
    "8": {"class_type": "VAEDecode", "inputs": {"samples": link("7", 0), "vae": link("3", 0)}},
    "9": {"class_type": "SaveImage", "inputs": {"images": link("8", 0), "filename_prefix": "moss_bench"}},
}

th = threading.Thread(target=sampler, daemon=True)
th.start()

t0 = time.time()
req = urllib.request.Request(BASE + "/prompt", data=json.dumps({"prompt": graph}).encode(),
                             headers={"Content-Type": "application/json"})
with opener.open(req, timeout=60) as r:
    pid = json.load(r)["prompt_id"]
while time.time() - t0 < 20 * 60:
    time.sleep(2)
    try:
        with opener.open(BASE + "/history/" + pid, timeout=20) as r:
            h = json.load(r)
    except Exception:
        continue
    e = h.get(pid)
    if not e:
        continue
    if e.get("status", {}).get("status_str") == "error":
        stop = True
        sys.exit("FAILED")
    if any(v.get("images") for v in e.get("outputs", {}).values()):
        break
else:
    stop = True
    sys.exit("TIMEOUT")

stop = True
time.sleep(1.5)
print("steps=%d elapsed=%.1fs  peak_vram=%dMB  base_vram=%dMB  样本=%d" % (
    STEPS, time.time() - t0, max(samples or [0]), min(samples or [0]), len(samples)))
