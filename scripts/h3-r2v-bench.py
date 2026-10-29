"""Ref2VA 权重 A/B 台架：base int8 vs W4A8，12 步台词镜链路。

只给参考图（不给音频）——r2v 合法输入，跳过 TTS 依赖；六段式提示词带
<Picture i> 引用（身份锁的最低要求，见 AGENTS 0.16 多镜身份锁条目）。
用法：
  python scripts/h3-r2v-bench.py --runs 2 --tag r2v-base
  python scripts/h3-r2v-bench.py --runs 2 --tag r2v-w4a8 \
      --unet minimax_h3_ref2va_pruned_w4a8_mixed.safetensors
结果追加 .tmp/h3r2v-results.jsonl。
"""
import argparse, json, os, subprocess, threading, time, urllib.request, uuid

BASE = os.environ.get("H3_BASE", "http://127.0.0.1:8189")
UNET = "minimax_h3_ref2va_pruned_int8_convrot.safetensors"
CLIP = "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors"
VVAE = "minimax_h3_video_vae_fp16.safetensors"
AVAE = "minimax_h3_audio_vae_fp32.safetensors"
REFS = ["ref_1_front.png", "ref_2_three4.png", "ref_3_side.png"]  # data/comfy/input 下

PROMPT = (
    "subject_definitions: <Picture 1> 是主角的正面定妆照，<Picture 2> 是她的四分之三侧面，"
    "<Picture 3> 是她的侧面。主角是蓝发齐肩的 chibi 鱼尾少女手办，穿藏青色女仆裙配白色围裙，"
    "头戴白色褶边女仆帽，站在深色木桌上。"
    "shot_list: 中景固定机位，她朝镜头方向走两小步后停下，歪头看向镜头。"
    "retention_analysis: 全程保持 <Picture 1> 的脸型、发色与裙装细节不漂移，"
    "室内暖光灯自右侧照明，鱼缸冷光自左侧补光。"
    "dialogue: 无台词，不说话。"
    "sound: 两次轻微的脚步声，一次围裙布料摩擦声，持续的低频鱼缸水泵声，无音乐。"
)


def req(path, payload=None, timeout=60):
    data = None if payload is None else json.dumps(payload).encode()
    r = urllib.request.Request(BASE + path, data=data, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def link(node, slot):
    return [str(node), slot]


def upload(name):
    # 文件已在实例的 input 目录（--input-directory），直接引用即可
    return name


def build(width, height, frames, steps, seed, unet):
    g = {
        "1": {"class_type": "UNETLoader", "inputs": {"unet_name": unet, "weight_dtype": "default"}},
        "2": {"class_type": "CLIPLoader", "inputs": {"clip_name": CLIP, "type": "minimax", "device": "default"}},
        "3": {"class_type": "VAELoader", "inputs": {"vae_name": VVAE}},
        "4": {"class_type": "VAELoader", "inputs": {"vae_name": AVAE}},
        "6": {"class_type": "RandomNoise", "inputs": {"noise_seed": seed}},
        "8": {"class_type": "KSamplerSelect", "inputs": {"sampler_name": "res_multistep"}},
        "11": {"class_type": "VAEDecode", "inputs": {"samples": link(10, 0), "vae": link(3, 0)}},
        "12": {"class_type": "VAEDecodeAudio", "inputs": {"samples": link(10, 0), "vae": link(4, 0)}},
        "13": {"class_type": "CreateVideo", "inputs": {"images": link(11, 0), "audio": link(12, 0), "fps": 24.0, "bit_depth": 8}},
        "14": {"class_type": "SaveVideo", "inputs": {"video": link(13, 0), "filename_prefix": "bench-r2v", "format": "mp4", "codec": "auto"}},
    }
    refs = {}
    for i, name in enumerate(REFS):
        g[str(50 + i)] = {"class_type": "LoadImage", "inputs": {"image": upload(name)}}
        refs[f"ref_image_{i + 1}"] = link(50 + i, 0)
    g["5"] = {"class_type": "MiniMaxH3ReferenceToVideo", "inputs": {
        "clip": link(2, 0), "vae": link(3, 0), "audio_vae": link(4, 0),
        "prompt": PROMPT, "width": width, "height": height, "length": frames,
        "ref_image_size": "match", "ref_images": refs, "ref_audios": {}}}
    g["7"] = {"class_type": "BasicGuider", "inputs": {"model": link(41, 0), "conditioning": link(5, 0)}}
    g["9"] = {"class_type": "BasicScheduler", "inputs": {
        "model": link(41, 0), "scheduler": "simple", "steps": steps, "denoise": 1.0}}
    g["10"] = {"class_type": "SamplerCustomAdvanced", "inputs": {
        "noise": link(6, 0), "guider": link(7, 0), "sampler": link(8, 0),
        "sigmas": link(9, 0), "latent_image": link(5, 1)}}
    g["41"] = {"class_type": "MiniMaxH3SigmaShift", "inputs": {
        "model": link(1, 0), "shift_video": 12.0, "shift_audio": 3.0}}
    return g


def vram():
    try:
        out = subprocess.run(["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"],
                             capture_output=True, text=True, timeout=5).stdout.strip().splitlines()[0]
        return int(out)
    except Exception:
        return -1


def run(width, height, frames, steps, seed, tag, unet):
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
    g = build(width, height, frames, steps, seed, unet)
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
            raise TimeoutError("run exceeded 1h")
    wall = time.time() - t0
    stop.set()
    t.join()
    st = hist.get("status", {})
    if st.get("status_str") == "error":
        raise RuntimeError(f"{tag} failed: {json.dumps(st, ensure_ascii=False)[:500]}")
    outs = []
    for node_out in hist.get("outputs", {}).values():
        for key in ("images", "gifs", "video", "audio"):
            for item in node_out.get(key, []) or []:
                outs.append(item.get("filename"))
    rec = {"tag": tag, "seed": seed, "unet": unet, "wall_s": round(wall, 1),
           "peak_vram_mb": peak[0], "out": outs}
    print(json.dumps(rec, ensure_ascii=False), flush=True)
    os.makedirs(".tmp", exist_ok=True)
    with open(".tmp/h3r2v-results.jsonl", "a", encoding="utf-8") as f:
        f.write(json.dumps(rec, ensure_ascii=False) + "\n")
    return rec


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--w", type=int, default=608)
    ap.add_argument("--h", type=int, default=352)
    ap.add_argument("--frames", type=int, default=124)
    ap.add_argument("--steps", type=int, default=12)
    ap.add_argument("--runs", type=int, default=2)
    ap.add_argument("--tag", default="r2v-base")
    ap.add_argument("--unet", default=UNET)
    a = ap.parse_args()
    for i in range(a.runs):
        run(a.w, a.h, a.frames, a.steps, 555000 + i, f"{a.tag}-r{i + 1}", a.unet)
