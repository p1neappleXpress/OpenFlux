#!/usr/bin/env bash
# Manual test harness for openflux-ctl. Run: bash cli/test-openflux-ctl.sh
# NOTE: sourcing openflux-ctl below imports its `set -euo pipefail` into this shell.
# Every call that is expected to fail must be guarded with `if`/`||`, never called bare.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FAILURES=0

assert_eq() {
  local desc="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "ok - $desc"
  else
    echo "FAIL - $desc"
    echo "  expected: $expected"
    echo "  actual:   $actual"
    FAILURES=$((FAILURES + 1))
  fi
}

setup_fixture() {
  TMPDIR_TEST="$(mktemp -d)"
  export OPENFLUX_STATE_FILE="$TMPDIR_TEST/state/clients.tsv"
  # openflux-ctl is sourced once at the top of this file, before any fixture
  # exists, so its STATE_FILE variable was already resolved to the default
  # path at that point; exporting OPENFLUX_STATE_FILE alone would not change
  # it. Set STATE_FILE directly too, so helper functions called in-process
  # (client_row, client_exists, record_client, remove_client_record) use the
  # fixture, not the real repo state file.
  export STATE_FILE="$OPENFLUX_STATE_FILE"
  export DOCKER_LOG="$TMPDIR_TEST/docker.log"
  export CONTAINERS_FILE="$TMPDIR_TEST/containers"
  mkdir -p "$TMPDIR_TEST/state"
  : > "$OPENFLUX_STATE_FILE"
  : > "$DOCKER_LOG"
  : > "$CONTAINERS_FILE"

  STUB_BIN="$TMPDIR_TEST/bin"
  mkdir -p "$STUB_BIN"
  cat > "$STUB_BIN/docker" <<'EOF'
#!/usr/bin/env bash
echo "$@" >> "$DOCKER_LOG"
case "$1" in
  run)
    name=""
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--name" ]; then name="$a"; fi
      prev="$a"
    done
    if [ -n "$name" ] && grep -qxF "$name" "$CONTAINERS_FILE" 2>/dev/null; then
      echo "docker: Error response from daemon: Conflict. The container name \"/$name\" is already in use" >&2
      exit 1
    fi
    if [ "${DOCKER_RUN_EXIT:-0}" != "0" ]; then
      echo "docker: Error response from daemon: simulated run failure" >&2
      exit "${DOCKER_RUN_EXIT}"
    fi
    if [ -n "${DOCKER_RUN_FAIL_NAME:-}" ] && [ "$name" = "$DOCKER_RUN_FAIL_NAME" ]; then
      echo "docker: Error response from daemon: simulated run failure for $name" >&2
      exit "${DOCKER_RUN_EXIT_CODE:-1}"
    fi
    [ -n "$name" ] && echo "$name" >> "$CONTAINERS_FILE"
    exit 0
    ;;
  rm)
    for a in "$@"; do
      case "$a" in
        -*) ;;
        *) sed -i "/^$a\$/d" "$CONTAINERS_FILE" 2>/dev/null || true ;;
      esac
    done
    exit 0
    ;;
  inspect)
    target="${@: -1}"
    if grep -qxF "$target" "$CONTAINERS_FILE" 2>/dev/null; then
      case "$*" in
        *"-f "*) echo "${DOCKER_INSPECT_STATUS:-running}" ;;
      esac
      exit 0
    fi
    exit 1
    ;;
  build)
    exit "${DOCKER_BUILD_EXIT:-0}"
    ;;
  info)
    exit "${DOCKER_INFO_EXIT:-0}"
    ;;
  logs)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
EOF
  chmod +x "$STUB_BIN/docker"

  # Stubs for install_docker()'s auto-install path. curl logs its args and
  # exits (${DOCKER_CURL_EXIT:-0}); it never actually fetches/installs
  # anything real, so a scenario where docker is genuinely absent stays
  # absent after the "install" -- exactly what the tests below check for.
  cat > "$STUB_BIN/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >> "$DOCKER_LOG"
exit "${DOCKER_CURL_EXIT:-0}"
EOF
  chmod +x "$STUB_BIN/curl"

  # sudo logs its args then runs the command directly (no real privilege
  # escalation -- this is a test double, not a security boundary).
  cat > "$STUB_BIN/sudo" <<'EOF'
