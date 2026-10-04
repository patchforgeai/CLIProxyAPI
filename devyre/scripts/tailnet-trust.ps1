#requires -Version 5.1
<#
.SYNOPSIS
  Writes management.tailnet-auth into the live CPA config: which tailnet devices, and this PC,
  may use CPA without a key.

.DESCRIPTION
  Reads `tailscale status --json` and builds the passwordless policy:
    allowed-logins   the login that owns this PC
    allowed-hosts    this PC's MagicDNS FQDN and short name, each with tailscale serve's port
                     (machine.<tailnet>.ts.net:8318), plus localhost and 127.0.0.1
    allowed-devices  every Tailscale IP, IPv4 and IPv6, of the selected devices

  Candidates are this PC and the untagged peers owned by the same login. Names are MagicDNS
  names: the first label of each device's MagicDNS name (the machine name in the admin console),
  case-insensitive, or the full MagicDNS name. The tailnet keeps them unique. Host names are
  never matched: a node reports its own host name, and many nodes can share one.

  -Include selects devices. A device that is not in the live allowed-devices yet (a new device)
  is selected only by its exact name. Wildcards such as iphone* only keep devices that are
  already allowed, plus this PC. A machine can rename itself, and its MagicDNS name follows its
  host name unless the name is pinned in the admin console, so a wildcard must never pull in a
  machine that renamed itself to fit it. New devices are listed with their node ID, OS and
  registration date. A write that adds one asks for confirmation; -Force skips the question once
  you have checked the -ShowOnly table. Without -Include, a re-run keeps the candidates already
  in the live allowed-devices, so it refreshes the names after a tailnet rename without changing
  the selection. -Exclude removes matches.

  A device whose MagicDNS name or host name holds the token ci, runner, bot, build or agent is
  dropped with a warning, even when -Include names it, unless -AllowAutomationName gives its
  exact MagicDNS name. That guard only catches mistakes; a compromised machine can pick any name.
  Every node on this tailnet is owned by the same login, the CI runner and the bot VM included,
  so tailscale serve stamps them all with the allowed login, and allowed-devices is what keeps
  them out. Tag the automation hosts in the tailnet policy: tagged nodes are never candidates
  and get no identity headers.

  Only management.tailnet-auth is written, with one
  PUT /v8/management/config/management/tailnet-auth to the CPA on this PC, using the management
  key in %USERPROFILE%\.cli-proxy-api\secrets\management-key.txt (never printed). enabled,
  allow-local and proxy-api keep their live values; when the block does not exist yet enabled and
  proxy-api start as true and allow-local as false, like the deploy template. Lists are always sent whole. Afterwards the script re-reads
  the management section and exits 1 unless tailnet-auth is exactly what was sent and nothing
  else changed. A server built before tailnet-auth existed has no
  GET /v8/management/auth/session and rejects the block; the script detects that and writes
  nothing.

.PARAMETER Include
  MagicDNS names of the devices to allow, for example machine,iphone-15,laptop. A new device
  needs its exact name; a wildcard (iphone*, *) only keeps devices that are already allowed,
  plus this PC. The selection replaces the live list, so name every device you want to keep.

.PARAMETER Exclude
  MagicDNS names of devices to leave out (wildcards allowed), applied after -Include.

.PARAMETER AllowAutomationName
  Exact MagicDNS names of devices that may be allowed although their names look like
  automation hosts.

.PARAMETER Force
  Add new devices without asking. Check them in the -ShowOnly table first.

.PARAMETER ShowOnly
  Print the device table and the resulting policy, and write nothing. -WhatIf does the same.

.PARAMETER Disable
  Write enabled: false and keep everything else. Every request needs a key again at once.

.PARAMETER Enable
  Write enabled: true, together with the refreshed lists (to undo -Disable).

.PARAMETER ApiBase
  The CPA on this PC. Default http://127.0.0.1:8317.

.PARAMETER KeyFile
  File with the plaintext management key. Default
  %USERPROFILE%\.cli-proxy-api\secrets\management-key.txt.

.PARAMETER StatusFile
  Read a saved `tailscale status --json` instead of asking tailscale.

