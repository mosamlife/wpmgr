#!/usr/bin/env bash
# scripts/ci-docker-mirror_test.sh
#
# The regression suite for scripts/ci-docker-mirror.sh.
#
# HOW IT WORKS. Each case builds a little runner out of fakes (docker,
# systemctl, sudo, journalctl) in its own directory, runs the real script
# against it with a PATH that holds only those fakes and symlinks to the real
# tools the script needs, and asserts the exit code, what came out, and what
# was left on disk. The fake daemon is stateful: `systemctl restart docker`
# makes it re-read the config file, and `docker info` then reports what it read.
# So "the mirror is active" is something the script has to establish by asking
# the daemon, not something the suite hands it.
#
# THREE KINDS OF CASE, because a guard is wrong in two directions:
#
#   * it must FIRE (exit 2, nothing rewritten where it should not be): a config
#     that is not JSON, a registry-mirrors that is not an array, a missing
#     binary, a restart that fails, a daemon that never comes back, and a daemon
#     that comes back without the mirror.
#   * it must NOT OVER-FIRE: a runner with no config at all, a config that
#     already holds other keys and other mirrors (all kept), a daemon that is
#     already running the mirror (no restart), a whitespace-only config.
#   * it must not pass by finding NOTHING: the report of an empty journal says
#     zero, it does not claim success; a missing journalctl is said out loud.
#
# RUN IT:
#   scripts/ci-docker-mirror_test.sh            # everything
#   scripts/ci-docker-mirror_test.sh restart    # only cases whose name contains "restart"
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_DOCKER_MIRROR_SCRIPT=/path/to/broken-copy.sh scripts/ci-docker-mirror_test.sh
#
# Needs bash and jq, nothing else beyond POSIX tools. No Docker, no network, no
# root. bash 3.2 compatible.

# Every helper and case below is called through run_case or by name from a case
# function, which shellcheck's static pass reports as never invoked.
# shellcheck disable=SC2329
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${WPMGR_DOCKER_MIRROR_SCRIPT:-$HERE/ci-docker-mirror.sh}"
FILTER="${1:-}"

if [ ! -f "$SCRIPT" ]; then
  echo "no script at $SCRIPT" >&2
  exit 2
fi

BASH_BIN="$(command -v bash)" || { echo "bash not found on PATH" >&2; exit 2; }

# The real tools the script under test and the fakes need. The suite links
# exactly these into each case's PATH, so a case can take one away and see what
# the script does without it.
REAL_TOOLS="jq cat tee mkdir dirname basename tr sort uniq head sed grep id sleep rm"
for t in $REAL_TOOLS; do
  command -v "$t" >/dev/null 2>&1 || { echo "required tool not found on PATH: $t" >&2; exit 2; }
done

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-docker-mirror.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
SKIPPED=0
FAILED_NAMES=''

MIRROR='https://mirror.gcr.io'

# ---------------------------------------------------------------------------
# The fakes. They live once in $WORK/fakes and each case copies them in.
# ---------------------------------------------------------------------------
mkdir -p "$WORK/fakes"

# docker: only `info`, which answers the way a real daemon does: not at all
# while it is down, and with the mirrors it was STARTED with (null when none)
# once it is up.
cat >"$WORK/fakes/docker" <<'EOF'
#!/bin/sh
case "$1" in
  info)
    [ -e "$FAKE_STATE/up" ] || exit 1
    if [ -e "$FAKE_STATE/mirrors" ]; then cat "$FAKE_STATE/mirrors"; else echo null; fi
    ;;
  *) echo "fake docker: unexpected call: $*" >&2; exit 99 ;;
esac
EOF

# systemctl: `restart docker` records itself, then behaves as the case says. A
# healthy restart makes the daemon re-read the config file, as dockerd does, and
# print its mirrors with a trailing slash, as `docker info` does.
cat >"$WORK/fakes/systemctl" <<'EOF'
#!/bin/sh
if [ "$1" != restart ] || [ "$2" != docker ]; then echo "fake systemctl: unexpected call: $*" >&2; exit 99; fi
echo restart >>"$FAKE_STATE/restarts"
[ -e "$FAKE_STATE/restart-fails" ] && exit 1
if [ -e "$FAKE_STATE/stays-down" ]; then rm -f "$FAKE_STATE/up"; exit 0; fi
: >"$FAKE_STATE/up"
if [ -e "$FAKE_STATE/ignores-config" ]; then exit 0; fi
if [ -e "$WPMGR_DOCKER_DAEMON_JSON" ]; then
  jq -c '(."registry-mirrors" // []) | map(. + "/")' "$WPMGR_DOCKER_DAEMON_JSON" >"$FAKE_STATE/mirrors" || echo null >"$FAKE_STATE/mirrors"
