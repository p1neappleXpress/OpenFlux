#!/usr/bin/env bash
set -Eeuo pipefail

# OpenFlux VPS installer
#
# Deploys the upstream OpenFlux Docker Compose exit-node profile on a
# Debian/Ubuntu VPS. It intentionally reuses the repository's Dockerfile,
# docker/entrypoint.sh and docker-compose.yml instead of maintaining a
# second container implementation.
#
# Features:
#   - installs Docker / Compose when missing
#   - clones or fast-forwards OpenFlux into /opt/OpenFlux
#   - configures yandex, vyandex or oneme transport via .env
#   - starts the upstream "exit-node" Compose profile
#   - installs openflux-health / openflux-log / openflux-status helpers
#   - installs a safe updater with image/source rollback
#   - optional weekly systemd update timer
#   - optional removal of the exact legacy host-wide outbound RST DROP rule
#
# The upstream exit-node container already installs its RST rule inside the
# container network namespace, so this installer does not add a host-wide
# firewall rule.

REPO_URL="${OPENFLUX_REPO_URL:-https://github.com/p1neappleXpress/OpenFlux.git}"
INSTALL_DIR="${OPENFLUX_INSTALL_DIR:-/opt/OpenFlux}"
CONFIG_FILE="/etc/openflux-vps.conf"

TRANSPORT="yandex"
DOC_URL=""
MAX_TOKEN=""
MAX_UID=""
DEBUG="0"

ENABLE_AUTO_UPDATE="0"
REMOVE_LEGACY_RST="0"
UPDATE_SOURCE="1"

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32mOK:\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage:
  sudo ./scripts/install-vps.sh [options]

Yandex / Volga Yandex:
  sudo ./scripts/install-vps.sh \
    --transport yandex \
    --url 'https://disk.yandex.ru/i/...'

MAX / OneMe:
  sudo ./scripts/install-vps.sh \
    --transport oneme \
    --max-token 'TOKEN' \
    --max-uid 'OTHER_SIDE_UID'

Options:
  --transport NAME            yandex | vyandex | oneme (default: yandex)
  --url URL                   Public Yandex document URL
  --max-token TOKEN           MAX Web token
  --max-uid UID               Other side's MAX user ID
  --debug                     Enable verbose OpenFlux logging
  --install-dir PATH          Deployment checkout (default: /opt/OpenFlux)
  --repo URL                  Git repository URL
  --no-source-update          Do not fast-forward an existing checkout
  --enable-auto-update        Install/enable weekly safe update timer
  --remove-legacy-global-rst  After a successful deployment, remove only the
                              exact legacy host-wide rule:
                                OUTPUT -p tcp --tcp-flags RST RST -j DROP
  -h, --help                  Show this help

Notes:
  * Must be run as root on Debian/Ubuntu with systemd.
  * Uses the repository's existing Docker Compose exit-node profile.
  * Does not change Docker networks or configuration of unrelated services
    such as Amnezia.
  * Automatic updates are opt-in.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --transport)
      [[ $# -ge 2 ]] || die "--transport requires a value"
      TRANSPORT="$2"; shift 2 ;;
    --url)
      [[ $# -ge 2 ]] || die "--url requires a value"
      DOC_URL="$2"; shift 2 ;;
    --max-token)
      [[ $# -ge 2 ]] || die "--max-token requires a value"
      MAX_TOKEN="$2"; shift 2 ;;
    --max-uid)
      [[ $# -ge 2 ]] || die "--max-uid requires a value"
      MAX_UID="$2"; shift 2 ;;
    --debug)
      DEBUG="1"; shift ;;
    --install-dir)
      [[ $# -ge 2 ]] || die "--install-dir requires a value"
      INSTALL_DIR="$2"; shift 2 ;;
    --repo)
      [[ $# -ge 2 ]] || die "--repo requires a value"
      REPO_URL="$2"; shift 2 ;;
    --no-source-update)
      UPDATE_SOURCE="0"; shift ;;
    --enable-auto-update)
      ENABLE_AUTO_UPDATE="1"; shift ;;
    --remove-legacy-global-rst)
      REMOVE_LEGACY_RST="1"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      die "Unknown argument: $1" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "Run this installer as root."

case "$TRANSPORT" in
  yandex|vyandex)
    [[ "$DOC_URL" =~ ^https:// ]] || die "$TRANSPORT requires --url with an https:// document URL."
    ;;
  oneme)
    [[ -n "$MAX_TOKEN" ]] || die "oneme requires --max-token."
    [[ -n "$MAX_UID" ]] || die "oneme requires --max-uid."
    ;;
  *)
    die "--transport must be yandex, vyandex or oneme."
    ;;
esac

for value in "$DOC_URL" "$MAX_TOKEN" "$MAX_UID"; do
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "Configuration values must not contain newlines."
done

if [[ ! -r /etc/os-release ]]; then
  die "Cannot identify the operating system."
fi
# shellcheck disable=SC1091
source /etc/os-release
case "${ID:-}" in
  ubuntu|debian) ;;
  *)
    die "This installer currently supports Debian/Ubuntu only (detected: ${ID:-unknown})."
    ;;
