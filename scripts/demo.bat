@echo off
rem Starts a demo you can watch in the browser: fleetd, the mock elevator E1,
rem 3 ROS 2 mock robots on floor 1 (with Wi-Fi roaming gaps), 2 on floor 9,
rem and the adapter, each in its own window.
rem Needs Mosquitto on 127.0.0.1:1883, Go on PATH and the pixi ROS 2 setup
rem in C:\pixi_ws (same as scripts\e2e.bat). Close the windows to stop.
setlocal
set ROOT=%~dp0..
set ROS=call C:\pixi_ws\ros2-windows\local_setup.bat ^>nul 2^>^&1

pushd %ROOT%
echo Building fleetd and mockelevator...
go build -o bin\fleetd.exe .\cmd\fleetd || (popd & exit /b 1)
go build -o bin\mockelevator.exe .\cmd\mockelevator || (popd & exit /b 1)
popd

rem fleetd and the mock elevator use the same default car: E1, floors 1-10,
rem lobby (10, 0) on every floor, 2 s per floor, 2 s doors.
start "mock elevator" /D "%ROOT%" cmd /k bin\mockelevator.exe
start "fleetd" /D "%ROOT%" cmd /k bin\fleetd.exe -http 127.0.0.1:8080 -db bin\demo.db
start "mock robots floor 1" /D C:\pixi_ws cmd /k pixi run cmd /c "%ROS% && python -u %ROOT%\mock_robots\mock_fleet.py --count 3 --origin-x 4 --spacing 1.5 --speed 1.2 --roam-rate 2"
start "mock robots floor 9" /D C:\pixi_ws cmd /k pixi run cmd /c "%ROS% && python -u %ROOT%\mock_robots\mock_fleet.py --count 2 --prefix top --floor 9 --origin-x 4 --spacing 1.5 --speed 1.2"
start "adapter" /D C:\pixi_ws cmd /k pixi run cmd /c "%ROS% && python -u %ROOT%\adapter\fleet_adapter.py --host-id demo"

echo.
echo Dashboard: http://127.0.0.1:8080/
echo   click the map to send a robot there, shift+click to collect waypoints.
echo.
echo Mixed-destination elevator rides (wait until the 5 robots are ONLINE):
echo   powershell -ExecutionPolicy Bypass -File "%ROOT%\scripts\demo_tasks.ps1" up
echo     robot_1..3 on floor 1 go to floors 2, 4, 3 together: one trip, boarding 4, 3, 2
echo     (the floor-2 robot boards last, at the door), stops 2, 3, 4.
echo   powershell -ExecutionPolicy Bypass -File "%ROOT%\scripts\demo_tasks.ps1" down
echo     then: top_1, top_2 on floor 9 go down to floors 8 and 7 together.
echo   Watch the elevator panel (floor, door, door zone, robots by slot, stage) and
echo   pick the car's floor on the map: robots in the car are drawn inside it in purple.
echo Tasks persist in bin\demo.db; delete it for a clean start.
endlocal
