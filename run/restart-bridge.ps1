$dir = Split-Path $PSScriptRoot -Parent
Get-Process -Name 'llm-bridge' -ErrorAction SilentlyContinue | Stop-Process -Force
# Orphaned `devin -p` children leak when the bridge dies or is killed. Match
# the bridge's own spawn signature (--respect-workspace-trust), NOT session:
# the Paseo ACP agent is also devin.exe in session 0 and a broad filter kills it.
Get-CimInstance Win32_Process -Filter "Name='devin.exe'" |
  Where-Object { $_.CommandLine -match '--respect-workspace-trust' } |
  ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Start-Sleep -Milliseconds 500
& powershell -NoProfile -ExecutionPolicy Bypass -File "$dir\run\start-bridge.ps1"
