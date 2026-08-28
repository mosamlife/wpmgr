#!/usr/bin/env bash
# scripts/with-machine-lock.sh
#
# Run a command under a machine-wide mutex, so a second invocation WAITS for
# the first instead of racing it.
#
#   scripts/with-machine-lock.sh NAME -- COMMAND [ARG...]
#
# WHY THIS EXISTS (GH #565). `make test-integration` starts a fresh Postgres
# testcontainer per test. Several agents work in parallel git worktrees on one
# machine and each runs that target, so two suites contend for ports. Measured
# directly: two overlapping runs, 14m36s and 11m39s elapsed, from two
# worktrees; #565 records the suite failing 2 of 3 runs on one machine in a
# single slice, one of them with `port "5432/tcp" not found`.
#
# The cost is not the wasted minutes. It is that a contention failure and a
# real regression LOOK IDENTICAL, so a genuine RLS regression gets waved
# through as "that flake again" — and .github/workflows/ci.yml deliberately
# does not run the integration package, so the local run is the only gate in
# front of it. Serialising the target makes a FAIL mean something again.
#
# WHY mkdir AND NOT flock OR shlock. Resolved with `command -v` on the machine
# this was written for (darwin 25.5.0, arm64), not from memory:
#
#   flock     ABSENT. It is util-linux. It does not ship on macOS at all.
#   timeout   ABSENT. Same story, and it has already burned this project once
#             by failing silently and reading as a pass — hence the bounded
#             polling loop below rather than wrapping anything in `timeout`.
#   shlock    PRESENT at /usr/bin/shlock, and it is a real mutex with real
#             PID-based staleness. It is still the WRONG pick: shlock is a BSD
#             utility, absent from ubuntu, and this script's self-test runs in
#             ci.yml on ubuntu. A lock that cannot be tested on the runner is
#             a lock nobody watches fail.
#   mkdir     PRESENT, POSIX, atomic on every filesystem this repo touches,
#             identical on darwin and ubuntu. That is the pick.
#
# `mkdir DIR` either creates the directory or fails, with no window in which
# two callers both believe they created it. That single property is the whole
# mutex. Everything else here is bookkeeping around it.
#
# EXIT CODES.
#   *   The command's own status, passed through unchanged, whatever it was.
#   64  Usage error (EX_USAGE): bad arguments, bad lock name, no command.
#   69  Unavailable (EX_UNAVAILABLE): the lock root or the lock itself could
#       not be created, or a tool this script needs is missing.
#   75  Timed out waiting (EX_TEMPFAIL), with a message on stderr naming what
#       it waited for and who held it.
#
# 75 is deliberately a code `go test` does not produce (it exits 1 for a test
# failure, 2 for a build failure), so "we never got to run" stays
# distinguishable from "we ran and it failed". If YOUR inner command can exit
# 75 on its own, read the stderr message, which is unambiguous.
#
# THE STATUS IS NEVER PIPED. This project's signature defect is a failure
# coerced into a plausible value, and `cmd | tail` reporting tail's status is
# that defect wearing a disguise; twenty-two instances have been found so far
# and four of them were inside code written to catch this exact class. So the
# command below runs bare, its status is captured on the very next line, the
# lock is released, and that status is what this script exits with. There is
# no pipe, no `&&` chain and no subshell anywhere on that path.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop with BSD ps/sed and on the ubuntu CI runner with the
# GNU ones. No mapfile, no associative arrays, no `timeout`, no EPOCHSECONDS.
#
# TUNING (environment):
#   WPMGR_LOCK_ROOT     directory holding the locks.
#                       Default /tmp/wpmgr-locks-$(id -u).
#   WPMGR_LOCK_TIMEOUT  seconds to wait before giving up. Default 3600.
#   WPMGR_LOCK_POLL     seconds between attempts. Default 5.
#   WPMGR_LOCK_NOTIFY   seconds between "still waiting" lines. Default 30.
#   WPMGR_LOCK_GRACE    seconds a lock may exist with no readable metadata
#                       before it is treated as a crashed acquire. Default 30.

set -uo pipefail

ME="$(basename "$0")"

die_usage() {
  echo "$ME: $1" >&2
  echo "usage: $ME NAME -- COMMAND [ARG...]" >&2
  exit 64
}

# ---- Arguments -------------------------------------------------------------

[ $# -ge 1 ] || die_usage "no lock name given"

NAME="$1"
shift

[ $# -ge 1 ] || die_usage "expected -- after the lock name"
[ "$1" = "--" ] || die_usage "expected -- after the lock name, got '$1'"
shift

[ $# -ge 1 ] || die_usage "no command given after --"

# The lock name becomes a path component, and this script `rm -rf`s that path
# when it reclaims a stale lock. An unvalidated directory built from an
# argument is precisely the shape that let a sibling tool in this repo
# `rm -rf` a home directory, so the name is checked against an allowlist
# rather than sanitised: no slash, no dot-dot, no empty string, nothing that
# can climb out of the lock root.
case "$NAME" in
  *[!A-Za-z0-9._-]*) die_usage "lock name '$NAME' has characters outside [A-Za-z0-9._-]" ;;
  "" )               die_usage "lock name is empty" ;;
  "." | ".." )       die_usage "lock name '$NAME' is a path traversal" ;;
