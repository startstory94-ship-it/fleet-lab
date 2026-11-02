# Submits the demo's elevator tasks to fleetd (scripts\demo.bat).
#   up    robot_1..3 (floor 1) to floors 2, 4, 3 at once: one trip, boarding 4,3,2, stops 2,3,4
#   down  top_1, top_2 (floor 9) to floors 8 and 7 at once: one trip down, stops 8,7 (run it after "up": idle robots on nearer floors would take these tasks)
# Usage: powershell -ExecutionPolicy Bypass -File scripts\demo_tasks.ps1 up|down [-Api http://127.0.0.1:8080]
param(
  [Parameter(Mandatory = $true)][ValidateSet("up", "down")][string]$Ride,
  [string]$Api = "http://127.0.0.1:8080"
)
$spots = if ($Ride -eq "up") { @(@(4.0, 3.0, 2), @(5.5, 3.0, 4), @(7.0, 3.0, 3)) } else { @(@(4.0, 3.0, 8), @(5.5, 3.0, 7)) }
foreach ($s in $spots) {
  $body = @{ waypoints = @(@{ x = $s[0]; y = $s[1]; floor = $s[2] }) } | ConvertTo-Json -Depth 4 -Compress
  $t = Invoke-RestMethod -Method Post -Uri "$Api/tasks" -ContentType "application/json" -Body $body
  Write-Host ("task {0}: floor {1} ({2}, {3})" -f $t.id, $s[2], $s[0], $s[1])
}
