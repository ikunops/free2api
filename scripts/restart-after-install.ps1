param(
    [Parameter(Mandatory = $true)][string]$StagedDesktop,
    [Parameter(Mandatory = $true)][string]$StagedServer,
    [string]$DeployDir = 'C:\Users\30849\Documents\Codex\2026-09-26\zhe\work\demo',
    [int]$Port = 7864,
    [int]$HealthTimeoutSec = 90,
    [int]$ModelsTimeoutSec = 30,
    [switch]$Detach,
    [switch]$ValidateOnly
)

$ErrorActionPreference = 'Stop'
$OutputEncoding = [Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$DeployDir = [IO.Path]::GetFullPath($DeployDir).TrimEnd('\')
$logDir = Join-Path $DeployDir 'data'
$logPath = Join-Path $logDir 'restart-after-install.log'
$pausePath = Join-Path $logDir 'keepalive.pause'
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$finalDesktop = Join-Path $DeployDir 'free2api.exe'
$finalServer = Join-Path $DeployDir 'free2api-server.exe'
$backupDesktop = Join-Path $DeployDir ('free2api.exe.rollback-' + $stamp)
$backupServer = Join-Path $DeployDir ('free2api-server.exe.rollback-' + $stamp)

function Log([string]$message) {
    $line = '[' + (Get-Date -Format 'yyyy-MM-dd HH:mm:ss') + '] ' + $message
    Write-Output $line
    try {
        [IO.Directory]::CreateDirectory($logDir) | Out-Null
        [IO.File]::AppendAllText($logPath, $line + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
    } catch {}
}

function Assert-UnderDeploy([string]$path, [string]$label) {
    $full = [IO.Path]::GetFullPath($path)
    if (-not $full.StartsWith($DeployDir + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw "$label is outside deploy dir: $full"
    }
    return $full
}

function Same-Path([string]$a, [string]$b) {
    if ([string]::IsNullOrWhiteSpace($a) -or [string]::IsNullOrWhiteSpace($b)) { return $false }
    return [IO.Path]::GetFullPath($a).Equals([IO.Path]::GetFullPath($b), [StringComparison]::OrdinalIgnoreCase)
}

function Set-KeepalivePaused([bool]$paused) {
    if ($paused) {
        [IO.Directory]::CreateDirectory($logDir) | Out-Null
        [IO.File]::WriteAllText($pausePath, ('restart-after-install paused at ' + (Get-Date -Format o) + [Environment]::NewLine), [Text.UTF8Encoding]::new($false))
        Log 'watchdog paused'
        return
    }
    if (Test-Path -LiteralPath $pausePath) {
        Remove-Item -LiteralPath $pausePath -Force
        Log 'watchdog resumed'
    }
}

function Get-Listener() {
    Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
}

function Wait-PortFree([int]$timeoutSec) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    do {
        $conn = Get-Listener
        if (-not $conn) { return }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)
    throw "port $Port is still occupied by pid=$((Get-Listener).OwningProcess)"
}

function Test-HttpResponds([string]$uri, [int]$timeoutSec) {
    try {
        $resp = Invoke-WebRequest -UseBasicParsing -Uri $uri -TimeoutSec $timeoutSec
        return [bool]$resp
    } catch {
        # /healthz deliberately returns 503 when the pool has no servable
        # account. That still proves the gateway process owns the port and
        # answered HTTP, so startup must not be treated as a bind failure.
        return [bool]$_.Exception.Response
    }
}

function Wait-GatewayReady([int]$timeoutSec) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    do {
        if (Test-HttpResponds "http://127.0.0.1:$Port/status" 3) { return }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)
    throw "gateway /status did not answer within $timeoutSec seconds"
}

function Wait-ModelsReady([int]$timeoutSec) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    do {
        try {
            $resp = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port/v1/models" -TimeoutSec 5
            if ($resp.StatusCode -ge 200 -and $resp.StatusCode -lt 300) { return $resp.Content }
        } catch {}
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)
    throw "gateway /v1/models was not ready within $timeoutSec seconds"
}

function Stop-OwnedProcesses() {
    $targets = @($finalDesktop, $finalServer)
    $owned = @()
    foreach ($proc in @(Get-Process -Name free2api,free2api-server -ErrorAction SilentlyContinue)) {
        $path = $null
        try { $path = $proc.Path } catch {}
        if ($path -and ($targets | Where-Object { Same-Path $_ $path })) {
            $owned += $proc
        }
    }
    foreach ($proc in $owned) {
        Log ("stopping pid=" + $proc.Id + " path=" + $proc.Path)
        Stop-Process -Id $proc.Id -Force
    }
    $deadline = (Get-Date).AddSeconds(20)
    do {
        $left = @($owned | Where-Object { Get-Process -Id $_.Id -ErrorAction SilentlyContinue })
        if ($left.Count -eq 0) { break }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)
    if ($left.Count -gt 0) {
        throw ("processes did not stop: " + (($left | ForEach-Object { $_.Id }) -join ','))
    }
}

function Start-GatewayProcess([string]$exePath, [string]$label) {
    $cfgPath = Join-Path $DeployDir 'config.json'
    if (-not (Test-Path -LiteralPath $cfgPath -PathType Leaf)) { throw "config.json missing: $cfgPath" }
    if (-not (Test-Path -LiteralPath $exePath -PathType Leaf)) { throw "$label missing: $exePath" }
    $quotedCfg = '"' + $cfgPath + '"'
    $proc = Start-Process -FilePath $exePath -ArgumentList @('-config', $quotedCfg) -WorkingDirectory $DeployDir -WindowStyle Hidden -PassThru
    Log ("started " + $label + " pid=" + $proc.Id)
    Wait-GatewayReady $HealthTimeoutSec
    $models = Wait-ModelsReady $ModelsTimeoutSec
    $count = 0
    try { $count = @(($models | ConvertFrom-Json).data).Count } catch {}
    $conn = Get-Listener
    if (-not $conn) { throw "gateway reported ready but port $Port has no listener" }
    $listenerPath = ''
    try { $listenerPath = (Get-Process -Id $conn.OwningProcess -ErrorAction Stop).Path } catch {}
    if (-not (Same-Path $listenerPath $exePath)) {
        throw "unexpected listener pid=$($conn.OwningProcess) path=$listenerPath (wanted $exePath)"
    }
    Log ("gateway ready via " + $label + " pid=" + $conn.OwningProcess + " models=" + $count)
}

function Start-Gateway() {
    $lastError = $null
    foreach ($candidate in @(
        @{ Path = $finalDesktop; Label = 'desktop' },
        @{ Path = $finalServer; Label = 'server' }
    )) {
        try {
            Start-GatewayProcess $candidate.Path $candidate.Label
            return
        } catch {
            $lastError = $_.Exception.Message
            Log ("start failed via " + $candidate.Label + ": " + $lastError)
            try { Stop-OwnedProcesses } catch {}
            try { Wait-PortFree 10 } catch {}
        }
    }
    throw ("gateway failed to start with desktop or server; last error: " + $lastError)
}

$stagedDesktopFull = [IO.Path]::GetFullPath($StagedDesktop)
$stagedServerFull = [IO.Path]::GetFullPath($StagedServer)
if ($Detach) {
    if (-not (Test-Path -LiteralPath $stagedDesktopFull -PathType Leaf)) { throw "staged desktop missing: $stagedDesktopFull" }
    if (-not (Test-Path -LiteralPath $stagedServerFull -PathType Leaf)) { throw "staged server missing: $stagedServerFull" }
    $hostExe = (Get-Process -Id $PID).Path
    $childArgs = @(
        '-NoProfile',
        '-ExecutionPolicy', 'Bypass',
        '-File', ('"' + $PSCommandPath + '"'),
        '-StagedDesktop', ('"' + $stagedDesktopFull + '"'),
        '-StagedServer', ('"' + $stagedServerFull + '"'),
        '-DeployDir', ('"' + $DeployDir + '"'),
        '-Port', [string]$Port,
        '-HealthTimeoutSec', [string]$HealthTimeoutSec,
        '-ModelsTimeoutSec', [string]$ModelsTimeoutSec
    )
    if ($ValidateOnly) { $childArgs += '-ValidateOnly' }
    $child = Start-Process -FilePath $hostExe -ArgumentList $childArgs -WorkingDirectory $DeployDir -WindowStyle Hidden -PassThru
    Log ("detached restart worker pid=" + $child.Id)
    exit 0
}
Log "restart requested"
Log ("deploy=" + $DeployDir)
Log ("staged desktop=" + $stagedDesktopFull)
Log ("staged server=" + $stagedServerFull)

if (-not (Test-Path -LiteralPath $DeployDir -PathType Container)) { throw "deploy dir missing: $DeployDir" }
if (-not (Test-Path -LiteralPath $stagedDesktopFull -PathType Leaf)) { throw "staged desktop missing: $stagedDesktopFull" }
if (-not (Test-Path -LiteralPath $stagedServerFull -PathType Leaf)) { throw "staged server missing: $stagedServerFull" }
if (-not (Test-Path -LiteralPath $finalDesktop -PathType Leaf)) { throw "current desktop missing: $finalDesktop" }
if (-not (Test-Path -LiteralPath $finalServer -PathType Leaf)) { throw "current server missing: $finalServer" }
$null = Assert-UnderDeploy $finalDesktop 'final desktop'
$null = Assert-UnderDeploy $finalServer 'final server'
$null = Assert-UnderDeploy $backupDesktop 'backup desktop'
$null = Assert-UnderDeploy $backupServer 'backup server'

$conn = Get-Listener
if ($conn) {
    $listenerPath = ''
    try { $listenerPath = (Get-Process -Id $conn.OwningProcess -ErrorAction Stop).Path } catch {}
    if (-not (Same-Path $listenerPath $finalDesktop)) {
        throw "refusing to stop unexpected listener pid=$($conn.OwningProcess) path=$listenerPath"
    }
    Log ("current listener pid=" + $conn.OwningProcess)
}

$newDesktopHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $stagedDesktopFull).Hash
$newServerHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $stagedServerFull).Hash
Log ("staged desktop sha256=" + $newDesktopHash)
Log ("staged server sha256=" + $newServerHash)

