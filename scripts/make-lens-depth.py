"""相机镜头 6 部件爆炸动画深度序列（球面镜片 + 空心镜筒 + 环）。
正交投影，逐像素解析求深度。X/Y 同尺度（以画面高为单位），圆不变形。
输出 .tmp/lens/depth/f000.png ...
"""
import os
import numpy as np
from PIL import Image

W, H = 352, 608
N = 124
OUT = ".tmp/lens/depth"
os.makedirs(OUT, exist_ok=True)

# 类型, 参数, y中心, 爆炸位移
# ring/tube: (r_out, r_in, h)；lens: (r, curvature)
PARTS = [
    ("ring",   (0.190, 0.160, 0.026), 0.050, 0.000),
    ("tube",   (0.180, 0.000, 0.180), 0.190, 0.000),
    ("lens_c", (0.125, 0.20),         0.320, 0.000),
    ("ring",   (0.160, 0.095, 0.022), 0.440, 0.000),
    ("lens_v", (0.120, 0.18),         0.560, 0.000),
    ("lens_c", (0.130, 0.18),         0.680, 0.000),
]
GAP = [0.000, 0.025, 0.050, 0.075, 0.100, 0.125]


def render(t):
    xs = (np.arange(W) - W / 2) / H          # 以画面高为单位，与 Y 同尺度
    ys = ((H - np.arange(H)) - H * 0.03) / (H * 0.94)
    X, Y = np.meshgrid(xs, ys)
    depth = np.full((H, W), -1.0, dtype=np.float32)

    for idx, (kind, p, cy, _) in enumerate(PARTS):
        yc = cy + GAP[idx] * t
        if kind in ("ring", "tube"):
            r_out, r_in, h = p
            y0, y1 = yc - h / 2, yc + h / 2
            inside = (Y >= y0) & (Y <= y1)
            ax = np.abs(X)
            zz = np.sqrt(np.maximum(r_out ** 2 - X ** 2, 0))
            ok = inside & (ax <= r_out) & (ax >= r_in)
            depth[ok] = np.maximum(depth[ok], zz[ok])
        else:
            r_lens, curv = p
            rr = np.sqrt(X ** 2 + (Y - yc) ** 2)
            edge = curv - np.sqrt(max(curv ** 2 - r_lens ** 2, 0))
            sag = curv - np.sqrt(np.maximum(curv ** 2 - rr ** 2, 0)) - edge
            zz = sag if kind == "lens_c" else -sag
            ok = rr <= r_lens
            depth[ok] = np.maximum(depth[ok], zz[ok])

    img = np.clip(0.25 + depth / 0.40, 0, 1.0)
    return (img * 255).astype(np.uint8)


for i in range(N):
    t = i / (N - 1)
    e = 0.0 if t < 0.25 else ((t - 0.25) / 0.75) ** 1.5
    Image.fromarray(render(e)).save(os.path.join(OUT, f"f{i:03d}.png"))
print("done", N)
