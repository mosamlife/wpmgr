#!/usr/bin/env bash
# scripts/with-machine-lock_test.sh
#
# The regression suite for scripts/with-machine-lock.sh.
#
# WHY THIS EXISTS. The lock is the only thing standing between two parallel
# worktrees and a `make test-integration` run that fails for reasons that have
# nothing to do with the code. A mutex that is subtly wrong is worse than no
# mutex, because people trust it: they stop reading "flake?" as a question. So
# every property the lock claims is asserted here, including the ones that are
# only interesting when they DO NOT fire.
#
# RUN IT:
#   scripts/with-machine-lock_test.sh            # everything
#   scripts/with-machine-lock_test.sh stale      # only cases matching "stale"
#
# Point it at a different implementation to prove the suite is not vacuous
# (break the lock in a copy, watch the suite go red):
#   WPMGR_MACHINE_LOCK_SCRIPT=/tmp/broken-lock.sh \
#     scripts/with-machine-lock_test.sh
#
# NOTHING HERE RUNS THE INTEGRATION SUITE. The inner commands are `echo`,
# `false`, `sleep 2` and friends. The real suite takes about sixteen minutes,
# needs Docker, and would test testcontainers rather than this lock. The whole
# point of putting the mutex in its own script is that it can be tested in
# seconds against a trivial payload.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it runs the same
# on a darwin laptop with BSD ps/sed and on the ubuntu CI runner with the GNU
# ones. No mapfile, no associative arrays, no `timeout`, no flock, no shlock.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOCK="${WPMGR_MACHINE_LOCK_SCRIPT:-$HERE/with-machine-lock.sh}"
FILTER="${1:-}"

if [ ! -f "$LOCK" ]; then
  echo "no lock script at $LOCK" >&2
  exit 2
fi
if [ ! -x "$LOCK" ]; then
  echo "lock script at $LOCK is not executable" >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-machine-lock.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
CASE=""
ROOT=""

# ---- Harness ---------------------------------------------------------------

begin() { # begin NAME
  CASE="$1"
  ROOT="$WORK/$(echo "$1" | tr -c 'A-Za-z0-9._-' '-')"
  mkdir -p "$ROOT"
}

skip_case() { # returns 0 when the filter excludes this case
  [ -n "$FILTER" ] || return 1
  case "$CASE" in *"$FILTER"*) return 1 ;; *) return 0 ;; esac
}

ok()   { PASSED=$(( PASSED + 1 )); echo "  ok   $CASE — $1"; }
fail() { FAILED=$(( FAILED + 1 )); echo "  FAIL $CASE — $1"; }

expect_status() { # expect_status WANT GOT WHAT
  if [ "$2" = "$1" ]; then ok "$3 (exit $2)"; else fail "$3: wanted exit $1, got $2"; fi
}

expect_contains() { # expect_contains FILE PATTERN WHAT
  if grep -q "$2" "$1" 2>/dev/null; then
    ok "$3"
  else
    fail "$3: output did not match /$2/"
    sed 's/^/       | /' "$1" | head -12
  fi
}

expect_absent() { # expect_absent FILE PATTERN WHAT
  if grep -q "$2" "$1" 2>/dev/null; then
    fail "$3: output unexpectedly matched /$2/"
    sed 's/^/       | /' "$1" | head -12
  else
    ok "$3"
  fi
}

# Run the lock script with this case's root. Never pipes: the status under test
# is the status this suite reads.
lockrun() { # lockrun OUTFILE ARGS...
  out="$1"; shift
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" "$@" > "$out" 2>&1
  return $?
}

now() { date +%s; }

# ============================================================================
# 1. The exit status survives. This is the requirement the project's history
#    says is most likely to be got wrong: a wrapper that returns its own 0, or
#    one whose status came through a pipe.
# ============================================================================

begin "status-zero"
if ! skip_case; then
  lockrun "$ROOT/o" itest -- echo hello
  expect_status 0 "$?" "a succeeding command exits 0"
  expect_contains "$ROOT/o" "hello" "the command's stdout reaches the caller"
fi

begin "status-false"
if ! skip_case; then
  lockrun "$ROOT/o" itest -- false
  expect_status 1 "$?" "false exits 1, not 0"
fi

begin "status-arbitrary"
if ! skip_case; then
  lockrun "$ROOT/o" itest -- sh -c 'exit 42'
  expect_status 42 "$?" "an arbitrary non-zero status passes through unchanged"
fi

begin "status-notfound"
if ! skip_case; then
  lockrun "$ROOT/o" itest -- this-command-does-not-exist-wpmgr
  expect_status 127 "$?" "a missing command still reports 127"
fi

# ============================================================================
# 2. The lock is released, whether the command passed or failed. A lock leaked
#    on the failure path would wedge every later run behind a dead holder.
# ============================================================================

begin "release-after-success"
if ! skip_case; then
  lockrun "$ROOT/o" itest -- true
  if [ -d "$ROOT/itest.lock" ]; then fail "lock left behind after success"; else ok "lock released after success"; fi
fi

