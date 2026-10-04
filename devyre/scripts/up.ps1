#requires -Version 5.1
<#
.SYNOPSIS
  Builds the cpa-devyre image from this checkout and (re)starts the cpa container.

.DESCRIPTION
  Builds cpa-devyre:current with docker compose, also tags it cpa-devyre:<git sha> for
  rollback (cpa-devyre:<sha>-dirty when tracked files have local changes), recreates the cpa
  container with docker compose up -d --force-recreate, then waits until the server answers
  /healthz and serves the management panel on http://127.0.0.1:8317.

  The container is always recreated, even when the image and compose file are unchanged:
  the running server does not see host-side edits to config.yaml or auths\ (Docker Desktop
  delivers no file events for bind mounts), so a restart is how such edits take effect.

  Every published port must be bound to 127.0.0.1: tailscale serve is the only way in from the
  tailnet, and passwordless access (management.tailnet-auth) trusts whatever reaches the
  container through the host's loopback. The script refuses to start when the compose file
  publishes a port anywhere else, and stops the container when the running container does.

.PARAMETER NoBuild
  Restart with the existing cpa-devyre:current image (after a rollback, or to apply host-side
  edits to config.yaml).
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
. (Join-Path $PSScriptRoot 'cpa-common.ps1')
$docker = Get-CpaDockerExe

# Refuse to start anything that publishes CPA beyond this PC's loopback.
$composeProblems = @(Get-CpaComposePublishProblems -Docker $docker -Compose $compose -EnvFile $envFile)
if ($composeProblems.Count -gt 0) {
  throw ("Refusing to start: $compose publishes ports beyond 127.0.0.1:`n  " + ($composeProblems -join "`n  ") +
    "`nBind every port to 127.0.0.1 (for example ""127.0.0.1:8317:8317""). tailscale serve is the only way in from the tailnet, " +
    'and passwordless access trusts whatever reaches the container through loopback.')
}

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

# --force-recreate: compose leaves a running container alone when nothing in its definition
# changed, and the server would keep the config it loaded at start.
& docker compose -f $compose --env-file $envFile up -d --force-recreate
if ($LASTEXITCODE -ne 0) { throw 'docker compose up failed.' }

# The running container is what counts: stop it when any published port is not on 127.0.0.1.
$info = Get-CpaContainerInfo -Docker $docker -Name 'cpa'
if ($null -eq $info) { throw 'docker compose up reported success, but there is no container named cpa.' }
$publish = Get-CpaPublishReport $info
if (@($publish.Problems).Count -gt 0) {
  & docker stop cpa | Out-Null
  throw ("Stopped cpa: it publishes beyond 127.0.0.1: " + (@($publish.Problems) -join '; ') +
    '. Bind every port to 127.0.0.1 in devyre\deploy\docker-compose.yml, then run up.ps1 again.')
}

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
