#requires -Version 5.1
<#
.SYNOPSIS
  Shared helpers for the Devyre CPA scripts. Dot-source it; it is not a command.

.DESCRIPTION
  Used by tailnet-trust.ps1, exposure-check.ps1, tailscale-serve.ps1 and up.ps1:
    - a native-command runner that quotes arguments correctly and never turns stderr into
      PowerShell errors (Windows PowerShell 5.1 does that with 2>&1),
    - tailscale status and serve-config readers,
    - docker publish checks for the cpa container and the compose file,
    - an HTTP client that ignores system proxies, never follows redirects and never throws
      on 4xx/5xx,
    - the management API client.
  Nothing here prints, logs or stores the management key. The helpers are written for
  Set-StrictMode -Version 2.0, which every caller sets.
#>

if ($MyInvocation.InvocationName -ne '.') {
  Write-Warning 'cpa-common.ps1 only defines helpers for the other devyre scripts; dot-source it instead of running it.'
}

$script:CpaTailnetRanges = @('100.64.0.0/10', 'fd7a:115c:a1e0::/48')
$script:CpaDefaultApiBase = 'http://127.0.0.1:8317'
$script:CpaServePort = 8318
$script:CpaServeTarget = 'http://127.0.0.1:8317'

# ---------------------------------------------------------------------------------------------
# Native commands

