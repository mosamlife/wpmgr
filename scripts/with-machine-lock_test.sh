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
# 5b. The three fail-open defects found in review of the first draft (PR #575).
#
#     All three had the same shape and the same direction, which is the worst
#     one for a mutex: the lock silently ADMITS a second run alongside a live
#     one. A lock people trust that lets two suites through is worse than no
#     lock, because they stop checking.
#
#     Each case below fails against the pre-review implementation and passes
#     against the current one. None of them runs the integration suite; the
#     protected command is `sleep` or `echo`, as everywhere else here.
# ============================================================================

begin "killed-wrapper-live-child"
if ! skip_case; then
  # DEFECT 1. SIGKILL cannot be trapped, and agents on this machine get killed
  # mid-run — that is the origin of the project's "commit before the slow
  # suite" rule. The wrapper dies; `go test` and its containers do not. The
  # first draft recorded only the wrapper's pid, so the next invocation saw a
  # dead pid, correctly concluded the WRAPPER was gone, and admitted a second
  # suite alongside a live one.
  #
  # The property: a live test process means the lock is not stale, regardless
  # of what happened to the wrapper.
  # A duration unique to this case: `pgrep -f` is machine-wide, so a shared
  # `sleep 30` would match another case's leftover and this one would pass or
  # fail on the wrong process.
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- sleep 4177 > "$ROOT/holder" 2>&1 &
  wp=$!

  # Bounded wait for the lock to record the command's pid. No unbounded loop.
  child=""
  i=0
  while [ "$i" -lt 10 ]; do
    child="$(sed -n 's/^child=//p' "$ROOT/itest.lock/meta" 2>/dev/null | head -1)"
    [ -n "$child" ] && break
    sleep 1
    i=$(( i + 1 ))
  done

  if [ -z "$child" ]; then
    fail "the lock never recorded the command's pid — a killed wrapper strands a live suite"
  else
    ok "the lock records the command's pid ($child), not only the wrapper's"

    kill -9 "$wp" 2>/dev/null
    wait "$wp" 2>/dev/null

    # The case only proves something if the child really did outlive the
    # wrapper. Assert that rather than assuming it.
    if ps -p "$child" >/dev/null 2>&1; then
      ok "the command outlived the SIGKILLed wrapper (pid $child still running)"
    else
      fail "the command died with the wrapper — this case proves nothing as written"
    fi
    if ps -p "$wp" >/dev/null 2>&1; then
      fail "the wrapper survived SIGKILL — this case proves nothing as written"
    else
      ok "the wrapper is gone (pid $wp), so only the child keeps the lock honest"
    fi

    # GRACE=1 so the metadata-less path cannot be what refuses; the refusal has
    # to come from the live child.
    WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 WPMGR_LOCK_GRACE=1 \
      "$LOCK" itest -- echo SECOND-SUITE-ADMITTED > "$ROOT/o" 2>&1
    st=$?
    expect_status 75 "$st" "a second run REFUSES while the first run's command is still alive"
    expect_absent "$ROOT/o" "SECOND-SUITE-ADMITTED" "no second suite started alongside a live one"
    expect_absent "$ROOT/o" "RECLAIM" "the live child's lock was not reclaimed as stale"

    # Kill the grandchild too, not just the recorded shell: leaving it behind
    # pollutes every later case. By PARENTAGE, not by name — `pkill -f` is
    # machine-wide and would reach a developer's unrelated `sleep 4177`.
    # Descendants first, while the parentage still exists to find them by.
    for _d in $(pgrep -P "$child" 2>/dev/null); do
      kill -9 "$_d" 2>/dev/null
    done
    kill -9 "$child" 2>/dev/null
  fi
  rm -rf "$ROOT/itest.lock" 2>/dev/null
fi

