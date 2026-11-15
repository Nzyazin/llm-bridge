$dir = Split-Path $PSScriptRoot -Parent
$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument ('-NoProfile -ExecutionPolicy Bypass -File "' + $dir + '\run\start-bridge.ps1"')
$trigger = New-ScheduledTaskTrigger -AtLogOn
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Days 3650)
Register-ScheduledTask -TaskName 'llm-bridge' -Action $action -Trigger $trigger -Settings $settings -Force
Get-ScheduledTask -TaskName 'llm-bridge' | Format-List TaskName, State
