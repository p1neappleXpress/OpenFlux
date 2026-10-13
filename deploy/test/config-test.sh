#!/bin/sh
# node-install.sh's config, its checks, the node.conf it writes and the .flux
# packages it installs, without a server: sourced without its command
# dispatch. $1: the installer; $2: a scratch directory. provision runs it
# (TestInstallerConfig).
set -u
lib="$2/lib.sh"
sed '/^case "\${1:-}" in/,$d' "$1" > "$lib"
. "$lib"
STATE_ROOT="$2/state"; CONF_ROOT="$2/conf"; NODE_USER=$(id -un)
chown() { :; }   # no node user here

pass=0; failn=0
ok() { pass=$((pass + 1)); }
bad() { failn=$((failn + 1)); printf 'FAIL: %s\n' "$1"; }

b64u() { printf '%s' "$1" | base64 | tr -d '\n=' | tr '/+' '_-'; }
KEY64=$(printf 'ab%.0s' $(seq 32))
PKG=$(printf 'PK-fake-flux' | base64 | tr -d '\n')

config='channel=t1
key='$KEY64'
port=40000
mailru=https://cloud.mail.ru/public/z5iM/XFh98DW3b
telemost=https://telemost.yandex.ru/j/88936358186517
avito=WyJhIiwiYiJd
script=mtslink,'$KEY64','$(b64u "https://my.mts-link.ru/j/a,b")',eyJ4IjoiMSJ9,'$PKG'
script=bitrix,'$KEY64',,,'$PKG'
'
read_config <<EOF
$config
EOF
CHANNEL=t1
( check_transports ) >/dev/null && ok || bad "a good config is refused"

conf=$(write_node_conf)
case "$conf" in *"[Transport telemost]"*"URL = https://telemost.yandex.ru/j/88936358186517"*) ok ;; *) bad "telemost section" ;; esac
case "$conf" in *"[Transport avito]"*"Priority = 75"*"URL = WyJhIiwiYiJd"*) ok ;; *) bad "avito section" ;; esac
case "$conf" in *"[Transport script]"*"Name = mtslink"*"Path = $STATE_ROOT/t1/scripts/mtslink.flux"*"URL = https://my.mts-link.ru/j/a,b"*"Params = eyJ4IjoiMSJ9"*) ok ;; *) bad "first script section: $conf" ;; esac
case "$conf" in *"[Transport script-2]"*"Name = bitrix"*) ok ;; *) bad "second script section" ;; esac
case "$conf" in *"ScriptAutoUpdate = yes"*) ok ;; *) bad "auto-update line" ;; esac
[ "$(session_context)" = "https://cloud.mail.ru/public/z5iM/XFh98DW3b" ] && ok || bad "context: mailru first"
names=$(transport_names)
[ "$names" = "Mail.ru Документ, Яндекс Телемост, Avito, JS: mtslink, JS: bitrix" ] && ok || bad "names: $names"

# The context falls to Telemost, then Avito, then the first script with a parameter.
MAILRU=""; [ "$(session_context)" = "https://telemost.yandex.ru/j/88936358186517" ] && ok || bad "context: telemost"
TELEMOST=""; [ "$(session_context)" = "WyJhIiwiYiJd" ] && ok || bad "context: avito"
AVITO=""; [ "$(session_context)" = "https://my.mts-link.ru/j/a,b" ] && ok || bad "context: script"

# Packages land in the state directory; a line without one keeps the file there.
mkdir -p "$STATE_ROOT/t1"
install_scripts && [ "$(cat "$STATE_ROOT/t1/scripts/mtslink.flux")" = "PK-fake-flux" ] && ok || bad "install: ${SCRIPT_ERROR:-}"
SCRIPTS='mtslink,'$KEY64',,,
'
install_scripts && ok || bad "a kept package: ${SCRIPT_ERROR:-}"
prune_scripts
[ -f "$STATE_ROOT/t1/scripts/mtslink.flux" ] && [ ! -f "$STATE_ROOT/t1/scripts/bitrix.flux" ] && ok || bad "prune"
SCRIPTS='other,'$KEY64',,,
'
install_scripts && bad "a missing package is accepted" || ok

# Bad lines are refused.
for line in 'BAD,'$KEY64',,,' 'x,nothex,,,' 'x,'$KEY64','$(b64u "a#b")',,' 'x,'$KEY64',,se;t,' 'x,'$KEY64',,,***'; do
    SCRIPTS="$line
"
    ( check_transports ) >/dev/null 2>&1 && bad "accepted: $line" || ok
done
SCRIPTS="x,$KEY64,,,
x,$KEY64,,,
"
( check_transports ) >/dev/null 2>&1 && bad "a package twice" || ok
SCRIPTS=""; TELEMOST="https://example.com/j/1"
( check_transports ) >/dev/null 2>&1 && bad "a bad Telemost link" || ok
TELEMOST=""; AVITO="not a list"
( check_transports ) >/dev/null 2>&1 && bad "a bad Avito list" || ok
printf 'passed %d, failed %d\n' "$pass" "$failn"
[ "$failn" = 0 ]
