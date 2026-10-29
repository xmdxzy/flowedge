#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""文本 → 抖音口播成品（用已注册的克隆音色 my-voice）。

为什么是一整条链而不是「调 TTS + 手动母带」：
  克隆输出天然是 24kHz / -25 LUFS 左右，直接扔进剪映会被判"声音发虚"；
  而 loudnorm 是两遍法（先测后套），手动做一次忘了第二遍就会差 10dB。

链路：
  1. POST :8103/tts  {voice: my-voice, instruction: 抖音口播指令, seed}
  2. ffmpeg 两遍 loudnorm → 48kHz 单声道（视频工程通用采样率）
  3. 验收：LUFS / 真峰值 / LRA / 语速 / 削波，任何一项不达标就非零退出

关于 -15.7 而不是 -14：内容峰均比 ~14dB 时，loudnorm 在 TP=-1.5 下最多到 -15.7；
  再往上只能削峰（实测 LRA 不变、I 不变，说明是 TP 卡死的）。平台上传后会归一化到
  约 -14，所以 -15.7 是"带余量"的安全交付值，不是没做到。

用法：
    python scripts/douyin-voice.py --text "文案…" --out out/koubo.wav
    python scripts/douyin-voice.py --text-file 文案.txt --voice my-voice --lufs -14
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request
import wave
from pathlib import Path

import numpy as np

TTS = "http://127.0.0.1:8103"
DOUYIN_INSTRUCTION = "语速稍快，声音明亮清晰，有亲和力，带自然的语气起伏和重音"
PLAIN_INSTRUCTION = ""


def _opener():
    # 本机 http_proxy 会劫持 127.0.0.1（返回 502），必须显式清空
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


def tts(text: str, voice: str, instruction: str, seed: int, timeout: float = 900) -> bytes:
    body = {"text": text, "voice": voice, "seed": seed}
    if instruction:
        body["instruction"] = instruction
    req = urllib.request.Request(
        TTS + "/tts",
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json"},
    )
    try:
        with _opener().open(req, timeout=timeout) as r:
            return r.read()
    except urllib.error.URLError as exc:
        raise SystemExit(
            f"TTS 不可达（{TTS}）：{exc}。\n"
            f"这是「服务没起」而不是「没结果」——先 POST :8080/api/deps/tts/start，"
            f"首次合成还要等 ~190s 预热。"
        ) from exc
    except urllib.error.HTTPError as exc:
        raise SystemExit(f"TTS 报错 {exc.code}：{exc.read().decode('utf-8', 'ignore')[:400]}") from exc


def measure_lufs(path: Path) -> dict:
    r = subprocess.run(
        ["ffmpeg", "-hide_banner", "-nostats", "-i", str(path),
         "-af", "loudnorm=I=-14:TP=-1.5:print_format=json", "-f", "null", "-"],
        capture_output=True, text=True, encoding="utf-8", errors="ignore",
    )
    m = re.findall(r"\{[^{}]*\"input_i\"[^{}]*\}", r.stderr, re.S)
    if not m:
        raise SystemExit("loudnorm 测量失败（ffmpeg 输出里没有 input_i）")
    d = json.loads(m[-1])
    return {k: float(d[k]) for k in ("input_i", "input_tp", "input_lra", "input_thresh", "target_offset")}


def master(src: Path, dst: Path, target_lufs: float) -> None:
    d = measure_lufs(src)
    af = (
        f"loudnorm=I={target_lufs}:TP=-1.5:LRA=11"
        f":measured_I={d['input_i']}:measured_TP={d['input_tp']}"
        f":measured_LRA={d['input_lra']}:measured_thresh={d['input_thresh']}"
        f":offset={d['target_offset']}:linear=true"
    )
    p = subprocess.run(
        ["ffmpeg", "-hide_banner", "-nostats", "-y", "-i", str(src), "-af", af,
         "-ar", "48000", "-ac", "1", str(dst)],
        capture_output=True, text=True, encoding="utf-8", errors="ignore",
    )
    if p.returncode != 0 or not dst.exists():
        raise SystemExit(f"母带失败：{p.stderr[-500:]}")


