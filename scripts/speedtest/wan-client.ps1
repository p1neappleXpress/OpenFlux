param([string]$Core = "bin/openflux-base.exe", [string]$Remote = "${SPEED_HOST}:47301", [int]$Socks = 11080, [string]$Tag = "wan", [string]$EnvStr = "", [string]$Extra = "")
Set-Location $PSScriptRoot
$pf = "$Tag.pid"
if (Test-Path $pf) { Stop-Process -Id (Get-Content $pf) -Force -ErrorAction SilentlyContinue; Remove-Item $pf }
foreach ($kv in ($EnvStr -split ";" | Where-Object { $_ })) { $k, $v = $kv -split "=", 2; Set-Item -Path "env:$k" -Value $v }
$a = @('--role=client', '--inbound=socks5', "--socks5=127.0.0.1:$Socks", '--transports=direct:100', "--direct-dial=$Remote", '--encryption-key-file=key.txt', "--cookie-store=cookies-$Tag.json")
if ($Extra) { $a += ($Extra -split " ") }
$p = Start-Process -FilePath (Join-Path $PSScriptRoot $Core) -ArgumentList $a -RedirectStandardOutput "$Tag-client.out" -RedirectStandardError "$Tag-client.err" -PassThru -WindowStyle Hidden
$p.Id | Set-Content $pf
Start-Sleep 6
Get-Content "$Tag-client.err" -Tail 2