# Quotes one argument for the Windows command line (the rules CommandLineToArgvW and Go use).
# Windows PowerShell 5.1 drops empty arguments and mangles embedded double quotes on its own.
function ConvertTo-CpaNativeArgument([string]$Value) {
  if ($Value.Length -gt 0 -and $Value -notmatch '[\s"]') { return $Value }
  $builder = New-Object System.Text.StringBuilder
  [void]$builder.Append('"')
  $backslashes = 0
  foreach ($ch in $Value.ToCharArray()) {
    if ($ch -eq '\') { $backslashes++; continue }
    if ($ch -eq '"') {
      [void]$builder.Append([char]'\', 2 * $backslashes + 1)
      [void]$builder.Append('"')
    } else {
      if ($backslashes -gt 0) { [void]$builder.Append([char]'\', $backslashes) }
      [void]$builder.Append($ch)
    }
    $backslashes = 0
  }
  if ($backslashes -gt 0) { [void]$builder.Append([char]'\', 2 * $backslashes) }
  [void]$builder.Append('"')
  return $builder.ToString()
}

# Runs a program and returns ExitCode, StdOut, StdErr and TimedOut. A process that outlives
# -TimeoutSec is killed and reported with exit code -1.
function Invoke-CpaNative {
  param(
    [Parameter(Mandatory = $true)][string]$FilePath,
    [string[]]$Arguments = @(),
    [int]$TimeoutSec = 60
  )
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $FilePath
  $psi.Arguments = ((@($Arguments) | ForEach-Object { ConvertTo-CpaNativeArgument ([string]$_) }) -join ' ')
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.CreateNoWindow = $true
  $psi.StandardOutputEncoding = [System.Text.Encoding]::UTF8
  $psi.StandardErrorEncoding = [System.Text.Encoding]::UTF8
  $proc = [System.Diagnostics.Process]::Start($psi)
  $stdoutTask = $proc.StandardOutput.ReadToEndAsync()
  $stderrTask = $proc.StandardError.ReadToEndAsync()
  $timedOut = $false
  if (-not $proc.WaitForExit($TimeoutSec * 1000)) {
    $timedOut = $true
    try { $proc.Kill() } catch { }
    [void]$proc.WaitForExit(5000)
  }
  $exitCode = -1
  if ($proc.HasExited -and -not $timedOut) { $exitCode = $proc.ExitCode }
  $stdout = ''
  $stderr = ''
  if ($stdoutTask.Wait(5000)) { $stdout = $stdoutTask.Result }
  if ($stderrTask.Wait(5000)) { $stderr = $stderrTask.Result }
  return [pscustomobject]@{ ExitCode = $exitCode; StdOut = $stdout; StdErr = $stderr; TimedOut = $timedOut }
}

# ---------------------------------------------------------------------------------------------
# IP helpers

function Test-CpaIpInCidr([string]$Ip, [string]$Cidr) {
  $address = $null
  $network = $null
  if (-not $Ip -or -not $Cidr) { return $false }
  if (-not [System.Net.IPAddress]::TryParse($Ip.Trim(), [ref]$address)) { return $false }
  $parts = $Cidr.Trim().Split('/')
  if (-not [System.Net.IPAddress]::TryParse($parts[0], [ref]$network)) { return $false }
  if ($address.IsIPv4MappedToIPv6) { $address = $address.MapToIPv4() }
  if ($network.IsIPv4MappedToIPv6) { $network = $network.MapToIPv4() }
  if ($address.AddressFamily -ne $network.AddressFamily) { return $false }
  $a = $address.GetAddressBytes()
  $n = $network.GetAddressBytes()
  $bits = $a.Length * 8
  $prefix = $bits
  if ($parts.Count -gt 1) {
    $parsed = 0
    if (-not [int]::TryParse($parts[1], [ref]$parsed) -or $parsed -lt 0 -or $parsed -gt $bits) { return $false }
    $prefix = $parsed
  }
  for ($i = 0; $i -lt $a.Length; $i++) {
    $remaining = $prefix - 8 * $i
    if ($remaining -le 0) { break }
    $mask = 0xFF
    if ($remaining -lt 8) { $mask = (0xFF -shl (8 - $remaining)) -band 0xFF }
    if (($a[$i] -band $mask) -ne ($n[$i] -band $mask)) { return $false }
  }
  return $true
}

function Test-CpaIpInAny([string]$Ip, [string[]]$Cidrs) {
  foreach ($cidr in @($Cidrs)) {
    if (Test-CpaIpInCidr $Ip $cidr) { return $true }
  }
  return $false
}

function Test-CpaTailnetIp([string]$Ip) { return (Test-CpaIpInAny $Ip $script:CpaTailnetRanges) }

function Test-CpaLoopbackIp([string]$Ip) { return (Test-CpaIpInAny $Ip @('127.0.0.0/8', '::1/128')) }

# Canonical text form of an IP (lower-case, compressed IPv6), or the trimmed input if it is not one.
function ConvertTo-CpaIpText([string]$Ip) {
  $address = $null
  if ([System.Net.IPAddress]::TryParse($Ip.Trim(), [ref]$address)) { return $address.ToString().ToLowerInvariant() }
  return $Ip.Trim().ToLowerInvariant()
}

# An allowed-hosts entry or a Host value as Name (lower-case, no trailing dot, no brackets) and
# Port (0 when absent), or $null when it does not parse. Same rules as the server: name:port,
# [IPv6]:port, or a bare IPv6 literal without a port.
function ConvertTo-CpaHostEntry([string]$Entry) {
  $text = ([string]$Entry).Trim()
  if (-not $text) { return $null }
  $name = $text
  $portText = ''
  if ($text.StartsWith('[')) {
    $end = $text.IndexOf(']')
    if ($end -lt 0) { return $null }
    $name = $text.Substring(1, $end - 1)
    $rest = $text.Substring($end + 1)
    if ($rest) {
      if (-not $rest.StartsWith(':')) { return $null }
      $portText = $rest.Substring(1)
      if (-not $portText) { return $null }
    }
  } elseif (($text.Split(':').Count - 1) -eq 1) {
    $index = $text.IndexOf(':')
    $name = $text.Substring(0, $index)
    $portText = $text.Substring($index + 1)
    if (-not $portText) { return $null }
  }
  $name = $name.TrimEnd('.').ToLowerInvariant()
  if (-not $name) { return $null }
  $port = 0
  if ($portText) {
    if ($portText -notmatch '^[0-9]{1,5}$') { return $null }
    $port = [int]$portText
    if ($port -lt 1 -or $port -gt 65535) { return $null }
  }
  if ($name.Contains(':')) { $name = ConvertTo-CpaIpText $name }
  return [pscustomobject]@{ Name = $name; Port = $port }
}

# True when an allowed-hosts entry names Name on Port. An entry with a port matches only that
# port; an entry without one matches any port unless -RequirePort asks for an entry with a port,
# which is what the server demands of a tailnet name (tailscale serve's port).
function Test-CpaHostListed([object[]]$Entries, [string]$Name, [int]$Port, [switch]$RequirePort) {
  $wanted = ([string]$Name).TrimEnd('.').ToLowerInvariant()
  foreach ($entry in @($Entries)) {
    if ($null -eq $entry -or $entry.Name -ne $wanted) { continue }
    if ($entry.Port -ne 0) {
      if ($entry.Port -eq $Port) { return $true }
    } elseif (-not $RequirePort) {
      return $true
    }
  }
  return $false
}

# ---------------------------------------------------------------------------------------------
# Tailscale

function Get-CpaTailscaleExe([string]$Override) {
  if ($Override) {
    if (-not (Test-Path -LiteralPath $Override)) { throw "tailscale executable not found: $Override" }
    return (Resolve-Path -LiteralPath $Override).ProviderPath
  }
  $path = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'
  if (Test-Path -LiteralPath $path) { return $path }
  $command = Get-Command tailscale -ErrorAction SilentlyContinue
  if (-not $command) { throw 'tailscale.exe not found. Install Tailscale and log in.' }
  return $command.Source
}

function ConvertFrom-CpaJsonText([string]$Text, [string]$What) {
  try {
    return (ConvertFrom-Json -InputObject $Text)
  } catch {
    throw "$What is not valid JSON."
  }
}

# Parsed `tailscale status --json`, or the saved output in -StatusFile.
function Get-CpaTailscaleStatus([string]$TailscaleExe, [string]$StatusFile) {
  if ($StatusFile) {
    if (-not (Test-Path -LiteralPath $StatusFile)) { throw "Status file not found: $StatusFile" }
    return (ConvertFrom-CpaJsonText ([IO.File]::ReadAllText((Resolve-Path -LiteralPath $StatusFile).ProviderPath)) $StatusFile)
  }
  $result = Invoke-CpaNative -FilePath $TailscaleExe -Arguments @('status', '--json') -TimeoutSec 30
  if ($result.ExitCode -ne 0) {
    throw "tailscale status failed (exit $($result.ExitCode)). Is Tailscale running and logged in? $($result.StdErr.Trim())"
  }
  return (ConvertFrom-CpaJsonText $result.StdOut 'tailscale status --json')
}

# Convention: functions that produce a list emit its items (call them inside @(...)). The two
# property readers below are the exception: they hand back the value itself, so an array stays
# an array (even an empty one) when it is assigned to a variable.

# A property of a parsed JSON object, or $null when the object or the property is missing.
function Get-CpaProperty($Object, [string]$Name) {
  if ($null -eq $Object) { return $null }
  $property = $Object.PSObject.Properties[$Name]
  if ($null -eq $property) { return $null }
  return , $property.Value
}

# The non-empty, trimmed strings of a value that may be $null, a scalar or an array.
function ConvertTo-CpaStringArray($Value) {
  $list = New-Object 'System.Collections.Generic.List[string]'
  foreach ($item in @($Value)) {
    if ($null -eq $item) { continue }
    $text = ([string]$item).Trim()
    if ($text) { $list.Add($text) }
  }
  return $list.ToArray()
}

# One record per node: this PC (IsSelf) first, then the peers sorted by MagicDNS label.
function Get-CpaTailnetDevices($Status) {
  $devices = New-Object 'System.Collections.Generic.List[object]'
  $self = Get-CpaProperty $Status 'Self'
  if ($null -eq $self) { throw 'tailscale status has no Self entry.' }
  $nodes = New-Object 'System.Collections.Generic.List[object]'
  $nodes.Add(@{ Node = $self; IsSelf = $true })
  $peerMap = Get-CpaProperty $Status 'Peer'
  if ($null -ne $peerMap) {
    foreach ($peer in $peerMap.PSObject.Properties) { $nodes.Add(@{ Node = $peer.Value; IsSelf = $false }) }
  }
  foreach ($entry in $nodes) {
    $node = $entry.Node
    $fqdn = ([string](Get-CpaProperty $node 'DNSName')).TrimEnd('.').ToLowerInvariant()
    $label = ''
    if ($fqdn) { $label = $fqdn.Split('.')[0] }
    $ips = @(ConvertTo-CpaStringArray (Get-CpaProperty $node 'TailscaleIPs'))
    $devices.Add([pscustomobject]@{
        IsSelf   = $entry.IsSelf
        HostName = [string](Get-CpaProperty $node 'HostName')
        Fqdn     = $fqdn
        Label    = $label
        OS       = [string](Get-CpaProperty $node 'OS')
        UserID   = [string](Get-CpaProperty $node 'UserID')
        Tags     = [string[]]@(ConvertTo-CpaStringArray (Get-CpaProperty $node 'Tags'))
        IPs      = [string[]]@($ips | ForEach-Object { ConvertTo-CpaIpText $_ })
        Online   = [bool](Get-CpaProperty $node 'Online')
      })
  }
  $selfDevice = $devices[0]
  $peers = @($devices | Where-Object { -not $_.IsSelf } | Sort-Object -Property Label, HostName)
  return (@($selfDevice) + $peers)
}

# This PC's MagicDNS names, Tailscale IPs and owning login.
function Get-CpaTailnetSelf($Status) {
  $self = Get-CpaProperty $Status 'Self'
  if ($null -eq $self) { throw 'tailscale status has no Self entry.' }
  $fqdn = ([string](Get-CpaProperty $self 'DNSName')).TrimEnd('.').ToLowerInvariant()
  if (-not $fqdn) { throw 'tailscale status has no Self.DNSName. Turn on MagicDNS for the tailnet.' }
  $userId = [string](Get-CpaProperty $self 'UserID')
  $login = ''
  $users = Get-CpaProperty $Status 'User'
  if ($null -ne $users -and $userId) {
    $userProfile = Get-CpaProperty $users $userId
    $login = [string](Get-CpaProperty $userProfile 'LoginName')
  }
  $ips = @(ConvertTo-CpaStringArray (Get-CpaProperty $self 'TailscaleIPs'))
  return [pscustomobject]@{
    Fqdn      = $fqdn
    ShortName = $fqdn.Split('.')[0]
    HostName  = [string](Get-CpaProperty $self 'HostName')
    UserID    = $userId
    Login     = $login
    Tags      = [string[]]@(ConvertTo-CpaStringArray (Get-CpaProperty $self 'Tags'))
    IPs       = [string[]]@($ips | ForEach-Object { ConvertTo-CpaIpText $_ })
    Backend   = [string](Get-CpaProperty $Status 'BackendState')
  }
}

# Parsed `tailscale serve status --json` (or `funnel status --json`, which prints the same
# config); an empty object when nothing is served.
function Get-CpaServeConfig([string]$TailscaleExe, [string]$Command = 'serve') {
  $result = Invoke-CpaNative -FilePath $TailscaleExe -Arguments @($Command, 'status', '--json') -TimeoutSec 30
  if ($result.ExitCode -ne 0) { throw "tailscale $Command status --json failed (exit $($result.ExitCode)): $($result.StdErr.Trim())" }
  $text = $result.StdOut.Trim()
  # Older clients print a sentence instead of {} when nothing is served.
  if (-not $text -or -not $text.StartsWith('{')) { $text = '{}' }
  return (ConvertFrom-CpaJsonText $text "tailscale $Command status --json")
}

# Every "AllowFunnel": {"<host:port>": true} entry anywhere in a serve config.
function Find-CpaFunnelEntries($Node, [string]$Path = '') {
  $found = New-Object 'System.Collections.Generic.List[string]'
  if ($null -eq $Node) { return $found.ToArray() }
  if ($Node -is [System.Array]) {
    foreach ($item in $Node) { foreach ($hit in @(Find-CpaFunnelEntries $item $Path)) { $found.Add($hit) } }
    return $found.ToArray()
  }
  if ($Node -isnot [System.Management.Automation.PSCustomObject]) { return $found.ToArray() }
  foreach ($property in $Node.PSObject.Properties) {
    if ($property.Name -eq 'AllowFunnel' -and $property.Value -is [System.Management.Automation.PSCustomObject]) {
      foreach ($entry in $property.Value.PSObject.Properties) {
        if ($entry.Value -eq $true) { $found.Add(($Path + $entry.Name).Trim()) }
      }
      continue
    }
    foreach ($hit in @(Find-CpaFunnelEntries $property.Value ($Path + $property.Name + ' > '))) { $found.Add($hit) }
  }
  return $found.ToArray()
}

function Split-CpaHostPort([string]$HostPort) {
  $index = $HostPort.LastIndexOf(':')
  if ($index -lt 0) { return @($HostPort.TrimEnd('.').ToLowerInvariant(), '') }
  return @($HostPort.Substring(0, $index).Trim('[', ']').TrimEnd('.').ToLowerInvariant(), $HostPort.Substring($index + 1))
}

# Summarizes a serve config for port 8318 and anything else that reaches CPA:
#   Handlers       every web handler on port 8318 (HostPort, Host, Path, Target, Source)
#   Stale          8318 entries keyed to another host than this PC's current MagicDNS name
#   RawForwards    TCP forwards to CPA's port (they skip serve's header rewriting)
#   OtherProxies   web handlers on other ports that proxy to CPA
#   OnlyCpaPort    true when every entry (TCP, web, funnel) is on port 8318 and there are no
#                  services or foreground sessions, so `tailscale serve reset` loses nothing else
#   Scheme         'https' or 'http' for port 8318, '' when it is not served
function Get-CpaServeAnalysis($Config, [string]$Fqdn, [string]$ShortName) {
  $port = [string]$script:CpaServePort
  $handlers = New-Object 'System.Collections.Generic.List[object]'
  $stale = New-Object 'System.Collections.Generic.List[string]'
  $rawForwards = New-Object 'System.Collections.Generic.List[string]'
  $otherProxies = New-Object 'System.Collections.Generic.List[string]'
  $otherEntries = New-Object 'System.Collections.Generic.List[string]'
  $scheme = ''

  $sources = New-Object 'System.Collections.Generic.List[object]'
  $sources.Add(@{ Name = 'serve'; Config = $Config })
  $foreground = Get-CpaProperty $Config 'Foreground'
  if ($null -ne $foreground) {
    foreach ($session in $foreground.PSObject.Properties) {
      $sources.Add(@{ Name = "foreground session $($session.Name)"; Config = $session.Value })
      $otherEntries.Add("foreground session $($session.Name)")
    }
  }
  $services = Get-CpaProperty $Config 'Services'
  if ($null -ne $services) {
    foreach ($service in $services.PSObject.Properties) {
      $sources.Add(@{ Name = "service $($service.Name)"; Config = $service.Value })
      $otherEntries.Add("service $($service.Name)")
    }
  }

  foreach ($source in $sources) {
    $tcp = Get-CpaProperty $source.Config 'TCP'
    if ($null -ne $tcp) {
      foreach ($entry in $tcp.PSObject.Properties) {
        $forward = [string](Get-CpaProperty $entry.Value 'TCPForward')
        if ($forward -match ':8317$') { $rawForwards.Add("$($source.Name): tcp port $($entry.Name) -> $forward") }
        if ($entry.Name -ne $port) {
          $otherEntries.Add("$($source.Name): tcp port $($entry.Name)")
        } elseif ($source.Name -eq 'serve') {
          if (Get-CpaProperty $entry.Value 'HTTPS') { $scheme = 'https' }
          elseif (Get-CpaProperty $entry.Value 'HTTP') { $scheme = 'http' }
        }
      }
    }
    $web = Get-CpaProperty $source.Config 'Web'
    if ($null -ne $web) {
      foreach ($site in $web.PSObject.Properties) {
        $split = Split-CpaHostPort $site.Name
        $siteHost = $split[0]
        $sitePort = $split[1]
        $siteHandlers = Get-CpaProperty $site.Value 'Handlers'
        $mounts = @()
        if ($null -ne $siteHandlers) { $mounts = @($siteHandlers.PSObject.Properties) }
        foreach ($mount in $mounts) {
          $target = ''
          $proxy = [string](Get-CpaProperty $mount.Value 'Proxy')
          if ($proxy) { $target = $proxy.TrimEnd('/') }
          elseif (Get-CpaProperty $mount.Value 'Path') { $target = 'files ' + [string](Get-CpaProperty $mount.Value 'Path') }
          elseif ($null -ne (Get-CpaProperty $mount.Value 'Text')) { $target = 'text' }
          elseif (Get-CpaProperty $mount.Value 'Redirect') { $target = 'redirect ' + [string](Get-CpaProperty $mount.Value 'Redirect') }
          else { $target = 'other' }
          if ($sitePort -eq $port) {
            $handlers.Add([pscustomobject]@{ HostPort = $site.Name; Host = $siteHost; Path = $mount.Name; Target = $target; Source = $source.Name })
          } elseif ($target -match '^https?://(127\.0\.0\.1|localhost|\[::1\]):8317$') {
            $otherProxies.Add("$($source.Name): $($site.Name)$($mount.Name) -> $target")
          }
        }
        if ($sitePort -eq $port) {
          if ($siteHost -ne $Fqdn -and $siteHost -ne $ShortName) { $stale.Add("$($site.Name) ($($source.Name))") }
        } else {
          $otherEntries.Add("$($source.Name): web $($site.Name)")
        }
      }
    }
  }
  foreach ($funnel in @(Find-CpaFunnelEntries $Config)) {
    if ($funnel -notmatch (':' + $port + '$')) { $otherEntries.Add("funnel $funnel") }
  }
  return [pscustomobject]@{
    Handlers     = $handlers.ToArray()
    Stale        = $stale.ToArray()
    RawForwards  = $rawForwards.ToArray()
    OtherProxies = $otherProxies.ToArray()
    OtherEntries = $otherEntries.ToArray()
    OnlyCpaPort  = ($otherEntries.Count -eq 0)
    Scheme       = $scheme
  }
}

# ---------------------------------------------------------------------------------------------
# HTTP

# One HTTP request. Never uses a system proxy, never follows redirects and never throws for an
# HTTP status: Status is 0 and Error is set when no response arrived.
function Invoke-CpaHttp {
  param(
    [Parameter(Mandatory = $true)][string]$Uri,
    [string]$Method = 'GET',
    [hashtable]$Headers,
    [string]$HostHeader,
    $Body = $null,
    [string]$ContentType = 'application/json',
    [int]$TimeoutSec = 10
  )
  $result = [pscustomobject]@{ Status = 0; Body = ''; Headers = @{}; Error = '' }
  [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor [System.Net.SecurityProtocolType]::Tls12
  try {
    $request = [System.Net.HttpWebRequest]::Create($Uri)
  } catch {
    $result.Error = "invalid URL $Uri"
    return $result
  }
  $request.Method = $Method
  $request.Proxy = $null
  $request.AllowAutoRedirect = $false
  $request.KeepAlive = $false
  $request.Timeout = $TimeoutSec * 1000
  $request.ReadWriteTimeout = $TimeoutSec * 1000
  $request.ServicePoint.Expect100Continue = $false
  if ($HostHeader) { $request.Host = $HostHeader }
  if ($Headers) {
    foreach ($name in $Headers.Keys) { $request.Headers.Add([string]$name, [string]$Headers[$name]) }
  }
  $response = $null
  try {
    if ($null -ne $Body) {
      $bytes = [System.Text.Encoding]::UTF8.GetBytes([string]$Body)
      $request.ContentType = $ContentType
      $request.ContentLength = $bytes.Length
      $stream = $request.GetRequestStream()
      try { $stream.Write($bytes, 0, $bytes.Length) } finally { $stream.Dispose() }
    }
    $response = $request.GetResponse()
  } catch {
    $exception = $_.Exception
    while ($null -ne $exception -and $exception -isnot [System.Net.WebException] -and $null -ne $exception.InnerException) {
      $exception = $exception.InnerException
    }
    if ($exception -is [System.Net.WebException] -and $null -ne $exception.Response) {
      $response = $exception.Response
    } else {
      $result.Error = $exception.Message
      return $result
    }
  }
  try {
    $result.Status = [int]$response.StatusCode
    foreach ($name in $response.Headers.AllKeys) { $result.Headers[$name] = $response.Headers[$name] }
    $reader = New-Object System.IO.StreamReader($response.GetResponseStream(), [System.Text.Encoding]::UTF8)
    try { $result.Body = $reader.ReadToEnd() } finally { $reader.Dispose() }
  } catch {
    if (-not $result.Error) { $result.Error = $_.Exception.Message }
  } finally {
    $response.Close()
  }
  return $result
}

# "401 missing management key" style text for a failed API response. Server error bodies carry
# no secrets; the text is still capped.
function Get-CpaHttpErrorText($Response) {
  if ($Response.Status -eq 0) { return "no response ($($Response.Error))" }
  $text = "HTTP $($Response.Status)"
  $json = $null
  try { $json = ConvertFrom-Json -InputObject $Response.Body } catch { }
  if ($json -is [System.Management.Automation.PSCustomObject]) {
    $errorCode = [string](Get-CpaProperty $json 'error')
    $message = [string](Get-CpaProperty $json 'message')
    if ($errorCode) { $text += " $errorCode" }
    if ($message) { $text += ": $message" }
  } elseif ($Response.Body) {
    $text += ' ' + $Response.Body.Trim()
  }
  if ($text.Length -gt 400) { $text = $text.Substring(0, 400) + '...' }
  return $text
}

# ---------------------------------------------------------------------------------------------
# Management API

function Get-CpaDefaultKeyFile { return (Join-Path $env:USERPROFILE '.cli-proxy-api\secrets\management-key.txt') }

# The plaintext management key. Callers must never print it or put it in a message.
function Read-CpaManagementKey([string]$KeyFile) {
  if (-not $KeyFile) { $KeyFile = Get-CpaDefaultKeyFile }
  if (-not (Test-Path -LiteralPath $KeyFile)) { throw "Missing $KeyFile. Run devyre\scripts\new-secrets.ps1 first." }
  $key = ([IO.File]::ReadAllText((Resolve-Path -LiteralPath $KeyFile).ProviderPath)).Trim()
  if (-not $key) { throw "$KeyFile is empty." }
  return $key
}

# A request to <ApiBase>/v8/management<Path> with the management key. Adds Json (the parsed
# body, or $null) to the Invoke-CpaHttp result.
function Invoke-CpaManagementApi {
  param(
    [Parameter(Mandatory = $true)][string]$ApiBase,
    [Parameter(Mandatory = $true)][string]$Key,
    [string]$Method = 'GET',
    [Parameter(Mandatory = $true)][string]$Path,
    $Body = $null
  )
  $uri = $ApiBase.TrimEnd('/') + '/v8/management' + $Path
  $response = Invoke-CpaHttp -Uri $uri -Method $Method -Headers @{ Authorization = "Bearer $Key" } -Body $Body
  $json = $null
  if ($response.Body) {
    try { $json = ConvertFrom-Json -InputObject $response.Body } catch { $json = $null }
  }
  $response | Add-Member -NotePropertyName Json -NotePropertyValue $json
  return $response
}

# Explains a rejected management key without retrying: every wrong key counts toward a
# 30-minute ban of the caller's IP, and for requests to 127.0.0.1 that IP is the Docker
# gateway, which every client on this PC shares.
function Get-CpaKeyRejectedText([string]$KeyFile) {
  return ("The management key in $KeyFile was rejected. Do not retry blindly: five wrong keys ban the caller's IP " +
    'for 30 minutes, and for requests to 127.0.0.1 that is the Docker gateway every client on this PC shares. ' +
    'Check the file (new-secrets.ps1 -ShowManagementKey prints the key it holds).')
}

# ---------------------------------------------------------------------------------------------
# Docker

function Get-CpaDockerExe {
  $command = Get-Command docker -ErrorAction SilentlyContinue
  if (-not $command) { return $null }
  return $command.Source
}

# Parsed `docker container inspect <name>`, or $null when the container does not exist.
function Get-CpaContainerInfo([string]$Docker, [string]$Name) {
  $result = Invoke-CpaNative -FilePath $Docker -Arguments @('container', 'inspect', $Name) -TimeoutSec 30
  if ($result.ExitCode -ne 0) { return $null }
  $parsed = ConvertFrom-CpaJsonText $result.StdOut 'docker container inspect'
  $first = @($parsed)
  if ($first.Count -eq 0) { return $null }
  return $first[0]
}

# Port bindings of a container and the ones that are not on 127.0.0.1 (an empty HostIp means
# every interface). Host networking and publish-all count as problems too.
function Get-CpaPublishReport($Info) {
  $bindings = New-Object 'System.Collections.Generic.List[string]'
  $problems = New-Object 'System.Collections.Generic.List[string]'
  $hostConfig = Get-CpaProperty $Info 'HostConfig'
  $networkMode = [string](Get-CpaProperty $hostConfig 'NetworkMode')
  if ($networkMode -eq 'host') { $problems.Add('network mode is host: every port the server opens is on every interface of this PC') }
  if (Get-CpaProperty $hostConfig 'PublishAllPorts') { $problems.Add('publish-all (docker run -P) is on: every exposed port is published on every interface') }
  $sources = @((Get-CpaProperty $hostConfig 'PortBindings'), (Get-CpaProperty (Get-CpaProperty $Info 'NetworkSettings') 'Ports'))
  foreach ($source in $sources) {
    if ($null -eq $source) { continue }
    foreach ($port in $source.PSObject.Properties) {
      foreach ($binding in @($port.Value)) {
        if ($null -eq $binding) { continue }
        $hostIp = [string](Get-CpaProperty $binding 'HostIp')
        $hostPort = [string](Get-CpaProperty $binding 'HostPort')
        $shownIp = $hostIp
        if (-not $shownIp) { $shownIp = '0.0.0.0 (every interface)' }
        $text = "${shownIp}:$hostPort -> $($port.Name)"
        if ($hostIp -eq '127.0.0.1') {
          if (-not $bindings.Contains($text)) { $bindings.Add($text) }
        } elseif (-not $problems.Contains($text)) {
          $problems.Add($text)
        }
      }
    }
  }
  return [pscustomobject]@{ Bindings = $bindings.ToArray(); Problems = $problems.ToArray() }
}

# Gateways of the networks a container is attached to (the address the container sees every
# host-side client as).
function Get-CpaContainerGateways($Info) {
  $gateways = New-Object 'System.Collections.Generic.List[string]'
  $networks = Get-CpaProperty (Get-CpaProperty $Info 'NetworkSettings') 'Networks'
  if ($null -eq $networks) { return $gateways.ToArray() }
  foreach ($network in $networks.PSObject.Properties) {
    foreach ($name in 'Gateway', 'IPv6Gateway') {
      $value = [string](Get-CpaProperty $network.Value $name)
      $address = $null
      if ($value -and [System.Net.IPAddress]::TryParse($value, [ref]$address) -and -not $gateways.Contains($value)) { $gateways.Add($value) }
    }
  }
  return $gateways.ToArray()
}

# Ports a compose file would publish beyond 127.0.0.1, read from `docker compose config`.
function Get-CpaComposePublishProblems([string]$Docker, [string]$Compose, [string]$EnvFile) {
  $arguments = @('compose', '-f', $Compose)
  if ($EnvFile) { $arguments += @('--env-file', $EnvFile) }
  $arguments += @('config', '--format', 'json')
  $result = Invoke-CpaNative -FilePath $Docker -Arguments $arguments -TimeoutSec 60
  if ($result.ExitCode -ne 0) { throw "docker compose config failed (exit $($result.ExitCode)): $($result.StdErr.Trim())" }
  $config = ConvertFrom-CpaJsonText $result.StdOut 'docker compose config'
  $problems = New-Object 'System.Collections.Generic.List[string]'
  $services = Get-CpaProperty $config 'services'
  if ($null -eq $services) { return $problems.ToArray() }
  foreach ($service in $services.PSObject.Properties) {
    if ([string](Get-CpaProperty $service.Value 'network_mode') -eq 'host') {
      $problems.Add("service $($service.Name): network_mode host puts every port on every interface")
    }
    $ports = Get-CpaProperty $service.Value 'ports'
    foreach ($port in @($ports)) {
      if ($null -eq $port) { continue }
      $hostIp = [string](Get-CpaProperty $port 'host_ip')
      $published = [string](Get-CpaProperty $port 'published')
      if (-not $published) { $published = '(random)' }
      if ($hostIp -ne '127.0.0.1') {
        $shownIp = $hostIp
        if (-not $shownIp) { $shownIp = 'every interface' }
        $problems.Add("service $($service.Name): port $published -> $(Get-CpaProperty $port 'target') is published on $shownIp")
      }
    }
  }
  return $problems.ToArray()
}

# ---------------------------------------------------------------------------------------------
# Listeners and LAN addresses

# TCP listeners on the given ports: Address, Port, Process.
function Get-CpaListeners([int[]]$Ports) {
  $rows = New-Object 'System.Collections.Generic.List[object]'
  $raw = @()
  try {
    $raw = @(Get-NetTCPConnection -State Listen -ErrorAction Stop | Where-Object { $Ports -contains [int]$_.LocalPort } |
        ForEach-Object { [pscustomobject]@{ Address = [string]$_.LocalAddress; Port = [int]$_.LocalPort; ProcessId = [int]$_.OwningProcess } })
  } catch {
    # netstat fallback; a listening socket is the one whose foreign address is 0.0.0.0:0 or [::]:0.
    $raw = @()
    foreach ($line in @(& netstat.exe -ano)) {
      if ($line -match '^\s*TCP\s+(\S+):(\d+)\s+(0\.0\.0\.0:0|\[::\]:0)\s+\S+\s+(\d+)\s*$') {
        $port = [int]$Matches[2]
        if ($Ports -contains $port) {
          $raw += [pscustomobject]@{ Address = $Matches[1].Trim('[', ']'); Port = $port; ProcessId = [int]$Matches[4] }
        }
      }
    }
  }
  foreach ($entry in $raw) {
    $name = ''
    try { $name = (Get-Process -Id $entry.ProcessId -ErrorAction Stop).ProcessName } catch { $name = "pid $($entry.ProcessId)" }
    $rows.Add([pscustomobject]@{ Address = $entry.Address; Port = $entry.Port; Process = $name })
  }
  return $rows.ToArray()
}

# IPv4 addresses of this PC's active interfaces other than loopback, link-local and the
# addresses in -Exclude (this PC's Tailscale IPs).
function Get-CpaLanIPv4Addresses([string[]]$Exclude) {
  $list = New-Object 'System.Collections.Generic.List[string]'
  foreach ($nic in [System.Net.NetworkInformation.NetworkInterface]::GetAllNetworkInterfaces()) {
    if ($nic.OperationalStatus -ne [System.Net.NetworkInformation.OperationalStatus]::Up) { continue }
    foreach ($unicast in $nic.GetIPProperties().UnicastAddresses) {
      $address = $unicast.Address
      if ($address.AddressFamily -ne [System.Net.Sockets.AddressFamily]::InterNetwork) { continue }
      $text = $address.ToString()
      if (Test-CpaIpInAny $text @('127.0.0.0/8', '169.254.0.0/16')) { continue }
      if (@($Exclude) -contains $text) { continue }
      if (-not $list.Contains($text)) { $list.Add($text) }
    }
  }
  return $list.ToArray()
}

function Test-CpaTcpConnect([string]$Address, [int]$Port, [int]$TimeoutMs = 1500) {
  $client = New-Object System.Net.Sockets.TcpClient
  try {
    $async = $client.BeginConnect($Address, $Port, $null, $null)
    if (-not $async.AsyncWaitHandle.WaitOne($TimeoutMs)) { return $false }
    try { $client.EndConnect($async); return $true } catch { return $false }
  } catch {
    return $false
  } finally {
    $client.Close()
  }
}
