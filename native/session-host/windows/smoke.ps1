# Runtime-free Windows UI/pipe smoke. This does not verify Feishu login or WinFsp.
[CmdletBinding()]
param([Parameter(Mandatory=$true)][string]$Helper)
$ErrorActionPreference = 'Stop'
$Helper = (Resolve-Path -LiteralPath $Helper).Path
function Start-Helper([string]$Argument) {
    $info = [System.Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $Helper
    $info.ArgumentList.Add($Argument)
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = $info
    if (-not $process.Start()) { throw 'Native helper did not start' }
    return $process
}
function Read-Reply($Process) {
    $read = $Process.StandardOutput.ReadLineAsync()
    if (-not $read.Wait(15000)) { throw 'Native helper reply timed out' }
    if ($null -eq $read.Result) { throw 'Native helper closed its pipe unexpectedly' }
    return $read.Result | ConvertFrom-Json
}
$process = Start-Helper '--self-test'
try {
    $reply = Read-Reply $process
    if ($reply.event -ne 'smoke_complete' -or $reply.ok -ne $true) { throw 'Native window smoke failed' }
    if (-not $process.WaitForExit(12000) -or $process.ExitCode -ne 0) { throw 'Native window smoke did not exit cleanly' }
} finally { if (-not $process.HasExited) { $process.Kill($true) }; $process.Dispose() }
$process = Start-Helper '--self-test-pipe'
try {
    $reply = Read-Reply $process
    if ($reply.event -ne 'smoke_ready' -or $reply.ok -ne $true) { throw 'Native parent pipe smoke did not start' }
    $process.StandardInput.Close()
    if (-not $process.WaitForExit(12000) -or $process.ExitCode -ne 0) { throw 'Native helper survived parent pipe EOF' }
} finally { if (-not $process.HasExited) { $process.Kill($true) }; $process.Dispose() }
Write-Output 'PASS: Windows native window visibility, close/minimize and parent EOF'
