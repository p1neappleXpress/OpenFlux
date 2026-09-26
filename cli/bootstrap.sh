#!/usr/bin/env bash
# One-shot installer for openflux-ctl on a fresh Ubuntu/Debian VPS -- no
# manual `git clone` needed. Fetches the repo, hands off to
# `openflux-ctl install` (which installs Docker itself if missing and
# builds the exit-node image), then symlinks `openflux-ctl` onto PATH.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/devslaweekq/OpenFlux/feature/multi-accounting/cli/bootstrap.sh | bash
#
# Override any of these via env vars before piping into bash, e.g.:
#   OPENFLUX_BRANCH=main curl -fsSL .../bootstrap.sh | bash
set -euo pipefail

REPO_URL="${OPENFLUX_REPO_URL:-https://github.com/devslaweekq/OpenFlux.git}"
BRANCH="${OPENFLUX_BRANCH:-feature/multi-accounting}"
INSTALL_DIR="${OPENFLUX_INSTALL_DIR:-$HOME/openflux}"
SYMLINK_TARGET="${OPENFLUX_SYMLINK:-/usr/local/bin/openflux-ctl}"

die() {
  echo "error: $*" >&2
  exit 1
}

as_root() {
  if [ "$(id -u)" = "0" ]; then
    "$@"
  else
    command -v sudo >/dev/null 2>&1 || die "need root (or sudo) to run: $*"
    sudo "$@"
  fi
}

if ! command -v git >/dev/null 2>&1; then
  echo "git not found -- installing ..."
  as_root apt-get update -y
  as_root apt-get install -y git
fi

if [ -d "$INSTALL_DIR/.git" ]; then
  echo "updating existing checkout at $INSTALL_DIR ..."
  git -C "$INSTALL_DIR" fetch --depth 1 origin "$BRANCH"
  git -C "$INSTALL_DIR" checkout "$BRANCH"
  git -C "$INSTALL_DIR" reset --hard "origin/$BRANCH"
else
  [ -e "$INSTALL_DIR" ] && die "$INSTALL_DIR already exists and is not a git checkout -- remove it or set OPENFLUX_INSTALL_DIR to a different path"
  echo "cloning $REPO_URL ($BRANCH) into $INSTALL_DIR ..."
  git clone --branch "$BRANCH" --depth 1 "$REPO_URL" "$INSTALL_DIR"
fi

echo "running openflux-ctl install ..."
"$INSTALL_DIR/cli/openflux-ctl" install

if [ "$SYMLINK_TARGET" != "-" ]; then
  echo "linking $SYMLINK_TARGET -> $INSTALL_DIR/cli/openflux-ctl ..."
  as_root ln -sf "$INSTALL_DIR/cli/openflux-ctl" "$SYMLINK_TARGET"
fi

cat <<EOF

openflux-ctl is installed.

  Repo:  $INSTALL_DIR
  State: $INSTALL_DIR/cli/state/clients.tsv

Next steps:
  openflux-ctl add <name> --transport=<yandex|vyandex|cupsonline|mailru> --url=<url>
  openflux-ctl list
EOF
