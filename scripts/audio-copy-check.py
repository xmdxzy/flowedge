#!/usr/bin/env python3
"""判断成片音轨是不是「参考音频原样复用」。

为什么不用简单相减：fully_copy 也可能有毫秒级偏移（AAC 编码延迟、音轨起始对齐），
直接相减会把「同一段声音但错了几十毫秒」判成完全不同。这里改比**包络**（10ms 能量的
滑动平均）并允许整体平移，抗偏移、抗编码噪声。

用法：python audio_corr.py <成片.mp4> <参考.wav>
输出：peak_corr（-1~1）、lag_ms、两段包络的时长
判据：peak_corr > 0.8 视为同一段声音（原样复用）；< 0.4 视为模型自己生成的另一段声音。
"""
import subprocess
import sys

import numpy as np

SR = 8000


def pcm(path: str) -> np.ndarray:
    out = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", path, "-ac", "1", "-ar", str(SR),
         "-f", "f32le", "-"],
        capture_output=True, check=True,
    ).stdout
    return np.frombuffer(out, dtype=np.float32)


def envelope(x: np.ndarray, win: int = 80) -> np.ndarray:
    # 80 采样 @8kHz = 10ms 窗口
    a = np.abs(x)
    k = np.ones(win, dtype=np.float32) / win
    return np.convolve(a, k, mode="same")


def best_lag_corr(a: np.ndarray, b: np.ndarray, max_lag: int = 4000):
    # 用 FFT 做互相关，再归一化成相关系数
    n = len(a) + len(b)
    size = 1 << (n - 1).bit_length()
    fa = np.fft.rfft(a - a.mean(), size)
    fb = np.fft.rfft(b - b.mean(), size)
    cc = np.fft.irfft(fa * np.conj(fb), size)
    # 只搜 ±max_lag（±0.5s）
    cand = np.concatenate([cc[-max_lag:], cc[: max_lag + 1]])
    lags = np.concatenate([np.arange(-max_lag, 0), np.arange(0, max_lag + 1)])
    i = int(np.argmax(cand))
    denom = (np.linalg.norm(a - a.mean()) * np.linalg.norm(b - b.mean())) or 1.0
    return float(cand[i] / denom), int(lags[i])


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    out, ref = sys.argv[1], sys.argv[2]
    a, b = pcm(out), pcm(ref)
    ea, eb = envelope(a), envelope(b)
    m = min(len(ea), len(eb))
    corr, lag = best_lag_corr(ea[:m], eb[:m])
    print({
        "file": out,
        "ref": ref,
        "seconds_out": round(len(a) / SR, 3),
        "seconds_ref": round(len(b) / SR, 3),
        "peak_corr": round(corr, 4),
        "lag_ms": round(lag / SR * 1000, 1),
        "verdict": "COPIED（同一段声音）" if corr > 0.8 else ("DIFFERENT（另一段声音）" if corr < 0.4 else "UNCLEAR"),
    })
    return 0


if __name__ == "__main__":
    sys.exit(main())
