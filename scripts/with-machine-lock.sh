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
# EVERY FAILURE MODE HERE FAILS CLOSED. A mutex has two ways to be wrong, and
# they are not symmetrical. Refusing to run when it should have run costs one
# loud, obvious minute. Admitting a second run alongside a live one costs the
# thing this script exists to prevent, silently, and it teaches people to stop
# trusting the "flake?" question again. So wherever this script cannot prove
# the lock is free, it treats the lock as HELD; wherever it cannot prove its
# own bookkeeping succeeded, it refuses to run the command at all.
#
# THREE FAIL-OPEN DEFECTS WERE FOUND IN REVIEW OF THE FIRST DRAFT (PR #575),
# and the three properties below are the fixes. Each has a red/green case in
# scripts/with-machine-lock_test.sh; none of them is asserted only in prose.
#
#   1. A LIVE TEST PROCESS MEANS THE LOCK IS NOT STALE, REGARDLESS OF WHAT
#      HAPPENED TO THE WRAPPER. The first draft recorded only this script's own
#      pid. But SIGKILL cannot be trapped, and agents on this machine get
#      killed mid-run — that is the origin of the project's "commit before the
#      slow suite" rule. The wrapper died, `go test` and its containers kept
#      running, the next invocation saw a dead pid, correctly concluded the
#      WRAPPER was gone, and admitted a second suite alongside a live one. So
#      the command now runs as a recorded background child and the lock names
#      BOTH pids. Either one alive, and the lock is live. See is_pid_live().
#
#   2. A LOCK WHOSE METADATA COULD NOT BE WRITTEN IS NEVER RUN UNDER. The first
#      draft's write was both suppressed (`2>/dev/null`) and unchecked, so a
#      failed write still reached the command. Another invocation then saw a
#      metadata-less lock, waited out the grace period, and admitted itself
#      alongside a live suite. The write is now checked AND read back, and on
#      failure this script releases the lock and exits 69 without running
#      anything. See write_meta() for why that is the right half of the choice.
#
#   3. A RECLAIM PROVES, UNDER A MUTEX, THAT IT IS REMOVING THE DIRECTORY IT
#      INSPECTED. In the first draft two waiters could both classify one lock
#      as stale; the first moved it, removed it and acquired a fresh lock, and
#      the second — still acting on its now-obsolete verdict — moved and
#      removed THAT live lock. Two commands ran. Reclaim now happens under a
#      second mkdir mutex and re-derives staleness from scratch inside it. See
#      the RECLAIM section.
#
# A SECOND REVIEW ROUND THEN FOUND BOTH OF THOSE AGAIN, ONE STEP IN, which is
# the interesting part and the reason for the design note below:
#
#   4. Recording the child AFTER spawning it left a spawn-to-record window. Fix
#      1 shrank the exposure from "the whole run" to "the microseconds between
#      `&` and the append", but shrinking a window is not closing it. See
#      IDENTIFYING THE RUN BEFORE IT EXISTS.
#   5. The reclaim marker's write was suppressed and unchecked — defect 2
#      verbatim, in the path added by fix 3. It now fails closed the same way.
#
# Two rounds, same two defects, narrower each time. That is the signature of an
# approach that patches windows instead of removing them, so the ordering
# problem is addressed directly rather than a third time.
#
# IDENTIFYING THE RUN BEFORE IT EXISTS.
#
# The bug in "spawn, then record the pid" is ordering: the identifier is
# learned only after the thing it identifies already exists, so a kill in
# between leaves a running command that nothing in the lock names. No amount of
# moving the write earlier fixes that, because the pid does not exist earlier.
# The fix has to be an identifier chosen BEFORE the fork.
#
# PROCESS GROUP: EVALUATED AND REJECTED, with the measurement, because it is
# the obvious candidate and the next reader will wonder. The idea is sound —
# a pgid is known before any fork — but this wrapper cannot own one:
#
#   command -v setsid   ABSENT on this machine (darwin). Same story as flock
#                       and timeout; it is util-linux. So the wrapper cannot
#                       put itself into a process group of its own, and bash
#                       has no setpgid builtin.
#   pgrep               PRESENT at /usr/bin/pgrep, on darwin and ubuntu both.
#
# Without setsid the wrapper INHERITS its caller's group, and "is any process
# in that group alive" then answers for the caller, not for the run. Measured
# here rather than assumed — a probe script reporting its own pgid membership:
#
#     wrapper pid : 5343
#     wrapper pgid: 5341
#     am I the group leader (pgid == pid)?  NO — the group is somebody else's
#     every process currently in that group:
#          5341  5341 /bin/zsh
#          5343  5341 bash
#
# The group belongs to the calling shell, which outlives every run in the
# session. A lock keyed on it would read as held forever: every later run waits
# out WPMGR_LOCK_TIMEOUT and exits 75. That is an over-fire, and an over-firing
# guard gets switched off, which is the one failure this script cannot survive.
#
# TOKEN: WHAT IS USED INSTEAD. A random token is generated, written into the
# metadata and VERIFIED there, all before anything is forked. The command is
# then launched with that token in its argv, so `pgrep -f TOKEN` answers "is
# this run still alive" against an identifier that already existed when the
# child did not. The orderings are now exhaustive, and none of them admits a
# second run:
#
#   killed before the fork   token in meta, pgrep finds nothing, no child
#                            recorded -> correctly stale, nothing was running.
#   killed after the fork    pgrep finds the command by its token -> LIVE. This
#                            is the window defect 4 reported, and it is closed
#                            by evidence rather than by being made smaller.
#   killed after the append  child pid recorded too -> LIVE, as since fix 1.
#
# THE FORK-TO-EXEC GAP, and how it is closed. Between the fork and the exec the
# child carries the parent's argv, so the token is not visible in it, and
# `child=` has not been written yet either — both identifiers are blind. A
# child that is stopped or delayed through that interval would be invisible to
# two consecutive assessments, and a waiter would reclaim a lock whose command
# then resumes beside the new holder.
#
# So the metadata carries `spawning=1`, written BEFORE the fork. A lock with
# `spawning=1` and no `child=` is not declared stale on sight: nothing can
# prove the command did not start, and for a mutex "cannot prove" resolves to
# held.
#
# THE HOLD IS BOUNDED, AND THE NUMBERS ARE WHY. An earlier version held such a
# lock forever, justified on two claims that were both measured false:
#
#   "a microsecond-wide window"   The `spawning=1` interval is a median 7.71 ms
#                                 (n=40, instrumentation cost subtracted). It
#                                 spans mv, two sed readbacks, the fork, ps+tr+
#                                 sed in lstart_of, and the append — six exec'd
#                                 processes. The blind interval it actually
#                                 protects, fork to exec, is a median 2.42 ms
#                                 and a max of 3.46 ms (n=30). So the hold
#                                 covered about 3x the interval it protects,
#                                 and the excess is the PRE-FORK part, where no
#                                 child exists at all and nothing can be
#                                 running. The permanent deadlock therefore
#                                 fired on a strict superset of the fail-open's
#                                 conditions.
#   "loud"                        It was not. WPMGR_LOCK_TIMEOUT defaults to
#                                 3600, and the explanatory NOTE only printed
#                                 in the timed-out branch. For that hour a
#                                 waiter printed the ordinary "held by pid N"
#                                 line, indistinguishable from a live suite —
#                                 and the population here is agents, which get
#                                 killed when they block. Silent for exactly
#                                 the people who hit it, an hour each. The NOTE
#                                 is now in the periodic notice too.
#
# `spawning=1` with no `child=` cannot legitimately persist past a forked sh's
# exec, so it is bounded by the same GRACE the metadata-less case uses: held on
# first sight, and after GRACE seconds of dead pid AND dead token AND no
# `child=`, declared stale with a reason that names it. For the fail-open to
# return, a child would have to sit SIGSTOPped inside a ~3 ms interval for the
# whole grace period and then resume, with its wrapper dead and a waiter
# sampling stale twice. That is strictly harder than the pre-`spawning` state,
# which had no protection for any duration.
#
# The SECOND ASSESSMENT still matters and is kept: declaring a lock stale
# requires assess_holder to return stale twice, once in the polling loop and
# again from scratch under the reclaim mutex.
#
# `child`/`clstart` do NOT cover this gap, and an earlier version of this
# comment claimed they did. They cannot, for two measured reasons: CHILD is the
# pid of the very `sh` that carries the token, so the two checks track ONE
# process rather than corroborating each other; and during the gap the metadata
# has no `child=` line at all, because write_meta runs before the fork and
# record_child runs after it, so is_pid_live gets an empty pid and returns
# false. What `child`/`clstart` are actually for is the case where the token is
# absent from the metadata entirely — see assess_holder — which is why the
# suite pins that path separately.
#
#
# KNOWN AND ACCEPTED RESIDUAL: A HOLDER WHOSE WRAPPER AND TOKEN SHELL BOTH DIE
# IS RECLAIMED WHILE ITS DESCENDANTS RUN. Decided, not missed — see GH #576.
#
# Every identifier this lock records lives in one of two processes: the wrapper
# (`pid`/`lstart`) and the token-bearing shell (`child`/`clstart`, and `token`,
# which exists only in that shell's argv). Kill both and leave the grandchildren
# — `go test` and its containers — and assess_holder finds every identifier dead
# and reclaims a lock whose work is still running. Reproduced:
#
#     wrapper alive : NO      token-sh alive: NO
#     mid alive     : YES     leaf alive    : YES
#     RECLAIMING STALE LOCK 'itest' — nothing is running under holder token ...
#     SECOND-ADMITTED    exit=0
#
# RUN_PIDS does not help. It is private to the process that recorded it, is
# built only inside a signal handler, and never reaches the metadata, so a
# waiter inspecting somebody else's lock cannot see it.
#
# WHY IT IS ACCEPTED. It needs the wrapper AND the token shell dead together.
# No ordinary kill does that: signalling the wrapper alone leaves the token
# shell, and the token then holds the lock correctly, which is the case the
# suite covers. Closing it means the holder periodically re-snapshotting its
# tree into the metadata mid-run plus a new field — a design change to the
# staleness subsystem, with its own review, not a patch. The comparison that
# decided it is not "perfect lock vs this lock" but "this lock vs no lock":
# before this script, `make test-integration` had no serialisation at all.
#
# THE ENVIRONMENT ROUTE IS CLOSED HERE, AND IT WAS MEASURED, so nobody spends
# that hour again. An exported variable is the natural carrier because unlike
# argv it survives exec into the whole tree — but no ps on this platform will
# show it:
#
#     export WPMGR_PROBE_TOKEN=envtok-12345   (grandchild reached via exec)
#     ps -E  -o pid=,command= | grep envtok-12345   ->  no output
#     ps eww -o pid=,command= | grep envtok-12345   ->  no output
#     pgrep -f envtok-12345                          ->  no match (argv only)
#
# That is the fourth capability this file has needed and not found, after
# flock, timeout and setsid.
#
# EXIT CODES.
#   *   The command's own status, passed through unchanged, whatever it was.
#   64  Usage error (EX_USAGE): bad arguments, bad lock name, no command.
#   69  Unavailable (EX_UNAVAILABLE): the lock root, the lock, or the lock's
#       metadata could not be created, or a tool this script needs is missing.
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
# that defect wearing a disguise; twenty-three instances have been found so far
# and several of them were inside code written to catch this exact class. So
# the command below runs bare in the background, its status is read with a
# bare `wait` on the very next line, the lock is released, and that status is
# what this script exits with. There is no pipe, no `&&` chain and no subshell
# anywhere on that path.
#
# WHY THE COMMAND IS BACKGROUNDED RATHER THAN RUN IN THE FOREGROUND. Solely so
# its pid can be recorded (property 1). Two consequences are handled explicitly
# rather than left to chance:
#   - A backgrounded command would otherwise have its stdin redirected from
#     /dev/null, per POSIX. `<&0` is an explicit redirection, which suppresses
#     that default and keeps the caller's stdin attached.
#   - A foreground child receives terminal signals directly; a backgrounded one
#     relies on us to forward them, so on_signal() does exactly that, and waits
#     for the child to actually go before releasing the lock.
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
for tool in mkdir ps date rm mv chmod pgrep; do
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
RECLAIM_DIR="$LOCK_ROOT/$NAME.reclaim"
RECLAIM_META="$RECLAIM_DIR/by"

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

