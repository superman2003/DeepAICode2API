[CmdletBinding()]
param([Parameter(Mandatory=$true)][ValidatePattern('^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$')][string]$Version,[string]$OutputDirectory='')
$ErrorActionPreference='Stop'
$root=Split-Path -Parent $PSScriptRoot
if (!$OutputDirectory) { $OutputDirectory=Join-Path $root 'dist\release' }
$OutputDirectory=[IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
$stage=Join-Path $OutputDirectory ('.release-'+[guid]::NewGuid().ToString('N'))
$update=Join-Path $stage 'update'
$full=Join-Path $stage 'full'
New-Item -ItemType Directory -Path (Join-Path $update 'bin'),(Join-Path $full 'bin'),(Join-Path $full 'scripts') -Force | Out-Null
Push-Location $root
try {
    $env:CGO_ENABLED='0'
    $env:GOOS='windows'
    $env:GOARCH='amd64'
    go build -trimpath -ldflags ('-s -w -X main.version='+$Version) -o (Join-Path $update 'bin\deepaicode2api.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'Gateway release build failed.' }
    go build -trimpath -ldflags ('-s -w -X main.version='+$Version) -o (Join-Path $update 'bin\register.exe') ./cmd/register
    if ($LASTEXITCODE -ne 0) { throw 'Register release build failed.' }
    $files=@()
    foreach ($name in @('deepaicode2api.exe','register.exe')) {
        $path=Join-Path $update ('bin\'+$name)
        $files+=@{path=('bin/'+$name);sha256=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()}
        Copy-Item -LiteralPath $path -Destination (Join-Path $full 'bin')
    }
    $manifest=@{version=$Version;os='windows';arch='amd64';protocol=1;files=$files}
    [IO.File]::WriteAllText((Join-Path $update 'update-manifest.json'),($manifest|ConvertTo-Json -Depth 5),(New-Object Text.UTF8Encoding($false)))
    foreach ($directory in @('cmd','internal','docs','examples','packaging','.github')) { Copy-Item -LiteralPath (Join-Path $root $directory) -Destination $full -Recurse }
    foreach ($name in @('main.go','go.mod','go.sum','LICENSE','VERSION','.gitignore','run-windows.cmd')) { Copy-Item -LiteralPath (Join-Path $root $name) -Destination $full }
    Copy-Item -LiteralPath (Join-Path $root 'scripts\run-clean-windows.ps1'),(Join-Path $root 'scripts\web-update.ps1'),(Join-Path $root 'scripts\build-release.ps1') -Destination (Join-Path $full 'scripts')
    Copy-Item -LiteralPath (Join-Path $root 'packaging\windows.env.example') -Destination (Join-Path $full '.env.example')
    Copy-Item -LiteralPath (Join-Path $root 'packaging\README-windows.md') -Destination (Join-Path $full 'README.md')
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $assets=@()
    foreach ($item in @(@{Directory=$update;Name='DeepAICode2API_windows_amd64_update.zip'},@{Directory=$full;Name='DeepAICode2API_windows_amd64.zip'})) {
        $path=Join-Path $OutputDirectory $item.Name
        if (Test-Path -LiteralPath $path) { throw 'Release output already exists; select a fresh output directory.' }
        [IO.Compression.ZipFile]::CreateFromDirectory($item.Directory,$path,[IO.Compression.CompressionLevel]::Optimal,$false)
        $assets+=$path
    }
    $lines=@($assets|ForEach-Object{(Get-FileHash -LiteralPath $_ -Algorithm SHA256).Hash.ToLowerInvariant()+'  '+[IO.Path]::GetFileName($_)})
    [IO.File]::WriteAllText((Join-Path $OutputDirectory 'SHA256SUMS.txt'),(($lines -join "`n")+"`n"),(New-Object Text.UTF8Encoding($false)))
    [PSCustomObject]@{Version=$Version;Assets=$assets;Checksums=(Join-Path $OutputDirectory 'SHA256SUMS.txt')} | ConvertTo-Json -Depth 3
} finally {
    Pop-Location
    $target=[IO.Path]::GetFullPath($stage)
    if (!$target.StartsWith($OutputDirectory.TrimEnd('\')+'\',[StringComparison]::OrdinalIgnoreCase)) { throw 'Unsafe release staging cleanup path.' }
    Remove-Item -LiteralPath $stage -Recurse -Force
}
