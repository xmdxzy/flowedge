#!/usr/bin/env python3
"""一条命令完成「换 exe → 重启引擎 → 自动验证」，并在失败时给出回滚路径。

为什么要它（2026-10-26）：
    部署原本是三件事：跑 `运行/3-应用更新并重启.bat`、等引擎起来、再人工点一遍界面。
    中间任何一步没人做，攒了几轮的修复就永远停在 dist/ 里（这已经持续了 8 轮）。
    这里把三件事串成一条命令，并且**用脚本验证新接口真的在线**，而不是靠眼睛看。

用法：
    python scripts/deploy-and-verify.py              # 演练：只检查前置条件，不动进程
    python scripts/deploy-and-verify.py --yes        # 真部署：停引擎 → 换文件 → 起引擎 → 验证
    python scripts/deploy-and-verify.py --rollback   # 回滚到旧的 .old-* 备份

注意：
    - 不带 --yes 绝不杀进程、绝不写 运行/
    - 引擎由本脚本以「分离进程」拉起，所以脚本退出后它继续跑
    - 依赖服务（ComfyUI/ASR/TTS）不会被停：引擎没有停机钩子，它们本来就会留着，
      这样重启后不必重新冷加载
"""
import argparse
import glob
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
RUN_DIR = "运行"
DIST = os.path.join("dist", "FlowEdge.exe")
TARGET = os.path.join(RUN_DIR, "FlowEdge.exe")
HEALTH = "http://127.0.0.1:8080/api/health"


