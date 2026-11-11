#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""fetch-stock.py — 素材获取与筛查（免 key 优先）

背景：Pexels 自 2026-10 起停发新 key（官方页原文 "New API key issuance is currently
paused and cannot be requested."），产线不能挂在它上面。这里接不需要审批的源：

  wikimedia  Wikimedia Commons（PD/CC，含 720p/1080p 转码）  免 key
  archive    Internet Archive（大量公共领域影像）             免 key
  nasa       NASA Image Library（科学影像）                   免 key
  pixabay    Pixabay（注册即给 key，无审批）                   需 PIXABAY_API_KEY

铁律：**标题会说谎**。先 `--sheet` 抽帧条目检，再决定下载哪一条；
带烧录文字 / 台标 / logo 的素材一律不用。每条成品都落 `<file>.license.json`。

用法：
  python scripts/fetch-stock.py search "ice skating" --provider wikimedia --limit 6
  python scripts/fetch-stock.py search "ice" --provider wikimedia --sheet
  python scripts/fetch-stock.py get "ice skating" --provider wikimedia --pick 1 --out assets/stock/skate
  python scripts/fetch-stock.py get "ice" --provider wikimedia --pick 1 --out assets/stock/x --grade
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import urllib.parse
import urllib.request
from pathlib import Path

try:
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

UA = {"User-Agent": "flowedge-stock/1.0 (local research)"}
FFMPEG = shutil.which("ffmpeg") or "ffmpeg"
FFPROBE = shutil.which("ffprobe") or "ffprobe"

# 暗场电光青通用调色（无 bloom，避免 blend 造成横带）
GRADE = ("eq=contrast=1.12:brightness=-0.06:saturation=0.52,"
         "colorbalance=rs=-0.05:bs=0.12:rh=-0.06:bh=0.10,"
         "vignette=angle=PI/5")


def http_json(url, headers=None):
    req = urllib.request.Request(url, headers={**UA, **(headers or {})})
    with urllib.request.urlopen(req, timeout=45) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


def http_download(url, dest, headers=None):
    req = urllib.request.Request(url, headers={**UA, **(headers or {})})
    dest = Path(dest)
    dest.parent.mkdir(parents=True, exist_ok=True)
    with urllib.request.urlopen(req, timeout=300) as r, open(dest, "wb") as f:
        shutil.copyfileobj(r, f, 1 << 20)
    return dest


def probe(path):
    r = subprocess.run([FFPROBE, "-v", "error", "-show_entries",
                        "stream=width,height,r_frame_rate:format=duration,size",
                        "-of", "json", str(path)],
                       capture_output=True, text=True, encoding="utf-8", errors="replace")
    try:
        j = json.loads(r.stdout)
        st = (j.get("streams") or [{}])[0]
        fmt = j.get("format", {})
        return {"w": st.get("width"), "h": st.get("height"), "fps": st.get("r_frame_rate"),
                "dur": round(float(fmt.get("duration", 0) or 0), 2),
                "mb": round(float(fmt.get("size", 0) or 0) / 1048576, 2)}
    except Exception:
        return {}


# ---------------- providers ----------------
def wm_search(q, limit):
    api = ("https://commons.wikimedia.org/w/api.php?action=query&format=json&generator=search"
           "&gsrsearch=" + urllib.parse.quote("filetype:video " + q) +
           "&gsrnamespace=6&gsrlimit=%d&prop=imageinfo&iiprop=url|size|mime|extmetadata" % limit)
    data = http_json(api)
    out = []
    for page in (data.get("query", {}).get("pages", {}) or {}).values():
        ii = (page.get("imageinfo") or [{}])[0]
        if not str(ii.get("mime", "")).startswith("video/"):
            continue
        meta = ii.get("extmetadata", {}) or {}

        def g(k):
            v = meta.get(k, {}).get("value", "")
            return v if isinstance(v, str) else str(v)

        out.append({
            "provider": "wikimedia", "title": page.get("title", ""),
            "url": ii.get("url", ""), "mime": ii.get("mime", ""),
            "w": ii.get("width"), "h": ii.get("height"), "bytes": ii.get("size"),
            "license": g("LicenseShortName"), "artist": g("Artist"), "credit": g("Credit"),
            "page": "https://commons.wikimedia.org/wiki/" + urllib.parse.quote(
                str(page.get("title", "")).replace(" ", "_")),
        })
    return out