# is_pid_live PID RECORDED_LSTART -> 0 if that exact process is still running.
#
# Every ambiguous answer here resolves to LIVE, because the caller uses this to
# decide whether it may delete somebody else's lock. "I could not prove it is
# dead" and "it is dead" are different answers, and the first draft's bug was
# spending the second one on evidence that only supported the first.
is_pid_live() {
  _p="${1:-}"
  _recorded="${2:-}"
  [ -n "$_p" ] || return 1                      # nothing recorded: not alive
  ps -p "$_p" >/dev/null 2>&1 || return 1       # no such process: dead
  [ -n "$_recorded" ] || return 0               # no start time to compare: assume live
  _current="$(lstart_of "$_p")"
  [ -n "$_current" ] || return 0                # cannot read it: assume live
  [ "$_recorded" = "$_current" ]                # differs => the pid was recycled
}

# The identifier that exists before the child does. Restricted to [A-Za-z0-9-]
# so it is safe as the ERE that `pgrep -f` treats it as, with no metacharacter
# able to widen the match onto an unrelated process.
TOKEN="wpmgrlock-$$-$(date +%s)-${RANDOM}${RANDOM}"

# is_token_live TOKEN -> 0 if a process launched under this token is running.
#
# Unlike a pid this cannot be recycled into a false positive: the token is
# unique per acquire, so a match is this run and nothing else.
is_token_live() {
  _t="${1:-}"
  [ -n "$_t" ] || return 1
  pgrep -f "$_t" >/dev/null 2>&1
}

