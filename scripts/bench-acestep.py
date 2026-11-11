#!/usr/bin/env python3
"""ACE-Step 1.5 XL 性能台架：冷加载 / 热态 / 长曲 三个点。

为什么不是跑一次冒烟就完事：单点测不出「加载成本」与「每秒音频成本」分开看，
也看不出扩展性是否线性。三个点才能回答"这模型能不能用于日常出片"。

判据（AGENTS §0.5 精神：工具没报错 ≠ 没问题）：
  · 产物必须非静音、不削波、采样率/声道符合预期——只看"生成成功"会漏掉空音频；
  · VRAM 峰值单独采（nvidia-smi 轮询），不靠模型自己报；
  · 分段计时：提交→拿到 task_id（排队）与 task_id→完成（计算）分开记。

用法：
  python scripts/bench-acestep.py                       # 三轮全跑
  python scripts/bench-acestep.py --only cold            # 只跑冷轮
  python scripts/bench-acestep.py --prompt "..." --dur 13
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import threading
import time
import urllib.request

BASE = "http://127.0.0.1:8104"
OUT_DIR = "运行/data/generated"
# 适配「暗色极简摄影棚 + 金属结构解构」：干净、精密、低频铺底、不要鼓点抢节奏
DEFAULT_PROMPT = ("clean minimal tech ambient, sparse low-frequency synth pad, "
                  "precise metallic arpeggio, soft electronic pulse, "
                  "no vocals, cinematic product film underscore")
MODEL = "acestep-v15-xl-turbo"

# 必须绕代理：本机 http_proxy 会劫持 127.0.0.1（AGENTS §0.6）
_op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
urllib.request.install_opener(_op)


def post(path: str, body: dict, timeout: int = 1800) -> dict:
    req = urllib.request.Request(
        BASE + path, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def vram_used() -> int:
    """当前显存占用 MiB；采不到返回 -1（区分「0」与「未知」）。"""
    try:
        out = subprocess.run(
            ["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=10).stdout.strip().splitlines()
        return int(out[0]) if out else -1
    except Exception:
        return -1


class PeakVram(threading.Thread):
    """后台高频采样显存峰值。热态只有 4–5 秒，轮询点不够会采不到。"""

    def __init__(self) -> None:
        super().__init__(daemon=True)
        self.peak = 0
        self.base = vram_used()
        self._stop = threading.Event()

    def run(self) -> None:
        while not self._stop.is_set():
            v = vram_used()
            if v > self.peak:
                self.peak = v
            self._stop.wait(0.4)

    def stop(self) -> None:
        self._stop.set()


def generate(prompt: str, dur: float, steps: int, tag: str) -> dict:
    """提交一次生成，返回耗时/产物/显存峰值。"""
    peak = PeakVram()
    peak.start()
    t0 = time.time()
    resp = post("/release_task", {
        "prompt": prompt, "lyrics": "", "audio_duration": dur,
        "audio_format": "wav", "batch_size": 1, "inference_steps": steps,
        "use_cot_caption": False, "use_cot_language": False,   # AGENTS：关 LM CoT，否则卡 15 分钟
        "model": MODEL,
    })
    t_submit = time.time() - t0
    tid = resp["data"]["task_id"]

    while True:
        q = post("/query_result", {"task_id_list": [tid]})
        item = q["data"][0]
        if item["status"] == 1:
            break
        if item["status"] == 2:
            peak.stop()
            raise SystemExit(f"[{tag}] 生成失败: {str(item.get('result'))[:400]}")
        time.sleep(1.0)
    t_total = time.time() - t0
    peak.stop()

    url = json.loads(item["result"])[0]["file"]
    os.makedirs(OUT_DIR, exist_ok=True)
    out = os.path.abspath(f"{OUT_DIR}/acestep-{tag}.wav")
    with urllib.request.urlopen(BASE + url, timeout=600) as r, open(out, "wb") as f:
        f.write(r.read())

    info = probe(out)
    return {
        "tag": tag, "prompt": prompt, "requested_s": dur, "steps": steps,
        "submit_s": round(t_submit, 2), "total_s": round(t_total, 2),
        "compute_s": round(t_total - t_submit, 2),
        "vram_base_mib": peak.base, "vram_peak_mib": peak.peak,
        "vram_delta_mib": peak.peak - peak.base if peak.peak > 0 else -1,
        "rtf": round(t_total / dur, 3),   # 实时倍率：<1 快于实时
        "file": out, "size_mb": round(os.path.getsize(out) / 1e6, 2), **info,
    }


def probe(path: str) -> dict:
    """ffprobe + 响度：非静音、不削波是硬判据。

    坑：volumedetect 的统计打在 **info 级日志**上，写成 `-v error` 会把它们全部吞掉，
    于是 mean/max 变成 None——那看起来像「音频是静音的」，实际是我测量失败。
    这类「探测失败被当成结论」必须区分（AGENTS §0.6 的同类）：这里用默认 info 级，
    并且把「解析不到」显式标成 None，由调用方报 FAIL 而不是当成 -inf dB 的静音。
    """
    p = subprocess.run(
        ["ffprobe", "-v", "error", "-show_entries",
         "stream=codec_name,sample_rate,channels,duration", "-of", "json", path],
        capture_output=True, text=True)
    st = (json.loads(p.stdout).get("streams") or [{}])[0]
    loud = subprocess.run(
        ["ffmpeg", "-hide_banner", "-i", path, "-af", "volumedetect", "-f", "null", "-"],
        capture_output=True, text=True).stderr
    mean = max_ = None
    for ln in loud.splitlines():
        if "mean_volume:" in ln:
            mean = float(ln.split("mean_volume:")[1].split("dB")[0])
        if "max_volume:" in ln:
            max_ = float(ln.split("max_volume:")[1].split("dB")[0])
    return {
        "codec": st.get("codec_name"), "sample_rate": st.get("sample_rate"),
        "channels": st.get("channels"), "actual_s": float(st.get("duration") or 0),
        "mean_db": mean, "max_db": max_,
        # 显式区分「测不到」与「静音」：前者是测量故障，后者才是产物问题
        "loudness_measured": mean is not None and max_ is not None,
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--prompt", default=DEFAULT_PROMPT)
    ap.add_argument("--dur", type=float, default=13.0)
    ap.add_argument("--steps", type=int, default=8)
    ap.add_argument("--only", default="", help="cold|hot|long，缺省全跑")
    ap.add_argument("--json-out", default=".tmp/acestep-bench.json")
    a = ap.parse_args()

    h = json.loads(urllib.request.urlopen(BASE + "/health", timeout=30).read().decode())["data"]
    print(f"health ok | models_initialized={h['models_initialized']} | loaded={h['loaded_model']}")
    cold = not h["models_initialized"]

    plan = [("cold" if cold else "hot", a.dur)] if a.only in ("", "cold", "hot") else []
    if a.only == "":
        plan = [("cold" if cold else "hot", a.dur), ("hot", a.dur), ("long", 30.0)]
    elif a.only == "long":
        plan = [("long", 30.0)]
    elif a.only in ("cold", "hot"):
        plan = [(a.only, a.dur)]

    rows = []
    for tag, dur in plan:
        print(f"\n▶ {tag}: {dur:.0f}s 曲 / {a.steps} 步 ...", flush=True)
        try:
            r = generate(a.prompt, dur, a.steps, tag)
        except SystemExit as e:
            print(e)
            return 2
        rows.append(r)
        print(f"  {r['total_s']:.1f}s（提交 {r['submit_s']:.1f}s + 计算 {r['compute_s']:.1f}s）"
              f"  RTF {r['rtf']}  显存峰值 {r['vram_peak_mib']}MiB (+{r['vram_delta_mib']})")
        print(f"  产物 {r['actual_s']:.2f}s {r['codec']} {r['sample_rate']}Hz {r['channels']}ch"
              f"  {r['size_mb']}MB  mean {r['mean_db']}dB max {r['max_db']}dB")

    os.makedirs(os.path.dirname(a.json_out) or ".", exist_ok=True)
    json.dump(rows, open(a.json_out, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
    print(f"\n结果已存 {a.json_out}")

    # 硬判据：静音 / 削波 / 采样率不对 都要显式报，不静默放过
    bad = []
    for r in rows:
        # 「测不到」与「静音」是两件事：前者是测量故障，必须单独报，不能混成产物缺陷
        if not r.get("loudness_measured"):
            bad.append(f"{r['tag']}: 响度**测量失败**（不是静音——检查 volumedetect 日志级别）")
        elif r["mean_db"] < -40:
            bad.append(f"{r['tag']}: 疑似静音（mean {r['mean_db']}dB）")
        if r["max_db"] is not None and r["max_db"] > -0.1:
            bad.append(f"{r['tag']}: 削波（max {r['max_db']}dB）")
        if abs(r["actual_s"] - r["requested_s"]) > 1.5:
            bad.append(f"{r['tag']}: 时长偏差 {r['actual_s']:.2f}s vs 请求 {r['requested_s']}s")
    for b in bad:
        print("  FAIL", b)
    print("结论：" + ("OK" if not bad else f"{len(bad)} 项不过"))
    return 0 if not bad else 1


if __name__ == "__main__":
    sys.exit(main())
