#!/bin/bash
# vps-prof.sh <binary> <tag> : exit+client on loopback, CPU-profile both during a download
BIN=$1; T=$2
P=$((47900 + RANDOM % 90)); SP=$((P+1))
cd /root
OPENFLUX_PPROF=127.0.0.1:$((P+2)) setsid nohup $BIN --role=exit --mode=l4 --transports=direct:100 --direct-listen=127.0.0.1:$P --encryption-key-file=/root/speedbench.key --cookie-store=/root/cookies-vp-$P.json </dev/null >/tmp/vp-exit-$P.log 2>&1 &
EP=$!
OPENFLUX_PPROF=127.0.0.1:$((P+3)) setsid nohup $BIN --role=client --inbound=socks5 --socks5=127.0.0.1:$SP --transports=direct:100 --direct-dial=127.0.0.1:$P --encryption-key-file=/root/speedbench.key --cookie-store=/root/cookies-vpc-$P.json </dev/null >/tmp/vp-client-$P.log 2>&1 &
CP=$!
sleep 6
S="--socks5-hostname 127.0.0.1:$SP"; H=${SPEED_HOST}:47302
curl -s -o /dev/null $S --max-time 100 "http://$H/big?mb=96" & DLP=$!
sleep 4
curl -s -o /root/prof-$T-exit.pb "http://127.0.0.1:$((P+2))/debug/pprof/profile?seconds=20" & P1=$!
curl -s -o /root/prof-$T-client.pb "http://127.0.0.1:$((P+3))/debug/pprof/profile?seconds=20" & P2=$!
wait $DLP $P1 $P2
kill $EP $CP 2>/dev/null
ls -la /root/prof-$T-*.pb
