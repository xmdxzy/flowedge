#!/usr/bin/env python3
"""判定 ACE-Step 的 reference_audio_path 是「节奏锚」还是「风格锚」。

为什么必须实测：这决定「配乐能不能踩画面节奏」。
  · 若是**节奏锚** → 可以喂一条按画面事件排的 click 轨，让音乐跟着卡点；
  · 若是**风格锚** → 只能靠多生成几条人工挑；
  · 若无影响 → 这个参数不该写进方案。

实验设计（单变量对照）：
  reference = 160 BPM 规律 click（每 0.375s 一击）+ 4.4kHz 持续正弦（音乐里罕见的频段）
  臂 A：纯 text2music，固定 seed
  臂 B：同 prompt / 同 seed / 同 bpm，仅多一个 reference_audio_path + cover_strength=1.0
判据：
  · BPM_B 接近 160 而 BPM_A 不接近 → 节奏锚
  · 4.4kHz 能量占比在 B 显著升高 → 风格锚
  · 两者都无差异 → 参数无效
"""
import json
import math
import os
import struct
import subprocess
import sys
import tempfile
import time
import urllib.request
import wave

import numpy as np

BASE = "http://127.0.0.1:8104"
OUT = "运行/data/generated"
# reference 路径规则（源码 release_task_audio_paths.py 实测）：
#   · 绝对路径 → 必须落在**服务进程自己的** tempfile.gettempdir() 内，否则 400
#     （引擎派生时改过 TEMP，与我这个 shell 的 temp 不是同一个，所以绝对路径全拒）
#   · 相对路径 → 不含 ".." 就放行，相对**服务 cwd**（= ACE 仓库根）
# 结论：参考音频放 ACE 仓库根、传裸文件名最稳。
ACE_HOME = r"D:\AI\ACE-Step-1.5"
REF_LOCAL = os.path.join(ACE_HOME, "_ref-160bpm.wav")   # 实际写盘位置
REF_PARAM = "_ref-160bpm.wav"                          # 传给 API 的相对路径
SEED = 20261103
PROMPT = "minimal cinematic tech underscore, warm analog synth pad, sparse metallic plucks, no drums, no vocals"
DUR = 12.0

urllib.request.install_opener(
    urllib.request.build_opener(urllib.request.ProxyHandler({})))


# ---------- 构造参考音轨 ----------
def make_reference(path: str, bpm: float = 160.0, dur: float = 12.0, sr: int = 48000) -> None:
    n = int(dur * sr)
    t = np.arange(n) / sr
    sig = np.zeros(n, dtype=np.float64)
    # 每拍一击：1kHz 短促衰减（明确的时间结构）
    period = 60.0 / bpm
    click_len = int(0.012 * sr)
    env = np.exp(-np.arange(click_len) / (0.002 * sr))
    burst = np.sin(2 * math.pi * 1000 * np.arange(click_len) / sr) * env
    k = 0
    while True:
        start = int(k * period * sr)
        if start + click_len >= n:
            break
        sig[start:start + click_len] += 0.55 * burst
        k += 1
    # 4.4kHz 持续正弦：风格标记（正常配乐几乎没有这个频段的持续能量）
    sig += 0.12 * np.sin(2 * math.pi * 4400 * t)
    pcm = np.clip(sig, -1, 1)
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes((pcm * 32767).astype(np.int16).tobytes())
    print(f"  reference: {path}  {bpm:.0f}BPM  {k} 击  +4.4kHz")


# ---------- BPM 估计（自相关 onset）----------
def load_mono(path: str, sr: int = 8000) -> np.ndarray:
    p = subprocess.run(["ffmpeg", "-v", "error", "-i", path, "-map", "0:a:0",
                        "-f", "s16le", "-ac", "1", "-ar", str(sr), "-"],
                       capture_output=True)
    return np.frombuffer(p.stdout, dtype=np.int16).astype(np.float64) / 32768.0


def estimate_bpm(path: str) -> tuple[float, float]:
    """返回 (BPM, 自相关峰值)；峰值低说明没有稳定节拍。"""
    sr = 8000
    x = load_mono(path, sr)
    if x.size < sr:
        return 0.0, 0.0
    hop = 64
    nfr = x.size // hop
    e = np.array([np.sqrt(np.mean(x[i * hop:(i + 1) * hop] ** 2)) for i in range(nfr)])
    onset = np.maximum(np.diff(e), 0)          # 半波整流差分 = onset envelope
    onset -= onset.mean()
    if not np.any(onset > 0):
        return 0.0, 0.0
    ac = np.correlate(onset, onset, mode="full")[len(onset) - 1:]
    fps = sr / hop
    lo, hi = int(fps * 60 / 200), int(fps * 60 / 60)   # 60–200 BPM
    if hi >= len(ac):
        hi = len(ac) - 1
    seg = ac[lo:hi]
    if seg.size == 0:
        return 0.0, 0.0
    lag = lo + int(np.argmax(seg))
    peak = float(ac[lag] / (ac[0] + 1e-9))
    return 60.0 * fps / lag, peak


