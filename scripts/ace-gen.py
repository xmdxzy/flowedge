#!/usr/bin/env python3
"""ACE-Step 1.5 单次生成 + 落盘（可复现的 BGM 取音入口）。

用法：
  python scripts/ace-gen.py --prompt "..." --duration 60 --model acestep-v15-sft \
      --steps 20 --thinking --seed 42 --out out.wav

为什么单独写一个：引擎的 music 节点用于出片流水线，本脚本用于「给已有成片配乐」
和 A/B 台架；协议与 service/music.go 完全一致（release_task -> query_result ->
/v1/audio），字段名以实测为准。

承诺：失败必须报错，绝不产出「静音/半截」却返回 0。
"""
from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.request
import wave
from pathlib import Path

BASE = "http://127.0.0.1:8104"


def _post(path: str, payload: dict) -> dict:
    req = urllib.request.Request(
        BASE + path,
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        body = resp.read().decode("utf-8-sig")
    return json.loads(body)


def generate(prompt: str, duration: float, model: str, steps: int,
             thinking: bool, seed: int, bpm: int, key: str,
             lyrics: str = "", language: str = "") -> str:
    payload = {
        "prompt": prompt,
        "lyrics": lyrics,
        "audio_format": "wav",
        "batch_size": 1,
        "audio_duration": duration,
        "inference_steps": steps,
        "model": model,
        "thinking": thinking,
    }
    if language:
        payload["vocal_language"] = language
    if bpm:
        payload["bpm"] = bpm
    if key:
        payload["key_scale"] = key
    if not thinking and not lyrics.strip():
        # 纯音乐不需要 LM 的 CoT（实测会卡在 Phase 1 五分钟以上）
        payload["use_cot_caption"] = False
        payload["use_cot_language"] = False
    if seed >= 0:
        payload["use_random_seed"] = False
        payload["seed"] = seed

    t0 = time.time()
    sub = _post("/release_task", payload)
    data = sub.get("data") or {}
    task_id = data.get("task_id") if isinstance(data, dict) else data
    if not task_id:
        raise SystemExit(f"未拿到 task_id: {sub}")
    print(f"[submit] task={task_id} queue={data.get('queue_position')}", flush=True)

    last = ""
    while True:
        if time.time() - t0 > 1500:
            raise SystemExit("等待超时（25 分钟）")
        q = _post("/query_result", {"task_id_list": [task_id]})
        items = q.get("data") or []
        it = next((x for x in items if x.get("task_id") == task_id), None)
        if it is None:
            time.sleep(2)
            continue
        st = it.get("status")
        if st == 1:
            files = json.loads(it.get("result") or "[]")
            if isinstance(files, dict):
                files = [files]
            url = files[0]["file"]
            print(f"[done] {time.time()-t0:.1f}s -> {url}", flush=True)
            return url
        if st == 2:
            raise SystemExit(f"生成失败: {it.get('result')}")
        prog = it.get("progress")
        msg = f"[{time.time()-t0:6.1f}s] status={st} progress={prog}"
        if msg != last:
            print(msg, flush=True)
            last = msg
        time.sleep(3)


def download(url: str, out: Path) -> None:
    out.parent.mkdir(parents=True, exist_ok=True)
    with urllib.request.urlopen(BASE + url, timeout=300) as resp, open(out, "wb") as f:
        while True:
            chunk = resp.read(1 << 20)
            if not chunk:
                break
            f.write(chunk)


def envelope(path: Path, win: float = 0.5, sr: int = 16000) -> None:
    """打印 RMS 包络，用来判断「有效音乐在哪里结束」（服务端给 pcm_f32le）。"""
    import subprocess

    import numpy as np

    raw = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", str(path), "-f", "f32le", "-ac", "1",
         "-ar", str(sr), "-"],
        check=True, capture_output=True,
    ).stdout
    x = np.frombuffer(raw, dtype="<f4")
    step = int(sr * win)
    print(f"[wav] {len(x)/sr:.2f}s {sr}Hz mono", flush=True)
    for start in range(0, len(x) - step + 1, step):
        seg = x[start:start + step]
        db = 10 * np.log10(float(np.mean(seg.astype(np.float64) ** 2)) + 1e-12)
        bar = "#" * max(0, int((db + 60) / 2))
        print(f"{start/sr:7.2f}s {db:7.1f}dB {bar}", flush=True)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--prompt", required=True)
    ap.add_argument("--duration", type=float, default=60)
    ap.add_argument("--model", default="acestep-v15-sft")
    ap.add_argument("--steps", type=int, default=20)
    ap.add_argument("--thinking", action="store_true")
    ap.add_argument("--seed", type=int, default=-1)
    ap.add_argument("--bpm", type=int, default=0)
    ap.add_argument("--key", default="")
    ap.add_argument("--lyrics", default="", help="歌词；非空则出人声")
    ap.add_argument("--language", default="", help="vocal_language，如 zh / en")
    ap.add_argument("--out", required=True)
    ap.add_argument("--envelope", action="store_true")
    a = ap.parse_args()

    url = generate(a.prompt, a.duration, a.model, a.steps, a.thinking, a.seed, a.bpm, a.key,
                   lyrics=a.lyrics, language=a.language)
    out = Path(a.out)
    download(url, out)
    print(f"[saved] {out} {out.stat().st_size/1e6:.2f}MB", flush=True)
    if a.envelope:
        envelope(out)


if __name__ == "__main__":
    main()
