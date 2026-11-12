[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('amd64')][string]$Arch,
    [Parameter(Mandatory)][ValidatePattern('^\d+\.\d+\.\d+$')][string]$Version,
    [Parameter(Mandatory)][string]$LarkPackage,
    [Parameter(Mandatory)][string]$LarkNativeArchive,
    [Parameter(Mandatory)][string]$NodeArchive,
    [string]$OutputDir = '',
    [string]$GitRuntime = '',
    [ValidateSet('feishu','full')][string]$Edition = 'feishu',
    [switch]$Unsigned,
    [switch]$CliOnly,
    [switch]$Desktop
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$here = $PSScriptRoot
$repo = (Resolve-Path (Join-Path $here '../..')).Path
function Invoke-Native([string]$Command, [string[]]$Arguments) {
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Native command failed ($LASTEXITCODE): $Command" }
}
foreach ($tool in @('python', 'go', 'dotnet', 'wix', 'ISCC')) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "missing tool: $tool" }
}
$dotnetVersion = (& dotnet --version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $dotnetVersion -notmatch '^10\.\d+\.\d+$') { throw 'Stable .NET 10 SDK is required to build native login' }
$mode = @()
if ($Unsigned) { $mode += '--unsigned' }
if ($CliOnly) { $mode += '--cli-only' }
if ($Desktop) { $mode += '--desktop' }
Invoke-Native python (@("$here/package.py", 'preflight', '--platform', 'windows', '--arch', $Arch) + $mode)
$wixVersion = (& wix --version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $wixVersion -notmatch '^4\.') { throw 'WiX Toolset 4 is required' }
if (-not $CliOnly) {
    $driver = Get-ItemProperty 'HKLM:\SOFTWARE\WOW6432Node\WinFsp' -ErrorAction SilentlyContinue
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
if ($Edition -eq 'feishu') { $name += '-feishu' }
if ($Desktop) { $name += '-desktop' }
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
    if ($Edition -eq 'feishu') { $tags = (@($tags,'drivebridge_feishu') | Where-Object { $_ }) -join ',' }
    Push-Location $repo
    try { Invoke-Native go @('build', '-trimpath', '-tags', $tags, '-ldflags', '-s -w', '-o', "$work/drivebridge.exe", '.') }
    finally { Pop-Location }
    $sessionOutput = Join-Path $work 'session'
    Invoke-Native dotnet @('publish', "$repo/native/session-host/windows/DriveBridge.SessionHost.csproj",
        '-c', 'Release', '-r', 'win-x64', '--self-contained', 'true', '-o', $sessionOutput,
        '-p:EnableWindowsTargeting=true', "-p:Version=$Version",
        "-p:PathMap=$repo=/_/DriveBridge", '-p:DebugType=None', '-p:DebugSymbols=false')
    $sessionFiles = @(Get-ChildItem -LiteralPath $sessionOutput -Recurse -File)
    if ($sessionFiles.Count -ne 1 -or $sessionFiles[0].Name -ne 'drivebridge-session-host.exe') {
        throw 'Native login must publish exactly one self-contained executable; unexpected dependencies cannot be omitted'
    }
    Invoke-Native python @("$here/session-licenses.py", '--assets', "$repo/native/session-host/windows/obj/project.assets.json", '--output', "$work/native-login.LICENSES")
    $sessionStage = @('--session-host-binary', "$sessionOutput/drivebridge-session-host.exe", '--session-host-licenses', "$work/native-login.LICENSES")
    $desktopStage = @()
    if ($Desktop) {
        Push-Location $repo
        try { Invoke-Native go @('build', '-trimpath', '-tags', "$tags,desktop", '-ldflags', '-s -w -H=windowsgui', '-o', "$work/drivebridge-desktop.exe", '.') }
        finally { Pop-Location }
        $desktopStage = @('--desktop-binary', "$work/drivebridge-desktop.exe", '--version', $Version)
    }
    $gitStage = @()
    if ($GitRuntime) { $gitStage = @('--git-runtime', $GitRuntime) }
    Invoke-Native python (@("$here/package.py", 'stage', '--platform', 'windows', '--arch', $Arch,
        '--root', $payload, '--repo', $repo, '--binary', "$work/drivebridge.exe", '--consume-binary', '--edition', $Edition, '--lark-package', $LarkPackage,
        '--lark-native-archive', $LarkNativeArchive, '--node-archive', $NodeArchive) + $mode + $desktopStage + $sessionStage + $gitStage)
    $innoVersion = (& ISCC '/?' 2>&1 | Out-String).Trim().Split("`n")[0]
    if ($LASTEXITCODE -ne 0) { throw 'Cannot determine Inno Setup version' }
    $versions = @{ wix = $wixVersion; inno = $innoVersion; powershell = $PSVersionTable.PSVersion.ToString(); dotnet = $dotnetVersion }
    $versions | ConvertTo-Json | Set-Content "$work/tools.json" -Encoding utf8NoBOM
    $signature = 'unsigned_test'
    if (-not $Unsigned) {
        $signature = 'signed'
        # Keep official CLI/Node signatures as supplied; only sign our executables.
        Invoke-Native signtool @('sign', '/sha1', $env:DRIVEBRIDGE_SIGN_CERT_SHA1, '/fd', 'SHA256', '/tr', $env:DRIVEBRIDGE_TIMESTAMP_URL, '/td', 'SHA256', "$payload/bin/drivebridge.exe")
        Invoke-Native signtool @('verify', '/pa', "$payload/bin/drivebridge.exe")
        Invoke-Native signtool @('sign', '/sha1', $env:DRIVEBRIDGE_SIGN_CERT_SHA1, '/fd', 'SHA256', '/tr', $env:DRIVEBRIDGE_TIMESTAMP_URL, '/td', 'SHA256', "$payload/libexec/drivebridge-session-host.exe")
        Invoke-Native signtool @('verify', '/pa', "$payload/libexec/drivebridge-session-host.exe")
        if ($Desktop) {
            Invoke-Native signtool @('sign', '/sha1', $env:DRIVEBRIDGE_SIGN_CERT_SHA1, '/fd', 'SHA256', '/tr', $env:DRIVEBRIDGE_TIMESTAMP_URL, '/td', 'SHA256', "$payload/bin/drivebridge-desktop.exe")
            Invoke-Native signtool @('verify', '/pa', "$payload/bin/drivebridge-desktop.exe")
        }
    }
    Invoke-Native python (@("$here/package.py", 'manifest', '--platform', 'windows', '--arch', $Arch,
	    '--edition', $Edition,
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