meta_field() { # meta_field KEY
  [ -f "$META" ] || return 1
  sed -n "s/^$1=//p" "$META" 2>/dev/null | head -1
}

# remove_dir_hard DIR -> 0 if DIR is gone afterwards.
#
# `rm -rf` cannot descend a directory it has no execute permission on, and it
# reports that on stderr, which the first draft discarded — so a release could
# print "released" over its own failure and leave the lock standing. Both the
# lock and the reclaim marker can end up mode 000 (a caller with a hostile
# umask creates them that way), so the retry lives in one place rather than
# being got right in one caller and forgotten in the next.
remove_dir_hard() {
  rm -rf "$1" 2>/dev/null
  if [ -d "$1" ]; then
    chmod u+rwx "$1" 2>/dev/null
    rm -rf "$1" 2>/dev/null
  fi
  [ ! -d "$1" ]
}

# _flat TEXT -> TEXT with every newline and carriage return turned into a
# space. The metadata is a line-oriented KEY=VALUE file, so any free-text value
# carrying a newline can forge a line of its own.
_flat() {
  printf '%s' "$1" | tr '\n\r' '  '
}

# Printed wherever a wait is reported, not only at the timeout an hour later.
# A waiter blocked on this state used to see the ordinary "held by pid N" line,
# indistinguishable from a live suite, for the whole of WPMGR_LOCK_TIMEOUT —
# and the population here is agents, which get killed when they block.
spawning_notice() {
  [ "${holder_spawning:-}" = "1" ] || return 0
  [ -z "${holder_child:-}" ] || return 0
  echo "$ME:   NOTE: that lock was taken by a run that died while starting its" >&2
  echo "$ME:   command, so whether the command started cannot be determined." >&2
  echo "$ME:   It is held for ${GRACE}s and then reclaimed automatically." >&2
  echo "$ME:   If you know nothing is running, remove $LOCK_DIR by hand." >&2
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
RECLAIM_HELD=0
CHILD=""

release() {
  if [ "$RECLAIM_HELD" = "1" ]; then
    if ! remove_dir_hard "$RECLAIM_DIR"; then
      echo "$ME: FAILED to remove $RECLAIM_DIR — remove it by hand or reclaims on this machine will wedge" >&2
    fi
    RECLAIM_HELD=0
  fi
  [ "$HELD" = "1" ] || return 0
  # Defensive: only remove a lock whose metadata still names this process.
  owner="$(meta_field pid 2>/dev/null || true)"
  if [ -n "$owner" ] && [ "$owner" != "$$" ]; then
    echo "$ME: NOT releasing $LOCK_DIR — it is now held by pid $owner, not by me ($$)" >&2
    HELD=0
    return 0
  fi
  # A release that quietly fails is this project's signature defect wearing a
  # mutex: the caller prints "released", the directory is still there, and the
  # next run either waits on a lock nobody holds or — if the leaked lock has no
  # metadata — reclaims it after the grace period. Caught by the metadata
  # failure case in the suite, where the lock directory is mode 000 and `rm -rf`
  # cannot descend it, so the first attempt fails with the error suppressed.
  if ! remove_dir_hard "$LOCK_DIR"; then
    echo "$ME: FAILED to release $LOCK_DIR — remove it by hand, or later runs will wait on a lock nobody holds" >&2
    HELD=0
    return 1
  fi
  HELD=0
  return 0
}

# The lock must outlive the wrapper only for as long as the command it protects
# is actually running. On a catchable signal we forward it to the child and
# WAIT for the child to go before releasing — releasing first would hand the
# lock to a second suite while this one's containers were still shutting down,
# which is property 1 failing in the other direction.
# kill_tree PID SIG — signal PID and every descendant, children first.
#
# `kill $CHILD` is not enough and never was. CHILD is the token-bearing `sh`,
# and the real work is ITS child (`go test`, which is itself under another
# `sh -c 'cd apps/api && ...'`). Signalling only CHILD reaped the shell, let
# `wait` return, and released the lock while the suite and its containers were
# still running — a second `make test-integration` could then start alongside
# it. Depth-first so a descendant is signalled before the parent that would
# otherwise re-parent it away mid-sweep.
# WHY A SNAPSHOT, AND NOT A LIVE WALK. Once CHILD dies its descendants
# re-parent to pid 1, so `pgrep -P "$CHILD"` returns nothing and there is no
# link left to walk down. Anything that asks "what is still running under this
# run?" AFTER the signal therefore finds nothing and concludes, wrongly, that
# the run is over. The tree has to be recorded while the parentage still
# exists — that is, before the first signal — and every later question asked
# against that recording.
#
# The recorded form is `pid:startkey`, with the process start time's spaces
# turned into underscores so an entry holds no whitespace and the list can be
# iterated with the default IFS. The start time is carried for the same reason
# the wrapper's is: so a recycled pid cannot make a dead process look alive.
RUN_PIDS=""

lstart_key() {
  lstart_of "$1" | tr ' ' '_'
}

# snapshot_tree PID — record PID and every descendant, children first, so a
# later signal pass reaches a child before the parent that would otherwise
# re-parent it away mid-sweep.
snapshot_tree() {
  _kids="$(pgrep -P "$1" 2>/dev/null)"
  for _k in $_kids; do
    snapshot_tree "$_k"
  done
  case " $RUN_PIDS " in
    *" $1:"*) ;;
    *) RUN_PIDS="$RUN_PIDS $1:$(lstart_key "$1")" ;;
  esac
}

