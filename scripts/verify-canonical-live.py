#!/usr/bin/env python3
"""canonical 出片工作流在线验证（任务 #9 收尾）。

单次调用内完成：探活 → 模块面 → 查重/复用 → 取任务书 → 断言 → 落盘。
为什么必须一次跑完：agent shell 拉起的引擎进程会随工具调用被沙箱回收，
跨调用续跑时引擎已经没了（DB 里的工作流还在，所以按名查重复用是安全的）。

用法：python scripts/verify-canonical-live.py
退出码：0 = 全部通过；1 = 有断言失败；2 = 引擎不可达。
"""
from __future__ import annotations

import json
import os
import sys
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BASE = os.environ.get("FLOWEDGE_BASE", "http://127.0.0.1:8080")
CANONICAL = os.path.join(ROOT, "data", "workflows", "canonical-film.json")
SPEC_MD = os.path.join(ROOT, "data", "workflows", "canonical-film-spec.md")

# 本机常驻企业代理（http_proxy=127.0.0.1:5751），本地探活必须绕过，否则 502 假死。
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

FAILS: list[str] = []


def check(cond: bool, label: str) -> bool:
    print(("  [PASS] " if cond else "  [FAIL] ") + label)
    if not cond:
        FAILS.append(label)
    return bool(cond)


def api(method: str, path: str, body=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data:
        req.add_header("Content-Type", "application/json")
    with OPENER.open(req, timeout=60) as r:
        return json.loads(r.read().decode("utf-8"))


def api_text(path: str) -> str:
    req = urllib.request.Request(BASE + path, method="GET")
    with OPENER.open(req, timeout=60) as r:
        return r.read().decode("utf-8")


def main() -> int:
    print("[1] 引擎探活")
    try:
        with OPENER.open(BASE + "/api/health", timeout=5) as r:
            check(200 <= r.status < 300, f"{BASE}/api/health")
    except Exception as e:
        print(f"  引擎不可达：{e}")
        return 2

    print("[2] 模块面（新契约二进制必须在跑）")
    raw = api("GET", "/api/modules")
    mods = raw.get("modules", raw) if isinstance(raw, dict) else raw
    check(len(mods) == 32, f"内置模块 32 个（实际 {len(mods)}）")
    types = {m.get("type") for m in mods}
    check("consistency_gate" in types, "consistency_gate 已注册")
    sl = next((m for m in mods if m.get("type") == "shotlist"), None)
    keys = {p.get("key") for p in (sl or {}).get("key_params", [])} if sl else set()
    check({"plan", "require_gates", "audio_check"} <= keys, "shotlist 新参数在契约里")

    print("[3] 工作流查重 / 复用")
    wfs = api("GET", "/api/workflows")
    hit = [w for w in wfs if "canonical" in (w.get("name") or "")]
    if hit:
        wid = hit[0]["id"]
        print(f"  复用已有工作流 id={wid}")
    else:
        with open(CANONICAL, encoding="utf-8") as f:
            wf = json.load(f)
        wid = api("POST", "/api/workflows", wf)["id"]
        print(f"  新建工作流 id={wid}")
    check(isinstance(wid, int) and wid > 0, f"工作流 id={wid}")

    print("[4] 任务书（format=json）")
    doc = api("GET", f"/api/workflows/{wid}/spec?format=json")
    layers = {g.get("layer") for g in doc.get("gates", [])}
    for layer in ("timing", "identity", "audio", "technical", "budget"):
        check(layer in layers, f"门层 {layer} 在任务书里")
    # 两种渲染口径：plan.gates 展平成 layer.key = value；节点参数门是 key = value（无层前缀）。
    blob = json.dumps(doc, ensure_ascii=False)
    for needle in (
        # plan.gates（五层门预检的唯一存放处）
        "timing.target_seconds = 30",
        "identity.stills = 2",
        "audio.loudness_db = [-20,-11]",
        "identity.max_ref_images = 9",
        "budget.wall_seconds_max = 1500",
        "technical.av_drift_max = 0.2",
        # 节点参数门（运行期的牙）
        "min_frames = 700",
        "duration_min = 25",
        "duration_max = 35",
        "loudness_min_db = -20",
        "max_silence_gap = 5",
        "require_audio = true",
        "layout_min = 0.94",
        "palette_min = 0.92",
        "drift_layout_min = 0.92",
        "expected_segments = 3",
        "loudness = -16",
    ):
        check(needle in blob, f"阈值 {needle}")
    gate_nodes = {m.get("id") for m in doc.get("modules", []) if m.get("gates")}
    for gid in ("sl-1", "cg-1", "pp-1", "qg-1"):
        check(gid in gate_nodes, f"门节点 {gid} 声明了门层")
    bare = [w for w in (doc.get("warnings") or []) if "等于没设门" in w]
    check(not bare, f"无裸门警告（{len(bare)} 条）")
    undeclared = [m.get("id") for m in doc.get("modules", []) if not m.get("declared", True)]
    check(not undeclared, f"所有节点都有契约（缺：{undeclared}）")
    # 菱形汇合：质检门必须同时吃一致性门与后期两条线（任一门 FAIL 都到得了交付前最后一关）
    qg = next((m for m in doc.get("modules", []) if m.get("id") == "qg-1"), None)
    qg_from = {i.get("from_node") for i in (qg or {}).get("inputs", [])}
    check({"cg-1", "pp-1"} <= qg_from, f"qg-1 汇合 cg-1+pp-1（实际来自 {sorted(qg_from)}）")
    if doc.get("warnings"):
        print("  任务书 warnings：")
        for w in doc["warnings"]:
            print(f"    - {w}")

    print("[5] 任务书落盘（format=md）")
    md = api_text(f"/api/workflows/{wid}/spec?format=md")
    with open(SPEC_MD, "w", encoding="utf-8") as f:
        f.write(md)
    check(len(md) > 2000, f"markdown {len(md)} 字节 -> {os.path.relpath(SPEC_MD, ROOT)}")
    check("## 2. 验收门" in md and "任一 FAIL 即不许交付" in md, "验收门章节完整")

    if FAILS:
        print(f"LIVE-VERIFY-FAIL（{len(FAILS)} 项）：")
        for x in FAILS:
            print("  -", x)
        return 1
    print("LIVE-VERIFY-OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
