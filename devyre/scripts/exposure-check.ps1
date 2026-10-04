#requires -Version 5.1
<#
.SYNOPSIS
  Checks that CPA is reachable only from this PC and the tailnet, and that passwordless access
  behaves as configured. Prints PASS, FAIL, WARN or SKIP per check and exits 1 on any FAIL.

.DESCRIPTION
  Exposure:
    - every port the cpa container publishes is bound to 127.0.0.1
    - nothing listens on 0.0.0.0, :: or a LAN address for 8317, 54545 or 1455, and 8318 listens on
      Tailscale addresses only (not even on loopback: keyless tailnet access is bound to that port)
    - this PC's LAN IPv4 addresses cannot reach 8317 or 8318
    - Tailscale Funnel is off
    - tailscale serve has exactly one web handler on port 8318, / -> http://127.0.0.1:8317, keyed
      to this PC's current MagicDNS name, and no raw TCP forward reaches CPA
    - <this PC's tailnet URL>/healthz and http://127.0.0.1:8317/healthz answer 200
  Passwordless (management.tailnet-auth, read from the live config with the management key):
    - the cpa container's Docker gateway is inside server.trusted-proxies
    - allowed-hosts holds this PC's current MagicDNS name with tailscale serve's port (:8318)
    - over the tailnet, GET /v8/management/auth/session with X-CPA-Keyless: 1 and no key is 200
      (SKIP when none of this PC's Tailscale IPs is in allowed-devices)
    - the same request without Origin and without a keyless signal is 401
    - the same request with Origin: http://evil.example is 401 or 403
    - direct http://127.0.0.1:8317 behaves as allow-local says, and forged tailnet headers or a
      foreign Origin on it are refused
    - a direct request to http://127.0.0.1:8317 with this PC's tailnet name as Host (on 8317, or
      without a port), a listed X-Forwarded-For and X-CPA-Keyless is refused: that is what a web
      page sends after getting the tailnet name resolved to 127.0.0.1
    - WARN only: whether a throwaway container from the local image, reaching
      host.docker.internal:8317, is trusted as local
  A failed keyless probe lists its preconditions, including management.allow-remote: the server
  trusts a management request without a key only when allow-remote is on or the client is
  loopback, and inside the container no client is loopback.
  Requests without a key never count toward the management ban; only wrong keys do, and this
  script sends no wrong key. It changes nothing; the container probe runs `docker run --rm`.

.PARAMETER Container
  The CPA container. Default cpa.

.PARAMETER ApiBase
  The CPA on this PC. Default http://127.0.0.1:8317.

.PARAMETER KeyFile
  File with the plaintext management key (read access to the live config). Default
  %USERPROFILE%\.cli-proxy-api\secrets\management-key.txt.

.PARAMETER Image
  Image for the container probe. Default cpa-devyre:current.

.PARAMETER TailnetUrl
  Tailnet base URL to probe instead of http(s)://<this PC's MagicDNS name>:8318.

.PARAMETER SkipContainerProbe
  Do not run the throwaway container.

.PARAMETER TailscaleExe
  Path to tailscale.exe when it is not in the default location.
