#!/bin/bash
# wan-var.sh <exitport> <rounds> "label|core|ENV=VAL;ENV2=VAL2" ...   sequential variants against ONE exit (single peer at a time)
EP=$1; R=$2; shift 2
SP=11090; H=${SPEED_HOST}:47302
for round in $(seq 1 $R); do
  for v in "$@"; do
    IFS='|' read -r label core envs <<< "$v"
    powershell -NoProfile -ExecutionPolicy Bypass -File wan-client.ps1 -Core "$core" -Tag wanvar -Remote ${SPEED_HOST}:$EP -Socks $SP -EnvStr "$envs" >/dev/null
    sleep 2
    S="--socks5-hostname 127.0.0.1:$SP"
    tt=$(for i in 1 2 3; do curl -s -o /dev/null $S --max-time 30 -w "%{time_starttransfer}\n" http://$H/small; done | sort -n | sed -n 2p)
    ul=$(curl -s -o /dev/null $S --max-time 120 -X POST --data-binary @up32.bin -w "%{speed_upload}" http://$H/up)
    dl=$(curl -s -o /dev/null $S --max-time 120 -w "%{speed_download}" "http://$H/big?mb=24")
    echo "$label round=$round ttfb=$(echo $tt | awk '{printf "%.0f",$1*1000}')ms up=$(echo $ul | awk '{printf "%.2f",$1/1e6}') down=$(echo $dl | awk '{printf "%.2f",$1/1e6}') MB/s"
  done
done
powershell -NoProfile -Command "if (Test-Path '.\wanvar.pid') { Stop-Process -Id (Get-Content '.\wanvar.pid') -Force -ErrorAction SilentlyContinue; Remove-Item '.\wanvar.pid' }"
