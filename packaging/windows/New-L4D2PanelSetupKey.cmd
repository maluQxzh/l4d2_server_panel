@echo off
powershell.exe -NoLogo -NoProfile -Command "Start-Process powershell.exe -Verb RunAs -ArgumentList '-NoExit -NoLogo -NoProfile -ExecutionPolicy Bypass -File ""%~dp0New-L4D2PanelSetupKey.ps1""'"
