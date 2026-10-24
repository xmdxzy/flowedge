#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""Goal 引擎的确定性验证器。

用途：在 Goal-Step 的 exec 与 judge 之间跑一次「不靠模型」的检查，
把退出码 / 文件字节 / sha256 作为硬证据交给质检门。

规格文件：<workspace>/.goal/verify.json
{
  "checks": [
    {"name": "a.txt", "type": "file", "path": "a.txt", "equals": "A"},
    {"name": "pytest", "type": "cmd", "cmd": "python", "args": ["-m", "pytest", "-q"],
     "cwd": ".", "expect_exit": 0, "timeout_sec": 120}
  ]
}

没有规格文件时输出 skipped=true（不阻断，质检门按报告判断）。
永远以 0 退出：验证失败是「证据」，不是「节点故障」；由 judge 决定下一步。
"""
import argparse
import hashlib
import json
import os
import subprocess
import sys


def fail(msg):
    print(json.dumps({"ok": False, "skipped": False, "error": msg, "checks": []},
                     ensure_ascii=False))
    return 0


def check_file(ws, c):
    rel = str(c.get("path", "")).strip()
    if not rel:
        return False, "path 为空"
    p = rel if os.path.isabs(rel) else os.path.join(ws, rel)
    if not os.path.exists(p):
        return False, "文件不存在: %s" % p
    if not os.path.isfile(p):
        return False, "不是文件: %s" % p
    data = open(p, "rb").read()
    detail = "exists bytes=%d" % len(data)
    if "equals" in c:
        want = str(c["equals"]).encode("utf-8")
        if data != want:
            return False, detail + " content_mismatch"
        detail += " equals=ok"
    if "contains" in c:
        if str(c["contains"]).encode("utf-8") not in data:
            return False, detail + " contains_missing"
        detail += " contains=ok"
    if "sha256" in c:
        got = hashlib.sha256(data).hexdigest()
        if got.lower() != str(c["sha256"]).lower():
            return False, detail + " sha256=%s" % got
        detail += " sha256=ok"
    return True, detail


def check_cmd(ws, c):
    cmd = str(c.get("cmd", "")).strip()
    if not cmd:
        return False, "cmd 为空"
    args = [str(x) for x in (c.get("args") or [])]
    cwd = str(c.get("cwd", "."))
    cwd = cwd if os.path.isabs(cwd) else os.path.join(ws, cwd)
    timeout = int(c.get("timeout_sec", 120))
    expect = int(c.get("expect_exit", 0))
    try:
        p = subprocess.run([cmd] + args, cwd=cwd, capture_output=True,
                           timeout=timeout)
    except FileNotFoundError as e:
        return False, "命令不存在: %s (%s)" % (cmd, e)
    except subprocess.TimeoutExpired:
        return False, "超时 %ss" % timeout
    out = (p.stdout or b"").decode("utf-8", "replace")[-2000:]
    err = (p.stderr or b"").decode("utf-8", "replace")[-2000:]
    ok = p.returncode == expect
    detail = "exit=%d expect=%d" % (p.returncode, expect)
    if out.strip():
        detail += " stdout=" + out.strip().replace("\n", " | ")[:600]
    if err.strip():
        detail += " stderr=" + err.strip().replace("\n", " | ")[:600]
    return ok, detail


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--workspace", required=True)
    args = ap.parse_args()
    ws = os.path.abspath(args.workspace)
    spec_path = os.path.join(ws, ".goal", "verify.json")
    if not os.path.exists(spec_path):
        print(json.dumps({"ok": True, "skipped": True,
                          "reason": "no .goal/verify.json", "checks": []},
                         ensure_ascii=False))
        return 0
    try:
        spec = json.load(open(spec_path, encoding="utf-8-sig"))
    except Exception as e:
        return fail("verify.json 解析失败: %s" % e)
    checks = spec.get("checks") or []
    if not isinstance(checks, list) or not checks:
        print(json.dumps({"ok": True, "skipped": True,
                          "reason": "checks 为空", "checks": []},
                         ensure_ascii=False))
        return 0

    results = []
    all_ok = True
    for i, c in enumerate(checks):
        name = str(c.get("name") or ("check-%d" % (i + 1)))
        typ = str(c.get("type", "file"))
        if typ == "file":
            ok, detail = check_file(ws, c)
        elif typ == "cmd":
            ok, detail = check_cmd(ws, c)
        else:
            ok, detail = False, "未知 type: %s" % typ
        all_ok = all_ok and ok
        results.append({"name": name, "type": typ, "ok": ok, "detail": detail})

    print(json.dumps({"ok": all_ok, "skipped": False, "workspace": ws,
                      "checks": results}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
