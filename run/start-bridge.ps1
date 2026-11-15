$env:TTS_PREFIX = '<speaker voice="zahar" emotion="evil">'
# Classifier backend key: reuse the Z.AI credential opencode already stores.
$authFile = "$env:USERPROFILE\.local\share\opencode\auth.json"
if ((Test-Path $authFile) -and -not $env:CLASSIFIER_API_KEY) {
  $auth = Get-Content $authFile -Raw | ConvertFrom-Json
  if ($auth.zai.key) { $env:CLASSIFIER_API_KEY = $auth.zai.key }
}
# z.ai / opencode.ai reachable directly and faster than via the loaded local proxy
$env:CLASSIFIER_PROXY = 'direct'
$dir = 'C:\OSPanel\domains\Golang\devin-llm-bridge'
$p = Start-Process -FilePath "$dir\devin-llm-bridge.exe" `
    -WorkingDirectory $dir `
    -RedirectStandardOutput "$dir\bridge.log" `
    -RedirectStandardError "$dir\bridge.err" `
    -WindowStyle Hidden -PassThru
$p.Id | Out-File "$dir\run\bridge.pid"
Write-Output "started pid $($p.Id)"
