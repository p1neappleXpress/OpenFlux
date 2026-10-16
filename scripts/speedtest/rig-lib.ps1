# Local WAN-emulating rig: http server <- exit <- delay proxy <- client (SOCKS5 on 11081)
function Stop-Rig([string]$Tag = "rig") {
  $pf = Join-Path $PSScriptRoot "$Tag.pids"
  if (Test-Path $pf) {
    Get-Content $pf | ForEach-Object { Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue }
    Remove-Item $pf
  }
}
function Start-Rig([string]$Core, [string]$Bench = "bin/speedbench-base.exe", [double]$DelayMs = 35, [int]$RateKB = 0, [int]$QueueKB = 0, [double]$StallHz = 0, [double]$StallMs = 90,
                   [string]$Tag = "rig", [string]$EnvStr = "", [string]$ExtraExit = "", [string]$ExtraClient = "") {
  Stop-Rig $Tag
  foreach ($kv in ($EnvStr -split ";" | Where-Object { $_ })) { $k, $v = $kv -split "=", 2; Set-Item -Path "env:$k" -Value $v }
  $ids = @()
  $run = {
    param($exe, $argv, $log)
    $p = Start-Process -FilePath (Join-Path $PSScriptRoot $exe) -ArgumentList $argv -WorkingDirectory $PSScriptRoot `
      -RedirectStandardOutput (Join-Path $PSScriptRoot "$log.out") -RedirectStandardError (Join-Path $PSScriptRoot "$log.err") -PassThru -WindowStyle Hidden
    return $p.Id
  }
  $ids += & $run $Bench @('-mode=http', '-addr=0.0.0.0:39303', '-key-file=key.txt') "$Tag-http"
  $exitArgs = @('--role=exit', '--mode=l4', '--transports=direct:100', '--direct-listen=127.0.0.1:39301', '--encryption-key-file=key.txt', '--cookie-store=cookies-rig-exit.json')
  if ($ExtraExit) { $exitArgs += ($ExtraExit -split " ") }
  $ids += & $run $Core $exitArgs "$Tag-exit"
  $ids += & $run $Bench @('-mode=delay', '-addr=127.0.0.1:39302', '-to=127.0.0.1:39301', '-key-file=key.txt', "-delay-ms=$DelayMs", "-rate-kb=$RateKB", "-queue-kb=$QueueKB", "-stall-hz=$StallHz", "-stall-ms=$StallMs") "$Tag-delay"
  Start-Sleep 3
  $cliArgs = @('--role=client', '--inbound=socks5', '--socks5=127.0.0.1:11081', '--transports=direct:100', '--direct-dial=127.0.0.1:39302', '--encryption-key-file=key.txt', '--cookie-store=cookies-rig-client.json')
  if ($ExtraClient) { $cliArgs += ($ExtraClient -split " ") }
  $ids += & $run $Core $cliArgs "$Tag-client"
  $ids | Set-Content (Join-Path $PSScriptRoot "$Tag.pids")
  Start-Sleep 5
}
