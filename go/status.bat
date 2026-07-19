@echo off
cd /d "%~dp0"

rem --- lovegw daemon status ---

tasklist /fi "imagename eq lovegw.exe" 2>nul | find /i "lovegw.exe" >nul
if errorlevel 1 (
    echo lovegw: STOPPED
) else (
    echo lovegw: RUNNING
    echo.
    tasklist /fi "imagename eq lovegw.exe"
)
echo.
pause
