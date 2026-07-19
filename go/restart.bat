@echo off
cd /d "%~dp0"

rem --- Restart lovegw daemon ---

if not exist "lovegw.exe" (
    echo lovegw.exe not found. Build:  go build -o lovegw.exe ./cmd/lovegw
    echo.
    pause
    exit /b 1
)

echo Stopping lovegw (if running)...
taskkill /IM lovegw.exe /F >nul 2>&1
ping -n 3 127.0.0.1 >nul

echo Starting lovegw...
powershell -NoProfile -Command "Start-Process cmd -WindowStyle Hidden -WorkingDirectory '%~dp0' -ArgumentList '/c','.\lovegw.exe run > run.log 2>&1'"
ping -n 3 127.0.0.1 >nul

tasklist /fi "imagename eq lovegw.exe" 2>nul | find /i "lovegw.exe" >nul
if errorlevel 1 (
    echo WARNING: lovegw did not start. Check run.log
) else (
    echo lovegw restarted. Logs: run.log
)
echo.
pause
