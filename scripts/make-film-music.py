#!/usr/bin/env python3
"""最高质量配乐：用画面运动曲线造节奏引导轨，喂给 ACE-Step 做音画同步。

依据（2026-11-03 实测）：`reference_audio_path` 是**节奏锚**——
同 prompt 同 seed，喂 160BPM 参考后输出 156.2BPM（差 3.8），
不喂则 197.4BPM（差 37.4）；而音色标记 4.4kHz 几乎没被带过去（0%→0.001%）。
⇒ 可以按画面事件排一条 click 轨来驱动配乐节奏，而不污染音色。

两个候选并行对比：
  A 纯 prompt（steps=20，不看画面）
  B prompt + 画面节奏引导轨（strength 可调）
客观指标：削波 / 响度 / 频谱平衡 / 静音缺口 / 与画面运动的相关性。
"""
import json
import math
import os
import subprocess
import sys
import time
import urllib.request
import wave

import numpy as np

BASE = "http://127.0.0.1:8104"
VIDEO = "exports/台式机结构解构-长停留-中文标注.mp4"
ACE_HOME = r"D:\AI\ACE-Step-1.5"
GUIDE_LOCAL = os.path.join(ACE_HOME, "_guide-pc.wav")
GUIDE_PARAM = "_guide-pc.wav"
OUT = "运行/data/generated"

PROMPT = ("minimal cinematic tech underscore for a premium product teardown film, "
          "warm analog synth pad, sparse metallic plucks, deep sub-bass foundation, "
          "airy high shimmer on top, continuous sustained bed from start to finish, "
          "no drums, no vocals, restrained and precise, "
          "slow build, steady flowing middle, calm resolve at the end")
SEED = 20261103
STEPS = 24          # 质量档：8 是日常，24 在 turbo 上算超配
GEN_DUR = 16.0      # 生成比成片长，再从里面挑「连续有声」的窗口（官方产品广告 Skill 的做法）

urllib.request.install_opener(
    urllib.request.build_opener(urllib.request.ProxyHandler({})))


# ---------- 画面运动曲线 ----------
def motion_curve(path: str, sw: int = 184, sh: int = 104) -> tuple[np.ndarray, float]:
    raw = subprocess.run(["ffmpeg", "-v", "error", "-i", path,
                          "-vf", f"scale={sw}:{sh},format=gray",
                          "-vsync", "0", "-f", "rawvideo", "-pix_fmt", "gray", "-"],
                         capture_output=True, check=True).stdout
    n = len(raw) // (sw * sh)
    seq = np.frombuffer(raw[:n * sw * sh], dtype=np.uint8).reshape(n, sh, sw).astype(np.float32)
    d = np.abs(np.diff(seq, axis=0)).mean(axis=(1, 2))     # 帧间平均差 = 运动量
    dur = n / 24.0
    return d, dur


# ---------- 节奏引导轨 ----------
def make_guide(path: str, motion: np.ndarray, dur: float, sr: int = 48000) -> dict:
    """按运动量决定 click 密度：动得厉害的地方密，静止处疏但不空。"""
    n = int(dur * sr)
    sig = np.zeros(n, dtype=np.float64)
    step = 0.05                                  # 50ms 一格
    grid = np.arange(0, dur, step)
    # 把运动量映射到该格的 click 概率（0.12–1.0），静止段也留弱脉冲保持流动
    norm = motion / (np.percentile(motion, 95) + 1e-9)
    hits = 0
    rng = np.random.default_rng(7)
    for t in grid:
        fi = min(int(t * 24), len(norm) - 1)
        m = float(np.clip(norm[max(0, fi - 2):fi + 3].max(), 0, 1))
        prob = 0.12 + 0.88 * m
        if rng.random() > prob:
            continue
        hits += 1
        i0 = int(t * sr)
        cl = int(0.030 * sr)
        if i0 + cl >= n:
            break
        tt = np.arange(cl) / sr
        env = np.exp(-tt / (0.006))
        # 低频 thump（80Hz）+ 高频 tick（2.4kHz）：给 ACE 清晰的 onset，且不带调性
        sig[i0:i0 + cl] += env * (0.45 * np.sin(2 * math.pi * 80 * tt)
                                  + 0.18 * np.sin(2 * math.pi * 2400 * tt))
    pcm = np.clip(sig, -1, 1)
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes((pcm * 32767).astype(np.int16).tobytes())
    print(f"  guide: {hits} 击 / {dur:.2f}s（{hits/dur:.1f} 击每秒）")
    return {"hits": hits, "per_sec": round(hits / dur, 2)}


