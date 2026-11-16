param([switch]$NoBrowser, [switch]$Build, [switch]$Update, [int]$Port = 18787, [int]$ProxyPort = 18788)
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot)
$source = Test-Path -LiteralPath (Join-Path $root 'go.mod')
$binary = Join-Path $root 'prtseek.exe'
if ($source) { $binary = Join-Path $root '.dev/prtseek.exe' }
$candidate = Join-Path $root '.dev/prtseek-next.exe'
$stateTool = Join-Path $root 'scripts/build-state.mjs'
$url = "http://127.0.0.1:$Port"
$runDir = Join-Path $root '.prtseek-run'
$hash = [Security.Cryptography.SHA256]::Create()
$key = [BitConverter]::ToString($hash.ComputeHash([Text.Encoding]::UTF8.GetBytes($root.ToLowerInvariant()))).Replace('-', '')
$hash.Dispose()
$mutex = New-Object Threading.Mutex($false, "Local\prtseek-$key")
$locked = $false

function Get-Instances {
    # Include the legacy root executable only in this exact installation.
    @(Get-CimInstance Win32_Process -Filter "Name='prtseek.exe'" | Where-Object {
        $_.ExecutablePath -and ($_.ExecutablePath -eq $binary -or $_.ExecutablePath -eq (Join-Path $root 'prtseek.exe'))
    })
}
function Get-Health {
    try { Invoke-RestMethod -Uri "$url/api/health" -TimeoutSec 2 -Headers @{'Cache-Control'='no-cache'} } catch { $null }
}
function Get-BinaryHash([string]$path) {
    # Use the .NET API: PS7 can pass a module path to Windows PowerShell 5.1
    # that prevents the Get-FileHash module from loading.
    $algorithm = [Security.Cryptography.SHA256]::Create()
    $stream = [IO.File]::OpenRead($path)
    try { [BitConverter]::ToString($algorithm.ComputeHash($stream)).Replace('-', '').ToLowerInvariant() }
    finally { $stream.Dispose(); $algorithm.Dispose() }
}
function Check-Ports($instances) {
    $owners = @($instances | ForEach-Object { [int]$_.ProcessId })
    $listeners = @(Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue | Where-Object { $_.LocalPort -eq $Port -or ($ProxyPort -ne 0 -and $_.LocalPort -eq $ProxyPort) })
    foreach ($listener in $listeners) {
        if ($owners -notcontains [int]$listener.OwningProcess) { throw "Port $($listener.LocalPort) belongs to another process. No service was stopped." }
    }
}
Push-Location -LiteralPath $root
try {
    try { $locked = $mutex.WaitOne(0) } catch [Threading.AbandonedMutexException] { $locked = $true }
    if (!$locked) { throw 'Another launcher/update is in progress.' }
    $instances = @(Get-Instances)
    Check-Ports $instances
    $needsBuild = $Build.IsPresent
    if ($source) {
        if (!(Get-Command node -ErrorAction SilentlyContinue)) { throw 'Source checkout requires Node.js and Go. Install them or use a release package.' }
        & node $stateTool check $binary
        $needsBuild = $needsBuild -or $LASTEXITCODE -ne 0
    } elseif (!(Test-Path -LiteralPath $binary)) { throw 'Release executable is missing.' }
    if ($needsBuild -and $instances.Count -gt 0 -and !($Update -or $Build)) {
        throw 'Source changed while an older service is running. Run update-prtseek.cmd to build and restart this installation.'
    }
    if ($needsBuild) {
        & (Join-Path $root 'build-prtseek.ps1')
        if (!$?) { throw 'Build failed. Existing executable and running service were preserved.' }
        & node $stateTool check $candidate
        if ($LASTEXITCODE -ne 0) { throw 'Candidate does not match current source. Existing service was preserved.' }
    }
    $desired = $binary
    if ($needsBuild) { $desired = $candidate }
    $manifest = $null
    if (Test-Path -LiteralPath "$desired.build.json") {
        $manifest = Get-Content -LiteralPath "$desired.build.json" -Raw | ConvertFrom-Json
        if ((Get-BinaryHash $desired) -ne $manifest.binary_sha256) { throw 'Executable checksum does not match its build manifest.' }
    }
    if ($instances.Count -gt 0 -and !$needsBuild -and !$Update -and !$Build) {
        $health = Get-Health
        if ($instances.Count -eq 1 -and $instances[0].ExecutablePath -eq $binary -and $manifest -and $health.service -eq 'prtseek' -and $health.build_id -eq $manifest.source_id) {
            Write-Host "Current build already running: $url/chat/"
            if (!$NoBrowser) { Start-Process "$url/chat/" }
            exit 0
        }
        throw 'Running service cannot be verified as this build. Run update-prtseek.cmd to restart this installation.'
    }
    # Build first. Recheck identities immediately before stopping only our instances.
    $instances = @(Get-Instances)
    Check-Ports $instances
    foreach ($instance in $instances) {
        $current = Get-CimInstance Win32_Process -Filter "ProcessId=$($instance.ProcessId)"
        if ($current -and $current.ExecutablePath -eq $instance.ExecutablePath -and $current.CreationDate -eq $instance.CreationDate) {
            Stop-Process -Id $instance.ProcessId -ErrorAction Stop
            Wait-Process -Id $instance.ProcessId -Timeout 15 -ErrorAction SilentlyContinue
        } else { throw 'Process identity changed during update; retry the launcher.' }
    }
    if ($needsBuild) {
        if (Test-Path -LiteralPath $binary) { Copy-Item -LiteralPath $binary -Destination "$binary.previous" -Force }
        Copy-Item -LiteralPath $candidate -Destination $binary -Force
        Copy-Item -LiteralPath "$candidate.build.json" -Destination "$binary.build.json" -Force
    }
    New-Item -ItemType Directory -Force -Path $runDir | Out-Null
    $arguments = @('-addr', "127.0.0.1:$Port", '-proxy', "127.0.0.1:$ProxyPort", '-data', ('"' + (Join-Path $root 'data') + '"'))
    $process = Start-Process -FilePath $binary -ArgumentList $arguments -WorkingDirectory $root -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $runDir 'stdout.log') -RedirectStandardError (Join-Path $runDir 'stderr.log')
    $ready = $false
    for ($attempt = 0; $attempt -lt 40; $attempt++) {
        $process.Refresh()
        if ($process.HasExited) { throw "Service exited. See $runDir/stderr.log" }
        $health = Get-Health
        if ($health.service -eq 'prtseek' -and (!$manifest -or $health.build_id -eq $manifest.source_id)) { $ready = $true; break }
        Start-Sleep -Milliseconds 250
    }
    if (!$ready) { throw "Service did not report the expected build. See $runDir/stderr.log" }
    Write-Host "prtseek $($health.version) started: $url/chat/ (PID $($process.Id))"
    if (!$NoBrowser) { Start-Process "$url/chat/" }
} finally {
    Pop-Location
    if ($locked) { $mutex.ReleaseMutex() }
    $mutex.Dispose()
}