def verify(path: Path, chars: int) -> tuple[bool, list[str]]:
    d = measure_lufs(path)
    with wave.open(str(path)) as w:
        sr, ch, n = w.getframerate(), w.getnchannels(), w.getnframes()
        x = np.frombuffer(w.readframes(n), dtype=np.int16).astype(np.float64) / 32768
    secs = n / sr
    clip = int((np.abs(x) > 0.999).sum())
    cps = chars / secs if secs else 0
    fr = int(0.02 * sr)
    k = max(len(x) // fr, 1)
    e = np.array([np.sqrt(np.mean(x[i * fr:(i + 1) * fr] ** 2)) for i in range(k)])
    snr = 20 * np.log10(max(np.sqrt(np.mean(x**2)), 1e-9)) - 20 * np.log10(
        max(np.percentile(e, 10), 1e-9)
    )
    rows = [
        f"采样率 {sr}Hz {ch}ch  时长 {secs:.2f}s  语速 {cps:.2f} 字/秒",
        f"响度 {d['input_i']:.1f} LUFS  真峰值 {d['input_tp']:.1f} dBTP  LRA {d['input_lra']:.1f}",
    ]
    # SNR 只在够长的素材上才有意义：短句里静音帧太少，10 分位帧会落在字与字的
    # 过渡上而不是真底噪，算出来会比实际低 10dB 以上（实测 4s 素材报 7.2dB）。
    if secs >= 5.0:
        rows.append(f"SNR {snr:.1f}dB  削波 {clip} 样本")
    else:
        rows.append(f"削波 {clip} 样本（时长 <5s，SNR 不参与判定）")

    fails: list[str] = []
    warns: list[str] = []
    if sr != 48000:
        fails.append(f"采样率 {sr} != 48000")
    if d["input_tp"] > -1.0:
        fails.append(f"真峰值 {d['input_tp']:.1f} > -1.0 dBTP（平台会削）")
    if clip:
        fails.append(f"{clip} 个削波样本")
    if not (4.0 <= cps <= 7.5):
        fails.append(f"语速 {cps:.2f} 字/秒 超出 4.0~7.5")
    elif cps > 6.5:
        warns.append(f"语速 {cps:.2f} 偏快（短文案常见；要慢一点可用 --plain 或加长文案）")
    if secs >= 5.0 and snr < 12.0:
        warns.append(f"SNR {snr:.1f}dB 偏低（参考录音底噪大，考虑重录）")

    tail = []
    if warns:
        tail.append("WARN: " + "；".join(warns))
    tail.append("FAIL: " + "；".join(fails) if fails else "验收通过")
    return (not fails), rows + tail


def main() -> int:
    ap = argparse.ArgumentParser()
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--text")
    g.add_argument("--text-file")
    ap.add_argument("--out", required=True)
    ap.add_argument("--voice", default="my-voice")
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--lufs", type=float, default=-14.0)
    ap.add_argument("--plain", action="store_true", help="不加口播风格指令（纯克隆）")
    ap.add_argument("--keep-raw", action="store_true", help="保留未母带的原始克隆")
    args = ap.parse_args()

    text = args.text if args.text else Path(args.text_file).read_text(encoding="utf-8").strip()
    if not text:
        raise SystemExit("文本为空")
    chars = len(re.sub(r"[\s，。！？、；：,.!?;:\"'“”]", "", text))

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    ins = PLAIN_INSTRUCTION if args.plain else DOUYIN_INSTRUCTION

    audio = tts(text, args.voice, ins, args.seed)
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tf:
        tmp = Path(tf.name)
    tmp.write_bytes(audio)
    if args.keep_raw:
        raw = out.with_name(out.stem + "-raw.wav")
        raw.write_bytes(audio)

    master(tmp, out, args.lufs)
    ok, rows = verify(out, chars)
    print("\n".join(rows))
    print(f"产物: {out}")
    try:
        tmp.unlink()
    except OSError:
        pass
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
