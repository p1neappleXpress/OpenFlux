#!/bin/bash
# start-exit.sh <binary> <port> <logname> [ENV=VAL ...]  - detached l4 direct exit for tests
BIN=$1; PORT=$2; LOG=$3; shift 3
for kv in "$@"; do export "$kv"; done
cd /root
setsid nohup $BIN --role=exit --mode=l4 --transports=direct:100 --direct-listen=0.0.0.0:$PORT --encryption-key-file=/root/speedbench.key --cookie-store=/root/cookies-$LOG.json </dev/null >/tmp/exit-$LOG.log 2>&1 &
sleep 2; ss -ltn | grep ":$PORT" | head -1
