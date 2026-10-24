#!/usr/bin/env python3
"""FlowEdge 出片 CLI（取代原来的「建工作流 → 跑工作流」两段式脚本）。

FlowEdge 收敛后只剩一个动作：**选流水线 + 填字段 → 出片**。这个脚本就是
那条动作的命令行壳子，顺便给 agent 一个不必手写 curl 的入口。

用法
----
    python scripts/run-flowedge.py pipelines
    python scripts/run-flowedge.py produce --pipeline image --params '{"prompt":"云海楼阁","qc":true}'
    python scripts/run-flowedge.py produce --pipeline video --params-file .tmp/p.json --wait
    python scripts/run-flowedge.py status 12
    python scripts/run-flowedge.py runs --limit 10 --status failed
    python scripts/run-flowedge.py artifacts --kind video

退出码：0 = 成功；1 = 出片失败/引擎报错；2 = 引擎不可达或参数不合法。
输出：一行 JSON（便于 agent 直接解析），不打印花哨日志。
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("FLOWEDGE_BASE", "http://127.0.0.1:8080")
TOKEN = os.environ.get("FLOWEDGE_API_TOKEN", "")

# 本机常驻企业代理（http_proxy=127.0.0.1:5751）：本地调用必须绕过，否则 502 假死
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def api(path: str, method: str = "GET", body=None, timeout: int = 60):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data:
        req.add_header("Content-Type", "application/json")
    if TOKEN:
        req.add_header("Authorization", "Bearer " + TOKEN)
    with OPENER.open(req, timeout=timeout) as r:
        raw = r.read().decode("utf-8", "replace")
    return json.loads(raw) if raw.strip() else {}


def emit(obj) -> None:
    print(json.dumps(obj, ensure_ascii=False))


def cmd_pipelines(_args) -> int:
    try:
        cat = api("/api/pipelines")
    except Exception as e:
        emit({"ok": False, "error": f"引擎不可达（{BASE}）：{e}", "hint": "先启动 FlowEdge"})
        return 2
    rows = []
    for p in cat.get("pipelines") or []:
        rows.append({
            "id": p.get("id"),
            "name": p.get("name"),
            "output": p.get("output"),
            "est_seconds": p.get("est_seconds"),
            "summary": p.get("summary"),
            "fields": [
                {"key": f.get("key"), "type": f.get("type"), "default": f.get("default"),
                 "required": bool(f.get("required")), "options": f.get("options")}
                for f in p.get("fields") or []
            ],
        })
    emit({"ok": True, "count": len(rows), "pipelines": rows})
    return 0


def cmd_produce(args) -> int:
    params = {}
    if args.params:
        try:
            params = json.loads(args.params)
        except json.JSONDecodeError as e:
            emit({"ok": False, "error": f"--params 不是合法 JSON：{e}"})
            return 2
    if args.params_file:
        try:
            with open(args.params_file, encoding="utf-8-sig") as f:
                params.update(json.load(f))
        except Exception as e:
            emit({"ok": False, "error": f"读 --params-file 失败：{e}"})
            return 2

    body = {"pipeline": args.pipeline, "params": params, "invoked_by": args.invoked_by}
    try:
        # 先体检：缺服务时给的是「怎么起」，不是十分钟后一条 failed
        pre = api("/api/preflight", "POST", body)
        if not pre.get("ok"):
            emit({
                "ok": False,
                "stage": "preflight",
                "missing": pre.get("missing"),
                "hint": "按每项的 fix 处理，或有 fixable 的调 POST /api/deps/<fix_dep>/start",
            })
            return 2
        sub = api("/api/produce", "POST", body)
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")
        emit({"ok": False, "stage": "submit", "status": e.code, "error": detail[:600]})
        return 2
    except Exception as e:
        emit({"ok": False, "error": f"引擎不可达（{BASE}）：{e}"})
        return 2

    run_id = sub.get("run_id")
    if not args.wait:
        emit({"ok": True, "stage": "submitted", "run_id": run_id, "pipeline": sub.get("pipeline")})
        return 0

    detail = wait_run(run_id, args.timeout)
    if detail is None:
        emit({"ok": False, "stage": "wait", "run_id": run_id,
              "error": f"等了 {args.timeout}s 还没结束（不是失败，是还没跑完）"})
        return 1
    out = {"ok": detail.get("status") == "success", "run_id": run_id,
           "status": detail.get("status"), "error": detail.get("error") or ""}
    out["artifacts"] = artifacts_of(detail)
    emit(out)
    return 0 if out["ok"] else 1


def cmd_status(args) -> int:
    try:
        detail = api(f"/api/runs/{args.run_id}")
    except Exception as e:
        emit({"ok": False, "error": f"查询失败：{e}"})
        return 2
    if args.wait and detail.get("status") == "running":
        detail = wait_run(args.run_id, args.timeout) or detail
    emit({"ok": True, "run": detail, "artifacts": artifacts_of(detail)})
    return 0


def cmd_runs(args) -> int:
    path = f"/api/runs?limit={args.limit}"
    if args.status:
        path += "&status=" + args.status
    try:
        data = api(path)
    except Exception as e:
        emit({"ok": False, "error": f"引擎不可达：{e}"})
        return 2
    emit({"ok": True, **data})
    return 0


def cmd_artifacts(args) -> int:
    path = "/api/artifacts"
    if args.kind:
        path += "?kind=" + args.kind
    try:
        data = api(path)
    except Exception as e:
        emit({"ok": False, "error": f"引擎不可达：{e}"})
        return 2
    emit({"ok": True, **data})
    return 0


def wait_run(run_id, timeout: int):
    """轮询到结束。返回详情；超时返回 None（超时 ≠ 失败）。

    进度打到 stderr：40 分钟的片子如果一行输出都没有，人和 agent 都会以为卡死。
    stdout 仍只有一行 JSON（可解析），互不污染。
    """
    deadline = time.time() + timeout
    started = time.time()
    last_tag, last_beat = "", 0.0
    while time.time() < deadline:
        time.sleep(5)
        try:
            info = api(f"/api/runs/{run_id}")
        except Exception:
            continue
        if info.get("status") != "running":
            return info
        nodes = info.get("node_states") or []
        running = [n for n in nodes if n.get("status") == "running"]
        tag = running[0].get("type") if running else "排队中"
        elapsed = time.time() - started
        if tag != last_tag or elapsed - last_beat >= 60:
            done = sum(1 for n in nodes if n.get("status") in ("success", "failed", "skipped"))
            print("[wait] %4.0fs  %-12s  已完成 %d/%d" % (elapsed, tag, done, len(nodes)),
                  file=sys.stderr, flush=True)
            last_tag, last_beat = tag, elapsed
    return None


def artifacts_of(run: dict) -> list:
    """从节点终态里挑出产物路径（image/video/audio 键），按路径去重。

    去重是必须的：end 节点会把上游的产物再透传一遍，同一条路径出现两次
    会让人以为"出了两个文件"。
    """
    out = []
    seen = set()
    for node in run.get("node_states") or []:
        output = node.get("output")
        if not isinstance(output, dict):
            continue
        def add(key, value):
            if not isinstance(value, str) or not value:
                return
            if value in seen:
                return
            seen.add(value)
            out.append({"node": node.get("id"), "type": node.get("type"), "key": key, "path": value})

        for key in ("image", "video", "audio", "path"):
            v = output.get(key)
            if isinstance(v, list):
                for item in v:
                    add(key, item)
            else:
                add(key, v)
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description="FlowEdge 出片 CLI")
    sub = ap.add_subparsers(dest="cmd", required=True)

    sub.add_parser("pipelines", help="列出本机能出的片子与字段")

    p = sub.add_parser("produce", help="出片")
    p.add_argument("--pipeline", required=True, help="流水线 id（先跑 pipelines）")
    p.add_argument("--params", help="JSON 字段，如 '{\"prompt\":\"...\"}'")
    p.add_argument("--params-file", help="从文件读字段（中文参数走文件更稳）")
    p.add_argument("--invoked-by", default="manual", choices=["manual", "workbuddy", "dsh", "mcp"])
    p.add_argument("--wait", action="store_true", help="等到跑完再返回")
    p.add_argument("--timeout", type=int, default=3600, help="--wait 的最长等待秒数")

    s = sub.add_parser("status", help="查一次出片")
    s.add_argument("run_id", type=int)
    s.add_argument("--wait", action="store_true")
    s.add_argument("--timeout", type=int, default=3600)

    r = sub.add_parser("runs", help="最近运行")
    r.add_argument("--limit", type=int, default=20)
    r.add_argument("--status", choices=["running", "success", "failed", "cancelled"])

    a = sub.add_parser("artifacts", help="产物清单")
    a.add_argument("--kind", choices=["image", "video", "audio"])

    args = ap.parse_args()
    return {
        "pipelines": cmd_pipelines,
        "produce": cmd_produce,
        "status": cmd_status,
        "runs": cmd_runs,
        "artifacts": cmd_artifacts,
    }[args.cmd](args)


if __name__ == "__main__":
    sys.exit(main())
