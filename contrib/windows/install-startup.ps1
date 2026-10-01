$ErrorActionPreference = 'Stop'
$startup = [Environment]::GetFolderPath('Startup')
$shortcutPath = Join-Path $startup 'winpipe.lnk'
if (Test-Path $shortcutPath) {
    throw "Startup entry already exists; inspect it before replacing: $shortcutPath"
}
$launcher = Join-Path $PSScriptRoot 'start-winpipe.ps1'
$shell = New-Object -ComObject WScript.Shell
$shortcut = $shell.CreateShortcut($shortcutPath)
$shortcut.TargetPath = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
$shortcut.Arguments = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "{0}"' -f $launcher
$shortcut.WorkingDirectory = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$shortcut.WindowStyle = 7
$shortcut.Description = 'winpipe Windows loopback TCP listener: 9223 to 9222'
$shortcut.Save()
Write-Output "Installed startup shortcut: $shortcutPath"
