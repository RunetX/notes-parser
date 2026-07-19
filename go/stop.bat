@echo off
cd /d "%~dp0"

rem --- Stop lovegw daemon ---

tasklist /fi "imagename eq lovegw.exe" 2>nul | find /i "lovegw.exe" >nul
if errorlevel 1 (
    echo lovegw is not running.
    echo.
    pause
    exit /b 0
)

echo Stopping lovegw...
taskkill /IM lovegw.exe /F >nul 2>&1
echo Stopped. State is saved in DB (daemon is crash-safe).
echo.
pause
