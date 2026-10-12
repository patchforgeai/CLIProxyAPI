@echo off
rem Start Devyre CPA now: Docker Desktop, the cpa container and the tailnet URL.
rem Safe to run any time. To keep it running automatically, run once:
rem   powershell -ExecutionPolicy Bypass -File "%~dp0scripts\install-autostart.ps1"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\ensure-running.ps1"
if errorlevel 1 (
  echo.
  echo CPA did not come up. Log: %USERPROFILE%\.cli-proxy-api\logs\ensure-running.log
) else (
  echo.
  echo CPA is running. Panel: http://127.0.0.1:8317/management.html
)
pause
