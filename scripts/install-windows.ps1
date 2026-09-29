<#
.SYNOPSIS
  One-time setup: runs homebase at startup and lets Tailscale reach its dashboard.

.DESCRIPTION
  Run from an elevated PowerShell (Run as administrator) in the folder that
  holds homebase.exe and homebase.json:

      powershell -ExecutionPolicy Bypass -File .\install-windows.ps1 -ReplaceOldTasks

  It will:
    1. Create jobtracker.env from your old start.ps1 key, if it isn't there yet.
    2. Add a firewall rule for the dashboard port, allowing Tailscale addresses only.
    3. Register a "homebase" scheduled task that starts at boot, runs whether
       or not you're logged in, never times out, and restarts if it dies.
    4. With -ReplaceOldTasks: disable app-jobtracker / app-sentinel and stop
       the apps they started, so homebase can take them over. This happens
       only after step 3 succeeds.
    5. Start homebase.
#>
param(
    [int]$Port = 8090,
    [switch]$ReplaceOldTasks
)

$ErrorActionPreference = 'Stop'
$dir = $PSScriptRoot
$exe = Join-Path $dir 'homebase.exe'
$config = Join-Path $dir 'homebase.json'

if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated PowerShell (right-click > Run as administrator).'
}
foreach ($f in $exe, $config) {
    if (-not (Test-Path $f)) { throw "Missing $f" }
}

# 1. Job Tracker's API key: move it out of start.ps1 into an env file.
$envFile = 'C:\Users\Admin\apps\jobtracker\jobtracker.env'
if (-not (Test-Path $envFile)) {
    $key = $null
    $oldStart = 'C:\Users\Admin\apps\jobtracker\start.ps1'
    if (Test-Path $oldStart) {
        $m = Select-String -Path $oldStart -Pattern 'ANTHROPIC_API_KEY\s*=\s*"([^"]+)"' | Select-Object -First 1
        if ($m -and $m.Matches[0].Groups[1].Value -notmatch '\.\.\.$') { $key = $m.Matches[0].Groups[1].Value }
    }
    if (-not $key) {
        $secure = Read-Host 'Anthropic API key for Job Tracker (leave blank to skip the AI features)' -AsSecureString
        $key = [Net.NetworkCredential]::new('', $secure).Password
    }
    if ($key) {
        Set-Content -Path $envFile -Value "ANTHROPIC_API_KEY=$key" -Encoding ascii
        # Only you (and SYSTEM/admins) can read it.
        icacls $envFile /inheritance:r /grant:r "${env:USERNAME}:(R,W)" 'SYSTEM:(F)' 'Administrators:(F)' | Out-Null
        Write-Host "Saved the key to $envFile (readable only by you)."
    } else {
        Set-Content -Path $envFile -Value '# ANTHROPIC_API_KEY=sk-ant-...' -Encoding ascii
        Write-Host "Created an empty $envFile. Add your key there later and restart Job Tracker."
    }
}

# 2. Firewall: dashboard reachable from Tailscale (100.64.0.0/10) only,
#    same pattern as your existing app rules.
$ruleName = 'homebase (Tailscale only)'
Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Action Allow -Protocol TCP `
    -LocalPort $Port -RemoteAddress 100.64.0.0/10 | Out-Null
Write-Host "Firewall: port $Port open to Tailscale only."

# 3. The homebase task itself. Registered BEFORE touching the old tasks, so a
#    mistyped password can't leave your apps stopped.
$action = New-ScheduledTaskAction -Execute $exe -Argument "-config `"$config`"" -WorkingDirectory $dir
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -MultipleInstances IgnoreNew
# "Run whether user is logged on or not" needs your Windows password once.
# Read-Host (not Get-Credential) so this works over SSH, where no window can pop up.
$user = $env:USERNAME
$registered = $false
for ($try = 1; $try -le 3 -and -not $registered; $try++) {
    $secure = Read-Host "Windows password for $user (so homebase can start at boot)" -AsSecureString
    $plain = [Net.NetworkCredential]::new('', $secure).Password
    try {
        Register-ScheduledTask -TaskName 'homebase' -Action $action -Trigger $trigger -Settings $settings `
            -User $user -Password $plain -RunLevel Limited -Force -ErrorAction Stop | Out-Null
        $registered = $true
    } catch {
        Write-Warning "Couldn't register the task: $($_.Exception.Message)"
    } finally {
        $plain = $null
    }
}
if (-not $registered) { throw 'Giving up after 3 tries. Nothing else was changed; your apps are still running.' }
Write-Host 'Registered scheduled task "homebase" (starts at boot).'

# 4. Hand the apps over from the old tasks.
if ($ReplaceOldTasks) {
    foreach ($t in 'app-jobtracker', 'app-sentinel') {
        if (Get-ScheduledTask -TaskName $t -ErrorAction SilentlyContinue) {
            Stop-ScheduledTask -TaskName $t -ErrorAction SilentlyContinue
            Disable-ScheduledTask -TaskName $t | Out-Null
            Write-Host "Disabled old task $t (not deleted; re-enable it to roll back)."
        }
    }
    Get-Process JobTracker, sentinel -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Seconds 2
}

# 5. Go.
Start-ScheduledTask -TaskName 'homebase'
Start-Sleep -Seconds 3
try {
    $st = Invoke-RestMethod "http://127.0.0.1:$Port/api/status"
    Write-Host "`nhomebase is up on $($st.host):"
    $st.apps | ForEach-Object { Write-Host ("  {0,-14} {1}" -f $_.name, $_.state) }
    Write-Host "`nDashboard: http://$($env:COMPUTERNAME.ToLower()):$Port/"
} catch {
    Write-Warning "homebase didn't answer yet. Check $dir\logs and Task Scheduler > homebase > History."
}