# ---------- 指标 ----------
def load_mono(path: str, sr: int = 16000) -> np.ndarray:
    p = subprocess.run(["ffmpeg", "-v", "error", "-i", path, "-map", "0:a:0",
                        "-f", "s16le", "-ac", "1", "-ar", str(sr), "-"], capture_output=True)
    return np.frombuffer(p.stdout, dtype=np.int16).astype(np.float64) / 32768.0


def metrics(path: str) -> dict:
    out = {}
    v = subprocess.run(["ffmpeg", "-hide_banner", "-i", path, "-af", "volumedetect",
                        "-f", "null", "-"], capture_output=True, text=True).stderr
    for ln in v.splitlines():
        if "mean_volume:" in ln:
            out["mean_db"] = float(ln.split("mean_volume:")[1].split("dB")[0])
        if "max_volume:" in ln:
            out["max_db"] = float(ln.split("max_volume:")[1].split("dB")[0])
    x = load_mono(path)
    sr = 16000
    if x.size < sr:
        return out
    spec = np.abs(np.fft.rfft(x * np.hanning(x.size))) ** 2
    fr = np.fft.rfftfreq(x.size, 1 / sr)
    tot = spec.sum() + 1e-12
    out["low_pct"] = round(float(spec[(fr >= 20) & (fr < 120)].sum() / tot * 100), 2)
    out["mid_pct"] = round(float(spec[(fr >= 200) & (fr < 2000)].sum() / tot * 100), 2)
    out["high_pct"] = round(float(spec[fr >= 8000].sum() / tot * 100), 2)
    # 能量包络 与 1 秒内的静音缺口
    hop = sr // 10
    n = x.size // hop
    env = np.array([np.sqrt(np.mean(x[i * hop:(i + 1) * hop] ** 2)) for i in range(n)])
    thr = env.max() * 0.02
    gaps, run = 0, 0
    for e in env:
        run = run + 1 if e < thr else 0
        gaps = max(gaps, run)
    out["max_silence_s"] = round(gaps / 10.0, 2)
    out["env"] = env
    return out


def find_best_window(env: np.ndarray, target_s: float, hop_s: float = 0.1) -> tuple[float, float]:
    """在生成的长曲里找一段「持续有声、能量稳」的窗口来当成片配乐。

    为什么不能直接用全曲：实测三条候选的静音都在**尾部**（1.8–2.4s），
    直接用会让成片最后两秒画面还在、音乐已死。
    官方产品广告 Skill 也是这个做法：从长曲里挑连续有声的片段，别盲目裁前 N 秒。
    评分 = 平均能量 ×（1 − 静音占比），并对起点加轻微惩罚让靠前窗口优先（保留开场淡入感）。
    """
    n = int(target_s / hop_s)
    if len(env) <= n:
        return 0.0, float(env.mean())
    peak = env.max() + 1e-9
    best, best_i = -1e9, 0
    for i in range(len(env) - n + 1):
        w = env[i:i + n]
        quiet = float((w < peak * 0.02).mean())
        score = w.mean() * (1.0 - quiet) - 0.0015 * i     # 起点越靠后越扣分
        if score > best:
            best, best_i = score, i
    return best_i * hop_s, best


def trim_audio(src: str, start: float, dur: float, dst: str) -> None:
    subprocess.run(["ffmpeg", "-y", "-v", "error", "-ss", f"{start:.3f}",
                    "-t", f"{dur:.3f}", "-i", src, "-c:a", "pcm_s16le", dst],
                   check=True)


def silence_profile(env: np.ndarray, dur: float) -> dict:
    """静音缺口出现在哪一段——**位置决定好坏**：
    结尾的自然收束配合画面淡出是好事；中段空洞才是缺陷。"""
    thr = env.max() * 0.02
    quiet = env < thr
    n = len(env)
    out = {"head": 0.0, "mid": 0.0, "tail": 0.0}
    run = longest = 0
    longest_at = 0
    for i, q in enumerate(quiet):
        if q:
            run += 1
            if run > longest:
                longest, longest_at = run, i
        else:
            run = 0
    out["longest"] = round(longest / 10.0, 2)
    out["longest_at"] = round(longest_at / 10.0, 2)
    if longest:
        pos = longest_at / 10.0
        if pos < dur * 0.2:
            out["head"] = out["longest"]
        elif pos > dur * 0.8:
            out["tail"] = out["longest"]
        else:
            out["mid"] = out["longest"]
    return out


