$dir = 'C:\OSPanel\domains\Golang\devin-llm-bridge'
Get-Process -Name 'devin-llm-bridge' -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 500
& powershell -NoProfile -ExecutionPolicy Bypass -File "$dir\run\start-bridge.ps1"
