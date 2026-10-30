@echo off
chcp 936 >nul
title Telegram 归档面板 - 关闭本窗口即停止服务
cd /d "%~dp0"

echo.
echo   ============================================================
echo      Telegram 归档面板
echo   ============================================================
echo.
echo      浏览器将在 3 秒后自动打开：
echo      http://127.0.0.1:8091
echo.
echo      如果没有自动打开，请手工把这个地址复制到浏览器
echo.
echo      注意：第一次使用必须先双击「首次登录归档.bat」
echo            登录一次 Telegram 账号，否则面板里没有数据
echo.
echo   ------------------------------------------------------------
echo      【重要】不要关闭这个黑色窗口
echo              关闭窗口 = 归档面板停止服务
echo   ------------------------------------------------------------
echo.

start "" powershell -NoProfile -WindowStyle Hidden -Command "Start-Sleep -Seconds 3; Start-Process 'http://127.0.0.1:8091'"

tdl.exe archive serve --addr 127.0.0.1:8091

echo.
echo   归档面板已停止。按任意键关闭窗口...
pause >nul
