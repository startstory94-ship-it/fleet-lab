@echo off
rem Starts a demo you can watch in the browser: fleetd, 3 ROS 2 mock robots
rem (with Wi-Fi roaming gaps) and the adapter, each in its own window.
rem Needs Mosquitto on 127.0.0.1:1883, Go on PATH and the pixi ROS 2 setup
rem in C:\pixi_ws (same as scripts\e2e.bat). Close the windows to stop.
setlocal
set ROOT=%~dp0..
set ROS=call C:\pixi_ws\ros2-windows\local_setup.bat ^>nul 2^>^&1

pushd %ROOT%
echo Building fleetd...
go build -o bin\fleetd.exe .\cmd\fleetd || (popd & exit /b 1)
popd

start "fleetd" /D "%ROOT%" cmd /k bin\fleetd.exe -http 127.0.0.1:8080 -db bin\demo.db
start "mock robots" /D C:\pixi_ws cmd /k pixi run cmd /c "%ROS% && python -u %ROOT%\mock_robots\mock_fleet.py --count 3 --roam-rate 2"
start "adapter" /D C:\pixi_ws cmd /k pixi run cmd /c "%ROS% && python -u %ROOT%\adapter\fleet_adapter.py --host-id demo"

echo.
echo Dashboard: http://127.0.0.1:8080/
echo   click the map to send a robot there, shift+click to collect waypoints.
echo Tasks persist in bin\demo.db; delete it for a clean start.
endlocal
