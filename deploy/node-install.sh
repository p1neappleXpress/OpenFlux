#!/bin/sh
# Managed by OpenFlux node-install.sh
#
# Installs one OpenFlux exit channel on a Linux VDS. The "Своя нода" wizard
# of the OpenFlux apps downloads this file by a pinned commit, checks
# its SHA-256 and runs it over SSH. Every channel is independent: its own
# transports (a Yandex document, a Mail.ru document, cups.online rooms, and
# always direct as the backup), key, port and systemd instance
# (openflux-node@<channel>). Nothing outside the paths below is touched, so
# existing services (other OpenFlux installs, Docker, VPNs) keep running.
#
#   /opt/openflux-node/bin/            core binaries (from GitHub Releases)
#   /opt/openflux-node/node-install.sh this script, for the updater
#   /etc/openflux-node/<channel>/      node.conf, encryption-key (0640), port,
#                                      firewall; the directory is 0751 so a
#                                      plain user's plan sees the channel and
#                                      its port but not its secrets
#   /var/lib/openflux-node/<channel>/  cookie store (systemd StateDirectory)
#   /etc/systemd/system/openflux-node@.service
#   /etc/systemd/system/openflux-node-update.{service,timer}
#                                      the core updater, when enabled
#
# Usage: node-install.sh probe
#        node-install.sh plan|status              (config on stdin)
#        node-install.sh apply|remove CONFIG_FILE (run as root)
#        node-install.sh remove CHANNEL           (run as root: remove one
#                                                 channel by name)
#        node-install.sh list                     (the channels and their state)
#        node-install.sh uninstall                (run as root: remove every
#                                                 channel and everything this
#                                                 script installed)
#        node-install.sh upgrade                  (run as root: move every
#                                                 channel to this core)
#        node-install.sh set-cookies CONFIG_FILE  (run as root: replace a
#                                                 channel's Yandex login)
#        node-install.sh set-transports CONFIG_FILE (run as root: add or
#                                                 remove the documents of a
#                                                 running channel; key, port
#                                                 and encryption context stay)
#        node-install.sh show CONFIG_FILE         (run as root: a channel's
#                                                 documents, port, state and
#                                                 encryption context)
#        node-install.sh update                   (run as root, by the timer:
#                                                 move every channel to the
#                                                 newest node-v* release)
#        node-install.sh autoupdate on|off        (run as root: the updater)
# The config is "key=value" lines: channel, key, port, the transports
# (vyandex= or its old name url=: a Yandex document; yandex=: a Yandex
# document through the older Yandex Docs transport; boards=: a Yandex board
# open to guests; mailru=: a Mail.ru
# public document; cupsonline=: the packed room list; telemost=: a Yandex
# Telemost conference; avito=: Avito Code's packed room list; script=: a .flux
# JS transport, one line each, "ID,PUBKEY,PARAMETER,SETTINGS,PACKAGE": the
# package id, the author's key (hex), the primary parameter (unpadded
# base64url), the settings its wizard saved, the .flux (base64; empty keeps the
# one on the server)), autoupdate=yes|no,
# and optionally cookies: the channel's Yandex sign-in as the core's cookie store JSON,
# base64-encoded. It goes to /var/lib/openflux-node/<channel>/cookies.json
# (0600, owned by the node user) and is never printed. apply and remove
# take it from a 0600 temp file, which they delete after reading, so that
# stdin stays free for `sudo -S` (a wrong sudo password would otherwise make
# sudo read the config as further password attempts). Secrets never appear
# in arguments, so they stay out of ps and shell history.
# Output is one JSON object on stdout.
#
# Works with any systemd from 219 (CentOS 7) on: no "systemctl --now" or
# "show --value", and the unit runs without StateDirectory= too.

set -u
umask 077

CORE_VERSION="node-v1.7.0"
SHA_amd64="13fd9eed6d970243dde0b95834e42b83fe81cfe5e69a7fd3056e864c66b20c8f"
SHA_arm64="e38e0368e34afd8a0f94780d35008b34a44e1a08bec7a0778f15d29b464e50f9"
SHA_arm="9dbf93a73a8266052e112215ece19586973d1da9db2ded68957f766e0af5d96d"
# The repository this script and its core come from: the core is one of its
# node-v* releases, and the updater follows them (UPDATE_CONF may override
# that with a "repo=owner/name" line).
RELEASE_REPO="p1neappleXpress/OpenFlux"
GITHUB_API="https://api.github.com"
GITHUB_RAW="https://raw.githubusercontent.com"
GITHUB_WEB="https://github.com"
CORE_BASE="$GITHUB_WEB/$RELEASE_REPO/releases/download/$CORE_VERSION"

BIN_DIR="/opt/openflux-node/bin"
CONF_ROOT="/etc/openflux-node"
STATE_ROOT="/var/lib/openflux-node"
UNIT_FILE="/etc/systemd/system/openflux-node@.service"
UPDATE_SERVICE="/etc/systemd/system/openflux-node-update.service"
UPDATE_TIMER="/etc/systemd/system/openflux-node-update.timer"
UPDATE_CONF="$CONF_ROOT/update.conf"
SELF_COPY="/opt/openflux-node/node-install.sh"
NODE_USER="openflux-node"
MARKER="# Managed by OpenFlux node-install.sh"

# ---- output -----------------------------------------------------------------

# fail STEP MESSAGE: prints the error object and exits. Messages are fixed
# strings or validated values, never secrets.
fail() {
    printf '{"ok":false,"step":"%s","error":"%s"}\n' "$1" "$(json_escape "$2")"
    exit 1
}

json_escape() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr '\n\t\r' '   '
}

json_list() {
    first=1
    printf '['
    for item in "$@"; do
        [ "$first" = 1 ] || printf ','
        printf '"%s"' "$(json_escape "$item")"
        first=0
    done
    printf ']'
}

# ---- environment ------------------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

# unit_start / unit_stop UNIT: enable and start, stop and disable, in two
# calls each: "systemctl --now" needs systemd 220.
unit_start() { systemctl enable "$1" >/dev/null 2>&1 && systemctl start "$1" >/dev/null 2>&1; }
unit_stop() {
    systemctl stop "$1" >/dev/null 2>&1
    systemctl disable "$1" >/dev/null 2>&1
    return 0
}

# main_pid UNIT: its main process id, 0 when none ("show --value" needs 230).
main_pid() { systemctl show -p MainPID "$1" 2>/dev/null | sed -n 's/^MainPID=//p'; }

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        armv7l|armv6l|armhf) echo arm ;;
        *) echo "" ;;
    esac
}

core_sha() {
    case "$1" in
        amd64) echo "$SHA_amd64" ;;
        arm64) echo "$SHA_arm64" ;;
        arm) echo "$SHA_arm" ;;
    esac
}

downloader() {
    if have curl; then echo curl; elif have wget; then echo wget; else echo ""; fi
}

fetch() { # URL DEST
    case "$(downloader)" in
        curl) curl -fsSL --retry 3 --connect-timeout 20 -o "$2" "$1" ;;
        wget) wget -q -T 20 -t 3 -O "$2" "$1" ;;
        *) return 1 ;;
    esac
}

sha256_of() {
    if have sha256sum; then sha256sum "$1" | cut -d' ' -f1
    else openssl dgst -sha256 "$1" | sed 's/.*= //'
    fi
}

firewall_kind() {
    if have ufw && ufw status 2>/dev/null | grep -q '^Status: active'; then echo ufw
    elif have firewall-cmd && firewall-cmd --state >/dev/null 2>&1; then echo firewalld
    else echo none
    fi
}

sudo_mode() {
    if [ "$(id -u)" = 0 ]; then echo root
    elif ! have sudo; then echo none
    elif sudo -n true 2>/dev/null; then echo nopasswd
    else echo password
    fi
}

