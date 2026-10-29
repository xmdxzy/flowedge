#!/usr/bin/env python3
"""成片验收器 —— 交付前必跑，把「看着像成功」变成「数字上成立」。

为什么需要这个脚本
------------------
2026-10-19 的 30 秒斗法成片出过三类只有在成片里才看得见的缺陷，事后复盘发现
全都是「验收标准不完整」造成的，而不是生成能力不够：

  1. 段间 xfade 时长 < 一帧 → 第二段画面整段丢失。**时长正常、音频完整**，
     只查 duration 永远发现不了；必须数视频帧。
  2. 每段音频独立生成 + 硬拼 → 接缝处「断一下」。听感主观，用 silencedetect
     找中段的静音缺口才有客观依据。
  3. 提示词没写「无对白」→ H3 即兴编含糊人声。只有跑 ASR 才能确认。

所以验收 = 流 + 时长 + 帧数一致性 + 静音缺口 + 响度 + (可选) ASR 转写。
少任何一项都会漏掉上面某类缺陷。

用法
----
    python scripts/verify-media.py 成片.mp4
    python scripts/verify-media.py a.mp4 b.mp4 --asr
    python scripts/verify-media.py 成片.mp4 --frames .tmp/frames --json

退出码：0 全过 / 1 有警告 / 2 有硬失败（可直接用于 CI 或流水线门禁）
零第三方依赖：只用 ffmpeg/ffprobe + 标准库。
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.request
import uuid

FAIL = "FAIL"
WARN = "WARN"
OK = "OK"

# 判定阈值（都有实测依据，不是拍脑袋）
FRAME_RATIO_TOL = 0.03      # 实际帧率与标称帧率偏差 >3% → 有片段丢失
AV_SYNC_TOL = 0.20          # 音视频总时长偏差 >0.2s → 拼接丢段
SILENCE_MIN = 0.45          # 中段静音 >0.45s → 疑似断音（正常呼吸间隔都短于此）
SILENCE_EDGE = 0.60         # 片头片尾各留 0.6s 不算（淡入淡出）
CLIP_DB = -0.5              # 峰值超过此值 → 削波
QUIET_MEAN_DB = -42.0       # 整片均值低于此值 → 基本没声音（硬伤）
# 交付电平：低于此值不算「没声音」，但已经不在可交付区间（-23 ~ -16 dB）。
# 为什么单独一档：2026-10-25 实测 video 单镜产物 mean=-41.0dB，旧阈值 -42 判它「通过」，
# 而耳朵上已经听不清。阈值掩盖偏差，比没有阈值更坏。
DELIVER_MEAN_DB = -30.0
DELIVER_HINT = ("交付电平偏低：用 loudnorm 收口（站内「产物 → 测响度 → 响度归一」一键完成，"
                "目标 -16 LUFS / 峰值 -1.5dB）")


def find_bin(name: str) -> str:
    """找 ffmpeg / ffprobe。PATH 优先，再退到常见安装位置。"""
    p = shutil.which(name)
    if p:
        return p
    for cand in (rf"C:\ffmpeg\bin\{name}.exe", rf"D:\ffmpeg\bin\{name}.exe"):
        if os.path.isfile(cand):
            return cand
    return name


FFMPEG = find_bin("ffmpeg")
FFPROBE = find_bin("ffprobe")


def run(cmd: list[str], timeout: int = 300) -> tuple[int, str, str]:
    try:
        cp = subprocess.run(
            cmd, capture_output=True, text=True, encoding="utf-8",
            errors="replace", timeout=timeout,
        )
        return cp.returncode, cp.stdout or "", cp.stderr or ""
    except FileNotFoundError:
        return 127, "", f"找不到可执行文件：{cmd[0]}"
    except subprocess.TimeoutExpired:
        return 124, "", "命令超时"


def probe(path: str) -> dict:
    """ffprobe -show_streams -show_format → dict（不解析文本输出，避免格式漂移）"""
    code, out, err = run([
        FFPROBE, "-v", "error", "-print_format", "json",
        "-show_format", "-show_streams", path,
    ])
    if code != 0:
        raise RuntimeError(f"ffprobe 失败：{err.strip()[:300]}")
    return json.loads(out)


def count_frames(path: str) -> int:
    """精确帧数。这是抓「画面丢段」的唯一可靠手段。"""
    code, out, _ = run([
        FFPROBE, "-v", "error", "-select_streams", "v:0", "-count_frames",
        "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", path,
    ], timeout=600)
    if code != 0:
        return -1
    m = re.search(r"(\d+)", out)
    return int(m.group(1)) if m else -1


def parse_rate(s: str) -> float:
    """'24/1' → 24.0"""
    if not s:
        return 0.0
    if "/" in s:
        a, b = s.split("/", 1)
        try:
            return float(a) / float(b) if float(b) else 0.0
        except ValueError:
            return 0.0
    try:
        return float(s)
    except ValueError:
        return 0.0


def detect_silence(path: str) -> list[tuple[float, float]]:
    """返回中段静音区间 [(start, end)]，用来发现接缝断音。"""
    code, _, err = run([
        FFMPEG, "-hide_banner", "-nostats", "-i", path,
        "-af", f"silencedetect=noise=-38dB:d={SILENCE_MIN}", "-f", "null", "-",
    ])
    starts = [float(m) for m in re.findall(r"silence_start:\s*(-?[\d.]+)", err)]
    ends = [float(m) for m in re.findall(r"silence_end:\s*(-?[\d.]+)", err)]
    return list(zip(starts, ends + [0.0] * (len(starts) - len(ends))))


def loudness(path: str) -> dict:
    code, _, err = run([
        FFMPEG, "-hide_banner", "-nostats", "-i", path,
        "-af", "volumedetect", "-f", "null", "-",
    ])
    res = {}
    for key, pat in (("mean", r"mean_volume:\s*(-?[\d.]+)"), ("max", r"max_volume:\s*(-?[\d.]+)")):
        m = re.search(pat, err)
        if m:
            res[key] = float(m.group(1))
    return res


def extract_audio_wav(path: str, out: str) -> bool:
    code, _, _ = run([
        FFMPEG, "-y", "-v", "error", "-i", path,
        "-vn", "-ac", "1", "-ar", "16000", out,
    ])
    return code == 0 and os.path.isfile(out)


def asr_transcribe(wav: str, endpoint: str) -> tuple[bool, str]:
    """POST multipart 到本地 ASR（默认 8101）→ (可达, 文本)。

    必须把「服务不可达」和「确实无人声」分开：两者的返回值都是空文本，
    但结论完全相反 —— 前者是验收没做，后者是验收通过。
    """
    boundary = "----flowedge" + uuid.uuid4().hex
    with open(wav, "rb") as f:
        data = f.read()
    body = b"".join([
        f"--{boundary}\r\n".encode(),
        b'Content-Disposition: form-data; name="file"; filename="a.wav"\r\n',
        b"Content-Type: audio/wav\r\n\r\n",
        data,
        f"\r\n--{boundary}--\r\n".encode(),
    ])
    req = urllib.request.Request(
        endpoint.rstrip("/") + "/asr", data=body,
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
    )
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            return True, json.loads(r.read().decode("utf-8")).get("text", "")
    except Exception:
        return False, ""


def verify(path: str, want_asr: bool, frames_dir: str | None, expect: str = "",
           no_speech: bool = False, max_silence: float = 0.0) -> dict:
    issues: list[tuple[str, str, str]] = []   # (level, code, message)
    info: dict = {"file": os.path.abspath(path)}

    if not os.path.isfile(path):
        return {"file": path, "issues": [(FAIL, "missing", "文件不存在")], "info": info}

    try:
        meta = probe(path)
    except RuntimeError as e:
        return {"file": path, "issues": [(FAIL, "probe", str(e))], "info": info}

    streams = meta.get("streams", [])
    v = next((s for s in streams if s.get("codec_type") == "video"), None)
    a = next((s for s in streams if s.get("codec_type") == "audio"), None)
    fmt = meta.get("format", {})

    total = float(fmt.get("duration") or 0)
    info["duration"] = round(total, 3)
    info["size_mb"] = round(int(fmt.get("size") or 0) / 1048576, 2)

    # 1) 流存在性
    #
    # 纯音频产物（配音 / 音效）不是缺陷：voice 流水线的成品就是 wav。
    # 以前一律报 no_video → FAIL，等于让「文案转配音」永远无法通过验收。
    audio_exts = {".wav", ".mp3", ".flac", ".m4a", ".aac", ".ogg", ".opus"}
    audio_only = (not v) and bool(a) and os.path.splitext(path)[1].lower() in audio_exts
    if audio_only:
        issues.append((OK, "audio_only", "纯音频产物（配音/音效）。检测到人声通常是预期；要核对语句请加 --expect '原句'"))
    elif not v:
        issues.append((FAIL, "no_video", "没有视频流（纯音频，常见于 -map 漏写）"))
    if not a:
        issues.append((WARN, "no_audio", "没有音频流（无声片；若本应有声则是 -map/编码问题）"))

    if v:
        fps = parse_rate(v.get("r_frame_rate") or v.get("avg_frame_rate") or "")
        vdur = float(v.get("duration") or total)
        width, height = v.get("width"), v.get("height")
        info.update({
            "video_codec": v.get("codec_name"),
            "size": f"{width}x{height}",
            "fps": round(fps, 3),
            "video_duration": round(vdur, 3),
        })

        # 2) 精确帧数 + 帧率自洽（抓「丢整段画面」）
        n = count_frames(path)
        info["frames"] = n
        if n > 0 and fps > 0 and vdur > 0:
            eff = n / vdur
            info["effective_fps"] = round(eff, 3)
            dev = abs(eff - fps) / fps
            info["fps_deviation"] = round(dev, 4)
            if dev > FRAME_RATIO_TOL:
                issues.append((
                    FAIL, "frame_shortfall",
                    f"实际帧率 {eff:.2f} vs 标称 {fps:.2f}（差 {dev*100:.1f}%）："
                    f"疑似有片段画面整段丢失 —— xfade 时长小于一帧时会这样，音频却仍是全长",
                ))
        elif n <= 0:
            issues.append((WARN, "frame_count", "无法精确计数帧（耗时为 0 或解码失败）"))

    if a:
        adur = float(a.get("duration") or total)
        info.update({
            "audio_codec": a.get("codec_name"),
            "sample_rate": a.get("sample_rate"),
            "channels": a.get("channels"),
            "audio_duration": round(adur, 3),
        })
        # 3) 音画总长一致
        if v and abs(adur - float(v.get("duration") or total)) > AV_SYNC_TOL:
            issues.append((
                FAIL, "av_desync",
                f"音视频时长差 {abs(adur - float(v.get('duration') or total)):.2f}s："
                f"拼接时丢段或音轨未同步截断",
            ))

        # 4) 中段静音缺口（断音）
        gaps = detect_silence(path)
        mid_gaps = [
            (s, e) for s, e in gaps
            if s > SILENCE_EDGE and (e == 0.0 or e < total - SILENCE_EDGE)
        ]
        info["silence_gaps"] = [[round(s, 2), round(e, 2)] for s, e in mid_gaps]
        for s, e in mid_gaps:
            dur = (e - s) if e else (total - s)
            if dur >= SILENCE_MIN:
                # 门给了上限就按门判：静音缺口是「声音断一下」的直接特征，
                # 只报 WARN 等于让它溜过去。
                hard = max_silence > 0 and dur >= max_silence
                issues.append((
                    FAIL if hard else WARN, "silence_gap",
                    f"中段静音 {dur:.2f}s @ {s:.2f}s"
                    + (f"（门上限 {max_silence}s）" if hard
                       else "：可能是接缝断音或该镜无环境声"),
                ))

        # 5) 响度
        lv = loudness(path)
        info["loudness"] = lv
        if "max" in lv and lv["max"] > CLIP_DB:
            issues.append((WARN, "clip", f"峰值 {lv['max']}dB 已削波（批量拼接前建议 loudnorm）"))
        if "mean" in lv and lv["mean"] < QUIET_MEAN_DB:
            # 低于 -42dB 就是听不见，不该只记 WARN 放过
            issues.append((FAIL, "silent", f"均值 {lv['mean']}dB 近乎无声（低于 {QUIET_MEAN_DB}dB，属硬伤）"))
        elif "mean" in lv and lv["mean"] < DELIVER_MEAN_DB:
            issues.append((WARN, "too_quiet", f"均值 {lv['mean']}dB 低于交付电平 {DELIVER_MEAN_DB}dB。{DELIVER_HINT}"))

    # 6) ASR 抽检（乱语/不该有的人声）
    if want_asr and os.path.isfile(path):
        with tempfile.TemporaryDirectory() as td:
            wav = os.path.join(td, "a.wav")
            if not extract_audio_wav(path, wav):
                issues.append((WARN, "asr_skip", "抽不出音轨，无法做 ASR 抽检"))
            else:
                ok, text = asr_transcribe(wav, os.environ.get("ASR_ENDPOINT", "http://127.0.0.1:8101"))
                if not ok:
                    # 关键区分：服务没起 ≠ 没有人声。前者是「验收没做」，必须显式报出来
                    issues.append((
                        FAIL, "asr_unreachable",
                        "ASR 服务不可达，人声抽检**没有执行**——不要当作「无人声」通过",
                    ))
                else:
                    info["asr_text"] = text
                    # --expect 是念稿核对模式：有人声是预期内的，不再报"乱语"
                    if len(text) >= 2 and not expect:
                        issues.append((
                            FAIL if no_speech else WARN, "speech_detected",
                            f"检测到人声/疑似乱语：「{text[:60]}」——若本片无对白，需压音或重跑",
                        ))
                    # 台词核对：念稿模式下，ASR 必须能转出脚本里的原话
                    if expect:
                        flat = re.sub(r"[\s，。！？、,.!?]", "", text)
                        want = re.sub(r"[\s，。！？、,.!?]", "", expect)
                        if want and want not in flat:
                            issues.append((
                                FAIL, "line_mismatch",
                                f"台词与脚本不符：期望「{expect}」，实际「{text[:60]}」",
                            ))

    # 7) 抽帧，供人眼/多模态复看（0/25/50/75/99%）
    if frames_dir and v and total > 0:
        os.makedirs(frames_dir, exist_ok=True)
        stem = os.path.splitext(os.path.basename(path))[0]
        saved = []
        for pct in (0.0, 0.25, 0.5, 0.75, 0.99):
            out = os.path.join(frames_dir, f"{stem}_{int(pct*100):02d}.jpg")
            t = max(0.0, min(total - 0.05, total * pct))
            code, _, _ = run([
                FFMPEG, "-y", "-v", "error", "-ss", f"{t:.3f}", "-i", path,
                "-frames:v", "1", "-q:v", "3", out,
            ])
            if code == 0 and os.path.isfile(out):
                saved.append(out)
        info["frames_saved"] = saved

    return {"file": path, "issues": issues, "info": info}


def main() -> int:
    ap = argparse.ArgumentParser(description="成片验收：流/时长/帧数/静音/响度/ASR")
    ap.add_argument("files", nargs="+", help="待验收的媒体文件")
    ap.add_argument("--asr", action="store_true", help="跑本地 ASR 抽检人声（需 8101 在跑）")
    ap.add_argument("--frames", metavar="DIR", help="抽 5 帧到这里，便于人眼/多模态复看")
    ap.add_argument("--json", action="store_true", help="输出 JSON")
    ap.add_argument("--expect", default="", help="期望的台词（念稿模式）；ASR 转写必须包含它")
    ap.add_argument("--no-speech", action="store_true",
                    help="氛围模式：检测到人声即判 FAIL（这是本模式的验收线）")
    ap.add_argument("--gate", action="store_true",
                    help="流水线门禁模式：只有 FAIL 才返回 2（WARN 返回 0），便于图里判定")
    ap.add_argument("--max-silence", type=float, default=0.0,
                    help="中段静音上限（秒）；超过即 FAIL。0 = 只告警")
    ap.add_argument("--require-expect", action="store_true",
                    help="台词模式的防呆：--expect 为空时直接 FAIL。"
                         "门不能形同虚设——忘了传期望台词，不应静默通过")
    args = ap.parse_args()

    if args.require_expect and not args.expect.strip():
        print("[FAIL] expect_missing: 台词模式要求 --expect，未提供则无从核对，按失败处理")
        return 2

    results = [verify(f, args.asr, args.frames, args.expect, args.no_speech,
                      args.max_silence) for f in args.files]

    worst = OK
    for r in results:
        for lvl, _, _ in r["issues"]:
            if lvl == FAIL:
                worst = FAIL
            elif lvl == WARN and worst != FAIL:
                worst = WARN

    if args.json:
        print(json.dumps({"worst": worst, "results": results}, ensure_ascii=False, indent=2))
    else:
        for r in results:
            i = r["info"]
            print(f"\n=== {r['file']} ===")
            if "duration" in i:
                print(
                    f"时长 {i['duration']}s · {i.get('size','?')} · {i.get('fps','?')}fps · "
                    f"帧数 {i.get('frames','?')} · 有效帧率 {i.get('effective_fps','?')} · "
                    f"{i.get('size_mb','?')}MB"
                )
                print(
                    f"视频 {i.get('video_codec','-')} / 音频 {i.get('audio_codec','-')} "
                    f"{i.get('sample_rate','-')}Hz {i.get('channels','-')}ch"
                )
                if i.get("silence_gaps"):
                    print(f"中段静音：{i['silence_gaps']}")
                if i.get("loudness"):
                    print(f"响度：{i['loudness']}")
                if "asr_text" in i:
                    print(f"ASR：{i['asr_text'][:80] or '（空）'}")
                if i.get("frames_saved"):
                    print("抽帧：" + " ".join(i["frames_saved"]))
            if not r["issues"]:
                print("✓ 全部通过")
            else:
                for lvl, code, msg in r["issues"]:
                    print(f"[{lvl}] {code}: {msg}")

        print(f"\n结论：{worst}")
        if worst == FAIL:
            print("→ 有硬失败，不要交付。先按 code 定位（frame_shortfall / av_desync 最常见于拼接）")
        elif worst == WARN:
            print("→ 有警告。逐条判断是否可接受；silence_gap / speech_detected 通常要修")

    if args.gate:
        return 2 if worst == FAIL else 0
    return 0 if worst == OK else (2 if worst == FAIL else 1)


if __name__ == "__main__":
    sys.exit(main())

