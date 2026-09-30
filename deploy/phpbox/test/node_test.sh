#!/usr/bin/env bash
# End-to-end test of the page/status/log/stop/"already running" machinery on a local PHP server.
#   deploy/phpbox/test/node_test.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
port=${PORT:-18765}; K=testtoken
tmp=$(mktemp -d); export TMPDIR=$tmp PHPBOX_TOKEN=$K TEST_CAP=8 PHP_CLI_SERVER_WORKERS=6
(cd "$here" && php -S 127.0.0.1:$port >/dev/null 2>&1 &) ; sleep 1
trap 'pkill -f "php -S 127.0.0.1:$port" 2>/dev/null; rm -rf "$tmp"' EXIT
base="http://127.0.0.1:$port/dummyexit.php?k=$K"; T="url=doc1"
fail=0
check() { if [ "$2" = "1" ]; then printf '%-58s OK\n' "$1"; else printf '%-58s FAIL  %s\n' "$1" "${3:-}"; fail=1; fi; }
js() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/dummyexit.php?k=wrong&$T")
check "wrong token -> 404" $([ "$code" = 404 ] && echo 1 || echo 0) "$code"

pg=$(curl -s "$base&a=ping")
check "ping: says what this host can do" $(echo "$pg" | grep -q '"phpbox":"0' && echo "$pg" | grep -q '"missing":\[\]' && echo 1 || echo 0) "$pg"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/dummyexit.php?k=wrong&a=ping")
check "ping: needs the token" $([ "$code" = 404 ] && echo 1 || echo 0) "$code"

page=$(curl -s -H 'Accept: text/html,application/xhtml+xml' -H 'Sec-Fetch-Mode: navigate' "$base&$T")
check "browser navigation -> the status page" $(echo "$page" | grep -q '<title>test · OpenFlux</title>' && echo 1 || echo 0)
check "page carries its config (target, carrier)" $(echo "$page" | grep -q '"carrier":"mailru"' && echo "$page" | grep -q '"target":"doc1"' && echo 1 || echo 0)
check "page hides nothing it should not hold (no PHP errors)" $(echo "$page" | grep -qi 'fatal error\|warning:' && echo 0 || echo 1)

st=$(curl -s "$base&a=status&$T"); check "idle before the first run" $([ "$(echo "$st" | js "d['running']")" = False ] && echo 1 || echo 0) "$st"
st=$(curl -s "$base&a=status"); check "status without a target -> error" $(echo "$st" | grep -q need_target && echo 1 || echo 0)

curl -s -m 12 "$base&a=run&$T" >/tmp/.first.$$ &  # the node (headless, as a pinger would start it)
sleep 2.5
st=$(curl -s "$base&a=status&$T")
check "running after the first request" $([ "$(echo "$st" | js "d['running']")" = True ] && echo 1 || echo 0) "$st"
check "it is run number 1" $([ "$(echo "$st" | js "d['state']['gen']")" = 1 ] && echo 1 || echo 0)
second=$(curl -s -m 5 "$base&a=run&$T")
check "a second open does NOT start another (already running)" $(echo "$second" | grep -q 'already running' && echo 1 || echo 0) "$second"
pg=$(curl -s -H 'Accept: text/html' -H 'Sec-Fetch-Mode: navigate' "$base&$T"); check "the page still renders while it runs" $(echo "$pg" | grep -q '<title>' && echo 1 || echo 0)

lg=$(curl -s "$base&a=log&$T&since=0")
check "log has the start line" $(echo "$lg" | grep -q 'starting on mailru' && echo 1 || echo 0)
check "log has what the carrier echoed" $(echo "$lg" | grep -q 'idle carrier joined doc1' && echo 1 || echo 0)
check "log records the refused second start" $(echo "$lg" | grep -q 'already running' && echo 1 || echo 0)
off=$(echo "$lg" | js "d['next']"); lg2=$(curl -s "$base&a=log&$T&since=$off")
check "log tail resumes from the offset (nothing repeated)" $(echo "$lg2" | grep -q 'starting on mailru' && echo 0 || echo 1)

