#!/bin/sh
# Maps environment variables to openflux flags and, for the exit node,
# installs the kernel-RST-drop rule *inside this container's netns*.
#
# Why the rule: the exit node's TCP connections live in a userspace (gVisor)
# stack, so the kernel has no socket for them and answers every inbound
# SYN-ACK with an RST, tearing the tunnel down. Confining the DROP to the
# container netns is the scoped variant of upstream's host-wide rule — it
# cannot affect the host or other containers.
set -eu

role="${ROLE:-client}"
transport="${TRANSPORT:-yandex}"
listen="${SOCKS5_LISTEN:-:1080}"

case "$role" in
  client|exit-node) ;;
  *)
    echo "ROLE must be 'client' or 'exit-node' (got '$role')" >&2
    exit 2
    ;;
esac

case "$transport" in
  yandex|vyandex|oneme) ;;
  *)
    echo "TRANSPORT must be one of yandex, vyandex, oneme (got '$transport')" >&2
    exit 2
    ;;
esac

set -- "--$role" --transport "$transport"

if [ "$role" = client ]; then
  set -- "$@" --socks5 "$listen"
fi

if [ -n "${URL:-}" ]; then
  set -- "$@" --url "$URL"
fi
if [ -n "${MAX_TOKEN:-}" ]; then
  set -- "$@" --maxToken "$MAX_TOKEN"
fi
if [ -n "${MAX_UID:-}" ]; then
  set -- "$@" --maxUid "$MAX_UID"
fi
if [ -n "${LOCAL_IP:-}" ]; then
  # Optional: pin the egress IP (alias IP) so the RST drop could be scoped
  # with `-s <ip>` too; inside a dedicated container netns it's usually
  # unnecessary.
  set -- "$@" --local-ip "$LOCAL_IP"
fi
case "${DEBUG:-0}" in
  1|true|yes) set -- "$@" --debug ;;
esac

if [ "$role" = exit-node ]; then
  echo "[entrypoint] dropping outbound TCP RSTs inside the container netns (except our own, fwmark 100)"
  # The mark-100 RETURN rule must come first: it exempts RSTs the exit node's
  # own raw socket sends on purpose (rawSocketMark in tunnel/rawsocket_linux.go)
  # so only the kernel's spurious auto-RSTs (no fwmark, since they're not
  # sent through that socket) get dropped below.
  if ! iptables -A OUTPUT -p tcp --tcp-flags RST RST -m mark --mark 100 -j RETURN; then
    echo "[entrypoint] WARNING: iptables failed (missing NET_ADMIN?); kernel RSTs will kill tunnel connections" >&2
  fi
  if ! iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP; then
    echo "[entrypoint] WARNING: iptables failed (missing NET_ADMIN?); kernel RSTs will kill tunnel connections" >&2
  fi
fi

echo "[entrypoint] exec: openflux $*"
exec openflux "$@"
