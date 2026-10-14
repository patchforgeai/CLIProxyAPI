#requires -Version 5.1
<#
.SYNOPSIS
  Refreshes the management panel built into the server (internal\managementasset\devyre_panel).

.DESCRIPTION
  The server embeds devyre_panel\management.html when it is built and never downloads a panel
  (devyre\README.md, "Embedded panel"). This script replaces that file and its record,
  devyre_panel\panel.json (version, source, sha256), from one of two sources:
    - A release of the panel fork (the default). gh release download fetches management.html
      with your own gh login, and the file must match the sha256 digest GitHub lists for it.
    - -Clone <path>: a local clone of the panel fork with no uncommitted changes, built the way
      its release workflow builds it: bun install --frozen-lockfile, then bun run build with
      VERSION set to the clone's git describe, which must reach a v*-devyre.* tag.
  It changes nothing else and never commits. Afterwards run the checks it prints, review the
  diff, commit, and rebuild (up.ps1). To try a panel build without vendoring it, point
  MANAGEMENT_STATIC_PATH at it instead; the server serves that file while it exists.

.PARAMETER Tag
  Release tag, such as v1.25.3-devyre.2. Default: the latest release.

.PARAMETER Repository
  The panel fork on GitHub, owner/name. Default: Devyre/Cli-Proxy-API-Management-Center.

.PARAMETER Clone
  Path to a local clone of the panel fork to build instead of downloading a release.

.EXAMPLE
  devyre\scripts\update-panel.ps1

.EXAMPLE
  devyre\scripts\update-panel.ps1 -Tag v1.25.3-devyre.2

.EXAMPLE
  devyre\scripts\update-panel.ps1 -Clone ..\Cli-Proxy-API-Management-Center
