#requires -Version 5.1
<#
.SYNOPSIS
  Archives config.yaml, auths\ and secrets\ from %USERPROFILE%\.cli-proxy-api.

.DESCRIPTION
  Writes cpa-backup-<yyyyMMdd-HHmmss>.zip into -Destination (created if missing). The archive
  holds live OAuth tokens and keys: keep it on BitLocker-protected storage or encrypt it, for
  example with 7-Zip AES. Refuses destinations inside this git checkout or inside the CPA home.

.PARAMETER Destination
  Folder that receives the archive.
#>
[CmdletBinding()]
param([Parameter(Mandatory = $true)][string]$Destination)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$cpaHome = Join-Path $env:USERPROFILE '.cli-proxy-api'
$items = @('config.yaml', 'auths', 'secrets' | ForEach-Object { Join-Path $cpaHome $_ })
foreach ($item in $items) {
  if (-not (Test-Path -LiteralPath $item)) { throw "Missing $item. Run devyre\scripts\new-secrets.ps1 first." }
}

if (-not (Test-Path -LiteralPath $Destination)) { New-Item -ItemType Directory -Path $Destination -Force | Out-Null }
$target = (Resolve-Path -LiteralPath $Destination).ProviderPath.TrimEnd('\')
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).ProviderPath.TrimEnd('\')
foreach ($forbidden in $repo, $cpaHome.TrimEnd('\')) {
  if ($target -eq $forbidden -or $target.StartsWith($forbidden + '\', [StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to write a token archive inside $forbidden."
  }
}

$archive = Join-Path $target "cpa-backup-$(Get-Date -Format 'yyyyMMdd-HHmmss').zip"
Compress-Archive -LiteralPath $items -DestinationPath $archive
Write-Host "Wrote $archive"
Write-Warning "$archive contains live OAuth tokens and keys. Keep it on encrypted storage (BitLocker or 7-Zip AES)."
