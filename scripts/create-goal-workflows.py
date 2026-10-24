# -*- coding: utf-8 -*-
"""Create the two Goal workflows (outer + inner) in the running FlowEdge engine.

Run:  python scripts/create-goal-workflows.py
Env:  FLOWEDGE_BASE  (default http://127.0.0.1:8080)
"""
import json
import os
import urllib.request

BASE = os.environ.get("FLOWEDGE_BASE", "http://127.0.0.1:8080").rstrip("/")
OUT_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "data", "workflows")


def post(path, obj):
    data = json.dumps(obj, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(req, timeout=30) as r:
        return json.loads(r.read().decode("utf-8"))


EXEC_PROMPT = """你是 Goal 引擎的【执行者】。你只执行当前这一步，不重新规划整个目标，也不修改验收标准。

状态包（JSON；若为空或仍是占位符，说明调用方没传 goal，直接输出 BLOCKED）：
{{start-1.text}}

规则：
1. 读状态包的 goal / tasks / next / workspace。
2. tasks 为空时，先把 goal 拆成 1-5 个可执行任务（简单目标 1 个即可）（每个任务有 id/desc/status/evidence），然后执行 next 指向的第一步。
3. 只做 next 指向的那一步；不要顺手做后面的步骤。
4. 所有产物写到 workspace 下，用可预测的文件名；不要写 workspace 之外的路径。
5. 不要伪造结果；没做完、做失败、缺依赖，都如实写。
6. 结束时给：做了什么（≤5 行）、证据（命令/文件/测试输出，≤20 行）、风险（≤3 行）。
7. 如果 workspace/.goal/verify.json 存在，按它的检查项交付；不要修改 verify.json。
8. 不要提问，直接动手。
8. 禁止调用 get_goal / plan 等 DSH 自身的目标工具；状态包就是唯一任务来源。

"""

JUDGE_PROMPT = """你是 Goal 引擎的【质检门】。你不执行任务，只判断目标是否完成、下一步做什么。
禁止调用 get_goal / plan 等 DSH 自身的目标工具；状态包就是唯一目标来源。

原始状态包：
{{start-1.text}}

执行者本轮报告：
{{exec-1.text}}

确定性验证（goal_verify.py，可能 skipped）：
{{verify-1.text}}

规则：
0. 先看确定性验证：ok=false → 必须 CONTINUE（即使执行者说完成），next 指向失败项；skipped=true 时才完全按报告证据判断。
0.1 verify.json 是验收标准，禁止修改（包括去 BOM）。verify 报解析/环境错误时判 BLOCKED，让调用方修环境，不要改 verify.json。
1. 以 goal 原文为准，逐条核对；tasks 是执行者的计划，不是目标的额外要求。goal 已完成且有证据即可 DONE；计划里未做的项若不影响 goal，可忽略并说明。
2. 证据不足、结果不一致、产物缺失、测试失败 → CONTINUE，并给出唯一 next。
3. 同一个 next 连续两轮没有新证据 → BLOCKED。
4. 不要相信「我完成了」这句话，只信证据（文件/命令输出/测试结果）。
5. 你不执行任务、不改文件。
6. 如果执行者报告里没有可验证的证据，判 CONTINUE，并明确要它补哪条证据。

必须原样输出 VERDICT 三选一（DONE / CONTINUE / BLOCKED），不要用「判定：PASS」代替。
ANALYSIS 最多 3 行，不要表格；整段 ≤800 字。
输出纯文本，最后四行严格如下（不要 markdown 代码块）：
ANALYSIS: <≤5 行>
VERDICT: DONE | CONTINUE | BLOCKED
NEXT: <CONTINUE 时唯一可执行动作；DONE/BLOCKED 写 ->
STATE: <单行 JSON：{"goal":"...","tasks":[{"id":"T1","desc":"...","status":"done|pending|blocked","evidence":"..."}],"next":"...","workspace":"...","iteration":N,"history":["..."]}>
"""

NORMALIZE_CODE = r'''text := inputText
verdict := ""
for _, line := range strings.Split(text, "\n") {
    t := strings.TrimSpace(line)
    if strings.HasPrefix(strings.ToUpper(t), "VERDICT:") {
        f := strings.Fields(strings.TrimSpace(t[len("VERDICT:"):]))
        if len(f) > 0 {
            verdict = strings.ToUpper(f[0])
        }
    }
}
if verdict == "" {
    up := strings.ToUpper(text)
    switch {
    case strings.Contains(text, "判定：PASS") || strings.Contains(text, "目标已完成") || strings.Contains(text, "全部完成") || strings.Contains(up, "PASS"):
        verdict = "DONE"
    case strings.Contains(up, "BLOCKED") || strings.Contains(text, "阻塞"):
        verdict = "BLOCKED"
    default:
        verdict = "CONTINUE"
    }
}
if verdict != "DONE" && verdict != "BLOCKED" && verdict != "CONTINUE" {
    if strings.Contains(verdict, "DONE") || strings.Contains(verdict, "PASS") {
        verdict = "DONE"
    } else if strings.Contains(verdict, "BLOCK") {
        verdict = "BLOCKED"
    } else {
        verdict = "CONTINUE"
    }
}
status := "GOAL_CONTINUE"
if verdict == "DONE" || verdict == "BLOCKED" {
    status = "GOAL_END"
}
var kept []string
for _, line := range strings.Split(text, "\n") {
    t := strings.TrimSpace(line)
    up := strings.ToUpper(t)
    if strings.HasPrefix(up, "VERDICT:") || strings.HasPrefix(up, "STATUS:") {
        continue
    }
    kept = append(kept, line)
}
result = strings.Join(kept, "\n") + "\nVERDICT: " + verdict + "\nSTATUS: " + status
'''

INIT_CODE = r'''ws := "{{workspace}}"
if strings.HasPrefix(ws, "{{") || strings.TrimSpace(ws) == "" {
    ws = "C:/Users/Administrator/Desktop/flowedge/.tmp/goal-workspace"
}
result = map[string]interface{}{
    "goal":      inputText,
    "tasks":     []interface{}{},
    "next":      "把目标拆成 1-5 个可执行任务（简单目标 1 个即可），并执行第一步",
    "workspace": ws,
    "iteration": 0,
    "history":   []interface{}{},
}
'''


def node(nid, ntype, x, y, data):
    return {"id": nid, "type": ntype, "position": {"x": x, "y": y}, "data": data}


def inner_workflow():
    nodes = [
        node("start-1", "start", 40, 300, {"prompt": ""}),
        node("exec-1", "agent_task", 250, 300, {
            "agent_id": "dsh",
            "timeout_ms": 900000,
            "workdir": "C:/Users/Administrator/Desktop/flowedge/.tmp/goal-workspace",
            "prompt": EXEC_PROMPT,
        }),
        node("verify-1", "command", 430, 300, {
            "command": "C:/Users/Administrator/.workbuddy/binaries/python/versions/3.13.12/python.exe",
            "args": json.dumps([
                "C:/Users/Administrator/Desktop/flowedge/scripts/goal_verify.py",
                "--workspace", "C:/Users/Administrator/Desktop/flowedge/.tmp/goal-workspace",
            ]),
            "workdir": "C:/Users/Administrator/Desktop/flowedge/.tmp/goal-workspace",
            "timeout_ms": 300000,
            "fail_on_nonzero": False,
        }),
        node("judge-1", "agent_task", 610, 300, {
            "agent_id": "dsh",
            "timeout_ms": 600000,
            "workdir": "C:/Users/Administrator/Desktop/flowedge/.tmp/goal-workspace",
            "prompt": JUDGE_PROMPT,
        }),
        node("normalize-1", "code", 790, 300, {"code": NORMALIZE_CODE, "timeout_ms": 30000}),
        node("end-1", "end", 970, 300, {}),
    ]
    edges = [
        {"id": "e1", "source": "start-1", "target": "exec-1"},
        {"id": "e2", "source": "exec-1", "target": "verify-1"},
        {"id": "e3", "source": "verify-1", "target": "judge-1"},
        {"id": "e4", "source": "judge-1", "target": "normalize-1"},
        {"id": "e5", "source": "normalize-1", "target": "end-1"},
    ]
    return {"name": "Goal-Step · 单步执行 + 质检（内部）", "nodes": nodes, "edges": edges}


def outer_workflow(inner_id):
    nodes = [
        node("start-1", "start", 40, 300, {"prompt": "{{goal}}"}),
        node("init-1", "code", 250, 300, {"code": INIT_CODE, "timeout_ms": 30000}),
        node("loop-1", "loop", 520, 300, {
            "workflow_id": inner_id,
            "mode": "until",
            "condition_type": "contains",
            "compare_value": "GOAL_END",
            "max_iterations": 8,
            # 循环总超时：必须显式给，否则 loop 走节点默认 180s，第一次 DSH 调用就被杀。
            "timeout_ms": 28800000,
        }),
        node("gate-1", "if_else", 820, 300, {
            "condition_type": "contains",
            "compare_value": "VERDICT: DONE",
        }),
        node("fail-1", "command", 1060, 440, {
            "command": "cmd",
            "args": "[\"/c\", \"echo GOAL_BLOCKED && exit 1\"]",
            "fail_on_nonzero": True,
            "timeout_ms": 10000,
        }),
        node("end-ok", "end", 1080, 160, {"prompt": "目标完成"}),
        node("end-fail", "end", 1320, 440, {"prompt": "目标未完成或被阻塞"}),
    ]
    edges = [
        {"id": "e1", "source": "start-1", "target": "init-1"},
        {"id": "e2", "source": "init-1", "target": "loop-1"},
        {"id": "e3", "source": "loop-1", "target": "gate-1"},
        {"id": "e4", "source": "gate-1", "target": "end-ok", "sourceHandle": "true"},
        {"id": "e5", "source": "gate-1", "target": "fail-1", "sourceHandle": "false"},
        {"id": "e6", "source": "fail-1", "target": "end-fail"},
    ]
    return {"name": "Goal · 目标引擎（执行→质检→循环直到完成）", "nodes": nodes, "edges": edges}


def main():
    inner = post("/api/workflows", inner_workflow())
    inner_id = inner["id"]
    print("inner workflow id =", inner_id)

    outer = post("/api/workflows", outer_workflow(inner_id))
    outer_id = outer["id"]
    print("outer workflow id =", outer_id)

    os.makedirs(OUT_DIR, exist_ok=True)
    for name, obj in (("goal-step.json", inner), ("goal.json", outer)):
        with open(os.path.join(OUT_DIR, name), "w", encoding="utf-8") as f:
            json.dump(obj, f, ensure_ascii=False, indent=2)
    print("saved to", OUT_DIR)


if __name__ == "__main__":
    main()