begin "killed-wrapper-dead-child-still-reclaimable"
if ! skip_case; then
  # The over-fire complement of the case above, and the one that decides
  # whether the fix is usable. Recording a child pid must not make locks
  # immortal: when the wrapper AND the command are both gone, the lock is still
  # stale and must still be reclaimed, or a killed run wedges the machine for
  # WPMGR_LOCK_TIMEOUT and everyone switches the lock off.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  d1=$!
  wait $d1 2>/dev/null
  sh -c 'exit 0' &
  d2=$!
  wait $d2 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\nhost=test\ncwd=/nowhere\ncmd=dead-holder\nchild=%s\nclstart=Mon Jan  1 00:00:00 2001\n' \
    "$d1" "$(now)" "$d2" > "$ROOT/itest.lock/meta"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=10 WPMGR_LOCK_POLL=1 \
    "$LOCK" itest -- echo RECLAIMED-BOTH-DEAD > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "a lock whose wrapper AND command are both dead is still reclaimable"
  expect_contains "$ROOT/o" "both gone" "the reclaim says both pids were checked"
  expect_contains "$ROOT/o" "RECLAIMED-BOTH-DEAD" "the command ran after the reclaim"
fi

begin "meta-write-unwritable"
if ! skip_case; then
  # DEFECT 2. The first draft's metadata write was both suppressed
  # (`2>/dev/null`) and unchecked, so a failed write still reached the
  # protected command. Another invocation then saw a metadata-less lock, waited
  # out the grace period, and admitted a concurrent suite.
  #
  # Forcing the failure cheaply: run under `umask 0777` so `mkdir` still
  # succeeds but creates the lock directory mode 000, and nothing can be
  # created inside it. The redirection below is evaluated by THIS shell, under
  # the normal umask, so the transcript stays readable.
  : > "$ROOT/o"
  ( umask 0777
    WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 \
      "$LOCK" itest -- echo RAN-WITHOUT-METADATA ) > "$ROOT/o" 2>&1
  st=$?
  expect_status 69 "$st" "a lock whose metadata cannot be written refuses to run the command"
  expect_absent "$ROOT/o" "RAN-WITHOUT-METADATA" "the command did not run under an unaccountable lock"
  expect_contains "$ROOT/o" "cannot write lock metadata" "the write failure is reported, never suppressed"

  # And the refusal must not leave the unaccountable lock behind. A leaked
  # metadata-less lock is the same defect one step later: the next run waits
  # out the grace period and reclaims it. `rm -rf` cannot descend a mode-000
  # directory, so this assertion is what proves the release actually released.
  if [ -d "$ROOT/itest.lock" ]; then
    fail "the failed acquire leaked a metadata-less lock at $ROOT/itest.lock"
    chmod u+rwx "$ROOT/itest.lock" 2>/dev/null
    rm -rf "$ROOT/itest.lock" 2>/dev/null
  else
    ok "the failed acquire released its lock rather than leaving one nobody can account for"
  fi
fi

begin "reclaim-not-raced"
if ! skip_case; then
  # DEFECT 3. Two waiters could both classify one lock as stale. The first
  # moved it, removed it and acquired a fresh lock; the second, still acting on
  # its now-obsolete verdict, moved and removed THAT live lock, and both
  # commands ran. Reclaimers are now serialised by a second mkdir mutex and
  # re-derive staleness inside it.
  #
  # Set up a genuinely stale lock AND a reclaim already in progress by a live
  # process (this test process). The waiter must not reclaim in parallel.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  deadpid=$!
  wait $deadpid 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\nhost=test\ncwd=/nowhere\ncmd=dead-holder\n' \
    "$deadpid" "$(now)" > "$ROOT/itest.lock/meta"

  mkdir -p "$ROOT/itest.reclaim"
  printf 'pid=%s\nlstart=%s\n' \
    "$$" "$(ps -o lstart= -p $$ 2>/dev/null | tr -s ' ' ' ' | sed 's/^ *//; s/ *$//')" \
    > "$ROOT/itest.reclaim/by"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 \
    "$LOCK" itest -- echo RECLAIMED-IN-PARALLEL > "$ROOT/o" 2>&1
  st=$?
  expect_status 75 "$st" "a reclaim already under way by a live process is waited for, not raced"
  expect_absent "$ROOT/o" "RECLAIMED-IN-PARALLEL" "the waiter did not reclaim behind another reclaimer's back"
  expect_contains "$ROOT/o" "reclaiming the stale" "the waiter says why it is waiting"
  rm -rf "$ROOT/itest.reclaim" "$ROOT/itest.lock" 2>/dev/null
fi