begin "release-after-failure"
if ! skip_case; then
  lockrun "$ROOT/o" itest -- false
  if [ -d "$ROOT/itest.lock" ]; then fail "lock left behind after failure"; else ok "lock released after failure"; fi
fi

# ============================================================================
# 3. Serialisation — the whole point. Two concurrent runs must not overlap.
# ============================================================================

begin "serialise-concurrent"
if ! skip_case; then
  trace="$ROOT/trace"
  : > "$trace"
  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_POLL=1 "$LOCK" itest -- \
    sh -c "echo A-in >> '$trace'; sleep 2; echo A-out >> '$trace'" > "$ROOT/oa" 2>&1 &
  pa=$!
  sleep 1
  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_POLL=1 "$LOCK" itest -- \
    sh -c "echo B-in >> '$trace'; sleep 2; echo B-out >> '$trace'" > "$ROOT/ob" 2>&1 &
  pb=$!
  wait $pa; sa=$?
  wait $pb; sb=$?

  order="$(tr '\n' ' ' < "$trace" | sed 's/ *$//')"
  # Serialised means one section closes before the other opens. Either order is
  # fine; interleaving is not.
  case "$order" in
    "A-in A-out B-in B-out" | "B-in B-out A-in A-out")
      ok "two concurrent runs serialised (order: $order)" ;;
    *)
      fail "two concurrent runs interleaved (order: $order)" ;;
  esac
  expect_status 0 "$sa" "first concurrent run exits cleanly"
  expect_status 0 "$sb" "second concurrent run exits cleanly"

  # Requirement one: a waiting run must SAY it is waiting.
  if grep -q "waiting for lock" "$ROOT/oa" 2>/dev/null || grep -q "waiting for lock" "$ROOT/ob" 2>/dev/null; then
    ok "the waiting run announced that it was waiting"
  else
    fail "neither run said it was waiting — silence is indistinguishable from a hang"
  fi
  if grep -q "held by pid [0-9]" "$ROOT/oa" 2>/dev/null || grep -q "held by pid [0-9]" "$ROOT/ob" 2>/dev/null; then
    ok "the waiting run named the holder's pid"
  else
    fail "the waiting run did not name who held the lock"
  fi
fi

# ============================================================================
# 4. The wait is bounded, and expiry is distinct from failure.
# ============================================================================

begin "timeout-exits-75"
if ! skip_case; then
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- sleep 6 > "$ROOT/holder" 2>&1 &
  hp=$!
  sleep 1
  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 \
    "$LOCK" itest -- echo THIS-MUST-NOT-RUN > "$ROOT/o" 2>&1
  st=$?
  expect_status 75 "$st" "a wait that expires exits 75, distinct from the suite's own codes"
  expect_contains "$ROOT/o" "TIMED OUT" "expiry says it timed out"
  expect_contains "$ROOT/o" "itest" "expiry names what it waited for"
  expect_absent "$ROOT/o" "THIS-MUST-NOT-RUN" "the command never ran"

  # A waiter that gave up must not steal the lock it failed to get.
  if [ -d "$ROOT/itest.lock" ]; then
    ok "the timed-out waiter left the live holder's lock alone"
  else
    fail "the timed-out waiter deleted the live holder's lock"
  fi
  wait $hp 2>/dev/null
fi

# ============================================================================
# 5. Stale locks are reclaimable, and reclaiming is noisy.
# ============================================================================

begin "stale-dead-pid"
if ! skip_case; then
  mkdir -p "$ROOT/itest.lock"
  # A pid that is definitely not running. Start a shell, let it exit, reuse its
  # pid number — cheaper and more honest than picking a big number and hoping.
  sh -c 'exit 0' &
  deadpid=$!
  wait $deadpid 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\nhost=test\ncwd=/nowhere\ncmd=dead-holder-command\n' \
    "$deadpid" "$(now)" > "$ROOT/itest.lock/meta"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=10 WPMGR_LOCK_POLL=1 \
    "$LOCK" itest -- echo RECLAIMED > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "a lock held by a dead pid does not deadlock the machine"
  expect_contains "$ROOT/o" "RECLAIM" "reclaiming a stale lock is announced, never silent"
  expect_contains "$ROOT/o" "dead-holder-command" "the reclaim names what the dead holder was running"
  expect_contains "$ROOT/o" "RECLAIMED" "the command ran after the reclaim"
fi

begin "stale-recycled-pid"
if ! skip_case; then
  # The pid is alive — it is this very test process — but its start time does
  # not match what the lock recorded, which is what pid reuse looks like. A
  # lock that only checked `ps -p` would call this live and wait forever.
  mkdir -p "$ROOT/itest.lock"
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\nhost=test\ncwd=/nowhere\ncmd=recycled-holder\n' \
    "$$" "$(now)" > "$ROOT/itest.lock/meta"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=6 WPMGR_LOCK_POLL=1 \
    "$LOCK" itest -- echo RAN-AFTER-RECYCLE > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "a recycled pid is detected as stale, not mistaken for a live holder"
  expect_contains "$ROOT/o" "recycled" "the reclaim says the pid was recycled"
fi