#!/usr/bin/env bash
echo "sudo $*" >> "$DOCKER_LOG"
exec "$@"
EOF
  chmod +x "$STUB_BIN/sudo"

  # id -u reports $FAKE_UID (default 1000, i.e. non-root) so tests can
  # exercise both the root and non-root branches of install_docker().
  cat > "$STUB_BIN/id" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "-u" ]; then
  echo "${FAKE_UID:-1000}"
  exit 0
fi
exit 1
EOF
  chmod +x "$STUB_BIN/id"

  export PATH="$STUB_BIN:$PATH"
}

teardown_fixture() {
  rm -rf "$TMPDIR_TEST"
}

source "$SCRIPT_DIR/openflux-ctl"

# --- validate_transport / validate_name ---

if validate_transport mailru; then
  assert_eq "validate_transport accepts mailru" "yes" "yes"
else
  assert_eq "validate_transport accepts mailru" "yes" "no"
fi
if validate_transport oneme; then
  assert_eq "validate_transport rejects oneme" "no" "yes"
else
  assert_eq "validate_transport rejects oneme" "no" "no"
fi

if validate_name "alice-1"; then
  assert_eq "validate_name accepts alice-1" "yes" "yes"
else
  assert_eq "validate_name accepts alice-1" "yes" "no"
fi
if validate_name "al ice"; then
  assert_eq "validate_name rejects spaces" "no" "yes"
else
  assert_eq "validate_name rejects spaces" "no" "no"
fi

# --- client_exists / client_row ---

setup_fixture
printf 'alice\tmailru\thttps://cloud.mail.ru/public/AAAA/1111\topenflux-alice\t2026-01-01T00:00:00Z\n' >> "$OPENFLUX_STATE_FILE"
if client_exists alice; then
  assert_eq "client_exists true for known client" "yes" "yes"
else
  assert_eq "client_exists true for known client" "yes" "no"
fi
if client_exists ghost; then
  assert_eq "client_exists false for unknown client" "no" "yes"
else
  assert_eq "client_exists false for unknown client" "no" "no"
fi
teardown_fixture

# --- record_client / remove_client_record ---

setup_fixture
record_client alice mailru "https://cloud.mail.ru/public/AAAA/1111" openflux-alice
got="$(client_row alice | cut -f1-4)"
assert_eq "record_client writes a row client_row can read" \
  "$(printf 'alice\tmailru\thttps://cloud.mail.ru/public/AAAA/1111\topenflux-alice')" "$got"
remove_client_record alice
if client_exists alice; then
  assert_eq "remove_client_record deletes the row" "gone" "still there"
else
  assert_eq "remove_client_record deletes the row" "gone" "gone"
fi
teardown_fixture

# --- cmd_add ---

setup_fixture
out="$(cmd_add alice --transport=mailru --url=https://cloud.mail.ru/public/AAAA/1111)"
assert_eq "cmd_add prints confirmation" \
  "added client 'alice' -> mailru https://cloud.mail.ru/public/AAAA/1111 (openflux-alice)" "$out"
row="$(client_row alice | cut -f1-4)"
assert_eq "cmd_add records client in state file" \
  "$(printf 'alice\tmailru\thttps://cloud.mail.ru/public/AAAA/1111\topenflux-alice')" "$row"
docker_call="$(cat "$DOCKER_LOG")"
case "$docker_call" in
  *"run -d --name openflux-alice --restart unless-stopped --cap-add NET_RAW --cap-add NET_ADMIN -e ROLE=exit-node -e TRANSPORT=mailru -e URL=https://cloud.mail.ru/public/AAAA/1111 openflux-exit:local"*)
    assert_eq "cmd_add invokes docker run with correct args" "ok" "ok" ;;
  *)
    assert_eq "cmd_add invokes docker run with correct args" "ok" "$docker_call" ;;
esac
teardown_fixture

