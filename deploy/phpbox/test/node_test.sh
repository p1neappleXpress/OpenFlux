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

# ---- a successor writes nothing: its request is closed by the generation that started it, and on hosts where
# ignore_user_abort does not hold, the first write to a closed connection would end it (the tunnel would not renew) ----
S="url=succ1"
body=$(curl -s -m 6 "$base&a=run&succ=1&from=0&chain=1&cap=30&$S")
check "successor: the response body stays empty" $([ -z "$body" ] && echo 1 || echo 0) "$body"
st=$(curl -s "$base&a=status&$S")
check "successor: it is running" $([ "$(echo "$st" | js "d['running']")" = True ] && echo 1 || echo 0) "$st"
lg=$(curl -s "$base&a=log&$S&since=0")
check "successor: what the carrier said is still in the log" $(echo "$lg" | grep -q 'idle carrier joined succ1' && echo 1 || echo 0)
curl -s "$base&a=stop&$S" >/dev/null; sleep 2.5

# ---- a host that died early teaches the next generations (host.json): they plan below what it lived ----
L="url=learn1"; key=$(php -r 'echo substr(sha1("mailru|learn1"), 0, 12);'); sd="$tmp/phpbox-state"; now=$(date +%s)
printf '{"carrier":"mailru","gen":1,"phase":"serving","started":%d,"beat":%d,"cap":240,"elapsed":50,"cpu":0.4,"budget":{"wall":240,"cpu":0}}' $((now-100)) $((now-50)) > "$sd/$key.g1.json"
curl -s -m 4 "$base&a=run&chain=1&cap=240&$L" >/dev/null &
sleep 2.5
st=$(curl -s "$base&a=status&$L")
check "learning: one sudden end teaches nothing (a host restart looks the same)" $([ "$(echo "$st" | js "d['state']['budget']['wall']")" = 240 ] && echo 1 || echo 0) "$st"
curl -s "$base&a=stop&$L" >/dev/null; sleep 2.5; rm -f "$sd/$key.stop"
now=$(date +%s)
printf '{"carrier":"mailru","gen":2,"phase":"holding","started":%d,"beat":%d,"cap":240,"elapsed":55,"cpu":0.4,"budget":{"wall":240,"cpu":0}}' $((now-120)) $((now-65)) > "$sd/$key.g2.json"
curl -s -m 4 "$base&a=run&chain=1&cap=240&$L" >/dev/null &
sleep 2.5
st=$(curl -s "$base&a=status&$L")
check "learning: the next run plans below the age it died at" $([ "$(echo "$st" | js "d['state']['budget']['wall']")" = 40 ] && echo 1 || echo 0) "$st"
check "learning: and hands over before it" $([ "$(echo "$st" | js "d['state']['spawn_at'] < 40")" = True ] && echo 1 || echo 0) "$st"
lg=$(curl -s "$base&a=log&$L&since=0")
check "learning: the log says what was learned" $(echo "$lg" | grep -q 'learned: the host ends a request after about 50s' && echo 1 || echo 0)
check "learning: host.json holds it" $(grep -q '"wall":50' "$sd/host.json" && echo 1 || echo 0) "$(cat "$sd/host.json" 2>/dev/null)"
curl -s "$base&a=stop&$L" >/dev/null; sleep 2.5

# ---- a host that took the optional functions away (PHP 8: calling a disabled function is a fatal error) ----
p2=$((port+1))
(cd "$here" && php -d disable_functions=set_time_limit,ignore_user_abort,getmypid,getenv,ini_set,putenv,sleep -S 127.0.0.1:$p2 >/dev/null 2>&1 &); sleep 1
b2="http://127.0.0.1:$p2/dummyexit.php?k=CHANGE-ME"     # getenv is gone too: the default token is what is left
pg=$(curl -s "$b2&a=ping")
check "disabled: ping names what the host took away" $(echo "$pg" | grep -q '"disabled":\["ignore_user_abort","set_time_limit","getmypid","getenv","ini_set"\]' && echo 1 || echo 0) "$pg"
curl -s -m 4 "$b2&a=run&url=dis1" >/dev/null &
sleep 2.5
st=$(curl -s "$b2&a=status&url=dis1")
check "disabled: the node still runs" $([ "$(echo "$st" | js "d['running']")" = True ] && echo 1 || echo 0) "$st"
curl -s "$b2&a=stop&url=dis1" >/dev/null; sleep 2.5
st=$(curl -s "$b2&a=status&url=dis1")
check "disabled: and stops" $([ "$(echo "$st" | js "d['running']")" = False ] && echo 1 || echo 0) "$st"
pkill -f "php -d disable_functions=.* -S 127.0.0.1:$p2" 2>/dev/null

echo; [ $fail = 0 ] && echo "NODE TEST PASS" || echo "NODE TEST FAIL"; exit $fail
