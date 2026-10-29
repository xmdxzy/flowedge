#!/usr/bin/env python3
"""出片成本账本：只读 sqlite，回答「GPU 时间花在哪」。

为什么要有这个脚本（2026-10-26）：
    查「某支片子为什么超时」时，日志里只有一句 "node timeout"，
    逐镜真实耗时得去 `run_events` 里挖 `shot_progress.data.elapsed_ms` 才算得出来。
    这套算术已经写过三遍（每次还要处理 sqlite 只读、时区、data 是 bytes 的坑），
    按 AGENTS §0.14「一次性测量先查 scripts/」，固化到这里。

用法：
    python scripts/run-cost.py                  # 最近 6 次运行
    python scripts/run-cost.py --limit 20
    python scripts/run-cost.py --db 运行/data/flowedge.db --run 203

只读打开（mode=ro），服务正在跑也能用；不写库、不加锁。
"""
import argparse
import json
import os
import sqlite3
import sys
from datetime import datetime

DEFAULT_DB = os.path.join("运行", "data", "flowedge.db")


def connect(path):
    if not os.path.exists(path):
        sys.exit(f"找不到数据库：{path}")
    return sqlite3.connect(f"file:{path}?mode=ro", uri=True)


def ts(value):
    """sqlite 里的时间可能是字符串/bytes/带时区，统一成 naive datetime。"""
    if value is None:
        return None
    if isinstance(value, bytes):
        value = value.decode("utf-8", "replace")
    text = str(value).replace("T", " ").split("+")[0].split("Z")[0].strip()
    for fmt in ("%Y-%m-%d %H:%M:%S.%f", "%Y-%m-%d %H:%M:%S"):
        try:
            return datetime.strptime(text[:26], fmt)
        except ValueError:
            continue
    return None


def as_json(value):
    if isinstance(value, bytes):
        value = value.decode("utf-8", "replace")
    if not value:
        return {}
    try:
        return json.loads(value)
    except (TypeError, ValueError):
        return {}


def summarize_run(cur, run_id, status, pipeline, started, ended):
    a, b = ts(started), ts(ended)
    wall = f"{(b - a).total_seconds():.0f}s" if a and b else "?"
    print(f"\n=== run {run_id}  pipeline={pipeline or '?'}  {status}  墙钟 {wall} ===")

    cur.execute(
        """select type, node_id, created_at, message, data from run_events
           where run_id = ? order by seq, id""",
        (run_id,),
    )
    events = [(t, n, ts(c), m or "", d) for t, n, c, m, d in cur.fetchall()]

    node_start = {}
    for typ, nid, at, msg, data in events:
        if typ == "node_started" and at:
            node_start[nid] = at
        elif typ in ("node_finished", "node_failed") and at and nid in node_start:
            cost = (at - node_start.pop(nid)).total_seconds()
            print(f"  节点 {nid:<8} {typ.split('_')[1]:<7} {cost:8.1f}s  {msg[:60]}")

    shots = [(at, as_json(d)) for typ, nid, at, msg, d in events if typ == "shot_progress"]
    if shots:
        print("  逐镜：")
        for at, d in shots:
            ms = d.get("elapsed_ms") or 0
            print(
                f"    {at.strftime('%H:%M:%S') if at else '--:--:--'}  "
                f"第 {d.get('index', '?')}/{d.get('total', '?')} 镜 "
                f"shot={d.get('shot_id', '?')}  {str(d.get('phase', '')):<11} "
                f"{ms / 1000:8.1f}s"
            )
    else:
        print("  逐镜：无 shot_progress 事件")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--limit", type=int, default=6)
    ap.add_argument("--run", type=int, default=0, help="只看某一次运行")
    args = ap.parse_args()

    cur = connect(args.db).cursor()
    if args.run:
        cur.execute(
            "select id, status, pipeline_id, started_at, ended_at from workflow_runs where id = ?",
            (args.run,),
        )
        rows = cur.fetchall()
    else:
        cur.execute(
            """select id, status, pipeline_id, started_at, ended_at
               from workflow_runs order by id desc limit ?""",
            (args.limit,),
        )
        rows = cur.fetchall()

    if not rows:
        sys.exit("没有运行记录")

    for run_id, status, pipeline, started, ended in reversed(rows):
        summarize_run(cur, run_id, status, pipeline, started, ended)


if __name__ == "__main__":
    main()
