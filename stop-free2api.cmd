@echo off
setlocal
cd /d "%~dp0"

set "FREE2API_EXE=%CD%\free2api.exe"
set "FREE2API_PID_FILE=%CD%\free2api.pid"
if not exist "%FREE2API_PID_FILE%" (
  echo Free2API is not running: no PID file.
  exit /b 0
)

set /p FREE2API_PID=<"%FREE2API_PID_FILE%"
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:FREE2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:FREE2API_EXE)) { exit 0 } } catch {}; exit 1"
if errorlevel 1 (
  echo Removed stale PID file; no process was stopped.
  del /q "%FREE2API_PID_FILE%" >nul 2>&1
  exit /b 0
)

taskkill /PID %FREE2API_PID% /T /F >nul 2>&1
if errorlevel 1 (
  echo Failed to stop Free2API PID=%FREE2API_PID%.
  exit /b 1
)
del /q "%FREE2API_PID_FILE%" >nul 2>&1
echo Free2API stopped.
endlocal
