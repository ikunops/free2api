@echo off
setlocal EnableDelayedExpansion

rem ===========================================================================
rem  install-keepalive.cmd -- register the windowless free2api watchdog.
rem
rem  WHY: a Task Scheduler action that is a console program (cmd.exe and/or
rem  free2api.exe) makes Windows allocate a console every time it runs. Windows 11
rem  uses Windows Terminal as the default console host, so that console is a real
rem  visible window -- it flashes once per trigger. The fix is to make the action
rem  wscript.exe, a GUI-subsystem host that never allocates a console, and let
rem  scripts\keepalive.vbs start the gateway hidden.
rem
rem  USAGE (run from an ELEVATED prompt):
rem      install-keepalive.cmd [minimal|standard|full]
rem
rem      minimal   only enable the already-registered task (no elevation needed)
rem      standard  default: refresh the launcher files and (re)register the task
rem      full      standard + install the pre-commit guard for the launcher files
rem
rem  After it finishes the task is registered and enabled. It does NOT call
rem  schtasks /query for verification: that tool answers ERROR_PATH_NOT_FOUND
rem  for this task on some hosts, which would look like a failure.
rem ===========================================================================

set "MODE=%~1"
if "%MODE%"=="" set "MODE=standard"
set "TASK_NAME=free2api-keepalive"

set "SCRIPT_DIR=%~dp0"
if "%SCRIPT_DIR:~-1%"=="\" set "SCRIPT_DIR=%SCRIPT_DIR:~0,-1%"

rem Two supported layouts:
rem   repo layout   <root>\scripts\install-keepalive.cmd   gateway in <root>
rem   flat layout   <deploy>\scripts\install-keepalive.cmd gateway in <deploy>
set "PKG_DIR=%SCRIPT_DIR%"
if exist "%PKG_DIR%\..\free2api.exe" (
  for %%I in ("%PKG_DIR%\..") do set "DEPLOY_DIR=%%~fI"
) else if exist "%PKG_DIR%\free2api.exe" (
  set "DEPLOY_DIR=%PKG_DIR%"
) else (
  echo [error] free2api.exe not found in "%PKG_DIR%" or its parent.
  echo         Put this script beside the gateway or in ^<root^>\scripts.
  exit /b 2
)

set "LAUNCHER=%DEPLOY_DIR%\scripts\keepalive.vbs"
set "XMLTEMPLATE=%PKG_DIR%\free2api-keepalive.xml"
set "REGISTER=%PKG_DIR%\register-keepalive-task.ps1"

echo [info] mode       = %MODE%
echo [info] deploy dir = %DEPLOY_DIR%
echo [info] launcher   = %LAUNCHER%

if /i "%MODE%"=="minimal" goto :enable_only

if not exist "%DEPLOY_DIR%\scripts" mkdir "%DEPLOY_DIR%\scripts"

if not exist "%LAUNCHER%" (
  if exist "%PKG_DIR%\keepalive.vbs" (
    copy /y "%PKG_DIR%\keepalive.vbs" "%LAUNCHER%" >nul
    echo [ok]   copied keepalive.vbs into the deploy dir
  ) else (
    echo [error] keepalive.vbs not found next to this installer.
    exit /b 3
  )
) else (
  if /i not "%LAUNCHER%"=="%PKG_DIR%\keepalive.vbs" (
    copy /y "%PKG_DIR%\keepalive.vbs" "%LAUNCHER%" >nul
    echo [ok]   refreshed keepalive.vbs in the deploy dir
  )
)

if not exist "%XMLTEMPLATE%" (
  echo [error] free2api-keepalive.xml not found in "%PKG_DIR%".
  exit /b 4
)
if not exist "%REGISTER%" (
  echo [error] register-keepalive-task.ps1 not found in "%PKG_DIR%".
  exit /b 5
)

powershell -NoProfile -ExecutionPolicy Bypass -File "%REGISTER%" -TaskName "%TASK_NAME%" -Template "%XMLTEMPLATE%" -Launcher "%LAUNCHER%" -WorkDir "%DEPLOY_DIR%"
if errorlevel 1 exit /b 7
goto :done

:enable_only
powershell -NoProfile -ExecutionPolicy Bypass -Command "try { Enable-ScheduledTask -TaskName '%TASK_NAME%' -ErrorAction Stop | Out-Null; Write-Host '[ok]   enabled task %TASK_NAME%' } catch { Write-Host '[warn] could not enable %TASK_NAME%; run elevated.' }"

:done

if /i "%MODE%"=="full" (
  where python >nul 2>&1
  if errorlevel 1 (
    echo [warn] python not found; skipping pre-commit guard install.
  ) else (
    python "%PKG_DIR%\install_hooks.py"
    if errorlevel 1 echo [warn] hook install reported an error; the task itself is fine.
  )
)

echo.
echo [done] watchdog installed. Verify:
echo          schtasks /query /tn "%TASK_NAME%" /v /fo LIST
echo        Pause without touching the task:
echo          type nul ^> "%DEPLOY_DIR%\data\keepalive.pause"
echo        Resume:
echo          del "%DEPLOY_DIR%\data\keepalive.pause"
echo        Disable / remove:
echo          schtasks /change /tn "%TASK_NAME%" /disable
echo          schtasks /delete /tn "%TASK_NAME%" /f

endlocal
exit /b 0
