<#
.SYNOPSIS
    Render and register the windowless free2api watchdog task.

.DESCRIPTION
    Split out of install-keepalive.cmd on purpose.

    schtasks.exe cannot even read this task on some hosts: it answers
    "The system cannot find the path specified" (ERROR_PATH_NOT_FOUND) for a
    task it should be able to see, which makes failures unreadable.
    Register-ScheduledTask reports the real condition (access denied) so the
    operator knows to elevate.

    The task action is wscript.exe (GUI subsystem, never allocates a console),
    which is what keeps Windows 11 from flashing a terminal every minute.

.PARAMETER TaskName
    Task name, registered in the root folder.

.PARAMETER Template
    Task XML template with __LAUNCHER__ / __WORKDIR__ / __TASKNAME__ slots.

.PARAMETER Launcher
    Absolute path to keepalive.vbs.

.PARAMETER WorkDir
    Gateway deploy directory (holds free2api.exe and config.json).
#>
[CmdletBinding()]
param(
    [string]$TaskName = 'free2api-keepalive',
    [Parameter(Mandatory = $true)][string]$Template,
    [Parameter(Mandatory = $true)][string]$Launcher,
    [Parameter(Mandatory = $true)][string]$WorkDir
)

$ErrorActionPreference = 'Stop'

function Esc([string]$value) { [System.Security.SecurityElement]::Escape($value) }

if (-not (Test-Path -LiteralPath $Template)) { throw "template not found: $Template" }
if (-not (Test-Path -LiteralPath $Launcher)) { throw "launcher not found: $Launcher" }

$text = [IO.File]::ReadAllText($Template)
$text = $text -replace '__LAUNCHER__', (Esc $Launcher)
$text = $text -replace '__WORKDIR__', (Esc $WorkDir)
$text = $text -replace '__TASKNAME__', (Esc $TaskName)

$rendered = Join-Path $env:TEMP (("free2api-keepalive.{0}.xml") -f (Get-Random))
[IO.File]::WriteAllText($rendered, $text, (New-Object Text.UTF8Encoding($false)))
Write-Host "[ok]   rendered task XML -> $rendered"

try {
    Register-ScheduledTask -TaskName $TaskName -Xml $text -Force -ErrorAction Stop | Out-Null
}
catch {
    Write-Host ("[error] Register-ScheduledTask failed: " + $_.Exception.Message)
    Write-Host "[hint]  access denied means you must run this from an ELEVATED prompt."
    exit 7
}

try {
    Enable-ScheduledTask -TaskName $TaskName -ErrorAction Stop | Out-Null
    Write-Host "[ok]   registered and enabled task '$TaskName' (windowless action)"
}
catch {
    Write-Host ("[warn] registered, but Enable-ScheduledTask failed: " + $_.Exception.Message)
}

exit 0
