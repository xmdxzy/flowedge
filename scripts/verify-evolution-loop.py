#!/usr/bin/env python3
"""自进化闭环端到端自检：会**真的建图、真的起跑、真的回滚**。

与 verify-evolution.py 的分工
-----------------------------
verify-evolution.py 是只读体检（牙齿在不在）；
本脚本是跑一遍完整闭环，证明「接尺子 → 配对 → 判定 → 盯回归 → 回滚」这条链
在真机上是通的，而不是只在单测里通。

它验证的核心事实（都是曾经会静默错的地方）
------------------------------------------
1. instrument 能把门接在终点前，且阈值从图里的生成参数反推
2. evaluate 起的跑能把 caseId / role / 版本写进证据（否则配对永远配不上）
3. **门判不过的运行记 failed，不是 unscored** —— 这一条曾因引擎丢弃失败产物
   而整体反向：真·质量不合格被记成「没测到」，评测与自进化同时失去信号
4. decide 能按 caseId 配对并给出三档结论之一
5. monitor 能判出退化并给出回滚目标版本；rollback 先快照再回滚、可再滚回
6. 晋升台账：decide/monitor 结论可归档，人按的晋升/保留必带理由，
   坏 action 400，回滚自动落库（可审计）

用法（必须指向一个**隔离实例**，不要打生产 8080）
------------------------------------------------
    python scripts/verify-evolution-loop.py --base http://127.0.0.1:18080
    python scripts/verify-evolution-loop.py --base http://127.0.0.1:18080 --keep

脚本自己创建两张图并在结束时删除；--keep 保留现场，便于人工复看。
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

_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def call(base: str, path: str, body=None, method: str | None = None, timeout: float = 60.0):
    url = base.rstrip("/") + path
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    verb = method or ("POST" if data is not None else "GET")
    req = urllib.request.Request(url, data=data, headers=headers, method=verb)
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
        self.ok = 0
        self.fail = 0

    def check(self, name: str, cond: bool, detail: str = "") -> None:
        if cond:
            self.ok += 1
            print(f"  PASS  {name}" + (f"  [{detail}]" if detail else ""))
        else:
            self.fail += 1
            print(f"  FAIL  {name}" + (f"  [{detail}]" if detail else ""))


def make_test_video(workdir: str) -> str:
    # 必须绝对路径：引擎按自己的 CWD 解析相对路径，图里写相对路径会「文件不存在」
    path = os.path.abspath(os.path.join(workdir, "gate-fixture.mp4"))
    if os.path.exists(path):
        return path
    os.makedirs(workdir, exist_ok=True)
    subprocess.run([
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
        "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=16",
        "-an", "-pix_fmt", "yuv420p", path,
    ], check=True)
    return path


def today() -> str:
    return time.strftime("%Y-%m-%d %H:%M")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:18080")
    ap.add_argument("--workdir", default=".tmp/evo-loop")
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--timeout", type=float, default=180.0, help="等一批跑完的上限（秒）")
    args = ap.parse_args()

    base = args.base
    ck = Checker()
    print(f"目标实例 {base}（应是隔离实例，不要指向生产 8080）")

    code, health = call(base, "/api/health")
    if code != 200:
        print(f"实例不可达：HTTP {code}")
        return 1

    video = make_test_video(args.workdir)
    print(f"测试视频 {video}")

    created: list[int] = []
    try:
        # ---------- A. 接尺子 ----------
        print("\n[A] instrument：给一张没门的图接门")
        nodes = [
            {"id": "s1", "type": "start", "position": {"x": 0, "y": 0},
             "data": {"prompt": "instrument 验证"}, "status": "pending"},
            {"id": "v1", "type": "text2video", "position": {"x": 300, "y": 0},
             "data": {"width": 608, "height": 352, "frames": 124, "fps": 24}, "status": "pending"},
            {"id": "e1", "type": "end", "position": {"x": 600, "y": 0}, "data": {}, "status": "pending"},
        ]
        edges = [
            {"id": "a", "source": "s1", "target": "v1"},
            {"id": "b", "source": "v1", "target": "e1"},
        ]
        code, wf1 = call(base, "/api/workflows", {"name": f"verify-loop instrument {today()}", "nodes": nodes, "edges": edges})
        ck.check("建图 A 成功", code == 201, f"HTTP {code}")
        if code != 201:
            return 2
        wid_a = wf1["id"]
        created.append(wid_a)

        code, plan = call(base, "/api/evolution/instrument", {"workflowId": wid_a})
        derived = plan.get("derived") or []
        ck.check("instrument dry-run 有方案", code == 200 and plan.get("wouldApply") is True,
                 f"HTTP {code} gates={plan.get('gates')}")
        ck.check("阈值从生成参数反推", any("min_frames=117" in d for d in derived),
                 "; ".join(derived)[:120])
        ck.check("dry-run 不落图", plan.get("applied") is False)

        code, applied = call(base, "/api/evolution/instrument", {"workflowId": wid_a, "apply": True})
        ck.check("instrument 落图成功", code == 200 and applied.get("applied") is True,
                 f"HTTP {code} snapshot=v{applied.get('snapshotVersion')}")
        ck.check("落图前有快照", (applied.get("snapshotVersion") or 0) > 0,
                 f"snapshot=v{applied.get('snapshotVersion')}")

        code, wf1b = call(base, f"/api/workflows/{wid_a}")
        gnodes = json.loads(wf1b["nodes"]) if isinstance(wf1b.get("nodes"), str) else wf1b.get("nodes") or []
        gedges = json.loads(wf1b["edges"]) if isinstance(wf1b.get("edges"), str) else wf1b.get("edges") or []
        gates = [n for n in gnodes if n["type"] == "quality_gate"]
        into_end = [e for e in gedges if e["target"] == "e1"]
        ck.check("图里多了一个质检门", len(gates) == 1, f"{len(gates)} 个")
        ck.check("门接在终点前（终点入边来自门）",
                 len(into_end) == 1 and gates and into_end[0]["source"] == gates[0]["id"],
                 f"end 入边来源 {[e['source'] for e in into_end]}")

        # ---------- B. 配对评测（真跑，无 GPU） ----------
        print("\n[B] evaluate：同一 case 在改前改后各跑一次")
        nodes_b = [
            {"id": "src1", "type": "input_source", "position": {"x": 0, "y": 0},
             "data": {"kind": "video", "path": video}, "status": "pending"},
            {"id": "g1", "type": "quality_gate", "position": {"x": 300, "y": 0},
             "data": {"min_frames": 1}, "status": "pending"},
            {"id": "e1", "type": "end", "position": {"x": 600, "y": 0}, "data": {}, "status": "pending"},
        ]
        edges_b = [
            {"id": "a", "source": "src1", "target": "g1"},
            {"id": "b", "source": "g1", "target": "e1"},
        ]
        code, wf2 = call(base, "/api/workflows", {"name": f"verify-loop evaluate {today()}", "nodes": nodes_b, "edges": edges_b})
        ck.check("建图 B 成功", code == 201, f"HTTP {code}")
        if code != 201:
            return 2
        wid_b = wf2["id"]
        created.append(wid_b)

        code, ver = call(base, f"/api/workflows/{wid_b}/versions?note=eval-baseline", method="POST")
        ck.check("存基线版本 v1", code == 201, f"HTTP {code} v={ver.get('version')}")
        base_version = ver.get("version") or 1

        # 候选版本：阈值抬到 999 帧 —— 这条 tsrc 视频只有 32 帧，必然判不过
        cand_nodes = json.loads(json.dumps(nodes_b))
        cand_nodes[1]["data"]["min_frames"] = 999
        code, _ = call(base, f"/api/workflows/{wid_b}", {"nodes": cand_nodes, "edges": edges_b}, method="PUT")
        ck.check("改候选图（min_frames 1→999）", code == 200, f"HTTP {code}")

        cases = [{"caseId": "loop-c1", "category": "short"}, {"caseId": "loop-c2", "category": "long"}]
        code, b1 = call(base, "/api/evolution/evaluate", {
            "workflowId": wid_b, "version": base_version, "role": "baseline",
            "cases": cases, "datasetFingerprint": "verify-loop",
        })
        ck.check("受理 baseline 批", code == 202, f"HTTP {code}")
        code, b2 = call(base, "/api/evolution/evaluate", {
            "workflowId": wid_b, "role": "candidate",
            "cases": cases, "datasetFingerprint": "verify-loop",
        })
        ck.check("受理 candidate 批", code == 202, f"HTTP {code}")

        def wait_batch(batch_id: str, expected: int) -> dict:
            # 起跑是异步的（逐个受理），所以「跑完了」的判据必须同时含
            # total>=expected：只看 done==total 会在第二个 run 还没入库时就返回。
            deadline = time.time() + args.timeout
            last = {}
            while time.time() < deadline:
                c, b = call(base, f"/api/evolution/batch?batchId={batch_id}")
                last = b
                if c == 200 and (b.get("total") or 0) >= expected and b.get("done") == b.get("total"):
                    return b
                time.sleep(2)
            return last

        bb = wait_batch(b1.get("batchId", ""), len(cases))
        cb = wait_batch(b2.get("batchId", ""), len(cases))
        print(f"   baseline: total={bb.get('total')} scored={bb.get('scored')} passed={bb.get('passed')}")
        print(f"   candidate: total={cb.get('total')} scored={cb.get('scored')} passed={cb.get('passed')}")
        ck.check("baseline 两例都拿到分数", bb.get("scored") == 2, f"scored={bb.get('scored')}")
        ck.check("baseline 两例都过门", bb.get("passed") == 2, f"passed={bb.get('passed')}")
        ck.check("candidate 两例都拿到分数（门判不过 ≠ 没测到）", cb.get("scored") == 2,
                 f"scored={cb.get('scored')}（若为 0 说明失败产物被丢弃，证据退化成 unscored）")
        ck.check("candidate 两例都不过门", cb.get("passed") == 0, f"passed={cb.get('passed')}")
        results = {r.get("caseId"): r.get("result") for r in (cb.get("runs") or [])}
        ck.check("candidate 证据 result=failed", all(v == "failed" for v in results.values()), str(results))

        # ---------- C. 判定 ----------
        print("\n[C] decide：按 caseId 配对")
        code, st = call(base, f"/api/evolution/status?workflowId={wid_b}")
        fuel = st.get("fuel") or {}
        ck.check("证据带上了 caseId/role（可配对数=2）", fuel.get("pairableCases") == 2,
                 f"pairable={fuel.get('pairableCases')}")
        ck.check("证据带上了类别", len(fuel.get("categories") or []) == 2, f"{fuel.get('categories')}")

        code, dc = call(base, "/api/evolution/decide", {"workflowId": wid_b, "minPairs": 2})
        dec = dc.get("decision") or {}
        ck.check("decide 返回三档结论", dec.get("verdict") in ("promote", "keep_baseline", "inconclusive"),
                 f"verdict={dec.get('verdict')}")
        ck.check("配对确实配上了", dec.get("pairs") == 2, f"pairs={dec.get('pairs')}")
        ck.check("候选更差 → keep_baseline", dec.get("verdict") == "keep_baseline",
                 f"verdict={dec.get('verdict')} worse={dec.get('worse')}")

        # ---------- C2. 台账：decide 归档 ----------
        print("\n[C2] decide record=true：结论归档进晋升台账")
        code, dcr = call(base, "/api/evolution/decide", {"workflowId": wid_b, "minPairs": 2, "record": True})
        ck.check("decide 归档成功", code == 200 and dcr.get("recorded") is True, f"HTTP {code}")

        code, pl = call(base, f"/api/evolution/promotions?workflowId={wid_b}")
        recs = pl.get("records") or []
        ck.check("台账能查到归档记录", code == 200 and len(recs) >= 1, f"HTTP {code} count={pl.get('count')}")
        arch = next((r for r in recs if r.get("action") == "archive"), None)
        ck.check("归档记 archive（引擎结论，人未按键）",
                 bool(arch) and arch.get("source") == "decide" and arch.get("verdict") == "keep_baseline",
                 f"action={arch.get('action') if arch else 'n/a'} verdict={arch.get('verdict') if arch else 'n/a'}")
        ck.check("归档带理由与数值快照",
                 bool(arch) and bool(arch.get("reason")) and arch.get("pairs") == 2,
                 f"reason={(arch or {}).get('reason', '')[:60]}")

        # ---------- D. 回归监测与回滚 ----------
        print("\n[D] monitor / rollback")
        # minPairs=2 与上面 decide 同口径：自检只有 2 对，默认门槛 8 会判 inconclusive
        code, mon = call(base, f"/api/evolution/monitor?workflowId={wid_b}&minPairs=2")
        rep = mon.get("report") or {}
        ck.check("monitor 判出退化", rep.get("verdict") == "regressed", f"verdict={rep.get('verdict')}")
        ck.check("给出回滚目标版本", rep.get("rollbackTo") == base_version,
                 f"rollbackTo=v{rep.get('rollbackTo')}（基线版本 v{base_version}）")

        # 体检结论归档（record=true）：退化结论也要可审计
        code, monr = call(base, f"/api/evolution/monitor?workflowId={wid_b}&minPairs=2&record=true")
        ck.check("monitor 归档成功", code == 200 and monr.get("recorded") is True, f"HTTP {code}")

        code, dry = call(base, "/api/evolution/rollback", {"workflowId": wid_b, "minPairs": 2})
        ck.check("rollback 默认 dry-run", code == 200 and dry.get("applied") is False, f"HTTP {code}")

        code, rb = call(base, "/api/evolution/rollback", {"workflowId": wid_b, "minPairs": 2, "apply": True})
        ck.check("rollback 执行成功", code == 200 and rb.get("applied") is True, f"HTTP {code}")
        ck.check("回滚前留了快照（可再滚回）", (rb.get("snapshotVersion") or 0) > 0,
                 f"snapshot=v{rb.get('snapshotVersion')}")

        code, wf2b = call(base, f"/api/workflows/{wid_b}")
        rn = json.loads(wf2b["nodes"]) if isinstance(wf2b.get("nodes"), str) else wf2b.get("nodes") or []
        gate = [n for n in rn if n["type"] == "quality_gate"]
        ck.check("回滚后阈值回到基线值 1",
                 bool(gate) and gate[0]["data"].get("min_frames") == 1,
                 f"min_frames={gate[0]['data'].get('min_frames') if gate else 'n/a'}")

        # ---------- E. 晋升台账：人按的键与校验 ----------
        print("\n[E] promote / promotions：人按下晋升或保留")

        # 坏 action：端点只收人按的两个键
        code, _ = call(base, "/api/evolution/promote",
                       {"workflowId": wid_b, "action": "rollback", "reason": "x"})
        ck.check("坏 action 400", code == 400, f"HTTP {code}")
        code, _ = call(base, "/api/evolution/promote",
                       {"workflowId": wid_b, "action": "promote", "toVersion": base_version})
        ck.check("缺理由 400", code == 400, f"HTTP {code}")
        code, _ = call(base, "/api/evolution/promote",
                       {"workflowId": wid_b, "action": "promote", "toVersion": 9999, "reason": "x"})
        ck.check("晋升到不存在的版本 404", code == 404, f"HTTP {code}")

        # dry-run 晋升：只落台账不改图
        code, pr = call(base, "/api/evolution/promote", {
            "workflowId": wid_b, "action": "promote", "fromVersion": base_version,
            "toVersion": base_version, "reason": "自检 dry-run：只验证落库", "actor": "verify-script",
        })
        ck.check("dry-run 晋升落库", code == 200 and pr.get("recorded") is True and pr.get("applied") is False,
                 f"HTTP {code}")
        code, wf2c = call(base, f"/api/workflows/{wid_b}")
        rn2 = json.loads(wf2c["nodes"]) if isinstance(wf2c.get("nodes"), str) else wf2c.get("nodes") or []
        gate2 = [n for n in rn2 if n["type"] == "quality_gate"]
        ck.check("dry-run 没动图（阈值仍是回滚后的 1）",
                 bool(gate2) and gate2[0]["data"].get("min_frames") == 1,
                 f"min_frames={gate2[0]['data'].get('min_frames') if gate2 else 'n/a'}")

        # 保留：只落台账
        code, kp = call(base, "/api/evolution/promote", {
            "workflowId": wid_b, "action": "keep", "reason": "自检：候选更差，保留基线",
        })
        ck.check("保留落库", code == 200 and kp.get("recorded") is True, f"HTTP {code}")

        # 台账按工作流隔离 + 记录齐全（归档 ×1 / dry-run 晋升 ×1 / 保留 ×1 / 回滚 ×1）
        code, pl2 = call(base, f"/api/evolution/promotions?workflowId={wid_b}")
        recs2 = pl2.get("records") or []
        actions = sorted(r.get("action") for r in recs2)
        ck.check("台账四类记录齐全", actions == ["archive", "archive", "keep", "promote", "rollback"],
                 f"actions={actions}")
        roll = next((r for r in recs2 if r.get("action") == "rollback"), None)
        ck.check("回滚自动落库（带版本跨度）",
                 bool(roll) and roll.get("to_version") == base_version and (roll.get("from_version") or 0) > 0,
                 f"from=v{roll.get('from_version') if roll else '?'} to=v{roll.get('to_version') if roll else '?'}")
        keep = next((r for r in recs2 if r.get("action") == "keep"), None)
        ck.check("保留记录带署名与理由", bool(keep) and bool(keep.get("reason")),
                 f"actor={(keep or {}).get('actor')}")

        # 别的工作流的台账不许混进来
        code, pl3 = call(base, f"/api/evolution/promotions?workflowId={wid_a}")
        ck.check("台账按 workflowId 隔离", code == 200 and (pl3.get("count") or 0) == 0,
                 f"count={pl3.get('count')}")

        print(f"\n结果：{ck.ok} 通过 / {ck.fail} 失败")
        return 0 if ck.fail == 0 else 2
    finally:
        if not args.keep:
            for wid in created:
                call(base, f"/api/workflows/{wid}", method="DELETE")
            print("已清理测试图" + ("（证据文件保留在隔离实例的证据目录，供复看）" if created else ""))


if __name__ == "__main__":
    sys.exit(main())