list_channels() {
    [ -d "$CONF_ROOT" ] || return 0
    for d in "$CONF_ROOT"/*/; do
        [ -d "$d" ] && basename "$d"
    done
}

# version_gt A B: whether release tag A (node-vX.Y.Z) is newer than B. An
# empty or malformed B counts as older than anything.
version_gt() {
    printf '%s\n%s\n' "${1#node-v}" "${2#node-v}" | awk -F. '
        NR == 1 { for (i = 1; i <= 3; i++) a[i] = $i + 0; ok = ($0 ~ /^[0-9]+\.[0-9]+\.[0-9]+$/) }
        NR == 2 { for (i = 1; i <= 3; i++) b[i] = $i + 0 }
        END {
            if (!ok) exit 1
            for (i = 1; i <= 3; i++) { if (a[i] > b[i]) exit 0; if (a[i] < b[i]) exit 1 }
            exit 1
        }'
}

# installed_core: the release BIN_DIR/openflux points at, "" if none.
installed_core() {
    target=$(readlink "$BIN_DIR/openflux" 2>/dev/null) || return 0
    case "$target" in
        openflux-node-v*) [ -x "$BIN_DIR/$target" ] && printf '%s' "${target#openflux-}" ;;
    esac
}

# managed_core REPO: installed_core, if this script or the updater put it
# there from REPO's releases (BIN_DIR/.managed lists "repo tag" lines).
# A core from anywhere else counts as none: another repository's release
# numbers, like an old fork's node-v1.4.0, say nothing about REPO's.
managed_core() {
    cur=$(installed_core)
    [ -n "$cur" ] && grep -qsxF "$1 $cur" "$BIN_DIR/.managed" && printf '%s' "$cur"
    return 0
}

# mark_managed REPO TAG: BIN_DIR/openflux-TAG is REPO's release now.
mark_managed() {
    { grep -sv " $2\$" "$BIN_DIR/.managed"; printf '%s %s\n' "$1" "$2"; } > "$BIN_DIR/.managed.new" \
        && mv -f "$BIN_DIR/.managed.new" "$BIN_DIR/.managed"
    # A plain user's plan reads it too.
    chmod 0644 "$BIN_DIR/.managed"
}

# core_to_use: this script's core, or the installed one when the updater has
# already moved the server to a newer release of the same repository (an
# older app must not downgrade every channel).
core_to_use() {
    cur=$(managed_core "$RELEASE_REPO")
    if [ -n "$cur" ] && version_gt "$cur" "$CORE_VERSION"; then printf '%s' "$cur"; else printf '%s' "$CORE_VERSION"; fi
}

autoupdate_on() {
    [ -f "$UPDATE_TIMER" ] && systemctl is-enabled --quiet openflux-node-update.timer 2>/dev/null
}

release_repo() {
    repo=$(sed -n 's/^repo=\([A-Za-z0-9_.-]*\/[A-Za-z0-9_.-]*\)$/\1/p' "$UPDATE_CONF" 2>/dev/null | tail -n 1)
    printf '%s' "${repo:-$RELEASE_REPO}"
}

port_busy() { # PORT
    if have ss; then
        # No -H: older iproute2 lacks it; the first line is the header.
        [ -n "$(ss -ltn "sport = :$1" 2>/dev/null | tail -n +2)" ]
    elif have netstat; then
        netstat -ltn 2>/dev/null | awk '{print $4}' | grep -q "[:.]$1\$"
    else
        return 1
    fi
}

port_claimed() { # PORT: another of our channels is configured for it
    [ -d "$CONF_ROOT" ] || return 1
    grep -qsx "$1" "$CONF_ROOT"/*/port
}

pick_port() {
    seed=$(od -An -N2 -tu2 /dev/urandom | tr -d ' ')
    i=0
    while [ $i -lt 200 ]; do
        p=$(( 20000 + (seed + i * 7919) % 40000 ))
        if ! port_busy "$p" && ! port_claimed "$p"; then echo "$p"; return 0; fi
        i=$((i + 1))
    done
    echo ""
}

# ---- input ------------------------------------------------------------------

CHANNEL=""; URL=""; YANDEX=""; MAILRU=""; BOARDS=""; CUPS=""; KEY=""; PORT=""; COOKIES=""; AUTOUPDATE=""; MODE=""
TELEMOST=""; AVITO=""; SCRIPTS=""
NL='
'

# read_config [FILE]: reads stdin, or FILE and then deletes it.
read_config() {
    if [ $# -gt 0 ]; then
        [ -f "$1" ] || fail input "нет файла конфигурации"
        read_config < "$1"
        rm -f "$1"
        return
    fi
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in
            channel=*) CHANNEL=${line#channel=} ;;
            url=*) URL=${line#url=} ;;
            vyandex=*) URL=${line#vyandex=} ;;
            yandex=*) YANDEX=${line#yandex=} ;;
            mailru=*) MAILRU=${line#mailru=} ;;
            boards=*) BOARDS=${line#boards=} ;;
            cupsonline=*) CUPS=${line#cupsonline=} ;;
            telemost=*) TELEMOST=${line#telemost=} ;;
            avito=*) AVITO=${line#avito=} ;;
            script=*) SCRIPTS="$SCRIPTS${line#script=}$NL" ;;
            key=*) KEY=${line#key=} ;;
            port=*) PORT=${line#port=} ;;
            cookies=*) COOKIES=${line#cookies=} ;;
            autoupdate=*) AUTOUPDATE=${line#autoupdate=} ;;
            mode=*) MODE=${line#mode=} ;;
            "") ;;
            *) fail input "неизвестная строка конфигурации" ;;
        esac
    done
}

valid_channel() { printf '%s' "$1" | grep -Eq '^[a-z0-9][a-z0-9-]{0,30}$'; }
valid_key() { printf '%s' "$1" | grep -Eq '^[0-9a-f]{64}$'; }
valid_url() {
    printf '%s' "$1" | grep -Eq '^https://(docs|disk)\.yandex\.(ru|com|by|kz|uz)/edit/d/[A-Za-z0-9_-]{16,200}$'
}
valid_yandex() {
    printf '%s' "$1" | grep -Eq '^https://(docs|disk)\.yandex\.(ru|com|by|kz|uz)/(edit/d/[A-Za-z0-9_-]{16,200}|i/[A-Za-z0-9_-]{6,64})$'
}
valid_boards() {
    printf '%s' "$1" | grep -Eq '^https://boards\.yandex\.ru/whiteboard/\?hash=[0-9a-f]{32}$'
}
valid_mailru() {
    printf '%s' "$1" | grep -Eq '^https://cloud\.mail\.ru/public/[A-Za-z0-9_-]{2,64}/[A-Za-z0-9_-]{2,128}$'
}
valid_telemost() {
    printf '%s' "$1" | grep -Eq '^https://telemost\.yandex\.ru/j/[0-9]{6,32}$'
}
# The cups.online room list: base64url of a JSON list of room ids.
valid_rooms() { [ ${#1} -ge 8 ] && [ ${#1} -le 4096 ] && printf '%s' "$1" | grep -Eq '^[A-Za-z0-9_-]+$'; }
valid_port() {
    printf '%s' "$1" | grep -Eq '^[0-9]{4,5}$' && [ "$1" -ge 1024 ] && [ "$1" -le 65535 ]
}

# check_transports: validates the chosen transports (plan and apply).
check_transports() {
    [ -z "$URL" ] || valid_url "$URL" || fail input "адрес документа должен быть вида https://docs.yandex.ru/edit/d/..."
    [ -z "$YANDEX" ] || valid_yandex "$YANDEX" || fail input "адрес документа Yandex Docs должен быть вида https://docs.yandex.ru/edit/d/..."
    [ -z "$MAILRU" ] || valid_mailru "$MAILRU" || fail input "ссылка Mail.ru должна быть вида https://cloud.mail.ru/public/..."
    [ -z "$BOARDS" ] || valid_boards "$BOARDS" || fail input "ссылка на доску должна быть вида https://boards.yandex.ru/whiteboard/?hash=..."
    [ -z "$CUPS" ] || valid_rooms "$CUPS" || fail input "неверный список комнат cups.online"
    [ -z "$TELEMOST" ] || valid_telemost "$TELEMOST" || fail input "ссылка на Телемост должна быть вида https://telemost.yandex.ru/j/..."
    [ -z "$AVITO" ] || valid_rooms "$AVITO" || fail input "неверный список комнат Avito"
    check_scripts
    [ -z "$COOKIES" ] || [ -n "$URL" ] || [ -n "$YANDEX" ] || fail input "вход в Яндекс нужен только каналу с документом Яндекса"
    case "$AUTOUPDATE" in ""|yes|no) ;; *) fail input "autoupdate: yes или no" ;; esac
    case "$MODE" in ""|l4|l3) ;; *) fail input "режим ноды: l4 или l3" ;; esac
}

