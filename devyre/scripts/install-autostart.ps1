#requires -Version 5.1
<#
.SYNOPSIS
  Installs (or removes) the "Devyre CPA" scheduled task that keeps CPA running.

.DESCRIPTION
  The task runs devyre\scripts\ensure-running.ps1 hidden, for the current user:
    - at every sign-in (after a 30 s delay), and
    - every 5 minutes after that, indefinitely (so a crash, a Docker restart, sleep/resume or a
      tailnet rename heals within minutes).
  ensure-running.ps1 starts Docker Desktop when the engine is down, so this works even when
  Docker Desktop's own "start at sign-in" entry is disabled in Task Manager. No admin rights
  are needed. Docker Desktop only runs inside a signed-in session, so CPA comes up when you sign
  in, not at the login screen.

.PARAMETER Remove
  Unregister the task.
#>
[CmdletBinding()]
param([switch]$Remove)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$taskName = 'Devyre CPA'
if ($Remove) {
  if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    Write-Host "Removed scheduled task '$taskName'."
  } else {
    Write-Host "Scheduled task '$taskName' is not installed."
  }
  exit 0
}

$script = (Resolve-Path (Join-Path $PSScriptRoot 'ensure-running.ps1')).Path
$user = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name

# conhost --headless keeps the console window from flashing every 5 minutes.
$conhost = Join-Path $env:SystemRoot 'System32\conhost.exe'
$psArgs = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$script`" -Quiet"
$action = New-ScheduledTaskAction -Execute $conhost -Argument "--headless powershell.exe $psArgs" -WorkingDirectory (Split-Path $script)

# Two triggers: sign-in (30 s delay), plus a time-based trigger that repeats every 5 minutes
# indefinitely. A logon trigger's own repetition only starts after the next sign-in, so the
# time-based one also covers the session the task was installed in.
$logon = New-ScheduledTaskTrigger -AtLogOn -User $user
$logon.Delay = 'PT30S'
$periodic = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) -RepetitionInterval (New-TimeSpan -Minutes 5)
$trigger = @($logon, $periodic)

$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
  -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Minutes 15)
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal `
  -Description 'Keeps Devyre CPA (CLIProxyAPI) running: Docker Desktop, the cpa container and the tailnet URL. See devyre\README.md.' `
  -Force | Out-Null
Write-Host "Installed scheduled task '$taskName': at sign-in and every 5 minutes, runs $script"
Write-Host 'Starting it once now...'
Start-ScheduledTask -TaskName $taskName
