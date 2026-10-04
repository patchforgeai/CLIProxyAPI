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

# When Serve or HTTPS is not enabled for the tailnet, tailscale serve prints a one-time enable
# link and then waits until someone enables it. Run it with a bounded wait so that case is
# reported instead of hanging this script.
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $ts
$psi.Arguments = 'serve --bg --https=8318 http://127.0.0.1:8317'
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
$serveOutput = ($stdoutTask.Result + "`n" + $stderrTask.Result).Trim()
$serveExit = if ($proc.HasExited) { $proc.ExitCode } else { 1 }
if ($serveOutput) { Write-Host $serveOutput }
if ($serveOutput -match 'not enabled|To enable') {
  throw 'Tailscale Serve/HTTPS is not enabled for this tailnet. Open the link above once (tailnet admin), then rerun this script. Note: enabling HTTPS certificates publishes this machine''s ts.net name in public Certificate Transparency logs.'
}
if ($serveExit -ne 0) { throw 'tailscale serve failed (see the message above).' }
$serveStatus = (& $ts serve status 2>&1 | Out-String).Trim()
Write-Host $serveStatus
if ($serveStatus -match 'No serve config') { throw 'tailscale serve reported success but no serve config exists.' }

$base = "https://$($dns):8318"
Write-Host ''
Write-Host "Panel:  $base/management.html"
Write-Host "T3 hub: $base"
Write-Host "Next:   set CPA_PUBLIC_URL=$base in devyre\deploy\.env, then run"
Write-Host "        devyre\scripts\new-secrets.ps1 -PublicUrl $base   (base URL for the PowerShell profile)"