begin "reclaim-marker-abandoned-is-cleared"
if ! skip_case; then
  # The over-fire complement of the case above. A reclaimer killed mid-reclaim
  # must not wedge the machine forever: its marker names a pid, and a reclaimer
  # holds no command, so a dead one is provably doing nothing. Clearing it is
  # safe in a way that clearing a LOCK is not, and the winner still revalidates
  # under the marker before removing anything.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  deadpid=$!
  wait $deadpid 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\nhost=test\ncwd=/nowhere\ncmd=dead-holder\n' \
    "$deadpid" "$(now)" > "$ROOT/itest.lock/meta"

  sh -c 'exit 0' &
  deadreclaimer=$!
  wait $deadreclaimer 2>/dev/null
  mkdir -p "$ROOT/itest.reclaim"
  printf 'pid=%s\nlstart=Mon Jan  1 00:00:00 2001\n' "$deadreclaimer" > "$ROOT/itest.reclaim/by"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=10 WPMGR_LOCK_POLL=1 \
    "$LOCK" itest -- echo RAN-AFTER-ABANDONED-RECLAIM > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "an abandoned reclaim marker does not wedge the lock forever"
  expect_contains "$ROOT/o" "abandoned reclaim marker" "clearing an abandoned marker is announced, never silent"
  expect_contains "$ROOT/o" "RAN-AFTER-ABANDONED-RECLAIM" "the command ran once the marker was cleared"
  if [ -d "$ROOT/itest.reclaim" ]; then
    fail "the reclaim marker was left behind"
  else
    ok "the reclaim marker was removed after the reclaim finished"
  fi
fi

# ============================================================================
# 5c. The second review round, which found defects 1 and 2 again one step in.
#     Same two shapes, narrower windows. These assert the ORDERING fix, not a
#     smaller gap: the identifier now exists before the thing it identifies.
# ============================================================================

begin "spawn-before-record-window"
if ! skip_case; then
  # DEFECT 4. Recording the command's pid AFTER spawning it leaves a window: a
  # SIGKILL between `&` and the append leaves a running command that nothing in
  # the lock names, and the next invocation reclaims and starts a second suite.
  #
  # Reconstructing that state deterministically rather than racing for it: run
  # a real holder, then strip `child=`/`clstart=` from its metadata. What is
  # left is byte-for-byte the state the lock is in during that window — the
  # wrapper's pid is recorded, the command's is not, and the command is running.
  # Then SIGKILL the wrapper, exactly as the finding describes.
  #
  # The lock must still refuse, and it can only do so via the token, which was
  # written before the fork.
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- sleep 4379 > "$ROOT/holder" 2>&1 &
  wp=$!

  tok=""
  i=0
  while [ "$i" -lt 10 ]; do
    tok="$(sed -n 's/^token=//p' "$ROOT/itest.lock/meta" 2>/dev/null | head -1)"
    [ -n "$tok" ] && break
    sleep 1
    i=$(( i + 1 ))
  done

  if [ -z "$tok" ]; then
    fail "the lock recorded no token, so it cannot identify a run it has not yet recorded the pid of"
  else
    ok "the lock recorded a token ($tok) before forking anything"

    # Collapse to the pre-append state.
    grep -v '^child=' "$ROOT/itest.lock/meta" | grep -v '^clstart=' > "$ROOT/meta.stripped"
    cp "$ROOT/meta.stripped" "$ROOT/itest.lock/meta"
    if grep -q '^child=' "$ROOT/itest.lock/meta" 2>/dev/null; then
      fail "the child pid was not stripped — this case would prove nothing"
    else
      ok "metadata reduced to the spawn-to-record window (wrapper recorded, command not)"
    fi

    kill -9 "$wp" 2>/dev/null
    wait "$wp" 2>/dev/null

    if ps -p "$wp" >/dev/null 2>&1; then
      fail "the wrapper survived SIGKILL — this case proves nothing as written"
    else
      ok "the wrapper is gone (pid $wp), and nothing in the metadata names the command"
    fi

    WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 WPMGR_LOCK_GRACE=1 \
      "$LOCK" itest -- echo ADMITTED-IN-SPAWN-WINDOW > "$ROOT/o" 2>&1
    st=$?
    expect_status 75 "$st" "a run killed inside the spawn-to-record window still holds the lock"
    expect_absent "$ROOT/o" "ADMITTED-IN-SPAWN-WINDOW" "no second suite started in the spawn-to-record window"
    expect_absent "$ROOT/o" "RECLAIM" "the token kept the lock from being read as stale"

    # The token is unique to this run, so matching on it is safe; its
    # descendants are then reached by parentage rather than by payload name.
    for _t in $(pgrep -f "$tok" 2>/dev/null); do
      for _d in $(pgrep -P "$_t" 2>/dev/null); do
        kill -9 "$_d" 2>/dev/null
      done
      kill -9 "$_t" 2>/dev/null
    done
  fi
  rm -rf "$ROOT/itest.lock" 2>/dev/null
