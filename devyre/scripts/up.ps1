#requires -Version 5.1
<#
.SYNOPSIS
  Builds the cpa-devyre image from this checkout and (re)starts the cpa container.

.DESCRIPTION
  Builds cpa-devyre:current with docker compose, also tags it cpa-devyre:<git sha> for
  rollback (cpa-devyre:<sha>-dirty when tracked files have local changes), runs
  docker compose up -d, then waits until the server answers /healthz and serves the
  management panel on http://127.0.0.1:8317.

.PARAMETER NoBuild
  Restart with the existing cpa-devyre:current image (after a rollback or a secret rotation).
#>
[CmdletBinding()]
param([switch]$NoBuild)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$repo    = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$compose = Join-Path $repo 'devyre\deploy\docker-compose.yml'
$envFile = Join-Path $repo 'devyre\deploy\.env'
if (-not (Test-Path -LiteralPath $envFile)) {
  throw "Missing $envFile. Copy devyre\deploy\.env.example to .env and edit it."
}
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw 'docker is not on PATH. Start Docker Desktop.' }

$imageTag = 'current'
if (-not $NoBuild) {
  $sha = & git -C $repo rev-parse --short HEAD
  if ($LASTEXITCODE -ne 0 -or -not $sha) { throw 'git rev-parse failed; run up.ps1 from a git checkout.' }
  $sha = "$sha".Trim()
  $dirty = & git -C $repo status --porcelain --untracked-files=no
  if ($LASTEXITCODE -ne 0) { throw 'git status failed.' }
  $imageTag = if ($dirty) { "$sha-dirty" } else { $sha }
  $version = & git -C $repo describe --tags --always --dirty
  if ($LASTEXITCODE -ne 0) { throw 'git describe failed.' }
  $env:CPA_VERSION    = "$version".Trim()
  $env:CPA_COMMIT     = $sha
  $env:CPA_BUILD_DATE = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

  & docker compose -f $compose --env-file $envFile build
  if ($LASTEXITCODE -ne 0) { throw 'docker compose build failed.' }
  & docker tag cpa-devyre:current "cpa-devyre:$imageTag"
  if ($LASTEXITCODE -ne 0) { throw "docker tag cpa-devyre:$imageTag failed." }
}

& docker compose -f $compose --env-file $envFile up -d
if ($LASTEXITCODE -ne 0) { throw 'docker compose up failed.' }

function Test-CpaUrl([string]$Url) {
  try {
    $response = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 10
    return ($response.StatusCode -ge 200 -and $response.StatusCode -lt 300)
  } catch {
    return $false
  }
}

$deadline = (Get-Date).AddSeconds(60)
while (-not (Test-CpaUrl 'http://127.0.0.1:8317/healthz')) {
  if ((Get-Date) -gt $deadline) {
    & docker logs --tail 80 cpa
    throw 'CPA did not answer http://127.0.0.1:8317/healthz within 60s (logs above).'
  }
  Start-Sleep -Seconds 2
}

# The first request downloads the panel from the Devyre panel fork's latest release.
$deadline = (Get-Date).AddSeconds(90)
while (-not (Test-CpaUrl 'http://127.0.0.1:8317/management.html')) {
  if ((Get-Date) -gt $deadline) {
    & docker logs --tail 80 cpa
    throw ('CPA is up but did not serve management.html within 90s (logs above). Check ' +
      'management.panel-github-repository and that the panel fork has a release with management.html.')
  }
  Start-Sleep -Seconds 3
}

$publicUrl = $null
foreach ($line in Get-Content -LiteralPath $envFile) {
  if ($line -match '^\s*CPA_PUBLIC_URL\s*=\s*(\S+)\s*$') { $publicUrl = $Matches[1].Trim('"', "'").TrimEnd('/') }
}
Write-Host "CPA is up (image cpa-devyre:$imageTag)."
Write-Host 'Local:   http://127.0.0.1:8317/management.html'
if ($publicUrl) { Write-Host "Tailnet: $publicUrl/management.html" }
