#!/bin/bash
# compat.sh "label|core|exitport" ...  - integrity (md5 both ways, 4 parallel) per client/exit pair
H=${SPEED_HOST}:47303; SP=11095
EXP_DL=84f1a83d35a4a50d836b76f178d6462e; EXP_UL=984bcba7c35a9120d3d1ae790b7173d5
for v in "$@"; do
  IFS='|' read -r label core ep <<< "$v"
  powershell -NoProfile -ExecutionPolicy Bypass -File wan-client.ps1 -Core "$core" -Tag compat -Remote ${SPEED_HOST}:$ep -Socks $SP >/dev/null
  sleep 2; S="--socks5-hostname 127.0.0.1:$SP"
  d=$(curl -s $S --max-time 150 "http://$H/det?mb=24" | md5sum | cut -d' ' -f1)
  u=$(curl -s $S --max-time 150 -X POST --data-binary @up32b.bin http://$H/up)
  rm -f /tmp/cp.*; for i in 1 2 3 4; do (curl -s $S --max-time 200 "http://$H/det?mb=24" | md5sum | cut -d' ' -f1 > /tmp/cp.$i) & done; wait
  p=$(cat /tmp/cp.* | sort | uniq -c | awk '{printf "%sx%s ", $1, substr($2,1,8)}')
  ok=OK; [ "$d" = "$EXP_DL" ] || ok=BAD-DL; [ "$u" = "$EXP_UL" ] || ok="$ok BAD-UL"; [ "$(cat /tmp/cp.* | grep -c $EXP_DL)" = "4" ] || ok="$ok BAD-PAR"
  echo "$label: $ok  (dl=${d:0:8} ul=${u:0:8} par=$p)"
done
powershell -NoProfile -Command "if (Test-Path '.\compat.pid') { Stop-Process -Id (Get-Content '.\compat.pid') -Force -ErrorAction SilentlyContinue; Remove-Item '.\compat.pid' }"