fi

begin "spawning-without-child-is-held"
if ! skip_case; then
  # The fork-to-exec backstop. Between write_meta and record_child both
  # identifiers are blind: `child=` is not written yet and the token is not in
  # the child's argv until it execs. A child stopped or delayed through that
  # interval is invisible to two consecutive assessments, so a waiter would
  # reclaim the lock and the command would resume beside the new holder.
  #
  # A lock carrying `spawning=1` with no `child=` must therefore read as HELD
  # even though every liveness check comes back negative — a dead wrapper, a
  # token nothing runs under, and no recorded child.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  deadpid=$!
  wait $deadpid 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\ntoken=wpmgrlock-nothing-runs-under-this-0001\nspawning=1\nhost=test\ncwd=/nowhere\ncmd=died-while-starting\n' \
    "$deadpid" "$(now)" > "$ROOT/itest.lock/meta"

  # GRACE well beyond the wait, so what is under test is the HOLD rather than
  # the bound below it.
  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 WPMGR_LOCK_GRACE=600 \
    "$LOCK" itest -- echo ADMITTED-OVER-SPAWNING > "$ROOT/o" 2>&1
  st=$?
  expect_status 75 "$st" "a lock that died while starting its command is held, not reclaimed"
  expect_absent "$ROOT/o" "ADMITTED-OVER-SPAWNING" "no second suite ran over a possibly-live command"
  expect_contains "$ROOT/o" "died while starting its" "the hold explains itself and names the remedy"

  # LOUDNESS IS PART OF THE PROPERTY, NOT A NICETY. The explanation used to
  # print only in the timed-out branch, so with the default WPMGR_LOCK_TIMEOUT
  # of 3600 a waiter saw the ordinary "held by pid N" line — indistinguishable
  # from a live suite — for an hour. The population here is agents, and a
  # blocked agent gets killed, so it was silent for exactly the people who hit
  # it. Assert the NOTE reaches the WAIT notice, before any timeout.
  if grep -q "waiting for lock" "$ROOT/o" 2>/dev/null; then
    ok "the waiter announced the wait"
  else
    fail "the waiter never announced the wait, so this case cannot test the notice"
  fi
  wait_line="$(grep -n 'waiting for lock' "$ROOT/o" | head -1 | cut -d: -f1)"
  note_line="$(grep -n 'died while starting its' "$ROOT/o" | head -1 | cut -d: -f1)"
  timeout_line="$(grep -n 'TIMED OUT' "$ROOT/o" | head -1 | cut -d: -f1)"
  if [ -n "$note_line" ] && [ -n "$wait_line" ] && [ "$note_line" -gt "$wait_line" ] \
     && { [ -z "$timeout_line" ] || [ "$note_line" -lt "$timeout_line" ]; }; then
    ok "the explanation reaches the wait notice, not only the timeout an hour later"
  else
    fail "the explanation appears only at timeout (wait=$wait_line note=$note_line timeout=$timeout_line)"
  fi
  rm -rf "$ROOT/itest.lock" 2>/dev/null
fi

