$ErrorActionPreference = 'Stop'
$dir = $PSScriptRoot
# Order matters: 'question' must precede 'tg-empty' so the reply cache is warm.
foreach ($name in 'classify', 'question', 'tg-empty', 'telegram') {
  $json = Get-Content "$dir\fixture-$name.json" -Raw -Encoding UTF8
  $sw = [Diagnostics.Stopwatch]::StartNew()
  $r = Invoke-RestMethod -Uri 'http://127.0.0.1:8765/v1/chat/completions' -Method Post `
    -Body ([Text.Encoding]::UTF8.GetBytes($json)) -ContentType 'application/json'
  $sw.Stop()
  "{0} elapsed={1}ms reply={2}" -f $name.ToUpper(), $sw.ElapsedMilliseconds, $r.choices[0].message.content
}
"--- bridge.log ---"
Get-Content "$dir\..\bridge.log" -Tail 10 -Encoding UTF8