curl -s "$base&a=stop&$T" >/dev/null; sleep 2.5
st=$(curl -s "$base&a=status&$T")
check "stop ends the node" $([ "$(echo "$st" | js "d['running']")" = False ] && echo 1 || echo 0) "$st"
check "the reason says it was stopped" $(echo "$st" | grep -q 'stopped from the page' && echo 1 || echo 0)

curl -s -m 12 "$base&a=run&$T" >/dev/null & sleep 2.5
st=$(curl -s "$base&a=status&$T"); check "it can be started again (run number 2)" $([ "$(echo "$st" | js "d['state']['gen']")" = 2 ] && echo 1 || echo 0) "$st"
sleep 8
st=$(curl -s "$base&a=status&$T"); check "it ends by itself at the cap" $([ "$(echo "$st" | js "d['running']")" = False ] && echo "$st" | grep -q 'cap' && echo 1 || echo 0) "$st"

out=$(curl -s -m 8 "$base&a=run&url=fail"); check "a node that cannot join reports it" $(echo "$out" | grep -q 'connect failed' && echo 1 || echo 0) "$out"
st=$(curl -s "$base&a=status&url=fail"); check "and is not left 'running'" $([ "$(echo "$st" | js "d['running']")" = False ] && echo 1 || echo 0) "$st"

# ---- self-renewing chain (cap 12s: hands over at ~8s, so a generation every ~8s) ----
export PHPBOX_ALLOW_PRIVATE=1
pkill -f "php -S 127.0.0.1:$port" 2>/dev/null; sleep 0.5
(cd "$here" && PHPBOX_ALLOW_PRIVATE=1 php -S 127.0.0.1:$port >/dev/null 2>&1 &) ; sleep 1
C="url=chain1"
curl -s -m 30 "$base&a=run&chain=1&cap=12&$C" >/dev/null &
sleep 27
st=$(curl -s "$base&a=status&$C"); gen=$(echo "$st" | js "d['state']['gen']")
check "chain: still running after 27s (cap is 12s)" $([ "$(echo "$st" | js "d['running']")" = True ] && echo 1 || echo 0) "$st"
check "chain: several generations took over (gen >= 3)" $([ "${gen:-0}" -ge 3 ] && echo 1 || echo 0) "gen=$gen"
check "chain: status reports continuous mode" $([ "$(echo "$st" | js "d['chain']")" = True ] && echo 1 || echo 0)
lg=$(curl -s "$base&a=log&$C&since=0")
check "chain: log shows the handover" $(echo "$lg" | grep -q 'handed over to gen 2' && echo 1 || echo 0)
check "chain: no generation died or broke the chain" $(echo "$lg" | grep -q 'chain broken\|died' && echo 0 || echo 1)
second=$(curl -s -m 5 "$base&a=run&chain=1&$C"); check "chain: a pinger during the chain just attaches" $(echo "$second" | grep -q 'already running' && echo 1 || echo 0) "$second"
curl -s "$base&a=stop&$C" >/dev/null; sleep 3.5
st=$(curl -s "$base&a=status&$C"); g1=$(echo "$st" | js "d['state']['gen']")
check "chain: stop ends the whole chain" $([ "$(echo "$st" | js "d['running']")" = False ] && echo 1 || echo 0) "$st"
sleep 11
st=$(curl -s "$base&a=status&$C"); g2=$(echo "$st" | js "d['state']['gen']")
check "chain: nothing respawns after a stop" $([ "$g1" = "$g2" ] && [ "$(echo "$st" | js "d['running']")" = False ] && echo 1 || echo 0) "$g1 -> $g2"

echo; [ $fail = 0 ] && echo "NODE TEST PASS" || echo "NODE TEST FAIL"; exit $fail