else
  echo null >"$FAKE_STATE/mirrors"
fi
EOF

cat >"$WORK/fakes/sudo" <<'EOF'
#!/bin/sh
exec "$@"
EOF

cat >"$WORK/fakes/journalctl" <<'EOF'
#!/bin/sh
[ -e "$FAKE_STATE/journal-fails" ] && exit 1
cat "$FAKE_STATE/journal" 2>/dev/null
exit 0
EOF

chmod +x "$WORK/fakes/"*

# ---------------------------------------------------------------------------
# Case plumbing
# ---------------------------------------------------------------------------
# new_runner NAME -- makes a runner whose daemon is up and has no mirror, and
# leaves its directory in E. Config path: $E/etc/daemon.json (absent).
new_runner() {
  E="$WORK/$1"
  rm -rf "$E"
  mkdir -p "$E/bin" "$E/state" "$E/etc"
  for t in $REAL_TOOLS; do ln -s "$(command -v "$t")" "$E/bin/$t"; done
  cp "$WORK/fakes/docker" "$WORK/fakes/systemctl" "$WORK/fakes/sudo" "$WORK/fakes/journalctl" "$E/bin/"
  : >"$E/state/up"
  CFG="$E/etc/daemon.json"
}

# run_script ARGS... -- runs the script under test with only the runner's PATH.
# Output lands in $E/out, the exit code in RC. WPMGR_EXTRA holds extra
# VAR=value pairs for the case.
run_script() {
  # shellcheck disable=SC2086
  env -i PATH="$E/bin" HOME="$E" FAKE_STATE="$E/state" \
    WPMGR_DOCKER_DAEMON_JSON="$CFG" \
    WPMGR_DOCKER_MIRROR_POLLS=3 WPMGR_DOCKER_MIRROR_POLL_SLEEP=0 \
    ${WPMGR_EXTRA:-} \
    "$BASH_BIN" "$SCRIPT" "$@" >"$E/out" 2>&1
  RC=$?
}

restarts() {
  if [ -e "$E/state/restarts" ]; then grep -c restart "$E/state/restarts"; else echo 0; fi
}

cfg_get() { jq -r "$1" "$CFG"; }

# A case is a function. check_* helpers record the first failure only.
CASE_FAIL=''
fail_with() { [ -z "$CASE_FAIL" ] && CASE_FAIL="$1"; return 0; }
expect_rc() { [ "$RC" = "$1" ] || fail_with "exit $RC, wanted $1"; }
expect_out() { grep -Fq -- "$1" "$E/out" || fail_with "output lacks: $1"; }
expect_no_out() { if grep -Fq -- "$1" "$E/out"; then fail_with "output has: $1"; fi; }
expect_eq() { [ "$1" = "$2" ] || fail_with "got '$1', wanted '$2' ($3)"; }

run_case() {
  _name="$1"
  _fn="$2"
  if [ -n "$FILTER" ] && ! printf '%s' "$_name" | grep -Fq -- "$FILTER"; then return 0; fi
  CASE_FAIL=''
  WPMGR_EXTRA=''
  "$_fn"
  if [ -z "$CASE_FAIL" ]; then
    PASSED=$((PASSED + 1))
    printf 'ok    %s\n' "$_name"
  else
    FAILED=$((FAILED + 1))
    FAILED_NAMES="${FAILED_NAMES}  ${_name}: ${CASE_FAIL}
"
    printf 'FAIL  %s: %s\n' "$_name" "$CASE_FAIL"
    sed 's/^/        | /' "$E/out" 2>/dev/null | head -20
  fi
}

skip_case() {
  SKIPPED=$((SKIPPED + 1))
  printf 'skip  %s (%s)\n' "$1" "$2"
}

# ===========================================================================
# configure: must work, and must not over-fire
# ===========================================================================
c_fresh_runner() {
  new_runner fresh
  run_script
  expect_rc 0
  expect_out "docker registry mirror active: $MIRROR"
  expect_eq "$(cfg_get '.["registry-mirrors"] | join(",")')" "$MIRROR" "mirrors on disk"
  expect_eq "$(cfg_get '.debug')" "true" "debug on disk"
  expect_eq "$(restarts)" "1" "restarts"
}
run_case "configure: a runner with no daemon.json gets the mirror, one restart" c_fresh_runner

