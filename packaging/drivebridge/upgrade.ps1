# Run as the desktop user. InstallerArguments are exact argv, not PowerShell code.
param([Parameter(Mandatory)][string]$Candidate, [Parameter(Mandatory)][string]$Installer,
      [string[]]$InstallerArguments = @())
$ErrorActionPreference = 'Stop'
if ($PSVersionTable.PSVersion.Major -lt 7) { throw 'PowerShell 7+ is required for lossless native argv; legacy shell quoting is unsupported' }
if (-not [IO.Path]::IsPathRooted($Candidate) -or -not [IO.Path]::IsPathRooted($Installer)) { throw 'Absolute candidate and synchronous installer paths required' }
if ((Get-Item -LiteralPath $Candidate).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Candidate must be a regular executable' }
$work = Join-Path ([IO.Path]::GetTempPath()) ('drivebridge-install-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
$helper = Join-Path $work 'drivebridge-install-helper.exe'
try {
    Copy-Item -LiteralPath $Candidate -Destination $helper
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = $helper
    $start.UseShellExecute = $false
    foreach ($argument in (@('feishu', 'maintenance', 'exec', '--', $Installer) + $InstallerArguments)) {
        $start.ArgumentList.Add($argument)
    }
    $child = [Diagnostics.Process]::Start($start)
    # Wait even if the invoking pipeline is interrupted; do not delete the helper
    # or abandon ownership merely because the wrapper receives cancellation.
    try { $child.WaitForExit() } finally { if (-not $child.HasExited) { $child.WaitForExit() } }
    if ($child.ExitCode -ne 0) { throw "Installation lease/child failed: $($child.ExitCode)" }
} finally {
    Remove-Item -LiteralPath $helper -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $work -ErrorAction SilentlyContinue
}