if ($ValidateOnly) {
    Log "validation completed; no process stopped and no file moved"
    exit 0
}

$swapped = $false
$backupDone = $false
try {
    Copy-Item -LiteralPath $finalDesktop -Destination $backupDesktop -Force
    Copy-Item -LiteralPath $finalServer -Destination $backupServer -Force
    $backupDone = $true
    Log ("backups created desktop=" + $backupDesktop + " server=" + $backupServer)

    Set-KeepalivePaused $true
    Stop-OwnedProcesses
    Wait-PortFree 20

    Move-Item -LiteralPath $stagedDesktopFull -Destination $finalDesktop -Force
    Move-Item -LiteralPath $stagedServerFull -Destination $finalServer -Force
    $swapped = $true
    Log "new binaries installed"

    Start-Gateway
    Set-KeepalivePaused $false
    Log "restart completed"
} catch {
    $failure = $_.Exception.Message
    Log ("ERROR " + $failure)
    try {
        if ($swapped) {
            Stop-OwnedProcesses
            Wait-PortFree 20
            Move-Item -LiteralPath $backupDesktop -Destination $finalDesktop -Force
            Move-Item -LiteralPath $backupServer -Destination $finalServer -Force
            Log "rolled back previous binaries"
            Start-Gateway
            Set-KeepalivePaused $false
            Log "rollback gateway is running"
        } elseif ($backupDone) {
            Move-Item -LiteralPath $backupDesktop -Destination $finalDesktop -Force
            Move-Item -LiteralPath $backupServer -Destination $finalServer -Force
            Log "original binaries restored"
        }
    } catch {
        Log ("ROLLBACK ERROR " + $_.Exception.Message)
    }
    try { Set-KeepalivePaused $false } catch {}
    exit 1
}