#>
[CmdletBinding(DefaultParameterSetName = 'Release')]
param(
  [Parameter(ParameterSetName = 'Release')][string]$Tag,
  [Parameter(ParameterSetName = 'Release')][string]$Repository = 'Devyre/Cli-Proxy-API-Management-Center',
  [Parameter(ParameterSetName = 'Clone', Mandatory = $true)][string]$Clone
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0
. (Join-Path $PSScriptRoot 'cpa-common.ps1')

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$panelDir = Join-Path $repo 'internal\managementasset\devyre_panel'
$panelFile = Join-Path $panelDir 'management.html'
$recordFile = Join-Path $panelDir 'panel.json'
$assetName = 'management.html'
# The server's own size limit for a panel file (maxAssetDownloadSize in updater.go).
$maxBytes = 50MB

if (-not (Test-Path -LiteralPath $panelDir -PathType Container)) {
  throw "Not found: $panelDir. Run this script from the server fork's checkout."
}

function Get-NativeTool([string]$Name, [string]$Hint) {
  $command = Get-Command $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
  if (-not $command) { throw "$Name is not on PATH. $Hint" }
  return $command.Source
}

function Get-FirstLine([string]$Text) {
  $lines = @($Text -split "`r?`n" | Where-Object { $_.Trim() })
  if ($lines.Count -eq 0) { return '(no output)' }
  return $lines[0].Trim()
}

function Get-Sha256Hex([string]$Path) {
  return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

# A panel build is one self-contained HTML page within the server's size limit.
function Assert-PanelPage([string]$Path) {
  $length = (Get-Item -LiteralPath $Path).Length
  if ($length -le 0 -or $length -gt $maxBytes) { throw "$Path is $length bytes; a panel build is 1 byte to 50 MB." }
  $stream = [System.IO.File]::OpenRead($Path)
  try {
    $buffer = New-Object byte[] 15
    $read = $stream.Read($buffer, 0, $buffer.Length)
  } finally {
    $stream.Dispose()
  }
  $head = [System.Text.Encoding]::ASCII.GetString($buffer, 0, $read)
  if ($head -ne '<!doctype html>') { throw "$Path is not a panel build: it does not start with <!doctype html>." }
}

function ConvertTo-JsonString([string]$Value) {
  return '"' + $Value.Replace('\', '\\').Replace('"', '\"') + '"'
}

function Get-PanelRecord {
  if (-not (Test-Path -LiteralPath $recordFile)) { return $null }
  try { return (ConvertFrom-Json -InputObject ([IO.File]::ReadAllText($recordFile))) } catch { return $null }
}

$work = Join-Path ([System.IO.Path]::GetTempPath()) ('cpa-panel-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
try {
  if ($PSCmdlet.ParameterSetName -eq 'Release') {
    if ($Repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw "-Repository must be owner/name, not '$Repository'." }
    if ($Tag -and $Tag -notmatch '^[A-Za-z0-9_.+-]{1,128}$') { throw "'$Tag' is not a release tag." }
    $gh = Get-NativeTool 'gh' 'Install GitHub CLI and run gh auth login with an account that can read the panel fork.'
    $endpoint = "repos/$Repository/releases/latest"
    if ($Tag) { $endpoint = "repos/$Repository/releases/tags/$Tag" }
    $view = Invoke-CpaNative -FilePath $gh -Arguments @('api', $endpoint) -TimeoutSec 60
    if ($view.TimedOut -or $view.ExitCode -ne 0) {
      throw ("gh api $endpoint failed: $(Get-FirstLine $view.StdErr) " +
        "Check gh auth status: your login must be able to read $Repository.")
    }
    $release = ConvertFrom-CpaJsonText $view.StdOut "The release of $Repository"
    $version = [string](Get-CpaProperty $release 'tag_name')
    if ($version -notmatch '^v[A-Za-z0-9_.+-]{1,127}$') { throw "The release has an unexpected tag name '$version'." }
    $assets = Get-CpaProperty $release 'assets'
    $asset = @($assets | Where-Object { $null -ne $_ -and [string](Get-CpaProperty $_ 'name') -eq $assetName })
    if ($asset.Count -ne 1) { throw "Release $version of $Repository has no $assetName asset." }
    $digest = [string](Get-CpaProperty $asset[0] 'digest')
    if ($digest -notmatch '^sha256:([0-9a-fA-F]{64})$') {
      throw "Release $version lists no sha256 digest for $assetName, so the download could not be verified."
    }
    $wantSha = $Matches[1].ToLowerInvariant()

    $built = Join-Path $work $assetName
    $download = Invoke-CpaNative -FilePath $gh -TimeoutSec 600 -Arguments @(
      'release', 'download', $version, '--repo', $Repository, '--pattern', $assetName, '--output', $built)
    if ($download.TimedOut -or $download.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $built)) {
      throw "gh release download $version failed: $(Get-FirstLine $download.StdErr)"
    }
    $gotSha = Get-Sha256Hex $built
    if ($gotSha -ne $wantSha) { throw "The downloaded $assetName hashes to $gotSha, but release $version lists $wantSha. Nothing was changed." }
    $source = "https://github.com/$Repository/releases/tag/$version"
  } else {
    $git = Get-NativeTool 'git' 'Install Git for Windows.'
    $bun = Get-NativeTool 'bun' 'Install Bun (the panel pins bun@1.3.14).'
    if (-not (Test-Path -LiteralPath $Clone -PathType Container)) { throw "Not a folder: $Clone" }
    $cloneDir = (Resolve-Path -LiteralPath $Clone).ProviderPath
    foreach ($name in 'package.json', 'bun.lock', 'vite.config.ts') {
      if (-not (Test-Path -LiteralPath (Join-Path $cloneDir $name))) { throw "$cloneDir is not a clone of the panel fork: $name is missing." }
    }
    $status = Invoke-CpaNative -FilePath $git -Arguments @('-C', $cloneDir, 'status', '--porcelain') -TimeoutSec 60
    if ($status.ExitCode -ne 0) { throw "git status failed in ${cloneDir}: $(Get-FirstLine $status.StdErr)" }
    if ($status.StdOut.Trim()) {
      throw "$cloneDir has uncommitted changes. Commit them first, so the embedded panel names a commit; to try a build as is, use MANAGEMENT_STATIC_PATH."
    }
    $describe = Invoke-CpaNative -FilePath $git -Arguments @('-C', $cloneDir, 'describe', '--tags', '--match', 'v*-devyre.*') -TimeoutSec 60
    if ($describe.ExitCode -ne 0) { throw "No v*-devyre.* tag is reachable from HEAD in ${cloneDir}: $(Get-FirstLine $describe.StdErr)" }
    $version = $describe.StdOut.Trim()
    $revParse = Invoke-CpaNative -FilePath $git -Arguments @('-C', $cloneDir, 'rev-parse', 'HEAD') -TimeoutSec 60
    $commit = $revParse.StdOut.Trim()
    if ($revParse.ExitCode -ne 0 -or $commit -notmatch '^[0-9a-f]{40}$') { throw "git rev-parse HEAD failed in $cloneDir." }

    $output = Join-Path $cloneDir 'dist\index.html'
    $started = (Get-Date).ToUniversalTime().AddSeconds(-2)
    $previousVersion = $env:VERSION
    Push-Location -LiteralPath $cloneDir
    try {
      Write-Host "Building the panel $version in $cloneDir ..."
      & $bun install --frozen-lockfile
      if ($LASTEXITCODE -ne 0) { throw "bun install --frozen-lockfile failed (exit $LASTEXITCODE)." }
      $env:VERSION = $version
      & $bun run build
      if ($LASTEXITCODE -ne 0) { throw "bun run build failed (exit $LASTEXITCODE)." }
    } finally {
      $env:VERSION = $previousVersion
      Pop-Location
    }
    if (-not (Test-Path -LiteralPath $output) -or (Get-Item -LiteralPath $output).LastWriteTimeUtc -lt $started) {
      throw "bun run build did not write $output."
    }
    $built = Join-Path $work $assetName
    Copy-Item -LiteralPath $output -Destination $built
    $source = "local build of $commit"
  }

  Assert-PanelPage $built
  $sha = Get-Sha256Hex $built
  $previous = Get-PanelRecord
  $previousSize = 0
  if (Test-Path -LiteralPath $panelFile) { $previousSize = (Get-Item -LiteralPath $panelFile).Length }

  # Replace the file through a rename on the same volume, so it is never half written.
  $staged = Join-Path $panelDir ('.management-' + [guid]::NewGuid().ToString('N') + '.tmp')
  try {
    Copy-Item -LiteralPath $built -Destination $staged
    if (Test-Path -LiteralPath $panelFile) {
      # [NullString]: a plain $null would reach .NET as an empty backup path.
      [System.IO.File]::Replace($staged, $panelFile, [NullString]::Value)
    } else {
      [System.IO.File]::Move($staged, $panelFile)
    }
  } finally {
    Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
  }
  $record = "{`n" +
    "  `"version`": $(ConvertTo-JsonString $version),`n" +
    "  `"source`": $(ConvertTo-JsonString $source),`n" +
    "  `"sha256`": $(ConvertTo-JsonString $sha)`n" +
    "}`n"
  [System.IO.File]::WriteAllText($recordFile, $record, (New-Object System.Text.UTF8Encoding $false))

  $size = (Get-Item -LiteralPath $panelFile).Length
  if ($previous -and [string](Get-CpaProperty $previous 'sha256') -eq $sha) {
    Write-Host "The embedded panel is already this build: $version (sha256 $sha, $size bytes)."
  } else {
    $was = 'none'
    if ($previous) { $was = "$([string](Get-CpaProperty $previous 'version')), $previousSize bytes" }
    Write-Host "Embedded panel: $version (was $was)"
  }
  Write-Host "  source  $source"
  Write-Host "  sha256  $sha"
  Write-Host "  size    $size bytes"
  Write-Host ''
  Write-Host 'Next, from the repository root:'
  Write-Host '  go test ./internal/managementasset/ ./internal/api/ -run DevyrePanel'
  Write-Host '  git add internal/managementasset/devyre_panel'
  Write-Host "  git commit -m `"feat(devyre): embed panel $version`""
  Write-Host '  devyre\scripts\up.ps1'
} finally {
  Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