begin "spawning-hold-is-bounded-by-grace"
if ! skip_case; then
  # THE BOUND. Holding such a lock forever was justified on two claims that
  # were both measured false: the interval is a median 7.71 ms rather than
  # microseconds, and the hold covered roughly 3x the fork-to-exec interval it
  # protects (median 2.42 ms, max 3.46 ms) — the excess being the PRE-FORK
  # part, where no child exists at all. So a permanent hold wedged locks over
  # runs that had provably started nothing, on a strict superset of the
  # fail-open's conditions.
  #
  # `spawning=1` with no `child=` cannot legitimately outlast a forked sh's
  # exec, so after GRACE it is declared stale like any other unaccountable
  # lock. Same metadata as the case above; only GRACE differs.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  deadpid=$!
  wait $deadpid 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\ntoken=wpmgrlock-nothing-runs-under-this-0003\nspawning=1\nhost=test\ncwd=/nowhere\ncmd=died-while-starting\n' \
    "$deadpid" "$(now)" > "$ROOT/itest.lock/meta"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=10 WPMGR_LOCK_POLL=1 WPMGR_LOCK_GRACE=1 \
    "$LOCK" itest -- echo RECLAIMED-AFTER-SPAWNING-GRACE > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "the spawning hold expires after the grace period instead of wedging the lock"
  expect_contains "$ROOT/o" "died while starting its command" "the reclaim names why the lock was held first"
  expect_contains "$ROOT/o" "RECLAIMED-AFTER-SPAWNING-GRACE" "the command ran once the hold expired"
  if [ -d "$ROOT/itest.lock" ]; then
    fail "the lock survived its own reclaim"
    rm -rf "$ROOT/itest.lock" 2>/dev/null
  else
    ok "the reclaimed lock was released normally afterwards"
  fi
fi

begin "spawning-with-child-recorded-is-reclaimable"
if ! skip_case; then
  # The over-fire complement, and the one that keeps the backstop usable. Once
  # `child=` is recorded the flag stops mattering, so an ordinary dead run is
  # still reclaimed rather than wedging the machine for WPMGR_LOCK_TIMEOUT.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  d1=$!
  wait $d1 2>/dev/null
  sh -c 'exit 0' &
  d2=$!
  wait $d2 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\ntoken=wpmgrlock-nothing-runs-under-this-0002\nspawning=1\nhost=test\ncwd=/nowhere\ncmd=dead-holder\nchild=%s\nclstart=Mon Jan  1 00:00:00 2001\n' \
    "$d1" "$(now)" "$d2" > "$ROOT/itest.lock/meta"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=10 WPMGR_LOCK_POLL=1 \
    "$LOCK" itest -- echo RECLAIMED-AFTER-SPAWNING > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "spawning=1 stops mattering once the child pid is recorded"
  expect_contains "$ROOT/o" "RECLAIMED-AFTER-SPAWNING" "the command ran after the reclaim"
fi

begin "token-gone-is-still-reclaimable"
if ! skip_case; then
  # The over-fire complement. The token must not make locks immortal: when the
  # token names nothing running and the wrapper is dead, the lock is stale and
  # must still be reclaimed, or a killed run wedges the machine for
  # WPMGR_LOCK_TIMEOUT and the lock gets switched off.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  deadpid=$!
  wait $deadpid 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\ntoken=wpmgrlock-nothing-runs-under-this-0000\nhost=test\ncwd=/nowhere\ncmd=dead-holder\n' \
    "$deadpid" "$(now)" > "$ROOT/itest.lock/meta"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=10 WPMGR_LOCK_POLL=1 \
    "$LOCK" itest -- echo RECLAIMED-TOKEN-DEAD > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "a lock whose token names nothing running is still reclaimable"
  expect_contains "$ROOT/o" "nothing is running under holder token" "the reclaim says the token was checked"
  expect_contains "$ROOT/o" "RECLAIMED-TOKEN-DEAD" "the command ran after the reclaim"
fi

begin "reclaim-marker-unwritable"
if ! skip_case; then
  # DEFECT 5, which is defect 2 in the path added by fix 3. An unwritten marker
  # is worse than unwritten lock metadata: the abandoned-marker recovery keys
  # on the pid inside it, so a marker with no pid is one that nothing can ever
  # clear, and every future reclaimer on the machine wedges permanently.
  #
  # Same forcing trick as meta-write-unwritable: `umask 0777` makes the marker
  # directory mode 000, so mkdir succeeds and the write inside it cannot.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  deadpid=$!
  wait $deadpid 2>/dev/null
  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\nhost=test\ncwd=/nowhere\ncmd=dead-holder\n' \
    "$deadpid" "$(now)" > "$ROOT/itest.lock/meta"

  : > "$ROOT/o"
  ( umask 0777
    WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=4 WPMGR_LOCK_POLL=1 \
      "$LOCK" itest -- echo RECLAIMED-WITHOUT-MARKER ) > "$ROOT/o" 2>&1
  st=$?
  expect_status 69 "$st" "a reclaim whose marker cannot be written refuses rather than proceeding"
  expect_absent "$ROOT/o" "RECLAIMED-WITHOUT-MARKER" "the command did not run behind an unwritable marker"
  expect_contains "$ROOT/o" "cannot write the reclaim marker" "the marker write failure is reported, never suppressed"

  # And it must not leave a marker nothing can ever clear.
  if [ -d "$ROOT/itest.reclaim" ]; then
    fail "left a reclaim marker at $ROOT/itest.reclaim that no other reclaimer could clear"
    chmod u+rwx "$ROOT/itest.reclaim" 2>/dev/null
    rm -rf "$ROOT/itest.reclaim" 2>/dev/null
  else
    ok "the failed reclaim removed its own marker instead of wedging every future reclaimer"
  fi
  rm -rf "$ROOT/itest.lock" 2>/dev/null