setup_fixture
status=0
# cmd_add's failure path calls die(), which calls exit; running it bare (not
# in a subshell) would terminate this whole harness instead of being caught
# by `||`, since `exit` inside a function is not a return status `||` can
# intercept. Wrap in a subshell so the exit only ends the subshell.
( cmd_add alice --transport=oneme --url=https://example.com ) >/dev/null 2>&1 || status=$?
assert_eq "cmd_add rejects unsupported transport" "1" "$status"
teardown_fixture

setup_fixture
status=0
( cmd_add "al ice" --transport=mailru --url=https://example.com ) >/dev/null 2>&1 || status=$?
assert_eq "cmd_add rejects invalid name" "1" "$status"
teardown_fixture

setup_fixture
cmd_add alice --transport=mailru --url=https://example.com/1 >/dev/null
status=0
( cmd_add alice --transport=mailru --url=https://example.com/2 ) >/dev/null 2>&1 || status=$?
assert_eq "cmd_add rejects duplicate name (exit code)" "1" "$status"
teardown_fixture

setup_fixture
export DOCKER_RUN_EXIT=125
status=0
err="$(( cmd_add alice --transport=mailru --url=https://example.com/1 ) 2>&1 1>/dev/null)" || status=$?
assert_eq "cmd_add fails (via die) when docker run fails" "1" "$status"
case "$err" in
  *"error: failed to start container"*) assert_eq "cmd_add docker-run failure prints error: prefix" "ok" "ok" ;;
  *) assert_eq "cmd_add docker-run failure prints error: prefix" "ok" "$err" ;;
esac
unset DOCKER_RUN_EXIT
if client_exists alice; then
  assert_eq "cmd_add does not record client when docker run fails" "no row" "row written"
else
  assert_eq "cmd_add does not record client when docker run fails" "no row" "no row"
fi
teardown_fixture

# --- ensure_state_file permissions ---

setup_fixture
rm -f "$OPENFLUX_STATE_FILE"
ensure_state_file
perm="$(stat -c %a "$OPENFLUX_STATE_FILE" 2>/dev/null || stat -f %Lp "$OPENFLUX_STATE_FILE" 2>/dev/null)"
assert_eq "ensure_state_file creates state file as 600" "600" "$perm"
teardown_fixture

setup_fixture
rm -f "$OPENFLUX_STATE_FILE"
cmd_add alice --transport=mailru --url=https://example.com/1 >/dev/null
perm="$(stat -c %a "$OPENFLUX_STATE_FILE" 2>/dev/null || stat -f %Lp "$OPENFLUX_STATE_FILE" 2>/dev/null)"
assert_eq "state file created via cmd_add is 600" "600" "$perm"
teardown_fixture

# --- cmd_remove ---

setup_fixture
cmd_add alice --transport=mailru --url=https://example.com/1 >/dev/null
cmd_remove alice >/dev/null
count="$(grep -c -F 'alice' "$OPENFLUX_STATE_FILE" || true)"
assert_eq "cmd_remove deletes client from state file" "0" "${count:-0}"
if grep -qxF "openflux-alice" "$CONTAINERS_FILE" 2>/dev/null; then
  assert_eq "cmd_remove removes the docker container" "gone" "still there"
else
  assert_eq "cmd_remove removes the docker container" "gone" "gone"
fi
teardown_fixture

setup_fixture
status=0
cmd_remove ghost >/dev/null 2>&1 || status=$?
assert_eq "cmd_remove on unknown client exits 0" "0" "$status"
teardown_fixture

# --- cmd_list ---

setup_fixture
out="$(cmd_list)"
assert_eq "cmd_list on empty state" "no clients" "$out"
teardown_fixture

setup_fixture
cmd_add alice --transport=mailru --url=https://example.com/1 >/dev/null
export DOCKER_INSPECT_STATUS="running"
out="$(cmd_list | tail -n 1 | awk '{print $1, $2, $3}')"
assert_eq "cmd_list shows client name, transport, status" "alice mailru running" "$out"
unset DOCKER_INSPECT_STATUS
teardown_fixture

setup_fixture
echo "" >> "$OPENFLUX_STATE_FILE"
out="$(cmd_list)"
assert_eq "cmd_list ignores blank lines (blank-only file)" "no clients" "$out"
teardown_fixture

setup_fixture
cmd_add alice --transport=mailru --url=https://example.com/1 >/dev/null
echo "" >> "$OPENFLUX_STATE_FILE"
export DOCKER_INSPECT_STATUS="running"
line_count="$(cmd_list | grep -c . || true)"
assert_eq "cmd_list with client and trailing blank line has exactly 2 lines (header + data)" "2" "$line_count"
unset DOCKER_INSPECT_STATUS
teardown_fixture

# --- cmd_logs ---

setup_fixture
cmd_add alice --transport=mailru --url=https://example.com/1 >/dev/null
status=0
err="$(cmd_logs ghost 2>&1 1>/dev/null)" || status=$?
assert_eq "cmd_logs on unknown client exits 1" "1" "$status"
case "$err" in
  *"known clients: alice"*) assert_eq "cmd_logs error lists known clients" "ok" "ok" ;;
  *) assert_eq "cmd_logs error lists known clients" "ok" "$err" ;;