def ia_search(q, limit):
    fl = "&".join("fl[]=" + k for k in ("identifier", "title", "licenseurl", "publicdate"))
    url = ("https://archive.org/advancedsearch.php?q=" +
           urllib.parse.quote(q + " AND mediatype:movies") + "&" + fl +
           "&rows=%d&page=1&output=json" % limit)
    data = http_json(url)
    out = []
    for d in data.get("response", {}).get("docs", []):
        out.append({
            "provider": "archive", "title": d.get("title", ""),
            "identifier": d.get("identifier", ""), "url": "",
            "license": d.get("licenseurl", "") or "见详情页（多为公共领域）",
            "w": None, "h": None, "bytes": None,
            "page": "https://archive.org/details/" + str(d.get("identifier", "")),
        })
    return out


def ia_resolve(identifier):
    meta = http_json("https://archive.org/metadata/" + urllib.parse.quote(identifier))
    best = None
    for f in meta.get("files", []):
        name = f.get("name", "")
        if not name.lower().endswith((".mp4", ".webm", ".mov")):
            continue
        size = int(f.get("size", 0) or 0)
        if best is None or size > best[1]:
            best = (name, size)
    if not best:
        return None
    return "https://archive.org/download/%s/%s" % (
        urllib.parse.quote(identifier), urllib.parse.quote(best[0]))


def nasa_search(q, limit):
    data = http_json("https://images-api.nasa.gov/search?q=" +
                     urllib.parse.quote(q) + "&media_type=video")
    out = []
    for it in data.get("collection", {}).get("items", [])[:limit]:
        d = (it.get("data") or [{}])[0]
        out.append({
            "provider": "nasa", "title": d.get("title", ""),
            "nasa_id": d.get("nasa_id", ""), "url": "",
            "license": "NASA 公共领域（多数）",
            "w": None, "h": None, "bytes": None,
            "page": "https://images.nasa.gov/details-" + str(d.get("nasa_id", "")),
        })
    return out


def nasa_resolve(nasa_id):
    data = http_json("https://images-api.nasa.gov/asset/" + urllib.parse.quote(nasa_id))
    items = data.get("collection", {}).get("items", [])
    for f in items:
        href = f.get("href", "")
        if href.endswith((".mp4", ".mov")) and ("small" in href or "preview" in href):
            return href
    for f in items:
        href = f.get("href", "")
        if href.endswith((".mp4", ".mov")):
            return href
    return None


def pixabay_search(q, limit):
    key = os.environ.get("PIXABAY_API_KEY", "")
    if not key:
        raise SystemExit("缺 PIXABAY_API_KEY：pixabay.com/api/docs 注册后页面即时显示，无审批")
    data = http_json("https://pixabay.com/api/videos/?key=%s&q=%s&per_page=%d&safesearch=true"
                     % (key, urllib.parse.quote(q), min(limit, 20)))
    out = []
    for h in data.get("hits", []):
        v = (h.get("videos") or {}).get("large") or (h.get("videos") or {}).get("medium") or {}
        out.append({
            "provider": "pixabay", "title": h.get("tags", ""), "url": v.get("url", ""),
            "w": v.get("width"), "h": v.get("height"), "bytes": v.get("size"),
            "license": "Pixabay Content License（可商用）", "artist": h.get("user", ""),
            "page": h.get("pageURL", ""),
        })
    return out


PROVIDERS = {"wikimedia": wm_search, "archive": ia_search, "nasa": nasa_search, "pixabay": pixabay_search}


def resolve_url(it, provider):
    u = it.get("url", "")
    if u:
        return u
    if provider == "archive":
        return ia_resolve(it.get("identifier", ""))
    if provider == "nasa":
        return nasa_resolve(it.get("nasa_id", ""))
    return None


