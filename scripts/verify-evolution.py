#!/usr/bin/env python3
"""自进化闭环自检：只读，不改图、不落盘、不起跑。

为什么要有这个脚本
------------------
自进化最容易出的问题不是算法写错，是**在没牙的状态下被判成"能跑"**：
门覆盖率是 0 的时候，status 照样返回 200，audit 照样 pass，
只有人真的去按一次才会发现"改门阈值也放行了"。
所以这里把三条最关键的牙齿单独拎出来做断言，跑不通就退出非 0。

断言（按顺序，任一失败即整体 FAIL）
----------------------------------
1. status 可访问，且给出 readiness（全库门覆盖率）
2. 给 workflowId 时，可调面非空，且**门判据被标 locked**
3. audit：改门判据 → 必须 rejected 且 hacks 非空（防作弊有牙）
4. audit：from 与现场不一致 → 必须 rejected（乐观锁有牙）
5. decide：必须返回三档之一，且不是 200 之外的错误

用法
----
    python scripts/verify-evolution.py
    python scripts/verify-evolution.py --base http://127.0.0.1:8080 --workflow-id 12
    python scripts/verify-evolution.py --all     # 逐个工作流扫一遍，列出有门的

注意：必须绕过本机 http_proxy（企业代理会把 127.0.0.1 变成假 502）。
"""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request

# 本机常驻代理会劫持 127.0.0.1，显式禁用
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def call(base: str, path: str, body: dict | None = None, timeout: float = 20.0):
    url = base.rstrip("/") + path
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers,
                                 method="POST" if data else "GET")
    try:
        with _OPENER.open(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(raw or "{}")
        except json.JSONDecodeError:
            return e.code, {"raw": raw}


class Checker:
    def __init__(self) -> None:
        self.failed = 0
        self.passed = 0

    def check(self, name: str, ok: bool, detail: str = "") -> None:
        if ok:
            self.passed += 1
            print(f"  PASS  {name}" + (f"  [{detail}]" if detail else ""))
        else:
            self.failed += 1
            print(f"  FAIL  {name}" + (f"  [{detail}]" if detail else ""))


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8080")
    ap.add_argument("--workflow-id", type=int, default=0)
    ap.add_argument("--all", action="store_true", help="只扫就绪度，逐条列出有门的工作流")
    args = ap.parse_args()

    ck = Checker()
    print(f"目标 {args.base}")

    # ---- 1. status ----
    code, st = call(args.base, "/api/evolution/status" + (
        f"?workflowId={args.workflow_id}" if args.workflow_id else ""))
    ck.check("status 200", code == 200, f"HTTP {code}")
    if code != 200:
        return 1

    rd = st.get("readiness") or {}
    if rd:
        print(f"  就绪度：{rd.get('withGate')}/{rd.get('workflows')} 条工作流接了门"
              f"（覆盖 {rd.get('gateCoverage', 0) * 100:.0f}%）")
        print(f"  结论：{rd.get('verdict')}")
        for g in rd.get("gatedWorkflows") or []:
            print(f"    - wf {g['id']} {g['name']}：{g['gates']} 门 / {g['knobs']} 旋钮")
        if args.all:
            return 0 if ck.failed == 0 else 2

    fuel = st.get("fuel") or {}
    print(f"  燃料：evidence={fuel.get('evidence')} scored={fuel.get('scored')} "
          f"pairable={fuel.get('pairableCases')} ready={fuel.get('ready')}")
    for b in fuel.get("blockers") or []:
        print(f"    blocker: {b}")

    if not args.workflow_id:
        print("\n未给 --workflow-id：只验了就绪度。要验牙齿请给一条有门的图。")
        return 0 if ck.failed == 0 else 2

    surface = (st.get("surface") or {})
    knobs = surface.get("knobs") or []
    ck.check("可调面非空", len(knobs) > 0, f"{len(knobs)} 个旋钮")
    locked = [k for k in knobs if k.get("gateLocked")]
    ck.check("门判据已标记为不可调", len(locked) > 0, f"{len(locked)} 个 locked")
    for k in locked[:5]:
        print(f"    locked: {k['nodeId']}.{k['key']} = {k['current']}")

    if not locked:
        print("\n这条图里没有门判据，无法验证防作弊牙齿 —— 换一条带 quality_gate 的图")
        return 2

    # ---- 2. audit：改门判据必须被拦 ----
    gate_knob = locked[0]
    cur = gate_knob["current"]
    if isinstance(cur, bool):
        new = not cur
    elif isinstance(cur, (int, float)):
        new = cur * 2 if cur else 1
    else:
        new = "0"
    code, au = call(args.base, "/api/evolution/audit", {
        "workflowId": args.workflow_id,
        "edits": [{
            "nodeId": gate_knob["nodeId"], "path": "data." + gate_knob["key"],
            "from": cur, "to": new, "why": "verify: 试改判据",
        }],
    })
    hacks = au.get("hacks") or []
    ck.check("改门判据被拒", au.get("verdict") == "rejected" and len(hacks) > 0,
             f"verdict={au.get('verdict')} hacks={len(hacks)}")
    if hacks:
        print(f"    拦下理由：{hacks[0]}")

    # ---- 3. audit：from 不一致必须被拦 ----
    editable = [k for k in knobs if not k.get("gateLocked")]
    if editable:
        k = editable[0]
        code, au2 = call(args.base, "/api/evolution/audit", {
            "workflowId": args.workflow_id,
            "edits": [{
                "nodeId": k["nodeId"], "path": "data." + k["key"],
                "from": "__definitely_not_the_current_value__",
                "to": k["current"], "why": "verify: 故意错 from",
            }],
        })
        ck.check("from 不一致被拒", au2.get("verdict") == "rejected",
                 f"verdict={au2.get('verdict')}")
    else:
        print("  SKIP  没有非门层旋钮，跳过乐观锁断言")

    # ---- 4. decide ----
    code, dc = call(args.base, "/api/evolution/decide", {"workflowId": args.workflow_id})
    dec = (dc.get("decision") or {})
    ck.check("decide 可判定", code == 200 and dec.get("verdict") in
             ("promote", "keep_baseline", "inconclusive"),
             f"verdict={dec.get('verdict')}")
    print(f"    pairs={dec.get('pairs')} matched={dec.get('matchedCases')} "
          f"胜/负/平={dec.get('better')}/{dec.get('worse')}/{dec.get('tie')}")
    for r in dec.get("reasons") or []:
        print(f"    理由：{r}")

    print(f"\n结果：{ck.passed} 通过 / {ck.failed} 失败")
    return 0 if ck.failed == 0 else 2


if __name__ == "__main__":
    sys.exit(main())