def motion_energy_split(env: np.ndarray, motion: np.ndarray, dur: float) -> dict:
    """运动段 vs 静止段的音乐平均能量（dB 差）。

    为什么不整条求相关：本片 57% 画面是静止的，而配乐**不该**跟着静止，
    整条求相关必然得负值，会把「好配乐」判成「不配」。分段对比才是正确问法。
    """
    m = np.interp(np.linspace(0, dur, len(env)), np.linspace(0, dur, len(motion)), motion)
    thr = np.percentile(motion, 60)
    hi, lo = env[m >= thr], env[m < thr]
    if hi.size == 0 or lo.size == 0:
        return {"ratio_db": 0.0, "note": "样本不足"}
    r = 20 * math.log10((hi.mean() + 1e-9) / (lo.mean() + 1e-9))
    return {"ratio_db": round(float(r), 2),
            "note": "正值=运动段更响（有跟随）；0 附近=平稳铺底"}


# ---------- 调用 ----------
def post(path: str, body: dict, timeout: int = 1800) -> dict:
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def gen(tag: str, dur: float, extra: dict) -> dict:
    body = {
        "prompt": PROMPT, "lyrics": "", "audio_duration": dur, "audio_format": "wav",
        "batch_size": 1, "inference_steps": STEPS, "model": "acestep-v15-xl-turbo",
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
    out = os.path.abspath(f"{OUT}/music-{tag}.wav")
    with urllib.request.urlopen(BASE + url, timeout=600) as r, open(out, "wb") as f:
        f.write(r.read())
    return {"tag": tag, "sec": round(el, 1), "file": out}


def main() -> int:
    print("▶ 画面运动曲线")
    motion, vdur = motion_curve(VIDEO)
    print(f"  {len(motion)} 帧 / {vdur:.2f}s  运动峰值 {motion.max():.2f}  "
          f"静止段比例 {float((motion < 1.2).mean())*100:.0f}%")

    print("\n▶ 节奏引导轨")
    g = make_guide(GUIDE_LOCAL, motion, vdur)

    jobs = [
        ("A-pure", {}),
        ("B-guide06", {"reference_audio_path": GUIDE_PARAM, "audio_cover_strength": 0.6}),
        ("C-guide035", {"reference_audio_path": GUIDE_PARAM, "audio_cover_strength": 0.35}),
    ]
    cands = []
    for tag, extra in jobs:
        print(f"\n▶ 候选 {tag} (steps={STEPS}, {GEN_DUR:.0f}s 长曲, extra={extra or '无'})")
        r = gen(tag, GEN_DUR, extra)
        print(f"  生成 {r['sec']}s")
        # 从长曲里挑连续有声窗口 → 裁成成片长度
        m = metrics(r["file"])
        start, score = find_best_window(m.pop("env"), vdur)
        trimmed = r["file"].replace(".wav", "-trim.wav")
        trim_audio(r["file"], start, vdur, trimmed)
        r["trim"] = trimmed
        r["window_start"] = round(start, 2)
        r["window_score"] = round(score, 5)
        print(f"  最佳窗口 {start:.2f}s 起（评分 {score:.4f}）→ 裁出 {vdur:.2f}s")
        cands.append(r)

    rows = []
    for r in cands:
        m = metrics(r["trim"])
        env = m.pop("env")
        sil = silence_profile(env, vdur)
        split = motion_energy_split(env, motion, vdur)
        rows.append({**r, **m, "silence": sil, "split": split})

    print("\n=== 客观指标对比（均在裁剪后的成片长度上测）===")
    print(f"{'候选':12s} {'起点':>5s} {'mean':>6s} {'max':>6s} {'低%':>5s} {'中%':>6s} {'高%':>5s} "
          f"{'最长静音':>8s} {'位置':>4s} {'动静差dB':>8s}")
    for r in rows:
        s = r["silence"]
        pos = "头" if s["head"] else ("尾" if s["tail"] else ("中" if s["mid"] else "—"))
        print(f"{r['tag']:12s} {r['window_start']:5.2f} {r['mean_db']:6.1f} {r['max_db']:6.1f} "
              f"{r['low_pct']:5.2f} {r['mid_pct']:6.2f} {r['high_pct']:5.2f} "
              f"{s['longest']:8.2f} {pos:>4s} {r['split']['ratio_db']:8.2f}")

    json.dump({"motion_peak": float(motion.max()), "guide": g,
               "candidates": [{k: v for k, v in r.items() if k != "file"} for r in rows]},
              open(".tmp/hq-music.json", "w"), ensure_ascii=False, indent=1)
    print("\n结果 .tmp/hq-music.json")
    for r in rows:
        print(f"  {r['tag']}: {r['file']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
