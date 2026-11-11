# -*- coding: utf-8 -*-
"""歌曲「录音模式」收样分析器 —— 只输出客观测量，不做听感断言。

用法：
    python scripts/song-intake.py <音频文件> [--out <json路径>] [--bars-per-section 8]

产出：
    1) JSON 档案（BPM / 调性 / 频段 / 立体声 / 响度 / 小节和弦 / 段落切分）
    2) stdout 一段人类可读摘要

依赖：ffmpeg、numpy。不联网、不调用任何模型。
已知局限：和弦识别基于 24 个三和弦模板，七和弦/挂留会被判成最接近的三和弦；
段落边界来自响度/音色的分块变化，不是人工标注的曲式。
"""
import json
import subprocess
import sys
from pathlib import Path

import numpy as np

SR = 22050
N_FFT = 2048
HOP = 512
NAMES = ["C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"]
MAJ = np.array([6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88])
MIN = np.array([6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17])


def run(cmd, binary=False):
    return subprocess.run(cmd, capture_output=True, check=True).stdout


def probe(path):
    raw = run(["ffprobe", "-v", "error", "-select_streams", "a:0",
               "-show_entries", "stream=codec_name,sample_rate,channels,bits_per_raw_sample",
               "-show_entries", "format=duration,bit_rate", "-of", "json", path]).decode("utf-8")
    d = json.loads(raw)
    st = d["streams"][0]
    return {
        "duration_s": round(float(d["format"]["duration"]), 3),
        "sample_rate": int(st["sample_rate"]),
        "channels": int(st["channels"]),
        "codec": st["codec_name"],
        "bits_per_raw_sample": st.get("bits_per_raw_sample") or None,
    }


def loudness(path):
    """两遍 loudnorm 的测量遍，拿集成响度 / True Peak / LRA。"""
    err = subprocess.run(["ffmpeg", "-v", "info", "-i", path, "-af",
                          "loudnorm=I=-14:TP=-1.5:LRA=11:print_format=json",
                          "-f", "null", "-"], capture_output=True).stderr.decode("utf-8", "ignore")
    tail = err[err.rfind("{"):err.rfind("}") + 1]
    try:
        d = json.loads(tail)
        return {"integrated_lufs": float(d["input_i"]),
                "true_peak_dbtp": float(d["input_tp"]),
                "lra_lu": float(d["input_lra"])}
    except Exception:
        return {"integrated_lufs": None, "true_peak_dbtp": None, "lra_lu": None}


def decode(path, ch=1, sr=SR):
    raw = run(["ffmpeg", "-v", "error", "-i", path, "-ac", str(ch), "-ar", str(sr),
               "-f", "f32le", "-"])
    return np.frombuffer(raw, dtype=np.float32).reshape(-1, ch)


def stft_mag(x, n=N_FFT, hop=HOP):
    cnt = 1 + (len(x) - n) // hop
    idx = np.arange(n)[None, :] + hop * np.arange(cnt)[:, None]
    return np.abs(np.fft.rfft(x[idx] * np.hanning(n).astype(np.float32), axis=1))


def tempo(mag):
    lm = np.log1p(mag * 10)
    flux = np.maximum(0, np.diff(lm, axis=0)).sum(axis=1)
    e = flux - flux.mean()
    ac = np.correlate(e, e, "full")[len(e) - 1:]
    ac = ac / (ac[0] + 1e-12)
    fps = SR / HOP

    def ac_at(lag):
        lag = int(round(lag))
        return float(ac[lag]) if 0 < lag < len(ac) else 0.0

    # 候选：70–160 BPM 内的自相关局部峰
    lo, hi = int(fps * 60 / 160), int(fps * 60 / 70)
    seg = ac[lo:hi]
    peaks = [lo + i for i in range(1, len(seg) - 1)
             if seg[i] >= seg[i - 1] and seg[i] >= seg[i + 1] and seg[i] > 0.1]
    peaks.sort(key=lambda L: -ac[L])
    peaks = peaks[:8]

    # 谐波簇打分：真拍在 2× / 3× 上也有能量；3:2 别名（如 76 vs 112）靠这个排序
    def family_score(lag):
        return ac_at(lag) + 0.6 * ac_at(2 * lag) + 0.4 * ac_at(3 * lag) + 0.3 * ac_at(lag / 2)

    scored = sorted(((60 * fps / L, family_score(L), float(ac[L])) for L in peaks),
                    key=lambda t: -t[1])
    if not scored:
        return 120.0, 0.0, flux, []
    bpm = scored[0][0]
    conf = scored[0][2]
    alts = []
    for b, fs, a in scored[1:4]:
        ratio = b / bpm
        tag = "同一拍族" if 0.9 < ratio < 1.1 else (
            "3:2 别名" if 1.4 < ratio < 1.6 else ("1.5 倍/半速" if 1.9 < ratio < 2.1 else ""))
        alts.append({"bpm": round(b, 1), "family_score": round(fs, 3), "ac": round(a, 3), "note": tag})
    return bpm, conf, flux, alts


