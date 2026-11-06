[CmdletBinding()]
param(
    [ValidateSet('Install','Start','Stop','Restart','Status','Validate','Backup','RepairPermissions','RemoveAutostart')]
    [string]$Action = 'Status',
    [switch]$OpenFirewall,
    [switch]$Offline,
    [string[]]$AllowedRemoteAddress = @('Any'),
    [ValidateRange(2,365)]
    [int]$KeepBackups = 14
)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$envPath = Join-Path $projectRoot '.env'
$dataDirectory = Join-Path $projectRoot 'data'
$executable = Join-Path $projectRoot 'bin\deepaicode2api.exe'
$runner = Join-Path $PSScriptRoot 'windows.ps1'
$powershell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
$taskName = 'DeepAICode2API-Server'
$backupTaskName = 'DeepAICode2API-Backup'
$scriptPath = Join-Path $PSScriptRoot 'windows-server.ps1'

function Import-LocalConfiguration {
    $configuration = @{}
    foreach ($line in Get-Content -LiteralPath $envPath -Encoding UTF8) {
        if ($line -match '^\s*(?:#|$)') { continue }
        if ($line -notmatch '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$') { throw 'Invalid .env entry.' }
        $name, $value = $Matches[1], $Matches[2].Trim()
        if ($value.Length -ge 2 -and (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'")))) {
            $value = $value.Substring(1,$value.Length-2)
        }
        $configuration[$name] = $value
        [Environment]::SetEnvironmentVariable($name,$value,'Process')
    }
    return $configuration
}

function Require-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (!$principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Run Windows PowerShell as Administrator.'
    }
}

function Resolve-StatePath {
    $path = $configuration['DEEPAI2API_DATA_PATH']
    if (!$path) { throw 'Set DEEPAI2API_DATA_PATH in .env.' }
    if (![IO.Path]::IsPathRooted($path)) { $path = Join-Path $projectRoot $path }
    return [IO.Path]::GetFullPath($path)
}

function Validate-State([string]$Path) {
    if (!(Test-Path -LiteralPath $Path -PathType Leaf)) { throw 'The account-state path is not a readable file. Fully extract the migration package and check its data directory.' }
    try {
        $stream=[IO.File]::Open($Path,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::ReadWrite)
        $stream.Dispose()
    } catch [UnauthorizedAccessException] {
        throw 'NTFS denied reading the state file. Run -Action RepairPermissions as Administrator, then retry.'
    }
    $json = & $executable -check-state -data $Path
    if ($LASTEXITCODE -ne 0) { throw 'State validation failed. Check the encryption key and data file.' }
    $result = ($json -join "`n") | ConvertFrom-Json
    if (!$result.ok) { throw 'State validation failed.' }
    return $result
}

function Get-LocalHealth {
    try { return Invoke-RestMethod -Uri "$baseUrl/health" -TimeoutSec 3 } catch { return $null }
}

function Start-Server {
    Start-ScheduledTask -TaskName $taskName
    for ($attempt=0;$attempt -lt 60;$attempt++) {
        $health = Get-LocalHealth
        if ($health) { $health | ConvertTo-Json -Compress; return }
        Start-Sleep -Milliseconds 500
    }
    throw 'Server did not become ready. Check data\gateway.stderr.log.'
}

function Stop-Server {
    if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) { Stop-ScheduledTask -TaskName $taskName }
    & $powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $runner -Action Stop
    if ($LASTEXITCODE -ne 0) { throw 'Could not stop the gateway.' }
}

function Stop-PreviousServerInstance {
    $previous=Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if (!$previous) { return }
    Stop-ScheduledTask -TaskName $taskName
    foreach ($previousAction in $previous.Actions) {
        if (!$previousAction.WorkingDirectory) { continue }
        $previousRoot=[IO.Path]::GetFullPath($previousAction.WorkingDirectory)
        $previousRunner=Join-Path $previousRoot 'scripts\windows.ps1'
        if (Test-Path -LiteralPath $previousRunner -PathType Leaf) {
            & $powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $previousRunner -Action Stop | Out-Null
            if ($LASTEXITCODE -ne 0) { throw 'Could not stop the previous server supervisor.' }
        }
        $previousExecutable=Join-Path $previousRoot 'bin\deepaicode2api.exe'
        Get-CimInstance Win32_Process -Filter "Name='deepaicode2api.exe'" | Where-Object { $_.ExecutablePath -eq $previousExecutable } | ForEach-Object {
            Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
        }
    }
}

