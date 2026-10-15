#requires -Version 5.1
<#
.SYNOPSIS
  Makes sure CPA is up: Docker Desktop -> cpa container -> local health -> tailnet URL.

.DESCRIPTION
  Safe to run at any time and as often as you like (the autostart task runs it at sign-in
  and every 5 minutes). Each step only acts when something is down:
    1. Docker engine unreachable: start Docker Desktop and wait for the engine (up to 5 min).
    2. cpa not running or http://127.0.0.1:8317/healthz not 200: docker compose up -d
       (builds the image only when it does not exist yet), then wait for health.
    3. http://<this PC's MagicDNS name>:8318/healthz not 200: run tailscale-serve.ps1 -Http to
       repair the serve entry (for example after a tailnet rename). At most once per hour.
  Log: %USERPROFILE%\.cli-proxy-api\logs\ensure-running.log (kept under about 1 MB).
  Exit code 0 when CPA answers locally, 1 otherwise.

.PARAMETER Quiet
  Log only; do not echo progress to the console (the scheduled task uses this).
#>
[CmdletBinding()]
param([switch]$Quiet)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$repo     = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$compose  = Join-Path $repo 'devyre\deploy\docker-compose.yml'
$envFile  = Join-Path $repo 'devyre\deploy\.env'
$cpaHome  = Join-Path $env:USERPROFILE '.cli-proxy-api'
$logDir   = Join-Path $cpaHome 'logs'
$logFile  = Join-Path $logDir 'ensure-running.log'
$stampFile = Join-Path $logDir 'ensure-running.serve-repair'
$dockerDesktop = Join-Path $env:ProgramFiles 'Docker\Docker\Docker Desktop.exe'
$tailscale = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'

. (Join-Path $PSScriptRoot 'cpa-common.ps1')

if (-not (Test-Path -LiteralPath $logDir)) { New-Item -ItemType Directory -Path $logDir -Force | Out-Null }
if ((Test-Path -LiteralPath $logFile) -and (Get-Item -LiteralPath $logFile).Length -gt 1MB) {
  Move-Item -LiteralPath $logFile -Destination "$logFile.1" -Force
}

function Write-Log([string]$Message) {
  $line = '{0} {1}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $Message
  Add-Content -LiteralPath $logFile -Value $line -Encoding ascii
  if (-not $Quiet) { Write-Host $line }
}

function Test-Http200([string]$Url, [int]$TimeoutSec = 5) {
  try {
    $r = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec $TimeoutSec
    return ($r.StatusCode -eq 200)
  } catch { return $false }
}

# Windows PowerShell 5.1 turns a native command's stderr into a terminating error under
# ErrorActionPreference Stop, even when redirected; native calls run with Continue instead.
function Invoke-NativeQuiet([scriptblock]$Block) {
  $ErrorActionPreference = 'Continue'
  $out = & $Block 2>&1 | Out-String
  return @{ Output = $out.Trim(); ExitCode = $LASTEXITCODE }
}

function Test-DockerEngine {
  $r = Invoke-NativeQuiet { docker info --format '{{.ServerVersion}}' }
  return ($r.ExitCode -eq 0 -and $r.Output -ne '' -and $r.Output -notmatch 'error|failed')
}

