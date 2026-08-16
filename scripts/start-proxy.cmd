@echo off
rem start-proxy.cmd - wrapper for start-proxy.ps1 with ExecutionPolicy Bypass
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start-proxy.ps1" %*
if %ERRORLEVEL% NEQ 0 (
    echo.
    echo [ERROR] freebuff-proxy stopped with exit code %ERRORLEVEL%.
    pause
)
