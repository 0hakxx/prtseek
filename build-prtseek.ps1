$ErrorActionPreference = 'Stop'
$projectRoot = [IO.Path]::GetFullPath($PSScriptRoot)
$workspacePrefix = $projectRoot.TrimEnd('\') + [IO.Path]::DirectorySeparatorChar
$goPath = Join-Path $projectRoot '.dev/runtime/go/bin/go.exe'
$outputPath = Join-Path $projectRoot '.dev/prtseek-next.exe'
$buildVersion = (Get-Content -LiteralPath (Join-Path $projectRoot 'VERSION') -Raw).Trim()

if (!(Test-Path -LiteralPath $goPath -PathType Leaf)) {
    $goCommand = Get-Command go.exe -ErrorAction SilentlyContinue
    if (!$goCommand) { throw 'Go 를 설치하고 PATH 에 추가하거나 .dev/runtime/go/bin 에 넣으세요.' }
    $goPath = $goCommand.Source
}
if (!(Get-Command npm.cmd -ErrorAction SilentlyContinue)) { throw 'npm.cmd 를 찾을 수 없습니다. 먼저 Node.js 빌드 환경을 준비하세요.' }

$oldExport = [Environment]::GetEnvironmentVariable('NEXT_EXPORT', 'Process')
$oldGoCache = [Environment]::GetEnvironmentVariable('GOCACHE', 'Process')
$oldGoModCache = [Environment]::GetEnvironmentVariable('GOMODCACHE', 'Process')
$oldGoPath = [Environment]::GetEnvironmentVariable('GOPATH', 'Process')
Push-Location -LiteralPath $projectRoot
try {
    Push-Location -LiteralPath (Join-Path $projectRoot 'web')
    try {
        & npm.cmd ci --no-audit --no-fund --cache (Join-Path $projectRoot '.dev/npm-cache')
        if ($LASTEXITCODE -ne 0) { throw '프론트엔드 의존성 설치에 실패했습니다.' }
        $env:NEXT_EXPORT = '1'
        & npm.cmd run build:static
        if ($LASTEXITCODE -ne 0) { throw '프론트엔드 정적 내보내기에 실패했습니다.' }
    } finally {
        [Environment]::SetEnvironmentVariable('NEXT_EXPORT', $oldExport, 'Process')
        Pop-Location
    }

    $sourcePath = [IO.Path]::GetFullPath((Join-Path $projectRoot 'web/out'))
    if (!(Test-Path -LiteralPath (Join-Path $sourcePath 'index.html') -PathType Leaf)) {
        throw '프론트엔드 정적 내보내기에 web/out/index.html 이 없어 기존 내장 디렉터리를 유지합니다.'
    }
    $destinationPath = [IO.Path]::GetFullPath((Join-Path $projectRoot 'server/webui/dist'))
    $allowedPath = [IO.Path]::GetFullPath((Join-Path $projectRoot 'server/webui')).TrimEnd('\') + [IO.Path]::DirectorySeparatorChar
    $archiveParent = [IO.Path]::GetFullPath((Join-Path $projectRoot '.dev/embed-history'))
    $archivePath = [IO.Path]::GetFullPath((Join-Path $archiveParent ([guid]::NewGuid().ToString('N'))))
    if (!$sourcePath.StartsWith($workspacePrefix, [StringComparison]::OrdinalIgnoreCase) -or
        !$destinationPath.StartsWith($allowedPath, [StringComparison]::OrdinalIgnoreCase) -or
        !$archiveParent.StartsWith($workspacePrefix, [StringComparison]::OrdinalIgnoreCase) -or
        !$archivePath.StartsWith($archiveParent + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw '빌드 입력, 내장 또는 보관 경로가 허용 범위를 벗어났습니다.'
    }
    foreach ($checkedPath in @((Join-Path $projectRoot 'web'), $sourcePath, (Join-Path $projectRoot 'server'), (Split-Path -Parent $destinationPath), $destinationPath, (Join-Path $projectRoot '.dev'), $archiveParent)) {
        if (Test-Path -LiteralPath $checkedPath) {
            $item = Get-Item -LiteralPath $checkedPath -Force
            if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw '빌드 디렉터리에 재분석 지점(reparse point)이 있어 이동이나 덮어쓰기를 거부합니다.' }
        }
    }
    if (Test-Path -LiteralPath $destinationPath) {
        New-Item -ItemType Directory -Path $archiveParent -Force | Out-Null
        Move-Item -LiteralPath $destinationPath -Destination $archivePath
        Write-Host "기존 내장 프론트엔드를 보관했습니다: $archivePath"
    }
    New-Item -ItemType Directory -Path $destinationPath -Force | Out-Null
    Get-ChildItem -LiteralPath $sourcePath -Force | Copy-Item -Destination $destinationPath -Recurse -Force
    $env:GOCACHE = Join-Path $projectRoot '.dev/gocache'
    $env:GOMODCACHE = Join-Path $projectRoot '.dev/gomodcache'
    $env:GOPATH = Join-Path $projectRoot '.dev/gopath'
    & $goPath build -buildvcs=false -tags embedui -ldflags "-s -w -X main.version=$buildVersion" -o $outputPath ./cmd/prtseek
    if ($LASTEXITCODE -ne 0) { throw 'prtseek 백엔드 빌드에 실패했습니다. 실행 중인 프로그램은 교체되지 않았습니다.' }
    & node (Join-Path $projectRoot 'scripts/build-state.mjs') write $outputPath
    if ($LASTEXITCODE -ne 0) { throw '빌드 지문 기록에 실패해 후보 프로그램을 설치할 수 없습니다.' }
    Write-Host "빌드 성공: $outputPath"
    Write-Host '이 스크립트는 빌드만 하며, 서비스를 중지하거나 .dev/prtseek.exe 또는 기존 프로그램을 교체하지 않습니다.'
} finally {
    [Environment]::SetEnvironmentVariable('NEXT_EXPORT', $oldExport, 'Process')
    [Environment]::SetEnvironmentVariable('GOCACHE', $oldGoCache, 'Process')
    [Environment]::SetEnvironmentVariable('GOMODCACHE', $oldGoModCache, 'Process')
    [Environment]::SetEnvironmentVariable('GOPATH', $oldGoPath, 'Process')
    Pop-Location
}
