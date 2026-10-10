# PowerShell 7. Native startup boundary only; use an empty disposable OS account.
param(
  [Parameter(Mandatory=$true)][string]$Executable,
  [Parameter(Mandatory=$true)][string]$Output,
  [switch]$IsolatedTestAccount
)
$ErrorActionPreference = 'Stop'
if ($PSVersionTable.PSVersion.Major -lt 7) { throw 'PowerShell 7 is required.' }
if ($env:OS -ne 'Windows_NT' -or [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') {
  throw 'A native Windows x64 runner is required.'
}
if (-not $IsolatedTestAccount) { throw 'An empty disposable test account must be explicitly confirmed.' }
$taskExe = (Resolve-Path -LiteralPath $Executable).Path
$taskHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $taskExe).Hash.ToLowerInvariant()
$taskCases = @(
  @{ Name='plain_launch'; Args=@(); Normal=$true },
  @{ Name='token_flag'; Args=@('--access-token=owned-fixture'); Normal=$false },
  @{ Name='split_token'; Args=@('--refresh-token','owned-fixture'); Normal=$false },
  @{ Name='business_https'; Args=@('https://owned.invalid/chat/fixture'); Normal=$false },
  @{ Name='loopback_in_argv'; Args=@('http://127.0.0.1:12345/oauth/callback?code=owned'); Normal=$false },
  @{ Name='custom_uri'; Args=@('com.enterpriseim.client:/oauth/callback?code=owned'); Normal=$false },
  @{ Name='file_path'; Args=@('C:\owned\private.txt'); Normal=$false },
  @{ Name='empty_argument'; Args=@(''); Normal=$false },
  @{ Name='cold_com_activation'; Args=@('-Embedding'); Normal=$false }
)
$taskResults = [System.Collections.Generic.List[object]]::new()
foreach ($taskCase in $taskCases) {
  $taskProcess = [System.Diagnostics.Process]::new()
  $taskProcess.StartInfo.FileName = $taskExe
  $taskProcess.StartInfo.WorkingDirectory = Split-Path -Parent $taskExe
  $taskProcess.StartInfo.UseShellExecute = $false
  foreach ($taskArg in $taskCase.Args) { $taskProcess.StartInfo.ArgumentList.Add($taskArg) }
  $taskExit = $null; $taskPid = $null; $taskFailure = $null; $taskPassed = $false; $taskForcedCleanup = $false; $taskCleanupFailed = $false
  try {
    if (-not $taskProcess.Start()) { throw 'start_failed' }
    $taskPid = $taskProcess.Id
    if ($taskCase.Normal) {
      $taskDeadline = [DateTime]::UtcNow.AddSeconds(25)
      do {
        $taskProcess.Refresh()
        if ($taskProcess.HasExited) { throw 'normal_start_exited_before_window' }
        if ($taskProcess.MainWindowHandle -ne [IntPtr]::Zero) { break }
        Start-Sleep -Milliseconds 100
      } while ([DateTime]::UtcNow -lt $taskDeadline)
      if ($taskProcess.MainWindowHandle -eq [IntPtr]::Zero) { throw 'normal_window_timeout' }
      if (-not $taskProcess.CloseMainWindow()) { throw 'normal_window_close_failed' }
      if (-not $taskProcess.WaitForExit(10000)) { throw 'normal_close_timeout' }
      $taskExit = $taskProcess.ExitCode
      if ($taskExit -ne 0) { throw 'normal_exit_not_zero' }
    } else {
      if (-not $taskProcess.WaitForExit(10000)) { throw 'rejected_input_started_app' }
      $taskExit = $taskProcess.ExitCode
      if ($taskExit -ne 1) { throw 'rejection_exit_not_one' }
    }
    $taskPassed = $true
  } catch {
    # Never put argument values, native command lines or credentials in reports.
    $taskFailure = $_.Exception.GetType().Name
  } finally {
    if ($taskPid -and -not $taskProcess.HasExited) {
      $taskForcedCleanup = $true
      try {
        $taskProcess.Kill($true)
        if (-not $taskProcess.WaitForExit(10000)) { throw 'owned_process_cleanup_failed' }
        $taskExit = $taskProcess.ExitCode
      } catch {
        $taskCleanupFailed = $true
        $taskPassed = $false
        $taskFailure = 'owned_process_cleanup_failed'
      }
    }
    $taskResults.Add(@{ name=$taskCase.Name; pid=$taskPid; passed=$taskPassed; actual_exit=$taskExit; forced_cleanup=$taskForcedCleanup; cleanup_failed=$taskCleanupFailed; failure_type=$taskFailure })
    $taskProcess.Dispose()
  }
}
$taskUnchanged = (Get-FileHash -Algorithm SHA256 -LiteralPath $taskExe).Hash.ToLowerInvariant() -eq $taskHash
$taskFailed = @($taskResults | Where-Object { -not $_.passed }).Count
$taskReport = @{ scope='native Windows startup/argument rejection only'; executable_sha256=$taskHash; unchanged=$taskUnchanged; cases=$taskResults; failures=$taskFailed; isolated_test_account=$true; os_version=[Environment]::OSVersion.VersionString }
$taskReport | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $Output -Encoding utf8
if (-not $taskUnchanged -or $taskFailed -ne 0) { throw 'native_startup_gate_failed; see case names and observed exits' }
