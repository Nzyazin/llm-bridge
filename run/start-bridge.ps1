$env:TTS_PREFIX = '<speaker voice="zahar" emotion="evil">'
# Classifier disabled: opencode spawn costs ~20s+ (vs 6s budget) and orphans
# processes on timeout. Classify prompts fall through to devin instead.
$env:CLASSIFIER_ENABLED = 'false'
$env:OPENCODE_MODEL = 'opencode/flint-40b-free'
$env:OPENCODE_VARIANT = 'low'
# Telegram forwarding («отправь в телеграм …»): bot token and target chat id
# live in gitignored run/secrets.ps1 ($env:BOT_TOKEN / $env:BOT_ID).
if (Test-Path "$PSScriptRoot\secrets.ps1") { . "$PSScriptRoot\secrets.ps1" }
$dir = Split-Path $PSScriptRoot -Parent
$p = Start-Process -FilePath "$dir\llm-bridge.exe" `
    -WorkingDirectory $dir `
    -RedirectStandardOutput "$dir\bridge.log" `
    -RedirectStandardError "$dir\bridge.err" `
    -WindowStyle Hidden -PassThru
$p.Id | Out-File "$dir\run\bridge.pid"
Write-Output "started pid $($p.Id)"
