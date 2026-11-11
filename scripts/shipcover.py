#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""shipcover.py — 封面三尺寸出图 + 三门 + 居中量测

尺寸（照 00-接续总纲）：9:16 1080×1920 · 4:3 1440×1080 · 16:9 1920×1080 · chat ≤720×1280 <150KB
判据：主视觉/标题水平居中偏差 ≤2% 画宽；文字落在各自安全区；封面也跑 collision/clip/crop。
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

try:
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

# 尺寸 -> (安全区 x0,y0,x1,y1)
SIZES = [
    ("cover",        1080, 1920, (150, 150, 930, 1700)),      # 抖音竖封面
    ("cover-h-4x3",  1440, 1080, (202, 86, 1238, 994)),       # 抖音横封面（左右≥14%、上下≥8%）
    ("cover-h-16x9", 1920, 1080, (269, 86, 1651, 994)),       # TikTok/其他
]
CHAT = (720, 1280)


def main() -> int:
    ap = argparse.ArgumentParser(description="封面三尺寸出图")
    ap.add_argument("project")
    ap.add_argument("--out", required=True)
    ap.add_argument("--quality", type=int, default=88)
    args = ap.parse_args()

    proj = Path(args.project).resolve()
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)

    try:
        from playwright.sync_api import sync_playwright
    except Exception as e:
        print(f"ERROR 缺 playwright：{e}")
        return 2

    fail = 0
    with sync_playwright() as pw:
        browser = pw.chromium.launch(args=["--force-color-profile=srgb", "--hide-scrollbars"])
        for name, w, h, safe in SIZES:
            page = browser.new_page(viewport={"width": w, "height": h}, device_scale_factor=1)
            page.goto((proj / "cover.html").as_uri())
            info = page.evaluate("c => window.__cover(c)", {"w": w, "h": h})
            boxes = page.evaluate("() => window.__boxes()")
            png = out / f"{name}.png"
            page.screenshot(path=str(png), animations="disabled")
            page.close()

            jpg = out / f"{name}.jpg"
            from PIL import Image
            Image.open(png).convert("RGB").save(jpg, "JPEG", quality=args.quality)
            png.unlink(missing_ok=True)

            bl = out / f"boxes_{name}.jsonl"
            bl.write_text("\n".join(json.dumps(b, ensure_ascii=False) for b in boxes), encoding="utf-8")

            off = info["heroCenterOff"] / w * 100
            print(f"{name:14s} {w}×{h}  {jpg.stat().st_size/1024:.0f}KB  "
                  f"主视觉偏移 {info['heroCenterOff']:.1f}px ({off:.2f}%)")
            if off > 2.0:
                print(f"   FAIL 主视觉居中偏差 {off:.2f}% > 2%")
                fail += 1
            r = subprocess.run([sys.executable, str(Path(__file__).with_name("shipgate.py")), "boxes", str(bl),
                                "--safe", ",".join(str(v) for v in safe),
                                "--title-max-y", str(int(safe[1] + (safe[3] - safe[1]) * 0.68))],
                               capture_output=True, text=True, encoding="utf-8")
            tail = [ln for ln in r.stdout.splitlines() if ln.startswith(("---", "GATE=", "汇总"))]
            print("   " + " | ".join(tail[-7:]))
            if r.returncode != 0:
                print("   " + "\n   ".join(ln for ln in r.stdout.splitlines() if ln.startswith("   t="))[:600])
                fail += 1

        # chat 版：从竖封面等比缩到 ≤720×1280，<150KB
        from PIL import Image
        src = out / "cover.jpg"
        if src.exists():
            im = Image.open(src).convert("RGB")
            im.thumbnail(CHAT, Image.LANCZOS)
            q = 82
            dst = out / "cover-chat.jpg"
            while q >= 45:
                im.save(dst, "JPEG", quality=q, optimize=True)
                if dst.stat().st_size < 150 * 1024:
                    break
                q -= 6
            print(f"{'cover-chat':14s} {im.width}×{im.height}  {dst.stat().st_size/1024:.0f}KB  q={q}"
                  f"  {'OK' if dst.stat().st_size < 150*1024 else 'FAIL >150KB'}")
            if dst.stat().st_size >= 150 * 1024:
                fail += 1
        browser.close()

    print(f"\n封面 {'FAIL' if fail else 'PASS'}（{fail} 项未过）")
    return 1 if fail else 0


if __name__ == "__main__":
    sys.exit(main())
