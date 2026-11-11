#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""YuE2 按谱渲染：ABC 谱 + style → 成品音频。

为什么用它：ACE-Step 对「具体和弦进行」控制力很弱（实测写死 Fm-Db-Eb，
模型自己换成了 i-iv，见 docs/L4读谱-SheetSage2-接入.md §6.3）。
YuE2 支持 symbolic planning——ABC 谱里的和弦是**显式控制项**，
渲染时和声由谱决定，不再由模型猜。

闭环：
    源曲 ──SheetSage2(hear-score.py)──> score.abc ──(改和弦)──> YuE2 渲染

用法：
    python scripts/yue2-render.py --abc .tmp/score-ref-abc/score.abc \
        --style "Ambient cinematic R&B, warm Rhodes..." --out .tmp/yue2-out

    # 纯器乐（不上传歌词）
    # 有歌词：--lyrics lyrics.txt

依赖：D:/AI/envs/yue2（yue2-infer + torch 2.5.1 cu121）
权重：D:/AI/Models/YuE2-3B（6.9GB model.safetensors + 27 个小文件）
"""
from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path


def main() -> int:
    ap = argparse.ArgumentParser(description="YuE2 按谱渲染")
    ap.add_argument("--abc", required=True, help="ABC 谱文件路径")
    ap.add_argument("--style", default="", help="风格提示词（英文）")
    ap.add_argument("--style-file", default="", help="从 UTF-8 文件读风格提示词（长文本推荐）")
    ap.add_argument("--lyrics", default="", help="歌词文件；留空=纯器乐")
    ap.add_argument("--cot", default="full", choices=["full", "melody", "off"],
                    help="full=旋律+和弦规划（给 abc 时用）；melody=仅旋律（cover）；off=无规划")
    ap.add_argument("--seed", type=int, default=831001)
    ap.add_argument("--out", default=".tmp/yue2-out")
    ap.add_argument("--model", default=r"D:\AI\Models\YuE2-3B")
    ap.add_argument("--backend", default="torch-eager", choices=["torch", "torch-eager", "vllm"],
                    help="torch=CUDA graph（需 build 启用 flash-attn；Windows 版 torch 未启用，会报错）")
    ap.add_argument("--nar-chunk", type=int, default=512,
                    help="NAR attention 分块。默认 None 会整段算 → 长曲 OOM；512 大幅省显存")
    ap.add_argument("--no-artifacts", action="store_true")
    a = ap.parse_args()

    from yue2 import YuE2Pipeline

    # --- 分块 attention 补丁 ---------------------------------------------
    # yue2/pipeline.py:300 调 synthesize() 时不传 query_chunk_size，
    # 而 nar.py:70 的 block = query_chunk_size or len(q) ⇒ CUDA 上整段不分块。
    # 263s 长曲实测 OOM（tried to allocate 4.96 GiB / only 2.50 GiB free）。
    # 这里把 CachedNAR 默认分块改成 --nar-chunk。
    import yue2.nar as _nar

    if a.nar_chunk and a.nar_chunk > 0:
        _orig_init = _nar.CachedNAR.__init__

        def _patched_init(self, model, chunk, attention="sdpa", query_chunk_size=None):
            _orig_init(self, model, chunk, attention, query_chunk_size or a.nar_chunk)

        _nar.CachedNAR.__init__ = _patched_init
        print(f"[0] nar attention chunk = {a.nar_chunk} (patched)", flush=True)

    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)

    abc = Path(a.abc).read_text(encoding="utf-8")
    lyrics = Path(a.lyrics).read_text(encoding="utf-8") if a.lyrics else ""
    style = Path(a.style_file).read_text(encoding="utf-8").strip() if a.style_file else a.style.strip()
    if not style:
        raise SystemExit("需要 --style 或 --style-file")
    print(f"[1] abc={len(abc)} chars  lyrics={len(lyrics)} chars  style={len(style)} chars  cot={a.cot}", flush=True)

    t0 = time.time()
    print(f"[2] loading YuE2 from {a.model} (backend={a.backend}) ...", flush=True)
    pipe = YuE2Pipeline.from_pretrained(a.model, backend=a.backend)
    print(f"[3] pipeline ready in {time.time()-t0:.1f}s", flush=True)

    t1 = time.time()
    print("[4] generating (this can take 1-3 min on 3090) ...", flush=True)
    song = pipe(style=style, lyrics=lyrics, abc=abc, cot=a.cot, seed=a.seed)

    dst = out / "song.flac"
    song.save(str(dst))
    print(f"[5] audio saved in {time.time()-t1:.1f}s -> {dst} ({dst.stat().st_size/1e6:.1f} MB)", flush=True)

    if not a.no_artifacts:
        try:
            song.save_artifacts(str(out / "artifacts"))
            print("[6] artifacts saved (ABC/tokens/latents)", flush=True)
        except Exception as e:
            print(f"[6] artifacts failed: {type(e).__name__}: {e}", flush=True)

    try:
        pipe.close()
    except Exception:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
