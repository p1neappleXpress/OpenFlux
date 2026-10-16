#!/bin/bash
BIN=${1:-/root/speedbench-base}
for pid in $(pgrep -x "$(basename $BIN)"); do kill $pid; done
sleep 0.5
for p in "47411 tcp" "47412 direct" "47413 enc" "47414 full"; do
  set -- $p
  setsid nohup $BIN -mode=server -addr=0.0.0.0:$1 -stack=$2 -key-file=/root/speedbench.key </dev/null >/dev/null 2>&1 &
done
sleep 1; ss -ltn | grep 4741
