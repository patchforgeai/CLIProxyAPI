#requires -Version 5.1
<#
.SYNOPSIS
  Publishes the local CPA (http://127.0.0.1:8317) on the tailnet at https://<machine>.<tailnet>.ts.net:8318.

.DESCRIPTION
  Runs `tailscale serve --bg --https=8318 http://127.0.0.1:8317`. The tailnet needs MagicDNS
  and HTTPS certificates (admin console -> DNS). Port 8318 keeps clear of T3 Code's own
  tailscale serve on 443. Never use Tailscale Funnel for CPA: it would expose it publicly.
#>
[CmdletBinding()]
param()
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
if (-not ($status.PSObject.Properties['CertDomains'] -and $status.CertDomains)) {
  throw 'HTTPS certificates are not enabled for this tailnet (CertDomains is empty). Enable them in the admin console -> DNS, then rerun.'
}

& $ts serve --bg --https=8318 http://127.0.0.1:8317
if ($LASTEXITCODE -ne 0) { throw 'tailscale serve failed (see the message above).' }
& $ts serve status

$base = "https://$($dns):8318"
Write-Host ''
Write-Host "Panel:  $base/management.html"
Write-Host "T3 hub: $base"
Write-Host "Next:   set CPA_PUBLIC_URL=$base in devyre\deploy\.env, then run"
Write-Host "        devyre\scripts\new-secrets.ps1 -PublicUrl $base   (base URL for the PowerShell profile)"