#>
[CmdletBinding()]
param(
  [string]$Container = 'cpa',
  [string]$ApiBase = 'http://127.0.0.1:8317',
  [string]$KeyFile,
  [string]$Image = 'cpa-devyre:current',
  [string]$TailnetUrl,
  [switch]$SkipContainerProbe,
  [string]$TailscaleExe
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0
. (Join-Path $PSScriptRoot 'cpa-common.ps1')
if (-not $KeyFile) { $KeyFile = Get-CpaDefaultKeyFile }
$ApiBase = $ApiBase.TrimEnd('/')
$sessionPath = '/v8/management/auth/session'
$evilOrigin = 'http://evil.example'

$results = New-Object 'System.Collections.Generic.List[object]'
function Report([string]$Status, [string]$Check, [string]$Message) {
  $results.Add([pscustomobject]@{ Status = $Status; Check = $Check; Message = $Message })
  $color = 'Gray'
  switch ($Status) { 'PASS' { $color = 'Green' } 'FAIL' { $color = 'Red' } 'WARN' { $color = 'Yellow' } 'SKIP' { $color = 'DarkGray' } }
  Write-Host ('[{0}] {1}: {2}' -f $Status, $Check, $Message) -ForegroundColor $color
}

function Get-SessionText($Response) {
  if ($Response.Status -eq 0) { return "no response ($($Response.Error))" }
  $text = "HTTP $($Response.Status)"
  $json = $null
  try { $json = ConvertFrom-Json -InputObject $Response.Body } catch { }
  if ($json -is [System.Management.Automation.PSCustomObject]) {
    $method = [string](Get-CpaProperty $json 'method')
    $login = [string](Get-CpaProperty $json 'login')
    $device = [string](Get-CpaProperty $json 'device')
    $errorText = [string](Get-CpaProperty $json 'error')
    if ($method) { $text += ", method $method" }
    if ($login) { $text += ", login $login" }
    if ($device) { $text += ", device $device" }
    if ($errorText) { $text += " ($errorText)" }
  }
  return $text
}

function Get-SessionMethod($Response) {
  if ($Response.Status -ne 200) { return '' }
  try { return [string](Get-CpaProperty (ConvertFrom-Json -InputObject $Response.Body) 'method') } catch { return '' }
}

# A refusal (401, or 403 unless -Only401) is what every negative probe expects; 404 means the
# server predates the session endpoint.
function Report-Refusal([string]$Check, [string]$What, $Response, [switch]$Only401) {
  if ($Response.Status -eq 401 -or ($Response.Status -eq 403 -and -not $Only401)) {
    Report 'PASS' $Check "$What is refused ($(Get-SessionText $Response))"
  } elseif ($Response.Status -eq 403) {
    Report 'FAIL' $Check ("$What got $(Get-SessionText $Response); expected 401 (a 403 means this client IP is banned after " +
      'wrong keys, or management.allow-remote is off)')
  } elseif ($Response.Status -eq 404) {
    Report 'FAIL' $Check "$What got 404: this server has no $sessionPath. Rebuild and restart it with devyre\scripts\up.ps1."
  } else {
    Report 'FAIL' $Check "$What was not refused ($(Get-SessionText $Response))"
  }
}

Write-Host "CPA exposure check, $(Get-Date -Format 'yyyy-MM-dd HH:mm')"

# --- docker -------------------------------------------------------------------------------------

$docker = Get-CpaDockerExe
$info = $null
$gateways = @()
if (-not $docker) {
  Report 'FAIL' 'docker' 'docker is not on PATH, so the published ports cannot be checked. Start Docker Desktop.'
} else {
  $info = Get-CpaContainerInfo $docker $Container
  if ($null -eq $info) {
    Report 'FAIL' 'docker' "container $Container does not exist. Start it with devyre\scripts\up.ps1."
  } else {
    $publish = Get-CpaPublishReport $info
    if (@($publish.Problems).Count -gt 0) {
      Report 'FAIL' 'docker' ("$Container publishes beyond 127.0.0.1: " + (@($publish.Problems) -join '; ') +
        '. Bind every port to 127.0.0.1 in devyre\deploy\docker-compose.yml and run devyre\scripts\up.ps1.')
    } elseif (@($publish.Bindings).Count -eq 0) {
      Report 'FAIL' 'docker' "$Container publishes no port, so CPA is unreachable."
    } else {
      Report 'PASS' 'docker' ('every published port of ' + $Container + ' is on 127.0.0.1 (' + (@($publish.Bindings) -join ', ') + ')')
    }
    if (-not [bool](Get-CpaProperty (Get-CpaProperty $info 'State') 'Running')) {
      Report 'FAIL' 'docker' "$Container is not running. Start it with devyre\scripts\up.ps1 -NoBuild."
    }
    $gateways = @(Get-CpaContainerGateways $info)
  }
}

# --- tailscale ----------------------------------------------------------------------------------

$self = $null
$tailscale = $null
try {
  $tailscale = Get-CpaTailscaleExe $TailscaleExe
  $self = Get-CpaTailnetSelf (Get-CpaTailscaleStatus -TailscaleExe $tailscale)
  if ($self.Backend -ne 'Running') {
    Report 'FAIL' 'tailscale' "Tailscale is not running (state $($self.Backend))."
  }
} catch {
  Report 'FAIL' 'tailscale' $_.Exception.Message
  $self = $null
}
$selfIps = @()
if ($null -ne $self) { $selfIps = @($self.IPs) }

# --- listeners and LAN --------------------------------------------------------------------------

$badListeners = New-Object 'System.Collections.Generic.List[string]'
$loopbackServe = New-Object 'System.Collections.Generic.List[string]'
$listenerText = New-Object 'System.Collections.Generic.List[string]'
foreach ($listener in @(Get-CpaListeners @(8317, 54545, 1455, $script:CpaServePort))) {
  $shown = "$($listener.Address):$($listener.Port) ($($listener.Process))"
  if ($listener.Port -eq $script:CpaServePort) {
    # Keyless tailnet access is bound to serve's port, so nothing may answer it on loopback.
    if (Test-CpaTailnetIp $listener.Address) { $listenerText.Add($shown) }
    elseif (Test-CpaLoopbackIp $listener.Address) { $loopbackServe.Add($shown) }
    else { $badListeners.Add($shown) }
  } elseif (Test-CpaLoopbackIp $listener.Address) {
    $listenerText.Add($shown)
  } else {
    $badListeners.Add($shown)
  }
}
if ($badListeners.Count -gt 0) {
  Report 'FAIL' 'listeners' ('listening beyond loopback and the tailnet: ' + ($badListeners.ToArray() -join ', '))
}
if ($loopbackServe.Count -gt 0) {
  Report 'FAIL' 'listeners' ('something listens on tailscale serve''s port on loopback: ' + ($loopbackServe.ToArray() -join ', ') +
    '. Keyless tailnet access is bound to that port, so a web page that gets the tailnet name resolved to 127.0.0.1 ' +
    'could reach this listener with a trusted Host. Stop it or move it to another port.')
}
if ($badListeners.Count -eq 0 -and $loopbackServe.Count -eq 0) {
  Report 'PASS' 'listeners' ('8317, 54545 and 1455 listen on loopback only and 8318 on Tailscale addresses only (' + ($listenerText.ToArray() -join ', ') + ')')
}

$lan = @(Get-CpaLanIPv4Addresses $selfIps)
if ($lan.Count -eq 0) {
  Report 'SKIP' 'lan' 'this PC has no LAN IPv4 address to test.'
} else {
  $reachable = New-Object 'System.Collections.Generic.List[string]'
  foreach ($address in $lan) {
    foreach ($port in 8317, 8318) {
      if (Test-CpaTcpConnect $address $port 1500) { $reachable.Add("${address}:$port") }
    }
  }
  if ($reachable.Count -gt 0) {
    Report 'FAIL' 'lan' ('reachable from the LAN: ' + ($reachable.ToArray() -join ', '))
  } else {
    Report 'PASS' 'lan' ("this PC's LAN addresses cannot reach 8317 or 8318 (" + ($lan -join ', ') + ')')
  }
}

# --- funnel and serve ---------------------------------------------------------------------------

$scheme = 'http'
if ($null -ne $tailscale -and $null -ne $self) {
  try {
    $funnels = @(Find-CpaFunnelEntries (Get-CpaServeConfig $tailscale 'funnel'))
    if ($funnels.Count -gt 0) {
      Report 'FAIL' 'funnel' ('Tailscale Funnel is on for ' + ($funnels -join ', ') + '. Turn it off with tailscale funnel --https=<port> off (see tailscale funnel status).')
    } else {
      Report 'PASS' 'funnel' 'Tailscale Funnel is off'
    }
  } catch {
    Report 'FAIL' 'funnel' $_.Exception.Message
  }
  try {
    $analysis = Get-CpaServeAnalysis (Get-CpaServeConfig $tailscale 'serve') $self.Fqdn $self.ShortName
    if ($analysis.Scheme) { $scheme = $analysis.Scheme }
    $handlers = @($analysis.Handlers)
    $problems = New-Object 'System.Collections.Generic.List[string]'
    if ($handlers.Count -eq 0) {
      $problems.Add('nothing is served on port 8318. Run devyre\scripts\tailscale-serve.ps1.')
    } elseif ($handlers.Count -gt 1) {
      $problems.Add("port 8318 has $($handlers.Count) web handlers (" + (@($handlers | ForEach-Object { "$($_.HostPort)$($_.Path) -> $($_.Target)" }) -join '; ') +
        '); exactly one is allowed, because a second mount or a stale name on that port shares the keyless origin. Run devyre\scripts\tailscale-serve.ps1.')
    } else {
      $handler = $handlers[0]
      if ($handler.Path -ne '/' -or $handler.Target -ne $script:CpaServeTarget) {
        $problems.Add("port 8318 serves $($handler.Path) -> $($handler.Target); expected / -> $($script:CpaServeTarget).")
      } elseif ($handler.Host -ne $self.Fqdn -and $handler.Host -ne $self.ShortName) {
        $problems.Add("the 8318 entry is keyed to $($handler.Host), not $($self.Fqdn) (tailnet renamed?). Run devyre\scripts\tailscale-serve.ps1.")
      }
    }
    foreach ($forward in @($analysis.RawForwards)) {
      $problems.Add("raw TCP forward to CPA: $forward. It skips serve's header rewriting, so any tailnet device could forge identity headers.")
    }
    if ($problems.Count -gt 0) {
      Report 'FAIL' 'serve' ($problems.ToArray() -join ' ')
    } else {
      Report 'PASS' 'serve' "tailnet only, one handler on port 8318 ($($self.Fqdn):8318/ -> $($script:CpaServeTarget), $scheme)"
    }
    foreach ($proxy in @($analysis.OtherProxies)) {
      Report 'WARN' 'serve' "another serve entry also proxies to CPA: $proxy"
    }
  } catch {
    Report 'FAIL' 'serve' $_.Exception.Message
  }
}

$tailnetBase = ''
if ($TailnetUrl) { $tailnetBase = $TailnetUrl.TrimEnd('/') }
elseif ($null -ne $self) { $tailnetBase = "${scheme}://$($self.Fqdn):8318" }

$tailnetHealthy = $false
if ($tailnetBase) {
  $health = Invoke-CpaHttp -Uri "$tailnetBase/healthz"
  if ($health.Status -eq 200) {
    $tailnetHealthy = $true
    Report 'PASS' 'tailnet' "$tailnetBase/healthz is 200"
  } elseif ($health.Status -eq 404) {
    Report 'FAIL' 'tailnet' ("$tailnetBase/healthz is 404 from tailscaled: no serve entry answers this name (stale tailnet name after a rename?). " +
      'Run devyre\scripts\tailscale-serve.ps1.')
  } else {
    Report 'FAIL' 'tailnet' "$tailnetBase/healthz: $(Get-CpaHttpErrorText $health)"
  }
} else {
  Report 'SKIP' 'tailnet' 'no tailnet URL to probe (Tailscale is not available).'
}
$localHealth = Invoke-CpaHttp -Uri "$ApiBase/healthz"
if ($localHealth.Status -eq 200) {
  Report 'PASS' 'local' "$ApiBase/healthz is 200"
} else {
  Report 'FAIL' 'local' "$ApiBase/healthz: $(Get-CpaHttpErrorText $localHealth)"
}

# --- live config --------------------------------------------------------------------------------

$key = $null
$configOk = $false
$configProblem = ''
$trustedProxies = @()
$auth = $null
$authExists = $false
# management.allow-remote: 'yes', 'NO', or 'unknown' when it could not be read.
$allowRemoteText = 'unknown'
try { $key = Read-CpaManagementKey $KeyFile } catch { $configProblem = $_.Exception.Message }
if ($key) {
  $proxiesResponse = Invoke-CpaManagementApi -ApiBase $ApiBase -Key $key -Path '/config/server/trusted-proxies'
  if ($proxiesResponse.Status -eq 401) {
    $configProblem = Get-CpaKeyRejectedText $KeyFile
  } elseif ($proxiesResponse.Status -eq 200 -or $proxiesResponse.Status -eq 404) {
    if ($proxiesResponse.Status -eq 200) { $trustedProxies = @(ConvertTo-CpaStringArray $proxiesResponse.Json) }
    $authResponse = Invoke-CpaManagementApi -ApiBase $ApiBase -Key $key -Path '/config/management/tailnet-auth'
    if ($authResponse.Status -eq 200) {
      $auth = $authResponse.Json
      $authExists = $true
      $configOk = $true
    } elseif ($authResponse.Status -eq 404) {
      $configOk = $true
    } else {
      $configProblem = 'GET /v8/management/config/management/tailnet-auth: ' + (Get-CpaHttpErrorText $authResponse)
    }
    $remoteResponse = Invoke-CpaManagementApi -ApiBase $ApiBase -Key $key -Path '/config/management/allow-remote'
    if ($remoteResponse.Status -eq 200 -and $remoteResponse.Json -eq $true) {
      $allowRemoteText = 'yes'
    } elseif ($remoteResponse.Status -eq 200 -or $remoteResponse.Status -eq 404) {
      $allowRemoteText = 'NO'
    }
  } else {
    $configProblem = 'GET /v8/management/config/server/trusted-proxies: ' + (Get-CpaHttpErrorText $proxiesResponse)
  }
}
$key = $null
if (-not $configOk) { Report 'FAIL' 'config' "cannot read the live config: $configProblem" }

$enabled = $false
$allowLocal = $false
$allowedHosts = @()
$allowedDevices = @()
$allowedLogins = @()
if ($authExists) {
  $enabled = (Get-CpaProperty $auth 'enabled') -eq $true
  $allowLocal = (Get-CpaProperty $auth 'allow-local') -eq $true
  $allowedHosts = @(ConvertTo-CpaStringArray (Get-CpaProperty $auth 'allowed-hosts') | ForEach-Object { ConvertTo-CpaHostEntry $_ } | Where-Object { $null -ne $_ })
  $allowedDevices = @(ConvertTo-CpaStringArray (Get-CpaProperty $auth 'allowed-devices') | ForEach-Object { ConvertTo-CpaIpText $_ })
  $allowedLogins = @(ConvertTo-CpaStringArray (Get-CpaProperty $auth 'allowed-logins'))
}

# (a) Every host-side client, tailscale serve included, reaches the container from the Docker
# gateway; keyless trust starts with that peer being a trusted proxy.
$gatewayText = 'not checked'
if ($configOk -and $gateways.Count -gt 0) {
  $outside = @($gateways | Where-Object { -not (Test-CpaIpInAny $_ $trustedProxies) })
  if ($outside.Count -gt 0) {
    $gatewayText = 'NO'
    Report 'FAIL' 'trusted-proxies' ("the Docker gateway " + ($outside -join ', ') + ' is not inside server.trusted-proxies (' + ($trustedProxies -join ', ') +
      '). Keyless access needs the gateway there, and bans and logs need it to see real client IPs. Keyless access reads the ' +
      'live list, but bans and logs read it only at start, so restart the container after changing it.')
  } else {
    $gatewayText = 'yes'
    Report 'PASS' 'trusted-proxies' ('the Docker gateway ' + ($gateways -join ', ') + ' is inside server.trusted-proxies')
  }
} elseif ($configOk) {
  Report 'SKIP' 'trusted-proxies' 'no container gateway to check.'
}

$hostsOk = $false
$devicesOk = $false
$loginOk = $false
if ($configOk) {
  if (-not $authExists) {
    Report 'SKIP' 'passwordless' 'management.tailnet-auth is not configured, so every request needs a key. Set it up with devyre\scripts\tailnet-trust.ps1.'
  } elseif (-not $enabled) {
    Report 'SKIP' 'passwordless' 'management.tailnet-auth.enabled is false, so every request needs a key (tailnet-trust.ps1 -Enable turns it on).'
  } elseif ($null -ne $self) {
    # (b) allowed-hosts goes stale on a tailnet rename while the short name keeps working, and a
    # tailnet name counts only together with tailscale serve's port.
    $serveHost = "$($self.Fqdn):$($script:CpaServePort)"
    $hostsOk = Test-CpaHostListed $allowedHosts $self.Fqdn $script:CpaServePort -RequirePort
    if ($hostsOk) {
      Report 'PASS' 'allowed-hosts' "allowed-hosts holds $serveHost"
    } elseif (@($allowedHosts | Where-Object { $_.Name -eq $self.Fqdn }).Count -gt 0) {
      Report 'FAIL' 'allowed-hosts' ("allowed-hosts lists $($self.Fqdn) without tailscale serve's port $($script:CpaServePort), as an older " +
        'tailnet-trust.ps1 wrote it. The server trusts a tailnet name only together with that port, so no device is keyless over ' +
        'the tailnet; re-run devyre\scripts\tailnet-trust.ps1.')
    } else {
      Report 'FAIL' 'allowed-hosts' "allowed-hosts is stale (tailnet renamed?); re-run devyre\scripts\tailnet-trust.ps1. It lacks $serveHost."
    }
    $devicesOk = @($selfIps | Where-Object { $allowedDevices -contains $_ }).Count -gt 0
    $loginOk = $allowedLogins -contains $self.Login
  }
}


# --- keyless probes -----------------------------------------------------------------------------

# One request per probe. When the session endpoint is missing, the server predates passwordless
# access and none of the probes means anything: report that once.
$tailnetProbe = $null
if ($tailnetHealthy) { $tailnetProbe = Invoke-CpaHttp -Uri "$tailnetBase$sessionPath" -Headers @{ 'X-CPA-Keyless' = '1' } }
$localProbe = Invoke-CpaHttp -Uri "$ApiBase$sessionPath" -Headers @{ 'X-CPA-Keyless' = '1' }
$sessionMissing = ($localProbe.Status -eq 404) -or ($null -ne $tailnetProbe -and $tailnetProbe.Status -eq 404)

if ($sessionMissing) {
  Report 'FAIL' 'session' ("GET $sessionPath is 404: the running server predates passwordless access, so the keyless probes cannot run. " +
    'Rebuild and restart it with devyre\scripts\up.ps1, then run this check again.')
} else {
  if ($null -eq $tailnetProbe) {
    Report 'SKIP' 'keyless-tailnet' 'the tailnet URL is not healthy (see above).'
  } elseif (-not $configOk) {
    Report 'SKIP' 'keyless-tailnet' "the live config is unavailable, so the expected answer is unknown ($(Get-SessionText $tailnetProbe))."
  } elseif (-not $enabled) {
    if ($tailnetProbe.Status -eq 200) {
      Report 'FAIL' 'keyless-tailnet' "keyless access answered over the tailnet although tailnet-auth is off: $(Get-SessionText $tailnetProbe)"
    } else {
      Report-Refusal 'keyless-tailnet' 'with tailnet-auth off, the keyless tailnet probe' $tailnetProbe
    }
  } elseif ($null -eq $self) {
    Report 'SKIP' 'keyless-tailnet' "tailscale status is unavailable, so this PC's Tailscale IPs are unknown ($(Get-SessionText $tailnetProbe))."
  } elseif (-not $devicesOk) {
    Report 'SKIP' 'keyless-tailnet' ("none of this PC's Tailscale IPs (" + ($selfIps -join ', ') + ') is in allowed-devices, so the keyless tailnet probe cannot run from here.')
  } elseif ($tailnetProbe.Status -eq 200 -and (Get-SessionMethod $tailnetProbe) -eq 'tailnet') {
    Report 'PASS' 'keyless-tailnet' "GET $sessionPath over the tailnet with X-CPA-Keyless and no key: $(Get-SessionText $tailnetProbe)"
  } else {
    Report 'FAIL' 'keyless-tailnet' "GET $sessionPath over the tailnet with X-CPA-Keyless and no key: $(Get-SessionText $tailnetProbe); expected 200 with method tailnet."
    $yesNo = @{ $true = 'yes'; $false = 'NO' }
    Write-Host '        Preconditions for keyless access over the tailnet:'
    Write-Host "        (a) Docker gateway inside server.trusted-proxies: $gatewayText"
    Write-Host "        (b) allowed-hosts holds $($self.Fqdn):$($script:CpaServePort): $($yesNo[$hostsOk])"
    Write-Host "        (c) one of this PC's Tailscale IPs is in allowed-devices: $($yesNo[$devicesOk])"
    Write-Host "        (d) management.allow-remote is true (the server sees a tailnet client as remote): $allowRemoteText"
    Write-Host "        allowed-logins holds $($self.Login): $($yesNo[$loginOk])"
  }

  if ($null -ne $tailnetProbe) {
    Report-Refusal 'no-signal' 'over the tailnet, a request without Origin and without a keyless signal' (Invoke-CpaHttp -Uri "$tailnetBase$sessionPath") -Only401
    Report-Refusal 'cross-site' "over the tailnet, Origin $evilOrigin with X-CPA-Keyless" (Invoke-CpaHttp -Uri "$tailnetBase$sessionPath" -Headers @{ 'X-CPA-Keyless' = '1'; 'Origin' = $evilOrigin })
  }

  # Direct requests on this PC: allow-local decides, and proxy markers on a loopback Host are
  # forged by definition (tailscale serve only forwards the tailnet names).
  $apiHost = ''
  $apiPort = 0
  try {
    $apiUri = [Uri]$ApiBase
    $apiHost = $apiUri.Host.Trim('[', ']').ToLowerInvariant()
    $apiPort = $apiUri.Port
  } catch {
    $apiHost = ''
  }
  if (-not $configOk) {
    Report 'SKIP' 'keyless-local' "the live config is unavailable, so the expected answer is unknown ($(Get-SessionText $localProbe))."
  } elseif ($enabled -and $allowLocal -and (Test-CpaHostListed $allowedHosts $apiHost $apiPort)) {
    if ($localProbe.Status -eq 200 -and (Get-SessionMethod $localProbe) -eq 'local') {
      Report 'PASS' 'keyless-local' "direct $ApiBase with X-CPA-Keyless and no key: $(Get-SessionText $localProbe) (allow-local is on)"
    } else {
      Report 'FAIL' 'keyless-local' "direct $ApiBase with X-CPA-Keyless and no key: $(Get-SessionText $localProbe); allow-local is on, so 200 with method local was expected."
      Write-Host '        Preconditions for keyless access from this PC:'
      Write-Host "        (a) Docker gateway inside server.trusted-proxies: $gatewayText"
      Write-Host "        (d) management.allow-remote is true (inside the container no client is loopback): $allowRemoteText"
    }
  } elseif ($localProbe.Status -eq 200) {
    Report 'FAIL' 'keyless-local' "direct $ApiBase answered without a key although allow-local does not apply: $(Get-SessionText $localProbe)"
  } else {
    Report-Refusal 'keyless-local' "with allow-local off (or $apiHost not in allowed-hosts), the direct keyless probe" $localProbe
  }
  $forgedHeaders = @{ 'X-CPA-Keyless' = '1'; 'X-Forwarded-For' = '100.64.0.1'; 'Tailscale-User-Login' = 'probe@example.invalid' }
  if ($null -ne $self) {
    if ($selfIps.Count -gt 0) { $forgedHeaders['X-Forwarded-For'] = $selfIps[0] }
    if ($self.Login) { $forgedHeaders['Tailscale-User-Login'] = $self.Login }
  }
  Report-Refusal 'forged-local' 'a direct request with forged X-Forwarded-For and Tailscale-User-Login on a loopback Host' (Invoke-CpaHttp -Uri "$ApiBase$sessionPath" -Headers $forgedHeaders)
  Report-Refusal 'cross-site-local' "a direct request with Origin $evilOrigin and X-CPA-Keyless" (Invoke-CpaHttp -Uri "$ApiBase$sessionPath" -Headers @{ 'X-CPA-Keyless' = '1'; 'Origin' = $evilOrigin })

  # A web page that gets this PC's tailnet name resolved to 127.0.0.1 reaches this port, and the
  # browser's Host names it. Its script can add a listed X-Forwarded-For and X-CPA-Keyless, and a
  # listed tagged node needs no identity headers. The tailnet path accepts only serve's port.
  if ($null -ne $self -and $apiPort -gt 0) {
    $reboundHeaders = @{ 'X-CPA-Keyless' = '1'; 'X-Forwarded-For' = $forgedHeaders['X-Forwarded-For'] }
    foreach ($reboundHost in @("$($self.Fqdn):$apiPort", "$($self.ShortName):$apiPort", $self.Fqdn)) {
      Report-Refusal 'tailnet-name-local' "a direct request with Host $reboundHost, a listed X-Forwarded-For and X-CPA-Keyless" (Invoke-CpaHttp -Uri "$ApiBase$sessionPath" -HostHeader $reboundHost -Headers $reboundHeaders)
    }
  }
}

# --- containers on this PC (WARN only) ----------------------------------------------------------

if ($SkipContainerProbe) {
  Report 'SKIP' 'containers' '-SkipContainerProbe'
} elseif (-not $docker) {
  Report 'SKIP' 'containers' 'docker is not available.'
} elseif ($sessionMissing) {
  Report 'SKIP' 'containers' 'the running server predates passwordless access.'
} elseif (-not ($configOk -and $enabled -and $allowLocal)) {
  Report 'SKIP' 'containers' 'passwordless access or allow-local is off, so containers on this PC are not trusted as local.'
} else {
  $imageCheck = Invoke-CpaNative -FilePath $docker -Arguments @('image', 'inspect', '--format', '{{.Id}}', $Image) -TimeoutSec 30
  if ($imageCheck.ExitCode -ne 0) {
    Report 'SKIP' 'containers' "image $Image not found."
  } else {
    $probe = 'exec 3<>/dev/tcp/host.docker.internal/8317 || exit 3; ' +
      'printf ''GET /v8/management/auth/session HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nX-CPA-Keyless: 1\r\nConnection: close\r\n\r\n'' >&3; ' +
      'IFS= read -r -t 10 line <&3; printf ''%s\n'' "$line"'
    $run = Invoke-CpaNative -FilePath $docker -Arguments @('run', '--rm', '--entrypoint', 'bash', $Image, '-c', $probe) -TimeoutSec 90
    $statusLine = (($run.StdOut -split "`n") | Where-Object { $_ -match '^HTTP/' } | Select-Object -First 1)
    $code = 0
    if ($statusLine -and $statusLine -match '^HTTP/\S+\s+(\d{3})') { $code = [int]$Matches[1] }
    if ($code -eq 200) {
      Report 'WARN' 'containers' ('a throwaway container reached host.docker.internal:8317 and was trusted as local: every container on this PC ' +
        'gets keyless admin access while allow-local is on (devyre/README.md, Tailnet-only and passwordless).')
    } elseif ($code -eq 401 -or $code -eq 403) {
      Report 'PASS' 'containers' "a container reaching host.docker.internal:8317 is not trusted (HTTP $code)"
    } else {
      $detail = ($run.StdErr.Trim() -split "`n" | Select-Object -First 1)
      Report 'SKIP' 'containers' "the container probe got no usable answer (HTTP $code, exit $($run.ExitCode)) $detail"
    }
  }
}

# --- summary ------------------------------------------------------------------------------------

$counts = @{}
foreach ($status in 'PASS', 'FAIL', 'WARN', 'SKIP') { $counts[$status] = @($results | Where-Object { $_.Status -eq $status }).Count }
Write-Host ''
Write-Host ('Summary: {0} PASS, {1} FAIL, {2} WARN, {3} SKIP' -f $counts['PASS'], $counts['FAIL'], $counts['WARN'], $counts['SKIP'])
if ($counts['FAIL'] -gt 0) { exit 1 }
exit 0
