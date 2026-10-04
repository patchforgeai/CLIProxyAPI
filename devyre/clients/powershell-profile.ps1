# --- CLIProxyAPI pool (devyre) ----------------------------------------------------------------
# Append this file to $PROFILE. `claude` goes through the CPA pool only after Enable-CpaPool has
# created %USERPROFILE%\.cli-proxy-api\pool-enabled, so installing the snippet before any Claude
# account is logged in to CPA cannot break Claude Code.
#   Enable-CpaPool    route claude through the pool in this shell and every new one
#   Disable-CpaPool   back to your own claude.ai login in this shell and every new one
#   claude-direct     one run with your own login while the pool is enabled
#   claudex           optional: Claude Code harness on GPT through the pool (needs Codex in CPA)
# Base URL: %USERPROFILE%\.cli-proxy-api\public-url.txt (devyre\scripts\new-secrets.ps1 -PublicUrl),
# else http://127.0.0.1:8317. Token: the claude-code-cli key in secrets\client-claude-code-cli.txt.
# Never put these values in ~/.claude/settings.json: they would also override T3 Code's
# per-instance environment.

function Get-CpaHome { Join-Path $env:USERPROFILE '.cli-proxy-api' }

function Get-CpaPoolSettings {
  $cpaHome = Get-CpaHome
  $keyFile = Join-Path $cpaHome 'secrets\client-claude-code-cli.txt'
  if (-not (Test-Path -LiteralPath $keyFile)) { return $null }
  $token = ([IO.File]::ReadAllText($keyFile)).Trim()
  if (-not $token) { return $null }
  $baseUrl = 'http://127.0.0.1:8317'
  $urlFile = Join-Path $cpaHome 'public-url.txt'
  if (Test-Path -LiteralPath $urlFile) {
    $savedUrl = ([IO.File]::ReadAllText($urlFile)).Trim().TrimEnd('/')
    if ($savedUrl) { $baseUrl = $savedUrl }
  }
  return @{ BaseUrl = $baseUrl; Token = $token }
}

function Set-CpaPoolEnv {
  $settings = Get-CpaPoolSettings
  if (-not $settings) { return $false }
  $env:ANTHROPIC_BASE_URL = $settings.BaseUrl      # no /v1 for Claude Code
  $env:ANTHROPIC_AUTH_TOKEN = $settings.Token
  $env:ENABLE_PROMPT_CACHING_1H = '1'               # keep the 1h prompt cache with token auth (verify, F15)
  # $env:ENABLE_TOOL_SEARCH = 'true'                # keep tool search with token auth (verify, F15)
  return $true
}

function Clear-CpaPoolEnv {
  Remove-Item Env:ANTHROPIC_BASE_URL, Env:ANTHROPIC_AUTH_TOKEN, Env:ENABLE_PROMPT_CACHING_1H -ErrorAction SilentlyContinue
}

function Enable-CpaPool {
  [CmdletBinding()]
  param()
  $marker = Join-Path (Get-CpaHome) 'pool-enabled'
  if (-not (Set-CpaPoolEnv)) {
    throw "Missing $(Get-CpaHome)\secrets\client-claude-code-cli.txt. Run devyre\scripts\new-secrets.ps1 first."
  }
  if (-not (Test-Path -LiteralPath $marker)) { New-Item -ItemType File -Path $marker | Out-Null }
  Write-Host "CPA pool enabled: claude now uses $env:ANTHROPIC_BASE_URL in this and new shells (claude-direct bypasses it)."
  try {
    # Windows PowerShell 5.1 may not offer TLS 1.2 by default; tailscale serve requires it.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -Uri "$env:ANTHROPIC_BASE_URL/healthz" -UseBasicParsing -TimeoutSec 5 | Out-Null
  } catch {
    Write-Warning "CPA did not answer $env:ANTHROPIC_BASE_URL/healthz. claude fails until it is up; use claude-direct or Disable-CpaPool meanwhile."
  }
}

function Disable-CpaPool {
  [CmdletBinding()]
  param()
  Remove-Item -LiteralPath (Join-Path (Get-CpaHome) 'pool-enabled') -ErrorAction SilentlyContinue
  Clear-CpaPoolEnv
  Write-Host 'CPA pool disabled: claude uses your own login in this and new shells.'
}

function claude-direct {
  # One run with your own claude.ai login; the pool settings are restored afterwards.
  $saved = @{}
  foreach ($name in 'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN') {
    $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    Remove-Item "Env:$name" -ErrorAction SilentlyContinue
  }
  try {
    & claude @args
  } finally {
    foreach ($name in $saved.Keys) {
      if ($null -ne $saved[$name]) { Set-Item "Env:$name" $saved[$name] }
    }
  }
}

function claudex {
  # Optional: Claude Code harness on GPT through the pool, whether or not the pool is enabled.
  # Needs Codex credentials in CPA; confirm the model id with GET /v1/models.
  $settings = Get-CpaPoolSettings
  if (-not $settings) { throw 'Missing the claude-code-cli key. Run devyre\scripts\new-secrets.ps1 first.' }
  $vars = [ordered]@{
    ANTHROPIC_BASE_URL                   = $settings.BaseUrl
    ANTHROPIC_AUTH_TOKEN                 = $settings.Token
    CLAUDE_CODE_SUBAGENT_MODEL           = 'gpt-5.6-sol'
    CLAUDE_CODE_ALWAYS_ENABLE_EFFORT     = '1'
    CLAUDE_CODE_MAX_TOOL_USE_CONCURRENCY = '3'
  }
  $saved = @{}
  foreach ($name in $vars.Keys) {
    $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    Set-Item "Env:$name" $vars[$name]
  }
  try {
    & claude --model gpt-5.6-sol @args
  } finally {
    foreach ($name in $vars.Keys) {
      if ($null -eq $saved[$name]) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue }
      else { Set-Item "Env:$name" $saved[$name] }
    }
  }
}

if (Test-Path -LiteralPath (Join-Path (Get-CpaHome) 'pool-enabled')) {
  if (-not (Set-CpaPoolEnv)) {
    Write-Warning 'CPA pool is enabled but the claude-code-cli key is missing, so claude uses your own login. Run devyre\scripts\new-secrets.ps1 or Disable-CpaPool.'
  }
}
# ----------------------------------------------------------------------------------------------
