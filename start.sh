#!/bin/bash
# FlowEdge 一键启动脚本

echo "🚀 启动 FlowEdge..."

# 检查 Docker
if ! command -v docker &> /dev/null; then
    echo "❌ 未找到 Docker，请先安装 Docker"
    exit 1
fi

# 启动服务
docker-compose up -d --build

echo ""
echo "✅ FlowEdge 已启动！"
echo "📱 前端: http://localhost:3000"
echo "🔧 后端: http://localhost:8080"
echo ""
echo "⚠️  请确保 Ollama 已在本地运行: ollama serve"
