param([string]$Action = "start", [string]$Core = "bin/openflux-base.exe", [double]$DelayMs = 35, [int]$RateKB = 0,
      [int]$QueueKB = 0, [double]$StallHz = 0, [double]$StallMs = 90, [string]$EnvStr = "")
. "$PSScriptRoot/rig-lib.ps1"
if ($Action -eq "stop") { Stop-Rig } else {
  Start-Rig -Core $Core -DelayMs $DelayMs -RateKB $RateKB -QueueKB $QueueKB -StallHz $StallHz -StallMs $StallMs -EnvStr $EnvStr
}