.EXAMPLE
  .\tailnet-trust.ps1 -Include machine,iphone-15,laptop -ShowOnly
  .\tailnet-trust.ps1 -Include machine,iphone-15,laptop     # asks before adding new devices
  .\tailnet-trust.ps1            # after a tailnet rename: same devices, fresh names
  .\tailnet-trust.ps1 -Disable
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
  [string[]]$Include,
  [string[]]$Exclude,
  [string[]]$AllowAutomationName,
  [switch]$Force,
  [switch]$ShowOnly,
  [switch]$Disable,
  [switch]$Enable,
  [string]$ApiBase = 'http://127.0.0.1:8317',
  [string]$KeyFile,
  [string]$StatusFile
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0
. (Join-Path $PSScriptRoot 'cpa-common.ps1')

if ($Disable -and $Enable) { throw 'Pass either -Disable or -Enable, not both.' }
if (-not $KeyFile) { $KeyFile = Get-CpaDefaultKeyFile }
$preview = [bool]($ShowOnly -or $WhatIfPreference)
$blockPath = '/config/management/tailnet-auth'
$knownKeys = @('enabled', 'allowed-logins', 'allowed-devices', 'allowed-hosts', 'allow-local', 'proxy-api')
$listKeys = @('allowed-logins', 'allowed-devices', 'allowed-hosts')
$flagKeys = @('enabled', 'allow-local', 'proxy-api')
# Generic name tokens only; never put real host names here.
$automationTokens = @('ci', 'runner', 'bot', 'build', 'agent')

# "a,b" arrives as one string when the script runs with powershell -File.
function Split-NameList([string[]]$Values) {
  $list = New-Object 'System.Collections.Generic.List[string]'
  foreach ($value in @($Values)) {
    if ($null -eq $value) { continue }
    foreach ($part in ([string]$value).Split(',')) {
      $name = $part.Trim()
      if ($name) { $list.Add($name) }
    }
  }
  return $list.ToArray()
}

# Hands back the value itself (an array stays an array), like Get-CpaProperty.
function Get-BlockField($Block, [string]$Name) {
  if ($null -eq $Block) { return $null }
  if ($Block -is [System.Collections.IDictionary]) {
    if ($Block.Contains($Name)) { return , $Block[$Name] }
    return $null
  }
  return , (Get-CpaProperty $Block $Name)
}

function Get-BlockKeys($Block) {
  if ($null -eq $Block) { return @() }
  if ($Block -is [System.Collections.IDictionary]) { return @($Block.Keys | ForEach-Object { [string]$_ }) }
  return @($Block.PSObject.Properties | ForEach-Object { $_.Name })
}

# A missing flag reads as false and a missing list as empty: the fail-closed defaults.
function Get-BlockFlag($Block, [string]$Name) { return ((Get-BlockField $Block $Name) -eq $true) }

function Get-BlockList($Block, [string]$Name) { return (ConvertTo-CpaStringArray (Get-BlockField $Block $Name)) }

# Canonical text of a tailnet-auth block, for comparisons.
function Get-BlockSignature($Block) {
  $lines = New-Object 'System.Collections.Generic.List[string]'
  foreach ($name in $flagKeys) { $lines.Add("$name=$(Get-BlockFlag $Block $name)") }
  foreach ($name in $listKeys) { $lines.Add("$name=" + ((Get-BlockList $Block $name) -join ',')) }
  foreach ($name in (Get-BlockKeys $Block | Where-Object { $knownKeys -notcontains $_ } | Sort-Object)) {
    $lines.Add("$name=" + (ConvertTo-Json -InputObject (Get-BlockField $Block $name) -Depth 20 -Compress))
  }
  return ($lines -join "`n")
}

# Name -> JSON text of every management.* key except tailnet-auth. Holds the secret-key hash:
# compare it, never print it.
function Get-SectionDigest($Section) {
  $digest = @{}
  if ($Section -isnot [System.Management.Automation.PSCustomObject]) { return $digest }
  foreach ($property in $Section.PSObject.Properties) {
    if ($property.Name -eq 'tailnet-auth') { continue }
    $digest[$property.Name] = ConvertTo-Json -InputObject $property.Value -Depth 50 -Compress
  }
  return $digest
}

function Find-AutomationToken([string[]]$Names) {
  foreach ($name in $Names) {
    foreach ($token in ($name.ToLowerInvariant() -split '[^a-z0-9]+')) {
      if ($automationTokens -contains $token) { return $token }
    }
  }
  return ''
}

