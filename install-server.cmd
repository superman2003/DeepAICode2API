@echo off
cd /d "%~dp0"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\windows-server.ps1" -Action Install -OpenFirewall
if errorlevel 1 echo Installation failed. Read the error above or run start-foreground.cmd to see the application log.
pause