# One run at a time: the task and a manual start must not overlap.
$mutex = New-Object System.Threading.Mutex($false, 'Local\DevyreCpaEnsureRunning')
if (-not $mutex.WaitOne(0)) { if (-not $Quiet) { Write-Host 'Another ensure-running is already working; exiting.' }; exit 0 }
try {
  # Fast path: everything already fine.
  $localOk = Test-Http200 'http://127.0.0.1:8317/healthz'

  if (-not $localOk) {
    # 1. Docker engine
    if (-not (Test-DockerEngine)) {
      if (-not (Get-Process 'Docker Desktop' -ErrorAction SilentlyContinue)) {
        if (-not (Test-Path -LiteralPath $dockerDesktop)) { Write-Log "FAIL Docker Desktop not found at $dockerDesktop"; exit 1 }
        Write-Log 'Docker engine down; starting Docker Desktop.'
        Start-Process -FilePath $dockerDesktop
      } else {
        Write-Log 'Docker Desktop is running but the engine is not ready yet; waiting.'
      }
      $deadline = (Get-Date).AddMinutes(5)
      while (-not (Test-DockerEngine)) {
        if ((Get-Date) -gt $deadline) { Write-Log 'FAIL Docker engine did not come up within 5 minutes.'; exit 1 }
        Start-Sleep -Seconds 5
      }
      Write-Log 'Docker engine is up.'
    }

    # 2. cpa container
    if (-not (Test-Path -LiteralPath $envFile)) { Write-Log "FAIL missing $envFile (copy .env.example)"; exit 1 }
    $docker = Get-CpaDockerExe
    if (-not $docker) { $docker = Join-Path $env:ProgramFiles 'Docker\Docker\resources\bin\docker.exe' }
    Write-Log 'cpa is not answering; running docker compose up -d.'
    $r = Invoke-CpaComposeUp -Docker $docker -Compose $compose -EnvFile $envFile
    if ($r.ExitCode -ne 0) { Write-Log ("FAIL docker compose up: " + $r.Output); exit 1 }
    if ($r.OAuthError) { Write-Log ("WARN started without the OAuth callback ports (Windows reserved them): " + $r.OAuthError) }
    $deadline = (Get-Date).AddSeconds(30)
    while (-not ($localOk = Test-Http200 'http://127.0.0.1:8317/healthz')) {
      if ((Get-Date) -gt $deadline) { break }
      Start-Sleep -Seconds 2
    }
    # A container can show "Up" with no port published, for example after Docker Desktop's
    # restart policy started it at boot and a publish failed; "up -d" leaves such a container
    # alone. Recreating it binds the ports again.
    if (-not $localOk) {
      Write-Log 'cpa is running but not reachable (ports not published?); recreating the container.'
      $r = Invoke-CpaComposeUp -Docker $docker -Compose $compose -EnvFile $envFile -UpArguments @('--force-recreate')
      if ($r.ExitCode -ne 0) { Write-Log ("FAIL docker compose up --force-recreate: " + $r.Output); exit 1 }
      if ($r.OAuthError) { Write-Log ("WARN started without the OAuth callback ports (Windows reserved them): " + $r.OAuthError) }
      $deadline = (Get-Date).AddSeconds(120)
      while (-not ($localOk = Test-Http200 'http://127.0.0.1:8317/healthz')) {
        if ((Get-Date) -gt $deadline) { Write-Log 'FAIL cpa did not answer /healthz within 120s (see docker logs cpa).'; exit 1 }
        Start-Sleep -Seconds 2
      }
    }
    Write-Log 'cpa is up: http://127.0.0.1:8317/healthz is 200.'
  }

  # 3. tailnet URL
  if (Test-Path -LiteralPath $tailscale) {
    $fqdn = ''
    try {
      $status = (Invoke-NativeQuiet { & $tailscale status --json }).Output | ConvertFrom-Json
      if ($status.BackendState -eq 'Running') { $fqdn = ([string]$status.Self.DNSName).TrimEnd('.') }
    } catch { $fqdn = '' }
    if (-not $fqdn) {
      Write-Log 'Tailscale is not running or not logged in; skipping the tailnet check.'
    } elseif (-not (Test-Http200 "http://$($fqdn):8318/healthz")) {
      $recent = (Test-Path -LiteralPath $stampFile) -and ((Get-Date) - (Get-Item -LiteralPath $stampFile).LastWriteTime).TotalMinutes -lt 60
      if ($recent) {
        Write-Log "WARN http://$($fqdn):8318 is not answering; a serve repair ran within the last hour, not retrying yet."
      } else {
        Set-Content -LiteralPath $stampFile -Value (Get-Date -Format o) -Encoding ascii
        Write-Log "http://$($fqdn):8318 is not answering; repairing tailscale serve."
        $repairScript = Join-Path $PSScriptRoot 'tailscale-serve.ps1'
        $repairOut = (Invoke-NativeQuiet { powershell.exe -NoProfile -ExecutionPolicy Bypass -File $repairScript -Http }).Output
        if (Test-Http200 "http://$($fqdn):8318/healthz") {
          Write-Log "Tailnet URL repaired: http://$($fqdn):8318/management.html"
        } else {
          Write-Log ("WARN tailnet URL still not answering after repair: " + (($repairOut -split "`n") | Select-Object -Last 3) -join ' ')
        }
      }
    } elseif (-not $Quiet) {
      Write-Log "Tailnet URL OK: http://$($fqdn):8318/management.html"
    }
  }
  exit 0
} finally {
  $mutex.ReleaseMutex()
  $mutex.Dispose()
}