# b64url_decode TEXT: unpadded base64url to what it encodes.
b64url_decode() {
    b=$(printf '%s' "$1" | tr '_-' '/+')
    case $((${#b} % 4)) in 2) b="$b==" ;; 3) b="$b=" ;; esac
    printf '%s' "$b" | base64 -d 2>/dev/null
}

# script_field LINE N: the Nth field of a script= line.
script_field() { printf '%s' "$1" | cut -d, -f"$2"; }

# check_scripts: each script= line is a package id once, the author's key, a
# parameter that fits a node.conf line, settings and a package in base64.
check_scripts() {
    seen=" "
    set -f
    old_ifs=$IFS
    IFS=$NL
    for line in $SCRIPTS; do
        IFS=$old_ifs
        id=$(script_field "$line" 1)
        printf '%s' "$id" | grep -Eq '^[a-z0-9][a-z0-9_-]{0,31}$' || fail input "JS-транспорт: неверный идентификатор пакета"
        case "$seen" in *" $id "*) fail input "JS-транспорт $id выбран дважды" ;; esac
        seen="$seen$id "
        valid_key "$(script_field "$line" 2)" || fail input "JS-транспорт $id: нужен ключ автора (64 hex)"
        param=$(script_field "$line" 3)
        # Lengths by ${#}: a large {m,n} makes some greps blow up compiling the pattern.
        # grep sees no line in an empty value: those are checked by -z.
        [ -z "$param" ] || { [ ${#param} -le 5600 ] && printf '%s' "$param" | grep -Eq '^[A-Za-z0-9_-]+$'; } || fail input "JS-транспорт $id: неверный параметр"
        value=$(b64url_decode "$param")
        case "$value" in *"#"*|*";"*|*"$NL"*|*"$(printf '\r')"*) fail input "JS-транспорт $id: параметр не помещается в node.conf" ;; esac
        settings=$(script_field "$line" 4)
        [ -z "$settings" ] || { [ ${#settings} -le 16384 ] && printf '%s' "$settings" | grep -Eq '^[A-Za-z0-9+/_=-]+$'; } || fail input "JS-транспорт $id: неверные настройки"
        pkg=$(script_field "$line" 5)
        [ -z "$pkg" ] || printf '%s' "$pkg" | grep -Eq '^[A-Za-z0-9+/=]+$' || fail input "JS-транспорт $id: пакет не в base64"
        IFS=$NL
    done
    IFS=$old_ifs
    set +f
}

# script_ids: the packages of the script= lines, one per line, in order.
script_ids() { printf '%s' "$SCRIPTS" | cut -d, -f1; }

# transport_names: the channel's transports for people, primary first.
transport_names() {
    names=""
    [ -n "$URL" ] && names="Яндекс Документ"
    [ -n "$YANDEX" ] && names="${names:+$names, }Yandex Docs"
    [ -n "$MAILRU" ] && names="${names:+$names, }Mail.ru Документ"
    [ -n "$TELEMOST" ] && names="${names:+$names, }Яндекс Телемост"
    [ -n "$BOARDS" ] && names="${names:+$names, }Яндекс Доска"
    [ -n "$AVITO" ] && names="${names:+$names, }Avito"
    [ -n "$CUPS" ] && names="${names:+$names, }cups.online"
    for id in $(script_ids); do names="${names:+$names, }JS: $id"; done
    printf '%s' "$names"
}

# session_context: the encryption context both sides derive, as the core's
# pickSessionContext does: the highest-priority transport's URL, cups.online
# aside. "" leaves node.conf without a URL line (the core's "http://#").
session_context() {
    if [ -n "$URL" ]; then printf '%s' "$URL"
    elif [ -n "$YANDEX" ]; then printf '%s' "$YANDEX"
    elif [ -n "$MAILRU" ]; then printf '%s' "$MAILRU"
    elif [ -n "$TELEMOST" ]; then printf '%s' "$TELEMOST"
    elif [ -n "$BOARDS" ]; then printf '%s' "$BOARDS"
    elif [ -n "$AVITO" ]; then printf '%s' "$AVITO"
    else
        # The scripts share one priority: the first with a parameter, as the core picks.
        for line in $SCRIPTS; do
            value=$(b64url_decode "$(script_field "$line" 3)")
            [ -n "$value" ] && { printf '%s' "$value"; break; }
        done
    fi
}

# write_cookies: decodes COOKIES into the channel's cookie store, which the
# node loads at start. Needs the node user to exist.
write_cookies() {
    printf '%s' "$COOKIES" | grep -Eq '^[A-Za-z0-9+/]+={0,2}$' || return 1
    [ ${#COOKIES} -le 65536 ] || return 1
    dir="$STATE_ROOT/$CHANNEL"
    # umask 077 would make the parent 0700 and lock the node user out of
    # its own state directory (systemd only creates it when missing).
    mkdir -p "$STATE_ROOT" && chmod 0755 "$STATE_ROOT" || return 1
    mkdir -p "$dir" || return 1
    tmp="$dir/.cookies.json.new"
    if have base64; then
        printf '%s' "$COOKIES" | base64 -d > "$tmp" 2>/dev/null || { rm -f "$tmp"; return 1; }
    else
        printf '%s' "$COOKIES" | openssl base64 -d -A > "$tmp" 2>/dev/null || { rm -f "$tmp"; return 1; }
    fi
    head -c 1 "$tmp" | grep -q '{' || { rm -f "$tmp"; return 1; }
    chown "$NODE_USER:$NODE_USER" "$dir" "$tmp" && chmod 0600 "$tmp" && mv -f "$tmp" "$dir/cookies.json"
}

check_channel() {
    [ -n "$CHANNEL" ] || fail input "не указан канал"
    valid_channel "$CHANNEL" || fail input "имя канала: только a-z, 0-9 и дефис, до 31 символа"
}

# ---- commands ---------------------------------------------------------------

cmd_probe() {
    arch=$(detect_arch)
    systemd=false
    [ -d /run/systemd/system ] && have systemctl && systemd=true
    os=""
    [ -r /etc/os-release ] && os=$(. /etc/os-release && printf '%s %s' "${ID:-linux}" "${VERSION_ID:-}")
    autoupdate=false
    autoupdate_on && autoupdate=true
    # shellcheck disable=SC2046
    printf '{"ok":true,"arch":"%s","os":"%s","systemd":%s,"sudo":"%s","downloader":"%s","firewall":"%s","core":"%s","autoupdate":%s,"channels":%s}\n' \
        "$arch" "$(json_escape "$os")" "$systemd" "$(sudo_mode)" "$(downloader)" "$(firewall_kind)" \
        "$(core_to_use)" "$autoupdate" "$(json_list $(list_channels))"
}

# plan: read-only. Tells the app exactly what apply will change.
cmd_plan() {
    read_config
    check_channel
    check_transports
    arch=$(detect_arch)
    [ -n "$arch" ] || fail plan "архитектура $(uname -m) не поддерживается"
    [ -d /run/systemd/system ] && have systemctl || fail plan "на сервере нет systemd"
    [ -n "$(downloader)" ] || fail plan "на сервере нет curl или wget"
    [ -d "$CONF_ROOT/$CHANNEL" ] && fail plan "канал $CHANNEL уже существует на сервере"
    if [ -n "$PORT" ]; then
        valid_port "$PORT" || fail plan "порт должен быть в диапазоне 1024-65535"
        if port_busy "$PORT" || port_claimed "$PORT"; then fail plan "порт $PORT занят"; fi
    else
        PORT=$(pick_port)
        [ -n "$PORT" ] || fail plan "не удалось найти свободный порт"
    fi
    if [ -f "$UNIT_FILE" ] && ! grep -qF "$MARKER" "$UNIT_FILE"; then
        fail plan "$UNIT_FILE создан не мастером OpenFlux, не трогаю его"
    fi

    core=$(core_to_use)
    set --
    id "$NODE_USER" >/dev/null 2>&1 || set -- "$@" "Создать системного пользователя $NODE_USER (без входа и домашней папки)"
    # The installed file counts only if it is this release of RELEASE_REPO
    # (another repository may have a release with the same number).
    if [ "$core" != "$CORE_VERSION" ] || { [ -x "$BIN_DIR/openflux-$core" ] && [ "$(sha256_of "$BIN_DIR/openflux-$core")" = "$(core_sha "$arch")" ]; }; then
        set -- "$@" "Использовать уже установленное ядро OpenFlux $core ($RELEASE_REPO)"
    else
        set -- "$@" "Скачать ядро OpenFlux $core (linux-$arch) из релизов $RELEASE_REPO на GitHub и сверить SHA-256 в $BIN_DIR"
    fi
    set -- "$@" "Создать $CONF_ROOT/$CHANNEL: node.conf и ключ шифрования канала (права 0640)"
    [ -f "$UNIT_FILE" ] || set -- "$@" "Установить шаблон systemd $UNIT_FILE"
    if [ "$MODE" = l3 ]; then
        l3_ready plan
        set -- "$@" "Режим l3: каналу даётся CAP_NET_RAW, а пока он работает, исходящие TCP-RST ядра с адреса сервера отбрасываются правилом iptables. Это касается и других сервисов на этом адресе"
    fi
    [ -n "$SCRIPTS" ] && set -- "$@" "Положить пакеты JS-транспортов в $STATE_ROOT/$CHANNEL/scripts: нода сама обновляет их из каталога транспортов"
    names=$(transport_names)
    if [ -n "$names" ]; then
        set -- "$@" "Запустить openflux-node@$CHANNEL: $names и direct на порту $PORT/tcp (резерв)"
    else
        set -- "$@" "Запустить openflux-node@$CHANNEL: только direct на порту $PORT/tcp"
    fi
    case "$(firewall_kind)" in
        ufw) set -- "$@" "Разрешить входящий $PORT/tcp в ufw" ;;
        firewalld) set -- "$@" "Разрешить входящий $PORT/tcp в firewalld" ;;
    esac
    case "$AUTOUPDATE" in
        yes)
            if autoupdate_on; then
                set -- "$@" "Автообновление ядра уже включено на сервере (openflux-node-update.timer)"
            else
                set -- "$@" "Включить автообновление ядра (openflux-node-update.timer): раз в 6 часов проверять релизы node-v* в $(release_repo) на GitHub, сверять SHA-256, перезапускать каналы и откатываться, если нода не поднялась"
            fi ;;
        no)
            autoupdate_on && set -- "$@" "Выключить автообновление ядра на сервере (для всех каналов)" ;;
    esac

    # shellcheck disable=SC2046
    printf '{"ok":true,"channel":"%s","port":%s,"arch":"%s","core":"%s","actions":%s,"untouched":%s}\n' \
        "$CHANNEL" "$PORT" "$arch" "$core" "$(json_list "$@")" "$(json_list $(list_channels))"
}

# Rollback state for apply: what this run created.
CREATED_USER=0; CREATED_UNIT=0; CREATED_BIN=0; CREATED_CONF=0; CREATED_FW=""; STARTED=0; CREATED_L3=0

rollback() {
    [ "$STARTED" = 1 ] && unit_stop "openflux-node@$CHANNEL"
    case "$CREATED_FW" in
        ufw) ufw delete allow "$PORT/tcp" >/dev/null 2>&1 ;;
        firewalld) firewall-cmd --permanent --remove-port="$PORT/tcp" >/dev/null 2>&1 && firewall-cmd --reload >/dev/null 2>&1 ;;
    esac
    [ "$CREATED_L3" = 1 ] && set_channel_mode "$CHANNEL" l4 && systemctl daemon-reload >/dev/null 2>&1
    [ "$CREATED_CONF" = 1 ] && rm -rf "${CONF_ROOT:?}/$CHANNEL" "${STATE_ROOT:?}/$CHANNEL"
    [ "$CREATED_UNIT" = 1 ] && rm -f "$UNIT_FILE" && systemctl daemon-reload >/dev/null 2>&1
    [ "$CREATED_BIN" = 1 ] && rm -f "$BIN_DIR/openflux-$CORE_VERSION"
    [ "$CREATED_USER" = 1 ] && userdel "$NODE_USER" >/dev/null 2>&1
}

apply_fail() {
    rollback
    fail "$1" "$2"
}

# install_core ARCH: puts this script's core version into BIN_DIR, checked
# against its pinned SHA-256, and points BIN_DIR/openflux at it. Sets
# CREATED_BIN when it downloaded, CORE_ERROR on failure.
CORE_ERROR=""
install_core() {
    mkdir -p "$BIN_DIR" && chmod 0755 /opt/openflux-node "$BIN_DIR"
    # The updater already runs a newer release: keep it.
    [ "$(core_to_use)" = "$CORE_VERSION" ] || return 0
    core="$BIN_DIR/openflux-$CORE_VERSION"
    want=$(core_sha "$1")
    if [ ! -x "$core" ] || [ "$(sha256_of "$core")" != "$want" ]; then
        tmp=$(mktemp "$BIN_DIR/.download.XXXXXX") || { CORE_ERROR="не удалось создать временный файл"; return 1; }
        if ! fetch "$CORE_BASE/openflux-linux-$1" "$tmp"; then
            rm -f "$tmp"
            CORE_ERROR="не удалось скачать ядро с GitHub"
            return 1
        fi
        if [ "$(sha256_of "$tmp")" != "$want" ]; then
            rm -f "$tmp"
            CORE_ERROR="SHA-256 скачанного ядра не совпал, установка остановлена"
            return 1
        fi
        chmod 0755 "$tmp" && mv -f "$tmp" "$core"
        CREATED_BIN=1
    fi
    ln -sfn "openflux-$CORE_VERSION" "$BIN_DIR/openflux"
    mark_managed "$RELEASE_REPO" "$CORE_VERSION"
}

write_unit() {
    cat > "$UNIT_FILE" <<EOF
$MARKER
[Unit]
Description=OpenFlux node channel %i
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$NODE_USER
Group=$NODE_USER
StateDirectory=openflux-node/%i
# apply creates the directory too: systemd before 235 ignores StateDirectory=.
ReadWritePaths=-$STATE_ROOT/%i
WorkingDirectory=$STATE_ROOT/%i
ExecStart=$BIN_DIR/openflux --config $CONF_ROOT/%i/node.conf
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 "$UNIT_FILE"
}

# ---- l3: the node forwards IP packets ------------------------------------
#
# Mode = l3 sends clients' packets out through raw sockets, which needs
# CAP_NET_RAW, and the kernel must not answer the replies with its own RSTs:
# while an l3 channel runs, outbound TCP RSTs from the node's address are
# dropped. That is host-wide for the address (other services on it lose
# their RSTs, nothing else), which is why l4 is the default.

L3_HELPER="/opt/openflux-node/l3-rst.sh"

# l3_dropin_file CHANNEL: the per-channel systemd drop-in that makes it l3.
l3_dropin_file() { printf '/etc/systemd/system/openflux-node@%s.service.d/l3.conf' "$1"; }

# systemd_new_enough: the "+" prefix (a command that runs as root even in a
# sandboxed unit) needs systemd 231.
systemd_new_enough() {
    v=$(systemctl --version 2>/dev/null | sed -n '1s/^systemd \([0-9]*\).*/\1/p')
    [ -n "$v" ] && [ "$v" -ge 231 ]
}

# l3_ready: what l3 needs of the server; the reason on stderr-less failure via fail.
l3_ready() { # STEP
    have iptables || fail "$1" "для режима l3 нужен iptables, а его на сервере нет"
    have ip || fail "$1" "для режима l3 нужна команда ip (iproute2)"
    systemd_new_enough || fail "$1" "для режима l3 нужен systemd 231 или новее"
}

write_l3_helper() {
    mkdir -p "$(dirname "$L3_HELPER")"
    cat > "$L3_HELPER" <<'EOF'
#!/bin/sh
# Managed by OpenFlux node-install.sh: drops the kernel's own TCP RSTs for the
# node's address while an l3 channel runs (the node answers those packets itself).
ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n 1)
[ -n "$ip" ] || exit 0
rule="OUTPUT -p tcp --tcp-flags RST RST -s $ip -j DROP"
case "$1" in
    add) iptables -C $rule 2>/dev/null || iptables -A $rule ;;
    del)
        # Another l3 channel still running keeps the rule.
        for d in /etc/systemd/system/openflux-node@*.service.d/l3.conf; do
            [ -f "$d" ] || continue
            ch=${d#/etc/systemd/system/openflux-node@}; ch=${ch%.service.d/l3.conf}
            [ "$ch" = "$2" ] && continue
            systemctl is-active --quiet "openflux-node@$ch" && exit 0
        done
        iptables -C $rule 2>/dev/null && iptables -D $rule ;;
esac
exit 0
EOF
    chmod 0755 "$L3_HELPER"
}

# set_channel_mode CHANNEL MODE: makes the channel's unit l3 or plain l4 (the caller reloads systemd).
set_channel_mode() {
    f=$(l3_dropin_file "$1")
    if [ "$2" = l3 ]; then
        write_l3_helper
        mkdir -p "$(dirname "$f")"
        cat > "$f" <<EOF
$MARKER
# This channel forwards IP packets (Mode = l3).
[Service]
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW
ExecStartPre=+$L3_HELPER add %i
ExecStopPost=+$L3_HELPER del %i
EOF
        chmod 0644 "$f"
    else
        [ -f "$f" ] && grep -qF "$MARKER" "$f" && rm -f "$f"
        rmdir "$(dirname "$f")" 2>/dev/null
    fi
    return 0
}

# channel_mode CHANNEL: l3 or l4, as the channel's node.conf says.
channel_mode() {
    m=$(sed -n 's/^Mode = //p' "$CONF_ROOT/$1/node.conf" 2>/dev/null | head -n 1)
    [ "$m" = l3 ] && printf l3 || printf l4
}

# script_core FILE: the CORE_VERSION a copy of this script pins.
script_core() { sed -n 's/^CORE_VERSION="\(node-v[0-9.]*\)"$/\1/p' "$1" 2>/dev/null | head -n 1; }

# script_repo FILE: the RELEASE_REPO a copy of this script follows.
script_repo() { sed -n 's/^RELEASE_REPO="\([^"]*\)"$/\1/p' "$1" 2>/dev/null | head -n 1; }

# install_self FILE: makes FILE the script the updater runs, unless the copy
# there follows the same repository and already pins a newer core (the
# updater put a newer release's script). A script from another repository
# always replaces it: the server now runs that repository's core.
install_self() {
    if [ -f "$SELF_COPY" ] && [ "$(script_repo "$SELF_COPY")" = "$(script_repo "$1")" ] &&
        version_gt "$(script_core "$SELF_COPY")" "$(script_core "$1")"; then
        return 0
    fi
    mkdir -p /opt/openflux-node && chmod 0755 /opt/openflux-node || return 1
    tmp=$(mktemp /opt/openflux-node/.node-install.XXXXXX) || return 1
    if cat "$1" > "$tmp" && chmod 0755 "$tmp" && mv -f "$tmp" "$SELF_COPY"; then return 0; fi
    rm -f "$tmp"
    return 1
}

# enable_updater: openflux-node-update.timer runs "update" from the copy of
# this script: shortly after boot or install, then every 6 hours.
enable_updater() {
    install_self "$0" || return 1
    cat > "$UPDATE_SERVICE" <<EOF
$MARKER
[Unit]
Description=Update the OpenFlux node core to the newest node-v* release
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/sh $SELF_COPY update
EOF
    cat > "$UPDATE_TIMER" <<EOF
$MARKER
[Unit]
Description=Check for a newer OpenFlux node core

[Timer]
OnBootSec=15min
OnUnitActiveSec=6h
RandomizedDelaySec=30min

[Install]
WantedBy=timers.target
EOF
    chmod 0644 "$UPDATE_SERVICE" "$UPDATE_TIMER"
    systemctl daemon-reload >/dev/null 2>&1 && unit_start openflux-node-update.timer
}

disable_updater() {
    unit_stop openflux-node-update.timer
    for f in "$UPDATE_TIMER" "$UPDATE_SERVICE"; do
        [ -f "$f" ] && grep -qF "$MARKER" "$f" && rm -f "$f"
    done
    rm -f "$SELF_COPY"
    systemctl daemon-reload >/dev/null 2>&1
}

# apply_autoupdate: AUTOUPDATE yes turns the updater on (and refreshes the
# script it runs), no turns it off; empty leaves it as it is.
apply_autoupdate() {
    case "$AUTOUPDATE" in
        yes) enable_updater ;;
        no) autoupdate_on && disable_updater; return 0 ;;
        *) autoupdate_on && install_self "$0"; return 0 ;;
    esac
}

