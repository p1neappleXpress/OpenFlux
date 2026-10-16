#!/bin/bash
# wan-final.sh <rounds> "label|core|exitport" ...  fresh client per sample: ttfb, upload 32MB, download 24MB, 4 parallel x 8MB
R=$1; shift; SP=11092; H=${SPEED_HOST}:47302
for round in $(seq 1 $R); do
  for v in "$@"; do
    IFS='|' read -r label core ep <<< "$v"
    powershell -NoProfile -ExecutionPolicy Bypass -File wan-client.ps1 -Core "$core" -Tag wanfinal -Remote ${SPEED_HOST}:$ep -Socks $SP >/dev/null
    sleep 2; S="--socks5-hostname 127.0.0.1:$SP"
    tt=$(for i in 1 2 3; do curl -s -o /dev/null $S --max-time 30 -w "%{time_starttransfer}\n" http://$H/small; done | sort -n | sed -n 2p)
    ul=$(curl -s -o /dev/null $S --max-time 120 -X POST --data-binary @up32.bin -w "%{speed_upload}" http://$H/up)
    dl=$(curl -s -o /dev/null $S --max-time 120 -w "%{speed_download}" "http://$H/big?mb=24")
    s=$(date +%s.%N); for i in 1 2 3 4; do curl -s -o /dev/null $S --max-time 180 "http://$H/big?mb=8" & done; wait; e=$(date +%s.%N)
    par=$(awk -v s=$s -v e=$e 'BEGIN{printf "%.2f", 4*8*1.048576/(e-s)}')
    echo "$label $(echo $tt | awk '{printf "%.0f",$1*1000}') $(echo $ul | awk '{printf "%.2f",$1/1e6}') $(echo $dl | awk '{printf "%.2f",$1/1e6}') $par"
  done
done | tee /tmp/wanfinal.txt
powershell -NoProfile -Command "if (Test-Path '.\wanfinal.pid') { Stop-Process -Id (Get-Content '.\wanfinal.pid') -Force -ErrorAction SilentlyContinue; Remove-Item '.\wanfinal.pid' }"
echo "=== medians (ttfb ms, up MB/s, down MB/s, par4x8MB MB/s) ==="
for l in $(awk '{print $1}' /tmp/wanfinal.txt | sort -u); do
  for c in 2 3 4 5; do awk -v l=$l -v c=$c '$1==l{print $c}' /tmp/wanfinal.txt | sort -n | awk '{a[NR]=$1} END{printf "%s%s", (NR%2?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2), " "}'; done | sed "s/^/$l: /"; echo
done
