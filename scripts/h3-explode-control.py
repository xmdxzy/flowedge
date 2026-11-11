"""MiniMax H3 + Fun ControlNet(depth) 爆炸图实验。
几何由深度序列锁定，材质/光影由 H3 生成。
"""
import argparse
import json
import os
import time
import urllib.request
import uuid

BASE = os.environ.get("H3_BASE", "http://127.0.0.1:8189")
UNET = "minimax_h3_fl2va_pruned_int8_convrot.safetensors"
CLIP = "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors"
VVAE = "minimax_h3_video_vae_fp16.safetensors"
AVAE = "minimax_h3_audio_vae_fp32.safetensors"
CN = "minimax_h3_fun_controlnet_union_pruned_int8_convrot.safetensors"

PROMPT = (
    "integrated_multimodal_description: [Shot 1] Photoreal product studio, seamless charcoal "
    "background, one hard key light from upper-left with soft rim, 9:16 vertical framing, product "
    "centered. A stainless-steel insulated tumbler slowly explodes into exactly four rigid parts "
    "along one vertical axis: a brushed-steel screw lid on top, a thin white silicone gasket ring "
    "with a visible center hole below it, the tall matte-steel cup body, and an open-mouth "
    "polished inner vacuum liner at the bottom. The parts separate evenly and stay parallel, "
    "coaxial and undeformed, with clean air gaps between them. No rotation, no extra parts, "
    "no text, no labels. Camera locked on tripod, slow push-in.\noverall_soundscape: none\n"
    "non_diegetic_music: N/A"
)


def req(path, payload=None, timeout=120):
    data = None if payload is None else json.dumps(payload).encode()
    r = urllib.request.Request(BASE + path, data=data, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        print("HTTP", e.code, e.read().decode()[:2500], flush=True)
        raise


def link(n, s):
    return [str(n), s]


def build(w, h, frames, steps, seed, depth_path, strength):
    g = {
        "1": {"class_type": "UNETLoader", "inputs": {"unet_name": UNET, "weight_dtype": "default"}},
        "2": {"class_type": "CLIPLoader", "inputs": {"clip_name": CLIP, "type": "minimax", "device": "default"}},
        "3": {"class_type": "VAELoader", "inputs": {"vae_name": VVAE}},
        "4": {"class_type": "VAELoader", "inputs": {"vae_name": AVAE}},
        "20": {"class_type": "VHS_LoadVideoPath", "inputs": {
            "video": depth_path, "force_rate": 24.0, "custom_width": 0, "custom_height": 0,
            "frame_load_cap": frames, "skip_first_frames": 0, "select_every_nth": 1}},
        "21": {"class_type": "ModelPatchLoader", "inputs": {"name": CN}},
        "22": {"class_type": "MiniMaxH3FunControlNetApply", "inputs": {
            "model": link(1, 0), "model_patch": link(21, 0), "vae": link(3, 0),
            "strength": strength, "start_percent": 0.0, "end_percent": 1.0,
            "control_video": link(20, 0)}},
        "23": {"class_type": "MiniMaxH3SigmaShift", "inputs": {
            "model": link(22, 0), "shift_video": 12.0, "shift_audio": 3.0}},
        "5": {"class_type": "MiniMaxH3ImageToVideo", "inputs": {
            "clip": link(2, 0), "vae": link(3, 0), "prompt": PROMPT,
            "width": w, "height": h, "length": frames}},
        "6": {"class_type": "RandomNoise", "inputs": {"noise_seed": seed}},
        "8": {"class_type": "KSamplerSelect", "inputs": {"sampler_name": "res_multistep"}},
        "7": {"class_type": "BasicGuider", "inputs": {"model": link(23, 0), "conditioning": link(5, 0)}},
        "9": {"class_type": "BasicScheduler", "inputs": {
            "model": link(23, 0), "scheduler": "simple", "steps": steps, "denoise": 1.0}},
        "10": {"class_type": "SamplerCustomAdvanced", "inputs": {
            "noise": link(6, 0), "guider": link(7, 0), "sampler": link(8, 0),
            "sigmas": link(9, 0), "latent_image": link(5, 1)}},
        "11": {"class_type": "VAEDecode", "inputs": {"samples": link(10, 0), "vae": link(3, 0)}},
        "12": {"class_type": "VAEDecodeAudio", "inputs": {"samples": link(10, 0), "vae": link(4, 0)}},
        "13": {"class_type": "CreateVideo", "inputs": {
            "images": link(11, 0), "audio": link(12, 0), "fps": 24.0, "bit_depth": 8}},
        "14": {"class_type": "SaveVideo", "inputs": {
            "video": link(13, 0), "filename_prefix": "explode-cn", "format": "mp4", "codec": "auto"}},
    }
    return g


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--w", type=int, default=352)
    ap.add_argument("--h", type=int, default=608)
    ap.add_argument("--frames", type=int, default=124)
    ap.add_argument("--steps", type=int, default=12)
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--strength", type=float, default=1.0)
    ap.add_argument("--depth", default=os.path.abspath("data/comfy/input/explode-depth.mp4"))
    ap.add_argument("--tag", default="cn")
    ap.add_argument("--prompt-file", default="")
    a = ap.parse_args()

    global PROMPT
    if a.prompt_file:
        with open(a.prompt_file, encoding="utf-8") as f:
            PROMPT = f.read().strip()

    g = build(a.w, a.h, a.frames, a.steps, a.seed, a.depth, a.strength)
    cid = str(uuid.uuid4())
    t0 = time.time()
    r = req("/prompt", {"prompt": g, "client_id": cid})
    pid = r["prompt_id"]
    print("submitted", pid, flush=True)
    while True:
        time.sleep(3)
        h = req("/history/" + pid)
        if pid in h:
            hist = h[pid]
            break
        if time.time() - t0 > 1800:
            raise TimeoutError("timeout")
    wall = round(time.time() - t0, 1)
    outs = []
    for node_out in hist.get("outputs", {}).values():
        for key in ("images", "gifs", "video", "audio"):
            for item in node_out.get(key, []) or []:
                outs.append(item.get("filename"))
    print(json.dumps({"tag": a.tag, "wall_s": wall, "out": outs}, ensure_ascii=False))


if __name__ == "__main__":
    main()
