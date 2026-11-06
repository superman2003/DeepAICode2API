[CmdletBinding()]
param(
    [ValidateSet('Install', 'Start', 'Stop', 'Restart', 'Status', 'Run', 'Foreground')]
    [string]$Action = 'Status'
)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$executable = Join-Path $projectRoot 'bin\deepaicode2api.exe'
$dataDirectory = Join-Path $projectRoot 'data'
$scriptPath = Join-Path $PSScriptRoot 'windows.ps1'
$powershellPath = Join-Path $PSHOME 'powershell.exe'
$runArguments = '-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "{0}" -Action Run' -f $scriptPath
$startupShortcut = $null
# SYSTEM has no interactive Startup folder. Resolve it only for the legacy
# per-user shortcut action, never while booting the server supervisor.
if ($Action -in @('Install','Status')) {
    $startupDirectory = [Environment]::GetFolderPath('Startup')
    if ($startupDirectory) { $startupShortcut = Join-Path $startupDirectory 'DeepAICode2API.lnk' }
}
$baseUrl = 'http://127.0.0.1:10087'
$envFile = Join-Path $projectRoot '.env'
if (Test-Path -LiteralPath $envFile) {
    $portSetting = Get-Content -LiteralPath $envFile -Encoding UTF8 | Where-Object { $_ -match '^DEEPAI2API_PORT=\d+$' } | Select-Object -First 1
    if ($portSetting) { $baseUrl = 'http://127.0.0.1:' + $portSetting.Split('=')[1] }
}

function Get-GatewayProcess {
    Get-CimInstance Win32_Process -Filter "Name='deepaicode2api.exe'" -OperationTimeoutSec 5 |
        Where-Object { $_.ExecutablePath -eq $executable }
}

function Get-GatewayHealth {
    try {
        Invoke-RestMethod -Uri "$baseUrl/health" -TimeoutSec 2
    } catch {
        return $null
    }
}

function Get-SupervisorProcess {
    Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" -OperationTimeoutSec 5 |
        Where-Object {
            $_.CommandLine -match [regex]::Escape($scriptPath) -and
            $_.CommandLine -match '-Action\s+Run(?:\s|$)'
        }
}

function Stop-Gateway {
    Get-SupervisorProcess | ForEach-Object {
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
    }
    Get-GatewayProcess | ForEach-Object {
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
    }
}

function Start-Gateway {
    if (!(Get-SupervisorProcess)) {
        Start-Process -FilePath $powershellPath -ArgumentList $runArguments `
            -WorkingDirectory $projectRoot -WindowStyle Hidden | Out-Null
    }
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        $health = Get-GatewayHealth
        if ($health) {
            $health | ConvertTo-Json -Compress
            return
        }
        Start-Sleep -Milliseconds 500
    }
    throw "Gateway did not become ready. Check $dataDirectory\gateway.stderr.log"
}

switch ($Action) {
    { $_ -in @('Run','Foreground') } {
        if (!(Test-Path -LiteralPath $executable)) { throw 'Build bin\deepaicode2api.exe first.' }
        $envFile = Join-Path $projectRoot '.env'
        if (!(Test-Path -LiteralPath $envFile)) { throw 'Missing local .env file.' }
        foreach ($line in Get-Content -LiteralPath $envFile -Encoding UTF8) {
            if ($line -match '^\s*(?:#|$)') { continue }
            if ($line -notmatch '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$') {
                throw 'Invalid .env entry.'
            }
            $variableName = $Matches[1]
            $variableValue = $Matches[2].Trim()
            if ($variableValue.Length -ge 2 -and (
                ($variableValue.StartsWith('"') -and $variableValue.EndsWith('"')) -or
                ($variableValue.StartsWith("'") -and $variableValue.EndsWith("'"))
            )) { $variableValue = $variableValue.Substring(1, $variableValue.Length - 2) }
            [Environment]::SetEnvironmentVariable($variableName, $variableValue, 'Process')
        }
        Set-Location -LiteralPath $projectRoot
        New-Item -ItemType Directory -Path $dataDirectory -Force | Out-Null
        if ($Action -eq 'Foreground') {
            Write-Host 'Starting the gateway in this window. Press Ctrl+C to stop.'
            & $executable
            exit $LASTEXITCODE
        }
        $rootHash = [Security.Cryptography.SHA256]::Create()
        try {
            $mutexName = 'Local\DeepAICode2API-' + [BitConverter]::ToString(
                $rootHash.ComputeHash([Text.Encoding]::UTF8.GetBytes($projectRoot.ToLowerInvariant()))
            ).Replace('-', '')
        } finally { $rootHash.Dispose() }
        $mutex = New-Object Threading.Mutex($false, $mutexName)
        if (!$mutex.WaitOne(0)) { $mutex.Dispose(); exit 0 }
        try {
            while ($true) {
                Add-Content -LiteralPath (Join-Path $dataDirectory 'supervisor.log') `
                    -Value ((Get-Date -Format o) + ' Starting gateway.') -Encoding UTF8
                $process = Start-Process -FilePath $executable -WorkingDirectory $projectRoot `
                    -WindowStyle Hidden -PassThru `
                    -RedirectStandardOutput (Join-Path $dataDirectory 'gateway.stdout.log') `
                    -RedirectStandardError (Join-Path $dataDirectory 'gateway.stderr.log')
                $watchdogEnabled = $env:DEEPAI2API_WATCHDOG -match '^(1|true|yes)$'
                $healthFailures = 0
                while (!$process.WaitForExit(10000)) {
                    if (!$watchdogEnabled) { continue }
                    if (Get-GatewayHealth) { $healthFailures = 0; continue }
                    $healthFailures++
                    if ($healthFailures -ge 6) {
                        Add-Content -LiteralPath (Join-Path $dataDirectory 'supervisor.log') `
                            -Value ((Get-Date -Format o) + ' Repeated health failures; restarting gateway.') -Encoding UTF8
                        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
                        $process.WaitForExit()
                        break
                    }
                }
                Add-Content -LiteralPath (Join-Path $dataDirectory 'supervisor.log') `
                    -Value ((Get-Date -Format o) + ' Gateway exited; restarting in 10 seconds.') -Encoding UTF8
                Start-Sleep -Seconds 10
            }
        } finally {
            $mutex.ReleaseMutex()
            $mutex.Dispose()
        }
    }
    'Install' {
        if (!$startupShortcut) { throw 'No per-user Startup directory. Use windows-server.ps1 for SYSTEM boot startup.' }
        $shell = New-Object -ComObject WScript.Shell
        $shortcut = $shell.CreateShortcut($startupShortcut)
        $shortcut.TargetPath = $powershellPath
        $shortcut.Arguments = $runArguments
        $shortcut.WorkingDirectory = $projectRoot
        $shortcut.WindowStyle = 7
        $shortcut.Description = 'Local DeepAICode2API gateway; starts when this user signs in.'
        $shortcut.Save()
        Start-Gateway
    }
    'Start' { Start-Gateway }
    'Stop' { Stop-Gateway; Write-Output 'Gateway stopped.' }
    'Restart' { Stop-Gateway; Start-Gateway }
    'Status' {
        [PSCustomObject]@{
            Autostart = if ($startupShortcut) { Test-Path -LiteralPath $startupShortcut } else { $false }
            SupervisorIds = @(Get-SupervisorProcess | ForEach-Object { $_.ProcessId })
            ProcessIds = @(Get-GatewayProcess | ForEach-Object { $_.ProcessId })
            Health = Get-GatewayHealth
            Console = "$baseUrl/"
            Logs = $dataDirectory
        } | ConvertTo-Json -Depth 4
    }
}
