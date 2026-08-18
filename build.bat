@echo off
rem TeaRTC screen share one-click build: exe + setup installer
rem NOTE: keep this file ASCII-only (cmd parses batch files in the system codepage)
setlocal
cd /d "%~dp0"

echo [1/3] Building TeaRTCShare.exe ...
go build -o TeaRTCShare.exe .
if errorlevel 1 (
    echo BUILD FAILED: go build
    exit /b 1
)

echo [2/3] Packing TeaRTCShare.zip (green version: exe + README) ...
powershell -NoProfile -Command "Compress-Archive -Path TeaRTCShare.exe,README.md -DestinationPath build\TeaRTCShare.zip -Force"
if errorlevel 1 (
    echo BUILD FAILED: zip
    exit /b 1
)

echo [3/3] Building installer ...
set "ISCC=%LOCALAPPDATA%\Programs\Inno Setup 6\ISCC.exe"
if not exist "%ISCC%" set "ISCC=C:\Program Files (x86)\Inno Setup 6\ISCC.exe"
if not exist "%ISCC%" set "ISCC=C:\Program Files\Inno Setup 6\ISCC.exe"
if not exist "%ISCC%" (
    echo ISCC.exe not found. Install Inno Setup 6 first.
    exit /b 1
)
"%ISCC%" installer\installer.iss
if errorlevel 1 (
    echo BUILD FAILED: installer
    exit /b 1
)

echo.
echo Build OK:
echo   exe:   TeaRTCShare.exe
echo   zip:   build\TeaRTCShare.zip
echo   setup: build\TeaRTCShare-Setup.exe
endlocal
