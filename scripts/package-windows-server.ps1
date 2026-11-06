[CmdletBinding()]
param([string]$OutputDirectory = '')

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$privateData = Join-Path $projectRoot 'data'
if (!$OutputDirectory) { $OutputDirectory = Join-Path $privateData 'server-migration' }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
$buildDirectory = Join-Path $privateData '.windows-server-build\bin'
$sourceEnv = @{}
foreach ($line in Get-Content -LiteralPath (Join-Path $projectRoot '.env') -Encoding UTF8) {
    if ($line -match '^\s*(?:#|$)') { continue }
    if ($line -notmatch '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$') { throw 'Invalid .env entry.' }
    $name,$value=$Matches[1],$Matches[2].Trim()
    if ($value.Length -ge 2 -and (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'")))) { $value=$value.Substring(1,$value.Length-2) }
    $sourceEnv[$name]=$value
}
if (!$sourceEnv['DEEPAI2API_MASTER_KEY']) { throw 'The existing encryption master key is required.' }
$sourceState=$sourceEnv['DEEPAI2API_DATA_PATH']
if (![IO.Path]::IsPathRooted($sourceState)) { $sourceState=Join-Path $projectRoot $sourceState }
if (!(Test-Path -LiteralPath $sourceState)) { throw 'The account state file is missing.' }
foreach ($binary in @('deepaicode2api.exe','register.exe')) {
    if (!(Test-Path -LiteralPath (Join-Path $buildDirectory $binary))) { throw 'Build the server executables in data\.windows-server-build\bin first.' }
}

# Persist current in-memory state without changing settings. The file itself is
# atomically replaced by the gateway, so reading it captures one complete state.
$runningJobs=$null
try {
    $sourcePort=if($sourceEnv['DEEPAI2API_PORT']){$sourceEnv['DEEPAI2API_PORT']}else{'10087'}
    $headers=@{'X-Admin-Password'=$sourceEnv['DEEPAI2API_ADMIN_PASSWORD']}
    Invoke-RestMethod -Uri "http://127.0.0.1:$sourcePort/admin/api/settings" -Method Post -Headers $headers -ContentType application/json -Body '{}' -TimeoutSec 5 | Out-Null
    $jobs=Invoke-RestMethod -Uri "http://127.0.0.1:$sourcePort/admin/api/register" -Headers $headers -TimeoutSec 5
    $runningJobs=@($jobs.jobs | Where-Object running).Count
} catch { }

$stamp=Get-Date -Format 'yyyyMMdd-HHmmss'
$stage=Join-Path $OutputDirectory ('.stage-'+[guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path (Join-Path $stage 'data'),(Join-Path $stage 'bin') -Force | Out-Null
try {
    foreach ($directory in @('cmd','internal','docs','scripts')) {
        Copy-Item -LiteralPath (Join-Path $projectRoot $directory) -Destination $stage -Recurse
    }
    foreach ($file in Get-ChildItem -LiteralPath $projectRoot -File -Force) {
        if ($file.Extension -eq '.go' -or $file.Name -in @('go.mod','go.sum','README.md','LICENSE','Makefile','Dockerfile','docker-compose.yml','.env.example','.gitignore','.dockerignore','install-server.cmd','start-foreground.cmd')) {
            Copy-Item -LiteralPath $file.FullName -Destination $stage
        }
    }
    foreach ($binary in @('deepaicode2api.exe','register.exe')) {
        Copy-Item -LiteralPath (Join-Path $buildDirectory $binary) -Destination (Join-Path $stage 'bin')
    }
    $snapshot=Join-Path $stage 'data\deepaicode2api-data.json'
    [IO.File]::WriteAllBytes($snapshot,[IO.File]::ReadAllBytes($sourceState))
    foreach ($name in @('exports','result','tempmail-lol-inboxes')) {
        $source=Join-Path $privateData $name
        if (Test-Path -LiteralPath $source) { Copy-Item -LiteralPath $source -Destination (Join-Path $stage 'data') -Recurse }
    }
    $access=Join-Path $privateData 'access.txt'
    if (Test-Path -LiteralPath $access) { Copy-Item -LiteralPath $access -Destination (Join-Path $stage 'data\access.txt') }

    $serverEnv=@{}
    foreach ($name in $sourceEnv.Keys) { $serverEnv[$name]=$sourceEnv[$name] }
    $serverEnv['DEEPAI2API_HOST']='0.0.0.0'
    $serverEnv['DEEPAI2API_PORT']='10087'
    $serverEnv['DEEPAI2API_DATA_PATH']='.\data\deepaicode2api-data.json'
    $serverEnv['DEEPAI2API_GATEWAY']='http://127.0.0.1:10087'
    foreach ($name in @('HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','DEEPAI2API_EGRESS_PROXY')) { $serverEnv[$name]='' }
    $serverEnv['NO_PROXY']='127.0.0.1,localhost,::1'
    $serverEnv['DEEPAI2API_PROXY_FALLBACK_DIRECT']='1'
    $serverEnv['DEEPAI2API_WATCHDOG']='1'
    $serverEnv['DEEPAI2API_FAST_START']='1'
    $serverEnv['TEMPMAIL_LOL_TOKEN_DIR']='.\data\exports\tempmail-lol-inboxes'
    if ($sourceEnv['PROXY_POOL_FILE']) {
        $proxyFile=$sourceEnv['PROXY_POOL_FILE']
        if (![IO.Path]::IsPathRooted($proxyFile)) { $proxyFile=Join-Path $projectRoot $proxyFile }
        Copy-Item -LiteralPath $proxyFile -Destination (Join-Path $stage 'data\proxy-pool.txt')
        $serverEnv['PROXY_POOL_FILE']='.\data\proxy-pool.txt'
    }
    if ($sourceEnv['TEMPMAIL_LOL_TOKEN_DIR']) {
        $tokenSource=$sourceEnv['TEMPMAIL_LOL_TOKEN_DIR']
        if (![IO.Path]::IsPathRooted($tokenSource)) { $tokenSource=Join-Path $projectRoot $tokenSource }
        $tokenTarget=Join-Path $stage 'data\exports\tempmail-lol-inboxes'
        if (Test-Path -LiteralPath $tokenSource) {
            New-Item -ItemType Directory -Path $tokenTarget -Force | Out-Null
            Get-ChildItem -LiteralPath $tokenSource -Force | ForEach-Object { Copy-Item -LiteralPath $_.FullName -Destination $tokenTarget -Recurse -Force }
        }
    }
    $envText="# Windows x64 server migration; preserve this master key with the state file.`r`n"
    foreach ($name in $serverEnv.Keys | Sort-Object) { $envText += $name+'='+$serverEnv[$name]+"`r`n" }
    [IO.File]::WriteAllText((Join-Path $stage '.env'),$envText,(New-Object Text.UTF8Encoding($false)))
    $previousMasterKey=[Environment]::GetEnvironmentVariable('DEEPAI2API_MASTER_KEY','Process')
    try {
        [Environment]::SetEnvironmentVariable('DEEPAI2API_MASTER_KEY',$serverEnv['DEEPAI2API_MASTER_KEY'],'Process')
        $validationJSON=& (Join-Path $stage 'bin\deepaicode2api.exe') -check-state -data $snapshot
        if ($LASTEXITCODE -ne 0) { throw 'Migrated account state could not be decrypted.' }
        $validation=($validationJSON -join "`n") | ConvertFrom-Json
        if (!$validation.ok -or $validation.keys -lt 1) { throw 'Migrated gateway credentials are incomplete.' }
    } finally { [Environment]::SetEnvironmentVariable('DEEPAI2API_MASTER_KEY',$previousMasterKey,'Process') }
    $manifest=[ordered]@{
        createdAtUtc=(Get-Date).ToUniversalTime().ToString('o')
        platform='windows-amd64'
        version=((& (Join-Path $stage 'bin\deepaicode2api.exe') -version) -join '')
        accounts=$validation.accounts
        gatewayKeys=$validation.keys
        encrypted=$validation.encrypted
        stateSHA256=(Get-FileHash -LiteralPath $snapshot -Algorithm SHA256).Hash
        sourceRunningRegistrationJobs=$runningJobs
        proxyEgress='server-native; no local 7890 dependency'
        proxyConnectionFailover='enabled before origin request submission'
        startup='SYSTEM boot task'
        backup='daily 03:00; keep 14; original master key included'
    }
    [IO.File]::WriteAllText((Join-Path $stage 'migration-manifest.json'),($manifest | ConvertTo-Json -Depth 4),(New-Object Text.UTF8Encoding($false)))
    Copy-Item -LiteralPath (Join-Path $projectRoot 'docs\windows-server.md') -Destination (Join-Path $stage 'START-HERE.md')
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $archive=Join-Path $OutputDirectory ('deepaicode2api-windows-amd64-'+$stamp+'.zip')
    [IO.Compression.ZipFile]::CreateFromDirectory($stage,$archive,[IO.Compression.CompressionLevel]::Optimal,$false)
    $checksum=(Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash
    [IO.File]::WriteAllText(($archive+'.sha256'),($checksum+'  '+[IO.Path]::GetFileName($archive)+"`r`n"),(New-Object Text.UTF8Encoding($false)))
    [PSCustomObject]@{Archive=$archive;Accounts=$validation.accounts;GatewayKeys=$validation.keys;Encrypted=$validation.encrypted;SHA256=$checksum;SourceRunningJobs=$runningJobs} | ConvertTo-Json
} finally {
    $resolvedStage=[IO.Path]::GetFullPath($stage)
    $resolvedOutput=$OutputDirectory.TrimEnd('\')+'\'
    if (!$resolvedStage.StartsWith($resolvedOutput,[StringComparison]::OrdinalIgnoreCase)) { throw 'Refusing cleanup outside the package output directory.' }
    Remove-Item -LiteralPath $resolvedStage -Recurse -Force
}
