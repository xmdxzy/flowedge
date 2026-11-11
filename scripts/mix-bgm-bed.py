#!/usr/bin/env python3
"""把一首 BGM 混进已有口播成片：固定垫乐电平 + 包络闪避（ducking），
再两遍 loudnorm 归一，视频流 -c:v copy 不重编码。

为什么不用 ffmpeg 的 sidechaincompress：它的压缩量随口播瞬时电平漂
（同一句里音量高低不同，垫乐忽大忽小），实测 3:1、threshold 0.02 时
理论 12.7dB、实测只有 3.8dB。这里自己算包络，闪避深度恒定。

用法：
  python scripts/mix-bgm-bed.py --video in.mp4 --bgm track.wav --out out.mp4 \
      [--voice voice.wav] [--gain -10] [--duck 7] [--offset 0] \
      [--fade-in 0.8] [--fade-out 3] [--target-lufs -16] [--tp -1.5]

判据（脚本自己复核，失败非零退出）：
  - 视频帧数与源一致；音轨时长 ≈ 视频流时长
  - 综合响度落在目标 ±0.5 LU，真峰值 ≤ TP+0.3
  - 垫乐在「口播段」与「停顿段」的实测电平打印出来，供人工判断
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys

import numpy as np

SR = 48000
LIMITER_ATTACK_MS = 5.0


def run(cmd: list[str], **kw) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, text=True, **kw)


def ffprobe(path: str) -> dict:
    out = run(["ffprobe", "-v", "error", "-print_format", "json",
               "-show_streams", "-show_format", path])
    if out.returncode != 0:
        raise SystemExit(f"ffprobe 失败：{out.stderr[:400]}")
    return json.loads(out.stdout)


def video_duration(path: str) -> float:
    info = ffprobe(path)
    for s in info["streams"]:
        if s["codec_type"] == "video":
            if s.get("duration"):
                return float(s["duration"])
            num, den = s["avg_frame_rate"].split("/")
            return float(s["nb_frames"]) * float(den) / float(num)
    raise SystemExit(f"没有视频流：{path}")


def decode(path: str, channels: int) -> np.ndarray:
    raw = subprocess.run(["ffmpeg", "-v", "error", "-i", path, "-f", "f32le",
                          "-ac", str(channels), "-ar", str(SR), "-"],
                         capture_output=True)
    if raw.returncode != 0:
        raise SystemExit(f"解码失败：{path}\n{raw.stderr[:400].decode('utf-8', 'replace')}")
    x = np.frombuffer(raw.stdout, dtype="<f4").astype(np.float64)
    if channels > 1:
        x = x.reshape(-1, channels)
    return x


def lufs(path: str) -> tuple[float | None, float | None]:
    out = run(["ffmpeg", "-hide_banner", "-nostats", "-i", path,
               "-af", "ebur128=peak=true", "-f", "null", "-"]).stderr
    i = re.findall(r"^\s*I:\s*(-?[\d.]+)", out, re.M)
    tp = re.findall(r"^\s*Peak:\s*(-?[\d.]+)", out, re.M)
    return (float(i[-1]) if i else None, float(tp[-1]) if tp else None)


def db(x: np.ndarray) -> float:
    return 20 * np.log10(np.sqrt(np.mean(x ** 2)) + 1e-12)


def envelope_db(voice: np.ndarray, hop: int) -> np.ndarray:
    n = len(voice) // hop
    frames = voice[: n * hop].reshape(n, hop)
    rms = np.sqrt(np.mean(frames ** 2, axis=1) + 1e-12)
    return 20 * np.log10(rms)


def duck_curve(voice: np.ndarray, depth_db: float, gate_db: float = -42.0,
               range_db: float = 18.0, attack_ms: float = 20.0,
               release_ms: float = 280.0) -> np.ndarray:
    """逐样本闪避增益（dB，0 到 -depth）。口播起音快、消失慢，避免抽气感。"""
    hop = int(SR * 0.01)
    env = envelope_db(voice, hop)
    # 线性映射：静音 -> 0，口播满幅 -> depth
    norm = np.clip((env - gate_db) / range_db, 0.0, 1.0)
    target = -depth_db * norm  # 负值
    # 非对称一阶平滑（attack 快，release 慢）
    out = np.empty_like(target)
    a_att = np.exp(-1.0 / (attack_ms / 10.0))
    a_rel = np.exp(-1.0 / (release_ms / 10.0))
    g = 0.0
    for i, t in enumerate(target):
        a = a_att if t < g else a_rel
        g = a * g + (1 - a) * t
        out[i] = g
    # 上采样到样本级（线性插值）
    idx = np.arange(len(voice)) / hop
    i0 = np.clip(idx.astype(int), 0, len(out) - 1)
    i1 = np.clip(i0 + 1, 0, len(out) - 1)
    frac = idx - i0
    return out[i0] * (1 - frac) + out[i1] * frac


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--video", required=True)
    ap.add_argument("--bgm", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--voice", help="口播干声；缺省从视频里抽音轨")
    ap.add_argument("--gain", type=float, default=-10.0, help="垫乐电平（dB，相对原始 BGM）")
    ap.add_argument("--duck", type=float, default=7.0, help="闪避深度 dB；0 = 不闪避")
    ap.add_argument("--offset", type=float, default=0.0)
    ap.add_argument("--fade-in", type=float, default=0.8)
    ap.add_argument("--fade-out", type=float, default=3.0)
    ap.add_argument("--target-lufs", type=float, default=-16.0)
    ap.add_argument("--tp", type=float, default=-1.5)
    a = ap.parse_args()

    dur = video_duration(a.video)
    if dur <= 0:
        raise SystemExit("视频流时长 <= 0")

    if a.voice:
        voice = decode(a.voice, 1)
    else:
        voice = decode(a.video, 1)
    if len(voice) < int(dur * SR):
        voice = np.pad(voice, (0, int(dur * SR) - len(voice)))
    voice = voice[: int(dur * SR)]

    music = decode(a.bgm, 2)
    need = int(dur * SR)
    start = int(a.offset * SR)
    if len(music) - start < need:  # 不够长就循环
        music = np.tile(music, (int(np.ceil(need / len(music))) + 1, 1))
        start = start % len(music)
    music = music[start:start + need]

    n = min(len(voice), len(music))
    voice, music = voice[:n], music[:n]

    fade_in = min(a.fade_in, dur / 3)
    fade_out = min(a.fade_out, dur / 3)
    t = np.arange(n) / SR
    env = np.ones(n)
    fi = int(fade_in * SR)
    if fi > 0:
        env[:fi] = 0.5 - 0.5 * np.cos(np.pi * np.arange(fi) / fi)
    fo = int(fade_out * SR)
    if fo > 0:
        env[-fo:] *= 0.5 + 0.5 * np.cos(np.pi * np.arange(fo) / fo)
    music *= env[:, None]

    duck_db = duck_curve(voice, a.duck) if a.duck > 0 else np.zeros(n)
    music *= (10 ** ((a.gain + duck_db) / 20.0))[:, None]

    mix = np.repeat(voice[:, None], 2, axis=1) + music
    peak = float(np.max(np.abs(mix)))
    if peak > 0.99:
        mix *= 0.99 / peak

    bed = mix - np.repeat(voice[:, None], 2, axis=1)

    raw = a.out + ".mix.f32"
    mix.astype("<f4").tofile(raw)
    wav = a.out + ".mix.wav"
    r = run(["ffmpeg", "-v", "error", "-y", "-f", "f32le", "-ar", str(SR),
             "-ac", "2", "-i", raw, wav])
    if r.returncode != 0:
        raise SystemExit(f"写 mix wav 失败：{r.stderr[:400]}")

    # 两遍 loudnorm
    probe = run(["ffmpeg", "-hide_banner", "-nostats", "-i", wav,
                 "-af", f"loudnorm=I={a.target_lufs}:TP={a.tp}:LRA=11:print_format=json",
                 "-f", "null", "-"]).stderr
    m = re.search(r"\{[^{}]+\}", probe, re.S)
    if not m:
        raise SystemExit(f"loudnorm 测量失败：{probe[-500:]}")
    st = json.loads(m.group(0))
    norm = (
        f"loudnorm=I={a.target_lufs}:TP={a.tp}:LRA=11:"
        f"measured_I={st['input_i']}:measured_TP={st['input_tp']}:"
        f"measured_LRA={st['input_lra']}:measured_thresh={st['input_thresh']}:"
        f"offset={st['target_offset']}:linear=true,"
        f"alimiter=limit={10 ** (a.tp / 20):.4f}:level=false:attack={LIMITER_ATTACK_MS}"
    )
    norm_wav = a.out + ".norm.wav"
    r = run(["ffmpeg", "-v", "error", "-y", "-i", wav, "-af", norm, norm_wav])
    if r.returncode != 0:
        raise SystemExit(f"loudnorm 失败：{r.stderr[:400]}")

    # alimiter 打开 lookahead 后会把整条音轨推迟一个 attack（默认 5ms），
    # 合流前按同一常数裁掉头部，否则口播会比画面晚 5ms（实测 lag 240 samples）。
    head = LIMITER_ATTACK_MS / 1000.0
    # 合流（视频流 copy，音频 AAC）
    r = run(["ffmpeg", "-v", "error", "-y", "-i", a.video, "-i", norm_wav,
             "-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy",
             "-af", f"atrim=start={head:.3f},asetpts=PTS-STARTPTS",
             "-c:a", "aac", "-b:a", "192k", "-ar", "48000",
             "-t", f"{dur:.3f}", "-movflags", "+faststart", a.out])
    if r.returncode != 0:
        raise SystemExit(f"合流失败：{r.stderr[:400]}")

    for f in (raw, wav, norm_wav):
        try:
            import os
            os.remove(f)
        except OSError:
            pass

    # ---- 自检 ----
    src, dst = ffprobe(a.video), ffprobe(a.out)
    sv = next(s for s in src["streams"] if s["codec_type"] == "video")
    dv = next(s for s in dst["streams"] if s["codec_type"] == "video")
    da = next((s for s in dst["streams"] if s["codec_type"] == "audio"), None)
    if da is None:
        raise SystemExit("产物没有音轨")
    if sv.get("nb_frames") and dv.get("nb_frames") and sv["nb_frames"] != dv["nb_frames"]:
        raise SystemExit(f"帧数变了：{sv['nb_frames']} -> {dv['nb_frames']}")
    if abs(float(da["duration"]) - float(dv["duration"])) > 0.05:
        raise SystemExit(f"音画时长差过大：{da['duration']} vs {dv['duration']}")

    hop = int(0.02 * SR)
    k = n // hop
    vdb = envelope_db(voice[: k * hop], hop)
    vrms = np.array([np.sqrt(np.mean(voice[i * hop:(i + 1) * hop] ** 2) + 1e-12) for i in range(k)])
    brms = np.array([np.sqrt(np.mean(bed[i * hop:(i + 1) * hop] ** 2) + 1e-12) for i in range(k)])
    act = vdb > -35
    q = ~act
    final_l, final_tp = lufs(a.out)
    print(f"[ok] {a.out}")
    print(f"     视频 {float(dv['duration']):.3f}s frames={dv.get('nb_frames')} "
          f"音频 {float(da['duration']):.3f}s")
    print(f"     响度 {final_l} LUFS / TP {final_tp} dB (目标 {a.target_lufs}/{a.tp})")
    if act.any():
        v_act = 20 * np.log10(np.sqrt(np.mean(vrms[act] ** 2)) + 1e-12)
        b_act = 20 * np.log10(np.sqrt(np.mean(brms[act] ** 2)) + 1e-12)
    else:
        v_act = b_act = -99.0
    b_q = 20 * np.log10(np.sqrt(np.mean(brms[q] ** 2)) + 1e-12) if q.any() else -99.0
    print(f"     口播 RMS {db(voice):.1f} dB；垫乐 口播段 {b_act:.1f} dB"
          f"（差 {b_act - v_act:.1f} dB），停顿段 {b_q:.1f} dB")
    if final_l is None or abs(final_l - a.target_lufs) > 0.5:
        raise SystemExit(f"响度未命中：{final_l}")
    if final_tp is None or final_tp > a.tp + 0.3:
        raise SystemExit(f"真峰值超标：{final_tp}")


if __name__ == "__main__":
    main()
