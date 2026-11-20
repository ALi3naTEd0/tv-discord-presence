# Installer for Windows: venv + two Scheduled Tasks that start at logon (no console window).
# Untested on real hardware so far; please report issues.
#   powershell -ExecutionPolicy Bypass -File .\install.ps1
$ErrorActionPreference = "Stop"
$Dir  = $PSScriptRoot
$Conf = Join-Path $env:APPDATA "tvpresence"
$Data = Join-Path $env:LOCALAPPDATA "tvpresence"
$Venv = Join-Path $Data "venv"

$py = (Get-Command py -ErrorAction SilentlyContinue)
if ($py) { & py -3 -m venv $Venv } else { & python -m venv $Venv }
& "$Venv\Scripts\python.exe" -c "import sys; sys.exit(sys.version_info < (3, 11))"
if ($LASTEXITCODE -ne 0) { throw "Python 3.11+ required" }
& "$Venv\Scripts\python.exe" -m pip install -q -r "$Dir\requirements.txt"

New-Item -ItemType Directory -Force -Path $Conf | Out-Null
if (-not (Test-Path "$Conf\config.toml")) { Copy-Item "$Dir\config.example.toml" "$Conf\config.toml" }

$pyw = "$Venv\Scripts\pythonw.exe"
$tasks = @{ "tvpresence" = "tvpresence.py"; "tvhistory" = "tvhistory_dashboard.py" }
foreach ($name in $tasks.Keys) {
    $action   = New-ScheduledTaskAction -Execute $pyw -Argument "`"$Dir\$($tasks[$name])`"" -WorkingDirectory $Dir
    $trigger  = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
                -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
    Register-ScheduledTask -TaskName $name -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
    Start-ScheduledTask -TaskName $name
}

Write-Host "Done."
Write-Host "1. Edit $Conf\config.toml (tv_host, client_id)"
Write-Host "2. Put TMDB_API_KEY=... (and PLEX_TOKEN=... if you use Plex) in $Conf\.env"
Write-Host "3. Restart: foreach ($t in 'tvpresence','tvhistory') { Stop-ScheduledTask $t; Start-ScheduledTask $t }"
Write-Host "Logs: $Data\tvpresence.log, $Data\tvhistory.log   Dashboard: http://127.0.0.1:7790"
