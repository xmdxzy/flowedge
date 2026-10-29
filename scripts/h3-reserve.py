#!/usr/bin/env python3
"""改 ComfyUI-H3 的 --reserve-vram，并**读回运行中进程的 argv** 验证真的生效。

为什么要这个脚本（2026-10-26）：
    ComfyUI 全量加载的判据是「模型 + extra_reserved」同时空闲
    （comfy/model_management.py:944，extra_reserved 来自 --reserve-vram，见 883 行）。
    24GB 卡上 H3 权重约 20GB、桌面基线约 3.1GB ⇒ 空闲只有 0.3–0.9GB，
    **4GB 预留永远凑不齐**，于是每次都退化成部分加载：权重留在内存、按步换进显存。
    实测同一镜 87s → 2269s（27 倍），然后撞节点超时。

    要验证「降到 1 会不会明显变快」，改配置只是一半 —— 另一半是确认**真的生效了**
    （§0.4：新开关上线必须验证实际生效值，不能只看配置文件）。

用法：
    python scripts/h3-reserve.py                     # 只读：当前生效值 + 配置值
    python scripts/h3-reserve.py --set 1 --dry-run   # 演练：只打印会做什么
    python scripts/h3-reserve.py --set 1 --yes       # 真改：写配置 → reload → 重启 H3 → 读回 argv

安全边界：
    - 不带 --yes 一律不写文件、不重启进程
    - 只改 data/deps.json 里的 h3.reserve_vram，其它内容原样保留
    - 回滚：--set 4 即可回到默认
"""
import argparse
import json
import os
import shutil
import sys
import time
import urllib.error
import urllib.request

# 本地端口一律绕开 http_proxy（AGENTS §0.6：被代理劫持过一次，表现为 502 假死）
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
ENGINE = "http://127.0.0.1:8080"
H3 = "http://127.0.0.1:8189"
DEPS_JSON = os.path.join("运行", "data", "deps.json")


def get(url, timeout=5):
    with OPENER.open(url, timeout=timeout) as r:
        return r.status, r.read().decode("utf-8", "replace")


def post(url, timeout=20):
    req = urllib.request.Request(url, data=b"{}", method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with OPENER.open(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def live_reserve():
    """从运行中的 H3 实例读 argv（这是唯一能证明「真的生效」的读数）。"""
    try:
        _, body = get(H3 + "/system_stats", timeout=8)
        argv = (json.loads(body).get("system") or {}).get("argv") or []
    except Exception:  # noqa: BLE001
        return None, "H3 未响应（可能没起，或忙到不回）"
    for i, a in enumerate(argv):
        if a == "--reserve-vram" and i + 1 < len(argv):
            return argv[i + 1], None
    return None, "argv 里没有 --reserve-vram"


def configured_reserve():
    if not os.path.isfile(DEPS_JSON):
        return None, f"没有 {DEPS_JSON}"
    with open(DEPS_JSON, "r", encoding="utf-8") as f:
        try:
            conf = json.load(f)
        except ValueError as e:
            return None, f"{DEPS_JSON} 不是合法 JSON：{e}"
    val = (conf.get("h3") or {}).get("reserve_vram")
    return val, None


def write_reserve(value):
    with open(DEPS_JSON, "r", encoding="utf-8") as f:
        conf = json.load(f)
    bak = DEPS_JSON + ".bak"
    shutil.copyfile(DEPS_JSON, bak)
    conf.setdefault("h3", {})["reserve_vram"] = value
    with open(DEPS_JSON, "w", encoding="utf-8") as f:
        json.dump(conf, f, ensure_ascii=False, indent=2)
    return bak


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--set", type=int, help="目标值（GB），例如 1 或 4")
    ap.add_argument("--dry-run", action="store_true", help="只打印会做什么")
    ap.add_argument("--yes", action="store_true", help="真的写文件并重启 H3")
    args = ap.parse_args()

    live, err = live_reserve()
    print(f"运行中生效值：{live if live is not None else '（读不到）'}"
          + (f"  ← {err}" if err else ""))
    conf, cerr = configured_reserve()
    print(f"配置里的值：  {conf if conf is not None else '（未配置，走代码默认 4）'}"
          + (f"  ← {cerr}" if cerr else ""))

    if args.set is None:
        print("\n只读模式。要改：--set <N> --dry-run 先演练，确认后加 --yes。")
        return 0

    if args.set < 0 or args.set > 8:
        print(f"拒绝：{args.set} 不像合理的预留值（0–8）")
        return 2

    print(f"\n计划：h3.reserve_vram = {args.set}")
    print("步骤：1) 备份并改写 data/deps.json  "
          "2) POST /api/deps/reload  "
          "3) stop + start comfy_h3（约 1 分钟，会清掉模型显存）  "
          "4) 读回 argv 验证")
    if args.dry_run or not args.yes:
        print("\n未执行（缺 --yes）。演练结束。")
        return 0

    bak = write_reserve(args.set)
    print(f"已改写配置（备份 {bak}）")

    st, body = post(ENGINE + "/api/deps/reload")
    if st != 200:
        print(f"reload 失败 {st}：{body[:200]}")
        return 1
    changes = (json.loads(body).get("changes") or {})
    hit = {k: v for k, v in changes.items() if k.startswith("comfy_h3")}
    print(f"reload 变更：{hit if hit else '（无差异——检查是否写对了字段）'}")

    post(ENGINE + "/api/deps/comfy_h3/stop")
    print("已停止 H3，等待进程退出…")
    time.sleep(2)
    st, body = post(ENGINE + "/api/deps/comfy_h3/start")
    print(f"启动请求：{st} {body[:120]}")
    # 冷启动最长等 180s
    for _ in range(60):
        time.sleep(3)
        v, e = live_reserve()
        if v is not None:
            print(f"\n读回 argv：--reserve-vram {v}")
            if str(v) == str(args.set):
                print(f"✅ 生效确认：配置 {args.set} → 运行中 {v}")
                return 0
            print(f"❌ 不一致：想要的 {args.set}，实际 {v}")
            return 1
    print("\n❌ 180s 内没等到 H3 就绪；用 --show 复查，或看 logs/comfy_h3.log")
    return 1


if __name__ == "__main__":
    sys.exit(main())
