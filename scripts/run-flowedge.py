#!/usr/bin/env python3
"""调用 FlowEdge 引擎出片（台词 / 氛围两种模式），供序策图编排。

为什么不让序策图直接怼 ComfyUI
------------------------------
H3 的图构建、互斥切换、显存仲裁、自检都在 FlowEdge 里（单一事实源）。
序策图只负责「何时跑、跑哪种模式、验收过不过」，不重复实现构图逻辑。

用法
----
    # 台词模式：ref2va + 念稿音轨（口型同步、音频 1:1 透传）
    python scripts/run-flowedge.py --mode dialogue --prompt-file p.txt --audio dub.wav --frames 107

    # 氛围模式：fl2va 联合生成，无台词（口型闭合）
    python scripts/run-flowedge.py --mode ambience --prompt-file p.txt --frames 124

    # 自动：给了 --audio 就是台词，否则氛围（序策图里由 mode 节点决定）
    python scripts/run-flowedge.py --mode auto --prompt-file p.txt

输出 JSON：{ok, mode, video, seconds, check_ok, detail}
退出码：0 成功 / 2 失败（供 evalgate 直接判定）
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

ENGINE = os.environ.get("FLOWEDGE_ENGINE", "http://127.0.0.1:8080")

# 本机常驻 http_proxy（企业代理），本地端口必须绕过，否则会拿到 502 假死。
# 这是 2026-10-19 踩过的坑：502 不代表服务挂了，而是请求根本没到本地。
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def api(path: str, method: str = "GET", body=None, timeout: int = 60):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body, ensure_ascii=False).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(ENGINE.rstrip("/") + path, data=data,
                                 headers=headers, method=method)
    with _OPENER.open(req, timeout=timeout) as r:
        raw = r.read().decode("utf-8")
        return json.loads(raw) if raw.strip() else {}


def wait_ready(url: str, seconds: int = 240) -> bool:
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            _OPENER.open(url, timeout=3).read()
            return True
        except Exception:
            time.sleep(5)
    return False


def ensure_h3(need_audio: bool) -> str:
    """确保引擎在、H3 在。（ASR/TTS 由调用方或图里其它节点负责）"""
    try:
        api("/api/capabilities")
    except Exception as e:
        return f"FlowEdge 引擎不可达（{ENGINE}）：{e}"
    try:
        api("/api/deps/comfy_h3/start", method="POST", timeout=30)
    except Exception as e:
        return f"H3 实例启动请求失败：{e}"
    if not wait_ready("http://127.0.0.1:8189/system_stats", 240):
        return "H3 实例 240s 内未就绪（冷启动约 50s，检查显存是否被别的进程占用）"
    return ""


def build_workflow(args, prompt: str) -> dict:
    dialogue = args.mode == "dialogue" or (args.mode == "auto" and args.audio)
    data = {
        "prompt": "",
        "backend": "h3",
        "width": args.width,
        "height": args.height,
        "frames": args.frames,
        "fps": args.fps,
        "steps": args.steps,
        "turbo": args.turbo,
        "prompt_opt": False,
        "segments": 1,
        "interp": False,
    }
    if dialogue:
        if not args.audio or not os.path.isfile(args.audio):
            raise SystemExit("台词模式需要 --audio 指向念稿 wav（先跑 dub-track.py）")
        data["ref_audio"] = os.path.abspath(args.audio)
        if args.images:
            data["ref_images"] = args.images.replace(";", "\n")
    else:
        # 氛围模式：同一份剧本也能切成"无台词版"——把 <d> 台词段剥掉，
        # 换成口型可见事实（cfg=1 无负分支，否定词不管用）。
        import re as _re
        prompt = _re.sub(r"\(S\d+(?:,S\d+)*\)[^<]*<d>.*?</d>", "", prompt, flags=_re.S)
        prompt = _re.sub(r"\s{2,}", " ", prompt).strip()
        if "lips stay closed" not in prompt.lower():
            prompt += " Lips stay closed and still, jaw tight, breath audible through the nose."
    return {
        "name": f"run-flowedge-{int(time.time())}",
        "nodes": [
            {"id": "s", "type": "start", "data": {"prompt": prompt}},
            {"id": "v", "type": "text2video", "data": data},
            {"id": "e", "type": "end", "data": {}},
        ],
        "edges": [{"id": "e1", "source": "s", "target": "v"},
                  {"id": "e2", "source": "v", "target": "e"}],
    }


def main() -> int:
    ap = argparse.ArgumentParser(description="驱动 FlowEdge 出片（台词/氛围）")
    ap.add_argument("--mode", choices=["dialogue", "ambience", "auto"], default="auto")
    ap.add_argument("--prompt")
    ap.add_argument("--prompt-file")
    ap.add_argument("--audio", help="念稿 wav（台词模式必填）")
    ap.add_argument("--images", help="参考图路径，分号分隔（≤9）")
    ap.add_argument("--width", type=int, default=608)
    ap.add_argument("--height", type=int, default=352)
    ap.add_argument("--frames", type=int, default=107, help="必须落在 17k+5；107≈4.46s / 124≈5.17s")
    ap.add_argument("--fps", type=int, default=24)
    ap.add_argument("--steps", type=int, default=12)
    ap.add_argument("--turbo", action="store_true", default=True)
    args = ap.parse_args()

    prompt = args.prompt or ""
    if args.prompt_file:
        with open(args.prompt_file, encoding="utf-8") as f:
            prompt = f.read()
    if not prompt.strip():
        print(json.dumps({"ok": False, "error": "缺少 --prompt 或 --prompt-file"}, ensure_ascii=False))
        return 2
    if (args.frames - 5) % 17 != 0:
        print(json.dumps({"ok": False, "error": f"frames={args.frames} 不在 17k+5 网格上"}, ensure_ascii=False))
        return 2

    err = ensure_h3(need_audio=bool(args.audio))
    if err:
        print(json.dumps({"ok": False, "error": err}, ensure_ascii=False))
        return 2

    try:
        wf = build_workflow(args, prompt)
    except SystemExit as e:
        print(json.dumps({"ok": False, "error": str(e)}, ensure_ascii=False))
        return 2

    mode = "dialogue" if ("ref_audio" in wf["nodes"][1]["data"]) else "ambience"

    try:
        created = api("/api/workflows", method="POST", body=wf)
        wid = created.get("id")
        run = api(f"/api/workflows/{wid}/run", method="POST", body={}, timeout=60)
        rid = run.get("run_id")
    except Exception as e:
        print(json.dumps({"ok": False, "error": f"提交失败：{e}"}, ensure_ascii=False))
        return 2

    # 轮询：H3 冷启 + 12 步 turbo，107 帧约 2~4 分钟
    deadline = time.time() + 1800
    status = "running"
    detail = None
    while time.time() < deadline:
        time.sleep(8)
        try:
            info = api(f"/api/runs/{rid}")
        except Exception:
            continue
        status = info.get("status") or "running"
        if status != "running":
            detail = info
            break

    if status != "success":
        msg = ""
        if detail:
            msg = str(detail.get("error") or "")[:400]
        print(json.dumps({"ok": False, "mode": mode, "status": status, "run_id": rid,
                          "error": msg or "运行未成功"}, ensure_ascii=False))
        return 2

    # 从节点状态里取产物与自检
    video, check_ok, seconds = "", None, None
    for n in (detail or {}).get("node_states") or []:
        if n.get("id") == "v":
            out = n.get("output") or {}
            video = out.get("video") or ""
            check_ok = out.get("check_ok")
            seconds = out.get("seconds")
    if not video or not os.path.isfile(video):
        print(json.dumps({"ok": False, "mode": mode, "error": "运行成功但没有产物文件",
                          "video": video}, ensure_ascii=False))
        return 2

    print(json.dumps({"ok": True, "mode": mode, "video": os.path.abspath(video),
                      "seconds": seconds, "check_ok": check_ok, "run_id": rid},
                     ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
