import argparse, json, subprocess, threading, time, urllib.request, uuid, os, sys

BASE = os.environ.get("H3_BASE", "http://127.0.0.1:8189")
UNET = "minimax_h3_fl2va_pruned_int8_convrot.safetensors"
CLIP = "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors"
VVAE = "minimax_h3_video_vae_fp16.safetensors"
AVAE = "minimax_h3_audio_vae_fp32.safetensors"
LORA = "minimax_h3_turbo_v4_step600_ema.safetensors"

PROMPT = ("A medium shot of a chibi fish-girl figurine in a navy maid dress standing on a dark wooden desk, "
          "warm tungsten desk lamp from the right, cool blue aquarium glow from the left, shallow water on the wood. "
          "She takes two small steps, turns to the lens, tilts her head and winks. "
          "Sound: two wet footsteps, a soft apron creak, one slow drip, a low aquarium hum. Music: none.")


def req(path, payload=None, timeout=60):
    data = None if payload is None else json.dumps(payload).encode()
    r = urllib.request.Request(BASE + path, data=data, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def link(node, slot):
    return [str(node), slot]


def build(width, height, frames, steps, seed, turbo, suffix="", unet=UNET, vvae=VVAE, lora="", shift_audio=None):
    prompt = PROMPT + (" " + suffix if suffix else "")
    g = {
        "1": {"class_type": "UNETLoader", "inputs": {"unet_name": unet, "weight_dtype": "default"}},
        "2": {"class_type": "CLIPLoader", "inputs": {"clip_name": CLIP, "type": "minimax", "device": "default"}},
        "3": {"class_type": "VAELoader", "inputs": {"vae_name": vvae}},
        "4": {"class_type": "VAELoader", "inputs": {"vae_name": AVAE}},
        "5": {"class_type": "MiniMaxH3ImageToVideo", "inputs": {
            "clip": link(2, 0), "vae": link(3, 0), "prompt": prompt,
            "width": width, "height": height, "length": frames}},
        "6": {"class_type": "RandomNoise", "inputs": {"noise_seed": seed}},
        "8": {"class_type": "KSamplerSelect", "inputs": {"sampler_name": "res_multistep"}},
        "11": {"class_type": "VAEDecode", "inputs": {"samples": link(10, 0), "vae": link(3, 0)}},
        "12": {"class_type": "VAEDecodeAudio", "inputs": {"samples": link(10, 0), "vae": link(4, 0)}},
        "13": {"class_type": "CreateVideo", "inputs": {"images": link(11, 0), "audio": link(12, 0), "fps": 24.0, "bit_depth": 8}},
        "14": {"class_type": "SaveVideo", "inputs": {"video": link(13, 0), "filename_prefix": "bench-h3", "format": "mp4", "codec": "auto"}},
    }
    model = link(1, 0)
    if lora:
        g["40"] = {"class_type": "LoraLoaderModelOnly", "inputs": {
            "model": model, "lora_name": lora, "strength_model": 1.0}}
        model = link(40, 0)
    if turbo:
        g["40"] = {"class_type": "LoraLoaderModelOnly", "inputs": {
            "model": model, "lora_name": LORA, "strength_model": 1.0}}
        model = link(40, 0)
    if shift_audio is None:
        shift_audio = 6.0 if (turbo or lora) else 3.0
    g["41"] = {"class_type": "MiniMaxH3SigmaShift", "inputs": {
        "model": model, "shift_video": 12.0, "shift_audio": shift_audio}}
    model = link(41, 0)
    g["7"] = {"class_type": "BasicGuider", "inputs": {"model": model, "conditioning": link(5, 0)}}
    g["9"] = {"class_type": "BasicScheduler", "inputs": {
        "model": model, "scheduler": "simple", "steps": steps, "denoise": 1.0}}
    g["10"] = {"class_type": "SamplerCustomAdvanced", "inputs": {
        "noise": link(6, 0), "guider": link(7, 0), "sampler": link(8, 0),
        "sigmas": link(9, 0), "latent_image": link(5, 1)}}
    return g


def vram():
    try:
        out = subprocess.run(["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"],
                             capture_output=True, text=True, timeout=5).stdout.strip().splitlines()[0]
        return int(out)
    except Exception:
        return -1


def run(width, height, frames, steps, seed, turbo, tag, suffix="", unet=UNET, vvae=VVAE, lora="", shift_audio=None):
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
    g = build(width, height, frames, steps, seed, turbo, suffix, unet, vvae, lora, shift_audio)
    cid = str(uuid.uuid4())
    t0 = time.time()
    r = req("/prompt", {"prompt": g, "client_id": cid})
    pid = r["prompt_id"]
    hist = None
    while True:
        time.sleep(2)
        h = req("/history/" + pid)
        if pid in h:
            hist = h[pid]
            break
        if time.time() - t0 > 3600:
            raise TimeoutError("run exceeded 1h")
    wall = time.time() - t0
    stop.set()
    msgs = hist.get("status", {}).get("messages", [])
    stamps = {}
    for name, payload in msgs:
        stamps[name] = payload.get("timestamp")
    server = None
    if "execution_start" in stamps and "execution_success" in stamps:
        server = (stamps["execution_success"] - stamps["execution_start"]) / 1000.0
    outs = []
    for node_out in hist.get("outputs", {}).values():
        for key in ("images", "gifs", "video", "audio"):
            for item in node_out.get(key, []) or []:
                outs.append(item.get("filename"))
    rec = {"tag": tag, "w": width, "h": height, "frames": frames, "steps": steps,
           "turbo": turbo, "lora": lora, "shift_audio": shift_audio, "seed": seed, "wall_s": round(wall, 1),
           "server_s": round(server, 1) if server else None,
           "peak_vram_mb": peak[0], "out": outs}
    print(json.dumps(rec, ensure_ascii=False), flush=True)
    with open(".tmp/h3bench-results.jsonl", "a", encoding="utf-8") as f:
        f.write(json.dumps(rec, ensure_ascii=False) + "\n")
    return rec


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--w", type=int, default=608)
    ap.add_argument("--h", type=int, default=352)
    ap.add_argument("--frames", type=int, default=124)
    ap.add_argument("--steps", type=int, default=12)
    ap.add_argument("--runs", type=int, default=2)
    ap.add_argument("--turbo", action="store_true")
    ap.add_argument("--tag", default="baseline")
    ap.add_argument("--suffix", default="")
    ap.add_argument("--unet", default=UNET)
    ap.add_argument("--vae", default=VVAE)
    ap.add_argument("--lora", default="")
    ap.add_argument("--shift-audio", type=float, default=None)
    a = ap.parse_args()
    for i in range(a.runs):
        s = a.suffix + (f" Take {i+1}." if a.suffix else "")
        run(a.w, a.h, a.frames, a.steps, 123456 + i, a.turbo, f"{a.tag}-r{i+1}", s, a.unet, a.vae, a.lora, a.shift_audio)
