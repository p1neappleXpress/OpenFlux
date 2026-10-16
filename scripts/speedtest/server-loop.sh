#!/bin/bash
# Runs every stack client<->server on this host over loopback (no network).
BIN=${BIN:-/root/speedbench-base}; MB=${MB:-64}; LABEL=${LABEL:-base}
port=47310
for stack in tcp direct enc full; do
  port=$((port+1))
  $BIN -mode=server -addr=127.0.0.1:$port -stack=$stack -key-file=/root/speedbench.key >/tmp/sb-srv.log 2>&1 &
  spid=$!
  sleep 1
  $BIN -mode=client -addr=127.0.0.1:$port -stack=$stack -key-file=/root/speedbench.key -mb=$MB -label="$LABEL loopback" -pings=200
  kill $spid 2>/dev/null; wait $spid 2>/dev/null
done