esac

install_packages() {
  log "Installing prerequisites"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -y
  apt-get install -y ca-certificates git curl iptables

  if ! command -v docker >/dev/null 2>&1; then
    apt-get install -y docker.io
  fi

  systemctl enable --now docker
  docker info >/dev/null
}

detect_compose() {
  if docker compose version >/dev/null 2>&1; then
    COMPOSE_STYLE="plugin"
    return 0
  fi

  if command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_STYLE="standalone"
    return 0
  fi

  log "Installing Docker Compose"
  # Package names differ between distro releases/repositories.
  if apt-cache show docker-compose-v2 >/dev/null 2>&1; then
    apt-get install -y docker-compose-v2
  elif apt-cache show docker-compose-plugin >/dev/null 2>&1; then
    apt-get install -y docker-compose-plugin
  else
    apt-get install -y docker-compose
  fi

  if docker compose version >/dev/null 2>&1; then
    COMPOSE_STYLE="plugin"
  elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_STYLE="standalone"
  else
    die "Docker Compose installation failed."
  fi
}

compose() {
  if [[ "${COMPOSE_STYLE:-}" == "plugin" ]]; then
    docker compose "$@"
  else
    docker-compose "$@"
  fi
}

prepare_checkout() {
  log "Preparing OpenFlux checkout"

  if [[ -d "$INSTALL_DIR/.git" ]]; then
    if ! git -C "$INSTALL_DIR" diff --quiet || ! git -C "$INSTALL_DIR" diff --cached --quiet; then
      die "$INSTALL_DIR contains tracked local changes. Commit/stash/revert them before deployment."
    fi

    if [[ "$UPDATE_SOURCE" == "1" ]]; then
      git -C "$INSTALL_DIR" fetch origin main
      CURRENT="$(git -C "$INSTALL_DIR" rev-parse HEAD)"
      TARGET="$(git -C "$INSTALL_DIR" rev-parse origin/main)"

      if [[ "$CURRENT" != "$TARGET" ]]; then
        # Only fast-forward existing installations. Do not rewrite user history.
        if ! git -C "$INSTALL_DIR" merge-base --is-ancestor "$CURRENT" "$TARGET"; then
          die "Existing checkout cannot be fast-forwarded to origin/main."
        fi
        git -C "$INSTALL_DIR" merge --ff-only "$TARGET"
      fi
    fi
  else
    [[ ! -e "$INSTALL_DIR" ]] || die "$INSTALL_DIR exists but is not a Git repository."
    mkdir -p "$(dirname "$INSTALL_DIR")"
    git clone "$REPO_URL" "$INSTALL_DIR"
  fi

  [[ -f "$INSTALL_DIR/docker-compose.yml" ]] || die "docker-compose.yml is missing from $INSTALL_DIR."
  [[ -f "$INSTALL_DIR/Dockerfile" ]] || die "Dockerfile is missing from $INSTALL_DIR."
  [[ -x "$INSTALL_DIR/docker/entrypoint.sh" ]] || die "docker/entrypoint.sh is missing or not executable."

  ok "Using OpenFlux commit $(git -C "$INSTALL_DIR" rev-parse --short HEAD)"
}