# Re-walk from everything still alive, to pick up processes spawned after the
# snapshot was taken. Cheap, and it only ever adds.
resnapshot() {
  for _e in $RUN_PIDS; do
    _p="${_e%%:*}"
    if kill -0 "$_p" 2>/dev/null; then
      snapshot_tree "$_p"
    fi
  done
}

# KNOWN AND ACCEPTED: an empty start key skips the pid-reuse guard, so a
# recorded pid whose start time could not be read reads as LIVE on any live pid
# that later takes its number. That is the over-fire direction (the lock is
# held, never wrongly released), and reaching it needs ~100k pid allocations
# within seconds. Recorded here so it is not rediscovered as new.
snapshot_pid_live() { # snapshot_pid_live PID STARTKEY
  _p="${1:-}"
  _key="${2:-}"
  [ -n "$_p" ] || return 1
  ps -p "$_p" >/dev/null 2>&1 || return 1
  [ -n "$_key" ] || return 0
  _cur="$(lstart_key "$_p")"
  [ -n "$_cur" ] || return 0
  [ "$_key" = "$_cur" ]
}

# signal_snapshot SIG — signal every recorded pid, in recorded order.
signal_snapshot() {
  for _e in $RUN_PIDS; do
    _p="${_e%%:*}"
    kill -"$1" "$_p" 2>/dev/null
  done
}

# kill_tree PID SIG — record the tree, then signal it.
#
# `kill $CHILD` is not enough and never was. CHILD is the token-bearing `sh`,
# and the real work is ITS child (`go test`, itself under another
# `sh -c 'cd apps/api && ...'`). Signalling only CHILD reaped the shell, let
# `wait` return, and released the lock while the suite and its containers were
# still running — a second `make test-integration` could then start alongside
# it.
# KNOWN AND ACCEPTED: a descendant spawned between snapshot_tree's `pgrep` and
# the TERM, whose parent then dies before the first resnapshot, is missed —
# roughly a 10 ms window, narrower than the 7.71 ms and 2.42 ms intervals this
# file already bounds elsewhere. Recorded so it is not rediscovered as new.
kill_tree() {
  snapshot_tree "$1"
  signal_snapshot "$2"
}

# still_running -> 0 if any part of the protected run survives.
#
# THE RECORDED TREE IS CHECKED FIRST AND IS THE LOAD-BEARING ARM. The two
# checks below it both watch the SAME process: CHILD is the token-bearing `sh`,
# and the token exists only in that process's argv — an exec'd descendant does
# not carry it. An earlier version had only those two, so when CHILD died and a
# descendant outlived it this returned false, the escalation never ran, and the
# lock was released over a live run. Proved by a descendant that ignores TERM
# (SIG_IGN survives exec), which is now the `signal-tree-ignoring-term` case.
still_running() {
  for _e in $RUN_PIDS; do
    _p="${_e%%:*}"
    _k="${_e#*:}"
    if snapshot_pid_live "$_p" "$_k"; then
      return 0
    fi
  done
  if [ -n "$CHILD" ] && kill -0 "$CHILD" 2>/dev/null; then
    return 0
  fi
  is_token_live "$TOKEN"
}

# stop_run — stop the protected run and everything under it. Returns 0 when
# nothing is left running, 1 when something survived.
#
# SHARED ON PURPOSE. There are two paths that must stop the run before letting
# go of the lock: a caught signal, and a failure to record the child. The
# second one used to TERM the token shell alone and then release
# unconditionally, so an inner shell, `go test` or its containers could still
# be live when the next invocation took the lock — the same defect the signal
# path had, in the other caller. One implementation means fixing it once.
stop_run() {
  if [ -z "$CHILD" ]; then
    # NO RECORDED CHILD. Two very different situations look identical from
    # here, and the old `return 0` answered both with "nothing is running":
    #
    #   a) we never forked      — signalled while still waiting for the lock,
    #                             or before the command was launched at all.
    #   b) we forked, and the signal landed before `CHILD=$!` assigned.
    #
    # In (b) the command is running and NOTHING NAMES IT: not $CHILD, which is
    # still empty, and not `child=` in the metadata, which record_child has not
    # written yet. Reporting success there released the lock over a live run.
    #
    # `spawning=1` does not help by itself — that flag stops OTHER waiters
    # reclaiming, and says nothing about this process releasing its own lock.
    # But combined with HELD it separates (a) from (b) well enough: we only
    # reach the fork after taking the lock and writing that flag, so holding
    # the lock with the flag set and no `child=` recorded IS the window. That
    # is "a run may exist that I cannot name", which is a refusal, not success.
    #
    # The interval is the same measured median 7.71 ms as the spawning hold, so
    # a wrongly-refused signal is rare; and every signal delivered BEFORE the
    # lock is held — the ordinary early Ctrl-C, which is the common case by far
    # — still returns success immediately, because HELD is 0.
    [ "$HELD" = "1" ] || return 0
    [ "$(meta_field spawning 2>/dev/null || true)" = "1" ] || return 0
    [ -z "$(meta_field child 2>/dev/null || true)" ] || return 0
    echo "$ME: signalled while starting the command; it may already be running" >&2
    return 1
  fi

  kill_tree "$CHILD" TERM

  # NO BLOCKING `wait` HERE. `wait "$CHILD"` blocks until the child exits, so
  # a child that ignores TERM would never let this reach the bounded loop or
  # the KILL escalation below — an unbounded wait inside the very path whose
  # job is to bound one. The child is reaped after the loop has established it
  # is gone.
  #
  # Bounded, never an unbounded wait: TERM, then escalate. A descendant that
  # ignores TERM must not leave us releasing the lock over a live suite.
  _n=0
  while [ "$_n" -lt 5 ]; do
    resnapshot
    still_running || break
    sleep 1
    _n=$(( _n + 1 ))
  done
  if still_running; then
    echo "$ME: the command did not stop on TERM — escalating to KILL" >&2
    # Signal the RECORDED tree, not a fresh walk from CHILD: by now CHILD is
    # usually gone and its descendants have re-parented to pid 1, so a walk
    # would find nothing to kill.
    resnapshot
    signal_snapshot KILL
    sleep 1
  fi

  # Reap now that the bounded path has run, so the child does not linger as a
  # zombie. Non-blocking in practice: by here it has been TERMed and KILLed.
  wait "$CHILD" 2>/dev/null

  still_running && return 1
  return 0
}

