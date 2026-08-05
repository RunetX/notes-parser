@echo off
cd /d "%~dp0"

rem --- Start moderation watcher in background (log -> modwatch.log) ---
rem Separate exe name on purpose: start/stop/status.bat find the daemon by
rem process name, and a second lovegw.exe would confuse them.

set MODWATCH_DB=%1
if "%MODWATCH_DB%"=="" set MODWATCH_DB=H:\DB\modwatch.db

tasklist /fi "imagename eq modwatch.exe" 2>nul | find /i "modwatch.exe" >nul
if not errorlevel 1 (
    echo modwatch is already running. Use modwatch-stop.bat.
    echo.
    pause
    exit /b 0
)

echo Building modwatch.exe...
go build -o modwatch.exe ./cmd/lovegw
if errorlevel 1 (
    echo Build failed.
    echo.
    pause
    exit /b 1
)

echo Starting modwatch (db: %MODWATCH_DB%)...
powershell -NoProfile -Command "Start-Process cmd -WindowStyle Hidden -WorkingDirectory '%~dp0' -ArgumentList '/c','.\modwatch.exe modwatch -db \"%MODWATCH_DB%\" watch > modwatch.log 2>&1'"

ping -n 3 127.0.0.1 >nul
tasklist /fi "imagename eq modwatch.exe" 2>nul | find /i "modwatch.exe" >nul
if errorlevel 1 (
    echo WARNING: modwatch did not start. Check modwatch.log
) else (
    echo Done. modwatch is running in background. Logs: modwatch.log
)
echo.
pause