esac
teardown_fixture

# --- cmd_install ---

setup_fixture
out="$(cmd_install)"
case "$out" in
  *"image built: openflux-exit:local"*) assert_eq "cmd_install builds the image" "ok" "ok" ;;
  *) assert_eq "cmd_install builds the image" "ok" "$out" ;;
esac
docker_call="$(cat "$DOCKER_LOG")"
case "$docker_call" in
  *"info"*"build -t openflux-exit:local"*) assert_eq "cmd_install checks docker info before building" "ok" "ok" ;;
  *) assert_eq "cmd_install checks docker info before building" "ok" "$docker_call" ;;
esac
teardown_fixture

setup_fixture
out="$(cmd_install)"
docker_call="$(cat "$DOCKER_LOG")"
case "$docker_call" in
  *curl*) assert_eq "cmd_install skips install_docker when docker is already present" "no curl" "$docker_call" ;;
  *) assert_eq "cmd_install skips install_docker when docker is already present" "no curl" "no curl" ;;
esac
teardown_fixture

setup_fixture
rm -f "$STUB_BIN/docker"
export FAKE_UID=1000
status=0
# install_docker's failure path (docker still missing after the "install")
# calls die(), which calls exit; run in a subshell for the same reason as
# the docker-daemon-unreachable test below.
( cmd_install ) >/dev/null 2>&1 || status=$?
assert_eq "cmd_install (docker missing, non-root) fails once install script leaves docker absent" "1" "$status"
docker_call="$(cat "$DOCKER_LOG")"
# curl and `sudo sh` are two ends of a pipeline (curl ... | sudo sh) -- they
# run as concurrent processes, so their log lines can land in either order.
# Check both substrings independently rather than requiring one order.
if echo "$docker_call" | grep -qF "curl -fsSL https://get.docker.com" \
  && echo "$docker_call" | grep -qF "sudo sh"; then
  assert_eq "cmd_install (docker missing, non-root) installs via curl|sudo sh" "ok" "ok"
else
  assert_eq "cmd_install (docker missing, non-root) installs via curl|sudo sh" "ok" "$docker_call"
fi
unset FAKE_UID
teardown_fixture

setup_fixture
rm -f "$STUB_BIN/docker"
export FAKE_UID=0
status=0
( cmd_install ) >/dev/null 2>&1 || status=$?
assert_eq "cmd_install (docker missing, root) fails once install script leaves docker absent" "1" "$status"
docker_call="$(cat "$DOCKER_LOG")"
case "$docker_call" in
  *sudo*)
    assert_eq "cmd_install (docker missing, root) does not use sudo" "no sudo" "$docker_call" ;;
  *"curl -fsSL https://get.docker.com"*)
    assert_eq "cmd_install (docker missing, root) does not use sudo" "no sudo" "no sudo" ;;
  *)
    assert_eq "cmd_install (docker missing, root) does not use sudo" "no sudo" "$docker_call" ;;
esac
unset FAKE_UID
teardown_fixture

setup_fixture
rm -f "$STUB_BIN/docker"
export DOCKER_CURL_EXIT=1
status=0
( cmd_install ) >/dev/null 2>"$TMPDIR_TEST/install-err.log" || status=$?
err="$(cat "$TMPDIR_TEST/install-err.log")"
assert_eq "cmd_install reports a clear error when the docker install script itself fails" "1" "$status"
case "$err" in
  *"automatic Docker install failed"*) assert_eq "cmd_install install-failure message is actionable" "ok" "ok" ;;
  *) assert_eq "cmd_install install-failure message is actionable" "ok" "$err" ;;
esac
unset DOCKER_CURL_EXIT
teardown_fixture

