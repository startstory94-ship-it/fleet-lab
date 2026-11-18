# Builds the Unity players (unity/FleetUnity):
#   robots     unity/FleetUnity/Build/FleetRobots.exe (headless MQTT robots, Main.unity only)
#   dashboard  unity/FleetUnity/Build/Dashboard/FleetDashboard.exe (3D fleet view, Dashboard.unity only)
#   powershell -ExecutionPolicy Bypass -File scripts\unity_build.ps1 [-Target robots|dashboard|all] [-Unity <Unity.exe>] [-Test]
# -Test also runs the EditMode tests first (results in unity/FleetUnity/TestResults.xml).
# Only one editor may open the project at a time, so the steps run one after another.
param(
    [ValidateSet('robots', 'dashboard', 'all')][string]$Target = 'robots',
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

# target -> build method, log, output
$builds = @{
    robots    = @('BuildWindows', 'build.log', 'Build\FleetRobots.exe')
    dashboard = @('BuildDashboard', 'build-dashboard.log', 'Build\Dashboard\FleetDashboard.exe')
}
$names = if ($Target -eq 'all') { @('robots', 'dashboard') } else { @($Target) }
foreach ($name in $names) {
    $method, $logName, $exe = $builds[$name]
    $log = Join-Path $logs $logName
    $p = Start-Process -FilePath $Unity -Wait -PassThru -NoNewWindow -ArgumentList @(
        '-batchmode', '-nographics', '-quit', '-projectPath', "`"$project`"",
        '-executeMethod', "Fleet.EditorTools.BuildScript.$method", '-logFile', "`"$log`"")
    Select-String -Path $log -Pattern '^\[build\]' | ForEach-Object { Write-Host $_.Line }
    if ($p.ExitCode -ne 0) { throw "$name build failed (exit $($p.ExitCode)); see $log" }
    Write-Host "built $project\$exe"
}
