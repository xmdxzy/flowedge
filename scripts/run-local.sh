#!/usr/bin/env bash
#
# run-local.sh —— FlowEdge 本地一键运行（Linux / macOS）
#
# 与 Windows 的「1-启动 FlowEdge.bat」等价：构建（按需）→ 后台拉起引擎
# → 探活 → 打印客户端地址。数据默认写在脚本同级的 data/ 下。
#
# 用法：
#   ./scripts/run-local.sh                 # 按需构建并后台启动，探活后打印 URL
#   ./scripts/run-local.sh --build         # 强制重新构建（哪怕产物已存在）
#   ./scripts/run-local.sh --foreground    # 前台运行（Ctrl+C 即停，不脱离终端）
#   ./scripts/run-local.sh --port 8081     # 自定义端口
#   ./scripts/run-local.sh --stop          # 停止后台引擎
#   ./scripts/run-local.sh --logs          # 跟随查看日志
#
# 依赖（缺了不影响启动，界面右上角「环境」会提示怎么补）：
#   go        Go 1.25+（构建时需要）
#   node/npm  Node 18+（首次构建前端时需要）
#   ffmpeg    ComfyUI / Ollama / Python 语音服务等为可选增强

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/dist/flowedge"
DATA_DIR="$ROOT/data"
LOG="$DATA_DIR/flowedge.log"
PORT="${PORT:-8080}"
BUILD=0
FG=0
ACTION="run"

for a in "$@"; do
  case "$a" in
    --build)     BUILD=1 ;;
    --foreground) FG=1 ;;
    --stop)      ACTION="stop" ;;
    --logs)      ACTION="logs" ;;
    --port)      : ;; # 占位：后一个参数为端口，下面处理
    *)           if [ "${prev:-}" = "--port" ]; then PORT="$a"; fi ;;
  esac
  prev="$a"
done

# 重新扫描 --port=xxxx 写法
for a in "$@"; do
  case "$a" in
    --port=*) PORT="${a#--port=}" ;;
  esac
done

mkdir -p "$DATA_DIR"

# ── 停止 ──────────────────────────────────────────────────────────────
stop_engine() {
  if [ -f "$DATA_DIR/flowedge.pid" ]; then
    PID="$(cat "$DATA_DIR/flowedge.pid" 2>/dev/null || true)"
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
      echo "→ 停止后台引擎（PID $PID）"
      kill "$PID" 2>/dev/null || true
      sleep 1
      kill -9 "$PID" 2>/dev/null || true
    fi
    rm -f "$DATA_DIR/flowedge.pid"
  fi
  # 兜底：按端口找监听进程
  if command -v fuser >/dev/null 2>&1; then
    fuser -k "${PORT}/tcp" 2>/dev/null || true
  fi
}

# ── 日志 ──────────────────────────────────────────────────────────────
if [ "$ACTION" = "logs" ]; then
  exec tail -f "$LOG"
fi

if [ "$ACTION" = "stop" ]; then
  stop_engine
  echo "已停止。"
  exit 0
fi

# ── 探活：先看是不是已经起来了（避免重复拉起）──────────────────────
# 本机可能有企业代理，本地端口必须绕过，否则会拿到 502 假死。
health_ok() {
  curl -s --noproxy '*' --max-time 3 "http://127.0.0.1:${PORT}/api/health" >/dev/null 2>&1
}

if health_ok; then
  echo "FlowEdge 已在 http://127.0.0.1:${PORT} 运行。"
  echo "  日志：tail -f $LOG"
  exit 0
fi

# ── 构建（按需）─────────────────────────────────────────────────────
source_newer() {
  # 若 backend 任意 .go 或 frontend/src 任意文件比二进制新，则视为需要重建
  if [ ! -x "$OUT" ]; then return 0; fi
  if find "$ROOT/backend" -name '*.go' -newer "$OUT" 2>/dev/null | grep -q .; then return 0; fi
  if [ -d "$ROOT/frontend/src" ] && find "$ROOT/frontend/src" -type f -newer "$OUT" 2>/dev/null | grep -q .; then return 0; fi
  return 1
}

if [ "$BUILD" -eq 1 ] || source_newer; then
  echo "→ 构建 FlowEdge（Linux 原生单文件）…"
  ( cd "$ROOT" && python3 scripts/build-windows.py --goos linux --goarch amd64 --output flowedge )
  if [ ! -x "$OUT" ]; then
    echo "构建失败：未生成 $OUT" >&2
    exit 1
  fi
else
  echo "→ 复用已有 $OUT（源码无变更，跳过构建）"
fi

# ── 启动 ──────────────────────────────────────────────────────────────
export FLOWEDGE_OPEN_BROWSER=0
export PORT="$PORT"

if [ "$FG" -eq 1 ]; then
  echo "→ 前台启动（Ctrl+C 停止）：$OUT"
  exec "$OUT"
fi

echo "→ 后台启动引擎（日志：$LOG）…"
nohup "$OUT" >"$LOG" 2>&1 &
PID=$!
echo "$PID" > "$DATA_DIR/flowedge.pid"

# ── 健康轮询 ────────────────────────────────────────────────────────
echo -n "→ 等待引擎就绪"
deadline=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  if health_ok; then
    echo ""
    echo ""
    echo "  FlowEdge 已就绪："
    echo "    客户端： http://127.0.0.1:${PORT}"
    echo "    日志：   tail -f $LOG"
    echo "    停止：   ./scripts/run-local.sh --stop"
    exit 0
  fi
  echo -n "."
  sleep 2
done

echo ""
echo "引擎 120s 内未就绪，最后 30 行日志：" >&2
tail -n 30 "$LOG" >&2 || true
echo "可观察日志：tail -f $LOG" >&2
exit 1
