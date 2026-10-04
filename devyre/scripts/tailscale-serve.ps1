#requires -Version 5.1
<#
.SYNOPSIS
  Publishes the local CPA (http://127.0.0.1:8317) on the tailnet at <machine>.<tailnet>.ts.net:8318
  and checks that the tailnet URL answers.

.DESCRIPTION
  Runs `tailscale serve --bg --https=8318 http://127.0.0.1:8317`. HTTPS needs Tailscale Serve and
  HTTPS certificates enabled for the tailnet (a one-time admin step; tailscale prints the enable
  link). When that is not enabled, or with -Http, it serves plain HTTP on the tailnet instead
  (`--http=8318`). When port 8318 already serves plain HTTP, the script keeps HTTP. Plain HTTP is
  still tailnet-only and WireGuard-encrypted between devices; browsers just show "Not secure".
  Port 8318 keeps clear of T3 Code's own tailscale serve on 443. The script never enables
  Tailscale Funnel: that would make CPA public.

  Afterwards it requests <scheme>://<this PC's current MagicDNS name>:8318/healthz. A 404 from
  tailscaled after a tailnet rename means the serve entry is keyed to the old name: plain-HTTP
  serve takes the name from the stored login profile, which refreshes only on a real prefs edit.
  The script then refreshes the profile by setting a temporary nickname and restoring the
  original one (`tailscale set --nickname`), and, when entries keyed to an old name remain and
  every serve entry belongs to port 8318, resets the serve config and adds CPA again. With other
  serve entries (T3 Code's on 443, for example) it prints the manual steps instead. Run
  devyre\scripts\tailnet-trust.ps1 after a rename too: allowed-hosts holds the old name.

  The serve config persists across reboots; remove it with `tailscale serve --https=8318 off`
  or `tailscale serve --http=8318 off`.

.PARAMETER Http
  Skip the HTTPS attempt and serve plain HTTP on the tailnet.

.PARAMETER ShowOnly
  Show the current serve state, whether the tailnet URL answers and what a real run would do,
  without changing anything. -WhatIf does the same.

.PARAMETER TailscaleExe
  Path to tailscale.exe when it is not in the default location.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
  [switch]$Http,
  [switch]$ShowOnly,
  [string]$TailscaleExe
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0
. (Join-Path $PSScriptRoot 'cpa-common.ps1')
$preview = [bool]($ShowOnly -or $WhatIfPreference)

$ts = Get-CpaTailscaleExe $TailscaleExe
$self = Get-CpaTailnetSelf (Get-CpaTailscaleStatus -TailscaleExe $ts)
$fqdn = $self.Fqdn
$port = $script:CpaServePort
$target = $script:CpaServeTarget

# One tailscale serve command with a bounded wait. When Serve or HTTPS is not enabled for the
# tailnet, tailscale prints a one-time enable link and then waits until someone enables it; the
# bounded wait turns that into a reported result instead of a hang.
function Invoke-TailscaleServe([string[]]$Arguments) {
  $result = Invoke-CpaNative -FilePath $ts -Arguments $Arguments -TimeoutSec 30
  $output = ($result.StdOut + "`n" + $result.StdErr).Trim()
  return @{ Output = $output; ExitCode = $result.ExitCode; NotEnabled = ($output -match 'not enabled|To enable') }
}

# Serves CPA on port 8318 and returns the scheme in use.
function Set-CpaServe([bool]$PlainHttp) {
  if (-not $PlainHttp) {
    $https = Invoke-TailscaleServe @('serve', '--bg', "--https=$port", $target)
    if ($https.NotEnabled) {
      Write-Host $https.Output
      Write-Warning ('Tailscale Serve/HTTPS is not enabled for this tailnet, so CPA is served over plain HTTP ' +
        'on the tailnet instead. To switch to HTTPS later, open the link above once (tailnet admin; it publishes ' +
        'this machine''s ts.net name in public Certificate Transparency logs), then run: ' +
        "tailscale serve --http=$port off; devyre\scripts\tailscale-serve.ps1")
    } elseif ($https.ExitCode -ne 0) {
      Write-Host $https.Output
      throw 'tailscale serve --https failed (see the message above).'
    } else {
      if ($https.Output) { Write-Host $https.Output }
      return 'https'
    }
  }
  $plain = Invoke-TailscaleServe @('serve', '--bg', "--http=$port", $target)
  if ($plain.Output) { Write-Host $plain.Output }
  if ($plain.NotEnabled -or $plain.ExitCode -ne 0) { throw 'tailscale serve --http failed (see the message above).' }
  return 'http'
}

function Get-ServeAnalysis { return (Get-CpaServeAnalysis (Get-CpaServeConfig $ts) $fqdn $self.ShortName) }

# One retry: the first request after a serve change can race tailscaled applying it.
function Test-TailnetHealth([string]$Scheme) {
  $uri = "${Scheme}://${fqdn}:$port/healthz"
  $response = Invoke-CpaHttp -Uri $uri -TimeoutSec 10
  if ($response.Status -ne 200) {
    Start-Sleep -Seconds 2
    $response = Invoke-CpaHttp -Uri $uri -TimeoutSec 10
  }
  return $response
}

# ProfileName from `tailscale debug prefs`; an absent field means no nickname. The same output
# holds the node's private key (Config.PrivateNodeKey): it is parsed in memory only and never
# printed, logged or saved, and parse errors are reported without the text.
function Get-TailscaleNickname {
  $result = Invoke-CpaNative -FilePath $ts -Arguments @('debug', 'prefs') -TimeoutSec 30
  if ($result.ExitCode -ne 0) { throw "tailscale debug prefs failed (exit $($result.ExitCode)): $($result.StdErr.Trim())" }
  $prefs = $null
  try { $prefs = ConvertFrom-Json -InputObject $result.StdOut } catch { $prefs = $null }
  $result = $null
  if ($prefs -isnot [System.Management.Automation.PSCustomObject]) { throw 'tailscale debug prefs did not print a JSON object.' }
  $property = $prefs.PSObject.Properties['ProfileName']
  if ($null -eq $property -or $null -eq $property.Value) { return '' }
  return [string]$property.Value
}

function ConvertTo-PsLiteral([string]$Value) { return "'" + $Value.Replace("'", "''") + "'" }

function Show-NicknameSteps([string]$Original, [string]$Temporary) {
  $exe = ConvertTo-PsLiteral $ts
  $shownOriginal = 'the default (no nickname)'
  if ($Original) { $shownOriginal = "'$Original'" }
  Write-Host 'Refresh the profile by hand (PowerShell), then run devyre\scripts\tailscale-serve.ps1 again:'
  Write-Host "  & $exe set $(ConvertTo-PsLiteral ('--nickname=' + $Temporary))"
  Write-Host "  & $exe set $(ConvertTo-PsLiteral ('--nickname=' + $Original))   # restores $shownOriginal"
  Write-Host "  ((& $exe debug prefs | Out-String) | ConvertFrom-Json).ProfileName   # must print the original nickname (nothing for the default)"
  Write-Host '  Never share the full `tailscale debug prefs` output: it holds the node''s private key.'
}

# Refreshes the stored login profile (and with it the MagicDNS name plain-HTTP serve uses) with a
# real prefs edit: a temporary nickname, then the original one. The restore always runs.
function Invoke-ProfileRefresh {
  $original = ''
  try {
    $original = Get-TailscaleNickname
  } catch {
    Write-Host "Cannot read the profile nickname: $($_.Exception.Message)"
    Show-NicknameSteps '' 'cpa-refresh'
    return $false
  }
  $temporary = 'cpa-refresh-' + [guid]::NewGuid().ToString('N').Substring(0, 6)
  $shown = 'no nickname'
  if ($original) { $shown = "nickname '$original'" }
  Write-Host "Refreshing the Tailscale profile ($shown): temporary nickname $temporary, then the original again."
  $attempted = $false
  $ok = $true
  try {
    $attempted = $true
    # One token, --nickname=<value>: Windows PowerShell drops an empty separate argument.
    $set = Invoke-CpaNative -FilePath $ts -Arguments @('set', "--nickname=$temporary") -TimeoutSec 30
    if ($set.ExitCode -ne 0) { throw "tailscale set --nickname=$temporary failed: $($set.StdErr.Trim())" }
    $now = Get-TailscaleNickname
    if ($now -cne $temporary) { throw "ProfileName reads '$now' after setting '$temporary'; the prefs output may have changed." }
  } catch {
    Write-Host "Profile refresh failed: $($_.Exception.Message)"
    $ok = $false
  } finally {
    if ($attempted) {
      $restore = Invoke-CpaNative -FilePath $ts -Arguments @('set', "--nickname=$original") -TimeoutSec 30
      $restored = $null
      try { $restored = Get-TailscaleNickname } catch { $restored = $null }
      if ($restore.ExitCode -ne 0 -or $null -eq $restored -or $restored -cne $original) {
        Write-Host "Restoring the nickname failed: it reads '$restored', expected '$original'."
        $ok = $false
      }
    }
  }
  if (-not $ok) {
    Show-NicknameSteps $original $temporary
  } elseif ($original) {
    Write-Host "Profile refreshed; nickname '$original' restored."
  } else {
    Write-Host 'Profile refreshed; the nickname is unset again (default name).'
  }
  return $ok
}

function Show-ResetSteps($Analysis) {
  Write-Host "Serve entries keyed to an old tailnet name remain on port ${port}: $(@($Analysis.Stale) -join ', ')"
  Write-Host "Other serve entries exist ($(@($Analysis.OtherEntries) -join ', ')), so this script does not reset the serve config. By hand:"
  Write-Host '  1. Note every entry:   tailscale serve status'
  Write-Host '  2. Reset:              tailscale serve reset'
  Write-Host '  3. Re-add CPA:         devyre\scripts\tailscale-serve.ps1'
  Write-Host '  4. Re-add the other entries with their own commands (T3 Code re-creates its own).'
}

# Resets the serve config and adds CPA again; only called when every entry is on port 8318.
function Reset-CpaServe([string]$Scheme) {
  Write-Host "Every serve entry belongs to port $port; resetting the serve config and adding CPA again."
  $reset = Invoke-TailscaleServe @('serve', 'reset')
  if ($reset.ExitCode -ne 0) { throw "tailscale serve reset failed: $($reset.Output)" }
  return (Set-CpaServe ($Scheme -eq 'http'))
}

# --- current state ------------------------------------------------------------------------------

$before = Get-ServeAnalysis
foreach ($handler in @($before.Handlers)) {
  $note = ''
  if (@($before.Stale) -contains "$($handler.HostPort) ($($handler.Source))") { $note = '   (old tailnet name)' }
  Write-Host "Current: $($handler.HostPort)$($handler.Path) -> $($handler.Target)$note"
}
$funnels = @(Find-CpaFunnelEntries (Get-CpaServeConfig $ts 'funnel'))
if ($funnels.Count -gt 0) {
  Write-Warning ("Tailscale Funnel is on for $($funnels -join ', '). CPA must never be public; this script never enables Funnel. " +
    'Turn it off with tailscale funnel --https=<port> off (see tailscale funnel status).')
}
if (-not $Http -and $before.Scheme -eq 'http') {
  $Http = $true
  Write-Host "Port $port already serves plain HTTP; keeping HTTP (to switch to HTTPS: tailscale serve --http=$port off, then run this script)."
}

if ($preview) {
  if ($Http) {
    Write-Host "Would run: tailscale serve --bg --http=$port $target"
  } else {
    Write-Host "Would run: tailscale serve --bg --https=$port $target (plain HTTP when Serve HTTPS is not enabled)"
  }
  $scheme = $before.Scheme
  if (-not $scheme) { $scheme = 'http' }
  $health = Invoke-CpaHttp -Uri "${scheme}://${fqdn}:$port/healthz" -TimeoutSec 10
  if ($health.Status -eq 200) {
    Write-Host "${scheme}://${fqdn}:$port/healthz answers 200 now."
  } else {
    Write-Host "${scheme}://${fqdn}:$port/healthz: $(Get-CpaHttpErrorText $health)"
    if ($health.Status -eq 404) {
      $nickname = 'no nickname'
      try {
        $current = Get-TailscaleNickname
        if ($current) { $nickname = "nickname '$current'" }
      } catch { $nickname = 'nickname unreadable' }
      Write-Host "A real run would refresh the Tailscale profile ($nickname): a temporary nickname, then the original again."
      if (@($before.Stale).Count -gt 0) {
        if ($before.OnlyCpaPort) {
          Write-Host "It would then reset the serve config and add CPA again (entries on old names: $(@($before.Stale) -join ', '))."
        } else {
          Write-Host "Entries on old names ($(@($before.Stale) -join ', ')) share the serve config with other entries; it would print the manual steps."
        }
      }
    }
  }
  Write-Host 'Nothing was changed.'
  exit 0
}

# --- configure and verify -----------------------------------------------------------------------

$scheme = Set-CpaServe ([bool]$Http)
$base = "${scheme}://${fqdn}:$port"
$health = Test-TailnetHealth $scheme
if ($health.Status -eq 404) {
  Write-Host "tailscaled answers 404 for ${fqdn}:${port}: its serve entry is keyed to another name (tailnet renamed?)."
  if (-not (Invoke-ProfileRefresh)) { exit 1 }
  $health = Test-TailnetHealth $scheme
  if ($health.Status -ne 200) {
    $now = Get-ServeAnalysis
    if (@($now.Stale).Count -gt 0) {
      if (-not $now.OnlyCpaPort) {
        Show-ResetSteps $now
        exit 1
      }
      $scheme = Reset-CpaServe $scheme
      $base = "${scheme}://${fqdn}:$port"
      $health = Test-TailnetHealth $scheme
    }
  }
}
if ($health.Status -ne 200) {
  Write-Host "$base/healthz still fails: $(Get-CpaHttpErrorText $health)"
  Write-Host 'Check that CPA answers http://127.0.0.1:8317/healthz (devyre\scripts\up.ps1) and compare tailscale serve status with tailscale status.'
  exit 1
}

# Entries left on an old name share port 8318 with CPA; drop them when nothing else is served.
$after = Get-ServeAnalysis
if (@($after.Stale).Count -gt 0) {
  if ($after.OnlyCpaPort) {
    $scheme = Reset-CpaServe $scheme
    $base = "${scheme}://${fqdn}:$port"
    $health = Test-TailnetHealth $scheme
    if ($health.Status -ne 200) {
      Write-Host "$base/healthz fails after the reset: $(Get-CpaHttpErrorText $health)"
      exit 1
    }
  } else {
    Show-ResetSteps $after
    Write-Warning 'CPA answers on the current name, but exposure-check.ps1 fails until the old entries are gone.'
  }
}

$serveStatus = Invoke-CpaNative -FilePath $ts -Arguments @('serve', 'status') -TimeoutSec 30
Write-Host (($serveStatus.StdOut + $serveStatus.StdErr).Trim())
Write-Host ''
Write-Host "Verified: $base/healthz answers 200."
Write-Host "Panel:  $base/management.html   (from any device on your tailnet, e.g. your phone)"
Write-Host "T3 hub: $base   (on this PC, http://127.0.0.1:8317 also works)"
Write-Host "Next:   set CPA_PUBLIC_URL=$base in devyre\deploy\.env. Other PCs can use this base URL for"
Write-Host "        ANTHROPIC_BASE_URL; on this PC the profile keeps using http://127.0.0.1:8317 unless you run"
Write-Host "        devyre\scripts\new-secrets.ps1 -PublicUrl $base"
Write-Host '        After a tailnet rename, also run devyre\scripts\tailnet-trust.ps1 (allowed-hosts holds the old name),'
Write-Host '        then devyre\scripts\exposure-check.ps1.'