function Test-WildcardName([string]$Pattern) {
  return [System.Management.Automation.WildcardPattern]::ContainsWildcardCharacters($Pattern)
}

# True when a -Include/-Exclude pattern names the device: a wildcard against its MagicDNS label,
# an exact name against the label or the full MagicDNS name. Never the self-reported host name.
function Test-DeviceName($Device, [string]$Pattern) {
  if (-not $Device.Label) { return $false }
  if (Test-WildcardName $Pattern) { return ($Device.Label -like $Pattern) }
  $exact = $Pattern.TrimEnd('.')
  return ($Device.Label -eq $exact -or $Device.Fqdn -eq $exact)
}

function Show-Policy($Block, [hashtable]$Notes, [hashtable]$DeviceNames) {
  Write-Host 'Resulting management.tailnet-auth:'
  foreach ($name in $flagKeys) {
    $note = ''
    if ($Notes.ContainsKey($name)) { $note = "   ($($Notes[$name]))" }
    Write-Host ('  {0,-16} {1}{2}' -f $name, (Get-BlockFlag $Block $name).ToString().ToLowerInvariant(), $note)
  }
  foreach ($name in $listKeys) {
    $values = @(Get-BlockList $Block $name)
    if ($values.Count -eq 0) {
      Write-Host ('  {0,-16} (empty)' -f $name)
      continue
    }
    $first = $true
    foreach ($value in $values) {
      $label = ''
      if ($name -eq 'allowed-devices' -and $DeviceNames.ContainsKey($value)) { $label = "   $($DeviceNames[$value])" }
      $shownName = ''
      if ($first) { $shownName = $name }
      Write-Host ('  {0,-16} {1}{2}' -f $shownName, $value, $label)
      $first = $false
    }
  }
  foreach ($name in (Get-BlockKeys $Block | Where-Object { $knownKeys -notcontains $_ })) {
    Write-Host ('  {0,-16} {1}   (kept: not managed by this script)' -f $name, (ConvertTo-Json -InputObject (Get-BlockField $Block $name) -Depth 20 -Compress))
  }
}

function Show-Changes($Before, $After, [bool]$BeforeExists, [hashtable]$DeviceNames) {
  if (-not $BeforeExists) {
    Write-Host 'Changes: management.tailnet-auth does not exist yet; it is created.'
    return
  }
  $changes = New-Object 'System.Collections.Generic.List[string]'
  foreach ($name in $flagKeys) {
    $old = Get-BlockFlag $Before $name
    $new = Get-BlockFlag $After $name
    if ($old -ne $new) { $changes.Add(("  {0}: {1} -> {2}" -f $name, $old.ToString().ToLowerInvariant(), $new.ToString().ToLowerInvariant())) }
  }
  foreach ($name in $listKeys) {
    $old = @(Get-BlockList $Before $name)
    $new = @(Get-BlockList $After $name)
    foreach ($value in $new) {
      if ($old -notcontains $value) {
        $label = ''
        if ($DeviceNames.ContainsKey($value)) { $label = " ($($DeviceNames[$value]))" }
        $changes.Add("  $name + $value$label")
      }
    }
    foreach ($value in $old) {
      if ($new -notcontains $value) {
        $label = ''
        if ($name -eq 'allowed-devices') {
          if ($DeviceNames.ContainsKey($value)) { $label = " ($($DeviceNames[$value]))" } else { $label = ' (not a device of this tailnet any more)' }
        }
        $changes.Add("  $name - $value$label")
      }
    }
  }
  if ($changes.Count -eq 0) {
    if ((Get-BlockSignature $Before) -ne (Get-BlockSignature $After)) { $changes.Add('  list order or unmanaged keys') }
  }
  if ($changes.Count -eq 0) {
    Write-Host 'Changes: none.'
  } else {
    Write-Host 'Changes against the live config:'
    foreach ($line in $changes) { Write-Host $line }
  }
}