# write_node_conf: the channel's node.conf on stdout. Priorities and the
# URL line (the encryption context) match what the app puts into the
# channel's link (provision.ShareLink).
write_node_conf() {
    cat <<EOF
# OpenFlux node channel $CHANNEL, written by node-install.sh
Role = exit
Mode = ${MODE:-l4}
EncryptionKeyFile = $CONF_ROOT/$CHANNEL/encryption-key
CookieStore = $STATE_ROOT/$CHANNEL/cookies.json
EOF
    # set-transports keeps the channel's context: links already handed out were made with it.
    if [ "${CONTEXT_KEEP:-}" = 1 ]; then context=$KEPT_CONTEXT; else context=$(session_context); fi
    [ -n "$context" ] && printf 'URL = %s\n' "$context"
    [ -n "$SCRIPTS" ] && printf 'ScriptAutoUpdate = yes\n'
    [ -n "$URL" ] && printf '\n[Transport vyandex]\nType = vyandex\nPriority = 100\nURL = %s\n' "$URL"
    [ -n "$YANDEX" ] && printf '\n[Transport yandex]\nType = yandex\nPriority = 95\nURL = %s\n' "$YANDEX"
    [ -n "$MAILRU" ] && printf '\n[Transport mailru]\nType = mailru\nPriority = 90\nURL = %s\n' "$MAILRU"
    [ -n "$TELEMOST" ] && printf '\n[Transport telemost]\nType = telemost\nPriority = 85\nURL = %s\n' "$TELEMOST"
    [ -n "$BOARDS" ] && printf '\n[Transport boards]\nType = boards\nPriority = 80\nURL = %s\n' "$BOARDS"
    [ -n "$AVITO" ] && printf '\n[Transport avito]\nType = avito\nPriority = 75\nURL = %s\n' "$AVITO"
    [ -n "$CUPS" ] && printf '\n[Transport cupsonline]\nType = cupsonline\nPriority = 70\nURL = %s\n' "$CUPS"
    write_script_sections
    printf '\n[Transport direct]\nType = direct\nPriority = 110\nListen = 0.0.0.0:%s\n' "$PORT"
}

