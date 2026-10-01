param(
    [string]$ListenAddress = '127.0.0.1:9223',
    [string]$TargetAddress = '127.0.0.1:9222'
)

$ErrorActionPreference = 'Stop'
$exe = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..\dist\winpipe.exe'))
$logDirectory = Join-Path $env:LOCALAPPDATA 'winpipe'
New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null
$logFile = Join-Path $logDirectory 'listener.log'

if (-not (Test-Path $exe)) {
    throw "Build the Windows executable first: $exe"
}

# Start-Process keeps the listener running after this launcher exits. Its console
# is hidden; the Go program appends diagnostics to a persistent log file.
$arguments = 'serve --listen "{0}" --target "{1}" --log-file "{2}"' -f $ListenAddress, $TargetAddress, $logFile
Start-Process -FilePath $exe -ArgumentList $arguments -WindowStyle Hidden