# PUT the block, then prove that exactly tailnet-auth changed. Returns the exit code. A write that
# adds new devices asks first, unless -Force.
function Write-TailnetAuth($Block, [string]$Key, $SectionBefore, [object[]]$NewDevices) {
  if ($ShowOnly) {
    Write-Host 'Not written (-ShowOnly).'
    return 0
  }
  if (-not $PSCmdlet.ShouldProcess("management.tailnet-auth on $ApiBase", 'PUT /v8/management/config/management/tailnet-auth')) {
    return 0
  }
  if ($serverTooOld) {
    Write-Host "Not written. $serverTooOldText"
    return 1
  }
  $newNames = @(@($NewDevices) | Where-Object { $null -ne $_ } | ForEach-Object { $_.Label })
  if ($newNames.Count -gt 0 -and -not $Force) {
    $confirmed = $false
    try {
      $confirmed = $PSCmdlet.ShouldContinue(('Let the new devices ' + ($newNames -join ', ') + ' use CPA without a key? ' +
          'Check their node IDs, OS and registration dates above first.'), 'Allow new tailnet devices')
    } catch {
      Write-Host ('Not written: this session cannot ask for confirmation of the new devices (' + ($newNames -join ', ') + '). ' +
        'Check them above, then run the same command with -Force.')
      return 1
    }
    if (-not $confirmed) {
      Write-Host 'Not written: the new devices were not confirmed.'
      return 1
    }
  }
  $body = ConvertTo-Json -InputObject $Block -Depth 20 -Compress
  $put = Invoke-CpaManagementApi -ApiBase $ApiBase -Key $Key -Method 'PUT' -Path $blockPath -Body $body
  if ($put.Status -ne 200) {
    Write-Host "The server refused the write: $(Get-CpaHttpErrorText $put)"
    return 1
  }
  $sectionAfter = Invoke-CpaManagementApi -ApiBase $ApiBase -Key $Key -Path '/config/management'
  $blockAfter = Invoke-CpaManagementApi -ApiBase $ApiBase -Key $Key -Path $blockPath
  if ($sectionAfter.Status -ne 200 -or $blockAfter.Status -ne 200) {
    Write-Host ('Written, but re-reading it failed (' + (Get-CpaHttpErrorText $sectionAfter) + '; ' + (Get-CpaHttpErrorText $blockAfter) +
      '). Check management.tailnet-auth in the panel''s config editor.')
    return 1
  }
  $failed = $false
  $before = Get-SectionDigest $SectionBefore
  $after = Get-SectionDigest $sectionAfter.Json
  $names = @(@($before.Keys) + @($after.Keys) | Sort-Object -Unique)
  foreach ($name in $names) {
    if (-not $before.ContainsKey($name) -or -not $after.ContainsKey($name) -or $before[$name] -ne $after[$name]) {
      Write-Host "management.$name changed during the write (another writer, or a server-side rewrite). Review it in the panel."
      $failed = $true
    }
  }
  if ((Get-BlockSignature $blockAfter.Json) -ne (Get-BlockSignature $Block)) {
    Write-Host 'The live management.tailnet-auth differs from what was sent. Review it in the panel''s config editor.'
    $failed = $true
  }
  if ($failed) { return 1 }
  Write-Host 'Written and verified: only management.tailnet-auth changed. The running server applies it at once.'
  return 0
}

# --- live config -------------------------------------------------------------------------------

$key = $null
$liveAvailable = $false
$liveProblem = ''
$section = $null
$liveBlock = $null
$blockExists = $false
$serverTooOld = $false
$serverTooOldText = ('This server was built before management.tailnet-auth existed: it has no GET /v8/management/auth/session ' +
  'and rejects the block as an invalid config. Rebuild and restart it with devyre\scripts\up.ps1, then run this script again.')