function Assert-PrivateChild([string]$Path) {
    $root = [IO.Path]::GetFullPath($dataDirectory).TrimEnd('\') + '\'
    $resolved = [IO.Path]::GetFullPath($Path)
    if (!$resolved.StartsWith($root,[StringComparison]::OrdinalIgnoreCase)) { throw 'Refusing an operation outside the private data directory.' }
}

function Backup-Server {
    # Save in-memory state first when the gateway is reachable. Atomic state
    # files can still be backed up and validated when the process is down.
    if (!$Offline) {
        try {
            $headers = @{'X-Admin-Password'=$configuration['DEEPAI2API_ADMIN_PASSWORD']}
            Invoke-RestMethod -Uri "$baseUrl/admin/api/settings" -Method Post -Headers $headers -ContentType application/json -Body '{}' -TimeoutSec 5 | Out-Null
        } catch { }
    }
    $backupRoot = Join-Path $dataDirectory 'server-backups'
    New-Item -ItemType Directory -Path $backupRoot -Force | Out-Null
    $stage = Join-Path $dataDirectory ('.backup-stage-' + [guid]::NewGuid().ToString('N'))
    Assert-PrivateChild $stage
    New-Item -ItemType Directory -Path (Join-Path $stage 'data') -Force | Out-Null
    try {
        Copy-Item -LiteralPath $envPath -Destination (Join-Path $stage '.env')
        $snapshot = Join-Path $stage 'data\deepaicode2api-data.json'
        Copy-Item -LiteralPath (Resolve-StatePath) -Destination $snapshot
        $validation = Validate-State $snapshot
        foreach ($name in @('exports','result','tempmail-lol-inboxes')) {
            $source = Join-Path $dataDirectory $name
            if (Test-Path -LiteralPath $source) { Copy-Item -LiteralPath $source -Destination (Join-Path $stage 'data') -Recurse }
        }
        $destination = Join-Path $backupRoot ('server-backup-' + (Get-Date -Format 'yyyyMMdd-HHmmss-fff') + '.zip')
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        [IO.Compression.ZipFile]::CreateFromDirectory($stage,$destination,[IO.Compression.CompressionLevel]::Optimal,$false)
        Get-FileHash -LiteralPath $destination -Algorithm SHA256 | Select-Object Hash,Path | ConvertTo-Json -Compress
        Get-ChildItem -LiteralPath $backupRoot -Filter 'server-backup-*.zip' -File | Sort-Object LastWriteTime -Descending | Select-Object -Skip $KeepBackups | ForEach-Object {
            Assert-PrivateChild $_.FullName
            Remove-Item -LiteralPath $_.FullName -Force
        }
        [PSCustomObject]@{Backup=$destination;Accounts=$validation.accounts;Keys=$validation.keys;Encrypted=$validation.encrypted} | ConvertTo-Json -Compress
    } finally {
        Assert-PrivateChild $stage
        Remove-Item -LiteralPath $stage -Recurse -Force
    }
}

. (Join-Path $PSScriptRoot 'windows-private-permissions.ps1')
if ($Action -in @('Install','RepairPermissions')) {
    Require-Administrator
    Repair-GatewayPermissions -ProjectRoot $projectRoot -TakeOwnership
    if ($Action -eq 'RepairPermissions') { Write-Output 'Repaired private-file permissions; account data and master key were preserved.'; return }
}
if (!(Test-Path -LiteralPath $envPath)) { throw 'Missing .env.' }
if (!(Test-Path -LiteralPath $executable)) { throw 'Missing bin\deepaicode2api.exe.' }
$configuration = Import-LocalConfiguration
$port = if ($configuration['DEEPAI2API_PORT']) { [int]$configuration['DEEPAI2API_PORT'] } else { 10087 }
$baseUrl = 'http://127.0.0.1:' + $port
Set-Location -LiteralPath $projectRoot

switch ($Action) {
    'Validate' { Validate-State (Resolve-StatePath) | ConvertTo-Json -Compress }
    'Backup' { Backup-Server }
    'Install' {
        Require-Administrator
        if (!$configuration['DEEPAI2API_MASTER_KEY'] -or !$configuration['DEEPAI2API_ADMIN_PASSWORD']) { throw 'Preserve the migrated master key and configure an admin password in .env.' }
        $validation = Validate-State (Resolve-StatePath)
        if ($validation.keys -lt 1) { throw 'A gateway API key is required before opening the server listener.' }
        Stop-PreviousServerInstance
        $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
        $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
        $runArguments = '-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "{0}" -Action Run' -f $runner
        $runAction = New-ScheduledTaskAction -Execute $powershell -Argument $runArguments -WorkingDirectory $projectRoot
        Register-ScheduledTask -TaskName $taskName -Action $runAction -Trigger (New-ScheduledTaskTrigger -AtStartup) -Principal $principal -Settings $settings -Description 'DeepAICode2API server supervisor; starts without interactive login.' -Force | Out-Null
        $backupArguments = '-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "{0}" -Action Backup -KeepBackups {1}' -f $scriptPath,$KeepBackups
        $backupAction = New-ScheduledTaskAction -Execute $powershell -Argument $backupArguments -WorkingDirectory $projectRoot
        $backupSettings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 15) -StartWhenAvailable -MultipleInstances IgnoreNew
        Register-ScheduledTask -TaskName $backupTaskName -Action $backupAction -Trigger (New-ScheduledTaskTrigger -Daily -At '03:00') -Principal $principal -Settings $backupSettings -Description 'Daily encrypted-state and private-configuration backup.' -Force | Out-Null
        if ($OpenFirewall) {
            $ruleName = 'DeepAICode2API-' + $port
            if (Get-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue) {
                Set-NetFirewallRule -Name $ruleName -Enabled True -Action Allow -RemoteAddress $AllowedRemoteAddress | Out-Null
            } else {
                New-NetFirewallRule -Name $ruleName -DisplayName $ruleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $port -RemoteAddress $AllowedRemoteAddress | Out-Null
            }
        }
        Start-Server
        Backup-Server
    }
    'Start' { Require-Administrator; Start-Server }
    'Stop' { Require-Administrator; Stop-Server }
    'Restart' { Require-Administrator; Stop-Server; Start-Server }
    'RemoveAutostart' {
        Require-Administrator
        foreach ($name in @($taskName,$backupTaskName)) {
            if (Get-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue) { Unregister-ScheduledTask -TaskName $name -Confirm:$false }
        }
        Write-Output 'Removed scheduled tasks; data and current process were preserved.'
    }
    'Status' {
        $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        $backupTask = Get-ScheduledTask -TaskName $backupTaskName -ErrorAction SilentlyContinue
        [PSCustomObject]@{TaskState=if($task){[string]$task.State}else{'Not installed'};DailyBackup=[bool]$backupTask;Health=Get-LocalHealth;Port=$port} | ConvertTo-Json -Depth 4
    }
}
