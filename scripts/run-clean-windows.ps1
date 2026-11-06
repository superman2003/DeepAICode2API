[CmdletBinding()]
param(
    [switch]$InitializeOnly,
    [switch]$NoBuild,
    [switch]$Build,
    [switch]$Once
)

$ErrorActionPreference='Stop'
$projectRoot=Split-Path -Parent $PSScriptRoot
$envFile=Join-Path $projectRoot '.env'
$dataDirectory=Join-Path $projectRoot 'data'
$stateFile=Join-Path $dataDirectory 'deepaicode2api-data.json'
$binary=Join-Path $projectRoot 'bin\deepaicode2api.exe'
Set-Location -LiteralPath $projectRoot

function New-PrivateHex([int]$Bytes) {
    $buffer=New-Object byte[] $Bytes
    $rng=[Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($buffer) } finally { $rng.Dispose() }
    return [BitConverter]::ToString($buffer).Replace('-','').ToLowerInvariant()
}

if (!(Test-Path -LiteralPath $envFile)) {
    if (Test-Path -LiteralPath $stateFile) { throw 'Existing encrypted data requires its original .env and master key.' }
    Copy-Item -LiteralPath (Join-Path $projectRoot '.env.example') -Destination $envFile
}
$configuration=@{}
foreach ($line in Get-Content -LiteralPath $envFile -Encoding UTF8) {
    if ($line -match '^\s*(?:#|$)') { continue }
    if ($line -notmatch '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$') { throw 'Invalid .env entry.' }
    $name,$value=$Matches[1],$Matches[2].Trim()
    if ($value.Length -ge 2 -and (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'")))) { $value=$value.Substring(1,$value.Length-2) }
    $configuration[$name]=$value
}
if ($configuration['DEEPAI2API_DATA_PATH']) {
    $stateFile=$configuration['DEEPAI2API_DATA_PATH']
    if (![IO.Path]::IsPathRooted($stateFile)) { $stateFile=Join-Path $projectRoot $stateFile }
    $stateFile=[IO.Path]::GetFullPath($stateFile)
}
$changed=$false
if (!$configuration['DEEPAI2API_MASTER_KEY']) {
    if (Test-Path -LiteralPath $stateFile) { throw 'Restore the original master key before opening existing data.' }
    $configuration['DEEPAI2API_MASTER_KEY']=New-PrivateHex 32
    $changed=$true
}
if (!$configuration['DEEPAI2API_ADMIN_PASSWORD']) {
    $configuration['DEEPAI2API_ADMIN_PASSWORD']=New-PrivateHex 12
    $changed=$true
}
if ($changed) {
    $text="# Private configuration; keep this file with your new data directory.`r`n"
    foreach ($name in $configuration.Keys | Sort-Object) { $text+=$name+'='+$configuration[$name]+"`r`n" }
    [IO.File]::WriteAllText($envFile,$text,(New-Object Text.UTF8Encoding($false)))
}
New-Item -ItemType Directory -Path $dataDirectory -Force | Out-Null
$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls.exe $dataDirectory /inheritance:r /grant:r "*${sid}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' /Q | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not protect the data directory.' }
& icacls.exe $envFile /inheritance:r /grant:r "*${sid}:F" '*S-1-5-18:F' /Q | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not protect .env.' }
Write-Host '[1/3] Private configuration ready. Admin password is saved in .env.'
if ($InitializeOnly) { Write-Output 'Initialized a fresh configuration; no accounts were imported.'; exit 0 }

$registrationBinary=Join-Path $projectRoot 'bin\register.exe'
$needsBuild=$Build -or !(Test-Path -LiteralPath $binary) -or !(Test-Path -LiteralPath $registrationBinary)
if (!$NoBuild -and $needsBuild) {
    $go=Get-Command go.exe -ErrorAction SilentlyContinue
    if (!$go) { throw 'Install Go 1.25 or newer, reopen PowerShell, then run this script again.' }
    Write-Host '[2/3] Building gateway and registration CLI...'
    New-Item -ItemType Directory -Path (Join-Path $projectRoot 'bin') -Force | Out-Null
    & $go.Source build -trimpath -o $binary .
    if ($LASTEXITCODE -ne 0) { throw 'Gateway build failed.' }
    & $go.Source build -trimpath -o $registrationBinary ./cmd/register
    if ($LASTEXITCODE -ne 0) { throw 'Registration CLI build failed.' }
} elseif (!$needsBuild) {
    Write-Host '[2/3] Using the included Windows executables. Go is not required.'
}
if (!(Test-Path -LiteralPath $binary)) { throw 'Missing compiled gateway. Run without -NoBuild first.' }
foreach ($name in $configuration.Keys) { [Environment]::SetEnvironmentVariable($name,$configuration[$name],'Process') }
[Environment]::SetEnvironmentVariable('DEEPAI2API_MANAGED_UPDATES',$(if($Once){''}else{'1'}),'Process')
$updateScript=Join-Path $PSScriptRoot 'web-update.ps1'
if (Test-Path -LiteralPath $updateScript) { . $updateScript }
$runningProcess=$null
Write-Host '[3/3] Starting in this window. Runtime errors and the first gateway API key appear below.'
do {
    if (!$runningProcess) { $runningProcess=Start-Process -FilePath $binary -ArgumentList '-no-import' -WorkingDirectory $projectRoot -NoNewWindow -PassThru; $null=$runningProcess.Handle }
    $runningProcess.WaitForExit()
    $exitCode=$runningProcess.ExitCode
    $runningProcess=$null
    if ($Once) { exit $exitCode }
    $updateData=Split-Path -Parent $stateFile
    $request=Join-Path $updateData 'updater\request.json'
    if ($exitCode -eq 75 -and (Test-Path -LiteralPath $request) -and (Get-Command Invoke-GatewayWebUpdate -ErrorAction SilentlyContinue)) {
        try {
            $port=if($configuration['DEEPAI2API_PORT']){[int]$configuration['DEEPAI2API_PORT']}else{10087}
            $runningProcess=Invoke-GatewayWebUpdate -ProjectRoot $projectRoot -DataRoot $updateData -StatePath $stateFile -Port $port
            continue
        } catch { Write-Host ('Web update failed before restart: '+$_.Exception.Message) }
    }
    Write-Host ('Gateway exited with code '+$exitCode+'. Restarting in 5 seconds; Ctrl+C stops this script.')
    Start-Sleep -Seconds 5
} while ($true)