try {
  $key = Read-CpaManagementKey $KeyFile
} catch {
  $liveProblem = $_.Exception.Message
}
if ($key) {
  $sectionResponse = Invoke-CpaManagementApi -ApiBase $ApiBase -Key $key -Path '/config/management'
  if ($sectionResponse.Status -eq 200) {
    $section = $sectionResponse.Json
    # Only a server that knows tailnet-auth serves the session endpoint (200 for this valid key).
    $serverTooOld = ((Invoke-CpaManagementApi -ApiBase $ApiBase -Key $key -Path '/auth/session').Status -eq 404)
    $blockResponse = Invoke-CpaManagementApi -ApiBase $ApiBase -Key $key -Path $blockPath
    if ($blockResponse.Status -eq 200) {
      $liveBlock = $blockResponse.Json
      $blockExists = $true
      $liveAvailable = $true
    } elseif ($blockResponse.Status -eq 404) {
      $liveAvailable = $true
    } else {
      $liveProblem = "GET /v8/management$blockPath -> " + (Get-CpaHttpErrorText $blockResponse)
    }
  } elseif ($sectionResponse.Status -eq 401) {
    $liveProblem = Get-CpaKeyRejectedText $KeyFile
    $key = $null
  } elseif ($sectionResponse.Status -eq 0) {
    $liveProblem = "CPA does not answer at $ApiBase ($($sectionResponse.Error)). Start it with devyre\scripts\up.ps1."
  } else {
    $liveProblem = 'GET /v8/management/config/management -> ' + (Get-CpaHttpErrorText $sectionResponse)
  }
}
if ($blockExists -and $liveBlock -isnot [System.Management.Automation.PSCustomObject]) {
  throw 'The live management.tailnet-auth is not a mapping. Fix it in the panel''s config editor first.'
}
if (-not $liveAvailable) {
  if (-not $preview) { throw "Cannot read the live config: $liveProblem" }
  Write-Warning "Cannot read the live config: $liveProblem"
  Write-Warning 'Previewing as if management.tailnet-auth did not exist yet.'
}
if ($serverTooOld) { Write-Warning $serverTooOldText }

# --- -Disable: flip the master switch only ------------------------------------------------------

if ($Disable) {
  if (-not $blockExists) {
    Write-Host 'management.tailnet-auth does not exist, so passwordless access is already off.'
    exit 0
  }
  if (-not (Get-BlockFlag $liveBlock 'enabled')) {
    Write-Host 'management.tailnet-auth.enabled is already false.'
    exit 0
  }
  $disabled = [ordered]@{}
  foreach ($name in (Get-BlockKeys $liveBlock)) { $disabled[$name] = Get-BlockField $liveBlock $name }
  foreach ($name in $flagKeys) { $disabled[$name] = Get-BlockFlag $liveBlock $name }
  foreach ($name in $listKeys) { $disabled[$name] = [string[]]@(Get-BlockList $liveBlock $name) }
  $disabled['enabled'] = $false
  Show-Policy $disabled @{ 'enabled' = 'disabled: every request needs a key again' } @{}
  exit (Write-TailnetAuth $disabled $key $section @())
}

# --- devices --------------------------------------------------------------------------------------

$tailscaleExe = $null
if (-not $StatusFile) { $tailscaleExe = Get-CpaTailscaleExe }
$status = Get-CpaTailscaleStatus -TailscaleExe $tailscaleExe -StatusFile $StatusFile
$self = Get-CpaTailnetSelf $status
if (-not $StatusFile -and $self.Backend -ne 'Running') { throw "Tailscale is not running (state $($self.Backend)). Start it and log in." }
if (@($self.Tags).Count -gt 0) {
  throw ('This PC is a tagged node, so tailscale status names no owner login to allow. Remove its tags in the ' +
    'tailnet admin console, or edit management.tailnet-auth by hand.')
}
if (-not $self.Login) { throw 'tailscale status names no login for this PC.' }

$includePatterns = @(Split-NameList $Include)
$excludePatterns = @(Split-NameList $Exclude)
$automationAllowed = @(Split-NameList $AllowAutomationName)
$liveDevices = @(@(Get-BlockList $liveBlock 'allowed-devices') | ForEach-Object { ConvertTo-CpaIpText $_ })

