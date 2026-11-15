"MY session=$([System.Diagnostics.Process]::GetCurrentProcess().SessionId) pid=$PID"
"--- devin.exe ---"
Get-CimInstance Win32_Process -Filter "Name='devin.exe'" | ForEach-Object {
  "pid={0} ppid={1} sess={2} cmd={3}" -f $_.ProcessId, $_.ParentProcessId, $_.SessionId, $_.CommandLine
}
"--- llm-bridge ---"
Get-CimInstance Win32_Process -Filter "Name='llm-bridge.exe'" | ForEach-Object {
  "pid={0} ppid={1} sess={2} started={3}" -f $_.ProcessId, $_.ParentProcessId, $_.SessionId, $_.CreationDate
}
"--- port 8765 ---"
Get-NetTCPConnection -LocalPort 8765 -State Listen -ErrorAction SilentlyContinue |
  Select-Object LocalAddress, OwningProcess | Format-Table -AutoSize | Out-String
"--- bridge.pid ---"
Get-Content "$PSScriptRoot\bridge.pid" -ErrorAction SilentlyContinue
"--- bridge.err (tail) ---"
Get-Content "$PSScriptRoot\..\bridge.err" -Tail 10 -ErrorAction SilentlyContinue
"--- bridge.log (tail) ---"
Get-Content "$PSScriptRoot\..\bridge.log" -Tail 10 -ErrorAction SilentlyContinue
"--- scheduled tasks ---"
Get-ScheduledTask | Where-Object { $_.TaskName -match 'bridge' } | ForEach-Object {
  "{0} [{1}] => {2} {3}" -f $_.TaskName, $_.State, $_.Actions[0].Execute, $_.Actions[0].Arguments
}
"--- opencode.exe ---"
Get-CimInstance Win32_Process -Filter "Name='opencode.exe'" | ForEach-Object {
  "pid={0} sess={1} cmd={2}" -f $_.ProcessId, $_.SessionId, $_.CommandLine
}