c_keeps_other_keys() {
  new_runner keeps
  mkdir -p "$E/etc"
  printf '{"exec-opts":["native.cgroupdriver=cgroupfs"],"log-driver":"json-file","registry-mirrors":["https://other.example"]}\n' >"$CFG"
  run_script
  expect_rc 0
  expect_eq "$(cfg_get '.["exec-opts"][0]')" "native.cgroupdriver=cgroupfs" "exec-opts kept"
  expect_eq "$(cfg_get '.["log-driver"]')" "json-file" "log-driver kept"
  expect_eq "$(cfg_get '.["registry-mirrors"] | join(",")')" "$MIRROR,https://other.example" "mirror first, other kept"
}
run_case "configure: other keys and other mirrors are kept, the mirror goes first" c_keeps_other_keys

c_already_active() {
  new_runner active
  mkdir -p "$E/etc"
  printf '{\n  "debug": true,\n  "registry-mirrors": [\n    "%s"\n  ]\n}\n' "$MIRROR" >"$CFG"
  printf '["%s/"]\n' "$MIRROR" >"$E/state/mirrors"
  before="$(cat "$CFG")"
  run_script
  expect_rc 0
  expect_out "already active"
  expect_eq "$(restarts)" "0" "restarts"
  expect_eq "$(cat "$CFG")" "$before" "config untouched"
}
run_case "configure: already active means no rewrite and no restart" c_already_active

c_on_disk_not_running() {
  new_runner ondisk
  mkdir -p "$E/etc"
  printf '{"debug":true,"registry-mirrors":["%s"]}\n' "$MIRROR" >"$CFG"
  run_script
  expect_rc 0
  expect_eq "$(restarts)" "1" "restarts (the file is right but the daemon is not running it)"
  expect_out "docker registry mirror active: $MIRROR"
}
run_case "configure: correct on disk but not running restarts the daemon" c_on_disk_not_running

c_reorders() {
  new_runner reorder
  mkdir -p "$E/etc"
  printf '{"registry-mirrors":["https://other.example","%s"]}\n' "$MIRROR" >"$CFG"
  run_script
  expect_rc 0
  expect_eq "$(cfg_get '.["registry-mirrors"] | join(",")')" "$MIRROR,https://other.example" "no duplicate, mirror first"
}
run_case "configure: the mirror is moved to the front and not listed twice" c_reorders

c_whitespace_config() {
  new_runner blank
  mkdir -p "$E/etc"
  printf '  \n\n' >"$CFG"
  run_script
  expect_rc 0
  expect_eq "$(cfg_get '.["registry-mirrors"] | join(",")')" "$MIRROR" "mirrors on disk"
}
run_case "configure: a whitespace-only daemon.json is treated as empty" c_whitespace_config

c_custom_mirror_trailing_slash() {
  new_runner custom
  WPMGR_EXTRA='WPMGR_DOCKER_MIRROR=https://mirror.example/'
  run_script
  expect_rc 0
  expect_eq "$(cfg_get '.["registry-mirrors"] | join(",")')" "https://mirror.example" "trailing slash removed"
}
run_case "configure: WPMGR_DOCKER_MIRROR with a trailing slash is normalised" c_custom_mirror_trailing_slash

c_creates_missing_dir() {
  new_runner mkdir
  CFG="$E/etc/docker/nested/daemon.json"
  run_script
  expect_rc 0
  [ -f "$CFG" ] || fail_with "config not created under a missing directory"
}
run_case "configure: a missing config directory is created" c_creates_missing_dir

# ===========================================================================
# configure: must fire
# ===========================================================================
c_invalid_json() {
  new_runner badjson
  mkdir -p "$E/etc"
  printf '{ this is not json' >"$CFG"
  before="$(cat "$CFG")"
  run_script
  expect_rc 2
  expect_out "left untouched"
  expect_eq "$(cat "$CFG")" "$before" "config byte-identical"
  expect_eq "$(restarts)" "0" "restarts"
}
run_case "fires: a daemon.json that is not JSON is exit 2, untouched, no restart" c_invalid_json