$rows = New-Object 'System.Collections.Generic.List[object]'
$selected = New-Object 'System.Collections.Generic.List[string]'
$eligible = New-Object 'System.Collections.Generic.List[string]'
$newDevices = New-Object 'System.Collections.Generic.List[object]'
$deviceNames = @{}
foreach ($device in @(Get-CpaTailnetDevices $status)) {
  $label = $device.Label
  $display = $label
  if (-not $display) { $display = $device.HostName }
  if ($device.IsSelf) { $display += ' (this PC)' }
  foreach ($ip in $device.IPs) { $deviceNames[$ip] = $display }
  $tagged = @($device.Tags).Count -gt 0
  $owned = ($device.UserID -eq $self.UserID)
  # A device is already allowed when one of its Tailscale IPs, which the control plane assigns,
  # is in the live list. Anything else is new and must be named exactly.
  $allowedNow = @($device.IPs | Where-Object { $liveDevices -contains $_ }).Count -gt 0
  $decision = 'excluded'
  $reason = ''
  # The guard reads every name the node reports, so it also catches a self-reported host name.
  $token = Find-AutomationToken @(@($label, $device.HostName) | Where-Object { $_ })
  $automationOk = [bool]($label -and @($automationAllowed | Where-Object { $_ -eq $label }).Count -gt 0)
  if (-not $device.IsSelf -and $tagged) {
    $reason = 'tagged node: never listed'
  } elseif (-not $device.IsSelf -and -not $owned) {
    $reason = 'owned by another login'
  } elseif (@($device.IPs).Count -eq 0) {
    $reason = 'no Tailscale IPs'
  } else {
    if ($label -and (-not $token -or $automationOk)) { $eligible.Add($label) }
    $match = ''
    $isNew = $false
    if ($includePatterns.Count -gt 0) {
      $exactBy = ''
      $wildcardBy = ''
      foreach ($pattern in $includePatterns) {
        if (-not (Test-DeviceName $device $pattern)) { continue }
        if (Test-WildcardName $pattern) {
          if (-not $wildcardBy) { $wildcardBy = $pattern }
        } elseif (-not $exactBy) {
          $exactBy = $pattern
        }
      }
      if ($exactBy) {
        $match = "named by -Include $exactBy"
      } elseif ($wildcardBy -and ($allowedNow -or $device.IsSelf)) {
        $match = "matches -Include $wildcardBy"
      } elseif ($wildcardBy) {
        $reason = "new device matched only by the wildcard $wildcardBy; name it exactly to add it (-Include $label)"
      } elseif (-not $label) {
        $reason = 'no MagicDNS name, so -Include cannot name it'
      } else {
        $reason = 'not matched by -Include'
      }
      if ($match -and $allowedNow) { $match = "already allowed; $match" }
      $isNew = -not $allowedNow -and -not $device.IsSelf
    } elseif ($allowedNow) {
      $match = 'already allowed in the live config'
    } else {
      $reason = 'not in the live allowed-devices'
    }
    if ($match) {
      $excludedBy = ''
      foreach ($pattern in $excludePatterns) {
        if (Test-DeviceName $device $pattern) { $excludedBy = $pattern; break }
      }
      if ($excludedBy) {
        $reason = "matches -Exclude $excludedBy"
      } elseif ($token -and -not $automationOk) {
        $reason = "automation name (token '$token')"
        Write-Warning ("Leaving out ${display}: its name has the automation token '$token'. Every device on this tailnet " +
          'shares your login, so an allowed CI runner or bot could read your Claude tokens through the management API. ' +
          "If it is a personal device, pass -AllowAutomationName $label.")
      } else {
        $decision = 'allowed'
        $reason = $match
        if ($isNew) {
          $decision = 'allowed (NEW)'
          $newDevices.Add($device)
        }
        if ($token) { $reason += ' (automation name allowed by -AllowAutomationName)' }
        foreach ($ip in $device.IPs) { if (-not $selected.Contains($ip)) { $selected.Add($ip) } }
      }
    }
  }
  $tagText = 'no'
  if ($tagged) { $tagText = 'yes' }
  $ownerText = 'no'
  if ($owned) { $ownerText = 'yes' }
  $rows.Add([pscustomobject][ordered]@{
      'Device'      = $display
      'Host'        = $device.HostName
      'OS'          = $device.OS
      'IPs'         = (@($device.IPs) -join ' ')
      'Tagged'      = $tagText
      'Owner match' = $ownerText
      'Decision'    = $decision
      'Reason'      = $reason
    })
}

Write-Host "Tailnet devices (names are MagicDNS names; owner match: owned by $($self.Login), the login that owns this PC):"
Write-Host (($rows | Format-Table -AutoSize -Wrap | Out-String -Width 400).TrimEnd())
Write-Host ''

