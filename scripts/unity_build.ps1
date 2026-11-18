# Builds the Unity robot player (unity/FleetUnity) to unity/FleetUnity/Build/FleetRobots.exe.
#   powershell -ExecutionPolicy Bypass -File scripts\unity_build.ps1 [-Unity <Unity.exe>] [-Test]
# -Test also runs the EditMode tests first (results in unity/FleetUnity/TestResults.xml).
param(
    [string]$Unity = $(if ($env:UNITY_EXE) { $env:UNITY_EXE } else { 'C:\Program Files\Unity\Hub\Editor\6000.6.9f1\Editor\Unity.exe' }),
    [switch]$Test
)
$ErrorActionPreference = 'Stop'
$project = (Resolve-Path (Join-Path $PSScriptRoot '..\unity\FleetUnity')).Path
$logs = Join-Path $project 'Logs'
New-Item -ItemType Directory -Force $logs | Out-Null
if (-not (Test-Path $Unity)) { throw "Unity editor not found: $Unity (pass -Unity or set UNITY_EXE)" }

if ($Test) {
    $xml = Join-Path $project 'TestResults.xml'
    Remove-Item $xml -ErrorAction SilentlyContinue
    # -runTests quits by itself; adding -quit would skip the tests.
    $p = Start-Process -FilePath $Unity -Wait -PassThru -NoNewWindow -ArgumentList @(
        '-batchmode', '-nographics', '-projectPath', "`"$project`"",
        '-runTests', '-testPlatform', 'EditMode', '-testResults', "`"$xml`"", '-logFile', "`"$logs\tests.log`"")
    if (-not (Test-Path $xml)) { throw "no test results; see $logs\tests.log" }
    $run = ([xml](Get-Content $xml)).'test-run'
    Write-Host "EditMode tests: total $($run.total), passed $($run.passed), failed $($run.failed)"
    if ($p.ExitCode -ne 0 -or $run.failed -ne '0') { throw "tests failed (exit $($p.ExitCode))" }
}

$p = Start-Process -FilePath $Unity -Wait -PassThru -NoNewWindow -ArgumentList @(
    '-batchmode', '-nographics', '-quit', '-projectPath', "`"$project`"",
    '-executeMethod', 'Fleet.EditorTools.BuildScript.BuildWindows', '-logFile', "`"$logs\build.log`"")
Select-String -Path "$logs\build.log" -Pattern '^\[build\]' | ForEach-Object { Write-Host $_.Line }
if ($p.ExitCode -ne 0) { throw "build failed (exit $($p.ExitCode)); see $logs\build.log" }
Write-Host "built $project\Build\FleetRobots.exe"