fi

begin "child-liveness-without-token"
if ! skip_case; then
  # PINS A BRANCH NOTHING ELSE REACHES. A mutation run deleting the whole
  # `child`/`clstart` liveness check from assess_holder left this suite green at
  # 74/74: every other case that reaches "wrapper dead, command alive" is
  # satisfied by the token check first, so the pid branch was load-bearing and
  # unasserted — removable in silence, which is the worst kind.
  #
  # The branch's real job is a lock whose metadata carries NO token: one written
  # by an older version of the script, or read mid-upgrade. So: no token, a dead
  # wrapper, and a genuinely live command pid.
  mkdir -p "$ROOT/itest.lock"
  sh -c 'exit 0' &
  deadwrapper=$!
  wait $deadwrapper 2>/dev/null

  sleep 30 &
  livechild=$!
  livestart="$(ps -o lstart= -p "$livechild" 2>/dev/null | tr -s ' ' ' ' | sed 's/^ *//; s/ *$//')"

  printf 'pid=%s\nacquired=%s\nlstart=Mon Jan  1 00:00:00 2001\nhost=test\ncwd=/nowhere\ncmd=old-version-holder\nchild=%s\nclstart=%s\n' \
    "$deadwrapper" "$(now)" "$livechild" "$livestart" > "$ROOT/itest.lock/meta"

  if grep -q '^token=' "$ROOT/itest.lock/meta" 2>/dev/null; then
    fail "the planted metadata has a token — this case would not reach the pid branch"
  else
    ok "the planted lock carries no token, so only the child pid can hold it"
  fi

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=2 WPMGR_LOCK_POLL=1 WPMGR_LOCK_GRACE=1 \
    "$LOCK" itest -- echo ADMITTED-OVER-LIVE-CHILD > "$ROOT/o" 2>&1
  st=$?
  expect_status 75 "$st" "a tokenless lock with a live child pid still reads as held"
  expect_absent "$ROOT/o" "ADMITTED-OVER-LIVE-CHILD" "no second suite started over a live recorded child"
  expect_absent "$ROOT/o" "RECLAIM" "the live child pid alone kept the lock from being reclaimed"

  kill -9 "$livechild" 2>/dev/null
  wait "$livechild" 2>/dev/null
  rm -rf "$ROOT/itest.lock" 2>/dev/null
fi

# A shared helper for the signal cases. Identifies the run's processes by
# walking DOWN from the pid the lock recorded, never by `pgrep -f` on the
# payload's name: the wrapper's own argv contains the payload string too, so a
# name match returns the wrapper, the token shell and the leaf, and `head -1`
# picks the wrapper. An assertion that prints the wrapper's pid while claiming
# to have found a grandchild is the thing this whole PR is about.

# wait_for_meta_field FILE KEY -> echoes the value, bounded
wait_for_meta_field() {
  _f="$1"; _k="$2"; _v=""; _i=0
  while [ "$_i" -lt 10 ]; do
    _v="$(sed -n "s/^$_k=//p" "$_f" 2>/dev/null | head -1)"
    [ -n "$_v" ] && break
    sleep 1
    _i=$(( _i + 1 ))
  done
  echo "$_v"
}

