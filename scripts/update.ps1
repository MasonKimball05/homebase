<#
.SYNOPSIS
  Pull the latest code from GitHub and redeploy an app (or homebase itself).

.EXAMPLE
  # From anywhere on the desktop:
  C:\Users\Admin\projects\homebase\scripts\update.ps1 -App sentinel
  C:\Users\Admin\projects\homebase\scripts\update.ps1 -App homebase

  # Or from the Mac:
  ssh desktop "powershell -NoProfile -ExecutionPolicy Bypass -File C:\Users\Admin\projects\homebase\scripts\update.ps1 -App sentinel"

.DESCRIPTION
  -App <name>   An app from homebase.json that has a "source" block:
                git pull in source.repo_dir, then (with the app stopped, because
                Windows locks a running .exe) run source.build, then start it.
                If the build fails, the old version is started again.

  -App homebase Updates homebase itself from this repo: builds a new exe,
                checks it against the new config, then swaps it in and
                restarts the task. On any failure the running copy is untouched.
#>
param(
    [Parameter(Mandatory)][string]$App,
    [string]$HomebaseDir = 'C:\Users\Admin\apps\homebase',
    [int]$Port = 8090
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent   # this script lives in <repo>\scripts
$liveConfig = Join-Path $HomebaseDir 'homebase.json'

function Invoke-Native([string]$exe, [string[]]$argv, [string]$where) {
    Push-Location $where
    try {
        & $exe @argv
        if ($LASTEXITCODE -ne 0) { throw "'$exe $($argv -join ' ')' failed with exit code $LASTEXITCODE" }
    } finally { Pop-Location }
}

function Invoke-Homebase([string]$action) {
    # The same endpoint the dashboard's buttons use (the header is its CSRF check).
    Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$Port/api/apps/$App/$action" -Headers @{ 'X-Homebase' = '1' } | Out-Null
}

function Wait-AppState([string[]]$states, [int]$seconds = 60) {
    $deadline = (Get-Date).AddSeconds($seconds)
    do {
        $s = (Invoke-RestMethod "http://127.0.0.1:$Port/api/status").apps | Where-Object name -eq $App
        if ($states -contains $s.state) { return $s }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)
    throw "$App didn't reach state '$($states -join '/')' within $seconds s (it's '$($s.state)')"
}

function Pull([string]$dir) {
    Write-Host "Pulling $dir"
    $before = (git -C $dir rev-parse --short HEAD)
    Invoke-Native git @('pull', '--ff-only') $dir
    $after = (git -C $dir rev-parse --short HEAD)
    if ($before -eq $after) { Write-Host "  already up to date ($after)" }
    else { Write-Host "  $before -> $after"; git -C $dir log --oneline "$before..$after" | ForEach-Object { "    $_" } }
}

if ($App -eq 'homebase') {
    Pull $repo
    $newExe = Join-Path $HomebaseDir 'homebase.new.exe'
    $newConfig = Join-Path $repo 'deploy\homebase.json'
    Write-Host 'Building homebase'
    Invoke-Native go @('build', '-trimpath', '-o', $newExe, '.') $repo

    # Check the new build against the new config before touching anything live.
    Write-Host 'Checking the new build and config'
    & $newExe -check -config $newConfig
    if ($LASTEXITCODE -ne 0) {
        Remove-Item $newExe -ErrorAction SilentlyContinue
        throw 'Check failed; homebase was not changed.'
    }

    Write-Host 'Swapping in the new version (apps restart with it)'
    schtasks /End /TN homebase | Out-Null
    Start-Sleep -Seconds 2
    $exe = Join-Path $HomebaseDir 'homebase.exe'
    Copy-Item $exe "$exe.previous" -Force          # one-step rollback
    Copy-Item $liveConfig "$liveConfig.previous" -Force
    Move-Item $newExe $exe -Force
    Copy-Item $newConfig $liveConfig -Force
    schtasks /Run /TN homebase | Out-Null

    Start-Sleep -Seconds 3
    try {
        $st = Invoke-RestMethod "http://127.0.0.1:$Port/api/status"
        Write-Host 'homebase is back:'
        $st.apps | ForEach-Object { Write-Host ("  {0,-14} {1}" -f $_.name, $_.state) }
    } catch {
        Write-Warning "homebase didn't answer. Roll back with: Copy-Item $exe.previous $exe -Force; Copy-Item $liveConfig.previous $liveConfig -Force; schtasks /Run /TN homebase"
        throw
    }
    return
}

# ---- A regular app ----
$cfg = Get-Content $liveConfig -Raw | ConvertFrom-Json
$entry = $cfg.apps | Where-Object name -eq $App
if (-not $entry) { throw "No app named '$App' in $liveConfig. Known: $(($cfg.apps.name) -join ', '), homebase" }
if (-not $entry.source) { throw "'$App' has no ""source"" block in $liveConfig, so there's nothing to pull or build." }

Pull $entry.source.repo_dir

Write-Host "Stopping $App"
Invoke-Homebase stop
Wait-AppState @('stopped', 'crashed') | Out-Null

$build = @($entry.source.build)
$built = $false
try {
    Write-Host "Building: $($build -join ' ')"
    Invoke-Native $build[0] ($build | Select-Object -Skip 1) $entry.source.repo_dir
    $built = $true
} finally {
    # Start it again whether or not the build worked: a failed build leaves
    # the previous version in place, which beats leaving the app down.
    Write-Host "Starting $App"
    Invoke-Homebase start
}
$s = Wait-AppState @('running', 'crashed', 'backoff')
if ($s.state -ne 'running') { throw "$App is '$($s.state)' after the update. Check its logs on the dashboard." }
if ($built) { Write-Host "$App updated and running." }