# refuse_release — say why the lock is staying, and make sure the EXIT trap
# does not quietly undo that decision on the way out.
refuse_release() {
  echo "$ME: NOT releasing '$NAME' — something is still running under it" >&2
    # Deliberately does NOT promise automatic recovery. RUN_PIDS is private to
    # this process; assess_holder reads only pid, child and token, so the next
    # waiter judges by those and may well reclaim immediately. An earlier
    # version of this line claimed the lock "will be reclaimed once that exits",
    # which was simply false.
  echo "$ME:   lock left at $LOCK_DIR. Nothing tracks those survivors for you:" >&2
  echo "$ME:   check them, stop them, then remove that directory by hand." >&2
  trap - EXIT
}

on_signal() { # on_signal SIGNAME SIGNUM
  echo "" >&2
  echo "$ME: caught SIG$1 — stopping the command and releasing the $NAME lock" >&2

  # The lock is released only once nothing is left running under it. Releasing
  # over a live command is the exact fail-open this whole change removes, and
  # refusing is loud and recoverable; releasing is silent and is not.
  if ! stop_run; then
    refuse_release
    exit $(( 128 + $2 ))
  fi

  release
  trap - EXIT
  exit $(( 128 + $2 ))
}

trap 'release' EXIT
trap 'on_signal INT 2'  INT
trap 'on_signal TERM 15' TERM
trap 'on_signal HUP 1'  HUP

# ---- Metadata --------------------------------------------------------------
#
# write_meta populates the file that every other invocation reads to decide
# whether this lock is live. If it cannot be written, this process is holding a
# lock that nobody can account for.
#
# THE CHOICE, AND WHY THIS HALF. There are exactly two honest outcomes when the
# metadata cannot be written, and "proceed and hope" is not one of them:
#
#   (a) fail, and release the lock  <-- what this does
#   (b) hold, and treat present-but-metadata-less as HELD rather than
#       reclaimable
#
# (b) was rejected because it converts every crash between mkdir and write into
# a PERMANENT lock: with no metadata there is no pid to test, so nothing could
# ever declare it dead, and every later run would wait out WPMGR_LOCK_TIMEOUT
# and exit 75 until somebody removed the directory by hand. On a machine where
# agents are killed mid-run routinely, that trades a rare fail-open for a
# frequent deadlock.
#
# (a) keeps the grace-period reclaim meaningful and safe. Once the write is
# checked, the ONLY way to produce a metadata-less lock is a kill inside the
# microseconds between mkdir and write — a window in which no command has been
# started yet, so reclaiming it cannot possibly race a live suite. The grace
# path stops being a guess and becomes a proof.
#
# The write is checked twice on purpose. `printf` reports a write error, but a
# readback is what proves the file another process will read actually parses:
# it catches a truncated write, a full filesystem, and a directory that took
# the create but not the content.
write_meta() {
  _tmp="$LOCK_DIR/meta.tmp.$$"
  # A newline inside the command would forge extra metadata lines — including a
  # `child=` line naming a live pid, which would make this lock permanent — so
  # the recorded command is flattened to one line.
  # EVERY free-text field gets flattened, not just this one. `cmd` was
  # flattened here while `cwd` on the same printf was not, so a run started
  # from a directory named `a<newline>child=1<newline>clstart=x` forged its own
  # `child=` line: an ordinary run printed its output AND THEN exited 69,
  # over-firing and partially executing on pathological input. Anything
  # interpolated into this file must go through _flat.
  _cmd="$(_flat "$*")"
  _cwd="$(_flat "$PWD")"
  _host="$(_flat "$(hostname 2>/dev/null || echo unknown)")"
  # `token` goes in HERE, before anything is forked, and is verified below.
  # That ordering is the whole point: after this function returns successfully,
  # the lock can identify the run even though the run does not exist yet.
  # `spawning=1` is the fork-to-exec backstop. Between this write and
  # record_child there is an interval in which a child may exist while neither
  # identifier can see it: `child=` is not written yet, and the token is not in
  # the child's argv until it execs. A child that is stopped or delayed through
  # that interval is invisible to both assessments, and a waiter would reclaim
  # a lock whose command then resumes beside the new holder.
  #
  # So the metadata says, before the fork, that a fork is about to happen. A
  # lock carrying `spawning=1` with no `child=` is NOT declared stale: we
  # cannot prove nothing is running, and for a mutex "cannot prove" resolves to
  # held. The flag stops mattering the moment `child=` appears, which is the
  # very next write. That interval is a measured median 7.71 ms, and the hold
  # it triggers is bounded by GRACE rather than permanent — see the header.
  if ! printf 'pid=%s\nacquired=%s\nlstart=%s\ntoken=%s\nspawning=1\nhost=%s\ncwd=%s\ncmd=%s\n' \
      "$$" "$(now)" "$(lstart_of $$)" "$TOKEN" "$_host" "$_cwd" "$_cmd" \
      > "$_tmp"; then
    echo "$ME: cannot write lock metadata to $_tmp" >&2
    return 1
  fi
  # mv within one directory is atomic, so no waiter ever reads a half-written
  # meta and mistakes a live lock for a crashed one.
  if ! mv "$_tmp" "$META"; then
    echo "$ME: cannot install lock metadata at $META" >&2
    rm -f "$_tmp" 2>/dev/null
    return 1
  fi
  if [ "$(meta_field pid 2>/dev/null || true)" != "$$" ]; then
    echo "$ME: lock metadata at $META did not read back as mine (pid $$)" >&2
    return 1
  fi
  # The token is what closes the spawn-to-record window, so an unverified token
  # is not good enough: if it did not land, this run would be unidentifiable
  # for exactly the interval the fix exists to cover.
  if [ "$(meta_field token 2>/dev/null || true)" != "$TOKEN" ]; then
    echo "$ME: lock metadata at $META did not read back with my token" >&2
    return 1
  fi
  return 0
}