write_env() {
  log "Writing OpenFlux .env"

  if [[ -f "$INSTALL_DIR/.env" ]]; then
    cp -a "$INSTALL_DIR/.env" "$INSTALL_DIR/.env.backup.$(date +%Y%m%d-%H%M%S)"
  fi

  cat >"$INSTALL_DIR/.env" <<EOF
# Generated by scripts/install-vps.sh
TRANSPORT=$TRANSPORT
DOC_URL=$DOC_URL
MAX_TOKEN=$MAX_TOKEN
MAX_UID=$MAX_UID
SOCKS5_LISTEN=:1080
EXIT_LOCAL_IP=
DEBUG=$DEBUG
EOF
  chmod 600 "$INSTALL_DIR/.env"
}

deploy() {
  log "Building and starting upstream exit-node Compose profile"
  (
    cd "$INSTALL_DIR"
    compose --profile exit-node up -d --build exit-node
  )

  local cid
  cid="$(
    cd "$INSTALL_DIR"
    compose --profile exit-node ps -q exit-node
  )"
  [[ -n "$cid" ]] || die "Compose did not create the exit-node container."

  local running="false"
  for _ in $(seq 1 12); do
    running="$(docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null || true)"
    [[ "$running" == "true" ]] && break
    sleep 2
  done

  if [[ "$running" != "true" ]]; then
    warn "Exit node did not remain running. Recent logs:"
    (
      cd "$INSTALL_DIR"
      compose --profile exit-node logs --tail 100 exit-node
    ) >&2 || true
    exit 1
  fi

  # Give the transport a short window to fail on obvious configuration errors.
  sleep 5
  running="$(docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null || true)"
  [[ "$running" == "true" ]] || {
    warn "Exit node stopped shortly after startup. Recent logs:"
    (
      cd "$INSTALL_DIR"
      compose --profile exit-node logs --tail 100 exit-node
    ) >&2 || true
    exit 1
  }

  ok "OpenFlux exit-node is running"
}

write_config() {
  cat >"$CONFIG_FILE" <<EOF
OPENFLUX_INSTALL_DIR=$(printf '%q' "$INSTALL_DIR")
OPENFLUX_REPO_URL=$(printf '%q' "$REPO_URL")
OPENFLUX_COMPOSE_STYLE=$(printf '%q' "$COMPOSE_STYLE")
EOF
  chmod 644 "$CONFIG_FILE"
}

