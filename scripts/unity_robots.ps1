# Runs the built Unity robots headless (no window, no GPU), talking MQTT to fleetd.
#   powershell -ExecutionPolicy Bypass -File scripts\unity_robots.ps1 [-Robots 2] [-HostId unity-1]
#       [-Broker 127.0.0.1:1883] [-Prefix unity] [-Floor 1] [-Origin "0,-3"] [-Spacing 2] [-Speed 0.8]
#       [-Battery 100] [-Window] [-Background]
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
    '-spacing', $Spacing.ToString($inv), '-speed', $Speed.ToString($inv), '-battery', $Battery.ToString($inv)

$p = Start-Process -FilePath $exe -ArgumentList $argv -PassThru
Write-Host "FleetRobots pid $($p.Id): $Robots robots ${Prefix}_1..$Robots, host $HostId, broker $Broker, log $log"
if ($Background) { return }
try { $p.WaitForExit() } finally { if (-not $p.HasExited) { Stop-Process -Id $p.Id } }
