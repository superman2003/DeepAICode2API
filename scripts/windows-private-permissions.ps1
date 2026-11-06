# Only directory ACLs receive inheritance flags. Files inherit ordinary access
# rights when their ACLs are reset beneath that directory.
function Repair-GatewayPermissions {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory=$true)][string]$ProjectRoot,
        [switch]$TakeOwnership
    )
    $root=[IO.Path]::GetFullPath($ProjectRoot).TrimEnd('\')
    $privateDirectory=Join-Path $root 'data'
    $configurationFile=Join-Path $root '.env'
    if (!(Test-Path -LiteralPath $privateDirectory -PathType Container)) { throw 'Missing data directory; fully extract the migration archive first.' }
    if (!(Test-Path -LiteralPath $configurationFile -PathType Leaf)) { throw 'Missing .env; preserve the original migration configuration.' }
    if (((Get-Item -LiteralPath $privateDirectory -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Refusing permission repair through a linked data directory.' }
    $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $aclTool=Join-Path $env:SystemRoot 'System32\icacls.exe'
    $ownerTool=Join-Path $env:SystemRoot 'System32\takeown.exe'
    if ($TakeOwnership) {
        & $ownerTool /F $privateDirectory /A /R /D Y | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not take ownership of private data. Run PowerShell as Administrator.' }
        & $ownerTool /F $configurationFile /A | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not take ownership of .env.' }
    }
    & $aclTool $privateDirectory /reset /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not reset private-directory permissions.' }
    & $aclTool $privateDirectory /inheritance:r /grant:r "*${sid}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not protect the private directory.' }
    foreach ($item in Get-ChildItem -LiteralPath $privateDirectory -Force) {
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Refusing permission repair through a linked data entry.' }
        # Do not use /grant with (OI)(CI) and /T: regular files must receive
        # inherited file permissions, rather than directory-only ACL entries.
        & $aclTool $item.FullName /reset /T /C /Q | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not restore inherited permissions on a private data entry.' }
    }
    & $aclTool $configurationFile /reset /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not reset .env permissions.' }
    & $aclTool $configurationFile /inheritance:r /grant:r "*${sid}:F" '*S-1-5-18:F' '*S-1-5-32-544:F' /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not protect .env.' }
}
