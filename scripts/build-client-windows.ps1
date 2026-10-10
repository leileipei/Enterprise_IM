# Execute only on a Windows x64 runner. No signing or installation is implied.
param([Parameter(Mandatory=$true)][string]$FlutterSdk)
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT' -or [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') {
  throw 'This gate requires a native Windows x64 runner.'
}
$taskSdkHead = & git -C $FlutterSdk rev-parse HEAD
if ($LASTEXITCODE -ne 0 -or $taskSdkHead -ne 'e8113bf45620cbeb8aff64947ee4c93e16adb4cf') {
  throw 'Fixed Flutter SDK source verification failed.'
}
$taskSdkChanges = & git -C $FlutterSdk status --porcelain --untracked-files=no
if ($LASTEXITCODE -ne 0 -or $taskSdkChanges) { throw 'Flutter SDK tracked source is modified.' }
$taskClientRoot = Join-Path $PSScriptRoot '../clients/enterprise_im'
Push-Location $taskClientRoot
try {
  $env:CI = 'true'
  $env:FLUTTER_SUPPRESS_ANALYTICS = 'true'
  & (Join-Path $FlutterSdk 'bin/flutter.bat') build windows --release --no-pub
  if ($LASTEXITCODE -ne 0) { throw "Windows build failed with actual exit $LASTEXITCODE." }
} finally { Pop-Location }