def band_ratio(path: str, center: float = 4400.0, width: float = 400.0) -> float:
    """目标窄带能量占总能量的比例。"""
    x = load_mono(path, 16000)
    if x.size < 4096:
        return 0.0
    spec = np.abs(np.fft.rfft(x * np.hanning(x.size))) ** 2
    freqs = np.fft.rfftfreq(x.size, 1 / 16000)
    band = (freqs >= center - width) & (freqs <= center + width)
    total = spec.sum() + 1e-12
    return float(spec[band].sum() / total)


# ---------- 调用 ----------
def post(path: str, body: dict, timeout: int = 1800) -> dict:
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def gen(tag: str, extra: dict) -> dict:
    body = {
        "prompt": PROMPT, "lyrics": "", "audio_duration": DUR, "audio_format": "wav",
        "batch_size": 1, "inference_steps": 8, "model": "acestep-v15-xl-turbo",
        "use_cot_caption": False, "use_cot_language": False,
        "use_random_seed": False, "seed": SEED,
    }
    body.update(extra)
    t0 = time.time()
    tid = post("/release_task", body)["data"]["task_id"]
    while True:
        item = post("/query_result", {"task_id_list": [tid]})["data"][0]
        if item["status"] == 1:
            break
        if item["status"] == 2:
            raise SystemExit(f"[{tag}] failed: {str(item.get('result'))[:300]}")
        time.sleep(1)
    el = time.time() - t0
    url = json.loads(item["result"])[0]["file"]
    out = os.path.abspath(f"{OUT}/_refexp-{tag}.wav")
    with urllib.request.urlopen(BASE + url, timeout=600) as r, open(out, "wb") as f:
        f.write(r.read())
    bpm, peak = estimate_bpm(out)
    hi = band_ratio(out)
    print(f"  [{tag}] {el:5.1f}s  BPM≈{bpm:5.1f} (ac峰 {peak:.3f})  4.4k占比 {hi*100:.2f}%")
    return {"tag": tag, "sec": round(el, 1), "bpm": round(bpm, 1),
            "ac_peak": round(peak, 3), "hf_ratio_pct": round(hi * 100, 3), "file": out}


def main() -> int:
    print("▶ 构造参考音轨")
    make_reference(REF_LOCAL)
    rb, rp = estimate_bpm(REF_LOCAL)
    rh = band_ratio(REF_LOCAL)
    print(f"  [ref] BPM≈{rb:.1f} (ac峰 {rp:.3f})  4.4k占比 {rh*100:.2f}%")

    print("\n▶ 预热（XL 冷加载）")
    h = json.loads(urllib.request.urlopen(BASE + "/health", timeout=30).read())["data"]
    if not h["models_initialized"]:
        gen("warmup", {})
    else:
        print("  已初始化，跳过")

    print("\n▶ 臂 A：纯 text2music")
    a = gen("A-textonly", {})
    print("\n▶ 臂 B：+ reference_audio_path (cover_strength=1.0)")
    b = gen("B-reference", {"reference_audio_path": REF_PARAM,
                            "audio_cover_strength": 1.0})

    print("\n=== 判定 ===")
    d_bpm = abs(b["bpm"] - 160.0)
    a_bpm = abs(a["bpm"] - 160.0)
    bpm_signal = d_bpm < 8 and a_bpm > 8
    hf_signal = b["hf_ratio_pct"] > max(0.5, a["hf_ratio_pct"] * 2)
    print(f"  BPM: A={a['bpm']} B={b['bpm']} 目标160 | 距目标 A={a_bpm:.1f} B={d_bpm:.1f}")
    print(f"  4.4kHz 占比: A={a['hf_ratio_pct']}% B={b['hf_ratio_pct']}%")
    if bpm_signal:
        print("  → **节奏锚**：输出节拍跟着 reference 走，可以做卡点")
    elif hf_signal:
        print("  → **风格锚**：音色被 reference 影响，但节拍不跟随")
    else:
        print("  → **无明显影响**：这个参数在 turbo 模型上可能被忽略，别写进方案")
    json.dump({"ref": {"bpm": rb, "hf": rh}, "A": a, "B": b},
              open(".tmp/refexp.json", "w"), ensure_ascii=False, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
