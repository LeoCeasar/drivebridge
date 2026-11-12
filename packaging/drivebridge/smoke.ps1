[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Payload,
    [Parameter(Mandatory)][string]$MountPoint,
    [Parameter(Mandatory)][string]$Evidence,
    [Parameter(Mandatory)][string]$Artifacts,
    [switch]$Release
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
function Run([string]$Command, [string[]]$Arguments) {
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Native gate failed ($LASTEXITCODE): $Command" }
}
Run python @("$PSScriptRoot/package.py", 'verify', '--root', $Payload)
$m = Get-Content (Join-Path $Payload 'manifest.json') -Raw | ConvertFrom-Json
Run python @("$PSScriptRoot/package.py", 'preflight', '--platform', 'windows', '--arch', $m.architecture)
if (-not $m.mount) { throw 'NOT VERIFIED: CLI-only package cannot pass mount gate' }
$driver = Get-ItemProperty 'HKLM:\SOFTWARE\WOW6432Node\WinFsp' -ErrorAction SilentlyContinue
if (-not $driver -or -not (Test-Path (Join-Path $driver.InstallDir 'bin/winfsp-x64.dll'))) { throw 'NOT VERIFIED: WinFsp absent' }
# Native runner supplies a disposable WinFsp drive letter. A normal NTFS directory cannot pass.
if ($MountPoint -notmatch '^[A-Za-z]:\\?$') { throw 'Provide disposable WinFsp drive root' }
$volume = Get-Volume -DriveLetter $MountPoint.Substring(0,1) -ErrorAction Stop
if ($volume.FileSystem -notmatch '^(rclone|WinFsp|FUSE)$') { throw 'NOT VERIFIED: drive is not the expected mounted filesystem' }
$index = Get-Content (Join-Path $Artifacts 'artifacts.json') -Raw | ConvertFrom-Json
$hash = (Get-FileHash (Join-Path $Payload 'manifest.json') -Algorithm SHA256).Hash.ToLowerInvariant()
if ($hash -ne $index.payload_manifest_sha256) { throw 'Artifact/payload manifest mismatch' }
foreach ($item in $index.artifacts) {
    if ([IO.Path]::GetFileName($item.path) -ne $item.path) { throw 'Invalid artifact path' }
    $hash = (Get-FileHash (Join-Path $Artifacts $item.path) -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($hash -ne $item.sha256) { throw 'Artifact checksum mismatch' }
}
Run (Join-Path $Payload 'bin/drivebridge.exe') @('feishu', '--help')
Run (Join-Path $Payload 'bin/drivebridge.exe') @('mount', '--help')
Run (Join-Path $Payload 'libexec/node/node.exe') @('--version')
$work = Join-Path $MountPoint ('.drivebridge-smoke-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory $work | Out-Null
try {
    $file = Join-Path $work 'save.tmp'
    $bytes = [Text.Encoding]::UTF8.GetBytes("DriveBridge native write / 原子保存`n")
    $stream = [IO.File]::Open($file, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    try { $stream.Write($bytes, 0, $bytes.Length); $stream.Flush($true) } finally { $stream.Dispose() }
    Move-Item -LiteralPath $file -Destination (Join-Path $work 'saved.txt')
    Rename-Item -LiteralPath (Join-Path $work 'saved.txt') -NewName 'renamed.txt'
    $actual = [IO.File]::ReadAllBytes((Join-Path $work 'renamed.txt'))
    if ([Convert]::ToBase64String($actual) -ne [Convert]::ToBase64String($bytes)) { throw 'Native readback mismatch' }
} finally { Remove-Item -LiteralPath $work -Recurse -Force }
$gate = '--native'
if ($Release) {
    $gate = '--release'
    $executables = @((Join-Path $Payload 'bin/drivebridge.exe'), (Join-Path $Payload 'libexec/drivebridge-session-host.exe'))
    if (Test-Path (Join-Path $Payload 'bin/drivebridge-desktop.exe')) { $executables += (Join-Path $Payload 'bin/drivebridge-desktop.exe') }
    foreach ($file in $executables + @(Get-ChildItem $Artifacts -File | Where-Object Extension -In '.msi','.exe' | Select-Object -ExpandProperty FullName)) {
        if ((Get-AuthenticodeSignature -LiteralPath $file).Status -ne 'Valid') { throw "Invalid signature: $file" }
    }
}
Run python @("$PSScriptRoot/package.py", 'verify', '--root', $Payload, $gate, '--evidence', $Evidence)
Write-Host 'Native smoke and supplied install/upgrade/stop evidence passed for this exact payload.'
