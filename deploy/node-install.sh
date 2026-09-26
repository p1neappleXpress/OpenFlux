#!/bin/sh
# Managed by OpenFlux node-install.sh
#
# Installs one OpenFlux exit channel on a Linux VDS. The Android app's
# "Создать свою ноду" wizard downloads this file by a pinned commit, checks
# its SHA-256 and runs it over SSH. Every channel is independent: its own
# document, key, port and systemd instance (openflux-node@<channel>).
# Nothing outside the paths below is touched, so existing services (other
# OpenFlux installs, Docker, VPNs) keep running.
#
#   /opt/openflux-node/bin/            core binaries (from GitHub Releases)
#   /etc/openflux-node/<channel>/      node.conf, encryption-key (0640), port,
#                                      firewall; the directory is 0751 so a
#                                      plain user's plan sees the channel and
#                                      its port but not its secrets
#   /var/lib/openflux-node/<channel>/  cookie store (systemd StateDirectory)
#   /etc/systemd/system/openflux-node@.service
#
# Usage: node-install.sh probe
#        node-install.sh plan|status              (config on stdin)
#        node-install.sh apply|remove CONFIG_FILE (run as root)
#        node-install.sh upgrade                  (run as root: move every
#                                                 channel to this core)
#        node-install.sh set-cookies CONFIG_FILE  (run as root: replace a
#                                                 channel's Yandex login)
# The config is "key=value" lines: channel, url, key, port, and optionally
# cookies: the channel's Yandex sign-in as the core's cookie store JSON,
# base64-encoded. It goes to /var/lib/openflux-node/<channel>/cookies.json
# (0600, owned by the node user) and is never printed. apply and remove
# take it from a 0600 temp file, which they delete after reading, so that
# stdin stays free for `sudo -S` (a wrong sudo password would otherwise make
# sudo read the config as further password attempts). Secrets never appear
# in arguments, so they stay out of ps and shell history.
# Output is one JSON object on stdout.

set -u
umask 077

CORE_VERSION="node-v1.2.3"
CORE_BASE="https://github.com/meepo161/openfluxandroidfork/releases/download/$CORE_VERSION"
SHA_amd64="e44152f19fa48ec4082a406c509896de99d7d403b5a681a101c6f807419ca7e6"
SHA_arm64="0b7ec511c3e4c9875d85dbc3c54be20e26906a5a409ce6b97ace31d2ccb14328"
SHA_arm="3cb0b4c0db5a021cae46fad14d9c0e4fbdea382c69211128730b3656dd9f2900"

BIN_DIR="/opt/openflux-node/bin"
CONF_ROOT="/etc/openflux-node"
STATE_ROOT="/var/lib/openflux-node"
UNIT_FILE="/etc/systemd/system/openflux-node@.service"
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

port_busy() { # PORT
    if have ss; then
        [ -n "$(ss -Hltn "sport = :$1" 2>/dev/null)" ]
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

CHANNEL=""; URL=""; KEY=""; PORT=""; COOKIES=""

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
            key=*) KEY=${line#key=} ;;
            port=*) PORT=${line#port=} ;;
            cookies=*) COOKIES=${line#cookies=} ;;
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
valid_port() {
    printf '%s' "$1" | grep -Eq '^[0-9]{4,5}$' && [ "$1" -ge 1024 ] && [ "$1" -le 65535 ]
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
    # shellcheck disable=SC2046
    printf '{"ok":true,"arch":"%s","os":"%s","systemd":%s,"sudo":"%s","downloader":"%s","firewall":"%s","core":"%s","channels":%s}\n' \
        "$arch" "$(json_escape "$os")" "$systemd" "$(sudo_mode)" "$(downloader)" "$(firewall_kind)" \
        "$CORE_VERSION" "$(json_list $(list_channels))"
}

# plan: read-only. Tells the app exactly what apply will change.
cmd_plan() {
    read_config
    check_channel
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

    set --
    id "$NODE_USER" >/dev/null 2>&1 || set -- "$@" "Создать системного пользователя $NODE_USER (без входа и домашней папки)"
    if [ -x "$BIN_DIR/openflux-$CORE_VERSION" ]; then
        set -- "$@" "Использовать уже установленное ядро OpenFlux $CORE_VERSION"
    else
        set -- "$@" "Скачать ядро OpenFlux $CORE_VERSION (linux-$arch) с GitHub и сверить SHA-256 в $BIN_DIR"
    fi
    set -- "$@" "Создать $CONF_ROOT/$CHANNEL: node.conf и ключ шифрования канала (права 0640)"
    [ -n "$COOKIES" ] && set -- "$@" "Сохранить вход в Яндекс для этого канала в $STATE_ROOT/$CHANNEL/cookies.json (права 0600, только для ноды)"
    [ -f "$UNIT_FILE" ] || set -- "$@" "Установить шаблон systemd $UNIT_FILE"
    set -- "$@" "Запустить openflux-node@$CHANNEL: Яндекс Документ (основной) и direct на порту $PORT/tcp (резерв)"
    case "$(firewall_kind)" in
        ufw) set -- "$@" "Разрешить входящий $PORT/tcp в ufw" ;;
        firewalld) set -- "$@" "Разрешить входящий $PORT/tcp в firewalld" ;;
    esac

    # shellcheck disable=SC2046
    printf '{"ok":true,"channel":"%s","port":%s,"arch":"%s","core":"%s","actions":%s,"untouched":%s}\n' \
        "$CHANNEL" "$PORT" "$arch" "$CORE_VERSION" "$(json_list "$@")" "$(json_list $(list_channels))"
}

