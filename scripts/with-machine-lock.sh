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
# The one remaining gap is between fork() and exec(), while the child still
# carries the parent's argv and the token is not yet visible in it — microseconds,
# and unobservable in practice because declaring a lock stale requires a SECOND
# assessment taken later, under the reclaim mutex. `child`/`clstart` are kept
# alongside the token for exactly that corroboration; either one alive is enough.
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
on_signal() { # on_signal SIGNAME SIGNUM
  echo "" >&2
  echo "$ME: caught SIG$1 — stopping the command and releasing the $NAME lock" >&2
  if [ -n "$CHILD" ] && kill -0 "$CHILD" 2>/dev/null; then
    kill -TERM "$CHILD" 2>/dev/null
    wait "$CHILD" 2>/dev/null
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
  _cmd="$(printf '%s' "$*" | tr '\n\r' '  ')"
  # `token` goes in HERE, before anything is forked, and is verified below.
  # That ordering is the whole point: after this function returns successfully,
  # the lock can identify the run even though the run does not exist yet.
  if ! printf 'pid=%s\nacquired=%s\nlstart=%s\ntoken=%s\nhost=%s\ncwd=%s\ncmd=%s\n' \
      "$$" "$(now)" "$(lstart_of $$)" "$TOKEN" "$(hostname 2>/dev/null || echo unknown)" "$PWD" "$_cmd" \
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
BLOCKED_ON_RECLAIM=0
ANNOUNCED_RECLAIM_WAIT=0

# assess_holder reads the lock's metadata and decides whether it is stale,
# setting holder_* and STALE_REASON. It is a function because the reclaim path
# re-runs it from scratch under the reclaim mutex; the first draft decided once
# and acted on that verdict later, which is what let a waiter delete a lock
# that had been acquired in the meantime.
assess_holder() {
  holder_pid="$(meta_field pid || true)"
  holder_lstart="$(meta_field lstart || true)"
  holder_token="$(meta_field token || true)"
  holder_child="$(meta_field child || true)"
  holder_clstart="$(meta_field clstart || true)"
  holder_since="$(meta_field acquired || true)"
  holder_cwd="$(meta_field cwd || true)"
  holder_cmd="$(meta_field cmd || true)"

  STALE_REASON=""

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
  # the other side (they cover the fork-to-exec instant, where the token is not
  # yet in the child's argv). Any one of the three alive keeps the lock.
  if is_token_live "$holder_token"; then
    return 0
  fi
  if is_pid_live "$holder_child" "$holder_clstart"; then
    return 0
  fi
  if is_pid_live "$holder_pid" "$holder_lstart"; then
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
          rm -rf "$salvage" 2>/dev/null
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
  kill -TERM "$CHILD" 2>/dev/null
  wait "$CHILD" 2>/dev/null
  release
  exit 69
fi

wait "$CHILD"
status=$?

release
exit "$status"
