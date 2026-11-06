function Set-WebUpdateState {
    param([string]$Directory,[string]$Phase,$Job,[string]$Message='')
    $state=[ordered]@{phase=$Phase;target_version=$Job.version;job_id=$Job.id;error=$Message;updated_at=[DateTimeOffset]::UtcNow.ToUnixTimeSeconds()}
    $path=Join-Path $Directory 'state.json'
    $temporary=$path+'.'+[guid]::NewGuid().ToString('N')+'.tmp'
    [IO.File]::WriteAllText($temporary,($state|ConvertTo-Json -Compress),(New-Object Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $temporary -Destination $path -Force
}

function Start-WebUpdatedProgram {
    param([string]$Root)
    $process=Start-Process -FilePath (Join-Path $Root 'bin\deepaicode2api.exe') -ArgumentList '-no-import' -WorkingDirectory $Root -NoNewWindow -PassThru
    $null=$process.Handle
    return $process
}

function Wait-WebUpdatedProgram {
    param($Process,[string]$Version,[int]$Port,[int]$Accounts)
    $deadline=[DateTime]::UtcNow.AddSeconds(45)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($Process.HasExited) { return $false }
        try {
            $health=Invoke-RestMethod -Uri ('http://127.0.0.1:'+$Port+'/health') -TimeoutSec 1
            if ($health.status -eq 'ok' -and $health.version -eq $Version -and [int]$health.accounts -ge $Accounts) { return $true }
        } catch { }
        Start-Sleep -Milliseconds 400
    }
    return $false
}

function Invoke-GatewayWebUpdate {
    param([string]$ProjectRoot,[string]$DataRoot,[string]$StatePath,[int]$Port)
    $directory=Join-Path $DataRoot 'updater'
    $request=Join-Path $directory 'request.json'
    $job=Get-Content -LiteralPath $request -Raw -Encoding UTF8 | ConvertFrom-Json
    $root=[IO.Path]::GetFullPath($ProjectRoot).TrimEnd('\')
    $data=[IO.Path]::GetFullPath($DataRoot).TrimEnd('\')
    if ($job.id -notmatch '^[a-f0-9]{24}$' -or [IO.Path]::GetFullPath($job.project_root).TrimEnd('\') -ne $root -or [IO.Path]::GetFullPath($job.data_root).TrimEnd('\') -ne $data) { throw 'Invalid update request scope.' }
    $allowed=@('bin/deepaicode2api.exe','bin/register.exe')
    if (@($job.files).Count -ne 2 -or @($job.files|ForEach-Object path|Select-Object -Unique).Count -ne 2) { throw 'Invalid update file list.' }
    foreach ($file in $job.files) { if ($file.path -notin $allowed -or $file.sha256 -notmatch '^[a-fA-F0-9]{64}$') { throw 'Update attempted to replace a protected path.' } }
    $stage=Join-Path (Join-Path $directory 'staged') $job.id
    $backup=Join-Path (Join-Path $directory 'backups') $job.id
    $environment=Join-Path $root '.env'
    $newProcess=$null
    $replaced=$false
    try {
        if ((Get-FileHash -LiteralPath $environment -Algorithm SHA256).Hash -ne $job.environment_sha256) { throw '.env changed after the update request; update cancelled.' }
        foreach ($file in $job.files) {
            $source=Join-Path $stage $file.path
            if ((Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash -ne $file.sha256) { throw 'Staged update checksum mismatch.' }
        }
        $candidate=Join-Path $stage 'bin\deepaicode2api.exe'
        $checkJSON=& $candidate -check-state -data $StatePath
        if ($LASTEXITCODE -ne 0) { throw 'New version cannot read the existing encrypted account database.' }
        $check=($checkJSON -join "`n")|ConvertFrom-Json
        if (!$check.ok -or [int]$check.accounts -lt [int]$job.accounts) { throw 'New version failed account-data compatibility validation.' }
        Set-WebUpdateState $directory 'replacing' $job
        New-Item -ItemType Directory -Path (Join-Path $backup 'bin') -Force | Out-Null
        foreach ($file in $job.files) { Copy-Item -LiteralPath (Join-Path $root $file.path) -Destination (Join-Path $backup $file.path) -Force }
        $replaced=$true
        foreach ($file in $job.files) { Copy-Item -LiteralPath (Join-Path $stage $file.path) -Destination (Join-Path $root $file.path) -Force }
        Set-WebUpdateState $directory 'restarting' $job
        $newProcess=Start-WebUpdatedProgram $root
        if (!(Wait-WebUpdatedProgram $newProcess $job.version $Port ([int]$job.accounts))) { throw 'New version did not pass the startup health check.' }
        if ((Get-FileHash -LiteralPath $environment -Algorithm SHA256).Hash -ne $job.environment_sha256) { throw 'Unexpected .env change after restart.' }
        Set-WebUpdateState $directory 'succeeded' $job
        Remove-Item -LiteralPath $request -Force
        Write-Host ('Update completed: '+$job.version)
        return $newProcess
    } catch {
        $message=$_.Exception.Message
        if ($newProcess -and !$newProcess.HasExited) { Stop-Process -Id $newProcess.Id -Force -ErrorAction SilentlyContinue; $newProcess.WaitForExit() }
        Set-WebUpdateState $directory 'rolling_back' $job $message
        if ($replaced) {
            foreach ($file in $job.files) {
                $original=Join-Path $backup $file.path
                $target=Join-Path $root $file.path
                if (Test-Path -LiteralPath $original) {
                    if (!(Test-Path -LiteralPath $target) -or (Get-FileHash -LiteralPath $original).Hash -ne (Get-FileHash -LiteralPath $target).Hash) { Copy-Item -LiteralPath $original -Destination $target -Force }
                }
            }
        }
        $oldProcess=Start-WebUpdatedProgram $root
        $recovered=Wait-WebUpdatedProgram $oldProcess $job.previous_version $Port ([int]$job.accounts)
        $phase=if($recovered){'rolled_back'}else{'failed'}
        Set-WebUpdateState $directory $phase $job $message
        Remove-Item -LiteralPath $request -Force -ErrorAction SilentlyContinue
        Write-Host ('Update failed; '+$phase+': '+$message)
        return $oldProcess
    }
}
