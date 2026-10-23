#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""FlowEdge Windows 原生构建：前端 → 内嵌 → 单个 FlowEdge.exe

产出：dist/FlowEdge.exe
用户拿到这个 exe 双击即可：
  - 自带 Web 界面（前端已内嵌，不需要 Node）
  - 自带数据库（SQLite，不需要 PostgreSQL / Docker）
  - 数据写在 exe 同目录的 data/ 下（不需要管理员权限）

可选依赖（缺了不影响启动，界面右上角「环境」会提示怎么补）：
  Ollama    → LLM 推理
  ComfyUI   → 文生图
  ffmpeg    → 视频合成
  Python    → 语音转写/合成（backend/local-services）

用法：
  python scripts/build-windows.py               # 完整构建
  python scripts/build-windows.py --skip-ui     # 只编后端（前端产物已存在时）
  python scripts/build-windows.py --version 1.0.0
"""

import argparse
import os
import shutil
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FRONTEND = os.path.join(ROOT, "frontend")
FRONTEND_DIST = os.path.join(FRONTEND, "dist")
STATIC_DIR = os.path.join(ROOT, "backend", "web", "static")
LOCAL_SERVICES = os.path.join(ROOT, "backend", "local-services")
SERVICES_EMBED = os.path.join(ROOT, "backend", "web", "services", "files")
BACKEND = os.path.join(ROOT, "backend")
OUT_DIR = os.path.join(ROOT, "dist")

# 目标平台（可被命令行覆盖，用于交叉构建 Linux / macOS 二进制）
GOOS = "windows"
GOARCH = "amd64"
OUTPUT_NAME = ""


def log(msg):
    print("[build] %s" % msg, flush=True)


def fail(msg):
    print("[build] 失败：%s" % msg, file=sys.stderr, flush=True)
    sys.exit(1)


def which(name):
    return shutil.which(name) or shutil.which(name + ".cmd") or shutil.which(name + ".exe")


def run(cmd, cwd=None, env=None):
    log("$ " + " ".join(cmd))
    proc = subprocess.run(cmd, cwd=cwd, env=env)
    if proc.returncode != 0:
        fail("命令返回 %d：%s" % (proc.returncode, " ".join(cmd)))


def build_frontend():
    npm = which("npm")
    if not npm:
        fail("未找到 npm。请安装 Node.js 18+（或改用 --skip-ui 复用已有前端产物）")

    if not os.path.isdir(os.path.join(FRONTEND, "node_modules")):
        log("安装前端依赖（首次会慢一些）…")
        run([npm, "install", "--no-audit", "--no-fund"], cwd=FRONTEND)
    else:
        log("前端依赖已存在，跳过安装")

    log("构建前端 …")
    run([npm, "run", "build"], cwd=FRONTEND)
    if not os.path.exists(os.path.join(FRONTEND_DIST, "index.html")):
        fail("前端构建产物缺失：%s" % FRONTEND_DIST)


def embed_frontend():
    """把 dist 复制到 backend/web/static（go:embed 的源目录）"""
    os.makedirs(STATIC_DIR, exist_ok=True)
    # 只清掉旧的构建产物，保留 .placeholder（干净 clone 时目录必须存在）
    for name in os.listdir(STATIC_DIR):
        if name == ".placeholder":
            continue
        path = os.path.join(STATIC_DIR, name)
        shutil.rmtree(path) if os.path.isdir(path) else os.remove(path)

    for name in os.listdir(FRONTEND_DIST):
        src = os.path.join(FRONTEND_DIST, name)
        dst = os.path.join(STATIC_DIR, name)
        shutil.copytree(src, dst) if os.path.isdir(src) else shutil.copy2(src, dst)
    log("前端产物已复制到 backend/web/static（将被内嵌进 exe）")


def embed_services():
    """把语音服务脚本复制进 embed 目录（单文件部署时可用 --export-services 导出）"""
    if not os.path.isdir(LOCAL_SERVICES):
        log("未找到 local-services，跳过语音服务内嵌")
        return
    os.makedirs(SERVICES_EMBED, exist_ok=True)
    # 先清掉上一轮留下的文件（保留 .placeholder），保证构建可重复
    for name in os.listdir(SERVICES_EMBED):
        if name == ".placeholder":
            continue
        path = os.path.join(SERVICES_EMBED, name)
        shutil.rmtree(path) if os.path.isdir(path) else os.remove(path)

    # 白名单：只带用户需要的服务脚本与依赖清单（不含测试脚本）
    include = {"asr_server.py", "tts_server.py", "requirements.txt"}
    copied = 0
    for name in sorted(os.listdir(LOCAL_SERVICES)):
        if name not in include:
            continue
        shutil.copy2(os.path.join(LOCAL_SERVICES, name), os.path.join(SERVICES_EMBED, name))
        copied += 1
    log("已内嵌 %d 个语音服务脚本（可执行 FlowEdge.exe --export-services 导出）" % copied)


def build_backend(version):
    go = which("go")
    if not go:
        fail("未找到 go 工具链。请安装 Go 1.25+ 或把 go 所在目录加入 PATH")

    os.makedirs(OUT_DIR, exist_ok=True)
    # 跨平台：Windows 默认 FlowEdge.exe，其它平台默认小写 flowedge
    if OUTPUT_NAME:
        out_name = OUTPUT_NAME
    elif GOOS == "windows":
        out_name = "FlowEdge.exe"
    else:
        out_name = "flowedge"
    out = os.path.join(OUT_DIR, out_name)
    ldflags = "-s -w -X main.version=%s" % version
    env = dict(os.environ)
    env.setdefault("CGO_ENABLED", "0")
    env["GOOS"] = GOOS
    env["GOARCH"] = GOARCH
    run([go, "build", "-trimpath", "-ldflags", ldflags, "-o", out, "."], cwd=BACKEND, env=env)

    size = os.path.getsize(out) / (1024 * 1024)
    log("已生成 %s（%.1f MB，GOOS=%s GOARCH=%s）" % (out, size, GOOS, GOARCH))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--skip-ui", action="store_true", help="跳过前端构建，直接内嵌已有产物")
    ap.add_argument("--version", default=time.strftime("%Y.%m.%d"), help="版本号（写入二进制）")
    ap.add_argument("--goos", default="windows", help="交叉构建目标系统（windows/linux/darwin），默认 windows")
    ap.add_argument("--goarch", default="amd64", help="交叉构建目标架构（amd64/arm64），默认 amd64")
    ap.add_argument("--output", default="", help="输出二进制名（默认 windows=FlowEdge.exe，其它=flowedge）")
    args = ap.parse_args()

    global GOOS, GOARCH, OUTPUT_NAME
    GOOS, GOARCH, OUTPUT_NAME = args.goos, args.goarch, args.output

    if GOOS == "windows":
        log("目标平台：Windows 原生单文件（无 Docker / 无 WSL / 无 Node 运行时依赖）")
    else:
        log("目标平台：%s/%s 原生单文件（CGO_ENABLED=0，无运行时依赖）" % (GOOS, GOARCH))

    if args.skip_ui:
        # --skip-ui 的语义是「不重新构建前端」，不是「不内嵌前端」。
        # 只要 frontend/dist 有产物就照常复制进 go:embed 目录 —— 否则会出现
        # 「前端改了、也 npm run build 了、但 exe 里还是旧界面」这种最难查的情况
        # （2026-10-22 实际踩到：首页仍引用旧 hash）。
        if os.path.isdir(FRONTEND_DIST) and os.path.exists(os.path.join(FRONTEND_DIST, "index.html")):
            log("跳过前端构建（沿用 frontend/dist 现有产物）")
            embed_frontend()
        elif os.path.exists(os.path.join(STATIC_DIR, "index.html")):
            log("跳过前端构建（frontend/dist 不存在，沿用 backend/web/static）")
        else:
            fail("既没有 frontend/dist 也没有 backend/web/static 的前端产物，先跑一次不带 --skip-ui 的构建")
    else:
        build_frontend()
        embed_frontend()

    embed_services()  # 语音服务脚本与前端无关，始终内嵌
    exe = build_backend(args.version)

    print()
    log("完成。部署方式：")
    log("  1) 把 %s 复制到任意目录" % os.path.basename(exe))
    log("  2) 双击运行（或运行 FlowEdge-启动.bat）")
    log("  3) 浏览器打开 http://127.0.0.1:8080")
    log("  数据保存在 exe 同目录的 data/ 下，卸载即删目录")
    log("  可选：FlowEdge.exe --export-services 导出语音服务脚本（需 Python）")


if __name__ == "__main__":
    main()
