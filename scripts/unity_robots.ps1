# Runs the built Unity robots headless (no window, no GPU), talking MQTT to fleetd.
#   powershell -ExecutionPolicy Bypass -File scripts\unity_robots.ps1 [-Robots 2] [-HostId unity-1]
#       [-Broker 127.0.0.1:1883] [-Prefix unity] [-Floor 1] [-Origin "0,-3"] [-Spacing 2] [-Speed 0.8]
#       [-Battery 100] [-CanRide true|false] [-ExitFailFloor F [-ExitFailCount N]] [-DoorMoveS s]
#       [-IgnoreTurns robot:n,...] [-Window] [-Background]
# The robots ride elevators like the ROS mocks (docs/ELEVATOR.md); -CanRide false
# makes them report can_ride:false instead. -ExitFailFloor/-ExitFailCount/-DoorMoveS/
# -IgnoreTurns are failure injection, as mock_fleet.py's and the adapter's flags.
# -Window shows the scene instead of running headless. -Background returns at once
# and prints the process id; stop it with Stop-Process (the broker then sends the LWT).
param(
    [int]$Robots = 2,
    [string]$HostId = 'unity-1',
    [string]$Broker = '127.0.0.1:1883',
    [string]$Prefix = 'unity',
    [int]$Floor = 1,
    [string]$Origin = '0,-3',
    [double]$Spacing = 2,
    [double]$Speed = 0.8,
    [double]$Battery = 100,
    [ValidateSet('true', 'false')][string]$CanRide = 'true',
    [int]$ExitFailFloor = 0,
    [int]$ExitFailCount = -1,
    [double]$DoorMoveS = 0,
    [string]$IgnoreTurns = '',
    [switch]$Window,
    [switch]$Background
)
$ErrorActionPreference = 'Stop'
$project = (Resolve-Path (Join-Path $PSScriptRoot '..\unity\FleetUnity')).Path
$exe = Join-Path $project 'Build\FleetRobots.exe'
if (-not (Test-Path $exe)) { throw "$exe not found; run scripts\unity_build.ps1 first" }
$log = Join-Path $project "Logs\robots-$HostId.log"
New-Item -ItemType Directory -Force (Split-Path $log) | Out-Null

$inv = [Globalization.CultureInfo]::InvariantCulture
$argv = @()
if (-not $Window) { $argv += '-batchmode', '-nographics' }
$argv += '-logFile', "`"$log`"", '-broker', $Broker, '-host', $HostId, '-robots', $Robots,
    '-prefix', $Prefix, '-floor', $Floor, '-origin', $Origin,
    '-spacing', $Spacing.ToString($inv), '-speed', $Speed.ToString($inv), '-battery', $Battery.ToString($inv),
    '-canRide', $CanRide
if ($ExitFailFloor -ne 0) { $argv += '-exitFailFloor', $ExitFailFloor, '-exitFailCount', $ExitFailCount }
if ($DoorMoveS -gt 0) { $argv += '-doorMoveS', $DoorMoveS.ToString($inv) }
if ($IgnoreTurns) { $argv += '-ignoreTurns', $IgnoreTurns }

$p = Start-Process -FilePath $exe -ArgumentList $argv -PassThru
Write-Host "FleetRobots pid $($p.Id): $Robots robots ${Prefix}_1..$Robots, host $HostId, broker $Broker, log $log"
if ($Background) { return }
try { $p.WaitForExit() } finally { if (-not $p.HasExited) { Stop-Process -Id $p.Id } }
