@echo off
cd /d "%~dp0"

rem --- Start lovegw daemon in background (log -> run.log) ---

tasklist /fi "imagename eq lovegw.exe" 2>nul | find /i "lovegw.exe" >nul
if not errorlevel 1 (
    echo lovegw is already running. Use restart.bat or stop.bat.
    echo.
    pause
    exit /b 0
)

if not exist "lovegw.exe" (
    echo lovegw.exe not found in this folder.
    echo Build it:  go build -o lovegw.exe ./cmd/lovegw
    echo.
    pause
    exit /b 1
)

echo Starting lovegw...
powershell -NoProfile -Command "Start-Process cmd -WindowStyle Hidden -WorkingDirectory '%~dp0' -ArgumentList '/c','.\lovegw.exe run > run.log 2>&1'"

ping -n 3 127.0.0.1 >nul
tasklist /fi "imagename eq lovegw.exe" 2>nul | find /i "lovegw.exe" >nul
if errorlevel 1 (
    echo WARNING: lovegw did not start. Check run.log
) else (
    echo Done. lovegw is running in background. Logs: run.log
)
echo.
pause