# record_child appends the protected command's pid to the metadata. This is
# what makes property 1 true: after this returns, the lock names a process that
# SIGKILL-ing the wrapper does not stop.
#
# Both lines go in one printf so a reader cannot see `child=` without its
# `clstart=`; and the same fail-closed rule applies as for write_meta, because
# an unrecorded child is exactly the fail-open this fixes.
record_child() {
  if ! printf 'child=%s\nclstart=%s\n' "$CHILD" "$(lstart_of "$CHILD")" >> "$META"; then
    return 1
  fi
  [ "$(sed -n 's/^child=//p' "$META" 2>/dev/null | head -1)" = "$CHILD" ]
}

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
SPAWNING_SINCE=0
BLOCKED_ON_RECLAIM=0
ANNOUNCED_RECLAIM_WAIT=0

# assess_holder reads the lock's metadata and decides whether it is stale,
# setting holder_* and STALE_REASON. It is a function because the reclaim path
# re-runs it from scratch under the reclaim mutex; the first draft decided once
# and acted on that verdict later, which is what let a waiter delete a lock
# that had been acquired in the meantime.
# NOTE: this sees only what the metadata records — the wrapper, the token shell
# and the token. Descendants of a dead token shell are invisible to it; that
# residual is documented in the header and tracked as GH #576.
assess_holder() {
  holder_pid="$(meta_field pid || true)"
  holder_lstart="$(meta_field lstart || true)"
  holder_token="$(meta_field token || true)"
  holder_spawning="$(meta_field spawning || true)"
  holder_child="$(meta_field child || true)"
  holder_clstart="$(meta_field clstart || true)"
  holder_since="$(meta_field acquired || true)"
  holder_cwd="$(meta_field cwd || true)"
  holder_cmd="$(meta_field cmd || true)"

  STALE_REASON=""

  # The spawning hold applies only while the flag is set and no child pid has
  # been recorded. Anything else clears the clock, so the grace period always
  # measures an uninterrupted run of that exact state.
  if [ "$holder_spawning" != "1" ] || [ -n "$holder_child" ]; then
    SPAWNING_SINCE=0
  fi

  if [ -z "$holder_pid" ]; then
    # No readable metadata. Since write_meta is checked, the only holder that
    # can be in this state died between its mkdir and its write, before it
    # started any command. Still not assumed either way: it is distinguished by
    # how long we have SEEN it in that state. NOMETA_SINCE is reset every time
    # we do read metadata, so a holder that is merely slow to write does not
    # accumulate credit towards being declared dead.
    if [ "$NOMETA_SINCE" = "0" ]; then
      NOMETA_SINCE="$(now)"
    fi
    if [ $(( $(now) - NOMETA_SINCE )) -ge "$GRACE" ]; then
      STALE_REASON="its metadata never appeared (crashed between mkdir and write; unreadable for ${GRACE}s)"
    fi
    return 0
  fi

  NOMETA_SINCE=0

  # PROPERTY 1. A live test process means the lock is not stale, regardless of
  # what happened to the wrapper. The wrapper can be SIGKILLed — it cannot trap
  # that — while `go test` and its containers keep running.
  #
  # The TOKEN is checked first and is the load-bearing one: it was written and
  # verified before the fork, so it covers the interval in which a child exists
  # but its pid has not been recorded yet. The pid checks corroborate it from
  # the other side. Any one of the three alive keeps the lock.
  if is_token_live "$holder_token"; then
    return 0
  fi
  # LOOKS REMOVABLE, IS NOT. Deleting this branch left the suite green at 74/74
  # in a mutation run, because every other case reaching "wrapper dead, command
  # alive" goes through the token above. It is what holds a lock written by a
  # version that recorded no token, and it is now pinned by the
  # `child-liveness-without-token` case so removing it reddens.
  if is_pid_live "$holder_child" "$holder_clstart"; then
    return 0
  fi
  if is_pid_live "$holder_pid" "$holder_lstart"; then
    return 0
  fi

  # FORK-TO-EXEC BACKSTOP. The holder said it was about to fork and never got
  # as far as recording a pid. A child may or may not exist, and if it does it
  # may be stopped or not yet exec'd, which makes it invisible to both the
  # token and the pid checks above. We cannot prove nothing is running, so the
  # lock stays held rather than being handed to a second run.
  #
  # Held on first sight, then bounded. A permanent hold here fired on a strict
  # superset of the fail-open's conditions — including the pre-fork part of the
  # interval, where no child exists at all — so it wedged locks over runs that
  # had provably started nothing. SPAWNING_SINCE is reset above whenever the
  # flag does not apply, so a holder that is merely slow to record its child
  # accumulates no credit towards being declared dead.
  if [ "$holder_spawning" = "1" ] && [ -z "$holder_child" ]; then
    if [ "$SPAWNING_SINCE" = "0" ]; then
      SPAWNING_SINCE="$(now)"
    fi
    if [ $(( $(now) - SPAWNING_SINCE )) -lt "$GRACE" ]; then
      return 0
    fi
    STALE_REASON="it died while starting its command (spawning, no child pid, dead holder and dead token for ${GRACE}s)"
    return 0
  fi

  if [ -n "$holder_token" ]; then
    STALE_REASON="nothing is running under holder token $holder_token, and pid $holder_pid is gone"
  elif [ -n "$holder_child" ]; then
    STALE_REASON="holder pid $holder_pid and its command pid $holder_child are both gone"
  elif ! ps -p "$holder_pid" >/dev/null 2>&1; then
    STALE_REASON="holder pid $holder_pid is no longer running"
  else
    STALE_REASON="pid $holder_pid was recycled (started '$(lstart_of "$holder_pid")', lock recorded '$holder_lstart')"
  fi
}