# write_script_sections: a [Transport script], [Transport script-2], ... per
# script= line, named as the apps name them, each running its package from
# the channel's state directory, where the node's updater keeps it current.
write_script_sections() {
    n=0
    set -f
    old_ifs=$IFS
    IFS=$NL
    for line in $SCRIPTS; do
        IFS=$old_ifs
        n=$((n + 1))
        name=script
        [ "$n" -gt 1 ] && name="script-$n"
        id=$(script_field "$line" 1)
        printf '\n[Transport %s]\nType = script\nPriority = 60\nName = %s\nPath = %s\nPubkey = %s\n' \
            "$name" "$id" "$STATE_ROOT/$CHANNEL/scripts/$id.flux" "$(script_field "$line" 2)"
        value=$(b64url_decode "$(script_field "$line" 3)")
        [ -n "$value" ] && printf 'URL = %s\n' "$value"
        settings=$(script_field "$line" 4)
        [ -n "$settings" ] && printf 'Params = %s\n' "$settings"
        IFS=$NL
    done
    IFS=$old_ifs
    set +f
}

# install_scripts: writes the packages the config carries into the channel's
# state directory (owned by the node user, so its updater can replace them).
# A line without a package keeps the file already there. Returns 1 with
# SCRIPT_ERROR set when it cannot.
install_scripts() {
    sdir="$STATE_ROOT/$CHANNEL/scripts"
    [ -n "$SCRIPTS" ] || return 0
    mkdir -p "$sdir" || { SCRIPT_ERROR="не удалось создать $sdir"; return 1; }
    set -f
    old_ifs=$IFS
    IFS=$NL
    for line in $SCRIPTS; do
        IFS=$old_ifs
        id=$(script_field "$line" 1)
        pkg=$(script_field "$line" 5)
        if [ -n "$pkg" ]; then
            if ! printf '%s' "$pkg" | base64 -d > "$sdir/.$id.flux.tmp" 2>/dev/null || [ ! -s "$sdir/.$id.flux.tmp" ]; then
                rm -f "$sdir/.$id.flux.tmp"
                SCRIPT_ERROR="пакет JS-транспорта $id не распаковался"
                IFS=$old_ifs; set +f
                return 1
            fi
            mv -f "$sdir/.$id.flux.tmp" "$sdir/$id.flux"
        elif [ ! -s "$sdir/$id.flux" ]; then
            SCRIPT_ERROR="пакета JS-транспорта $id нет на сервере: передайте его"
            IFS=$old_ifs; set +f
            return 1
        fi
        IFS=$NL
    done
    IFS=$old_ifs
    set +f
    chown -R "$NODE_USER:$NODE_USER" "$sdir" && chmod 0750 "$sdir"
}

# prune_scripts: removes packages the channel no longer runs.
prune_scripts() {
    sdir="$STATE_ROOT/$CHANNEL/scripts"
    [ -d "$sdir" ] || return 0
    keep=" $(script_ids | tr '\n' ' ') "
    for f in "$sdir"/*.flux; do
        [ -e "$f" ] || continue
        id=${f##*/}
        id=${id%.flux}
        case "$keep" in *" $id "*) ;; *) rm -f "$f" ;; esac
    done
}

