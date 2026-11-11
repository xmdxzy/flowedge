#!/usr/bin/env python3
"""等 run 完成，然后对产物音轨做静音判定。

判定为什么必须三重：
  · volumedetect 的 max = -inf / -91dB 才是**数字静音**；
  · 若 max 在 -60~-40dB，是模型输出的一层极低底噪（人耳几乎不可闻但仍不是静音）；
  · ebur128 的 I = -inf 或 -70 才有旁证意义（它的门限会把静音报成 -70）。
单一指标会把「底噪」误判成「静音」，进而让后期方案建立在错的假设上。
"""
import json
import subprocess
import sys
import time
import urllib.request

RUN = sys.argv[1] if len(sys.argv) > 1 else "434"
BASE = "http://127.0.0.1:8080"
urllib.request.install_opener(
    urllib.request.build_opener(urllib.request.ProxyHandler({})))


def get(path: str) -> dict:
    return json.load(urllib.request.urlopen(BASE + path, timeout=30))


def wait(run_id: str) -> dict:
    last = ""
    while True:
        r = get(f"/api/runs/{run_id}")
        st = r.get("status", "")
        if st != last:
            print(f"  status={st}", flush=True)
            last = st
        if st in ("success", "failed", "cancelled", "interrupted"):
            return r
        time.sleep(5)


def measure(path: str) -> dict:
    out = {}
    v = subprocess.run(["ffmpeg", "-hide_banner", "-i", path, "-af", "volumedetect",
                        "-f", "null", "-"], capture_output=True, text=True).stderr
    for ln in v.splitlines():
        if "mean_volume:" in ln:
            out["mean_db"] = ln.split("mean_volume:")[1].strip()
        if "max_volume:" in ln:
            out["max_db"] = ln.split("max_volume:")[1].strip()
    e = subprocess.run(["ffmpeg", "-hide_banner", "-nostats", "-i", path,
                        "-af", "ebur128", "-f", "null", "-"],
                       capture_output=True, text=True).stderr
    for ln in e.splitlines():
        s = ln.strip()
        if s.startswith("I:") and "LUFS" in s:
            out["integrated_lufs"] = s
    a = subprocess.run(["ffmpeg", "-hide_banner", "-i", path, "-af",
                        "astats=metadata=1:reset=0", "-f", "null", "-"],
                       capture_output=True, text=True).stderr
    for ln in a.splitlines():
        s = ln.strip()
        if s.startswith("RMS level dB"):
            out["rms_db"] = s
        if s.startswith("Peak level dB"):
            out["peak_db"] = s
    return out


def main() -> int:
    r = wait(RUN)
    print("run:", r.get("status"), "|", (r.get("error") or "")[:160])
    vid = ""
    for n in r.get("node_states", []):
        if n.get("id") == "end-1":
            d = n.get("output") or n.get("data") or {}
            vid = d.get("video") or ""
    if not vid:
        print("没拿到产物路径")
        return 2
    print("video:", vid)
    m = measure(vid)
    print("\n=== 音轨测量 ===")
    for k, v in m.items():
        print(f"  {k:18s} {v}")
    # 判定
    # 阈值依据：16bit 理论动态范围 96dB，`max < -80dB` 只可能是编码量化残差，
    # 任何回放链路的底噪都高于它 ⇒ 属**数字静音**，不该当"有底噪"去修。
    mx = m.get("max_db", "")
    if "inf" in mx.lower():
        print("\n判定：数字静音（max = -inf）")
    elif mx:
        db = float(mx.replace("dB", "").strip())
        if db < -80:
            print(f"\n判定：**数字静音**（max {mx}，属 AAC 量化残差）→ 可后期配乐")
        elif db < -60:
            print(f"\n判定：极低底噪（max {mx}）→ 人耳不可闻，但严谨起见配乐前可 mute")
        elif db < -40:
            print(f"\n判定：**可闻底噪**（max {mx}）→ 必须显式 mute 再叠加音乐")
        else:
            print(f"\n判定：**仍有实际声音**（max {mx}）→ N/A 未生效，换策略")
    return 0


if __name__ == "__main__":
    sys.exit(main())
