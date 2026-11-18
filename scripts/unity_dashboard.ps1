# Runs the built 3D fleet dashboard (scripts\unity_build.ps1 -Target dashboard) in a window.
# It only talks to fleetd (/ws/live, /elevators, /tasks, /events), so ROS and Unity robots show alike.
#   powershell -ExecutionPolicy Bypass -File scripts\unity_dashboard.ps1 [-Api http://127.0.0.1:8080]
#       [-Width 1280] [-Height 720] [-FloorHeight 5] [-Follow] [-Focus 2]
#       [-Capture <dir> [-CaptureMs 200] [-CaptureSeconds 60] [-CaptureQuit]] [-Background]
# -Capture saves PNG frames (HUD included) for a GIF. -Background returns at once and prints the process id.
# Keys in the window: 1-9/0 focus a floor, F follow the car, H overview; mouse drag/wheel to orbit, pan, zoom.
param(
    [string]$Api = 'http://127.0.0.1:8080',
    [int]$Width = 1280,
    [int]$Height = 720,
    [double]$FloorHeight = 5,
    [switch]$Follow,
    [int]$Focus = 0,
    [string]$Capture = '',
    [int]$CaptureMs = 200,
    [double]$CaptureSeconds = 60,
    [switch]$CaptureQuit,
    [switch]$Background
)
$ErrorActionPreference = 'Stop'
$project = (Resolve-Path (Join-Path $PSScriptRoot '..\unity\FleetUnity')).Path
$exe = Join-Path $project 'Build\Dashboard\FleetDashboard.exe'
if (-not (Test-Path $exe)) { throw "$exe not found; run scripts\unity_build.ps1 -Target dashboard first" }
$log = Join-Path $project 'Logs\dashboard.log'
New-Item -ItemType Directory -Force (Split-Path $log) | Out-Null

$inv = [Globalization.CultureInfo]::InvariantCulture
$argv = @('-logFile', "`"$log`"", '-screen-fullscreen', '0', '-screen-width', $Width, '-screen-height', $Height,
    '-api', $Api, '-floorHeight', $FloorHeight.ToString($inv))
if ($Follow) { $argv += '-follow' }
if ($Focus -ne 0) { $argv += '-focus', $Focus }
if ($Capture) {
    $dir = [IO.Path]::GetFullPath($Capture)
    $argv += '-capture', "`"$dir`"", '-captureMs', $CaptureMs, '-captureSeconds', $CaptureSeconds.ToString($inv)
    if ($CaptureQuit) { $argv += '-captureQuit' }
}

$p = Start-Process -FilePath $exe -ArgumentList $argv -PassThru
Write-Host "FleetDashboard pid $($p.Id): api $Api, log $log"
if ($Background) { return }
try { $p.WaitForExit() } finally { if (-not $p.HasExited) { Stop-Process -Id $p.Id } }
