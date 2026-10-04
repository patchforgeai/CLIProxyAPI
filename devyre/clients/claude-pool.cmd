@echo off
rem claude-pool: run Claude Code through the CPA pool (Devyre CLIProxyAPI fork) for one command.
rem Works in cmd, PowerShell and Git Bash, and needs no PowerShell execution policy change.
rem Install: copy this file into a folder on PATH, for example %USERPROFILE%\.local\bin.
rem Base URL: %USERPROFILE%\.cli-proxy-api-client\public-url.txt when present, else http://127.0.0.1:8317.
rem Token: the claude-code-cli client key in %USERPROFILE%\.cli-proxy-api\secrets\client-claude-code-cli.txt.
rem Plain `claude` keeps using your own claude.ai login.
setlocal
set "CPA_KEY_FILE=%USERPROFILE%\.cli-proxy-api\secrets\client-claude-code-cli.txt"
if not exist "%CPA_KEY_FILE%" (
  echo claude-pool: missing %CPA_KEY_FILE%. Run devyre\scripts\new-secrets.ps1 first. 1>&2
  exit /b 1
)
set /p ANTHROPIC_AUTH_TOKEN=<"%CPA_KEY_FILE%"
set "ANTHROPIC_BASE_URL=http://127.0.0.1:8317"
if exist "%USERPROFILE%\.cli-proxy-api-client\public-url.txt" set /p ANTHROPIC_BASE_URL=<"%USERPROFILE%\.cli-proxy-api-client\public-url.txt"
rem No /v1 for Claude Code. Token auth instead of an API key avoids the "use this API key?" prompt.
set "ANTHROPIC_API_KEY="
rem Keep the 1h prompt cache with token auth (verify on your Claude Code version).
set "ENABLE_PROMPT_CACHING_1H=1"
claude %*
exit /b %ERRORLEVEL%
