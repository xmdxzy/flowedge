"""P2 批量构图 A/B：N 镜逐镜提交（seq）vs 合并一张图 + 屏障（batch）。

先测后接：引擎侧改造（shotlist 批量路径）只有在 batch 模式实测省时之后才做。
两臂同实例、同 prompts、同 seeds、同参数，唯一变量是图的形态。

用法：
  python scripts/h3-batch-bench.py --shots 3 --mode both
  python scripts/h3-batch-bench.py --shots 3 --mode batch   # 只跑批量臂
环境变量 H3_BASE 指定实例（默认 127.0.0.1:8189）。
结果追加 .tmp/h3batch-results.jsonl。
"""
import argparse, json, os, subprocess, threading, time, urllib.request, uuid

BASE = os.environ.get("H3_BASE", "http://127.0.0.1:8189")
UNET = os.environ.get("H3_UNET", "minimax_h3_fl2va_pruned_int8_convrot.safetensors")
CLIP = "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors"
VVAE = "minimax_h3_video_vae_fp16.safetensors"
AVAE = "minimax_h3_audio_vae_fp32.safetensors"

PROMPTS = [
    ("A medium shot of a chibi fish-girl figurine in a navy maid dress on a dark wooden desk. "
     "She takes two small steps, turns to the lens, tilts her head and winks. "
     "Sound: two wet footsteps, a soft apron creak, a low aquarium hum."),
    ("Close-up of the same chibi fish-girl figurine, warm desk lamp from the right. "
     "She leans forward and blows a small bubble that pops into droplets. "
     "Sound: a soft pop, water droplets tapping wood, a low aquarium hum."),
    ("Wide shot of the chibi fish-girl figurine at the desk edge, cool blue aquarium glow. "
     "She waves one fin, then sits back down cross-legged. "
     "Sound: fabric rustle, a single drip, a low aquarium hum."),
]

LORA_TURBO = "minimax_h3_turbo_v4_step600_ema.safetensors"


