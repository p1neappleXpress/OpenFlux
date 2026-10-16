param([string]$Core = "bin/openflux-base.exe", [string]$Label = "base", [int]$Runs = 3, [double]$DelayMs = 35, [int]$RateKB = 0, [int]$QueueKB = 0, [double]$StallHz = 0, [double]$StallMs = 90,
      [int]$DownMB = 256, [int]$UpMB = 64, [string]$EnvStr = "")
Set-Location $PSScriptRoot
. "$PSScriptRoot/rig-lib.ps1"
$ip = "${SPEED_LAN_IP}"; $base = "http://${ip}:39303"; $sp = "127.0.0.1:11081"
if (-not (Test-Path up64.bin)) {
  $b = New-Object byte[] (64MB); (New-Object Random 7).NextBytes($b); [IO.File]::WriteAllBytes("$PSScriptRoot\up64.bin", $b)
}
function Get-CoreCpu($name) {
  $t = 0.0
  Get-Process -Name $name -ErrorAction SilentlyContinue | ForEach-Object { $t += $_.TotalProcessorTime.TotalSeconds }
  return $t
}
function Run-Curl($extra) {
  $a = @("-s", "-o", "NUL", "--socks5-hostname", $sp) + $extra
  return (& curl.exe @a)
}



Stop-Rig
Start-Rig -Core $Core -DelayMs $DelayMs -RateKB $RateKB -QueueKB $QueueKB -StallHz $StallHz -StallMs $StallMs -EnvStr $EnvStr
$coreName = [IO.Path]::GetFileNameWithoutExtension($Core)
"== rig $Label core=$Core delay=${DelayMs}ms one-way (RTT $($DelayMs*2)ms) runs=$Runs env='$EnvStr' =="
for ($r = 1; $r -le $Runs; $r++) {
  $c0 = Get-CoreCpu $coreName
  $w = [Diagnostics.Stopwatch]::StartNew()
  $o = Run-Curl @("-w", "%{speed_download}", "$base/big?mb=$DownMB")
  $dl = [double]$o / 1e6; $c1 = Get-CoreCpu $coreName
  $w.Restart()
  $o = Run-Curl @("-w", "%{speed_upload}", "-X", "POST", "--data-binary", "@up64.bin", "$base/up")
  $ul = [double]$o / 1e6; $c2 = Get-CoreCpu $coreName
  $w.Restart()
  $jobs = 1..8 | ForEach-Object {
    Start-Process -FilePath curl.exe -ArgumentList @("-s", "-o", "NUL", "--socks5-hostname", $sp, "$base/big?mb=64") -PassThru -WindowStyle Hidden
  }
  $jobs | Wait-Process
  $pt = $w.Elapsed.TotalSeconds; $c3 = Get-CoreCpu $coreName
  $par = (8 * 64) / $pt
  $lat = @()
  1..15 | ForEach-Object { $o = Run-Curl @("-w", "%{time_starttransfer}", "$base/small"); $lat += ([double]$o * 1000) }
  $lat = $lat | Sort-Object
  "run{0}  down {1,6:N1} MB/s  up {2,6:N1} MB/s  par8 {3,6:N1} MB/s | small ttfb p50 {4:N0} ms min {5:N0} max {6:N0} | cpu s/GB: down {7:N1} up {8:N1} par {9:N1}" -f `
    $r, $dl, $ul, $par, $lat[7], $lat[0], $lat[14], (($c1 - $c0) / ($DownMB / 1024)), (($c2 - $c1) / ($UpMB / 1024)), (($c3 - $c2) / 0.5)
}
Stop-Rig