def chroma_of(mag, freqs):
    """FFT bin → 音级（A=0 基准 +9 偏移回 C=0），功率加权。"""
    sel = (freqs >= 55) & (freqs <= 4000)
    p = (mag[:, sel] ** 2)
    f = freqs[sel]
    pc = np.mod(np.round(12 * np.log2(f / 440.0)).astype(int) + 9, 12)
    ch = np.zeros((mag.shape[0], 12))
    for k in range(12):
        ch[:, k] = p[:, pc == k].sum(axis=1)
    return ch


def key_of(chroma, idx=None):
    c = chroma if idx is None else chroma[idx]
    v = c.sum(axis=0)
    v = v / (v.sum() + 1e-12)
    best = (-9, "")
    for k in range(12):
        for t, mode in ((MAJ, "major"), (MIN, "minor")):
            r = float(np.corrcoef(v, np.roll(t, k))[0, 1])
            if r > best[0]:
                best = (r, f"{NAMES[k]} {mode}")
    return {"name": best[1], "corr": round(best[0], 3)}


def chords_of(chroma, idx):
    """小节内平均 chroma → 24 三和弦模板最优匹配。"""
    idx = np.asarray(idx)
    idx = idx[idx < len(chroma)]
    if idx.size == 0:
        return "N", 0.0
    v = chroma[idx].mean(axis=0)
    v = v / (v.sum() + 1e-12)
    best = (-9, "N")
    for root in range(12):
        for third, label in ((4, ""), (3, "m")):
            tpl = np.zeros(12)
            tpl[root] = 1.0
            tpl[(root + third) % 12] = 0.8
            tpl[(root + 7) % 12] = 0.6
            tpl /= tpl.sum()
            r = float(np.dot(v, tpl))
            if r > best[0]:
                best = (r, NAMES[root] + label)
    return best[1], round(best[0], 3)


def band_percent(mag, freqs):
    power = (mag ** 2).sum(axis=0)
    lo = freqs
    bands = [(0, 60), (60, 250), (250, 2000), (2000, 6000), (6000, freqs[-1] + 1)]
    total = power.sum() + 1e-12
    out = {}
    for a, b in bands:
        sel = (lo >= a) & (lo < b)
        label = f"{a}-{b}" if b < 1000 else f"{a // 1000}k-{b // 1000}k"
        out[label] = round(100 * float(power[sel].sum()) / total, 1)
    return out