if ($newDevices.Count -gt 0) {
  Write-Host 'New devices, not allowed before. Check each one before you confirm:'
  foreach ($device in $newDevices) {
    $created = $device.Created
    if (-not $created) { $created = 'unknown' }
    Write-Host ('  {0}: node {1}, OS {2}, host name {3}, registered {4}' -f $device.Label, $device.ID, $device.OS, $device.HostName, $created)
    Write-Host ('      IPs {0}' -f (@($device.IPs) -join ' '))
  }
  Write-Host '  A device you just set up shows a recent registration date and the OS you expect. An old date or an unexpected OS'
  Write-Host '  means an existing machine has taken that name: leave it out and check it.'
  Write-Host ''
}

if ($selected.Count -eq 0) {
  if ($includePatterns.Count -gt 0) {
    Write-Host 'No device is selected: -Include named nothing that -Exclude and the automation guard let through.'
  } else {
    Write-Host 'No device is selected. Without -Include the script keeps the devices already in the live allowed-devices, and there are none.'
  }
  $suggestion = '<name>,<name>'
  if ($eligible.Count -gt 0) { $suggestion = ($eligible.ToArray() -join ',') }
  Write-Host 'Name the devices you want by their exact MagicDNS names. These are all the devices that could be allowed; keep only yours:'
  Write-Host "  devyre\scripts\tailnet-trust.ps1 -Include $suggestion -ShowOnly"
  Write-Host 'Nothing was written.'
  exit 1
}

# --- policy ---------------------------------------------------------------------------------------

# The tailnet names carry tailscale serve's port: the server trusts a tailnet name only together
# with that port, so a page that gets the name resolved to 127.0.0.1 and reaches the loopback
# publish (port 8317) gets nothing. Loopback names stay bare; the local path ignores the port.
$hosts = New-Object 'System.Collections.Generic.List[string]'
foreach ($name in @("$($self.Fqdn):$($script:CpaServePort)", "$($self.ShortName):$($script:CpaServePort)", 'localhost', '127.0.0.1')) {
  if ($name -and -not $hosts.Contains($name)) { $hosts.Add($name) }
}
$notes = @{}
$policy = [ordered]@{}
foreach ($name in $flagKeys) {
  if ($blockExists) {
    $policy[$name] = Get-BlockFlag $liveBlock $name
    $notes[$name] = 'kept from the live config'
  } else {
    # allow-local starts off: containers on this PC reach the loopback publish through
    # host.docker.internal and would count as local. This PC uses the tailnet URL instead.
    $policy[$name] = ($name -ne 'allow-local')
    $notes[$name] = 'deploy-template default for a new block'
  }
}
if ($Enable) {
  $policy['enabled'] = $true
  $notes['enabled'] = '-Enable'
} elseif (-not $policy['enabled']) {
  $notes['enabled'] = 'kept from the live config; pass -Enable to turn passwordless access on'
}
$policy['allowed-logins'] = [string[]]@($self.Login)
$policy['allowed-devices'] = $selected.ToArray()
$policy['allowed-hosts'] = $hosts.ToArray()
foreach ($name in (Get-BlockKeys $liveBlock | Where-Object { $knownKeys -notcontains $_ })) {
  $policy[$name] = Get-BlockField $liveBlock $name
}
# Keep the familiar key order: flags first, then the lists, then unmanaged keys.
$ordered = [ordered]@{}
foreach ($name in @('enabled', 'allowed-logins', 'allowed-devices', 'allowed-hosts', 'allow-local', 'proxy-api')) { $ordered[$name] = $policy[$name] }
foreach ($name in $policy.Keys) { if (-not $ordered.Contains($name)) { $ordered[$name] = $policy[$name] } }

Show-Policy $ordered $notes $deviceNames
Show-Changes $liveBlock $ordered $blockExists $deviceNames
Write-Host ''

if ($blockExists -and (Get-BlockSignature $liveBlock) -eq (Get-BlockSignature $ordered)) {
  Write-Host 'The live config already holds this policy; nothing to write.'
  exit 0
}
if (-not $liveAvailable) {
  Write-Host 'Not written (preview without the live config).'
  exit 0
}
$code = Write-TailnetAuth $ordered $key $section $newDevices.ToArray()
if ($code -eq 0 -and -not $preview) {
  Write-Host 'Check the result with devyre\scripts\exposure-check.ps1.'
}
exit $code