begin "signal-stops-a-two-deep-tree"
if ! skip_case; then
  # A SIGTERM to the wrapper must stop the whole run before the lock is
  # released. `kill $CHILD` reaped only the token-bearing `sh`, let `wait`
  # return, and released the lock while the real command kept running.
  #
  # DEPTH MATTERS AND IS ASSERTED. `sh -c 'sleep N'` is a single simple command
  # and the shell exec-optimizes it away, so that payload is only one level
  # deep and never exercises the recursion. `; :` keeps the payload shell
  # alive, giving token-sh -> sh -> sleep, which is the shape the real target
  # has (`sh -c 'cd apps/api && go test ...'`).
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- sh -c 'sleep 3771; :' > "$ROOT/holder" 2>&1 &
  wp=$!

  child="$(wait_for_meta_field "$ROOT/itest.lock/meta" child)"
  if [ -z "$child" ]; then
    fail "the holder never took the lock — this case would prove nothing"
  else
    mid="$(pgrep -P "$child" 2>/dev/null | head -1)"
    leaf=""
    [ -n "$mid" ] && leaf="$(pgrep -P "$mid" 2>/dev/null | head -1)"
    if [ -n "$mid" ] && [ -n "$leaf" ]; then
      ok "the run is two deep below the recorded pid (child $child -> $mid -> $leaf)"
    else
      fail "the run is not two deep (child=$child mid=${mid:-none} leaf=${leaf:-none}); the recursion would go untested"
    fi

    kill -TERM "$wp" 2>/dev/null
    wait "$wp" 2>/dev/null

    i=0
    while [ "$i" -lt 8 ]; do
      kill -0 "$leaf" 2>/dev/null || break
      sleep 1
      i=$(( i + 1 ))
    done

    # Judge the two facts together. Reporting on the lock alone would let a
    # message claim "released after the run stopped" while the run was still
    # running, which is exactly what the previous version of this case did.
    leaf_alive=0
    kill -0 "$leaf" 2>/dev/null && leaf_alive=1
    lock_present=0
    [ -d "$ROOT/itest.lock" ] && lock_present=1

    if [ "$leaf_alive" = "1" ] && [ "$lock_present" = "0" ]; then
      fail "the lock was released while the leaf process ($leaf) was still running"
    elif [ "$leaf_alive" = "1" ]; then
      fail "the leaf process ($leaf) survived the signal (lock correctly still held)"
    elif [ "$lock_present" = "1" ]; then
      fail "the run stopped but the lock was left behind"
    else
      ok "the whole tree stopped, and only then was the lock released"
    fi

    kill -9 "$leaf" "$mid" 2>/dev/null
  fi
  rm -rf "$ROOT/itest.lock" 2>/dev/null
fi

begin "signal-tree-ignoring-term"
if ! skip_case; then
  # THE CASE THE PREVIOUS FIX CLAIMED TO HANDLE AND DID NOT. `still_running`
  # watched CHILD and the token, and both name the SAME process — the token
  # lives only in the token shell's argv and an exec'd descendant does not
  # carry it. So when CHILD died and a descendant outlived it, still_running
  # said false, the escalation loop broke on its first iteration, and the lock
  # was released over a live run.
  #
  # SIG_IGN survives exec, so this descendant genuinely ignores SIGTERM. The
  # handler must notice it is still there and escalate.
  WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- sh -c 'trap "" TERM; exec sleep 3779' > "$ROOT/holder" 2>&1 &
  wp=$!

  child="$(wait_for_meta_field "$ROOT/itest.lock/meta" child)"
  if [ -z "$child" ]; then
    fail "the holder never took the lock — this case would prove nothing"
  else
    leaf="$(pgrep -P "$child" 2>/dev/null | head -1)"
    if [ -n "$leaf" ]; then
      ok "the run has a descendant ($leaf) that ignores SIGTERM"
    else
      fail "no descendant was started; this case would not exercise the escalation"
    fi

    kill -TERM "$wp" 2>/dev/null
    wait "$wp" 2>/dev/null

    i=0
    while [ "$i" -lt 10 ]; do
      kill -0 "$leaf" 2>/dev/null || break
      sleep 1
      i=$(( i + 1 ))
    done

    leaf_alive=0
    kill -0 "$leaf" 2>/dev/null && leaf_alive=1
    lock_present=0
    [ -d "$ROOT/itest.lock" ] && lock_present=1

    # The property, stated as the review asked: a surviving descendant means
    # the lock is NOT released. Both honest outcomes pass — reap it and
    # release, or fail to reap it and refuse — and only the fail-open fails.
    if [ "$leaf_alive" = "1" ] && [ "$lock_present" = "0" ]; then
      fail "the lock was released while a TERM-ignoring descendant ($leaf) was still running"
    elif [ "$leaf_alive" = "1" ]; then
      ok "the descendant survived and the lock was deliberately NOT released"
    elif [ "$lock_present" = "1" ]; then
      fail "the descendant was reaped but the lock was left behind"
    else
      ok "the descendant was escalated to KILL and reaped, and only then was the lock released"
    fi

    # The escalation is the mechanism under test; assert it actually ran rather
    # than inferring it from the outcome.
    expect_contains "$ROOT/holder" "escalating to KILL" "the handler escalated rather than giving up after TERM"

    kill -9 "$leaf" 2>/dev/null
  fi
  rm -rf "$ROOT/itest.lock" 2>/dev/null