esac

# ---- Tools this script depends on ------------------------------------------
#
# A step that cannot find its binary fails loudly here; it is never skipped.
# Staleness detection is the whole reason this cannot deadlock the machine, and
# it is `ps` that provides it. Without `ps` we would have to either assume every
# lock is live (deadlock) or assume it is dead (no mutex at all). Both are
# worse than refusing.
for tool in mkdir ps date rm mv; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$ME: required tool '$tool' not found on PATH; refusing to run unlocked" >&2
    exit 69
  fi
done

LOCK_ROOT="${WPMGR_LOCK_ROOT:-/tmp/wpmgr-locks-$(id -u)}"
TIMEOUT="${WPMGR_LOCK_TIMEOUT:-3600}"
POLL="${WPMGR_LOCK_POLL:-5}"
NOTIFY="${WPMGR_LOCK_NOTIFY:-30}"
GRACE="${WPMGR_LOCK_GRACE:-30}"

for pair in "TIMEOUT:$TIMEOUT" "POLL:$POLL" "NOTIFY:$NOTIFY" "GRACE:$GRACE"; do
  var="${pair%%:*}"
  val="${pair#*:}"
  case "$val" in
    *[!0-9]* | "") echo "$ME: WPMGR_LOCK_$var must be a whole number of seconds, got '$val'" >&2; exit 64 ;;
  esac
done
[ "$POLL" -gt 0 ] || { echo "$ME: WPMGR_LOCK_POLL must be greater than zero" >&2; exit 64; }

LOCK_DIR="$LOCK_ROOT/$NAME.lock"
META="$LOCK_DIR/meta"

if ! mkdir -p "$LOCK_ROOT" 2>/dev/null; then
  echo "$ME: cannot create lock root $LOCK_ROOT" >&2
  exit 69
fi

# ---- Helpers ---------------------------------------------------------------

now() { date +%s; }

# The process start time, whitespace-collapsed. Used together with the pid so a
# recycled pid cannot make a dead holder look alive. `ps -p` is used rather
# than `kill -0` because `kill -0` cannot distinguish "no such process" from
# "process owned by another user" — it returns non-zero for both, which would
# report a live holder as stale.
lstart_of() {
  ps -o lstart= -p "$1" 2>/dev/null | tr -s ' ' ' ' | sed 's/^ *//; s/ *$//'
}

meta_field() { # meta_field KEY
  [ -f "$META" ] || return 1
  sed -n "s/^$1=//p" "$META" 2>/dev/null | head -1
}

human_secs() { # human_secs N  ->  "3m12s"
  m=$(( $1 / 60 ))
  s=$(( $1 % 60 ))
  if [ "$m" -gt 0 ]; then echo "${m}m${s}s"; else echo "${s}s"; fi
}

# ---- Release ---------------------------------------------------------------
#
# HELD gates this. A run that timed out while waiting, or that was killed
# before it acquired, must never delete the lock the OTHER run is holding.
HELD=0
release() {
  [ "$HELD" = "1" ] || return 0
  # Defensive: only remove a lock whose metadata still names this process.
  owner="$(meta_field pid 2>/dev/null || true)"
  if [ -n "$owner" ] && [ "$owner" != "$$" ]; then
    echo "$ME: NOT releasing $LOCK_DIR — it is now held by pid $owner, not by me ($$)" >&2
    HELD=0
    return 0
  fi
  rm -rf "$LOCK_DIR" 2>/dev/null
  HELD=0
}

on_signal() { # on_signal SIGNAME SIGNUM
  echo "" >&2
  echo "$ME: caught SIG$1 — releasing $NAME lock and exiting" >&2
  release
  trap - EXIT
  exit $(( 128 + $2 ))
}

trap 'release' EXIT
trap 'on_signal INT 2'  INT
trap 'on_signal TERM 15' TERM
trap 'on_signal HUP 1'  HUP

# ---- Acquire ---------------------------------------------------------------

STARTED_WAITING="$(now)"
DEADLINE=$(( STARTED_WAITING + TIMEOUT ))
LAST_NOTICE="$STARTED_WAITING"
ANNOUNCED_WAIT=0

# Belt and braces against a clock that jumps backwards mid-wait: the loop is
# bounded by a wall-clock deadline AND by a poll count, so it terminates even
# if `date` misbehaves. `until ... sleep` with no cap is the shape that has
# killed the most agents on this project; this is not that shape.
MAX_POLLS=$(( TIMEOUT / POLL + 60 ))
POLLS=0
NOMETA_SINCE=0

