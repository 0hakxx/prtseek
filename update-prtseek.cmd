@echo off
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0start-prtseek.ps1" -Update %*
if errorlevel 1 pause