fi

begin "reclaim-salvage-removed"
if ! skip_case; then
  # The reclaim renames the stale lock aside and then deletes the copy. That
  # delete was the last suppressed, unchecked write in the script, three lines
  # under a comment about the pattern. It only leaks directories rather than
  # failing open, but leaking is what it does, and this pins it.
  #
  # A mode-000 stale lock is the case `rm -rf` cannot handle: it cannot descend
  # the directory to empty it. Built at 700 so the metadata can be written,
  # then locked down.
  mkdir -p "$ROOT/itest.lock"
  printf 'unreadable\n' > "$ROOT/itest.lock/meta"
  chmod 000 "$ROOT/itest.lock"

  WPMGR_LOCK_ROOT="$ROOT" WPMGR_LOCK_TIMEOUT=10 WPMGR_LOCK_POLL=1 WPMGR_LOCK_GRACE=1 \
    "$LOCK" itest -- echo RECLAIMED-UNREADABLE > "$ROOT/o" 2>&1
  st=$?
  expect_status 0 "$st" "an unreadable stale lock is reclaimed rather than wedging the machine"
  expect_contains "$ROOT/o" "RECLAIMED-UNREADABLE" "the command ran after the reclaim"

  # The salvaged copy must not survive. `ls` rather than a glob so an unmatched
  # pattern cannot be mistaken for a filename.
  leaked="$(ls -d "$ROOT"/itest.lock.stale.* 2>/dev/null | head -1)"
  if [ -n "$leaked" ]; then
    fail "the reclaim leaked its salvaged copy at $leaked"
    chmod -R u+rwx "$ROOT"/itest.lock.stale.* 2>/dev/null
    rm -rf "$ROOT"/itest.lock.stale.* 2>/dev/null
  else
    ok "the reclaim removed the salvaged copy it renamed aside"
  fi
  chmod -R u+rwx "$ROOT/itest.lock" 2>/dev/null
  rm -rf "$ROOT/itest.lock" 2>/dev/null
fi

begin "metadata-newline-injection"
if ! skip_case; then
  # The metadata is a line-oriented KEY=VALUE file, so any free-text value
  # carrying a newline can forge a line of its own. `cmd` was flattened while
  # `cwd` on the same printf was not, so running from a directory whose name
  # contained a newline forged a `child=` line: the command ran AND THEN the
  # run exited 69. Over-fire plus partial execution on pathological input.
  evil="$ROOT/$(printf 'a\nchild=1\nclstart=x\nb')"
  mkdir -p "$evil" 2>/dev/null
  if [ ! -d "$evil" ]; then
    # Not `ok`: if the fixture cannot be built, the regression case did not
    # run, and reporting success over a case that never executed is the exact
    # defect this suite exists to catch.
    fail "could not create the newline-named fixture, so the injection case did not run"
  else
    ( cd "$evil" && WPMGR_LOCK_ROOT="$ROOT" "$LOCK" itest -- echo THE-COMMAND-RAN ) > "$ROOT/o" 2>&1
    st=$?
    expect_status 0 "$st" "an ordinary run from a directory with a newline in its name still exits 0"
    expect_contains "$ROOT/o" "THE-COMMAND-RAN" "the command ran"
    expect_absent "$ROOT/o" "cannot record the command's pid" "the cwd did not forge a child= line"
    if [ -d "$ROOT/itest.lock" ]; then
      fail "the lock was left behind after a run from a newline-named directory"
      rm -rf "$ROOT/itest.lock" 2>/dev/null
    else
      ok "the lock was released normally"
    fi
  fi
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
