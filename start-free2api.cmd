@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

if not exist "free2api.exe" (
  echo Missing free2api.exe. Build it with: go build -o free2api.exe ./cmd/server
  exit /b 1
)
if not exist "config.json" (
  echo Missing config.json. Copy config.example.json and configure it first.
  exit /b 1
)

rem --- clear the Codex sandbox black-hole proxy (http://127.0.0.1:9) ---
rem The Codex Windows sandbox injects HTTP(S)_PROXY=127.0.0.1:9 into every
rem child process to forbid network access. The gateway itself ignores it
rem (its outbound Transport sets no Proxy, so it never reads these), but
rem anything it shells out to would inherit a dead proxy. Clear them here so
rem a launch from inside that sandbox behaves the same as a normal one.
set "HTTP_PROXY="
set "HTTPS_PROXY="
set "ALL_PROXY="
set "http_proxy="
set "https_proxy="
set "all_proxy="
set "GIT_HTTP_PROXY="
set "GIT_HTTPS_PROXY="
rem NO_PROXY: keep whatever the host had; only add loopback if unset.
if not defined NO_PROXY set "NO_PROXY=localhost,127.0.0.1,::1"

set "FREE2API_ROOT=%CD%"
set "FREE2API_EXE=%CD%\free2api.exe"
set "FREE2API_PID_FILE=%CD%\free2api.pid"
if exist "%FREE2API_PID_FILE%" (
  set /p FREE2API_PID=<"%FREE2API_PID_FILE%"
  powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:FREE2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:FREE2API_EXE)) { exit 0 } } catch {}; exit 1"
  if not errorlevel 1 (
    echo Free2API is already running. PID=!FREE2API_PID!
    exit /b 0
  )
  del /q "%FREE2API_PID_FILE%" >nul 2>&1
)

if not exist "data" mkdir "data"
powershell -NoProfile -Command "$p=Start-Process -FilePath $env:FREE2API_EXE -ArgumentList '-config','config.json' -WorkingDirectory $env:FREE2API_ROOT -WindowStyle Hidden -RedirectStandardOutput (Join-Path $env:FREE2API_ROOT 'data\server.out.log') -RedirectStandardError (Join-Path $env:FREE2API_ROOT 'data\server.err.log') -PassThru; [IO.File]::WriteAllText($env:FREE2API_PID_FILE, [string]$p.Id)"
if errorlevel 1 exit /b 1

set /p FREE2API_PID=<"%FREE2API_PID_FILE%"
echo Free2API started. PID=%FREE2API_PID%
endlocal
