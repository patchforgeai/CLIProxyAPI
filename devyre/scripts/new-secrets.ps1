#requires -Version 5.1
<#
.SYNOPSIS
  Generates the CPA secrets and renders %USERPROFILE%\.cli-proxy-api\config.yaml.

.DESCRIPTION
  Creates %USERPROFILE%\.cli-proxy-api\{auths,logs,static,plugins,secrets}, restricts
  secrets\ to the current user, generates the management key plus one API key per client
  (t3-code, claude-code-cli, codex-cli, other-devices) and renders config.yaml from
  devyre\deploy\config.template.yaml.

  Idempotent: existing secrets and config.yaml are kept unless -Rotate is passed.
  The management key is printed once, when it is generated (or again with
  -ShowManagementKey). Client keys are never printed; they stay in secrets\client-*.txt.

.PARAMETER Rotate
  Regenerate every secret and re-render config.yaml. The previous config.yaml is backed up
  next to it first, and its management.tailnet-auth block (the passwordless devices, which
  devyre\scripts\tailnet-trust.ps1 writes into the live config only) is carried over unchanged.
  When the cpa container is running it is restarted right away: the running server does not
  see host-side edits to config.yaml (Docker Desktop delivers no file events for them), so until
  it restarts the old keys keep working.

.PARAMETER ResetTailnetAuth
  With -Rotate, take management.tailnet-auth from the template as well: its lists start empty,
  so nothing is keyless until tailnet-trust.ps1 -Include <names> fills them again. Use it after
  a device you had allowed may have been compromised.

.PARAMETER PublicUrl
  The tailnet URL, e.g. https://<machine>.<tailnet>.ts.net:8318 (no path). Saved to
  %USERPROFILE%\.cli-proxy-api-client\public-url.txt, which the PowerShell profile snippet uses
  as ANTHROPIC_BASE_URL. Without it the snippet uses http://127.0.0.1:8317. The file stays out
  of %USERPROFILE%\.cli-proxy-api, which the container mounts, so nothing running in the
  container can redirect the clients on this PC.

.PARAMETER ShowManagementKey
  Print the existing management key again.

.EXAMPLE
  .\new-secrets.ps1
  .\new-secrets.ps1 -PublicUrl https://<machine>.<tailnet>.ts.net:8318
  .\new-secrets.ps1 -Rotate                     # new secrets, same passwordless devices
  .\new-secrets.ps1 -Rotate -ResetTailnetAuth   # new secrets, nothing keyless until tailnet-trust.ps1 runs
#>
[CmdletBinding()]
param(
  [switch]$Rotate,
  [switch]$ResetTailnetAuth,
  [string]$PublicUrl,
  [switch]$ShowManagementKey
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0
if ($ResetTailnetAuth -and -not $Rotate) { throw '-ResetTailnetAuth only applies together with -Rotate.' }

$cpaHome    = Join-Path $env:USERPROFILE '.cli-proxy-api'
$secrets    = Join-Path $cpaHome 'secrets'
# Client-side settings live outside the CPA home, which the container mounts read-write.
$clientHome = Join-Path $env:USERPROFILE '.cli-proxy-api-client'
$template   = Join-Path $PSScriptRoot '..\deploy\config.template.yaml'
if (-not (Test-Path -LiteralPath $template)) { throw "Missing template: $template" }

# Validate -PublicUrl before touching anything.
$normalizedUrl = $null
if ($PSBoundParameters.ContainsKey('PublicUrl')) {
  $uri = $null
  $isValid = [Uri]::TryCreate($PublicUrl.Trim(), [UriKind]::Absolute, [ref]$uri)
  if ($isValid) {
    $isValid = ($uri.Scheme -eq 'https' -or $uri.Scheme -eq 'http') -and $uri.AbsolutePath -eq '/' -and
      -not $uri.Query -and -not $uri.Fragment -and -not $uri.UserInfo
  }
  if (-not $isValid) {
    throw '-PublicUrl must be a bare http(s) origin such as https://<machine>.<tailnet>.ts.net:8318 (no path).'
  }
  $normalizedUrl = $uri.GetLeftPart([UriPartial]::Authority)
}

foreach ($dir in 'auths', 'logs', 'static', 'plugins', 'secrets') {
  $path = Join-Path $cpaHome $dir
  if (-not (Test-Path -LiteralPath $path)) { New-Item -ItemType Directory -Path $path -Force | Out-Null }
}

# Only the current user may open secrets\: drop inherited ACEs and grant the user's SID full
# control. Files created inside inherit this ACL.
$userSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls.exe $secrets /inheritance:r /grant:r "*$($userSid):(OI)(CI)F" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "icacls could not restrict $secrets (exit code $LASTEXITCODE)." }

