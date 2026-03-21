@echo off
setlocal

set APP_NAME=systray-queue-app
set EXE_NAME=systray-queue-app.exe
set INSTALL_DIR=%LOCALAPPDATA%\%APP_NAME%

echo Installing %APP_NAME%...

:: Check that the exe is next to this script
if not exist "%~dp0%EXE_NAME%" (
    echo ERROR: %EXE_NAME% not found next to install.bat
    echo Make sure both files are in the same folder.
    pause
    exit /b 1
)

:: Create install directory
if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"

:: Copy executable
copy /Y "%~dp0%EXE_NAME%" "%INSTALL_DIR%\%EXE_NAME%" >nul
if errorlevel 1 (
    echo ERROR: Failed to copy executable.
    pause
    exit /b 1
)

:: Create desktop shortcut
powershell -NoProfile -Command ^
  "$s=(New-Object -COM WScript.Shell).CreateShortcut('%USERPROFILE%\Desktop\Queue.lnk');" ^
  "$s.TargetPath='%INSTALL_DIR%\%EXE_NAME%';" ^
  "$s.Description='Task queue app';" ^
  "$s.Save()"

echo.
echo Done! A shortcut was created on your Desktop.
echo.
echo To enable autostart: open the app, then go to Settings and check "Autostart".
echo.

:: Ask to start now
choice /C YN /M "Start the app now?"
if errorlevel 2 goto :end
start "" "%INSTALL_DIR%\%EXE_NAME%"

:end
endlocal
