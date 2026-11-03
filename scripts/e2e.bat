@echo off
rem Runs scripts\e2e.py inside the ROS 2 Jazzy pixi environment.
rem Needs Mosquitto running on 127.0.0.1:1883 and Go on PATH.
setlocal
set ROOT=%~dp0..
pushd C:\pixi_ws
pixi run cmd /c "call C:\pixi_ws\ros2-windows\local_setup.bat >nul 2>&1 && python -u %ROOT%\scripts\e2e.py %*"
set RC=%ERRORLEVEL%
popd
exit /b %RC%