def analyze(path):
    p = str(Path(path))
    x = decode(p, 1)[:, 0]
    st = decode(p, 2)
    dur = len(x) / SR
    mag = stft_mag(x)
    freqs = np.fft.rfftfreq(N_FFT, 1 / SR)
    fps = SR / HOP

    bpm, conf, flux, alts = tempo(mag)
    beat = 60 / bpm
    # chroma 用 8192 窗：2048 窗的频率分辨率（约 10.8Hz）分不开低音区的半音
    magc = stft_mag(x, 8192, HOP)
    freqs_c = np.fft.rfftfreq(8192, 1 / SR)
    chroma = chroma_of(magc, freqs_c)
    key = key_of(chroma)

    # ---- 拍点相位：把 onset 折叠到一拍内找峰 ----
    hop_t = HOP / SR
    n_beats = int(dur / beat)
    phase_scores = np.zeros(20)
    for k in range(20):
        off = k * beat / 20
        idx = ((np.arange(n_beats) * beat + off) / hop_t).astype(int)
        idx = idx[(idx >= 0) & (idx < len(flux))]
        phase_scores[k] = flux[idx].sum()
    phase = float(np.argmax(phase_scores) * beat / 20)

    # ---- 小节网格 ----
    bar = beat * 4
    bar_starts = np.arange(phase, dur, bar)
    bar_idx = []
    for b0 in bar_starts:
        i0, i1 = int(b0 / hop_t), int(min(b0 + bar, dur) / hop_t)
        if i1 - i0 >= 2:
            bar_idx.append((i0, i1))

    # ---- 立体声 ----
    L, R = st[:, 0], st[:, 1]
    n = min(len(L), len(R))
    corr = float(np.corrcoef(L[:n], R[:n])[0, 1])
    mid = (L[:n] + R[:n]) / 2
    side = (L[:n] - R[:n]) / 2
    sm = 20 * np.log10((np.sqrt((side ** 2).mean()) + 1e-9) / (np.sqrt((mid ** 2).mean()) + 1e-9))

    # ---- 段落：每 2 小节一块，响度跃迁 > 3.5dB 或和弦根音变 → 切 ----
    blocks = []
    step = 2
    for i in range(0, len(bar_idx), step):
        chunk = bar_idx[i:i + step]
        i0, i1 = chunk[0][0], chunk[-1][1]
        seg = x[i0 * HOP:i1 * HOP]
        rms = 20 * np.log10(np.sqrt((seg ** 2).mean()) + 1e-9) if len(seg) else -99
        ch, _ = chords_of(chroma, np.arange(i0, i1))
        blocks.append({"t": round(i * bar + phase, 1), "rms": round(float(rms), 1), "chord": ch})
    sections = []
    cur = None
    for b in blocks:
        jump = cur is not None and abs(b["rms"] - cur["rms_last"]) > 3.5
        if cur is None or jump:
            if cur:
                sections.append(cur)
            cur = {"start_s": b["t"], "end_s": None, "rms_last": b["rms"],
                   "rms_db": [b["rms"]], "chords": [b["chord"]]}
        else:
            cur["rms_db"].append(b["rms"])
            cur["chords"].append(b["chord"])
            cur["rms_last"] = b["rms"]
    if cur:
        sections.append(cur)
    for s in sections:
        s["end_s"] = None
        s["mean_rms_db"] = round(float(np.mean(s.pop("rms_db"))), 1)
        chords = s.pop("chords")
        s.pop("rms_last")
        # 相邻重复和弦压掉
        seq = [c for i, c in enumerate(chords) if i == 0 or c != chords[i - 1]]
        s["chords"] = seq
    for a, b in zip(sections, sections[1:]):
        a["end_s"] = b["start_s"]
    if sections:
        sections[-1]["end_s"] = round(dur, 1)

    return {
        "file": p,
        "probe": probe(p),
        "loudness": loudness(p),
        "tempo": {"bpm": round(bpm, 1), "confidence": round(conf, 2),
                  "beat_s": round(beat, 3), "phase_s": round(phase, 3), "beats": n_beats,
                  "alternatives": alts},
        "key": key,
        "bands_percent": band_percent(mag, freqs),
        "stereo": {"lr_corr": round(corr, 3), "side_minus_mid_db": round(float(sm), 1)},
        "sections": sections,
        "bar_chords": [chords_of(chroma, np.arange(i0, i1))[0] for i0, i1 in bar_idx],
        "notes": ["和弦=三和弦模板最优匹配", "段落边界来自响度跃迁，非人工曲式标注",
                  "未做人声检测（本机无 ASR 服务）"],
    }


def main():
    args = [a for a in sys.argv[1:]]
    if not args:
        print("usage: python scripts/song-intake.py <audio> [--out <json>]")
        sys.exit(2)
    path = args[0]
    out = None
    if "--out" in args:
        out = args[args.index("--out") + 1]
    prof = analyze(path)
    text = json.dumps(prof, ensure_ascii=False, indent=2, default=float)
    if out:
        Path(out).parent.mkdir(parents=True, exist_ok=True)
        Path(out).write_text(text, encoding="utf-8")
    pr = prof["probe"]
    print(f"文件      {Path(path).name}")
    print(f"时长/格式 {pr['duration_s']}s  {pr['sample_rate']}Hz  {pr['channels']}ch  {pr['codec']}")
    print(f"速度      {prof['tempo']['bpm']} BPM (置信 {prof['tempo']['confidence']})  首拍 {prof['tempo']['phase_s']}s")
    print(f"调性      {prof['key']['name']} (相关 {prof['key']['corr']})")
    print(f"频段%     {' / '.join(f'{k}:{v}' for k, v in prof['bands_percent'].items())}")
    print(f"立体声    L/R 相关 {prof['stereo']['lr_corr']}  side-mid {prof['stereo']['side_minus_mid_db']} dB")
    l = prof["loudness"]
    print(f"响度      {l['integrated_lufs']} LUFS  TP {l['true_peak_dbtp']} dBTP  LRA {l['lra_lu']}")
    for i, s in enumerate(prof["sections"], 1):
        print(f"段{i}  {s['start_s']:>6.1f}-{s['end_s']:>6.1f}s  {s['mean_rms_db']:>6.1f} dB  "
              f"{' '.join(s['chords'][:8])}")
    if out:
        print(f"JSON -> {out}")


if __name__ == "__main__":
    main()