cmd_apply() {
    [ "$(id -u)" = 0 ] || fail apply "нужны права root (sudo)"
    read_config "$@"
    check_channel
    valid_key "$KEY" || fail input "ключ канала должен быть 64 hex-символа"
    check_transports
    valid_port "$PORT" || fail input "не указан порт из плана"
    [ -z "$COOKIES" ] || printf '%s' "$COOKIES" | grep -Eq '^[A-Za-z0-9+/]+={0,2}$' || fail input "cookies должны быть в base64"
    arch=$(detect_arch)
    [ -n "$arch" ] || fail apply "архитектура $(uname -m) не поддерживается"
    [ -d "$CONF_ROOT/$CHANNEL" ] && fail apply "канал $CHANNEL уже существует на сервере"
    if port_busy "$PORT" || port_claimed "$PORT"; then fail apply "порт $PORT занят"; fi
    if [ -f "$UNIT_FILE" ] && ! grep -qF "$MARKER" "$UNIT_FILE"; then
        fail apply "$UNIT_FILE создан не мастером OpenFlux"
    fi

    if ! id "$NODE_USER" >/dev/null 2>&1; then
        nologin=/usr/sbin/nologin
        [ -x "$nologin" ] || nologin=/sbin/nologin
        [ -x "$nologin" ] || nologin=/bin/false
        if have useradd; then
            useradd --system --no-create-home --home-dir /nonexistent --shell "$nologin" "$NODE_USER" \
                || apply_fail user "не удалось создать пользователя $NODE_USER"
        else
            adduser -S -H -D -s "$nologin" "$NODE_USER" 2>/dev/null \
                || apply_fail user "не удалось создать пользователя $NODE_USER"
        fi
        CREATED_USER=1
    fi

    install_core "$arch" || apply_fail download "$CORE_ERROR"

    mkdir -p "$CONF_ROOT" && chmod 0755 "$CONF_ROOT"
    dir="$CONF_ROOT/$CHANNEL"
    mkdir "$dir" || apply_fail config "не удалось создать $dir"
    CREATED_CONF=1
    printf '%s\n' "$KEY" > "$dir/encryption-key"
    write_node_conf > "$dir/node.conf"
    printf '%s\n' "$PORT" > "$dir/port"
    chown -R "root:$NODE_USER" "$dir"
    chmod 0751 "$dir"
    chmod 0640 "$dir/encryption-key" "$dir/node.conf"
    chmod 0644 "$dir/port"
    # The node's state directory (systemd before 235 does not make it).
    mkdir -p "$STATE_ROOT/$CHANNEL" && chmod 0755 "$STATE_ROOT" && chown "$NODE_USER:$NODE_USER" "$STATE_ROOT/$CHANNEL" \
        && chmod 0750 "$STATE_ROOT/$CHANNEL" || apply_fail config "не удалось создать $STATE_ROOT/$CHANNEL"
    if [ -n "$COOKIES" ]; then
        write_cookies || apply_fail cookies "не удалось сохранить вход в Яндекс на сервере"
    fi
    install_scripts || apply_fail scripts "$SCRIPT_ERROR"

    if [ ! -f "$UNIT_FILE" ]; then
        write_unit
        CREATED_UNIT=1
    fi
    if [ "$MODE" = l3 ]; then
        l3_ready apply
        set_channel_mode "$CHANNEL" l3
        CREATED_L3=1
    fi
    systemctl daemon-reload || apply_fail systemd "systemctl daemon-reload не удался"

    case "$(firewall_kind)" in
        ufw)
            ufw allow "$PORT/tcp" comment "openflux-node $CHANNEL" >/dev/null 2>&1 \
                || apply_fail firewall "не удалось открыть порт в ufw"
            CREATED_FW=ufw ;;
        firewalld)
            { firewall-cmd --permanent --add-port="$PORT/tcp" && firewall-cmd --reload; } >/dev/null 2>&1 \
                || apply_fail firewall "не удалось открыть порт в firewalld"
            CREATED_FW=firewalld ;;
    esac
    [ -n "$CREATED_FW" ] && printf '%s %s\n' "$CREATED_FW" "$PORT" > "$dir/firewall"

    unit_start "openflux-node@$CHANNEL" \
        || apply_fail start "не удалось запустить openflux-node@$CHANNEL"
    STARTED=1
    sleep 4
    if ! systemctl is-active --quiet "openflux-node@$CHANNEL"; then
        logs=$(journalctl -u "openflux-node@$CHANNEL" -n 8 -o cat --no-pager 2>/dev/null | tail -n 8)
        apply_fail start "нода не запустилась: $logs"
    fi
    # The channel runs; the updater is extra, and its failure only shows.
    autoupdate=false
    apply_autoupdate
    autoupdate_on && autoupdate=true
    printf '{"ok":true,"channel":"%s","port":%s,"core":"%s","autoupdate":%s,"mode":"%s"}\n' "$CHANNEL" "$PORT" "$(core_to_use)" "$autoupdate" "${MODE:-l4}"
}

# remove_channel CHANNEL: stops the channel, closes its firewall port and
# deletes its config and state.
remove_channel() {
    unit_stop "openflux-node@$1"
    if [ -f "$(l3_dropin_file "$1")" ]; then
        set_channel_mode "$1" l4
        systemctl daemon-reload >/dev/null 2>&1
    fi
    if [ -f "$CONF_ROOT/$1/firewall" ]; then
        read -r kind port < "$CONF_ROOT/$1/firewall"
        case "$kind" in
            ufw) ufw delete allow "$port/tcp" >/dev/null 2>&1 ;;
            firewalld) firewall-cmd --permanent --remove-port="$port/tcp" >/dev/null 2>&1 && firewall-cmd --reload >/dev/null 2>&1 ;;
        esac
    fi
    rm -rf "${CONF_ROOT:?}/$1" "${STATE_ROOT:?}/$1"
}

# remove_everything: what this script installed besides the channels: the
# updater, the unit template, the cores, the folders and the node user.
# Files that do not carry this script's marker are left alone.
remove_everything() {
    disable_updater
    [ -f "$UNIT_FILE" ] && grep -qF "$MARKER" "$UNIT_FILE" && rm -f "$UNIT_FILE"
    systemctl daemon-reload >/dev/null 2>&1
    systemctl reset-failed 'openflux-node@*' >/dev/null 2>&1
    rm -rf /opt/openflux-node "$CONF_ROOT" "$STATE_ROOT"
    id "$NODE_USER" >/dev/null 2>&1 && userdel "$NODE_USER" >/dev/null 2>&1
    return 0
}

