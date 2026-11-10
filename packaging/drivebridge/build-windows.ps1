[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('amd64')][string]$Arch,
    [Parameter(Mandatory)][ValidatePattern('^\d+\.\d+\.\d+$')][string]$Version,
    [Parameter(Mandatory)][string]$LarkPackage,
    [Parameter(Mandatory)][string]$LarkNativeArchive,
    [Parameter(Mandatory)][string]$NodeArchive,
    [string]$OutputDir = '',
    [switch]$Unsigned,
    [switch]$CliOnly
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$here = $PSScriptRoot
$repo = (Resolve-Path (Join-Path $here '../..')).Path
function Invoke-Native([string]$Command, [string[]]$Arguments) {
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Native command failed ($LASTEXITCODE): $Command" }
}
foreach ($tool in @('python', 'go', 'wix', 'ISCC')) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "missing tool: $tool" }
}
$mode = @()
if ($Unsigned) { $mode += '--unsigned' }
if ($CliOnly) { $mode += '--cli-only' }
Invoke-Native python (@("$here/package.py", 'preflight', '--platform', 'windows', '--arch', $Arch) + $mode)
$wixVersion = (& wix --version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $wixVersion -notmatch '^4\.') { throw 'WiX Toolset 4 is required' }
if (-not $CliOnly) {
    $driver = Get-ItemProperty 'HKLM:\SOFTWARE\WinFsp' -ErrorAction SilentlyContinue
    if (-not $driver -or -not (Test-Path (Join-Path $driver.InstallDir 'bin/winfsp-x64.dll'))) {
        throw 'WinFsp driver/runtime missing; full native build blocked. Explicit -CliOnly -Unsigned is test-only.'
    }
}
if (-not $Unsigned) {
    if (-not (Get-Command signtool -ErrorAction SilentlyContinue)) { throw 'missing tool: signtool' }
    if (-not $env:DRIVEBRIDGE_SIGN_CERT_SHA1 -or -not $env:DRIVEBRIDGE_TIMESTAMP_URL) {
        throw 'Release needs DRIVEBRIDGE_SIGN_CERT_SHA1 and DRIVEBRIDGE_TIMESTAMP_URL (or explicit -Unsigned)'
    }
}
$name = "drivebridge-$Version-windows-$Arch"
if ($Unsigned) { $name += '-unsigned-test' }
if (-not $OutputDir) { $OutputDir = Join-Path $repo "dist/$name" }
if (Test-Path -LiteralPath $OutputDir) { throw 'Output exists; refusing overwrite' }
$work = Join-Path ([IO.Path]::GetTempPath()) ('drivebridge-package-' + [guid]::NewGuid().ToString('N'))
$payload = Join-Path $work 'payload'
$artifacts = Join-Path $work 'artifacts'
New-Item -ItemType Directory -Path $artifacts -Force | Out-Null
$oldGOOS = $env:GOOS; $oldGOARCH = $env:GOARCH; $oldCGO = $env:CGO_ENABLED
try {
    $env:GOOS = 'windows'; $env:GOARCH = $Arch; $env:CGO_ENABLED = '0'
    $tags = 'cmount'
    if ($CliOnly) { $tags = '' }
    Push-Location $repo
    try { Invoke-Native go @('build', '-trimpath', '-tags', $tags, '-ldflags', '-s -w', '-o', "$work/drivebridge.exe", '.') }
    finally { Pop-Location }
    Invoke-Native python (@("$here/package.py", 'stage', '--platform', 'windows', '--arch', $Arch,
        '--root', $payload, '--repo', $repo, '--binary', "$work/drivebridge.exe", '--consume-binary', '--lark-package', $LarkPackage,
        '--lark-native-archive', $LarkNativeArchive, '--node-archive', $NodeArchive) + $mode)
    $innoVersion = (& ISCC '/?' 2>&1 | Out-String).Trim().Split("`n")[0]
    if ($LASTEXITCODE -ne 0) { throw 'Cannot determine Inno Setup version' }
    $versions = @{ wix = $wixVersion; inno = $innoVersion; powershell = $PSVersionTable.PSVersion.ToString() }
    $versions | ConvertTo-Json | Set-Content "$work/tools.json" -Encoding utf8NoBOM
    $signature = 'unsigned_test'
    if (-not $Unsigned) {
        $signature = 'signed'
        # Keep official CLI/Node signatures as supplied; only sign our executable.
        Invoke-Native signtool @('sign', '/sha1', $env:DRIVEBRIDGE_SIGN_CERT_SHA1, '/fd', 'SHA256', '/tr', $env:DRIVEBRIDGE_TIMESTAMP_URL, '/td', 'SHA256', "$payload/bin/drivebridge.exe")
        Invoke-Native signtool @('verify', '/pa', "$payload/bin/drivebridge.exe")
    }
    Invoke-Native python (@("$here/package.py", 'manifest', '--platform', 'windows', '--arch', $Arch,
        '--root', $payload, '--version', $Version, '--tools', "$work/tools.json", '--signature', $signature) + $mode)
    Invoke-Native python @("$here/package.py", 'verify', '--root', $payload)
    Invoke-Native python @("$here/package.py", 'render', '--template', 'windows.wxs', '--version', $Version,
        '--arch', $Arch, '--root', $payload, '--output', "$work/windows.wxs")
    Invoke-Native wix @('build', '-arch', 'x64', '-o', "$artifacts/$name.msi", "$work/windows.wxs")
    Invoke-Native python @("$here/package.py", 'render', '--template', 'windows.iss', '--version', $Version,
        '--arch', $Arch, '--root', $payload, '--output', "$work/windows.iss")
    $innoArgs = @("/O$artifacts", "/F$name")
    if (-not $Unsigned) {
        if ($env:DRIVEBRIDGE_SIGN_CERT_SHA1 -notmatch '^[A-Fa-f0-9]{40}$' -or $env:DRIVEBRIDGE_TIMESTAMP_URL -notmatch '^https://[^\s";]+$') { throw 'Invalid signing identity/timestamp settings' }
        $innoArgs += '/DSIGNED'
        $innoArgs += ('/Sdbsign=signtool sign /sha1 ' + $env:DRIVEBRIDGE_SIGN_CERT_SHA1 + ' /fd SHA256 /tr ' + $env:DRIVEBRIDGE_TIMESTAMP_URL + ' /td SHA256 $f')
    }
    Invoke-Native ISCC ($innoArgs + @("$work/windows.iss"))
    if (-not $Unsigned) {
        foreach ($artifact in @("$artifacts/$name.msi", "$artifacts/$name.exe")) {
            Invoke-Native signtool @('sign', '/sha1', $env:DRIVEBRIDGE_SIGN_CERT_SHA1, '/fd', 'SHA256', '/tr', $env:DRIVEBRIDGE_TIMESTAMP_URL, '/td', 'SHA256', $artifact)
            Invoke-Native signtool @('verify', '/pa', $artifact)
        }
    }
    # Exclude WiX debug databases; publish exactly the promised artifacts/checksum manifests.
    Get-ChildItem -LiteralPath $artifacts -Filter '*.wixpdb' | Remove-Item
    Compress-Archive -Path "$payload/*" -DestinationPath "$artifacts/$name.zip"
    Invoke-Native python @("$here/package.py", 'artifacts', '--root', $payload, '--dir', $artifacts)
    Copy-Item "$payload/manifest.json" "$artifacts/payload-manifest.json"
    $parentDir = Split-Path -Parent ([IO.Path]::GetFullPath($OutputDir))
    New-Item -ItemType Directory -Path $parentDir -Force | Out-Null
    if (Test-Path -LiteralPath $OutputDir) { throw 'Output appeared during build' }
    Move-Item -LiteralPath $artifacts -Destination $OutputDir
    Write-Host "Artifacts: $OutputDir; native install/mount/upgrade NOT VERIFIED; release requires smoke evidence."
} finally {
    $env:GOOS = $oldGOOS; $env:GOARCH = $oldGOARCH; $env:CGO_ENABLED = $oldCGO
    Remove-Item -LiteralPath $work -Recurse -Force
}
