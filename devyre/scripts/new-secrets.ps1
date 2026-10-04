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
  next to it first. When the cpa container is running it is restarted right away: the running
  server does not see host-side edits to config.yaml (Docker Desktop delivers no file events
  for them), so until it restarts the old keys keep working.

.PARAMETER PublicUrl
  The tailnet URL, e.g. https://<machine>.<tailnet>.ts.net:8318 (no path). Saved to
  %USERPROFILE%\.cli-proxy-api\public-url.txt, which the PowerShell profile snippet uses as
  ANTHROPIC_BASE_URL. Without it the snippet uses http://127.0.0.1:8317.

.PARAMETER ShowManagementKey
  Print the existing management key again.

.EXAMPLE
  .\new-secrets.ps1
  .\new-secrets.ps1 -PublicUrl https://<machine>.<tailnet>.ts.net:8318
#>
[CmdletBinding()]
param(
  [switch]$Rotate,
  [string]$PublicUrl,
  [switch]$ShowManagementKey
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$cpaHome  = Join-Path $env:USERPROFILE '.cli-proxy-api'
$secrets  = Join-Path $cpaHome 'secrets'
$template = Join-Path $PSScriptRoot '..\deploy\config.template.yaml'
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
  if (Test-Path -LiteralPath $configPath) {
    $backup = "$configPath.bak-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
    Copy-Item -LiteralPath $configPath -Destination $backup
    Write-Host "Backed up the previous config.yaml to $backup"
  }
  $text = [IO.File]::ReadAllText((Resolve-Path -LiteralPath $template).ProviderPath)
  # Check the template (not the rendered text: a random token may contain "__x__").
  $unknown = @([regex]::Matches($text, '__[A-Z0-9_]+__') | ForEach-Object { $_.Value } |
      Where-Object { -not $values.Contains($_) } | Sort-Object -Unique)
  if ($unknown.Count -gt 0) { throw "Template placeholders without a generated value: $($unknown -join ', ')" }
  foreach ($key in $values.Keys) { $text = $text.Replace($key, $values[$key]) }
  [IO.File]::WriteAllText($configPath, $text, $utf8NoBom)
  Write-Host "Rendered $configPath"
  if ($Rotate) { Restart-CpaContainer }
}

if ($null -ne $normalizedUrl) {
  [IO.File]::WriteAllText((Join-Path $cpaHome 'public-url.txt'), $normalizedUrl, $utf8NoBom)
  Write-Host "Saved $normalizedUrl to public-url.txt (base URL for the PowerShell profile snippet)."
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
