# Runs a Godot project's headless tests, for Away (see README, Away): Skirmish's
# test_*.gd scripts, each one `extends SceneTree` and exits non-zero on failure.
#
#   godot-test.ps1 game/tests/test_wallet.gd [more files]   those tests
#   godot-test.ps1                                          every game/tests/test_*.gd
#
# Run from the repository root (Away and Loom's run_tests do). The Godot project
# is the game/ folder. A fresh worktree has no .godot/ import cache, so its
# class_name scripts don't resolve yet ("Could not find base class"): the first
# run imports the project (about 15 s).
#
# Each test gets $TimeoutSec. A test script must call quit() itself; one that
# doesn't (or calls it from _ready(), which a SceneTree script never runs) keeps
# Godot open forever. On 10-19 that cost the local model 37 minutes of waiting
# on timeouts, so this stops it early and says why.
param([Parameter(ValueFromRemainingArguments)][string[]]$Files)
$TimeoutSec = if ($env:GODOT_TEST_TIMEOUT) { [int]$env:GODOT_TEST_TIMEOUT } else { 120 }

$exe = Get-ChildItem "$env:LOCALAPPDATA\Microsoft\WinGet\Packages\GodotEngine.GodotEngine_*\Godot_v*_win64_console.exe" -ErrorAction SilentlyContinue |
    Sort-Object Name -Descending | Select-Object -First 1
if (-not $exe) { Write-Output 'Godot not found (winget install GodotEngine.GodotEngine)'; exit 1 }
$godot = $exe.FullName
$project = Join-Path (Get-Location) 'game'
if (-not (Test-Path (Join-Path $project 'project.godot'))) { Write-Output "no Godot project at $project"; exit 1 }

if (-not (Test-Path (Join-Path $project '.godot'))) {
    Write-Output '== importing the project (first run in this checkout)'
    & $godot --headless --path $project --import 2>&1 | Out-Null
}

if (-not $Files) {
    $Files = Get-ChildItem (Join-Path $project 'tests') -Filter 'test_*.gd' | ForEach-Object { "game/tests/$($_.Name)" }
}

$failed = 0
foreach ($f in $Files) {
    $rel = ($f -replace '\\', '/') -replace '^\./', ''
    if ($rel -notmatch '^game/(.+\.gd)$') { Write-Output "FAIL: $f is not a .gd file under game/"; $failed++; continue }
    Write-Output "== $rel"
    # A script that fails to compile doesn't always exit non-zero, so a script
    # error in the output counts as a failure too (as in open-godot.ps1 -Test).
    $res = "res://$($Matches[1])"
    $log = [IO.Path]::GetTempFileName()
    $err = [IO.Path]::GetTempFileName()
    $p = Start-Process -FilePath $godot -ArgumentList @('--headless', '--path', "`"$project`"", '--script', $res) `
        -NoNewWindow -PassThru -RedirectStandardOutput $log -RedirectStandardError $err
    $null = $p.Handle # without this, ExitCode is empty once the process has exited
    $done = $p.WaitForExit($TimeoutSec * 1000)
    if (-not $done) { & taskkill /PID $p.Id /T /F 2>&1 | Out-Null; $p.WaitForExit(5000) | Out-Null }
    $out = @(Get-Content $log) + @(Get-Content $err)
    Remove-Item $log, $err -ErrorAction SilentlyContinue
    $out | Write-Output
    if (-not $done) {
        Write-Output "FAIL: $rel didn't finish in $TimeoutSec s and was stopped. A test script must call quit() itself when its checks are done, from _initialize() or a function it calls; _ready() never runs in a script that extends SceneTree."
        $failed++
    } elseif ($p.ExitCode -ne 0 -or ($out -match 'SCRIPT ERROR|Failed to load script|Parse Error')) {
        Write-Output "FAIL: $rel (exit code $($p.ExitCode) or a script error above)"
        $failed++
    }
}
exit $failed
