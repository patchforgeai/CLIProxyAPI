#requires -Version 5.1
<#
.SYNOPSIS
  Publishes the local CPA (http://127.0.0.1:8317) on the tailnet at <machine>.<tailnet>.ts.net:8318.

.DESCRIPTION
  Tries `tailscale serve --bg --https=8318 http://127.0.0.1:8317` first. HTTPS needs Tailscale
  Serve and HTTPS certificates enabled for the tailnet (a one-time admin step; tailscale prints
  the enable link). When that is not enabled, or with -Http, it serves plain HTTP on the
  tailnet instead (`--http=8318`). Plain HTTP is still tailnet-only and WireGuard-encrypted
  between devices; browsers just show "Not secure". Port 8318 keeps clear of T3 Code's own
  tailscale serve on 443. Never use Tailscale Funnel for CPA: it would expose it publicly.
  The serve config persists across reboots; remove it with `tailscale serve --https=8318 off`
  or `tailscale serve --http=8318 off`.

.PARAMETER Http
  Skip the HTTPS attempt and serve plain HTTP on the tailnet.
#>
[CmdletBinding()]
param([switch]$Http)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$ts = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'
if (-not (Test-Path -LiteralPath $ts)) {
  $command = Get-Command tailscale -ErrorAction SilentlyContinue
  if (-not $command) { throw 'tailscale.exe not found. Install Tailscale and log in.' }
  $ts = $command.Source
}

$statusJson = & $ts status --json | Out-String
if ($LASTEXITCODE -ne 0) { throw 'tailscale status failed. Is Tailscale running and logged in?' }
$status = $statusJson | ConvertFrom-Json

$dns = ''
if ($status.PSObject.Properties['Self'] -and $status.Self.PSObject.Properties['DNSName']) {
  $dns = ([string]$status.Self.DNSName).TrimEnd('.')
}
if (-not $dns) { throw 'tailscale status has no Self.DNSName. Turn on MagicDNS for the tailnet.' }

# Runs one tailscale serve command with a bounded wait. When Serve or HTTPS is not enabled for
# the tailnet, tailscale prints a one-time enable link and then waits until someone enables it;
# the bounded wait turns that into a reported result instead of a hang.
function Invoke-TailscaleServe([string]$Arguments) {
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $ts
  $psi.Arguments = $Arguments
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.CreateNoWindow = $true
  $proc = [System.Diagnostics.Process]::Start($psi)
  $stdoutTask = $proc.StandardOutput.ReadToEndAsync()
  $stderrTask = $proc.StandardError.ReadToEndAsync()
  if (-not $proc.WaitForExit(30000)) {
    try { $proc.Kill() } catch { }
    $proc.WaitForExit(5000) | Out-Null
  }
  $output = ($stdoutTask.Result + "`n" + $stderrTask.Result).Trim()
  $exitCode = if ($proc.HasExited) { $proc.ExitCode } else { 1 }
  return @{ Output = $output; ExitCode = $exitCode; NotEnabled = ($output -match 'not enabled|To enable') }
}

$scheme = 'https'
if (-not $Http) {
  $https = Invoke-TailscaleServe 'serve --bg --https=8318 http://127.0.0.1:8317'
  if ($https.NotEnabled) {
    Write-Host $https.Output
    Write-Warning ('Tailscale Serve/HTTPS is not enabled for this tailnet, so CPA is served over plain HTTP ' +
      'on the tailnet instead. To switch to HTTPS later, open the link above once (tailnet admin; it publishes ' +
      'this machine''s ts.net name in public Certificate Transparency logs), then run: ' +
      'tailscale serve --http=8318 off; devyre\scripts\tailscale-serve.ps1')
    $Http = $true
  } elseif ($https.ExitCode -ne 0) {
    Write-Host $https.Output
    throw 'tailscale serve --https failed (see the message above).'
  } else {
    if ($https.Output) { Write-Host $https.Output }
  }
}
if ($Http) {
  $scheme = 'http'
  $plain = Invoke-TailscaleServe 'serve --bg --http=8318 http://127.0.0.1:8317'
  if ($plain.Output) { Write-Host $plain.Output }
  if ($plain.NotEnabled -or $plain.ExitCode -ne 0) { throw 'tailscale serve --http failed (see the message above).' }
}

$serveStatus = (& $ts serve status 2>&1 | Out-String).Trim()
Write-Host $serveStatus
if ($serveStatus -match 'No serve config') { throw 'tailscale serve reported success but no serve config exists.' }

$base = "$($scheme)://$($dns):8318"
Write-Host ''
Write-Host "Panel:  $base/management.html   (from any device on your tailnet, e.g. your phone)"
Write-Host "T3 hub: $base   (on this PC, http://127.0.0.1:8317 also works)"
Write-Host "Next:   set CPA_PUBLIC_URL=$base in devyre\deploy\.env. Other PCs can use this base URL for"
Write-Host "        ANTHROPIC_BASE_URL; on this PC the profile keeps using http://127.0.0.1:8317 unless you run"
Write-Host "        devyre\scripts\new-secrets.ps1 -PublicUrl $base"
