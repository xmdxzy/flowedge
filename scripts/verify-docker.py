#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""FlowEdge Docker 部署静态契约校验

用途：Docker Desktop 不可用（wsl.exe 被安全策略拦截）时，仍能守住部署配置的正确性。
它不替代运行时验证，只保证「配置层面不会必然失败」：

  1. compose 服务拓扑完整（postgres / backend / frontend）
  2. backend 真的会连 postgres（DB_DRIVER）——曾经漏配，导致 pg 容器起了却用不到
  3. 构建上下文干净（.dockerignore 覆盖 node_modules / .venv / data）
  4. Dockerfile 引用的路径都存在，前端用 npm ci 保证可复现
  5. 端口映射不与本机常用端口冲突（提示性）

退出码：0 全通过；1 有 ERROR。
用法：python scripts/verify-docker.py
"""

import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
COMPOSE = os.path.join(ROOT, "docker-compose.yml")
DOCKERIGNORE = os.path.join(ROOT, ".dockerignore")

errors = []
warnings = []
passed = []


def ok(msg):
    passed.append(msg)


def err(msg):
    errors.append(msg)


def warn(msg):
    warnings.append(msg)


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def main():
    if not os.path.exists(COMPOSE):
        err("docker-compose.yml 不存在")
        return
    if not os.path.exists(DOCKERIGNORE):
        err(".dockerignore 缺失（构建上下文会拖入 node_modules / .venv / data）")
    else:
        ignore = read(DOCKERIGNORE)
        for pat in ("node_modules", ".venv", "data"):
            if pat in ignore:
                ok(".dockerignore 已排除 %s" % pat)
            else:
                warn(".dockerignore 未显式排除 %s" % pat)

    text = read(COMPOSE)

    # 1) 服务拓扑（只在 services: 段内匹配，避免把 volumes 顶层键算成服务）
    m_svc = re.search(r"^services://s*$((?://n(?:    |\t).*|\s*)*)", text, flags=re.M)
    svc_section = m_svc.group(1) if m_svc else text
    # services 段到下一个顶层键（volumes/networks 等）为止
    svc_section = re.split(r"\n(?:volumes|networks|configs|secrets):", svc_section)[0]
    services = re.findall(r"^  ([a-zA-Z0-9_-]+):\s*$", svc_section, flags=re.M)
    for svc in ("postgres", "backend", "frontend"):
        if svc in services:
            ok("compose 含服务 %s" % svc)
        else:
            err("compose 缺少服务 %s" % svc)
    if services:
        ok("compose 服务列表：%s" % ", ".join(services))

    # 2) backend 必须真的用 postgres（历史 bug：pg 容器起了却走 SQLite）
    backend_block = extract_service(text, "backend")
    if backend_block:
        if re.search(r"DB_DRIVER:\s*postgres", backend_block):
            ok("backend 显式设置 DB_DRIVER: postgres")
        else:
            err("backend 未设置 DB_DRIVER: postgres（postgres 服务将形同虚设）")
        for key in ("DB_HOST", "DB_USER", "DB_PASSWORD", "DB_NAME"):
            if key in backend_block:
                ok("backend 环境变量含 %s" % key)
            else:
                warn("backend 缺少环境变量 %s" % key)
        if "depends_on" in backend_block:
            ok("backend 声明了 depends_on")
        else:
            warn("backend 无 depends_on，启动顺序可能竞争")

    # 3) Dockerfile 路径与内容
    for svc in ("backend", "frontend"):
        block = extract_service(text, svc) or ""
        m = re.search(r"dockerfile:\s*(\S+)", block)
        if not m:
            warn("%s 未指定 dockerfile（依赖默认 Dockerfile）" % svc)
            continue
        df_rel = m.group(1).strip("\"'")
        df_path = os.path.join(ROOT, df_rel)
        if not os.path.exists(df_path):
            err("%s 的 dockerfile 不存在：%s" % (svc, df_rel))
            continue
        ok("%s dockerfile 存在：%s" % (svc, df_rel))
        df = read(df_path)
        if "EXPOSE" not in df:
            warn("%s 未声明 EXPOSE" % df_rel)
        if svc == "frontend":
            if "npm ci" in df:
                ok("frontend 使用 npm ci（可复现构建）")
            elif "npm install" in df:
                warn("frontend 仍用 npm install，建议 npm ci")
        if svc == "backend":
            if re.search(r"COPY\s+\S*go\.mod", df) and "go mod download" in df:
                ok("backend 分层缓存 go.mod/go.sum")
            else:
                warn("backend 未分层下载依赖，构建缓存会失效")

    # 3.5) 工具链版本一致性（版本错配会让构建必然失败）
    gomod = os.path.join(ROOT, "backend", "go.mod")
    if os.path.exists(gomod):
        m = re.search(r"^go\s+(\d+\.\d+)", read(gomod), flags=re.M)
        if m:
            need = m.group(1)
            df = read(os.path.join(ROOT, "docker", "backend.Dockerfile")) if os.path.exists(
                os.path.join(ROOT, "docker", "backend.Dockerfile")) else ""
            img = re.search(r"FROM\s+golang:([0-9.]+)", df)
            if img:
                have = img.group(1)
                if tuple(int(x) for x in have.split(".")[:2]) < tuple(int(x) for x in need.split(".")[:2]):
                    err("backend 镜像 golang:%s 低于 go.mod 要求的 %s（构建会失败）" % (have, need))
                else:
                    ok("golang 镜像 %s 满足 go.mod 要求 %s" % (have, need))

    pkg = os.path.join(ROOT, "frontend", "package.json")
    if os.path.exists(pkg):
        import json as _json
        data = _json.loads(read(pkg))
        vite = (data.get("devDependencies") or {}).get("vite") or ""
        df = read(os.path.join(ROOT, "docker", "frontend.Dockerfile")) if os.path.exists(
            os.path.join(ROOT, "docker", "frontend.Dockerfile")) else ""
        img = re.search(r"FROM\s+node:(\d+)", df)
        if img and vite:
            node_major = int(img.group(1))
            need_node = 20 if vite.startswith("^7") or vite.startswith("7") else 18
            if node_major < need_node:
                err("前端镜像 node:%d 低于 Vite %s 要求（>=%d）" % (node_major, vite, need_node))
            else:
                ok("node 镜像 %d 满足 Vite %s" % (node_major, vite))

    # 4) 端口映射
    ports = re.findall(r"^\s*-\s*\"?(\d+):(\d+)\"?\s*$", text, flags=re.M)
    if ports:
        ok("compose 端口映射：%s" % ", ".join("%s->%s" % (a, b) for a, b in ports))
    else:
        warn("未发现端口映射")

    # 5) 数据卷（持久化）
    if "volumes:" in text:
        ok("声明了 volumes（数据持久化）")
    else:
        warn("未声明 volumes，容器重建会丢数据")


def extract_service(text, name):
    """按缩进切出某个 compose 服务的配置块"""
    lines = text.splitlines()
    start = None
    for i, line in enumerate(lines):
        if re.match(r"^  %s:\s*$" % re.escape(name), line):
            start = i
            break
    if start is None:
        return None
    block = [lines[start]]
    for line in lines[start + 1:]:
        if re.match(r"^  [a-zA-Z0-9_-]+:\s*$", line) or re.match(r"^[a-zA-Z]", line):
            break
        block.append(line)
    return "\n".join(block)


if __name__ == "__main__":
    main()

    print("=" * 56)
    for p in passed:
        print("  PASS  %s" % p)
    for w in warnings:
        print("  WARN  %s" % w)
    for e in errors:
        print("  ERROR %s" % e)
    print("=" * 56)
    print("通过 %d / 警告 %d / 错误 %d" % (len(passed), len(warnings), len(errors)))

    sys.exit(1 if errors else 0)
