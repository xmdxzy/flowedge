#!/usr/bin/env python3
"""部署后验证：确认这一批修复真的上线了，而不是"exe 换了个文件"。

为什么要这个脚本（2026-10-26）：
    连续 7 轮攒下的修复（超时中断、慢镜告警、显存前瞻、重复实例告警、
    卸载接口、单镜看门狗、依赖日志落盘）都只经过单测，**没在任何线上进程里跑过**。
    "代码已落地"和"效果已验证"是两件事（AGENTS §0.13），中间差的就是这一跑。

用法（部署后、引擎起来之后）：
    python scripts/post-deploy-check.py
    python scripts/post-deploy-check.py --base http://127.0.0.1:8080

退出码：0 = 全通；1 = 有 FAIL。WARN 不算失败，但要人看一眼。
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request

# 不用 shell 的 http_proxy：本地端口被代理劫持过一次（AGENTS §0.6）
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def get(url, timeout=5):
    with OPENER.open(url, timeout=timeout) as r:
        return r.status, r.read().decode("utf-8", "replace")


def post(url, payload=None, timeout=8):
    data = json.dumps(payload or {}).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with OPENER.open(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


class Report:
    def __init__(self):
        self.fails = []
        self.warns = []
        self.oks = []

    def ok(self, msg):
        self.oks.append(msg)
        print(f"  [PASS] {msg}")

    def warn(self, msg):
        self.warns.append(msg)
        print(f"  [WARN] {msg}")

    def fail(self, msg):
        self.fails.append(msg)
        print(f"  [FAIL] {msg}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8080")
    args = ap.parse_args()
    base = args.base.rstrip("/")
    rep = Report()

    print("[1] 引擎活着吗")
    try:
        st, body = get(base + "/api/health")
        if st == 200 and '"ok"' in body:
            health = json.loads(body)
            rep.ok(f"引擎在线，版本 {health.get('version')}，uptime {health.get('uptime_seconds')}s")
        else:
            rep.fail(f"/api/health 返回 {st}")
            print("\n引擎没起来，后面的检查没有意义。")
            return 1
    except Exception as e:  # noqa: BLE001
        rep.fail(f"连不上 {base}：{e}")
        print("\n引擎没起来，后面的检查没有意义。")
        return 1

    print("[2] 出片前预检是否带 warnings（轮 8：显存前瞻 + 重复指令提醒）")
    st, body = post(base + "/api/preflight", {"pipeline": "image", "params": {"prompt": "post-deploy-check"}})
    if st == 200:
        data = json.loads(body)
        if "warnings" in data:
            rep.ok(f"preflight 带 warnings 字段（{len(data.get('warnings') or [])} 条）")
        else:
            rep.fail("preflight 没有 warnings 字段——exe 还是旧的")
    else:
        rep.warn(f"preflight 返回 {st}（可能是依赖没起，不代表代码旧）：{body[:120]}")

    print("[3] 卸载接口是否存在（轮 6：卸权重不杀进程）")
    # 对不存在的依赖发请求：新版本回 404 且 body 含「未知依赖」；旧版本路由不存在也回 404，
    # 所以要看 body 区分"路由存在但依赖未知"和"路由根本不存在"。
    st, body = post(base + "/api/deps/__nonexistent__/unload")
    if st == 404 and "未知依赖" in body:
        rep.ok("卸载接口已上线（未知依赖被正确拒绝）")
    elif st == 404:
        rep.fail("404 但不像我们的接口——/unload 路由可能还没上线")
    else:
        rep.warn(f"意外状态 {st}：{body[:120]}")

    print("[4] 依赖状态里 running / managed 是否分得开")
    st, body = get(base + "/api/deps")
    if st == 200:
        data = json.loads(body)
        items = data.get("items") or []
        # 只有 ComfyUI 三实例会各占十几到二十 GB；ASR/TTS/Ollama 是 CPU 或托管进程，
        # 它们"没被本进程托管"通常只是引擎重启造成的，不该当成风险喊。
        risky = [i["name"] for i in items
                 if i.get("running") and not i.get("managed") and i["name"].startswith("comfy")]
        if risky:
            rep.warn(f"这些 ComfyUI 实例的端口是外部进程在服务：{risky}"
                     f"——若同一张卡上还有第二份，会抢显存（轮 7 已能在出片前告警）")
        else:
            rep.ok(f"依赖 {len(items)} 项，没有外部 ComfyUI 实例占端口")
    else:
        rep.warn(f"/api/deps 返回 {st}")

    print("[5] 依赖日志是否开始落盘（轮 2：logs/<name>.log）")
    logs_dir = os.path.join("运行", "logs")
    if not os.path.isdir(logs_dir):
        logs_dir = "logs"
    deps_logs = []
    if os.path.isdir(logs_dir):
        deps_logs = [f for f in os.listdir(logs_dir)
                     if f.endswith(".log") and f != "engine.log"]
    if deps_logs:
        rep.ok(f"依赖日志已落盘：{', '.join(sorted(deps_logs)[:5])}")
    else:
        rep.warn("还没有 logs/<依赖名>.log —— 需要**重启过某个依赖**才会产生"
                 "（旧的进程不是新代码拉起的，不会写日志）")

    print("[6] engine.log 里有没有新的可读时间线")
    engine_log = os.path.join(logs_dir, "engine.log")
    # 标记必须**只有新代码会写**，否则旧引擎会蒙混过关。
    # 反例：只用 "[shotlist]" 当标记，旧版的分镜告警也是这个前缀，检查直接假绿。
    markers = {
        "释放扩散侧模型": "换侧卸载留痕",
        "已请求 ComfyUI 中断": "超时中断留痕",
        "生成完成，用时": "逐镜耗时留痕",
        "显存前瞻": "显存前瞻留痕",
        "判定为换页/卡死": "看门狗留痕",
    }
    if os.path.isfile(engine_log):
        with open(engine_log, "r", encoding="utf-8", errors="replace") as f:
            text = f.read()
        hit = [desc for k, desc in markers.items() if k in text]
        if hit:
            rep.ok(f"engine.log 里已出现：{'、'.join(hit)}")
        else:
            rep.warn("engine.log 里还没有新的时间线标记——出片一次后再看")
    else:
        rep.warn(f"没有找到 {engine_log}")

    print("\n" + "=" * 60)
    print(f"结果：{len(rep.oks)} PASS / {len(rep.warns)} WARN / {len(rep.fails)} FAIL")
    if rep.fails:
        print("有 FAIL：上面的失败项说明 exe 或部署流程没对上，别急着出片。")
        return 1
    if rep.warns:
        print("没有 FAIL。WARN 多为「还没跑过」——出片一次后再跑本脚本复查。")
    else:
        print("全部通过。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