while :; do
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    HELD=1
    {
      echo "pid=$$"
      echo "acquired=$(now)"
      echo "lstart=$(lstart_of $$)"
      echo "host=$(hostname 2>/dev/null || echo unknown)"
      echo "cwd=$PWD"
      echo "cmd=$*"
    } > "$META" 2>/dev/null
    break
  fi

  # Held by someone. Work out by whom, and whether they are still alive.
  holder_pid="$(meta_field pid || true)"
  holder_lstart="$(meta_field lstart || true)"
  holder_since="$(meta_field acquired || true)"
  holder_cwd="$(meta_field cwd || true)"
  holder_cmd="$(meta_field cmd || true)"

  stale_reason=""

  if [ -z "$holder_pid" ]; then
    # No readable metadata. Either the holder is between its mkdir and its
    # write (microseconds), or it died in that window and left a lock nobody
    # can ever account for. Distinguished by how long we have SEEN it in that
    # state, not assumed either way. NOMETA_SINCE is reset every time we do
    # read metadata, so a holder that is merely slow to write does not
    # accumulate credit towards being declared dead.
    if [ "$NOMETA_SINCE" = "0" ]; then
      NOMETA_SINCE="$(now)"
    fi
    if [ $(( $(now) - NOMETA_SINCE )) -ge "$GRACE" ]; then
      stale_reason="its metadata never appeared (crashed between mkdir and write; unreadable for ${GRACE}s)"
    fi
  elif ! ps -p "$holder_pid" >/dev/null 2>&1; then
    NOMETA_SINCE=0
    stale_reason="holder pid $holder_pid is no longer running"
  else
    NOMETA_SINCE=0
    current_lstart="$(lstart_of "$holder_pid")"
    if [ -n "$holder_lstart" ] && [ -n "$current_lstart" ] && [ "$holder_lstart" != "$current_lstart" ]; then
      stale_reason="pid $holder_pid was recycled (started '$current_lstart', lock recorded '$holder_lstart')"
    fi
  fi

  if [ -n "$stale_reason" ]; then
    # Reclaiming is NOISY, never silent — a lock that vanishes without a word
    # is how you end up debugging the wrong thing for an hour.
    #
    # The reclaim itself is made atomic by `mv`: two waiters that both decide
    # the lock is stale will both try to rename it out of the way, and the
    # rename can only succeed once, so exactly one of them clears it. The
    # loser's mv fails, it loops, and it finds the lock either gone or freshly
    # taken. Without this, both would rm -rf and both would then mkdir, and
    # the mutex would have quietly let two runs through.
    salvage="$LOCK_DIR.stale.$$.$(now)"
    if mv "$LOCK_DIR" "$salvage" 2>/dev/null; then
      echo "$ME: RECLAIMING STALE LOCK '$NAME' — $stale_reason" >&2
      [ -n "$holder_cmd" ] && echo "$ME:   dead holder ran: $holder_cmd" >&2
      [ -n "$holder_cwd" ] && echo "$ME:   dead holder cwd: $holder_cwd" >&2
      echo "$ME:   removing $LOCK_DIR and taking it" >&2
      rm -rf "$salvage" 2>/dev/null
    fi
    continue
  fi

  # Live holder. Say so, and keep saying so: ten minutes of silence is
  # indistinguishable from a hang, which is requirement one.
  n="$(now)"
  waited=$(( n - STARTED_WAITING ))

  if [ "$ANNOUNCED_WAIT" = "0" ]; then
    held_for="?"
    [ -n "$holder_since" ] && held_for="$(human_secs $(( n - holder_since )))"
    echo "$ME: waiting for lock '$NAME' — held by pid ${holder_pid:-?} for $held_for" >&2
    [ -n "$holder_cwd" ] && echo "$ME:   holder cwd: $holder_cwd" >&2
    [ -n "$holder_cmd" ] && echo "$ME:   holder cmd: $holder_cmd" >&2
    echo "$ME:   will wait up to $(human_secs "$TIMEOUT"), reporting every ${NOTIFY}s" >&2
    ANNOUNCED_WAIT=1
    LAST_NOTICE="$n"
  elif [ $(( n - LAST_NOTICE )) -ge "$NOTIFY" ]; then
    held_for="?"
    [ -n "$holder_since" ] && held_for="$(human_secs $(( n - holder_since )))"
    echo "$ME: still waiting for '$NAME' after $(human_secs "$waited") — pid ${holder_pid:-?} has held it $held_for" >&2
    LAST_NOTICE="$n"
  fi

  POLLS=$(( POLLS + 1 ))
  if [ "$n" -ge "$DEADLINE" ] || [ "$POLLS" -ge "$MAX_POLLS" ]; then
    echo "$ME: TIMED OUT after $(human_secs "$waited") waiting for lock '$NAME'" >&2
    echo "$ME:   still held by pid ${holder_pid:-?}${holder_cmd:+ running: $holder_cmd}" >&2
    echo "$ME:   nothing was run. Raise WPMGR_LOCK_TIMEOUT, or wait for that run to finish." >&2
    exit 75
  fi

  sleep "$POLL"
done

# ---- Run -------------------------------------------------------------------
#
# Bare invocation. Status captured immediately. No pipe, no chain, no subshell.
"$@"
status=$?

release
exit "$status"
