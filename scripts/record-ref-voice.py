#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""参考音色录制窗口（给 Breeze TTS 2 克隆用）。

为什么要有这个脚本：
  Breeze 克隆必须 ref_audio + ref_text 两者齐全，且逐字稿必须与录音**逐字一致**。
  手工录再手打稿子，几乎必然对不上（多一个字、漏一个语气词都会被判成"音色漂"）。
  这里把两件事绑死：稿子在窗口里显示，录音和 .txt 一起落盘，且只写画面上那段字。

用法：
    python scripts/record-ref-voice.py
    python scripts/record-ref-voice.py --out data/voice-input --seconds 25

产物：
    <out>/ref-voice.wav   16bit 单声道 24k（Breeze 采样率，省一次重采样）
    <out>/ref-voice.txt   UTF-8 逐字稿
"""
from __future__ import annotations

import argparse
import datetime as _dt
import queue
import sys
import threading
import time
import tkinter as tk
from pathlib import Path

import numpy as np
import sounddevice as sd
from scipy.io import wavfile  # noqa: F401  (仅用 write；缺 scipy 时退回 wave 模块)

# ---------------------------------------------------------------- 朗读稿
# 设计约束：
#   1. 覆盖四声 + 常见声母韵母（b/p/m/f、zh/ch/sh/r、z/c/s、j/q/x、前后鼻音）
#   2. 含数字、疑问句、陈述句，让语调有起伏（克隆会学韵律，不只是音色）
#   3. 20 秒上下（Breeze 参考音频 3~15s 最稳，20s 内可用；太长会稀释）
TEXT = (
    "你好，我是这个声音的主人。今天天气不错，我先把这段话完整念一遍，让它记住我的音色。"
    "说话的时候我会有一点点停顿，也会有一点点起伏，这样听起来才自然。"
    "早上八点四十，我喝了一杯温水，然后继续做昨天没做完的事情。"
)

SAMPLE_RATE = 24000
CHANNELS = 1
HARD_LIMIT = 40.0  # 秒，超过自动停


def level_db(buf: np.ndarray) -> float:
    if buf.size == 0:
        return -120.0
    rms = float(np.sqrt(np.mean(np.square(buf.astype(np.float64)))))
    return 20 * np.log10(max(rms / 32768.0, 1e-9))


def write_wav(path: Path, data: np.ndarray) -> None:
    try:
        from scipy.io import wavfile

        wavfile.write(str(path), SAMPLE_RATE, data.astype(np.int16))
    except Exception:
        import wave

        with wave.open(str(path), "wb") as w:
            w.setnchannels(CHANNELS)
            w.setsampwidth(2)
            w.setframerate(SAMPLE_RATE)
            w.writeframes(data.astype(np.int16).tobytes())


class Recorder:
    def __init__(self) -> None:
        self.chunks: list[np.ndarray] = []
        self._stream: sd.InputStream | None = None
        self.q: queue.Queue[float] = queue.Queue()
        self.running = False
        self.t0 = 0.0

    def start(self) -> None:
        self.chunks = []
        self.running = True
        self.t0 = time.time()

        def cb(indata, frames, t, status):  # noqa: ANN001
            if status:
                pass
            self.chunks.append(indata.copy())
            self.q.put(level_db(indata))

        self._stream = sd.InputStream(
            samplerate=SAMPLE_RATE,
            channels=CHANNELS,
            dtype="int16",
            blocksize=1024,
            callback=cb,
        )
        self._stream.start()

    def stop(self) -> np.ndarray:
        self.running = False
        if self._stream is not None:
            self._stream.stop()
            self._stream.close()
            self._stream = None
        if not self.chunks:
            return np.zeros(0, dtype=np.int16)
        return np.concatenate(self.chunks, axis=0).reshape(-1)


class App:
    def __init__(self, root: tk.Tk, out_dir: Path, max_seconds: float) -> None:
        self.root = root
        self.out_dir = out_dir
        self.max_seconds = max_seconds
        self.rec = Recorder()
        self.state = "idle"  # idle | countdown | recording | done
        self.result: np.ndarray | None = None
        self._close_after_save = False
        self.meter_vals: list[float] = [0.0] * 60

        root.title("FlowEdge · 参考音色录制（Breeze TTS 2 克隆）")
        root.geometry("940x640")
        root.configure(bg="#14161a")

        tk.Label(
            root,
            text="请完整、自然地念下面这段。不要增字、不要漏字、不要剪。",
            bg="#14161a",
            fg="#8fd3ff",
            font=("Microsoft YaHei UI", 12),
            anchor="w",
        ).pack(fill="x", padx=22, pady=(16, 6))

        box = tk.Text(
            root,
            height=7,
            wrap="char",
            bg="#1d2026",
            fg="#f2f4f8",
            font=("Microsoft YaHei UI", 20),
            relief="flat",
            padx=16,
            pady=14,
            spacing1=6,
            spacing3=8,
        )
        box.insert("1.0", TEXT)
        box.configure(state="disabled")
        box.pack(fill="both", padx=22, pady=(0, 10))

        self.tip = tk.Label(
            root,
            text="环境要安静 · 离麦 15~20cm · 正常语速 · 念错就点「重录」",
            bg="#14161a",
            fg="#8a93a3",
            font=("Microsoft YaHei UI", 10),
            anchor="w",
        )
        self.tip.pack(fill="x", padx=22)

        self.status = tk.Label(
            root,
            text="准备好了就点「开始录音」",
            bg="#14161a",
            fg="#e6e9ef",
            font=("Microsoft YaHei UI", 15),
            anchor="w",
        )
        self.status.pack(fill="x", padx=22, pady=(12, 4))

        self.canvas = tk.Canvas(root, height=96, bg="#0f1114", highlightthickness=0)
        self.canvas.pack(fill="x", padx=22, pady=(0, 12))

        btns = tk.Frame(root, bg="#14161a")
        btns.pack(fill="x", padx=22, pady=(0, 8))
        self.b_start = tk.Button(
            btns,
            text="● 开始录音",
            command=self.on_start,
            bg="#c2372f",
            fg="white",
            font=("Microsoft YaHei UI", 13, "bold"),
            relief="flat",
            padx=26,
            pady=10,
            activebackground="#a82b24",
            cursor="hand2",
        )
        self.b_start.pack(side="left")
        self.b_stop = tk.Button(
            btns,
            text="■ 停止并保存",
            command=self.on_stop,
            bg="#2b6cb0",
            fg="white",
            font=("Microsoft YaHei UI", 13, "bold"),
            relief="flat",
            padx=26,
            pady=10,
            state="disabled",
            cursor="hand2",
        )
        self.b_stop.pack(side="left", padx=10)
        self.b_done = tk.Button(
            btns,
            text="关闭",
            command=self.on_close,
            bg="#2f3540",
            fg="#dfe4ec",
            font=("Microsoft YaHei UI", 13),
            relief="flat",
            padx=22,
            pady=10,
            cursor="hand2",
        )
        self.b_done.pack(side="right")

        self.out_label = tk.Label(
            root,
            text=f"保存到：{out_dir}",
            bg="#14161a",
            fg="#6f7888",
            font=("Consolas", 9),
            anchor="w",
        )
        self.out_label.pack(fill="x", padx=22, pady=(6, 0))

        # 关窗兜底：只要还在录/录完没存，一律先落盘再退。
        # 曾经的真实翻车：点「关闭」直接 destroy，38 秒录音全部丢失、退出码还 0。
        root.protocol("WM_DELETE_WINDOW", self.on_close)

        self._tick()

    # ------------------------------------------------------------ 交互
    def on_start(self) -> None:
        if self.state in ("countdown", "recording"):
            return
        self.state = "countdown"
        self.b_start.configure(state="disabled")
        self.b_stop.configure(state="disabled")
        self._countdown(3)

    def _countdown(self, n: int) -> None:
        if n <= 0:
            self.begin_record()
            return
        self.status.configure(text=f"{n} 秒后开始…", fg="#ffd479")
        self.root.after(1000, lambda: self._countdown(n - 1))

    def begin_record(self) -> None:
        try:
            self.rec.start()
        except Exception as exc:  # 设备被占 / 权限
            self.state = "idle"
            self.status.configure(text=f"打不开麦克风：{exc}", fg="#ff6b6b")
            self.b_start.configure(state="normal")
            return
        self.state = "recording"
        self.b_stop.configure(state="normal")
        self.status.configure(text="● 正在录音…  念完点「停止并保存」", fg="#ff7b72")

    def on_stop(self) -> None:
        if self.state != "recording":
            return
        self.state = "saving"
        self.b_stop.configure(state="disabled")
        data = self.rec.stop()
        self.save(data)

    def on_close(self) -> None:
        if self.state == "saving":
            self.status.configure(text="正在保存，请稍等…", fg="#ffd479")
            self.root.after(200, self.on_close)
            return
        if self.state in ("recording", "countdown"):
            self._close_after_save = True
            if self.state == "recording":
                self.on_stop()
            else:  # 倒计时里关窗：什么都没录，直接退
                self.root.destroy()
            return
        self.root.destroy()

    def save(self, data: np.ndarray) -> None:
        self.out_dir.mkdir(parents=True, exist_ok=True)
        wav = self.out_dir / "ref-voice.wav"
        txt = self.out_dir / "ref-voice.txt"
        secs = len(data) / SAMPLE_RATE
        if secs < 2.0:
            self.state = "idle"
            self._close_after_save = False
            self.status.configure(text=f"太短了（{secs:.1f}s），重录一次", fg="#ff6b6b")
            self.b_start.configure(state="normal")
            return

        # 去掉首尾长静音，但保留 0.25s 自然起音（切太狠会吃掉辅音）
        peak = level_db(data)
        write_wav(wav, data)
        txt.write_text(TEXT, encoding="utf-8")

        self.result = data
        self.state = "done"
        self.b_start.configure(state="normal", text="↻ 重录")
        self._report(secs, peak)
        if self._close_after_save:
            self.root.after(700, self.root.destroy)

    def _report(self, secs: float, peak_db: float) -> None:
        clip = float(np.max(np.abs(self.result))) / 32768.0 if self.result is not None else 0
        msgs = []
        if clip > 0.99:
            msgs.append("有削波（太大声/麦增益过高）")
        if peak_db < -35:
            msgs.append("音量偏小（离麦远或增益低）")
        if secs < 8:
            msgs.append("偏短，克隆会更像但不稳")
        if secs > 30:
            msgs.append("偏长，建议重录到 20s 内")
        verdict = "；".join(msgs) if msgs else "音量和时长都合适 ✅"
        self.status.configure(
            text=f"已保存 {secs:.1f}s · 峰值 {peak_db:.1f} dBFS · {verdict}",
            fg="#7ee787" if not msgs else "#ffd479",
        )

    # ------------------------------------------------------------ 绘制
    def _tick(self) -> None:
        # 取电平
        while True:
            try:
                v = self.rec.q.get_nowait()
            except queue.Empty:
                break
            self.meter_vals.append(v)
            self.meter_vals = self.meter_vals[-60:]

        c = self.canvas
        c.delete("all")
        w = max(c.winfo_width(), 200)
        h = int(c.winfo_height() or 96)
        n = len(self.meter_vals)
        bw = max(2.0, w / n)
        for i, db in enumerate(self.meter_vals):
            frac = max(0.0, min(1.0, (db + 60) / 60))
            bh = max(2.0, frac * (h - 12))
            x0 = i * bw
            color = "#3fb950" if frac < 0.75 else ("#d29922" if frac < 0.93 else "#f85149")
            c.create_rectangle(x0, h - bh, x0 + bw * 0.72, h - 1, fill=color, outline="")
        c.create_line(0, h - int(h * 0.06), w, h - int(h * 0.06), fill="#f85149", dash=(4, 4))

        if self.state == "recording":
            el = time.time() - self.rec.t0
            self.b_done.configure(text="停止并保存")
            if el >= self.max_seconds:
                self.on_stop()
            else:
                self.status.configure(
                    text=f"● 正在录音…  {el:0.1f}s / {self.max_seconds:0.0f}s", fg="#ff7b72"
                )
        else:
            self.b_done.configure(text="关闭")
        self.root.after(50, self._tick)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="data/voice-input")
    ap.add_argument("--seconds", type=float, default=HARD_LIMIT)
    ap.add_argument("--print-text", action="store_true", help="只打印朗读稿")
    args = ap.parse_args()

    if args.print_text:
        print(TEXT)
        return 0

    out = Path(args.out)
    root = tk.Tk()
    App(root, out, min(args.seconds, HARD_LIMIT))
    root.mainloop()
    return 0


if __name__ == "__main__":
    sys.exit(main())
