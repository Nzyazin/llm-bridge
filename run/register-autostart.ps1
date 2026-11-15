$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument '-NoProfile -ExecutionPolicy Bypass -File "C:\OSPanel\domains\Golang\devin-llm-bridge\run\start-bridge.ps1"'
$trigger = New-ScheduledTaskTrigger -AtLogOn
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Days 3650)
Register-ScheduledTask -TaskName 'devin-llm-bridge' -Action $action -Trigger $trigger -Settings $settings -Force
Get-ScheduledTask -TaskName 'devin-llm-bridge' | Format-List TaskName, State
