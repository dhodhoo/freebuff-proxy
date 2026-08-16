@echo off
rem gen-token.cmd - wrapper for gen-freebuff-token.ps1 with ExecutionPolicy Bypass
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0gen-freebuff-token.ps1" %*
