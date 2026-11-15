$env:TTS_PREFIX = '<speaker voice="zahar" emotion="evil">'
$dir = 'C:\OSPanel\domains\Golang\devin-llm-bridge'
$p = Start-Process -FilePath "$dir\devin-llm-bridge.exe" `
    -WorkingDirectory $dir `
    -RedirectStandardOutput "$dir\bridge.log" `
    -RedirectStandardError "$dir\bridge.err" `
    -WindowStyle Hidden -PassThru
$p.Id | Out-File "$dir\run\bridge.pid"
Write-Output "started pid $($p.Id)"