while :; do
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    HELD=1
    # PROPERTY 2. Fail closed: a lock we cannot account for is released, and
    # the command does not run. No `2>/dev/null` here — the reason a write
    # failed is the most useful line on the screen when this fires.
    if ! write_meta "$@"; then
      echo "$ME: refusing to run under a lock nobody can account for; releasing '$NAME'" >&2
      release
      exit 69
    fi
    break
  fi

  # Held by someone. Work out by whom, and whether they are still alive.
  assess_holder

  if [ -n "$STALE_REASON" ]; then
    # ---- RECLAIM (PROPERTY 3) ----------------------------------------------
    #
    # Reclaiming is NOISY, never silent — a lock that vanishes without a word
    # is how you end up debugging the wrong thing for an hour.
    #
    # Two waiters can reach this point holding the same verdict about the same
    # lock. If both act on it, the first moves the stale lock, removes it and
    # acquires a fresh one, and the second then moves and removes THAT — a live
    # lock — and both commands run. `mv` alone does not prevent this: it makes
    # each individual move atomic, but it cannot tell that the directory being
    # moved is still the one that was inspected.
    #
    # So reclaimers are serialised by a second mkdir mutex, and the verdict is
    # re-derived from scratch INSIDE it. That closes the window completely,
    # because once assess_holder has said "dead" under this mutex, the only
    # actors that could replace LOCK_DIR are the holder (dead, by that verdict)
    # and another reclaimer (excluded, by this mutex).
    if mkdir "$RECLAIM_DIR" 2>/dev/null; then
      RECLAIM_HELD=1

      # DEFECT 5, and it is defect 2 in a second location: this write used to
      # be suppressed and unchecked. An unwritten marker is worse here than a
      # missing lock metadata file, because the abandoned-marker recovery below
      # keys on the pid INSIDE it: with no pid to read, `[ -n "$r_pid" ]` is
      # false, nothing ever clears it, and every future reclaimer on this
      # machine wedges permanently. So it fails closed and loudly, the same way
      # write_meta does, and read-back is what proves it rather than the exit
      # status alone.
      if ! printf 'pid=%s\nlstart=%s\n' "$$" "$(lstart_of $$)" > "$RECLAIM_META"; then
        echo "$ME: cannot write the reclaim marker at $RECLAIM_META" >&2
        echo "$ME: releasing it rather than leaving one no other reclaimer could ever clear" >&2
        if ! remove_dir_hard "$RECLAIM_DIR"; then
          echo "$ME: FAILED to remove $RECLAIM_DIR — remove it by hand or reclaims on this machine will wedge" >&2
        fi
        RECLAIM_HELD=0
        exit 69
      fi
      if [ "$(sed -n 's/^pid=//p' "$RECLAIM_META" 2>/dev/null | head -1)" != "$$" ]; then
        echo "$ME: the reclaim marker at $RECLAIM_META did not read back as mine (pid $$)" >&2
        echo "$ME: releasing it rather than leaving one no other reclaimer could ever clear" >&2
        if ! remove_dir_hard "$RECLAIM_DIR"; then
          echo "$ME: FAILED to remove $RECLAIM_DIR — remove it by hand or reclaims on this machine will wedge" >&2
        fi
        RECLAIM_HELD=0
        exit 69
      fi

      # Re-derive the verdict from scratch. The lock may have been reclaimed
      # and re-taken by a live holder while we were getting here, and acting on
      # the older verdict is exactly how the first draft deleted a live lock.
      assess_holder
      if [ -n "$STALE_REASON" ]; then
        salvage="$LOCK_DIR.stale.$$.$(now)"
        if mv "$LOCK_DIR" "$salvage" 2>/dev/null; then
          echo "$ME: RECLAIMING STALE LOCK '$NAME' — $STALE_REASON" >&2
          [ -n "$holder_cmd" ] && echo "$ME:   dead holder ran: $holder_cmd" >&2
          [ -n "$holder_cwd" ] && echo "$ME:   dead holder cwd: $holder_cwd" >&2
          echo "$ME:   removing $LOCK_DIR and taking it" >&2
          # Suppressing this was the last unchecked write in the file, three
          # lines under a comment about that pattern. It only leaks *.stale.*
          # directories rather than failing open, but removing that shape is
          # what this whole change is for.
          if ! remove_dir_hard "$salvage"; then
            echo "$ME:   NOTE: could not remove the salvaged copy at $salvage — remove it by hand" >&2
          fi
        else
          # Not fail-open: without the rename we simply do not hold the lock,
          # and the loop re-assesses. Still said out loud, because a reclaim
          # that silently achieves nothing looks identical to one that worked.
          echo "$ME: could not rename $LOCK_DIR out of the way to reclaim it; will re-assess" >&2
        fi
      fi

      if ! remove_dir_hard "$RECLAIM_DIR"; then
        echo "$ME: FAILED to remove $RECLAIM_DIR — remove it by hand or reclaims on this machine will wedge" >&2
      fi
      RECLAIM_HELD=0

      # Retry the mkdir immediately rather than sleeping a whole poll interval,
      # but still spend a poll doing it. Without this the loop could `continue`
      # forever without ever reaching the deadline check below — a busy spin,
      # which is the unbounded-wait shape this project keeps killing agents on.
      POLLS=$(( POLLS + 1 ))
      if [ "$POLLS" -lt "$MAX_POLLS" ]; then
        continue
      fi
    else
      # Another waiter is reclaiming. Let it finish and re-look; do not race it.
      # A reclaimer killed mid-reclaim would otherwise wedge this forever, and
      # clearing one is safe in a way that clearing a LOCK is not: a reclaimer
      # holds no command, so a dead one cannot be running anything, and whoever
      # wins the marker still re-validates under it before removing anything.
      r_pid="$(sed -n 's/^pid=//p' "$RECLAIM_META" 2>/dev/null | head -1)"
      r_lstart="$(sed -n 's/^lstart=//p' "$RECLAIM_META" 2>/dev/null | head -1)"
      if [ -n "$r_pid" ] && ! is_pid_live "$r_pid" "$r_lstart"; then
        echo "$ME: clearing an abandoned reclaim marker left by dead pid $r_pid" >&2
        if ! remove_dir_hard "$RECLAIM_DIR"; then
          echo "$ME: FAILED to clear $RECLAIM_DIR — remove it by hand or reclaims on this machine will wedge" >&2
        fi
      fi
      BLOCKED_ON_RECLAIM=1
      # Deliberately NOT `continue`: fall through to the bounded wait below so
      # a reclaim that never completes still expires at the deadline instead of
      # spinning.
    fi
  fi

  # Live holder. Say so, and keep saying so: ten minutes of silence is
  # indistinguishable from a hang, which is requirement one.
  n="$(now)"
  waited=$(( n - STARTED_WAITING ))

  if [ "$BLOCKED_ON_RECLAIM" = "1" ]; then
    BLOCKED_ON_RECLAIM=0
    if [ "$ANNOUNCED_RECLAIM_WAIT" = "0" ]; then
      echo "$ME: another process is reclaiming the stale '$NAME' lock — waiting for it rather than racing it" >&2
      ANNOUNCED_RECLAIM_WAIT=1
    fi
  elif [ "$ANNOUNCED_WAIT" = "0" ]; then
    held_for="?"
    [ -n "$holder_since" ] && held_for="$(human_secs $(( n - holder_since )))"
    echo "$ME: waiting for lock '$NAME' — held by pid ${holder_pid:-?} for $held_for" >&2
    [ -n "$holder_child" ] && echo "$ME:   holder command pid: $holder_child" >&2
    [ -n "$holder_cwd" ] && echo "$ME:   holder cwd: $holder_cwd" >&2
    [ -n "$holder_cmd" ] && echo "$ME:   holder cmd: $holder_cmd" >&2
    echo "$ME:   will wait up to $(human_secs "$TIMEOUT"), reporting every ${NOTIFY}s" >&2
    spawning_notice
    ANNOUNCED_WAIT=1
    LAST_NOTICE="$n"
  elif [ $(( n - LAST_NOTICE )) -ge "$NOTIFY" ]; then
    held_for="?"
    [ -n "$holder_since" ] && held_for="$(human_secs $(( n - holder_since )))"
    echo "$ME: still waiting for '$NAME' after $(human_secs "$waited") — pid ${holder_pid:-?} has held it $held_for" >&2
    spawning_notice
    LAST_NOTICE="$n"
  fi

  POLLS=$(( POLLS + 1 ))
  if [ "$n" -ge "$DEADLINE" ] || [ "$POLLS" -ge "$MAX_POLLS" ]; then
    echo "$ME: TIMED OUT after $(human_secs "$waited") waiting for lock '$NAME'" >&2
    echo "$ME:   still held by pid ${holder_pid:-?}${holder_cmd:+ running: $holder_cmd}" >&2
    echo "$ME:   nothing was run. Raise WPMGR_LOCK_TIMEOUT, or wait for that run to finish." >&2
    spawning_notice
    exit 75
  fi

  sleep "$POLL"
