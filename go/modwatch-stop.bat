@echo off
cd /d "%~dp0"

rem --- Stop moderation watcher ---
rem Hard kill is safe: modwatch writes through to SQLite (WAL), a lost tick only
rem widens the uncertainty window of the next event.

tasklist /fi "imagename eq modwatch.exe" 2>nul | find /i "modwatch.exe" >nul
if errorlevel 1 (
    echo modwatch is not running.
    echo.
    pause
    exit /b 0
)

taskkill /im modwatch.exe /f >nul 2>&1
ping -n 2 127.0.0.1 >nul
tasklist /fi "imagename eq modwatch.exe" 2>nul | find /i "modwatch.exe" >nul
if errorlevel 1 (
    echo Stopped.
) else (
    echo WARNING: modwatch is still running.
)
echo.
pause
