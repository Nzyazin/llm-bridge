$env:HTTPS_PROXY = $null
$env:https_proxy = $null
$env:HTTP_PROXY = $null
$env:http_proxy = $null

Write-Host "=== DIRECT (no proxy) ==="
& curl.exe -s -o NUL -w "hh.ru direct: %{http_code} in %{time_total}s`n" --max-time 10 "https://api.hh.ru/vacancies?per_page=1" -H "User-Agent: Mozilla/5.0"
& curl.exe -s -o NUL -w "tg direct: %{http_code} in %{time_total}s`n" --max-time 10 "https://api.telegram.org"
& curl.exe -s -o NUL -w "google direct: %{http_code} in %{time_total}s`n" --max-time 10 "https://www.google.com"