done

# ---- Run -------------------------------------------------------------------
#
# The command is launched through a `sh -c` whose $0 is the TOKEN, which is
# what puts the token into the child's argv where `pgrep -f` can find it.
# `sh -c '...' TOKEN cmd args...` sets $0=TOKEN and runs cmd with args, so the
# command is unchanged and its status is sh's status.
#
# `; exit $?` IS LOAD-BEARING AND MUST NOT BE "SIMPLIFIED" AWAY. Written as the
# obvious `sh -c '"$@"' TOKEN cmd`, the body is a single simple command, so the
# shell exec-optimizes it: sh replaces itself with the command and the argv —
# token included — is gone. Measured, not assumed:
#
#   sh -c '"$@"'          TOKEN sleep 5  -> argv: `sleep 5`   pgrep: NO MATCH
#   sh -c '"$@"; exit $?' TOKEN sleep 5  -> argv: `sh -c "$@"; exit $? TOKEN sleep 5`
#
# The second statement gives sh something to do after the command, so it forks
# instead of exec'ing and stays alive holding the token for the whole run. The
# status still passes through untouched (`exit 42` -> 42, a missing command ->
# 127, both measured).
#
# The cost is one extra `sh` in the tree, which costs nothing in practice: the
# real target already invokes `sh -c 'cd apps/api && go test ...'`, so the
# command was a grandchild either way.
#
# `<&0` keeps the caller's stdin attached, which POSIX would otherwise replace
# with /dev/null for an asynchronous command. Status read with a bare `wait` on
# the next line. No pipe, no chain, no subshell.
sh -c '"$@"; exit $?' "$TOKEN" "$@" <&0 &
CHILD=$!

if ! record_child; then
  # An unrecorded child is precisely the fail-open this whole change removes:
  # kill this wrapper and the lock would look free while the command ran on. So
  # stop the command and refuse, rather than protect it with a lock that lies.
  echo "$ME: cannot record the command's pid ($CHILD) in $META" >&2
  echo "$ME: stopping it rather than running under a lock that cannot see it" >&2
  # The command has already started and may have descendants of its own, so
  # this stops the whole tree and refuses to release over anything that
  # survives. TERMing the token shell alone and releasing regardless left an
  # inner shell, `go test` or its containers live while the next invocation
  # took the lock.
  if ! stop_run; then
    refuse_release
    exit 69
  fi
  release
  exit 69
fi

wait "$CHILD"
status=$?

release
exit "$status"