begin "stale-no-metadata"
if ! skip_case; then
  # A holder that died between its mkdir and its metadata write. Nobody can
  # ever account for this lock, so it must be reclaimable after the grace
  # period rather than wedging the machine forever.
  mkdir -p "$ROOT/itest.lock"
  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=10 WPMGR_LOCK_POLL=1 WPMGR_LOCK_GRACE=1 \
    "$LOCK" itest -- echo RAN-AFTER-GRACE > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "a lock whose metadata never appeared is reclaimed after the grace period"
  expect_contains "$ROOT/o" "metadata never appeared" "the reclaim explains why"
fi

# ============================================================================
# 6. It must NOT over-fire. A guard that blocks correct work gets switched off,
#    and then it guards nothing. These are the cases that must stay green.
# ============================================================================

begin "no-overfire-grace-not-yet-elapsed"
if ! skip_case; then
  # Same metadata-less lock as above, but the grace period has NOT elapsed. The
  # lock must be respected, not stolen from a holder that is merely slow to
  # write its metadata. Stealing here would let two suites run at once, which
  # is the exact bug this script exists to prevent.
  mkdir -p "$ROOT/itest.lock"
  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 WPMGR_LOCK_GRACE=600 \
    "$LOCK" itest -- echo MUST-NOT-RUN > "$ROOT/o" 2>&1
  st=$?
  expect_status 75 "$st" "a metadata-less lock inside its grace period is respected, not stolen"
  expect_absent "$ROOT/o" "MUST-NOT-RUN" "the command did not run while the lock was still credible"
fi

begin "no-overfire-distinct-names"
if ! skip_case; then
  # Two different lock names are two different mutexes. The integration suite
  # must not block an unrelated target.
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" alpha -- sleep 3 > "$ROOT/oa" 2>&1 &
  pa=$!
  sleep 1
  start="$(now)"
  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 \
    "$LOCK" beta -- echo INDEPENDENT > "$ROOT/o" 2>&1
  st=$?
  elapsed=$(( $(now) - start ))
  expect_status 0 "$st" "a different lock name is not blocked by an unrelated holder"
  expect_contains "$ROOT/o" "INDEPENDENT" "the independent command ran"
  if [ "$elapsed" -le 1 ]; then
    ok "the independent run did not wait (${elapsed}s)"
  else
    fail "the independent run waited ${elapsed}s for an unrelated lock"
  fi
  wait $pa 2>/dev/null
fi

begin "no-overfire-sequential"
if ! skip_case; then
  # Back-to-back runs must not block each other, and must not print a wait
  # notice. This is the common case: one agent, one machine, no contention.
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- echo one > "$ROOT/o1" 2>&1
  s1=$?
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- echo two > "$ROOT/o2" 2>&1
  s2=$?
  expect_status 0 "$s1" "first sequential run succeeds"
  expect_status 0 "$s2" "second sequential run succeeds"
  expect_absent "$ROOT/o2" "waiting for lock" "an uncontended run prints no wait noise"
fi

# ============================================================================
# 7. Refusals. Bad input fails loudly with a usage code; it never runs the
#    command unlocked, and it never touches a path outside the lock root.
# ============================================================================

begin "usage-no-args"
if ! skip_case; then
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" > "$ROOT/o" 2>&1
  expect_status 64 "$?" "no arguments is a usage error"
fi

begin "usage-missing-separator"
if ! skip_case; then
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest echo hi > "$ROOT/o" 2>&1
  expect_status 64 "$?" "a missing -- separator is a usage error, not a silent run"
  expect_absent "$ROOT/o" "^hi$" "the command did not run without a separator"
fi

begin "usage-no-command"
if ! skip_case; then
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- > "$ROOT/o" 2>&1
  expect_status 64 "$?" "nothing after -- is a usage error"
fi

begin "usage-bad-name-slash"
if ! skip_case; then
  # The lock name becomes a path that the script rm -rf's on reclaim. A name
  # that can climb out of the lock root must be refused outright.
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" "../../etc/passwd" -- echo pwned > "$ROOT/o" 2>&1
  expect_status 64 "$?" "a lock name containing a path separator is refused"
  expect_absent "$ROOT/o" "pwned" "the command did not run under a traversing name"
fi

begin "usage-bad-name-dotdot"
if ! skip_case; then
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" ".." -- echo pwned > "$ROOT/o" 2>&1
  expect_status 64 "$?" "a lock name of .. is refused"
fi

begin "usage-bad-timeout"
if ! skip_case; then
  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=soon "$LOCK" itest -- echo hi > "$ROOT/o" 2>&1
  expect_status 64 "$?" "a non-numeric timeout is refused rather than coerced to zero"
fi

# ============================================================================

echo ""
echo "passed $PASSED, failed $FAILED"
if [ "$FAILED" -gt 0 ]; then
  exit 1
fi
if [ "$PASSED" -eq 0 ]; then
  # A suite that asserted nothing must go red, not green. If a filter matched
  # no cases, or every case was skipped, that is a broken invocation and
  # reporting success over it is this project's signature defect.
  echo "no cases ran${FILTER:+ (filter: $FILTER)} — refusing to report success" >&2
  exit 2
fi
exit 0
