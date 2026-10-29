#!/usr/bin/env python3
"""出片台在线冒烟：验证「只剩多模态流程」之后的接口面。

为什么重写：这张脚本原来验的是「通用工作流平台」那套（/api/modules、
/api/workflows、任务书 spec）。FlowEdge 收敛成固定出片流水线后，那些接口
**已经被删掉**，继续验它们等于给一个不存在的产品发合格证。

这条脚本只做三件事：
  1. 该有的在：健康、流水线目录、运行列表、产物、依赖、MCP 配置；
  2. 参数错误在入口变成 400 + 人话（不是先建 run 再失败）；
  3. 已经删掉的接口必须 **404**——防止有人把画布/治理那套悄悄加回来。

不触发真实生成：冒烟只走到「提交前的校验」为止，不点着 GPU 跑几分钟。

用法：python scripts/verify-canonical-live.py
退出码：0 = 全部通过；1 = 有断言失败；2 = 引擎不可达。
"""
from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("FLOWEDGE_BASE", "http://127.0.0.1:8080")

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


def status_of(method: str, path: str, body=None) -> int:
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with OPENER.open(req, timeout=30) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def api_raw(method: str, path: str, body=None):
    """返回 (status, text)。要断言「被拒绝」时必须拿到 4xx 的响应体——

    api() 遇到 4xx 会抛 HTTPError，那正是"用工具没报错当没问题"的镜像面：
    拒绝是预期行为，得看清它拒绝的理由。"""
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with OPENER.open(req, timeout=60) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def main() -> int:
    print("[1] 引擎探活")
    try:
        health = api("GET", "/api/health")
        check(health.get("status") == "ok", f"{BASE}/api/health → ok")
    except Exception as e:
        print(f"  引擎不可达：{e}")
        return 2

    # 版本号：构建时 -X main.version 注进来的，用来确认"验的就是这一版"。
    version = str(health.get("version") or "")
    check(bool(version), f"health 带版本号（{version or '空'}）")
    expect = os.environ.get("FLOWEDGE_EXPECT_VERSION", "")
    if expect:
        check(version == expect, f"版本号 = {expect}（实际 {version}）")

    print("[2] 流水线目录（产品形态本身）")
    cat = api("GET", "/api/pipelines")
    pipes = cat.get("pipelines") or []
    check(len(pipes) >= 5, f"流水线数量 {len(pipes)} >= 5")
    ids = {p.get("id") for p in pipes}
    for need in ("image", "video", "film", "image_edit", "voice"):
        check(need in ids, f"存在流水线 {need}")
    bad = []
    for p in pipes:
        for f in p.get("fields") or []:
            if not f.get("key") or not f.get("type"):
                bad.append(f"{p.get('id')}.{f}")
        if not p.get("output"):
            bad.append(f"{p.get('id')}.output 缺失")
        if not p.get("nodes"):
            bad.append(f"{p.get('id')}.nodes 缺失")
    check(not bad, f"每条流水线的字段/产物类型都完整（问题：{bad[:3]}）")
    film = next((p for p in pipes if p.get("id") == "film"), None)
    check(bool(film) and len(film.get("nodes") or []) >= 3,
          "多段短片流水线含 storyboard → shotlist 两步以上")

    print("[3] 参数校验必须发生在入口")
    check(status_of("POST", "/api/produce", {"pipeline": "nope", "params": {}}) == 400,
          "未知流水线 → 400")
    body = None
    try:
        api("POST", "/api/produce", {"pipeline": "image", "params": {}})
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")
    check(body is not None and "画面描述" in body, f"缺必填 → 400 且点名缺什么（{body}）")

    print("[4] 出片前体检（不跑）")
    pre = api("POST", "/api/preflight", {"pipeline": "image", "params": {"prompt": "冒烟测试"}})
    check("ok" in pre and "missing" in pre, "preflight 返回 ok/missing")
    check(isinstance(pre.get("missing"), list), "missing 是清单（缺什么逐项列出）")

    print("[5] 看板与产物")
    runs = api("GET", "/api/runs?limit=5")
    check(isinstance(runs.get("runs"), list), "runs 列表可读")
    arts = api("GET", "/api/artifacts")
    check(isinstance(arts.get("items"), list), "artifacts 列表可读")
    check("dir" in arts, "artifacts 给出产物目录（人要知道去哪儿拿文件）")
    deps = api("GET", "/api/deps")
    check(isinstance(deps.get("items"), list), "deps 列表可读")
    cfg = api("GET", "/api/mcp/config")
    check(bool(cfg), "mcp/config 可读（DSH 接入靠它）")

    print("[6] 已删除的接口必须 404（防止平台形态偷偷回来）")
    # /api/workflows* 已于 2026-10-29 恢复（画布回来了，见 docs/画布与流水线统一-20261029.md），
    # 所以它从这份「必须 404」清单里移出，改由 [7] 正向验收。
    for path in ("/api/modules", "/api/estimate", "/api/models",
                 "/api/capabilities", "/api/llm/profiles", "/api/instances",
                 "/api/gpu/status", "/api/preflight/old"):
        check(status_of("GET", path) == 404, f"{path} → 404")

    print("[7] 画布契约（一张图两个视图）")
    cat = api("GET", "/api/node-catalog")
    nodes = cat.get("nodes") or []
    check(len(nodes) == 13, f"节点目录 13 种（实际 {len(nodes)}，2026-10-29 起含 comfy 逃生舱）")
    check(any(n.get("type") == "comfy" for n in nodes), "comfy 逃生舱节点在目录里")
    check(all(n.get("label") and (n.get("cost") or {}).get("unit") for n in nodes),
          "每个节点都有中文名与成本单位（前端不硬编码）")
    check(all(("in" in n) and ("out" in n) for n in nodes), "每个节点都声明了 in/out 端口")

    tpl = api("GET", "/api/workflows/templates")
    tpls = tpl.get("templates") or []
    check(len(tpls) >= 7, f"模板来自 pipelines.json（{len(tpls)} 条）")
    bad = [t.get("pipeline_id") for t in tpls
           if not (t.get("report") or {}).get("ok") or not t.get("nodes")]
    check(not bad, f"每条模板都能组装成零阻断的图（问题：{bad}）")
    laid = all(n.get("position") for t in tpls for n in (t.get("nodes") or []))
    check(laid, "模板的每个节点都有坐标（不然打开画布堆在原点）")

    lst = api("GET", "/api/workflows")
    check(isinstance(lst.get("workflows"), list), "工作流列表可读（不再是 404）")

    # 坏图必须在保存期被拦，且点名稳定错误码
    bad_graph = {"name": "坏图", "nodes": [
        {"id": "s", "type": "start", "data": {"prompt": "x"}},
        {"id": "up", "type": "upscale", "data": {}},
    ], "edges": [{"id": "e1", "source": "s", "target": "up"}]}
    code, body = api_raw("POST", "/api/workflows", bad_graph)
    check(code == 400 and "input_unreachable" in (body or ""),
          "必填输入没来源的图 → 400 + input_unreachable")

    # 干跑：不提交，且估算必须是区间（不许出现单一总时长）
    dry = api("POST", "/api/graph/dry-run", {
        "nodes": [
            {"id": "s", "type": "start", "data": {"prompt": "白鹿立于雪峰"}},
            {"id": "t2v", "type": "text2video", "data": {"frames": 124, "segments": 1}},
            {"id": "e", "type": "end", "data": {}},
        ],
        "edges": [{"id": "e1", "source": "s", "target": "t2v"},
                  {"id": "e2", "source": "t2v", "target": "e"}],
    })
    est = dry.get("estimate") or {}
    check((dry.get("report") or {}).get("ok") is True, "干跑：干净图判绿")
    check(est.get("p50_sec", 0) > 0 and est.get("p90_sec", 0) > est.get("p50_sec", 0)
          and est.get("worst_sec", 0) > est.get("p90_sec", 0), "干跑：四档估算单调且非零")
    check("seconds" not in est, "干跑：顶层没有单一总时长字段")

    # 运行 = 最后一次守门。断言只挑**与环境无关**的事实：
    #   - ComfyUI/TTS 能不能用取决于这台机器此刻的状态，不该写进验收断言
    #     （实测本机 TTS 在 :8101 活着，所以缺 8103 不等于"依赖缺失"）；
    #   - 该钉死的是：不存在的图 404、坏图 400、好图要么入队要么被明确拦下，绝不 500。
    code, _ = api_raw("POST", "/api/workflows/999999/run", {})
    check(code == 404, f"运行不存在的图 → 404（{code}）")

    code, saved = api_raw("POST", "/api/workflows", {
        "name": "冒烟图",
        "nodes": [
            {"id": "s", "type": "start", "data": {"prompt": "雨夜旧客栈，师徒对峙"}},
            {"id": "tts", "type": "tts", "data": {"text": "门外有人。"}},
            {"id": "e", "type": "end", "data": {}},
        ],
        "edges": [{"id": "e1", "source": "s", "target": "tts"},
                  {"id": "e2", "source": "tts", "target": "e"}],
    })
    check(code == 201, f"合法图可保存（{code}）")
    wfid = (json.loads(saved or "{}") or {}).get("id")
    if wfid:
        code, body = api_raw("POST", f"/api/workflows/{wfid}/run", {})
        check(code in (202, 409, 503), f"运行被受理或被明确拒绝（{code}），不 500")
        if code == 202:
            check("\"run_id\"" in (body or ""), "202 必须带 run_id")
        else:
            check("\"error\"" in (body or ""), "被拦时必须说明原因（error 字段）")
        code, _ = api_raw("DELETE", f"/api/workflows/{wfid}")
        check(code == 200, "清理临时工作流")

    print()
    if FAILS:
        print(f"RESULT: HAS-FAIL ({len(FAILS)} 项)")
        for f in FAILS:
            print(f"  - {f}")
        return 1
    print("RESULT: ALL-PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
