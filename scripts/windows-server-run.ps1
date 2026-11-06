# A small boot-task entrypoint with no interactive-profile dependencies. Capture
# PowerShell initialization errors as well as the application's own log files.
$ErrorActionPreference='Stop'
$projectRoot=Split-Path -Parent $PSScriptRoot
$dataDirectory=Join-Path $projectRoot 'data'
$log=Join-Path $dataDirectory 'server-runner.log'
try {
    New-Item -ItemType Directory -Path $dataDirectory -Force | Out-Null
    Add-Content -LiteralPath $log -Value ((Get-Date -Format o)+' Runner started as '+[Security.Principal.WindowsIdentity]::GetCurrent().Name) -Encoding UTF8
    & (Join-Path $PSScriptRoot 'windows.ps1') -Action Run
    $code=$LASTEXITCODE
    if ($null -eq $code) { $code=0 }
    Add-Content -LiteralPath $log -Value ((Get-Date -Format o)+' Runner exited: '+$code) -Encoding UTF8
    exit $code
} catch {
    $message=($_ | Out-String)
    try { Add-Content -LiteralPath $log -Value ((Get-Date -Format o)+" Runner error:`r`n"+$message) -Encoding UTF8 } catch { }
    Write-Error $message -ErrorAction Continue
    exit 1
}