c_mirrors_not_array() {
  new_runner notarray
  mkdir -p "$E/etc"
  printf '{"registry-mirrors":"https://other.example"}\n' >"$CFG"
  before="$(cat "$CFG")"
  run_script
  expect_rc 2
  expect_eq "$(cat "$CFG")" "$before" "config byte-identical"
  expect_eq "$(restarts)" "0" "restarts"
}
run_case "fires: registry-mirrors that is not an array is exit 2, untouched" c_mirrors_not_array

c_root_not_object() {
  new_runner rootarray
  mkdir -p "$E/etc"
  printf '["registry-mirrors"]\n' >"$CFG"
  run_script
  expect_rc 2
  expect_eq "$(restarts)" "0" "restarts"
}
run_case "fires: a daemon.json whose root is not an object is exit 2" c_root_not_object

c_no_docker() {
  new_runner nodocker
  rm "$E/bin/docker"
  run_script
  expect_rc 2
  expect_out "docker not found"
  expect_eq "$(restarts)" "0" "restarts"
}
run_case "fires: docker missing from PATH is exit 2" c_no_docker

c_no_jq() {
  new_runner nojq
  rm "$E/bin/jq"
  run_script
  expect_rc 2
  expect_out "jq not found"
}
run_case "fires: jq missing from PATH is exit 2" c_no_jq

c_no_sudo() {
  new_runner nosudo
  rm "$E/bin/sudo"
  run_script
  expect_rc 2
  expect_out "sudo not found"
}
if [ "$(id -u)" = 0 ]; then
  skip_case "fires: not root and no sudo is exit 2" "running as root, the script correctly needs no sudo"
else
  run_case "fires: not root and no sudo is exit 2" c_no_sudo
fi

c_no_systemctl() {
  new_runner nosystemctl
  rm "$E/bin/systemctl"
  run_script
  expect_rc 2
  expect_out "systemctl not found"
}
run_case "fires: systemctl missing from PATH is exit 2" c_no_systemctl

c_restart_fails() {
  new_runner restartfails
  : >"$E/state/restart-fails"
  run_script
  expect_rc 2
  expect_out "systemctl restart docker failed"
}
run_case "fires: a failing restart is exit 2" c_restart_fails

c_daemon_never_returns() {
  new_runner stays
  : >"$E/state/stays-down"
  run_script
  expect_rc 2
  expect_out "did not answer"
  expect_out "within 3 polls"
}
run_case "fires: a daemon that never comes back is exit 2 after a bounded wait" c_daemon_never_returns

c_daemon_ignores_mirror() {
  new_runner ignores
  : >"$E/state/ignores-config"
  run_script
  expect_rc 2
  expect_out "does not list $MIRROR"
}
run_case "fires: a daemon that comes back without the mirror is exit 2, not a pass" c_daemon_ignores_mirror

c_error_is_an_annotation() {
  new_runner annotation
  rm "$E/bin/jq"
  run_script
  expect_out "::error title=Docker registry mirror::"
}
run_case "fires: the reason is also a workflow error annotation" c_error_is_an_annotation

# ===========================================================================
# usage
# ===========================================================================
c_unknown_option() {
  new_runner unknownopt
  run_script --bogus
  expect_rc 2
  expect_out "usage:"
  expect_eq "$(restarts)" "0" "restarts"
}
run_case "usage: an unknown option is exit 2 and touches nothing" c_unknown_option

c_two_args() {
  new_runner twoargs
  run_script --report extra
  expect_rc 2
  expect_out "usage:"
}
run_case "usage: more than one argument is exit 2" c_two_args

c_help() {
  new_runner help
  run_script --help
  expect_rc 0
  expect_out "usage:"
  expect_eq "$(restarts)" "0" "restarts"
}
run_case "usage: --help is exit 0 and touches nothing" c_help

