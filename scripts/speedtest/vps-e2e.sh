#!/bin/bash
# vps-e2e.sh <binary> <label> [ENV=VAL...]: exit+client+curl all on this host over loopback (CPU-bound measure)
BIN=$1; L=$2; shift 2
for kv in "$@"; do export "$kv"; done
P=$((47500 + RANDOM % 400)); SP=$((P+1))
cd /root
setsid nohup $BIN --role=exit --mode=l4 --transports=direct:100 --direct-listen=127.0.0.1:$P --encryption-key-file=/root/speedbench.key --cookie-store=/root/cookies-ve-$P.json </dev/null >/tmp/ve-exit-$P.log 2>&1 &
EP=$!
setsid nohup $BIN --role=client --inbound=socks5 --socks5=127.0.0.1:$SP --transports=direct:100 --direct-dial=127.0.0.1:$P --encryption-key-file=/root/speedbench.key --cookie-store=/root/cookies-vc-$P.json </dev/null >/tmp/ve-client-$P.log 2>&1 &
CP=$!
sleep 6
H=${SPEED_HOST}:47302; S="--socks5-hostname 127.0.0.1:$SP"
cpu() { awk '{print ($14+$15)/100}' /proc/$1/stat; }
c0e=$(cpu $EP); c0c=$(cpu $CP)
dl=$(curl -s -o /dev/null $S --max-time 120 -w "%{speed_download}" "http://$H/big?mb=96")
c1e=$(cpu $EP); c1c=$(cpu $CP)
ul=$(curl -s -o /dev/null $S --max-time 120 -X POST --data-binary @/root/up32.bin -w "%{speed_upload}" http://$H/up)
c2e=$(cpu $EP); c2c=$(cpu $CP)
tt=$(for i in 1 2 3 4 5; do curl -s -o /dev/null $S -w "%{time_starttransfer}\n" http://$H/small; done | sort -n | sed -n 3p)
echo "$L vps-loopback: down=$(echo $dl | awk '{printf "%.2f",$1/1e6}') MB/s up=$(echo $ul | awk '{printf "%.2f",$1/1e6}') MB/s ttfb=$(echo $tt | awk '{printf "%.1f",$1*1000}')ms | cpu s: down exit=$(echo "$c1e $c0e" | awk '{printf "%.1f",$1-$2}') client=$(echo "$c1c $c0c" | awk '{printf "%.1f",$1-$2}') (96MB) up exit=$(echo "$c2e $c1e" | awk '{printf "%.1f",$1-$2}') client=$(echo "$c2c $c1c" | awk '{printf "%.1f",$1-$2}') (32MB)"
if awk -v d="$dl" "BEGIN{exit !(d+0<=0)}"; then echo "--- exit log"; tail -15 /tmp/ve-exit-$P.log; echo "--- client log"; tail -15 /tmp/ve-client-$P.log; fi
kill $EP $CP 2>/dev/null; rm -f /root/cookies-ve-$P.json /root/cookies-vc-$P.json
