@echo off
title FlowEdge Desktop
cd /d "%~dp0desktop"
if not exist node_modules\electron (
  echo [FlowEdge] 首次运行，安装桌面运行时...
  set ELECTRON_MIRROR=https://npmmirror.com/mirrors/electron/
  call npm install --no-audit --no-fund
)
start "" /b cmd /c "npx electron ."