# Rollback state for apply: what this run created.
CREATED_USER=0; CREATED_UNIT=0; CREATED_BIN=0; CREATED_CONF=0; CREATED_FW=""; STARTED=0

rollback() {
    [ "$STARTED" = 1 ] && systemctl disable --now "openflux-node@$CHANNEL" >/dev/null 2>&1
    case "$CREATED_FW" in
        ufw) ufw delete allow "$PORT/tcp" >/dev/null 2>&1 ;;
        firewalld) firewall-cmd --permanent --remove-port="$PORT/tcp" >/dev/null 2>&1 && firewall-cmd --reload >/dev/null 2>&1 ;;
    esac
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

cmd_apply() {
    [ "$(id -u)" = 0 ] || fail apply "нужны права root (sudo)"
    read_config "$@"
    check_channel
    valid_key "$KEY" || fail input "ключ канала должен быть 64 hex-символа"
    valid_url "$URL" || fail input "адрес документа должен быть вида https://docs.yandex.ru/edit/d/..."
    valid_port "$PORT" || fail input "не указан порт из плана"
    [ -z "$COOKIES" ] || printf '%s' "$COOKIES" | grep -Eq '^[A-Za-z0-9+/]+={0,2}$'         || fail input "cookies должны быть в base64"
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
    cat > "$dir/node.conf" <<EOF
# OpenFlux node channel $CHANNEL, written by node-install.sh
Role = exit
Mode = l4
EncryptionKeyFile = $dir/encryption-key
CookieStore = $STATE_ROOT/$CHANNEL/cookies.json
URL = $URL

[Transport vyandex]
Type = vyandex
Priority = 100
URL = $URL

[Transport direct]
Type = direct
Priority = 50
Listen = 0.0.0.0:$PORT
EOF
    printf '%s
' "$PORT" > "$dir/port"
    chown -R "root:$NODE_USER" "$dir"
    chmod 0751 "$dir"
    chmod 0640 "$dir/encryption-key" "$dir/node.conf"
    chmod 0644 "$dir/port"
    if [ -n "$COOKIES" ]; then
        write_cookies || apply_fail cookies "не удалось сохранить вход в Яндекс на сервере"
    fi

    if [ ! -f "$UNIT_FILE" ]; then
        write_unit
        CREATED_UNIT=1
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

    systemctl enable --now "openflux-node@$CHANNEL" >/dev/null 2>&1 \
        || apply_fail start "не удалось запустить openflux-node@$CHANNEL"
    STARTED=1
    sleep 4
    if ! systemctl is-active --quiet "openflux-node@$CHANNEL"; then
        logs=$(journalctl -u "openflux-node@$CHANNEL" -n 8 -o cat --no-pager 2>/dev/null | tail -n 8)
        apply_fail start "нода не запустилась: $logs"
    fi
    printf '{"ok":true,"channel":"%s","port":%s,"core":"%s"}\n' "$CHANNEL" "$PORT" "$CORE_VERSION"
}

cmd_remove() {
    [ "$(id -u)" = 0 ] || fail remove "нужны права root (sudo)"
    read_config "$@"
    check_channel
    dir="$CONF_ROOT/$CHANNEL"
    [ -d "$dir" ] || fail remove "канала $CHANNEL нет на сервере"
    systemctl disable --now "openflux-node@$CHANNEL" >/dev/null 2>&1
    if [ -f "$dir/firewall" ]; then
        read -r kind port < "$dir/firewall"
        case "$kind" in
            ufw) ufw delete allow "$port/tcp" >/dev/null 2>&1 ;;
            firewalld) firewall-cmd --permanent --remove-port="$port/tcp" >/dev/null 2>&1 && firewall-cmd --reload >/dev/null 2>&1 ;;
        esac
    fi
    rm -rf "${CONF_ROOT:?}/$CHANNEL" "${STATE_ROOT:?}/$CHANNEL"
    if [ -z "$(list_channels)" ]; then
        # The last channel is gone: remove everything this script installed.
        rm -f "$UNIT_FILE"
        systemctl daemon-reload >/dev/null 2>&1
        rm -rf /opt/openflux-node "$CONF_ROOT" "$STATE_ROOT"
        userdel "$NODE_USER" >/dev/null 2>&1
    fi
    printf '{"ok":true,"channel":"%s"}\n' "$CHANNEL"
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
    for old in "$BIN_DIR"/openflux-node-v*; do
        [ "$old" = "$BIN_DIR/openflux-$CORE_VERSION" ] || rm -f "$old"
    done
    printf '{"ok":true,"core":"%s","restarted":%s}
' "$CORE_VERSION" "$(json_list "$@")"
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
    printf '{"ok":true,"channel":"%s"}
' "$CHANNEL"
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
    status) cmd_status ;;
    upgrade) cmd_upgrade ;;
    set-cookies) shift; cmd_set_cookies "$@" ;;
    *) fail usage "usage: node-install.sh probe|plan|apply|remove|status|upgrade|set-cookies" ;;
esac
