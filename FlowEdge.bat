@echo off
chcp 65001 >nul
title FlowEdge
cd /d "%~dp0"

set "EXE=%~dp0dist\FlowEdge.exe"
if not exist "%EXE%" set "EXE=%~dp0FlowEdge.exe"
if not exist "%EXE%" (
  echo.
  echo   [FlowEdge] 没找到 FlowEdge.exe
  echo   请先构建：python scripts\build-windows.py
  echo.
  pause
  exit /b 1
)

echo.
echo   FlowEdge 正在启动，浏览器会自动打开 http://127.0.0.1:8080
echo   关闭本窗口即停止服务；数据保存在 exe 同目录的 data\ 下。
echo.

set FLOWEDGE_OPEN_BROWSER=1
"%EXE%"
pause
