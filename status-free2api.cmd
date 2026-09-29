@echo off
setlocal
cd /d "%~dp0"

set "FREE2API_EXE=%CD%\free2api.exe"
set "FREE2API_PID_FILE=%CD%\free2api.pid"
if not exist "%FREE2API_PID_FILE%" (
  echo Free2API is not running: no PID file.
  exit /b 1
)

set /p FREE2API_PID=<"%FREE2API_PID_FILE%"
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:FREE2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:FREE2API_EXE)) { exit 0 } } catch {}; exit 1"
if errorlevel 1 (
  echo Free2API is not running: stale PID file.
  exit /b 1
)

echo Free2API is running. PID=%FREE2API_PID%
if "%FREE2API_HEALTH_URL%"=="" set "FREE2API_HEALTH_URL=http://127.0.0.1:7863/healthz"
curl.exe --fail-with-body --silent --show-error --max-time 5 "%FREE2API_HEALTH_URL%"
set "FREE2API_STATUS=%ERRORLEVEL%"
echo.
exit /b %FREE2API_STATUS%