def req(path, payload=None, timeout=60):
    """带瞬时拒连重试：实测 ComfyUI 忙时会偶发一次 connection refused，
    直接放弃会把整轮 A/B 报废（服务器其实活着并在继续执行）。"""
    data = None if payload is None else json.dumps(payload).encode()
    last = None
    for attempt in range(4):
        try:
            r = urllib.request.Request(BASE + path, data=data, headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(r, timeout=timeout) as resp:
                return json.loads(resp.read().decode())
        except urllib.error.URLError as e:
            reason = getattr(e, "reason", None)
            if not isinstance(reason, ConnectionError):
                raise
            last = e
            time.sleep(2 * (attempt + 1))
    raise last


def link(node, slot):
    return [str(node), slot]


def build_seq(width, height, frames, steps, seed, prompt, turbo):
    """与引擎 h3 单镜图等价的独立图（同 scripts/h3-bench.py）。"""
    g = {
        "1": {"class_type": "UNETLoader", "inputs": {"unet_name": UNET, "weight_dtype": "default"}},
        "2": {"class_type": "CLIPLoader", "inputs": {"clip_name": CLIP, "type": "minimax", "device": "default"}},
        "3": {"class_type": "VAELoader", "inputs": {"vae_name": VVAE}},
        "4": {"class_type": "VAELoader", "inputs": {"vae_name": AVAE}},
        "5": {"class_type": "MiniMaxH3ImageToVideo", "inputs": {
            "clip": link(2, 0), "vae": link(3, 0), "prompt": prompt,
            "width": width, "height": height, "length": frames}},
        "6": {"class_type": "RandomNoise", "inputs": {"noise_seed": seed}},
        "8": {"class_type": "KSamplerSelect", "inputs": {"sampler_name": "res_multistep"}},
        "11": {"class_type": "VAEDecode", "inputs": {"samples": link(10, 0), "vae": link(3, 0)}},
        "12": {"class_type": "VAEDecodeAudio", "inputs": {"samples": link(10, 0), "vae": link(4, 0)}},
        "13": {"class_type": "CreateVideo", "inputs": {"images": link(11, 0), "audio": link(12, 0), "fps": 24.0, "bit_depth": 8}},
        "14": {"class_type": "SaveVideo", "inputs": {"video": link(13, 0), "filename_prefix": "bench-seq", "format": "mp4", "codec": "auto"}},
    }
    model = link(1, 0)
    if turbo:
        g["40"] = {"class_type": "LoraLoaderModelOnly", "inputs": {
            "model": model, "lora_name": LORA_TURBO, "strength_model": 1.0}}
        model = link(40, 0)
    g["41"] = {"class_type": "MiniMaxH3SigmaShift", "inputs": {
        "model": model, "shift_video": 12.0, "shift_audio": 6.0 if turbo else 3.0}}
    model = link(41, 0)
    g["7"] = {"class_type": "BasicGuider", "inputs": {"model": model, "conditioning": link(5, 0)}}
    g["9"] = {"class_type": "BasicScheduler", "inputs": {
        "model": model, "scheduler": "simple", "steps": steps, "denoise": 1.0}}
    g["10"] = {"class_type": "SamplerCustomAdvanced", "inputs": {
        "noise": link(6, 0), "guider": link(7, 0), "sampler": link(8, 0),
        "sigmas": link(9, 0), "latent_image": link(5, 1)}}
    return g


def build_batch(width, height, frames, steps, seeds, prompts, turbo):
    """N 镜合并图：loaders 共享， conditioning/latent 过屏障强制两阶段。"""
    g = {
        "1": {"class_type": "UNETLoader", "inputs": {"unet_name": UNET, "weight_dtype": "default"}},
        "2": {"class_type": "CLIPLoader", "inputs": {"clip_name": CLIP, "type": "minimax", "device": "default"}},
        "3": {"class_type": "VAELoader", "inputs": {"vae_name": VVAE}},
        "4": {"class_type": "VAELoader", "inputs": {"vae_name": AVAE}},
    }
    model = link(1, 0)
    if turbo:
        g["40"] = {"class_type": "LoraLoaderModelOnly", "inputs": {
            "model": model, "lora_name": LORA_TURBO, "strength_model": 1.0}}
        model = link(40, 0)
    g["41"] = {"class_type": "MiniMaxH3SigmaShift", "inputs": {
        "model": model, "shift_video": 12.0, "shift_audio": 6.0 if turbo else 3.0}}
    model = link(41, 0)

    cond_in, lat_in = {}, {}
    for s in range(len(prompts)):
        e, c, n, gsel, sched, samp = 50 + s, 70 + s, 60 + s, 80 + s, 85 + s, 100 + s
        vd, ad, vid, sv = 110 + s, 120 + s, 130 + s, 140 + s
        g[str(e)] = {"class_type": "MiniMaxH3ImageToVideo", "inputs": {
            "clip": link(2, 0), "vae": link(3, 0), "prompt": prompts[s],
            "width": width, "height": height, "length": frames}}
        g[str(n)] = {"class_type": "RandomNoise", "inputs": {"noise_seed": seeds[s]}}
        g[str(gsel)] = {"class_type": "KSamplerSelect", "inputs": {"sampler_name": "res_multistep"}}
        g[str(sched)] = {"class_type": "BasicScheduler", "inputs": {
            "model": model, "scheduler": "simple", "steps": steps, "denoise": 1.0}}
        g[str(c)] = {"class_type": "BasicGuider", "inputs": {
            "model": model, "conditioning": link(90, s)}}
        g[str(samp)] = {"class_type": "SamplerCustomAdvanced", "inputs": {
            "noise": link(n, 0), "guider": link(c, 0), "sampler": link(gsel, 0),
            "sigmas": link(sched, 0), "latent_image": link(e, 1)}}
        g[str(vd)] = {"class_type": "VAEDecode", "inputs": {"samples": link(91, s), "vae": link(3, 0)}}
        g[str(ad)] = {"class_type": "VAEDecodeAudio", "inputs": {"samples": link(91, s), "vae": link(4, 0)}}
        g[str(vid)] = {"class_type": "CreateVideo", "inputs": {
            "images": link(vd, 0), "audio": link(ad, 0), "fps": 24.0, "bit_depth": 8}}
        g[str(sv)] = {"class_type": "SaveVideo", "inputs": {
            "video": link(vid, 0), "filename_prefix": f"bench-batch-s{s}", "format": "mp4", "codec": "auto"}}
        cond_in[f"cond_{s + 1}"] = link(e, 0)
        lat_in[f"lat_{s + 1}"] = link(samp, 0)

    # 屏障：所有采样器等所有 TE 编码；所有解码等所有采样。
    # guider 在循环里直接接屏障（JSON 字典无顺序约束，先引用后定义无所谓）。
    g["90"] = {"class_type": "FlowEdgeCondBarrier", "inputs": cond_in}
    g["91"] = {"class_type": "FlowEdgeLatentBarrier", "inputs": lat_in}
    return g


def vram():
    try:
        out = subprocess.run(["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"],
                             capture_output=True, text=True, timeout=5).stdout.strip().splitlines()[0]
        return int(out)
    except Exception:
        return -1


def submit_and_wait(g, tag):
    cid = str(uuid.uuid4())
    t0 = time.time()
    r = req("/prompt", {"prompt": g, "client_id": cid})
    pid = r["prompt_id"]
    while True:
        time.sleep(2)
        h = req("/history/" + pid)
        if pid in h:
            hist = h[pid]
            break
        if time.time() - t0 > 3600:
            raise TimeoutError(f"{tag} exceeded 1h")
    wall = time.time() - t0
    st = hist.get("status", {})
    if st.get("status_str") == "error":
        detail = ""
        for name, payload in st.get("messages", []):
            if name == "execution_error":
                detail = json.dumps(payload, ensure_ascii=False)[:800]
                break
        raise RuntimeError(f"{tag} failed: {detail or json.dumps(st)[:300]}")
    stamps = {}
    for name, payload in st.get("messages", []):
        stamps[name] = payload.get("timestamp")
    server = None
    if "execution_start" in stamps and "execution_success" in stamps:
        server = (stamps["execution_success"] - stamps["execution_start"]) / 1000.0
    outs = []
    for node_out in hist.get("outputs", {}).values():
        for key in ("images", "gifs", "video", "audio"):
            for item in node_out.get(key, []) or []:
                outs.append(item.get("filename"))
    rec = {"tag": tag, "wall_s": round(wall, 1),
           "server_s": round(server, 1) if server else None,
           "outs": outs}
    return rec


def run_arm(mode, width, height, frames, steps, shots, turbo, nonce=0):
    # nonce 挪开 seed 与 prompt 尾巴：ComfyUI 有节点输出缓存，完全相同的图会
    # 直接返回旧产物（实测 2s 出"结果"），A/B 两臂必须同 nonce、跨次必不同。
    seeds = [123456 + nonce + i for i in range(shots)]
    prompts = [PROMPTS[i % len(PROMPTS)] + f" Take {nonce}." for i in range(shots)]
    peak = [0]
    stop = threading.Event()

    def sampler():
        while not stop.is_set():
            v = vram()
            if v > peak[0]:
                peak[0] = v
            stop.wait(0.8)

    t = threading.Thread(target=sampler, daemon=True)
    t.start()
    recs = []
    try:
        if mode == "seq":
            total0 = time.time()
            for s in range(shots):
                rec = submit_and_wait(build_seq(width, height, frames, steps, seeds[s], prompts[s], turbo),
                                      f"seq-s{s}")
                recs.append(rec)
                print(json.dumps(rec, ensure_ascii=False), flush=True)
            total = time.time() - total0
        else:
            total0 = time.time()
            rec = submit_and_wait(build_batch(width, height, frames, steps, seeds, prompts, turbo),
                                  "batch")
            total = time.time() - total0
            recs.append(rec)
            print(json.dumps(rec, ensure_ascii=False), flush=True)
    finally:
        stop.set()
        t.join()
    summary = {"mode": mode, "shots": shots, "frames": frames, "steps": steps, "turbo": turbo, "nonce": nonce,
               "total_wall_s": round(total, 1), "peak_vram_mb": peak[0], "runs": recs}
    print("SUMMARY " + json.dumps(summary, ensure_ascii=False), flush=True)
    os.makedirs(".tmp", exist_ok=True)
    with open(".tmp/h3batch-results.jsonl", "a", encoding="utf-8") as f:
        f.write(json.dumps(summary, ensure_ascii=False) + "\n")
    return summary


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--w", type=int, default=608)
    ap.add_argument("--h", type=int, default=352)
    ap.add_argument("--frames", type=int, default=124)
    ap.add_argument("--steps", type=int, default=12)
    ap.add_argument("--shots", type=int, default=3)
    ap.add_argument("--turbo", action="store_true")
    ap.add_argument("--mode", choices=["seq", "batch", "both"], default="both")
    ap.add_argument("--nonce", type=int, default=None,
                    help="挪 seed/prompt 防节点缓存；缺省随机")
    a = ap.parse_args()
    import random
    nonce = a.nonce if a.nonce is not None else random.randint(1, 10**6)
    print(f"nonce={nonce}", flush=True)
    modes = ["seq", "batch"] if a.mode == "both" else [a.mode]
    for m in modes:
        run_arm(m, a.w, a.h, a.frames, a.steps, a.shots, a.turbo, nonce)
