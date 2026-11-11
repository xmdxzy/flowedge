"""渲染保温杯 4 部件的爆炸动画深度序列（正交投影 z-buffer）。
输出: .tmp/explode/depth/f000.png ... f123.png
用途: 作为 MiniMax H3 Fun ControlNet 的 depth 控制视频。
"""
import os
import numpy as np
from PIL import Image

W, H = 352, 608
N = 124
OUT = ".tmp/explode/depth"
os.makedirs(OUT, exist_ok=True)


def tube(r0, r1, y0, y1, n=768, m=140):
    th = np.linspace(0, 2 * np.pi, n, endpoint=False)
    ys = np.linspace(y0, y1, m)
    T, Y = np.meshgrid(th, ys)
    R = np.linspace(r0, r1, m)[:, None] * np.ones_like(T)
    return (R * np.cos(T)).ravel(), Y.ravel(), (R * np.sin(T)).ravel()


def disk(r, y, n=512, m=40):
    th = np.linspace(0, 2 * np.pi, n, endpoint=False)
    rr = np.linspace(0, r, m)
    T, RR = np.meshgrid(th, rr)
    return (RR * np.cos(T)).ravel(), np.full(RR.size, y), (RR * np.sin(T)).ravel()


def merge(*parts):
    return (np.concatenate([p[0] for p in parts]),
            np.concatenate([p[1] for p in parts]),
            np.concatenate([p[2] for p in parts]))


def part_points(kind, t):
    if kind == "lid":
        r, h, cy = 0.335, 0.085, 0.80
        off = 0.20 * t
        y0, y1 = cy - h / 2 + off, cy + h / 2 + off
        return merge(tube(r, r, y0, y1), disk(r, y1), disk(r, y0))
    if kind == "ring":
        r, h, cy = 0.345, 0.014, 0.735
        off = 0.13 * t
        y0, y1 = cy - h / 2 + off, cy + h / 2 + off
        return merge(tube(r, r, y0, y1))
    if kind == "body":
        r, h, cy = 0.315, 0.40, 0.44
        y0, y1 = cy - h / 2, cy + h / 2
        return merge(tube(r, r, y0, y1), disk(r, y0))
    # cup: 锥台，上宽下窄
    r0, r1, h, cy = 0.295, 0.225, 0.28, 0.14
    off = -0.16 * t
    y0, y1 = cy - h / 2 + off, cy + h / 2 + off
    return merge(tube(r0, r1, y0, y1), disk(r0, y1), disk(r1, y0))


def render(t):
    """逐像素解析求深度：圆柱/锥台侧面 z = sqrt(r^2 - x^2)。无采样条纹。"""
    xs = (np.arange(W) - W / 2) / W
    ys = ((H - np.arange(H)) - H * 0.04) / (H * 0.92)
    X, Y = np.meshgrid(xs, ys)
    depth = np.full((H, W), -1.0, dtype=np.float32)

    parts = [
        (0.335, 0.335, 0.80 - 0.035 + 0.16 * t, 0.80 + 0.035 + 0.16 * t),
        (0.345, 0.345, 0.735 - 0.014 + 0.10 * t, 0.735 + 0.014 + 0.10 * t),
        (0.315, 0.315, 0.44 - 0.18, 0.44 + 0.18),
        (0.295, 0.225, 0.13 - 0.12 - 0.08 * t, 0.13 + 0.12 - 0.08 * t),
    ]
    for r0, r1, y0, y1 in parts:
        inside = (Y >= y0) & (Y <= y1)
        r = r0 + (r1 - r0) * np.clip((Y - y0) / max(y1 - y0, 1e-6), 0, 1)
        zz = np.sqrt(np.maximum(r ** 2 - X ** 2, 0.0))
        ok = inside & (np.abs(X) <= r)
        depth[ok] = np.maximum(depth[ok], zz[ok])

    img = np.clip(depth, 0, 0.5) / 0.5
    return (img * 255).astype(np.uint8)


for i in range(N):
    t = i / (N - 1)
    # 缓动：前 30% 保持合体，后 70% 分离
    e = 0.0 if t < 0.30 else ((t - 0.30) / 0.70) ** 1.6
    Image.fromarray(render(e)).save(os.path.join(OUT, f"f{i:03d}.png"))
print("done", N, "frames")
