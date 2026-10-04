#requires -Version 5.1
<#
.SYNOPSIS
  Merges upstream/main (router-for-me/CLIProxyAPI) into a new sync/upstream-<date> branch and
  runs the checks. It never pushes.

.DESCRIPTION
  Guided and stop-on-failure: requires a clean working tree, fast-forwards main from origin,
  creates sync/upstream-YYYYMMDD, merges upstream/main with --no-ff, then runs gofmt,
  go build ./cmd/server and go test ./... . On merge conflicts it stops; resolve them per
  devyre/README.md ("Conflict hotspots"), commit, and run the checks by hand.
#>
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path

function Invoke-Git {
  & git -C $repo @args
  if ($LASTEXITCODE -ne 0) { throw "git $($args -join ' ') failed (exit code $LASTEXITCODE)." }
}

foreach ($tool in 'git', 'go', 'gofmt') {
  if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool is not on PATH." }
}
$null = & git -C $repo remote get-url upstream
if ($LASTEXITCODE -ne 0) {
  throw 'No "upstream" remote. Add it with: git remote add upstream https://github.com/router-for-me/CLIProxyAPI.git'
}
$pending = & git -C $repo status --porcelain
if ($LASTEXITCODE -ne 0) { throw 'git status failed.' }
if ($pending) { throw 'The working tree has uncommitted changes. Commit or stash them first.' }

Invoke-Git fetch upstream --tags
Invoke-Git fetch origin
Invoke-Git switch main
Invoke-Git pull --ff-only origin main

$behind = "$(Invoke-Git rev-list --count HEAD..upstream/main)".Trim()
if ($behind -eq '0') {
  Write-Host 'main already contains upstream/main; nothing to sync.'
  return
}

$branch = "sync/upstream-$(Get-Date -Format 'yyyyMMdd')"
$null = & git -C $repo rev-parse --verify --quiet "refs/heads/$branch"
if ($LASTEXITCODE -eq 0) { throw "Branch $branch already exists. Finish or delete it first." }
Invoke-Git switch -c $branch

$upstreamSha = "$(Invoke-Git rev-parse --short upstream/main)".Trim()
& git -C $repo merge --no-ff upstream/main -m "merge: upstream/main $upstreamSha"
if ($LASTEXITCODE -ne 0) {
  throw ("Merge conflicts on $branch. Resolve them per devyre/README.md 'Conflict hotspots', commit, " +
    'then run gofmt -l, go build ./cmd/server and go test ./... by hand.')
}

Push-Location $repo
try {
  # Format only tracked Go trees; a bare "gofmt -l ." would also walk .claude\worktrees.
  $goRoots = @(& git ls-files -- '*.go' | ForEach-Object { ($_ -split '/')[0] } | Sort-Object -Unique)
  if ($LASTEXITCODE -ne 0 -or $goRoots.Count -eq 0) { throw 'git ls-files found no Go files.' }
  $unformatted = @(& gofmt -l @goRoots)
  if ($LASTEXITCODE -ne 0) { throw 'gofmt failed.' }
  if ($unformatted.Count -gt 0) { throw "gofmt needed:`n$($unformatted -join "`n")" }

  $buildOutput = Join-Path $env:TEMP "cpa-build-check-$PID.exe"
  & go build -o $buildOutput ./cmd/server
  if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/server failed.' }
  Remove-Item -LiteralPath $buildOutput -ErrorAction SilentlyContinue

  & go test ./...
  if ($LASTEXITCODE -ne 0) { throw 'go test ./... failed.' }
} finally {
  Pop-Location
}

Write-Host "Green. Push $branch and open the PR against the fork, never upstream:"
Write-Host "  git push -u origin $branch"
Write-Host "  gh pr create --repo Devyre/CLIProxyAPI --base main --head $branch"
