@echo off
setlocal EnableExtensions DisableDelayedExpansion
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0build-windows.ps1" %*
exit /b %ERRORLEVEL%