# ===========================================================================
# report
# ===========================================================================
# Lines as the daemon writes them. The first three are the containerd-store
# shape, captured from a real daemon whose only mirror was mirror.gcr.io.
c_report_containerd() {
  new_runner reportcontainerd
  cat >"$E/state/journal" <<'EOF'
time="2026-10-09T21:35:33.942377222Z" level=debug msg=resolving host=mirror.gcr.io method=HEAD url="https://mirror.gcr.io/v2/library/redis/manifests/7-alpine?ns=docker.io"
time="2026-10-09T21:35:34.370235847Z" level=debug msg="fetch response received" host=mirror.gcr.io method=HEAD
time="2026-10-09T21:35:40.917492211Z" level=debug msg=resolving host=mirror.gcr.io method=HEAD url="https://mirror.gcr.io/v2/chrislusf/seaweedfs/manifests/sha256:1055999e08eed1789b0ae45d235126e4495e23d3fb9d6396293fd42539b1ae6a?ns=docker.io"
time="2026-10-09T21:36:01.000000000Z" level=debug msg=resolving host=registry-1.docker.io method=HEAD url="https://registry-1.docker.io/v2/library/nginx/manifests/1.27-alpine"
EOF
  run_script --report
  expect_rc 0
  expect_out "resolved against mirror.gcr.io:  2"
  expect_out "resolved against Docker Hub (registry-1.docker.io):  1"
  expect_out "resolving host=mirror.gcr.io https://mirror.gcr.io/v2/library/redis/manifests/7-alpine?ns=docker.io"
  expect_out "resolving host=registry-1.docker.io https://registry-1.docker.io/v2/library/nginx/manifests/1.27-alpine"
}
run_case "report: containerd-store lines are counted per registry and listed" c_report_containerd

c_report_classic() {
  new_runner reportclassic
  cat >"$E/state/journal" <<'EOF'
time="2026-10-09T21:35:33Z" level=debug msg="Trying to pull postgres from https://mirror.gcr.io/ v2"
time="2026-10-09T21:35:34Z" level=debug msg="Trying to pull node from https://mirror.gcr.io/ v2"
time="2026-10-09T21:35:35Z" level=debug msg="Trying to pull node from https://registry-1.docker.io/ v2"
EOF
  run_script --report
  expect_rc 0
  expect_out "resolved against mirror.gcr.io:  2"
  expect_out "resolved against Docker Hub (registry-1.docker.io):  1"
  expect_out 'msg="Trying to pull postgres from https://mirror.gcr.io/ v2"'
}
run_case "report: classic-store lines are counted per registry and listed" c_report_classic

c_report_no_lookalikes() {
  new_runner reportlookalike
  cat >"$E/state/journal" <<'EOF'
time="2026-10-09T21:35:33Z" level=debug msg=resolving host=mirror.gcr.io.evil.example method=HEAD url="https://mirror.gcr.io.evil.example/v2/x/manifests/1"
time="2026-10-09T21:35:34Z" level=debug msg=resolving host=notmirror.gcr.io method=HEAD url="https://notmirror.gcr.io/v2/x/manifests/1"
time="2026-10-09T21:35:35Z" level=debug msg="Trying to pull x from https://mirror.gcr.io.evil.example/ v2"
EOF
  run_script --report
  expect_rc 0
  expect_out "resolved against mirror.gcr.io:  0"
}
run_case "report: a host that merely starts or ends like the mirror is not counted" c_report_no_lookalikes

c_report_empty() {
  new_runner reportempty
  : >"$E/state/journal"
  run_script --report
  expect_rc 0
  expect_out "resolved against mirror.gcr.io:  0"
  expect_out "resolved against Docker Hub (registry-1.docker.io):  0"
}
run_case "report: an empty journal reports zero, it does not claim success" c_report_empty

c_report_no_journalctl() {
  new_runner reportnojournal
  rm "$E/bin/journalctl"
  run_script --report
  expect_rc 0
  expect_out "journalctl not found"
}
run_case "report: a missing journalctl is said out loud and does not fail the job" c_report_no_journalctl

c_report_journal_unreadable() {
  new_runner reportunreadable
  : >"$E/state/journal-fails"
  run_script --report
  expect_rc 0
  expect_out "could not read the docker journal"
}
run_case "report: an unreadable journal is said out loud and does not fail the job" c_report_journal_unreadable

c_report_changes_nothing() {
  new_runner reportnochange
  : >"$E/state/journal"
  run_script --report
  expect_eq "$(restarts)" "0" "restarts"
  [ ! -e "$CFG" ] || fail_with "--report wrote a config"
}
run_case "report: it never writes the config or restarts the daemon" c_report_changes_nothing

# ===========================================================================
# Summary
# ===========================================================================
TOTAL=$((PASSED + FAILED))
echo
echo "ci-docker-mirror_test: $PASSED passed, $FAILED failed, $SKIPPED skipped (of $((TOTAL + SKIPPED)) cases)"

if [ "$TOTAL" -eq 0 ]; then
  echo "ERROR: no case ran (filter '$FILTER' matched nothing); a suite that ran nothing proves nothing." >&2
  exit 2
fi

if [ "$FAILED" -gt 0 ]; then
  echo "failed:"
  printf '%s' "$FAILED_NAMES"
  exit 1
fi
exit 0