$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$generated = @{}

function New-CpaToken([string]$Prefix) {
  $bytes = New-Object byte[] 32
  $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
  try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
  return $Prefix + '-' + [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

# CPA reloads config.yaml only on file events, and Docker Desktop delivers none for host-side
# writes to a bind mount. A running server would keep accepting the old keys, and a management
# save from memory could write them back over the rotated file. So rotation restarts it now.
function Restart-CpaContainer {
  if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Warning 'docker is not on PATH. Restart the cpa container (docker restart cpa) before relying on the rotation: until then the old keys still work.'
    return
  }
  $running = $null
  $found = $false
  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'   # docker writes to stderr when the container or the engine is missing
  try {
    $running = & docker container inspect --format '{{.State.Running}}' cpa 2>$null
    $found = $LASTEXITCODE -eq 0
  } finally {
    $ErrorActionPreference = $previous
  }
  if (-not $found -or "$running".Trim() -ne 'true') {
    Write-Host 'The cpa container is not running; it starts with the new secrets (devyre\scripts\up.ps1).'
    return
  }
  & docker restart cpa | Out-Null
  if ($LASTEXITCODE -ne 0) {
    throw 'docker restart cpa failed, so the server still accepts the old keys. Run devyre\scripts\up.ps1 -NoBuild now.'
  }
  Write-Host 'Restarted the cpa container: the new secrets are live and the old keys no longer work.'
}

# Where the management.tailnet-auth block sits in config text split into lines: Start and Count
# cover the "tailnet-auth:" key directly under the top-level management key and every deeper line
# after it (trailing blank and comment lines excluded), Indent is the key's indentation. $null
# when there is no such block.
function Find-TailnetAuthBlock([string[]]$Lines) {
  $inManagement = $false
  $childIndent = -1
  for ($i = 0; $i -lt $Lines.Count; $i++) {
    $line = $Lines[$i]
    if ($line -match '^\s*(#.*)?$') { continue }
    $indent = $line.Length - $line.TrimStart(' ').Length
    if ($indent -eq 0) {
      $inManagement = ($line -match '^(remote-)?management:\s*(#.*)?$')
      $childIndent = -1
      continue
    }
    if (-not $inManagement) { continue }
    if ($childIndent -lt 0) { $childIndent = $indent }
    if ($indent -ne $childIndent -or $line -notmatch '^\s+tailnet-auth:(\s|$)') { continue }
    $last = $i
    for ($j = $i + 1; $j -lt $Lines.Count; $j++) {
      $next = $Lines[$j]
      if ($next -match '^\s*(#.*)?$') { continue }
      if (($next.Length - $next.TrimStart(' ').Length) -le $childIndent) { break }
      $last = $j
    }
    return [pscustomobject]@{ Start = $i; Count = $last - $i + 1; Indent = $childIndent }
  }
  return $null
}

# $Text with its management.tailnet-auth block replaced by the one in $Previous, moved to the same
# depth. $null when either text has no block, or the previous block is empty. The block is copied
# as text, so its values and comments stay exactly as the server wrote them.
function Copy-TailnetAuthBlock([string]$Text, [string]$Previous) {
  $newline = "`n"
  if ($Text.Contains("`r`n")) { $newline = "`r`n" }
  $target = [string[]]($Text -split "\r?\n")
  $source = [string[]]($Previous -split "\r?\n")
  $to = Find-TailnetAuthBlock $target
  $from = Find-TailnetAuthBlock $source
  if ($null -eq $to -or $null -eq $from) { return $null }
  if ($from.Count -lt 2 -and $source[$from.Start] -match '^\s+tailnet-auth:\s*(#.*)?$') { return $null }
  $result = New-Object 'System.Collections.Generic.List[string]'
  for ($k = 0; $k -lt $to.Start; $k++) { $result.Add($target[$k]) }
  for ($k = $from.Start; $k -lt $from.Start + $from.Count; $k++) {
    $line = $source[$k]
    if ($line.Trim().Length -eq 0) { $result.Add(''); continue }
    $indent = $line.Length - $line.TrimStart(' ').Length
    $depth = $to.Indent
    if ($indent -ge $from.Indent) { $depth = $to.Indent + $indent - $from.Indent }
    $result.Add((' ' * $depth) + $line.TrimStart(' '))
  }
  for ($k = $to.Start + $to.Count; $k -lt $target.Count; $k++) { $result.Add($target[$k]) }
  return ($result.ToArray() -join $newline)
}

function Get-CpaSecret([string]$Name, [string]$Prefix) {
  $path = Join-Path $secrets "$Name.txt"
  if ($Rotate -or -not (Test-Path -LiteralPath $path)) {
    [IO.File]::WriteAllText($path, (New-CpaToken $Prefix), $utf8NoBom)
    $generated[$Name] = $true
  }
  $value = ([IO.File]::ReadAllText($path)).Trim()
  if (-not $value) { throw "Secret file $path is empty; delete it and run this script again." }
  return $value
}

$values = [ordered]@{
  '__MANAGEMENT_KEY__'      = Get-CpaSecret 'management-key'         'cpa-mgmt'
  '__KEY_T3_CODE__'         = Get-CpaSecret 'client-t3-code'         'cpa-t3'
  '__KEY_CLAUDE_CODE_CLI__' = Get-CpaSecret 'client-claude-code-cli' 'cpa-cc'
  '__KEY_CODEX_CLI__'       = Get-CpaSecret 'client-codex-cli'       'cpa-cx'
  '__KEY_OTHER_DEVICES__'   = Get-CpaSecret 'client-other-devices'   'cpa-dev'
}

$configPath = Join-Path $cpaHome 'config.yaml'
if ((Test-Path -LiteralPath $configPath) -and -not $Rotate) {
  if ($generated.Count -gt 0) {
    Write-Warning ('Generated new secrets (' + (($generated.Keys | Sort-Object) -join ', ') +
      ') but config.yaml already exists and was left unchanged. Run with -Rotate to re-render it with every secret.')
  }
  Write-Host 'config.yaml exists; left unchanged (use -Rotate to regenerate secrets and re-render).'
} else {
  $previousText = $null
  $backup = ''
  if (Test-Path -LiteralPath $configPath) {
    $backup = "$configPath.bak-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
    Copy-Item -LiteralPath $configPath -Destination $backup
    Write-Host "Backed up the previous config.yaml to $backup"
    $previousText = [IO.File]::ReadAllText($configPath)
  }
  $text = [IO.File]::ReadAllText((Resolve-Path -LiteralPath $template).ProviderPath)
  # Check the template (not the rendered text: a random token may contain "__x__").
  $unknown = @([regex]::Matches($text, '__[A-Z0-9_]+__') | ForEach-Object { $_.Value } |
      Where-Object { -not $values.Contains($_) } | Sort-Object -Unique)
  if ($unknown.Count -gt 0) { throw "Template placeholders without a generated value: $($unknown -join ', ')" }
  foreach ($key in $values.Keys) { $text = $text.Replace($key, $values[$key]) }
  # The passwordless policy lives only in the runtime config; a rotation keeps it (after the
  # placeholders are filled, so nothing in the copied block is ever replaced).
  $tailnetNote = ''
  if ($null -ne $previousText) {
    $restoreHint = 'Allow your devices again with devyre\scripts\tailnet-trust.ps1 -Include <names> once the container runs; ' +
      "the previous lists are in $backup."
    if ($ResetTailnetAuth) {
      $tailnetNote = "management.tailnet-auth was reset to the template (-ResetTailnetAuth): nothing is keyless now. $restoreHint"
    } else {
      $carried = Copy-TailnetAuthBlock $text $previousText
      if ($null -ne $carried) {
        $text = $carried
        $tailnetNote = 'Kept management.tailnet-auth (the passwordless devices) from the previous config.yaml.'
      } else {
        $tailnetNote = ('The previous config.yaml had no management.tailnet-auth block to keep, so it comes from the template ' +
          "with empty lists and nothing is keyless. $restoreHint")
      }
    }
  }
  [IO.File]::WriteAllText($configPath, $text, $utf8NoBom)
  Write-Host "Rendered $configPath"
  if ($tailnetNote) { Write-Host $tailnetNote }
  if ($Rotate) { Restart-CpaContainer }
}

if ($null -ne $normalizedUrl) {
  if (-not (Test-Path -LiteralPath $clientHome)) { New-Item -ItemType Directory -Path $clientHome -Force | Out-Null }
  $urlFile = Join-Path $clientHome 'public-url.txt'
  [IO.File]::WriteAllText($urlFile, $normalizedUrl, $utf8NoBom)
  Write-Host "Saved $normalizedUrl to $urlFile (base URL for the PowerShell profile snippet)."
}

if ($generated.ContainsKey('management-key') -or $ShowManagementKey) {
  Write-Host ''
  Write-Host 'Management key (save it in your password manager; config.yaml keeps only its hash after the first start):'
  Write-Host $values['__MANAGEMENT_KEY__']
  Write-Host ''
} else {
  Write-Host "Management key unchanged ($secrets\management-key.txt); use -ShowManagementKey to print it."
}
Write-Host "Client keys are in $secrets\client-*.txt and are never printed."
