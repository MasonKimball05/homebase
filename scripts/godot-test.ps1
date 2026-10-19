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
param([Parameter(ValueFromRemainingArguments)][string[]]$Files)

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
    $out = & $godot --headless --path $project --script "res://$($Matches[1])" 2>&1 | ForEach-Object { "$_" }
    $out | Write-Output
    if ($LASTEXITCODE -ne 0 -or ($out -match 'SCRIPT ERROR|Failed to load script|Parse Error')) {
        Write-Output "FAIL: $rel (exit code $LASTEXITCODE or a script error above)"
        $failed++
    }
}
exit $failed