setup_fixture
export DOCKER_INFO_EXIT=1
status=0
# cmd_install's failure path here calls die(), which calls exit; running it
# bare (not in a subshell) would terminate this whole harness instead of
# being caught by `||`, since `exit` inside a function is not a return
# status `||` can intercept. Wrap in a subshell so the exit only ends the
# subshell.
( cmd_install ) >/dev/null 2>&1 || status=$?
assert_eq "cmd_install fails when docker daemon is unreachable" "1" "$status"
unset DOCKER_INFO_EXIT
teardown_fixture

# --- bulk_import ---

setup_fixture
import_file="$TMPDIR_TEST/import.csv"
cat > "$import_file" <<'EOF'
# comment line, ignored
alice,mailru,https://example.com/alice

bob,yandex,https://example.com/bob
EOF
status=0
out="$(bulk_import "$import_file")" || status=$?
assert_eq "bulk_import succeeds when every row is valid" "0" "$status"
count="$(awk -F'\t' '!/^#/ && NF' "$OPENFLUX_STATE_FILE" | wc -l | tr -d ' ')"
assert_eq "bulk_import adds every valid row" "2" "$count"
teardown_fixture

setup_fixture
import_file="$TMPDIR_TEST/import.csv"
cat > "$import_file" <<'EOF'
alice,mailru,https://example.com/alice
bob,oneme,https://example.com/bob
carol,yandex,https://example.com/carol
EOF
status=0
bulk_import "$import_file" >/dev/null 2>&1 || status=$?
assert_eq "bulk_import fails overall when one row is bad" "1" "$status"
count="$(awk -F'\t' '!/^#/ && NF' "$OPENFLUX_STATE_FILE" | wc -l | tr -d ' ')"
assert_eq "bulk_import still adds the valid rows around a bad one" "2" "$count"
if client_exists alice && client_exists carol && ! client_exists bob; then
  assert_eq "bulk_import adds alice and carol, skips bob" "yes" "yes"
else
  assert_eq "bulk_import adds alice and carol, skips bob" "yes" "no"
fi
teardown_fixture

setup_fixture
status=0
# cmd_install --import's failure path goes through bulk_import's
# `[ -f "$file" ] || die ...`, which calls exit; running it bare (not in a
# subshell) would terminate this whole harness instead of being caught by
# `||`, since `exit` inside a function is not a return status `||` can
# intercept. Wrap in a subshell so the exit only ends the subshell.
( cmd_install --import "$TMPDIR_TEST/does-not-exist.csv" ) >/dev/null 2>&1 || status=$?
assert_eq "cmd_install --import fails on a missing file" "1" "$status"
teardown_fixture

setup_fixture
import_file="$TMPDIR_TEST/import.csv"
cat > "$import_file" <<'EOF'
alice,mailru,https://example.com/alice
bob,mailru,https://example.com/bob
carol,yandex,https://example.com/carol
EOF
export DOCKER_RUN_FAIL_NAME="openflux-bob"
status=0
bulk_import "$import_file" >/dev/null 2>&1 || status=$?
unset DOCKER_RUN_FAIL_NAME
assert_eq "bulk_import fails overall when one row's docker run fails" "1" "$status"
if client_exists alice && client_exists carol && ! client_exists bob; then
  assert_eq "bulk_import skips row whose docker run fails, adds the rest" "yes" "yes"
else
  assert_eq "bulk_import skips row whose docker run fails, adds the rest" "yes" "no"
fi
teardown_fixture

setup_fixture
import_file="$TMPDIR_TEST/import.csv"
printf 'alice,mailru,https://example.com/a\r\n' > "$import_file"
status=0
bulk_import "$import_file" >/dev/null 2>&1 || status=$?
assert_eq "bulk_import with CRLF line succeeds" "0" "$status"
recorded_url="$(client_row alice | cut -f3)"
case "$recorded_url" in
  *$'\r'*) assert_eq "bulk_import strips trailing CR from imported URL" "no CR" "has CR" ;;
  *) assert_eq "bulk_import strips trailing CR from imported URL" "no CR" "no CR" ;;
esac
assert_eq "bulk_import CRLF-stripped URL matches expected value" \
  "https://example.com/a" "$recorded_url"
teardown_fixture

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "All tests passed."
  exit 0
else
  echo "$FAILURES test(s) failed."
  exit 1
fi