def build_sheet(items, provider, sheet_out):
    tmp = Path(".tmp/stock-probe")
    tmp.mkdir(parents=True, exist_ok=True)
    tiles = []
    for i, it in enumerate(items[:4]):
        u = resolve_url(it, provider)
        if not u:
            continue
        raw = tmp / ("c%d" % i)
        try:
            http_download(u, raw)
        except Exception as e:
            print("[%d] 下载失败 %s" % (i, str(e)[:70]))
            continue
        for j, ss in enumerate((1.0, 3.0)):
            png = tmp / ("c%d_%d.png" % (i, j))
            subprocess.run([FFMPEG, "-y", "-v", "error", "-ss", str(ss), "-i", str(raw),
                            "-frames:v", "1", "-vf",
                            "scale=480:270:force_original_aspect_ratio=increase,crop=480:270",
                            str(png)], capture_output=True)
            if png.exists():
                tiles.append((png, "#%d %s @%.0fs" % (i, it.get("title", "")[:26], ss)))
    if not tiles:
        print("这批候选抽帧全失败，换 provider 或关键词。")
        return 2
    cols = 2
    sheet = Path(sheet_out)
    sheet.parent.mkdir(parents=True, exist_ok=True)
    cmd = [FFMPEG, "-y", "-v", "error"]
    for t, _ in tiles:
        cmd += ["-i", str(t)]
    layout = "|".join("%d_%d" % (480 * (k % cols), 270 * (k // cols)) for k in range(len(tiles)))
    cmd += ["-filter_complex",
            "".join("[%d:v]" % k for k in range(len(tiles))) +
            "xstack=inputs=%d:layout=%s[v]" % (len(tiles), layout),
            "-map", "[v]", str(sheet)]
    r = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace")
    if r.returncode != 0:
        print("拼图失败: " + r.stderr[-400:])
        return 2
    print("抽帧条 %s（%d 格，每格 480x270）" % (sheet, len(tiles)))
    for _, cap in tiles:
        print("  " + cap)
    print("逐格看过再决定下载哪一条；带烧录文字/台标/logo 的一律不用。")
    return 0


def main():
    ap = argparse.ArgumentParser(description="免 key 素材获取（带许可 sidecar）")
    ap.add_argument("action", choices=["search", "get"])
    ap.add_argument("query")
    ap.add_argument("--provider", choices=list(PROVIDERS), default="wikimedia")
    ap.add_argument("--limit", type=int, default=8)
    ap.add_argument("--pick", type=int, default=0)
    ap.add_argument("--out", default="assets/stock")
    ap.add_argument("--sheet", action="store_true", help="候选抽帧条（先目检）")
    ap.add_argument("--sheet-out", default=".tmp/stock-sheet.jpg")
    ap.add_argument("--grade", action="store_true", help="套暗场电光青通用调色")
    ap.add_argument("--no-crop", action="store_true", help="保持原尺寸，不裁 1080p")
    args = ap.parse_args()

    items = PROVIDERS[args.provider](args.query, args.limit)
    if not items:
        print("没有命中。换个关键词或换 provider。")
        return 2

    if args.sheet:
        return build_sheet(items, args.provider, args.sheet_out)

    if args.action == "search":
        for i, it in enumerate(items):
            res = "%sx%s" % (it.get("w"), it.get("h")) if it.get("w") else "?"
            mb = "%.1fMB" % (it["bytes"] / 1048576) if it.get("bytes") else "?"
            print("[%d] %s  %s  %s  %s" % (i, it["provider"], res, mb, str(it.get("license", "?"))[:34]))
            print("    " + str(it.get("title", ""))[:88])
        print("")
        print("下载: get \"<query>\" --provider <p> --pick <i> --out <dir>")
        return 0

    it = items[min(args.pick, len(items) - 1)]
    url = resolve_url(it, args.provider)
    if not url:
        print("这条没有直连文件，换一条。")
        return 2

    outdir = Path(args.out)
    name = urllib.parse.urlparse(url).path.rsplit("/", 1)[-1] or "clip.mp4"
    raw = outdir / "raw" / name
    print("下载 " + url[:100])
    http_download(url, raw)
    print("原始 %s %s" % (raw.name, probe(raw)))

    final = outdir / "clip.mp4"
    if args.no_crop:
        shutil.copyfile(raw, final)
    else:
        vf = "scale=1920:1080:force_original_aspect_ratio=increase,crop=1920:1080"
        if args.grade:
            vf += "," + GRADE
        r = subprocess.run([FFMPEG, "-y", "-v", "error", "-i", str(raw),
                            "-vf", vf, "-r", "30", "-c:v", "libx264", "-crf", "18",
                            "-preset", "medium", "-pix_fmt", "yuv420p", "-an", str(final)],
                           capture_output=True, text=True, encoding="utf-8", errors="replace")
        if r.returncode != 0:
            print("转码失败: " + r.stderr[-600:])
            return 2

    side = final.with_suffix(".license.json")
    side.write_text(json.dumps({**it, "source_url": url, "final": str(final),
                                "graded": bool(args.grade), "final_info": probe(final)},
                               ensure_ascii=False, indent=1), encoding="utf-8")
    print("成品 %s %s" % (final, probe(final)))
    print("许可 %s · %s" % (it.get("license", "?"), str(it.get("artist", ""))[:60]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