install_helpers() {
  log "Installing management helpers"

  cat >/usr/local/lib/openflux-compose <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
# shellcheck disable=SC1091
source /etc/openflux-vps.conf
cd "$OPENFLUX_INSTALL_DIR"

if [[ "$OPENFLUX_COMPOSE_STYLE" == "plugin" ]]; then
  exec docker compose "$@"
else
  exec docker-compose "$@"
fi
EOF
  chmod 755 /usr/local/lib/openflux-compose

  cat >/usr/local/bin/openflux-status <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
exec /usr/local/lib/openflux-compose --profile exit-node ps exit-node
EOF
  chmod 755 /usr/local/bin/openflux-status

  cat >/usr/local/bin/openflux-log <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
exec /usr/local/lib/openflux-compose --profile exit-node logs -f --tail 100 exit-node
EOF
  chmod 755 /usr/local/bin/openflux-log

  cat >/usr/local/bin/openflux-health <<'EOF'
#!/usr/bin/env bash
set -u
# shellcheck disable=SC1091
source /etc/openflux-vps.conf

CID="$(/usr/local/lib/openflux-compose --profile exit-node ps -q exit-node 2>/dev/null || true)"

echo "=== OpenFlux Health ==="
echo

printf "container: "
if [[ -n "$CID" && "$(docker inspect -f '{{.State.Running}}' "$CID" 2>/dev/null)" == "true" ]]; then
  echo "RUNNING"
else
  echo "DOWN"
fi

if [[ -n "$CID" ]]; then
  printf "image:     "
  docker inspect -f '{{.Config.Image}}' "$CID" 2>/dev/null || echo "unknown"

  printf "started:   "
  docker inspect -f '{{.State.StartedAt}}' "$CID" 2>/dev/null || echo "unknown"
fi

echo
echo "Host-wide outbound RST DROP rules:"
RULES="$(iptables -S OUTPUT 2>/dev/null | grep -E -- '^-A OUTPUT .*--tcp-flags RST RST .* -j DROP' || true)"
if [[ -n "$RULES" ]]; then
  echo "$RULES"
else
  echo "none"
fi

echo
echo "Recent exit-node logs:"
/usr/local/lib/openflux-compose --profile exit-node logs --tail 12 exit-node 2>/dev/null || true
EOF
  chmod 755 /usr/local/bin/openflux-health

  cat >/usr/local/bin/openflux-url <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
# shellcheck disable=SC1091
source /etc/openflux-vps.conf

NEW_URL="${1:-}"
[[ "$NEW_URL" =~ ^https:// ]] || {
  echo "Usage: openflux-url https://disk.yandex.ru/i/..." >&2
  exit 2
}

ENV_FILE="$OPENFLUX_INSTALL_DIR/.env"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

awk -v url="$NEW_URL" '
  BEGIN {done=0}
  /^DOC_URL=/ {print "DOC_URL=" url; done=1; next}
  {print}
  END {if (!done) print "DOC_URL=" url}
' "$ENV_FILE" >"$TMP"

chmod 600 "$TMP"
mv "$TMP" "$ENV_FILE"
trap - EXIT

/usr/local/lib/openflux-compose --profile exit-node up -d --no-build exit-node
echo "Document URL updated."
EOF
  chmod 755 /usr/local/bin/openflux-url
}

install_updater() {
  log "Installing safe OpenFlux updater"

  cat >/usr/local/bin/openflux-update <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
# shellcheck disable=SC1091
source /etc/openflux-vps.conf

exec 9>/run/openflux-update.lock
if ! flock -n 9; then
  echo "Another OpenFlux update is already running."
  exit 0
fi

cd "$OPENFLUX_INSTALL_DIR"

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "Tracked local changes detected. Update aborted." >&2
  exit 1
fi

echo "Fetching origin/main..."
git fetch origin main

CURRENT="$(git rev-parse HEAD)"
TARGET="$(git rev-parse origin/main)"

echo "Current: $CURRENT"
echo "Target:  $TARGET"

if [[ "$CURRENT" == "$TARGET" ]]; then
  echo "Already up to date."
  exit 0
fi

if ! git merge-base --is-ancestor "$CURRENT" "$TARGET"; then
  echo "origin/main is not a fast-forward from the current checkout. Update aborted." >&2
  exit 1
fi

OLD_IMAGE_ID="$(docker image inspect openflux:local -f '{{.Id}}' 2>/dev/null || true)"
[[ -n "$OLD_IMAGE_ID" ]] || {
  echo "Current openflux:local image is missing; refusing an unattended update." >&2
  exit 1
}

TMP="$(mktemp -d /tmp/openflux-update.XXXXXX)"
cleanup() {
  if [[ -d "$TMP/source" ]]; then
    git -C "$OPENFLUX_INSTALL_DIR" worktree remove --force "$TMP/source" >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "Preparing candidate source..."
git worktree add --detach "$TMP/source" "$TARGET" >/dev/null
cp -a .env "$TMP/source/.env"

echo "Building candidate image..."
if [[ "$OPENFLUX_COMPOSE_STYLE" == "plugin" ]]; then
  (
    cd "$TMP/source"
    docker compose --profile exit-node build exit-node
  )
else
  (
    cd "$TMP/source"
    docker-compose --profile exit-node build exit-node
  )
fi

# The upstream Compose file tags the build as openflux:local.
NEW_IMAGE_ID="$(docker image inspect openflux:local -f '{{.Id}}' 2>/dev/null || true)"
[[ -n "$NEW_IMAGE_ID" ]] || {
  echo "Candidate image is missing after build." >&2
  docker tag "$OLD_IMAGE_ID" openflux:local
  exit 1
}

echo "Candidate image: $NEW_IMAGE_ID"

# Move the checkout only after a successful candidate build.
git merge --ff-only "$TARGET"

rollback() {
  echo "Rolling back OpenFlux image/source..." >&2
  git reset --hard "$CURRENT" >/dev/null
  docker tag "$OLD_IMAGE_ID" openflux:local
  /usr/local/lib/openflux-compose --profile exit-node up -d --no-build --force-recreate exit-node >/dev/null || true
}

echo "Deploying candidate..."
if ! /usr/local/lib/openflux-compose --profile exit-node up -d --no-build --force-recreate exit-node; then
  rollback
  exit 1
fi

CID="$(/usr/local/lib/openflux-compose --profile exit-node ps -q exit-node 2>/dev/null || true)"
SUCCESS=0

for i in $(seq 1 12); do
  sleep 5

  if [[ -n "$CID" && "$(docker inspect -f '{{.State.Running}}' "$CID" 2>/dev/null || true)" == "true" ]]; then
    SUCCESS=1
  else
    SUCCESS=0
    break
  fi

  echo "Health wait: $i/12"
done

if [[ "$SUCCESS" != "1" ]]; then
  echo "Candidate failed runtime check." >&2
  /usr/local/lib/openflux-compose --profile exit-node logs --tail 100 exit-node >&2 || true
  rollback
  exit 1
fi

echo "Update completed successfully: ${CURRENT:0:12} -> ${TARGET:0:12}"
EOF
  chmod 755 /usr/local/bin/openflux-update

  cat >/etc/systemd/system/openflux-update.service <<'EOF'
[Unit]
Description=Safely update OpenFlux Docker exit node
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/openflux-update
EOF

  cat >/etc/systemd/system/openflux-update.timer <<'EOF'
[Unit]
Description=Weekly OpenFlux safe update

[Timer]
OnCalendar=Sun *-*-* 01:30:00 UTC
RandomizedDelaySec=10m
Persistent=true

[Install]
WantedBy=timers.target
EOF

  systemctl daemon-reload

  if [[ "$ENABLE_AUTO_UPDATE" == "1" ]]; then
    systemctl enable --now openflux-update.timer
    ok "Weekly safe update timer enabled"
  else
    systemctl disable --now openflux-update.timer >/dev/null 2>&1 || true
    ok "Safe updater installed; automatic timer left disabled"
  fi
}

remove_legacy_rst() {
  [[ "$REMOVE_LEGACY_RST" == "1" ]] || return 0

  log "Removing exact legacy host-wide RST DROP rule"
  local removed=0

  # Intentionally matches only the historical exact rule, not scoped/custom
  # firewall rules that may belong to another service.
  while iptables -C OUTPUT -p tcp --tcp-flags RST RST -j DROP 2>/dev/null; do
    iptables -D OUTPUT -p tcp --tcp-flags RST RST -j DROP
    removed=$((removed + 1))
  done

  ok "Removed $removed exact legacy host-wide RST rule(s)"

  if command -v netfilter-persistent >/dev/null 2>&1; then
    netfilter-persistent save >/dev/null || true
  fi
}

warn_about_host_rst() {
  local rules
  rules="$(iptables -S OUTPUT 2>/dev/null | grep -E -- '^-A OUTPUT .*--tcp-flags RST RST .* -j DROP' || true)"
  if [[ -n "$rules" ]]; then
    warn "Host-level outbound RST DROP rule(s) still exist:"
    echo "$rules" >&2
    warn "The upstream Docker exit node installs its own RST rule inside its container network namespace."
  fi
}

main() {
  install_packages
  detect_compose
  prepare_checkout
  write_env
  deploy
  write_config
  install_helpers
  install_updater
  remove_legacy_rst
  warn_about_host_rst

  log "Final status"
  openflux-health

  cat <<EOF

Installation complete.

Useful commands:
  openflux-health
  openflux-status
  openflux-log
  openflux-update

For Yandex/vyandex:
  openflux-url 'https://disk.yandex.ru/i/NEW_DOCUMENT'

Deployment checkout:
  $INSTALL_DIR

Automatic weekly updates:
  $([[ "$ENABLE_AUTO_UPDATE" == "1" ]] && echo "enabled" || echo "disabled (enable with: systemctl enable --now openflux-update.timer)")
EOF
}

main "$@"
