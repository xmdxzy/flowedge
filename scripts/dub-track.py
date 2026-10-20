#!/usr/bin/env python3
"""把台词台词表合成为一条「念稿音轨」，供 H3 ref2va 的 ref_audio 使用。

为什么需要这一层
----------------
ref2va 里写 `<Audio 1>: fully_copy` 时，这段音频会成为成片的**完整最终音轨**，
模型只负责对口型，不再自己造声音。所以：
  - 台词必须提前合成（TTS），不能指望模型念
  - 环境声必须提前混进来（模型不会往音轨里加东西）
  - 音轨长度要与视频时长对齐，否则口型会漂

用法
----
    python scripts/dub-track.py --lines '[{"speaker":"S1","text":"这件事，我其实早就知道了。","pause":0.4}]' \
        --out dub.wav --duration 4.5
    python scripts/dub-track.py --lines-file lines.json --out dub.wav --ambience amb.wav --duration 4.5

输出：单条 32kHz / 立体声 wav（H3 音频 VAE 的习惯格式），并打印 JSON 摘要。
零第三方依赖（ffmpeg + 标准库 + 本地 TTS 服务）。
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
import urllib.request

TTS_ENDPOINT = os.environ.get("TTS_ENDPOINT", "http://127.0.0.1:8102")

# 本地端口必须绕过企业代理，否则 502 假死
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
OUT_RATE = 32000


def find_ffmpeg() -> str:
    from shutil import which
    for name in ("ffmpeg", "ffmpeg.exe"):
        p = which(name)
        if p:
            return p
    for c in (r"C:\ffmpeg\bin\ffmpeg.exe", r"D:\ffmpeg\bin\ffmpeg.exe"):
        if os.path.isfile(c):
            return c
    return "ffmpeg"


FFMPEG = find_ffmpeg()


def run(cmd: list[str], timeout: int = 300) -> tuple[int, str]:
    cp = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8",
                        errors="replace", timeout=timeout)
    return cp.returncode, (cp.stderr or "") + (cp.stdout or "")


def polish_voice(src: str, dst: str) -> bool:
    """裁静音 + 去机械感 + 加空间感 + 归一化。失败返回 False。

    为什么必须做（2026-10-20 实测）：
      1. Kokoro 每句自带约 0.44s 前导静音、0.6-1.1s 尾部静音。
         「十年了。」原始 2.23s，真正说话只有 0.72s。不裁的话每句台词都比
         计划晚 0.44s 开口，而画面是按计划时点切镜头的——这就是"音画不同"的来源。
      2. TTS 是干声，塞进空旷城门/雪夜的画面里必然"贴上去"。加早期反射 +
         短混响尾，人声才会落在同一个空间里。
    """
    af = (
        # 裁首尾静音，保留 30/50ms 余量避免切到起音和收尾
        "silenceremove=start_periods=1:start_threshold=-45dB:start_silence=0.03,"
        "areverse,"
        "silenceremove=start_periods=1:start_threshold=-45dB:start_silence=0.05,"
        "areverse,"
        # 去机械感：切掉低频嗡、抬一点胸腔共鸣
        "highpass=f=85,"
        "equalizer=f=180:t=q:w=1.0:g=1.5,"
        # 空间感：三个抽头的早期反射（约 2.3m / 4.8m / 8.8m 的墙面反射）
        "aecho=0.85:0.85:55|115|210:0.22|0.14|0.08,"
        # 混响尾不要被裁掉
        "apad=pad_dur=0.2,"
        "loudnorm=I=-16:TP=-2:LRA=9,"
        f"aresample={OUT_RATE}"
    )
    code, _ = run([FFMPEG, "-y", "-v", "error", "-i", src, "-af", af,
                   "-ar", str(OUT_RATE), "-ac", "2", dst])
    return code == 0


def tts(text: str, out_wav: str, speed: float = 1.0, voice: str = "") -> None:
    body = json.dumps({"text": text, "speed": speed, **({"voice": voice} if voice else {})}).encode()
    req = urllib.request.Request(TTS_ENDPOINT.rstrip("/") + "/tts", data=body,
                                 headers={"Content-Type": "application/json"})
    data = _OPENER.open(req, timeout=120).read()
    if len(data) < 1000:
        raise RuntimeError(f"TTS 返回异常（{len(data)} 字节）")
    with open(out_wav, "wb") as f:
        f.write(data)


def duration_of(path: str) -> float:
    code, out = run([FFMPEG, "-hide_banner", "-i", path], timeout=60)
    for ln in out.splitlines():
        ln = ln.strip()
        if ln.startswith("Duration:"):
            seg = ln.split("Duration:")[1].split(",")[0].strip()
            h, m, s = seg.split(":")
            return float(h) * 3600 + float(m) * 60 + float(s)
    return 0.0


def silence(path: str, seconds: float) -> None:
    code, out = run([FFMPEG, "-y", "-v", "error", "-f", "lavfi",
                     "-i", f"anullsrc=r={OUT_RATE}:cl=stereo", "-t", f"{seconds:.3f}", path])
    if code != 0:
        raise RuntimeError(f"生成静音失败：{out[:200]}")


def main() -> int:
    ap = argparse.ArgumentParser(description="台词 → 念稿音轨（TTS + 环境声混合）")
    ap.add_argument("--lines", help="JSON 数组：[{speaker,text,speed,pause}]")
    ap.add_argument("--lines-file", help="从文件读上面的 JSON")
    ap.add_argument("--out", required=True, help="输出 wav")
    ap.add_argument("--duration", type=float, default=0.0,
                    help="目标时长（秒）；给定则补静音/截断到该长度，用于对齐视频")
    ap.add_argument("--ambience", help="环境底噪 wav（会循环铺满并混入）")
    ap.add_argument("--ambience-gain", type=float, default=0.85,
                    help="环境声线性增益（默认 0.85，约等于把底噪定在 -32dB 可闻但远低于人声）")
    ap.add_argument("--lead", type=float, default=0.25, help="开场留白（秒）")
    ap.add_argument("--frames", type=int, default=0, help="按视频帧数对齐时长（优先于 --duration）")
    ap.add_argument("--fps", type=int, default=24)
    args = ap.parse_args()

    raw = args.lines
    if args.lines_file:
        with open(args.lines_file, encoding="utf-8") as f:
            raw = f.read()
    if not raw:
        print(json.dumps({"ok": False, "error": "缺少 --lines 或 --lines-file"}, ensure_ascii=False))
        return 2

    if args.frames > 0:
        args.duration = args.frames / float(args.fps)

    lines = json.loads(raw)
    if isinstance(lines, dict):
        lines = lines.get("lines", [])
    if not lines:
        print(json.dumps({"ok": False, "error": "台词表为空"}, ensure_ascii=False))
        return 2

    tmpdir = tempfile.mkdtemp(prefix="dub-")
    parts: list[str] = []
    report = []

    # 开场留白
    cursor = 0.0
    if args.lead > 0:
        p = os.path.join(tmpdir, "lead.wav")
        silence(p, args.lead)
        parts.append(p)
        cursor = args.lead

    for i, item in enumerate(lines):
        text = (item.get("text") or "").strip()
        if not text:
            continue
        wav = os.path.join(tmpdir, f"line{i}.wav")
        tts(text, wav, float(item.get("speed") or 1.0), item.get("voice") or "")
        # 逐句整形：裁静音 → 去机械感 → 加空间感 → 单句归一化。
        # 归一化必须在**单句**上做：整条轨道跑 loudnorm 会被长静音带偏，
        # 实测把台词从 -18dB 压到 -27dB，底噪反而更响，ASR 直接转不出字。
        norm = os.path.join(tmpdir, f"line{i}-n.wav")
        if not polish_voice(wav, norm):
            # 整形失败不中断，退回原始 TTS（至少还有声音），但要说出来
            print(json.dumps({"ok": True, "warn": f"第 {i+1} 句整形失败，使用原始 TTS"},
                             ensure_ascii=False))
        else:
            wav = norm
        parts.append(wav)
        dur = duration_of(wav)
        # 记录真实起止：台词是否落在镜头切点上，靠这两个数核对，不靠猜
        report.append({"speaker": item.get("speaker") or f"S{i+1}", "text": text,
                       "seconds": round(dur, 3),
                       "start": round(cursor, 3), "end": round(cursor + dur, 3)})
        cursor += dur
        pause = float(item.get("pause", 0.35))
        if pause > 0:
            p = os.path.join(tmpdir, f"gap{i}.wav")
            silence(p, pause)
            parts.append(p)
            cursor += pause

    if not parts:
        print(json.dumps({"ok": False, "error": "没有可用台词"}, ensure_ascii=False))
        return 2

    # concat
    listfile = os.path.join(tmpdir, "list.txt")
    with open(listfile, "w", encoding="utf-8") as f:
        for p in parts:
            f.write("file '%s'\n" % p.replace("\\", "/").replace("'", r"'\''"))
    joined = os.path.join(tmpdir, "joined.wav")
    code, out = run([FFMPEG, "-y", "-v", "error", "-f", "concat", "-safe", "0",
                     "-i", listfile, "-ar", str(OUT_RATE), "-ac", "2", joined])
    if code != 0:
        print(json.dumps({"ok": False, "error": "拼接失败", "detail": out[:400]}, ensure_ascii=False))
        return 2

    spoken = duration_of(joined)

    # 目标时长对齐：不足补静音，超出必须截断。
    # 不截断会让 wav 比视频长，mux 时 -shortest 会砍掉结尾——如果台词压在末尾就丢字。
    target = args.duration or 0.0
    if target > 0 and spoken > target + 0.05:
        print(json.dumps({"ok": True, "warn": f"台词总长 {spoken:.2f}s 超过目标 {target:.2f}s，"
                                              f"已截断（lead/pause 需要收紧）"}, ensure_ascii=False))
        cut = os.path.join(tmpdir, "cut.wav")
        run([FFMPEG, "-y", "-v", "error", "-i", joined, "-t", f"{target:.3f}",
             "-ar", str(OUT_RATE), "-ac", "2", cut])
        joined, spoken = cut, target
    if target > 0 and spoken < target:
        pad = os.path.join(tmpdir, "pad.wav")
        silence(pad, target - spoken)
        lst2 = os.path.join(tmpdir, "list2.txt")
        with open(lst2, "w", encoding="utf-8") as f:
            f.write("file '%s'\nfile '%s'\n" % (joined.replace("\\", "/"), pad.replace("\\", "/")))
        joined2 = os.path.join(tmpdir, "joined2.wav")
        run([FFMPEG, "-y", "-v", "error", "-f", "concat", "-safe", "0", "-i", lst2,
             "-ar", str(OUT_RATE), "-ac", "2", joined2])
        joined = joined2

    final = joined
    mixed_env = False
    if args.ambience and os.path.isfile(args.ambience):
        mixed = os.path.join(tmpdir, "mixed.wav")
        span = max(spoken, target or spoken)
        # 顺序很重要：先分别定电平，再相加。
        # 先 amix 再 loudnorm 会让底噪占掉响度预算 → 人声被压到听不清
        # （实测：底噪混到 -45dB 后第三段整段被判静音；抬到 -17dB 又让 ASR 转不出字）。
        code, out = run([
            FFMPEG, "-y", "-v", "error",
            "-i", joined, "-stream_loop", "-1", "-i", args.ambience,
            "-filter_complex",
            f"[0:a]anull[voc];"
            f"[1:a]volume={args.ambience_gain},atrim=0:{span:.3f}[amb];"
            f"[voc][amb]amix=inputs=2:duration=first:normalize=0,"
            f"alimiter=limit=0.95[out]",
            "-map", "[out]", "-ar", str(OUT_RATE), "-ac", "2", mixed,
        ])
        if code == 0:
            final = mixed
            mixed_env = True
        else:
            print(json.dumps({"ok": False, "warn": "环境声混入失败，仅保留人声", "detail": out[:300]},
                             ensure_ascii=False))

    outdir = os.path.dirname(os.path.abspath(args.out))
    if outdir:
        os.makedirs(outdir, exist_ok=True)
    # 限幅：TTS 原始峰值常顶到 0dB，fully_copy 会把这个削波原样带进成片
    # 混过环境声的已经在上一步归一化，再 loudnorm 会把底噪重新抬起来
    af = "alimiter=limit=0.95" if mixed_env else "loudnorm=I=-16:TP=-1.5:LRA=11"
    code, out = run([FFMPEG, "-y", "-v", "error", "-i", final,
                     "-af", af,
                     "-ar", str(OUT_RATE), "-ac", "2", "-c:a", "pcm_s16le", args.out])
    if code != 0:
        print(json.dumps({"ok": False, "error": "写出失败", "detail": out[:400]}, ensure_ascii=False))
        return 2

    total = duration_of(args.out)
    print(json.dumps({
        "ok": True, "out": os.path.abspath(args.out),
        "seconds": round(total, 3), "spoken_seconds": round(spoken, 3),
        "ambience_mixed": final != joined, "lines": report,
    }, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
