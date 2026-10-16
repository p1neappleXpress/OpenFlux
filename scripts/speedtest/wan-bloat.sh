#!/bin/bash
# wan-bloat.sh <exitport> <label> <core> <envs>: small-request latency while a bulk download runs
EP=$1; label=$2; core=$3; envs=$4; SP=11091; H=${SPEED_HOST}:47302; S="--socks5-hostname 127.0.0.1:$SP"
powershell -NoProfile -ExecutionPolicy Bypass -File wan-client.ps1 -Core "$core" -Tag wanbloat -Remote ${SPEED_HOST}:$EP -Socks $SP -EnvStr "$envs" -Extra "$XTRA" >/dev/null
sleep 2
idle=$(for i in 1 2 3; do curl -s -o /dev/null $S -w "%{time_total}\n" http://$H/small; done | sort -n | sed -n 2p)
curl -s -o /dev/null $S --max-time 100 "http://$H/big?mb=48" -w "bulk %{speed_download}\n" > /tmp/bulk.txt &
sleep 4
loaded=$(for i in 1 2 3 4 5; do curl -s -o /dev/null $S --max-time 30 -w "%{time_total}\n" http://$H/small; sleep 0.5; done | sort -n)
wait
echo "$label idle=$(echo $idle | awk '{printf "%.0f",$1*1000}')ms | during bulk: $(echo $loaded | awk '{printf "min=%.0f med=%.0f max=%.0f ms", $1*1000, $3*1000, $5*1000}') | $(cat /tmp/bulk.txt | awk '{printf "bulk=%.2f MB/s",$2/1e6}')"
powershell -NoProfile -Command "if (Test-Path '.\wanbloat.pid') { Stop-Process -Id (Get-Content '.\wanbloat.pid') -Force -ErrorAction SilentlyContinue; Remove-Item '.\wanbloat.pid' }"
