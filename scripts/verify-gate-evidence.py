#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""验证「门 → 证据 → 聚合」这条链路在真机上真的通。

为什么需要这个脚本：
  `evidence` 的三态、`gateChecks` 的明细、`CR_clean` 的分母口径，
  这三样东西都可以在单元测试里绿着，而真机上一条数据都没有 ——
  2026-10-23 就是这样：20 条工作流里 10 条有门节点，但 evidence 目录里
  8 份信封的 `gateChecks` 全是 0（那些图从没在带信封的版本下跑过）。
  评测层拿不到门明细，自进化就是个空转的环。

  所以必须有这样一个脚本：**用真实 ffmpeg 产物跑三种情况，逐项核对信封内容**。

它做什么：
  1. 起一个临时实例（自己的 data 目录，不碰常驻 8080）
  2. 用现成素材跑三张图，覆盖三态：
       A 音画同步 + 宽松门 → 期望 passed
       B 音画漂移 + 严门   → 期望 failed（av_drift 判不过）
       C 同步 + 帧数不足   → 期望 failed（min_frames 判不过）
  3. 逐份读 evidence/*.json，核对：
       - result 三态正确、passed 与 result 一致
       - gateChecks 非空，且每条都有 detail（实测值）
       - av_drift / min_frames 的 ok 与素材事实相符
       - caseId / role / category 原样落库
       - schemaVersion >= 3
  4. 调 /api/evidence/summary 核对 CR_clean 分子分母
  5. 打印 PASS/FAIL 表，退出码 0/1

用法：
  python scripts/verify-gate-evidence.py                # 用默认素材
  python scripts/verify-gate-evidence.py --keep         # 保留临时目录以便排查
  python scripts/verify-gate-evidence.py --exe dist/FlowEdge.exe

前提：
  - ffmpeg/ffprobe 在 PATH（脚本要用它造同步素材、也用于核对事实）
  - 端口 18190 空闲
  - **不要在生产引擎跑着的时候用它**：临时实例是独立 data 目录，互不影响，
    但两个实例抢同一个 Ollama 会让耗时失真（本脚本不用 LLM，只跑 ffprobe，所以其实无所谓）
"""

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

PORT = 18190
BASE = "http://127.0.0.1:%d" % PORT
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

failures = []
checks_run = 0


def ok(msg):
    print("  \033[32mOK\033[0m   %s" % msg)


def bad(msg):
    global failures
    failures.append(msg)
    print("  \033[31mFAIL\033[0m %s" % msg)


def check(cond, msg):
    global checks_run
    checks_run += 1
    if cond:
        ok(msg)
    else:
        bad(msg)
    return cond


def http(path, method="GET", body=None, timeout=120):
    url = BASE + path
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    # 必须绕开代理：本机常驻 http_proxy 会把 127.0.0.1 也发给代理，拿到假 502
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode("utf-8") or "{}")
        except Exception:
            return e.code, {}


def wait_up(seconds=30):
    for i in range(seconds):
        try:
            st, _ = http("/api/health", timeout=2)
            if st == 200:
                return True
        except Exception:
            pass
        time.sleep(1)
    return False


def port_free(port):
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        s.close()


def make_sync_clip(path):
    """造一个音画严格同步的 1 秒片子（320x240 / 25 帧 / 有音轨）。"""
    cmd = [
        "ffmpeg", "-y", "-v", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=25",
        "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
        "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
        "-c:a", "aac", "-shortest", path,
    ]
    subprocess.run(cmd, check=True, capture_output=True)


def probe(path):
    """用 ffprobe 读出事实（脚本的期望值来自素材本身，不是写死的）。"""
    def one(stream, entries):
        cmd = ["ffprobe", "-v", "error", "-select_streams", stream,
               "-show_entries", entries, "-of", "json", path]
        out = subprocess.run(cmd, capture_output=True, text=True)
        try:
            d = json.loads(out.stdout or "{}")
            ss = d.get("streams") or []
            return ss[0] if ss else {}
        except Exception:
            return {}

    v = one("v:0", "stream=width,height,nb_frames,duration")
    a = one("a:0", "stream=duration")
    vd = float(v.get("duration") or 0)
    ad = float(a.get("duration") or 0)
    return {
        "width": int(v.get("width") or 0),
        "height": int(v.get("height") or 0),
        "frames": int(v.get("nb_frames") or 0),
        "vsec": vd,
        "asec": ad,
        "drift": abs(vd - ad) if ad and vd else 0.0,
    }


def build_graph(name, path, gate_data):
    body = {
        "name": name,
        "nodes": [
            {"id": "src", "type": "input_source", "position": {"x": 0, "y": 0},
             "data": {"kind": "video", "path": path}},
            {"id": "q", "type": "quality_gate", "position": {"x": 240, "y": 0}, "data": gate_data},
            {"id": "e", "type": "end", "position": {"x": 480, "y": 0}, "data": {}},
        ],
        "edges": [{"id": "e1", "source": "src", "target": "q"},
                  {"id": "e2", "source": "q", "target": "e"}],
    }
    st, d = http("/api/workflows", "POST", body)
    if st not in (200, 201):
        bad("建图失败 %s：%s" % (name, d))
        return 0
    return d.get("id") or 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--exe", default=os.path.join(REPO, "dist", "FlowEdge.exe"))
    ap.add_argument("--keep", action="store_true", help="保留临时目录")
    args = ap.parse_args()

    if not os.path.exists(args.exe):
        print("找不到 exe：%s（先跑 python scripts/build-windows.py）" % args.exe)
        return 2
    if not port_free(PORT):
        print("端口 %d 被占用，先关掉那里的进程" % PORT)
        return 2

    tmp = tempfile.mkdtemp(prefix="fe-gatecheck-")
    print("临时目录：%s" % tmp)
    exe = os.path.join(tmp, "FlowEdge.exe")
    shutil.copy2(args.exe, exe)

    # 素材：同步的现场造，漂移的优先用仓库里的真实产物（没有就也造一个）
    sync_mp4 = os.path.join(tmp, "sync.mp4")
    make_sync_clip(sync_mp4)
    drift_candidates = [
        os.path.join(REPO, "运行", "data", "generated", "film-shots-test.mp4"),
    ]
    drift_mp4 = next((p for p in drift_candidates if os.path.exists(p)), None)

    print("\n== 起临时实例（关准入闸，只跑 ffprobe 不占 GPU）==")
    env = dict(os.environ)
    env["PORT"] = str(PORT)
    env["FLOWEDGE_GOVERNOR"] = "off"
    proc = subprocess.Popen([exe], cwd=tmp, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        if not wait_up():
            print("实例没起来")
            return 2
        ok("实例已就绪 :%d" % PORT)

        s_fact = probe(sync_mp4)
        print("\n== 素材事实（ffprobe）==")
        print("  同步素材：%dx%d / %d 帧 / 视频 %.2fs 音频 %.2fs（差 %.3fs）" % (
            s_fact["width"], s_fact["height"], s_fact["frames"],
            s_fact["vsec"], s_fact["asec"], s_fact["drift"]))

        print("\n== 建图并跑三态 ==")
        # A：同步素材 + 宽松门 → 应 passed
        wf_a = build_graph("A-同步-宽松门", sync_mp4.replace("\\", "/"), {
            "min_frames": 20, "min_width": 300, "require_audio": True,
        })
        # C：同步素材 + 帧数门槛高于实际 → 应 failed（min_frames）
        wf_c = build_graph("C-同步-帧数不足", sync_mp4.replace("\\", "/"), {
            "min_frames": s_fact["frames"] + 100, "require_audio": True,
        })
        # B：漂移素材 + 严门 → 应 failed（av_drift）
        wf_b = 0
        if drift_mp4:
            wf_b = build_graph("B-漂移-严门", drift_mp4.replace("\\", "/"), {
                "min_frames": 1, "require_audio": True, "duration_tolerance": 0.5,
            })

        plans = [(wf_a, "A", "baseline", "case-sync-pass"),
                 (wf_c, "C", "baseline", "case-frames-fail")]
        if wf_b:
            plans.append((wf_b, "B", "candidate", "case-drift-fail"))

        run_ids = []
        for wid, tag, role, case in plans:
            st, d = http("/api/workflows/%d/run" % wid, "POST", {
                "invoked_by": "manual", "caseId": case, "role": role,
                "category": "technical", "datasetFingerprint": "gate-evidence-smoke",
                # 注：invoked_by 有白名单（manual/workbuddy/dsh/mcp/webhook/cron），
                # 不能随便写 -- 写错了起跑直接被拒
            })
            if st not in (200, 202):
                bad("起跑失败 wf%d：%s" % (wid, d))
                continue
            run_ids.append((d.get("run_id"), tag, case))
            time.sleep(4)  # 等队列落盘（只跑 ffprobe，通常 1-2 秒）

        evdir = os.path.join(tmp, "data", "evidence")
        deadline = time.time() + 60
        while time.time() < deadline:
            if os.path.isdir(evdir) and len(os.listdir(evdir)) >= len(run_ids):
                break
            time.sleep(1)

        print("\n== 核对信封 ==")
        by_run = {}
        for fn in os.listdir(evdir) if os.path.isdir(evdir) else []:
            if not fn.endswith(".json"):
                continue
            with open(os.path.join(evdir, fn), encoding="utf-8") as f:
                by_run[fn] = json.load(f)

        idx = {d.get("runId"): d for d in by_run.values()}
        expect = {"A": "passed", "B": "failed", "C": "failed"}
        for rid, tag, case in run_ids:
            d = idx.get(rid)
            if not d:
                bad("run %s（%s）没有信封" % (rid, tag))
                continue
            checks = d.get("gateChecks") or []
            check(d.get("result") == expect[tag],
                  "%s result=%s（期望 %s）" % (tag, d.get("result"), expect[tag]))
            check(bool(d.get("passed")) == (expect[tag] == "passed"),
                  "%s passed 与 result 自洽" % tag)
            check(len(checks) > 0, "%s gateChecks 非空（%d 项）" % (tag, len(checks)))
            check(all(c.get("detail") for c in checks),
                  "%s 每条检查都带实测值" % tag)
            check(d.get("caseId") == case, "%s caseId 原样落库（%s）" % (tag, d.get("caseId")))
            check(d.get("role") == ("candidate" if tag == "B" else "baseline"),
                  "%s role 原样落库（%s）" % (tag, d.get("role")))
            check(int(d.get("schemaVersion") or 0) >= 3,
                  "%s schemaVersion=%s ≥ 3" % (tag, d.get("schemaVersion")))
            # 版本号必须是引擎给的事实：没有它，评测只能靠 role 区分"改前/改后"，
            # 而 role 是评测层自己标的（可能标错、标错也没人发现）。
            # 起跑时会自动给"从没快照过"的图建基线，所以这里应当 >= 1。
            check(int(d.get("workflowVersion") or 0) >= 1,
                  "%s workflowVersion=%s ≥ 1（起跑自动立基线）" % (tag, d.get("workflowVersion")))

            names = {c.get("name"): c for c in checks}
            if tag == "A":
                check((names.get("av_drift") or {}).get("ok") is True,
                      "A av_drift 通过（素材实际差 %.3fs）" % s_fact["drift"])
                check((names.get("min_frames") or {}).get("ok") is True,
                      "A min_frames 通过（实际 %d 帧）" % s_fact["frames"])
            if tag == "C":
                check((names.get("min_frames") or {}).get("ok") is False,
                      "C min_frames 判不过（实际 %d 帧 < 门限）" % s_fact["frames"])
            if tag == "B":
                f = probe(drift_mp4)
                check((names.get("av_drift") or {}).get("ok") is False,
                      "B av_drift 判不过（素材实际差 %.3fs > 0.5s）" % f["drift"])

        # ---- 输入注入（vars）----
        # 没有它，「跑 8 个 case」就是把同一张图同一个 prompt 跑 8 遍：
        # 只能测稳定性，测不出"换个输入还成不成"。配对评测的前提是**同题**，
        # 而同题要先有"题"。
        print("\n== 核对输入注入（vars 真的改变执行输入）==")
        tpl = "请介绍{{topic}}，控制在{{n}}句内，不要出现{{missing}}"
        st, d = http("/api/workflows", "POST", {
            "name": "vars-probe",
            "nodes": [
                {"id": "s", "type": "start", "position": {"x": 0, "y": 0},
                 "data": {"prompt": tpl}},
                {"id": "e", "type": "end", "position": {"x": 240, "y": 0}, "data": {}},
            ],
            "edges": [{"id": "e1", "source": "s", "target": "e"}],
        })
        wf_vars = (d or {}).get("id") or 0
        if not wf_vars:
            bad("建 vars 探针图失败：%s" % d)
        else:
            seen = {}
            # n 故意传**整数**：MCP 的 vars 声明是 {"type":"object"}，
            # 外部智能体最自然写 {"n": 2}。这里替它先把这条路的坑踩掉。
            for case_id, topic, n in [("vars-moon", "月亮", 2), ("vars-quantum", "量子计算", 3)]:
                st, d = http("/api/workflows/%d/run" % wf_vars, "POST", {
                    "invoked_by": "manual", "caseId": case_id, "role": "baseline",
                    "vars": {"topic": topic, "n": n},
                })
                if st not in (200, 202):
                    bad("vars 起跑失败 %s：%s" % (case_id, d))
                    continue
                seen[case_id] = d.get("run_id")
            time.sleep(4)
            texts = {}
            for case_id, rid in seen.items():
                st, d = http("/api/runs/%s" % rid)
                outs = {n.get("id"): (n.get("output") or {}) for n in (d.get("node_states") or [])}
                texts[case_id] = str((outs.get("s") or {}).get("text") or "")
            if len(texts) == 2:
                a = texts.get("vars-moon", "")
                b = texts.get("vars-quantum", "")
                check(a != b, "两个 case 的实际输入不同（%r vs %r）" % (a[:18], b[:18]))
                check("{{topic}}" not in a and "{{n}}" not in a,
                      "已提供的占位被替换（%r）" % a[:24])
                # 缺失的 key 必须原样保留：静默变空会让 case 跑出空输入却显示成功
                check("{{missing}}" in a, "缺失的 key 原样保留（不静默变空）")
                check("月亮" in a and "量子计算" in b, "替换值正确落到对应 case")
                # 数字必须按人类写法进来：2 而不是 2.000000（它直接进提示词）
                check("2句" in a and "3句" in b, "数字按人类写法替换（%r）" % a[:26])

        print("\n== 核对聚合（CR_clean 必须等于 通过/（通过+门判不过））==")
        st, d = http("/api/evidence/summary?limit=50")
        rep = (d or {}).get("report") or {}
        o = rep.get("overall") or {}
        if not run_ids:
            bad("一条都没起跑成功，无法核对聚合")
            return 1
        want = 1.0 / len(run_ids)
        got = o.get("crClean")
        check(got is not None, "CR_clean 不是 null（有门样本时必须有值）")
        if got is not None:
            check(abs(float(got) - want) < 1e-6,
                  "CR_clean=%.3f（期望 1/%d=%.3f）" % (float(got), len(run_ids), want))
        check(o.get("passed") == 1, "passed=1（实际 %s）" % o.get("passed"))
        check(o.get("failed") == len(run_ids) - 1,
              "failed=%d（实际 %s）" % (len(run_ids) - 1, o.get("failed")))
        check(isinstance(rep.get("diagnoses"), list),
              "diagnoses 是数组而不是 null（前端会 .map）")

        print("\n== 结果 ==")
        if failures:
            print("\033[31m%d 项 FAIL（共 %d 项检查）\033[0m" % (len(failures), checks_run))
            for f in failures:
                print("  - %s" % f)
            return 1
        print("\033[32m全部 %d 项通过\033[0m" % checks_run)
        return 0
    finally:
        try:
            proc.terminate()
            proc.wait(timeout=10)
        except Exception:
            try:
                proc.kill()
            except Exception:
                pass
        if args.keep:
            print("临时目录已保留：%s" % tmp)
        else:
            shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
