@echo off
cd /d "%~dp0"

rem --- Moderation watcher status: process, DB contents, recent events ---
rem Usage: modwatch-status.bat [db_path]
rem UTF-8 console: modwatch.exe and its log are UTF-8, cp866 would garble them.
chcp 65001 >nul

set MODWATCH_DB=%1
if "%MODWATCH_DB%"=="" set MODWATCH_DB=H:\DB\modwatch.db

tasklist /fi "imagename eq modwatch.exe" 2>nul | find /i "modwatch.exe" >nul
if errorlevel 1 (
    echo modwatch: STOPPED   ^(modwatch-start.bat to start^)
) else (
    echo modwatch: RUNNING
    echo.
    tasklist /fi "imagename eq modwatch.exe"
)

echo.
if not exist "%MODWATCH_DB%" (
    echo DB not found: %MODWATCH_DB%
    echo.
    pause
    exit /b 0
)

if not exist "modwatch.exe" (
    echo modwatch.exe not found, building...
    go build -o modwatch.exe ./cmd/lovegw
    if errorlevel 1 (
        echo Build failed - DB stats skipped.
        echo.
        pause
        exit /b 1
    )
)

rem Reads only: safe while the watcher is writing (SQLite WAL).
echo --- %MODWATCH_DB% ---
.\modwatch.exe modwatch -db "%MODWATCH_DB%" status

echo.
echo --- moderation events, last 24h ---
.\modwatch.exe modwatch -db "%MODWATCH_DB%" -since 24h -limit 15 -kind note_gone,comment_gone,image_added,comments_closed events

if exist "modwatch.log" (
    echo.
    echo --- modwatch.log, last lines ---
    powershell -NoProfile -Command "[Console]::OutputEncoding=[Text.Encoding]::UTF8; Get-Content 'modwatch.log' -Tail 8 -Encoding UTF8"
)

echo.
pause