def health(timeout=3):
    try:
        with OPENER.open(HEALTH, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        return 0, str(e)


def port_free(timeout=15):
    deadline = time.time() + timeout
    while time.time() < deadline:
        st, _ = health(timeout=1)
        if st == 0:
            return True
        time.sleep(0.5)
    return False


def sha(path):
    import hashlib

    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def newest_backup():
    """最近一次被替换下来的 exe。

    必须按**修改时间**排，不能按文件名：目录里同时存在
    `FlowEdge.exe.old-20261026` 与 `FlowEdge.exe.old-swap` 这类命名，
    按字典序 `old-swap` 会排在 `old-2...` 之后被误选——而它其实是 10-23 的旧包。
    """
    cands = (glob.glob(os.path.join(RUN_DIR, "FlowEdge.exe.old-*")) +
             glob.glob(os.path.join(RUN_DIR, "FlowEdge.exe.bak-*")))
    cands = [p for p in cands if os.path.isfile(p)]
    if not cands:
        return None
    return max(cands, key=os.path.getmtime)


def smoke_test(port="18099", wait=60):
    """先在空闲端口把**待部署的那个 exe** 跑起来，确认它能启动再动线上。

    为什么值得多花这 20 秒：换 exe 是唯一会「把能用的系统换坏」的动作。
    先冒烟一次，坏就什么都不动；而不是先杀引擎、再发现新 exe 起不来。
    用独立 data 副本 + 独立端口，不碰 运行/ 里的真实数据。
    """
    tmp = os.path.join(".tmp", "smoke-deploy")
    if os.path.isdir(tmp):
        shutil.rmtree(tmp, ignore_errors=True)
    os.makedirs(os.path.join(tmp, "data"), exist_ok=True)
    exe = os.path.join(tmp, "FlowEdge.exe")
    shutil.copyfile(DIST, exe)
    pj = os.path.join("dist", "data", "pipelines.json")
    if os.path.isfile(pj):
        shutil.copyfile(pj, os.path.join(tmp, "data", "pipelines.json"))

    env = dict(os.environ, PORT=port)
    env.pop("FLOWEDGE_OPEN_BROWSER", None)
    flags = 0
    if os.name == "nt":
        flags = 0x00000008 | 0x00000200 | 0x08000000
    print(f"  冒烟：在 :{port} 起一份待部署的 exe（独立数据目录）")
    proc = subprocess.Popen([os.path.abspath(exe)], cwd=os.path.abspath(tmp), env=env,
                            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL, creationflags=flags, close_fds=True)
    url = f"http://127.0.0.1:{port}/api/health"
    try:
        for _ in range(wait):
            time.sleep(1)
            try:
                with OPENER.open(url, timeout=2) as r:
                    body = r.read().decode("utf-8", "replace")
                    print(f"  ✅ 冒烟通过：{body[:90]}")
                    return True
            except Exception:  # noqa: BLE001
                continue
        print("  ❌ 冒烟失败：60 秒没起来（不动线上，请先看日志）")
        return False
    finally:
        try:
            proc.kill()
        except Exception:  # noqa: BLE001
            pass


def preflight(do_smoke=True):
    ok = True
    if not os.path.isfile(DIST):
        print(f"  ❌ 找不到 {DIST}（先跑 python scripts/build-windows.py）")
        ok = False
    if not os.path.isdir(RUN_DIR):
        print(f"  ❌ 找不到部署目录 {RUN_DIR}/")
        ok = False
    if os.path.isfile(DIST) and os.path.isfile(TARGET):
        same = sha(DIST) == sha(TARGET)
        print(f"  {'✅' if same else 'ℹ️ '} dist 与 运行/ 的 exe "
              f"{'已一致（部署只差重启）' if same else '不一致（部署会替换）'}")
    st, body = health()
    if st:
        print(f"  ℹ️  引擎正在跑：{body[:80]}")
    else:
        print("  ℹ️  引擎当前没在跑")
    print(f"  回滚备份：{newest_backup() or '（无）'}")
    if ok and do_smoke:
        ok = smoke_test()
    return ok


def do_rollback():
    bak = newest_backup()
    if not bak:
        print("没有可回滚的备份")
        return 1
    print(f"回滚：停引擎 → 用 {bak} 覆盖 {TARGET}")
    subprocess.run(["taskkill", "/IM", "FlowEdge.exe", "/F"],
                   capture_output=True, text=True)
    if not port_free():
        print("❌ 引擎 15 秒内没停下，放弃（避免半途替换）")
        return 1
    shutil.copyfile(bak, TARGET)
    print(f"✅ 已恢复 {bak}（{os.path.getsize(TARGET)//(1<<20)}MB）")
    print("   现在手动启动：双击 运行\\1-启动 FlowEdge.bat")
    return 0


def deploy():
    print("[1/5] 停引擎")
    subprocess.run(["taskkill", "/IM", "FlowEdge.exe", "/F"],
                   capture_output=True, text=True)
    if not port_free():
        print("  ❌ 8080 15 秒内没释放，中止（可能还有别的实例在跑）")
        return 1
    print("  ✅ 已停，端口已释放")

    print("[2/5] 备份 + 替换 exe")
    if os.path.isfile(TARGET):
        bak = TARGET + ".old-" + time.strftime("%Y%m%d-%H%M%S")
        shutil.move(TARGET, bak)
        print(f"  备份 → {bak}")
    shutil.copyfile(DIST, TARGET)
    if sha(DIST) != sha(TARGET):
        print("  ❌ 复制后 hash 不一致，中止（可用 --rollback 恢复）")
        return 1
    print(f"  ✅ 已替换（{os.path.getsize(TARGET)//(1<<20)}MB）")

    print("[3/5] 同步 data/pipelines.json（缺它引擎会拒绝启动）")
    src = os.path.join("dist", "data", "pipelines.json")
    dst = os.path.join(RUN_DIR, "data", "pipelines.json")
    if os.path.isfile(src):
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        shutil.copyfile(src, dst)
        print(f"  ✅ {dst}")
    else:
        print("  ℹ️  dist 里没有 pipelines.json，沿用现有配置")

    print("[4/5] 以分离进程启动引擎")
    flags = 0
    if os.name == "nt":
        flags = 0x00000008 | 0x00000200 | 0x08000000  # DETACHED | NEW_GROUP | NO_WINDOW
    subprocess.Popen([os.path.abspath(TARGET)], cwd=os.path.abspath(RUN_DIR),
                     stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                     stderr=subprocess.DEVNULL, creationflags=flags, close_fds=True)
    print("  已拉起，等待健康检查（最多 120 秒）")
    for _ in range(120):
        time.sleep(1)
        st, body = health()
        if st:
            print(f"  ✅ 引擎在线：{body[:100]}")
            break
    else:
        print("  ❌ 120 秒内没起来。看 运行/logs/engine.log；必要时 --rollback")
        return 1

    print("[5/5] 验证新接口")
    return subprocess.call([sys.executable, os.path.join("scripts", "post-deploy-check.py")])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--yes", action="store_true", help="真的执行（默认只演练）")
    ap.add_argument("--rollback", action="store_true", help="回滚到最近的备份")
    args = ap.parse_args()

    if os.getcwd().endswith("backend"):
        print("请在项目根目录运行（有 运行/ 和 dist/ 的那层）")
        return 2

    if args.rollback:
        return do_rollback()

    print("前置检查：")
    if not preflight():
        return 1
    if not args.yes:
        print("\n演练结束（未动进程、未写文件）。确认无误后加 --yes 执行。")
        return 0
    print()
    return deploy()


if __name__ == "__main__":
    sys.exit(main())
