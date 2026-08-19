@echo off
rem TeaRTC screen share one-click build: exe + setup installer
rem NOTE: keep this file ASCII-only (cmd parses batch files in the system codepage)
setlocal
cd /d "%~dp0"

rem Extract version from web/index.html: const appVer = '0.1.0';
for /f "tokens=2 delims='" %%v in ('findstr /C:"const appVer" web\index.html') do set APPVER=%%v
if not defined APPVER (
    echo BUILD FAILED: version not found in web\index.html
    exit /b 1
)
echo Version: v%APPVER%

echo [1/3] Building TeaRTCShare.exe ...
go build -o TeaRTCShare.exe .
if errorlevel 1 (
    echo BUILD FAILED: go build
    exit /b 1
)

echo [2/3] Packing TeaRTCShare-v%APPVER%.zip (green version: exe + README) ...
powershell -NoProfile -Command "Compress-Archive -Path TeaRTCShare.exe,README.md -DestinationPath build\TeaRTCShare-v%APPVER%.zip -Force"
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
"%ISCC%" /DMyAppVersion=%APPVER% installer\installer.iss
if errorlevel 1 (
    echo BUILD FAILED: installer
    exit /b 1
)

echo.
echo Build OK:
echo   exe:   TeaRTCShare.exe
echo   zip:   build\TeaRTCShare-v%APPVER%.zip
echo   setup: build\TeaRTCShare-Setup-v%APPVER%.exe
endlocal