# remove CONFIG_FILE (the apps: channel= in a temp file) or remove CHANNEL
# (by hand on the server).
cmd_remove() {
    [ "$(id -u)" = 0 ] || fail remove "нужны права root (sudo)"
    if [ $# -gt 0 ] && [ ! -f "$1" ] && valid_channel "$1"; then
        CHANNEL=$1
    else
        read_config "$@"
    fi
    check_channel
    [ -d "$CONF_ROOT/$CHANNEL" ] || fail remove "канала $CHANNEL нет на сервере"
    remove_channel "$CHANNEL"
    # The last channel is gone: remove everything this script installed.
    [ -z "$(list_channels)" ] && remove_everything
    printf '{"ok":true,"channel":"%s"}\n' "$CHANNEL"
}

# uninstall: every channel, then everything this script installed. Also
# stops channel instances whose config is already gone.
cmd_uninstall() {
    [ "$(id -u)" = 0 ] || fail uninstall "нужны права root (sudo)"
    set --
    for ch in $(list_channels); do
        remove_channel "$ch"
        set -- "$@" "$ch"
    done
    for unit in $(systemctl list-units --all --plain --no-legend 'openflux-node@*' 2>/dev/null | awk '{print $1}'); do
        unit_stop "$unit"
    done
    remove_everything
    printf '{"ok":true,"removed":%s}\n' "$(json_list "$@")"
}

# list: the channels, their state and port; readable without root.
cmd_list() {
    printf '{"ok":true,"core":"%s","autoupdate":%s,"channels":[' "$(installed_core)" "$(autoupdate_on && echo true || echo false)"
    first=1
    for ch in $(list_channels); do
        [ "$first" = 1 ] || printf ','
        first=0
        port=$(cat "$CONF_ROOT/$ch/port" 2>/dev/null)
        printf '{"channel":"%s","state":"%s","port":%s}' "$ch" "$(json_escape "$(systemctl is-active "openflux-node@$ch" 2>/dev/null)")" "${port:-0}"
    done
    printf ']}\n'
}

# upgrade: switches every channel to this script's core and restarts the
# running ones. Configs, keys and ports stay as they are.
cmd_upgrade() {
    [ "$(id -u)" = 0 ] || fail upgrade "нужны права root (sudo)"
    [ -n "$(list_channels)" ] || fail upgrade "на сервере нет каналов OpenFlux"
    arch=$(detect_arch)
    [ -n "$arch" ] || fail upgrade "архитектура $(uname -m) не поддерживается"
    install_core "$arch" || fail upgrade "$CORE_ERROR"
    set --
    for ch in $(list_channels); do
        if systemctl is-active --quiet "openflux-node@$ch"; then
            systemctl restart "openflux-node@$ch" || fail upgrade "не удалось перезапустить openflux-node@$ch"
            set -- "$@" "$ch"
        fi
    done
    # Older cores nothing points at any more.
    core=$(core_to_use)
    for old in "$BIN_DIR"/openflux-node-v*; do
        [ "$old" = "$BIN_DIR/openflux-$core" ] || rm -f "$old"
    done
    apply_autoupdate
    printf '{"ok":true,"core":"%s","restarted":%s}\n' "$core" "$(json_list "$@")"
}

# latest_release FILE: the newest node-vX.Y.Z tag in a GitHub releases
# listing (JSON in FILE), prereleases left out.
latest_release() {
    best=""
    # Every release has one tag_name and one prerelease, in either order;
    # the objects nested in it (author, assets) have neither.
    for tag in $(tr ',{}' '\n\n\n' < "$1" | awk '
        function pair() {
            if (tag != "" && pre != "") {
                if (pre == "false" && tag ~ /^node-v[0-9]+\.[0-9]+\.[0-9]+$/) print tag
                tag = ""; pre = ""
            }
        }
        /"tag_name"[ \t]*:/ { tag = $0; sub(/.*"tag_name"[ \t]*:[ \t]*"/, "", tag); sub(/".*/, "", tag); pair() }
        /"prerelease"[ \t]*:[ \t]*false/ { pre = "false"; pair() }
        /"prerelease"[ \t]*:[ \t]*true/ { pre = "true"; pair() }'); do
        version_gt "$tag" "$best" && best=$tag
    done
    printf '%s' "$best"
}

# restart_channels: restarts every enabled or running channel and prints
# their names.
restart_channels() {
    for ch in $(list_channels); do
        if systemctl is-active --quiet "openflux-node@$ch" || systemctl is-enabled --quiet "openflux-node@$ch"; then
            systemctl restart "openflux-node@$ch" >/dev/null 2>&1
            printf '%s\n' "$ch"
        fi
    done
}

# channels_stay_up CHANNEL...: whether each channel is running and did not
# restart during 20 seconds (a crashing core restarts every 5).
channels_stay_up() {
    sleep 3
    pids=""
    for ch in "$@"; do pids="$pids $ch=$(main_pid "openflux-node@$ch")"; done
    sleep 20
    for pair in $pids; do
        ch=${pair%%=*}; pid=${pair#*=}
        systemctl is-active --quiet "openflux-node@$ch" || return 1
        [ -n "$pid" ] && [ "$pid" != 0 ] && [ "$pid" = "$(main_pid "openflux-node@$ch")" ] || return 1
    done
}

# update: what openflux-node-update.timer runs. When RELEASE_REPO has a
# node-v* release newer than the installed core, moves every channel to it:
# the release's own node-install.sh (at its tag) pins the core's SHA-256,
# and the release's SHA256SUMS must say the same. If a channel does not stay
# up on the new core, the previous one comes back and that release is
# skipped from then on.
cmd_update() {
    [ "$(id -u)" = 0 ] || fail update "нужны права root"
    if [ -z "$(list_channels)" ]; then
        printf '{"ok":true,"updated":false,"reason":"на сервере нет каналов"}\n'
        return 0
    fi
    arch=$(detect_arch)
    [ -n "$arch" ] || fail update "архитектура $(uname -m) не поддерживается"
    repo=$(release_repo)
    current=$(managed_core "$repo")
    work=$(mktemp -d /tmp/openflux-node-update.XXXXXX) || fail update "не удалось создать временную папку"
    trap 'rm -rf "$work"' EXIT
    fetch "$GITHUB_API/repos/$repo/releases?per_page=30" "$work/releases.json" \
        || fail update "GitHub не ответил на список релизов $repo"
    latest=$(latest_release "$work/releases.json")
    [ -n "$latest" ] || fail update "в $repo нет релизов node-v*"
    skip="$STATE_ROOT/update-skip"
    if [ -n "$current" ] && ! version_gt "$latest" "$current"; then
        printf '{"ok":true,"updated":false,"core":"%s"}\n' "$current"
        return 0
    fi
    if grep -qsx "$latest" "$skip"; then
        printf '{"ok":true,"updated":false,"core":"%s","skipped":"%s"}\n' "$current" "$latest"
        return 0
    fi

    fetch "$GITHUB_RAW/$repo/$latest/deploy/node-install.sh" "$work/node-install.sh" \
        || fail update "не удалось скачать node-install.sh релиза $latest"
    grep -qxF "$MARKER" "$work/node-install.sh" && [ "$(script_core "$work/node-install.sh")" = "$latest" ] \
        || fail update "node-install.sh в $latest не относится к этому релизу"
    want=$(sed -n "s/^SHA_$arch=\"\([0-9a-f]\{64\}\)\"\$/\1/p" "$work/node-install.sh")
    [ -n "$want" ] || fail update "node-install.sh релиза $latest не знает хеш ядра для $arch"
    base="$GITHUB_WEB/$repo/releases/download/$latest"
    fetch "$base/SHA256SUMS" "$work/SHA256SUMS" || fail update "не удалось скачать SHA256SUMS релиза $latest"
    grep -qx "$want  openflux-linux-$arch" "$work/SHA256SUMS" \
        || fail update "SHA256SUMS релиза $latest расходится с его node-install.sh"
    fetch "$base/openflux-linux-$arch" "$work/core" || fail update "не удалось скачать ядро $latest"
    [ "$(sha256_of "$work/core")" = "$want" ] || fail update "SHA-256 скачанного ядра $latest не совпал"

    new="openflux-$latest"
    mkdir -p "$BIN_DIR" && chmod 0755 /opt/openflux-node "$BIN_DIR"
    cp "$work/core" "$BIN_DIR/.$new.new" && chmod 0755 "$BIN_DIR/.$new.new" && mv -f "$BIN_DIR/.$new.new" "$BIN_DIR/$new" \
        || fail update "не удалось записать ядро в $BIN_DIR"
    prev=$(readlink "$BIN_DIR/openflux" 2>/dev/null)
    ln -sfn "$new" "$BIN_DIR/openflux"
    # shellcheck disable=SC2046
    set -- $(restart_channels)
    if [ $# -gt 0 ] && ! channels_stay_up "$@"; then
        if [ -n "$prev" ] && [ -x "$BIN_DIR/$prev" ]; then
            ln -sfn "$prev" "$BIN_DIR/openflux"
            restart_channels >/dev/null
        fi
        rm -f "$BIN_DIR/$new"
        printf '%s\n' "$latest" >> "$skip"
        fail update "на ядре $latest каналы не поднялись, вернул ${prev#openflux-}; этот релиз больше не ставлю"
    fi
    mark_managed "$repo" "$latest"
    install_self "$work/node-install.sh" || true
    # Keep the previous core for a manual rollback, drop the older ones.
    for old in "$BIN_DIR"/openflux-node-v*; do
        case "${old##*/}" in "$new"|"$prev") ;; *) rm -f "$old" ;; esac
    done
    printf '{"ok":true,"updated":true,"core":"%s","previous":"%s","restarted":%s}\n' \
        "$latest" "$(json_escape "${prev#openflux-}")" "$(json_list "$@")"
}

# set-cookies: replaces a channel's Yandex sign-in and restarts it.
cmd_set_cookies() {
    [ "$(id -u)" = 0 ] || fail set-cookies "нужны права root (sudo)"
    read_config "$@"
    check_channel
    [ -d "$CONF_ROOT/$CHANNEL" ] || fail set-cookies "канала $CHANNEL нет на сервере"
    [ -n "$COOKIES" ] || fail set-cookies "нет cookies"
    write_cookies || fail set-cookies "не удалось сохранить вход в Яндекс на сервере"
    systemctl restart "openflux-node@$CHANNEL" || fail set-cookies "не удалось перезапустить openflux-node@$CHANNEL"
    printf '{"ok":true,"channel":"%s"}\n' "$CHANNEL"
}

# set-transports CONFIG_FILE: changes the transports of a channel that already
# runs (add or remove a document). The key, the port, the cookies and the
# encryption context stay, so the links handed out keep working; the node is
# restarted, and if it does not come back the old node.conf is put back.
cmd_set_transports() {
    [ "$(id -u)" = 0 ] || fail set-transports "нужны права root (sudo)"
    read_config "$@"
    check_channel
    dir="$CONF_ROOT/$CHANNEL"
    [ -f "$dir/node.conf" ] || fail set-transports "канала $CHANNEL нет на сервере"
    KEY=$(cat "$dir/encryption-key" 2>/dev/null)
    # The direct transport's listen port is what the node runs on; the port file mirrors it.
    PORT=$(sed -n 's/^Listen = [^:]*:\([0-9]*\)$/\1/p' "$dir/node.conf" | tail -n 1)
    valid_key "$KEY" || fail set-transports "ключ канала $CHANNEL не читается"
    valid_port "$PORT" || fail set-transports "порт канала $CHANNEL не читается"
    printf '%s\n' "$PORT" > "$dir/port" && chown "root:$NODE_USER" "$dir/port" && chmod 0644 "$dir/port"
    check_transports
    [ -n "$URL$YANDEX$MAILRU$BOARDS$CUPS$TELEMOST$AVITO$SCRIPTS" ] || fail input "нужен хотя бы один документ: без него канал не нужен"

    # The context is the URL line before the first [Transport] section.
    KEPT_CONTEXT=$(awk '/^\[/{exit} /^URL = /{sub(/^URL = /,""); print; exit}' "$dir/node.conf")
    CONTEXT_KEEP=1
    PREV_MODE=$(channel_mode "$CHANNEL")
    [ -n "$MODE" ] || MODE=$PREV_MODE
    [ "$MODE" = l3 ] && l3_ready set-transports
    cp -p "$dir/node.conf" "$dir/node.conf.prev" || fail set-transports "не удалось сохранить прежний node.conf"
    tmp=$(mktemp "$dir/.node.conf.XXXXXX") || fail set-transports "не удалось создать временный файл"
    write_node_conf > "$tmp" && chown "root:$NODE_USER" "$tmp" && chmod 0640 "$tmp" && mv -f "$tmp" "$dir/node.conf" \
        || { rm -f "$tmp"; fail set-transports "не удалось записать node.conf"; }
    [ -n "$COOKIES" ] && { write_cookies || fail set-transports "не удалось сохранить вход в Яндекс на сервере"; }
    if ! install_scripts; then
        set_transports_rollback
        fail set-transports "$SCRIPT_ERROR"
    fi
    if [ "$MODE" != "$PREV_MODE" ]; then
        set_channel_mode "$CHANNEL" "$MODE"
        systemctl daemon-reload >/dev/null 2>&1
    fi

    if ! systemctl restart "openflux-node@$CHANNEL" >/dev/null 2>&1; then
        set_transports_rollback
        fail set-transports "не удалось перезапустить openflux-node@$CHANNEL"
    fi
    sleep 4
    if ! systemctl is-active --quiet "openflux-node@$CHANNEL"; then
        logs=$(journalctl -u "openflux-node@$CHANNEL" -n 8 -o cat --no-pager 2>/dev/null | tail -n 8)
        set_transports_rollback
        fail set-transports "нода не поднялась с новыми транспортами, вернули прежние: $logs"
    fi
    rm -f "$dir/node.conf.prev"
    prune_scripts
    printf '{"ok":true,"channel":"%s","transports":"%s","mode":"%s"}\n' "$CHANNEL" "$(json_escape "$(transport_names)")" "$MODE"
}

# show CONFIG_FILE: one channel's documents, port, state, mode, encryption
# context and key, read from its node.conf (root: the files are not
# world-readable). The key goes only to the root who asks over SSH: the
# owner's app builds the channel's link and profile from it.
cmd_show() {
    [ "$(id -u)" = 0 ] || fail show "нужны права root (sudo)"
    read_config "$@"
    check_channel
    dir="$CONF_ROOT/$CHANNEL"
    [ -f "$dir/node.conf" ] || fail show "канала $CHANNEL нет на сервере"
    port=$(sed -n 's/^Listen = [^:]*:\([0-9]*\)$/\1/p' "$dir/node.conf" | tail -n 1)
    context=$(awk '/^\[/{exit} /^URL = /{sub(/^URL = /,""); print; exit}' "$dir/node.conf")
    state=$(systemctl is-active "openflux-node@$CHANNEL" 2>/dev/null)
    key=$(cat "$dir/encryption-key" 2>/dev/null)
    valid_key "$key" || key=""
    printf '{"ok":true,"channel":"%s","state":"%s","port":%s,"context":"%s","mode":"%s","key":"%s","transports":[' \
        "$CHANNEL" "$(json_escape "$state")" "${port:-0}" "$(json_escape "$context")" "$(channel_mode "$CHANNEL")" "$key"
    first=1
    awk '
        function flush() { if (type != "") print type "|" url "|" name "|" pubkey "|" params; type = ""; url = ""; name = ""; pubkey = ""; params = "" }
        /^\[Transport /          { flush(); section = 1; next }
        /^\[/                    { flush(); section = 0; next }
        section && /^Type = /    { sub(/^Type = /, ""); type = $0; next }
        section && /^URL = /     { sub(/^URL = /, ""); url = $0; next }
        section && /^Name = /    { sub(/^Name = /, ""); name = $0; next }
        section && /^Pubkey = /  { sub(/^Pubkey = /, ""); pubkey = $0; next }
        section && /^Params = /  { sub(/^Params = /, ""); params = $0; next }
        END                      { flush() }
    ' "$dir/node.conf" | while IFS='|' read -r type url name pubkey params; do
        [ "$type" = direct ] && continue
        [ "$first" = 1 ] || printf ','
        first=0
        if [ "$type" = script ]; then
            printf '{"type":"script","url":"%s","script":"%s","pubkey":"%s","params":"%s"}' \
                "$(json_escape "$url")" "$(json_escape "$name")" "$(json_escape "$pubkey")" "$(json_escape "$params")"
        else
            printf '{"type":"%s","url":"%s"}' "$(json_escape "$type")" "$(json_escape "$url")"
        fi
    done
    printf ']}\n'
}

# set_transports_rollback: the previous node.conf back, and the node restarted on it.
set_transports_rollback() {
    dir="$CONF_ROOT/$CHANNEL"
    [ -f "$dir/node.conf.prev" ] && mv -f "$dir/node.conf.prev" "$dir/node.conf"
    if [ -n "${PREV_MODE:-}" ]; then
        set_channel_mode "$CHANNEL" "$PREV_MODE"
        systemctl daemon-reload >/dev/null 2>&1
    fi
    systemctl restart "openflux-node@$CHANNEL" >/dev/null 2>&1
}

# autoupdate on|off: turns the core updater on or off for the whole server,
# e.g. on channels installed before the wizard offered it.
cmd_autoupdate() {
    [ "$(id -u)" = 0 ] || fail autoupdate "нужны права root (sudo)"
    case "${1:-}" in
        on)
            [ -n "$(list_channels)" ] || fail autoupdate "на сервере нет каналов OpenFlux"
            AUTOUPDATE=yes ;;
        off) AUTOUPDATE=no ;;
        *) fail usage "usage: node-install.sh autoupdate on|off" ;;
    esac
    apply_autoupdate || fail autoupdate "не удалось включить openflux-node-update.timer"
    state=false
    autoupdate_on && state=true
    printf '{"ok":true,"autoupdate":%s}\n' "$state"
}

cmd_status() {
    read_config
    check_channel
    [ -d "$CONF_ROOT/$CHANNEL" ] || fail status "канала $CHANNEL нет на сервере"
    state=$(systemctl is-active "openflux-node@$CHANNEL" 2>/dev/null)
    printf '{"ok":true,"channel":"%s","state":"%s"}\n' "$CHANNEL" "$(json_escape "$state")"
}

case "${1:-}" in
    probe) cmd_probe ;;
    plan) cmd_plan ;;
    apply) shift; cmd_apply "$@" ;;
    remove) shift; cmd_remove "$@" ;;
    uninstall) cmd_uninstall ;;
    list) cmd_list ;;
    status) cmd_status ;;
    upgrade) cmd_upgrade ;;
    set-cookies) shift; cmd_set_cookies "$@" ;;
    set-transports) shift; cmd_set_transports "$@" ;;
    show) shift; cmd_show "$@" ;;
    update) cmd_update ;;
    autoupdate) shift; cmd_autoupdate "$@" ;;
    *) fail usage "usage: node-install.sh probe|plan|apply|remove|list|uninstall|status|upgrade|set-cookies|set-transports|show|update|autoupdate" ;;
esac
