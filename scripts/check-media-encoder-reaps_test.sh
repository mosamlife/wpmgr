#!/usr/bin/env bash
# scripts/check-media-encoder-reaps_test.sh
#
# The regression suite for scripts/check-media-encoder-reaps.sh (GH #290).
#
# WHY THIS EXISTS. The check's job is to be trusted when it says a container
# reaps its orphans. A check nobody can run is a check nobody can verify, and
# this one fails in the silent direction: a /proc that cannot be read, a
# `docker exec` that errors, or a scan that prints nothing would all look like
# "no zombies" to a careless implementation. So the cases that matter most are
# not the ones where it finds a zombie. They are:
#
#   * it learns NOTHING and must still go red (exit 2, never 0), and
#   * it is shown CORRECT work and must stay green: other process states, a
#     process whose name imitates a state, an entry that vanishes mid-scan, a
#     zombie caught mid-reap.
#
# The second is not optional politeness. A check that reddens correct work gets
# switched off, and then it guards nothing at all.
#
# HOW IT WORKS. No Docker. Each case builds a fixture /proc tree on disk and a
# fake `docker` that serves it: the fake runs the REAL in-container scan script
# (the same text production sends through `docker exec`, under dash when the
# machine has it) against the fixture, and answers inspect / run / stop / rm
# from small files. The check is run exactly as production runs it, through
# WPMGR_REAPS_DOCKER, and the suite asserts on its exit status, its output and
# the calls the fake saw.
#
# RUN IT:
#   scripts/check-media-encoder-reaps_test.sh            # everything
#   scripts/check-media-encoder-reaps_test.sh image      # cases whose name matches "image"
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a defect in a copy, watch the suite go red):
#   WPMGR_REAPS_GUARD_SCRIPT=/tmp/check-with-hole.sh \
#     scripts/check-media-encoder-reaps_test.sh
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop and on an ubuntu runner. No mapfile, no associative
# arrays, no sed -i, no grep -P.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_REAPS_GUARD_SCRIPT:-$HERE/check-media-encoder-reaps.sh}"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  echo "no check script at $GUARD" >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-reaps-test.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
FAILED_NAMES=''
RC=0
OUT=''

# The fake runs the in-container scan under the shell the image has, when this
# machine has it. The runtime image's /bin/sh is dash.
FAKE_SH="$(command -v dash || command -v sh)" || { echo "no sh on PATH" >&2; exit 2; }

# ---------------------------------------------------------------------------
# The fake docker
# ---------------------------------------------------------------------------

mkdir -p "$WORK/fakebin"
cat > "$WORK/fakebin/docker" <<'FAKE'
#!/bin/sh
# Fake docker for check-media-encoder-reaps_test.sh. Behaviour comes from files
# under $FAKE_DIR; every call is appended to $FAKE_DIR/calls.
D="${FAKE_DIR:?FAKE_DIR is not set}"

role_of() {
  case "$1" in
    *-neg) echo neg ;;
    *) echo good ;;
  esac
}

case "$1" in
  exec)
    # exec CONTAINER sh -c SCRIPT NAME [ARGS...]
    ctr="$2"
    name="$6"
    echo "exec $ctr $name" >> "$D/calls"
    role="$(role_of "$ctr")"
    case "$name" in
      wpmgr-orphan-probe)
        if [ -e "$D/orphan_fail" ]; then echo "fake: orphan exec failed" >&2; exit 1; fi
        exit 0
        ;;
      wpmgr-proc-scan)
        if [ -e "$D/scan_fail" ]; then echo "fake: scan exec failed" >&2; exit 1; fi
        if [ -e "$D/scan_silent" ]; then exit 0; fi
        if [ -e "$D/scan_canned" ]; then cat "$D/scan_canned"; exit 0; fi
        n=0
        if [ -r "$D/scans.$role" ]; then n="$(cat "$D/scans.$role")"; fi
        n=$((n + 1))
        echo "$n" > "$D/scans.$role"
        list="$D/proc.$role"
        if [ ! -r "$list" ]; then echo "fake: no proc fixture for $role" >&2; exit 1; fi
        total="$(wc -l < "$list" | tr -d ' ')"
        pick="$n"
        if [ "$pick" -gt "$total" ]; then pick="$total"; fi
        root="$(sed -n "${pick}p" "$list")"
        exec "${FAKE_SH:-sh}" -c "$5" "$name" "$root"
        ;;
    esac
    echo "fake: unexpected exec $*" >&2
    exit 1
    ;;
  inspect)
    # inspect --format FORMAT ID
    echo "inspect $4" >> "$D/calls"
    if [ -e "$D/inspect_fail" ]; then echo "fake: inspect failed" >&2; exit 1; fi
    role="$(role_of "$4")"
    case "$3" in
      *State.Running*)
        if [ -r "$D/running.$role" ]; then cat "$D/running.$role"; else echo true; fi
        ;;
      *State.ExitCode*)
        if [ -r "$D/exitcode.$role" ]; then cat "$D/exitcode.$role"; else echo 143; fi
        ;;
      *)
        echo "fake: unexpected inspect format $3" >&2
        exit 1
        ;;
    esac
    ;;
  image)
    # image inspect --format FORMAT IMAGE
    echo "image inspect $5" >> "$D/calls"
    if [ -e "$D/image_inspect_fail" ]; then echo "fake: no such image" >&2; exit 1; fi
    if [ -r "$D/entrypoint" ]; then cat "$D/entrypoint"; else echo "fake: no entrypoint fixture" >&2; exit 1; fi
    ;;
  run)
    echo "$*" >> "$D/calls"
    if [ -e "$D/run_fail" ]; then echo "fake: run failed" >&2; exit 125; fi
    name=""
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--name" ]; then name="$a"; fi
      prev="$a"
    done
    echo "$name"
    ;;
  stop)
    echo "$*" >> "$D/calls"
    if [ -e "$D/stop_fail" ]; then echo "fake: stop failed" >&2; exit 1; fi
    ;;
  rm)
    echo "$*" >> "$D/calls"
    ;;
  *)
    echo "fake: unexpected command $*" >&2
    exit 1
    ;;
esac
FAKE
chmod +x "$WORK/fakebin/docker"

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

# stat_line PID COMM STATE: a realistic /proc/PID/stat line.
stat_line() {
  printf '%s (%s) %s 0 %s %s 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 1000 1000000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n' \
    "$1" "$2" "$3" "$1" "$1"
}

# add_proc ROOT PID COMM STATE
add_proc() {
  mkdir -p "$1/$2"
  stat_line "$2" "$3" "$4" > "$1/$2/stat"
  printf '%s\n' "$3" > "$1/$2/comm"
}

# A fixture /proc whose PID 1 is $2 (a reaping init in the good case), with the
# processes a running media-encoder container really has.
fx_base() {
  rm -rf "$1"
  mkdir -p "$1"
  add_proc "$1" 1 "$2" S
  add_proc "$1" 8 media-encoder S
  add_proc "$1" 21 chromium S
  add_proc "$1" 22 chromium R
}

# ---------------------------------------------------------------------------
# Harness
# ---------------------------------------------------------------------------

CASE_N=0
CASEDIR=''

new_case() {
  CASE_N=$((CASE_N + 1))
  CASEDIR="$WORK/case-$CASE_N"
  mkdir -p "$CASEDIR"
  : > "$CASEDIR/calls"
}

# set_proc ROLE ROOT [ROOT...]: the fixture the Nth scan of ROLE reads.
set_proc() {
  _role="$1"
  shift
  : > "$CASEDIR/proc.$_role"
  for _r in "$@"; do printf '%s\n' "$_r" >> "$CASEDIR/proc.$_role"; done
}

# run_check ARGS...: sets OUT (stdout and stderr) and RC.
run_check() {
  OUT="$(
    FAKE_DIR="$CASEDIR" FAKE_SH="$FAKE_SH" \
      WPMGR_REAPS_DOCKER="${CHECK_DOCKER:-$WORK/fakebin/docker}" \
      WPMGR_REAPS_POLL_SLEEP=0 WPMGR_REAPS_POLLS=3 \
      "$GUARD" "$@" 2>&1
  )"
  RC=$?
}

pass() { PASSED=$((PASSED + 1)); printf 'ok   %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES
  - $1"
  printf 'FAIL %s\n' "$1"
  printf '     %s\n' "$2"
  printf '     --- output (rc=%s) ---\n' "$RC"
  printf '%s\n' "$OUT" | sed 's/^/     /'
  printf '     --- end ---\n'
}

want_rc() {
  if [ "$RC" = "$2" ]; then return 0; fi
  fail "$1" "expected exit $2, got $RC"
  return 1
}

want_says() {
  if printf '%s' "$OUT" | grep -qF -- "$2"; then return 0; fi
  fail "$1" "expected the output to mention: $2"
  return 1
}

want_silent_about() {
  if printf '%s' "$OUT" | grep -qF -- "$2"; then
    fail "$1" "expected the output NOT to mention: $2"
    return 1
  fi
  return 0
}

# want_calls NAME PATTERN COUNT: the fake saw exactly COUNT calls matching.
want_calls() {
  _n="$(grep -c -- "$2" "$CASEDIR/calls" || true)"
  if [ "$_n" = "$3" ]; then return 0; fi
  fail "$1" "expected $3 call(s) matching '$2', saw $_n"
  printf '     --- calls ---\n'
  sed 's/^/     /' "$CASEDIR/calls"
  return 1
}

should_run() {
  [ -z "$FILTER" ] && return 0
  case "$1" in *"$FILTER"*) return 0 ;; esac
  return 1
}

# ===========================================================================
# GROUP 1 -- correct work stays green.
# ===========================================================================

NAME='baseline: PID 1 is tini and nothing is a zombie'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 0 &&
    want_says "$NAME" "OK: PID 1 is 'tini'; 0 zombies among 4 processes" &&
    pass "$NAME"
fi

NAME='baseline: every state other than Z is left alone'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  add_proc "$CASEDIR/p1" 30 a D
  add_proc "$CASEDIR/p1" 31 b T
  add_proc "$CASEDIR/p1" 32 c t
  add_proc "$CASEDIR/p1" 33 d I
  add_proc "$CASEDIR/p1" 34 e X
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 0 &&
    want_says "$NAME" '0 zombies among 9 processes' &&
    want_silent_about "$NAME" 'zombie pid=' &&
    pass "$NAME"
fi

NAME='baseline: a process NAMED like a zombie state is not a zombie'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  # stat reads: 40 (a) Z (b) S ...  The first ") " is followed by Z; the real
  # state, after the LAST ") ", is S.
  add_proc "$CASEDIR/p1" 40 'a) Z (b' S
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 0 &&
    want_says "$NAME" '0 zombies among 5 processes' &&
    want_silent_about "$NAME" 'zombie pid=' &&
    pass "$NAME"
fi

NAME='baseline: a /proc entry that vanished mid-scan and non-process entries are skipped'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  mkdir -p "$CASEDIR/p1/50"              # exited between the glob and the read: no stat
  mkdir -p "$CASEDIR/p1/self" "$CASEDIR/p1/1abc"
  echo 'not a process' > "$CASEDIR/p1/1abc/stat"
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 0 &&
    want_says "$NAME" '0 zombies among 4 processes' &&
    pass "$NAME"
fi

NAME='baseline: --expect-init accepts another init when PID 1 is that init'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" docker-init
  set_proc good "$CASEDIR/p1"
  run_check --expect-init docker-init fakectr
  want_rc "$NAME" 0 &&
    want_says "$NAME" "OK: PID 1 is 'docker-init'" &&
    pass "$NAME"
fi

NAME='baseline: a zombie caught mid-reap that is gone on the next look is not a finding'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  add_proc "$CASEDIR/p1" 60 sleep Z
  fx_base "$CASEDIR/p2" tini
  set_proc good "$CASEDIR/p1" "$CASEDIR/p2"
  run_check fakectr
  want_rc "$NAME" 0 &&
    want_says "$NAME" "OK: PID 1 is 'tini'" &&
    want_calls "$NAME" 'wpmgr-proc-scan' 2 &&
    pass "$NAME"
fi

NAME='baseline: the orphan is made once, and before any scan'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  first="$(grep -n 'wpmgr-orphan-probe' "$CASEDIR/calls" | head -1 | cut -d: -f1)"
  firstscan="$(grep -n 'wpmgr-proc-scan' "$CASEDIR/calls" | head -1 | cut -d: -f1)"
  want_rc "$NAME" 0 &&
    want_calls "$NAME" 'wpmgr-orphan-probe' 1 &&
    { [ -n "$first" ] && [ -n "$firstscan" ] && [ "$first" -lt "$firstscan" ] ||
      fail "$NAME" "the orphan must be created before the first scan (probe at $first, scan at $firstscan)"; } &&
    pass "$NAME"
fi

# ===========================================================================
# GROUP 2 -- findings go red.
# ===========================================================================

NAME='finding: a zombie that survives every look exits 1 and names it'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  add_proc "$CASEDIR/p1" 20 sleep Z
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 1 &&
    want_says "$NAME" '1 zombie process(es) remained' &&
    want_says "$NAME" 'zombie pid=20 comm=sleep' &&
    want_calls "$NAME" 'wpmgr-proc-scan' 3 &&
    pass "$NAME"
fi

NAME='finding: several zombies are all counted'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  add_proc "$CASEDIR/p1" 20 sleep Z
  add_proc "$CASEDIR/p1" 23 chrome_crashpad Z
  add_proc "$CASEDIR/p1" 24 chromium Z
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 1 &&
    want_says "$NAME" '3 zombie process(es) remained' &&
    want_says "$NAME" 'zombie pid=23 comm=chrome_crashpad' &&
    pass "$NAME"
fi

NAME='finding: a zombie whose name imitates a live state is still a zombie'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  # stat reads: 41 (a) S (b) Z ...  Reading after the FIRST ") " says S.
  add_proc "$CASEDIR/p1" 41 'a) S (b' Z
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'zombie pid=41 comm=a) S (b' &&
    pass "$NAME"
fi

NAME='finding: PID 1 that is not the init exits 1 even with no zombie'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" media-encoder
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 1 &&
    want_says "$NAME" "PID 1 is 'media-encoder', expected 'tini'" &&
    pass "$NAME"
fi

NAME='finding: the real defect, a non-reaping PID 1 with a zombie, reports both'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" media-encoder
  add_proc "$CASEDIR/p1" 20 sleep Z
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 1 &&
    want_says "$NAME" "PID 1 is 'media-encoder', expected 'tini'" &&
    want_says "$NAME" 'zombie pid=20 comm=sleep' &&
    pass "$NAME"
fi

NAME='finding: --expect-init names the init that must be PID 1'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  set_proc good "$CASEDIR/p1"
  run_check --expect-init docker-init fakectr
  want_rc "$NAME" 1 &&
    want_says "$NAME" "expected 'docker-init'" &&
    pass "$NAME"
fi

# ===========================================================================
# GROUP 3 -- A CHECK THAT LEARNS NOTHING MUST GO RED, never green.
#
# Every case here would, in a naive implementation, print nothing, find no
# zombie and exit 0. Each must exit exactly 2.
# ===========================================================================

NAME='broken: docker is not on the machine'
if should_run "$NAME"; then
  new_case
  CHECK_DOCKER=/nonexistent/path/to/docker run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'docker was not found' &&
    pass "$NAME"
fi

NAME='broken: the container is not running'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  set_proc good "$CASEDIR/p1"
  echo false > "$CASEDIR/running.good"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'is not running' &&
    want_calls "$NAME" 'wpmgr-orphan-probe' 0 &&
    pass "$NAME"
fi

NAME='broken: docker inspect fails'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  set_proc good "$CASEDIR/p1"
  touch "$CASEDIR/inspect_fail"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'docker inspect' &&
    pass "$NAME"
fi

NAME='broken: the orphan cannot be created'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  set_proc good "$CASEDIR/p1"
  touch "$CASEDIR/orphan_fail"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'could not create the orphan' &&
    want_calls "$NAME" 'wpmgr-proc-scan' 0 &&
    pass "$NAME"
fi

NAME='broken: docker exec of the scan fails'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  set_proc good "$CASEDIR/p1"
  touch "$CASEDIR/scan_fail"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'could not scan /proc' &&
    pass "$NAME"
fi

NAME='broken: /proc/1/comm cannot be read'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  rm -f "$CASEDIR/p1/1/comm"
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'cannot read' &&
    pass "$NAME"
fi

NAME='broken: /proc holds no processes at all'
if should_run "$NAME"; then
  new_case
  mkdir -p "$CASEDIR/p1"
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 2 &&
    pass "$NAME"
fi

NAME='broken: /proc does not exist'
if should_run "$NAME"; then
  new_case
  set_proc good "$CASEDIR/no-such-proc"
  run_check fakectr
  want_rc "$NAME" 2 &&
    pass "$NAME"
fi

NAME='broken: PID 1 has a comm but no stat, so it was never scanned'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  rm -f "$CASEDIR/p1/1/stat"
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'pid 1 was not scanned' &&
    pass "$NAME"
fi

NAME='broken: PID 1 stat is garbage'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  echo 'garbage with no state' > "$CASEDIR/p1/1/stat"
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'unparseable' &&
    pass "$NAME"
fi

NAME='broken: a corrupt stat for any process is not silently skipped'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  add_proc "$CASEDIR/p1" 70 sleep Z
  echo '70 sleep Z 1' > "$CASEDIR/p1/70/stat"   # no parentheses: not a stat line
  set_proc good "$CASEDIR/p1"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'unparseable' &&
    pass "$NAME"
fi

NAME='broken: the scan exits 0 and prints nothing'
if should_run "$NAME"; then
  new_case
  fx_base "$CASEDIR/p1" tini
  set_proc good "$CASEDIR/p1"
  touch "$CASEDIR/scan_silent"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'did not print its summary line' &&
    pass "$NAME"
fi

# The next cases feed the host a scan the in-container script would never
# print. They guard the host's own reading of it: a count it cannot reconcile
# with the listing, or a scan that saw nothing, is not a clean bill of health.

NAME='broken: the scan counts more zombies than it lists'
if should_run "$NAME"; then
  new_case
  printf 'init=tini\nzombie pid=7 comm=sleep\nscanned=4 zombies=2\n' > "$CASEDIR/scan_canned"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'counted 2 zombies but listed 1' &&
    pass "$NAME"
fi

NAME='broken: the scan lists a zombie its summary does not count'
if should_run "$NAME"; then
  new_case
  printf 'init=tini\nzombie pid=7 comm=sleep\nscanned=4 zombies=0\n' > "$CASEDIR/scan_canned"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'counted 0 zombies but listed 1' &&
    pass "$NAME"
fi

NAME='broken: the scan saw no processes at all'
if should_run "$NAME"; then
  new_case
  printf 'init=tini\nscanned=0 zombies=0\n' > "$CASEDIR/scan_canned"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'saw no processes at all' &&
    pass "$NAME"
fi

NAME='broken: the scan does not say what PID 1 is'
if should_run "$NAME"; then
  new_case
  printf 'scanned=4 zombies=0\n' > "$CASEDIR/scan_canned"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'did not report what PID 1 is' &&
    pass "$NAME"
fi

NAME='broken: the scan prints two summaries'
if should_run "$NAME"; then
  new_case
  printf 'init=tini\nscanned=4 zombies=0\nscanned=4 zombies=0\n' > "$CASEDIR/scan_canned"
  run_check fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'more than one summary line' &&
    pass "$NAME"
fi

NAME='broken: no container given'
if should_run "$NAME"; then
  new_case
  run_check
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'usage:' &&
    pass "$NAME"
fi

NAME='broken: an unknown option'
if should_run "$NAME"; then
  new_case
  run_check --frobnicate fakectr
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'unknown option' &&
    pass "$NAME"
fi

NAME='broken: two targets'
if should_run "$NAME"; then
  new_case
  run_check one two
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'usage:' &&
    pass "$NAME"
fi

# ===========================================================================
# GROUP 4 -- --image: what a release runs against a built image.
# ===========================================================================

# image_case ENTRYPOINT_LINES...: a good image. The shipped init's container
# is clean; the negative-control container, whose PID 1 is a plain sleep,
# shows the zombie it must show.
image_case() {
  new_case
  : > "$CASEDIR/entrypoint"
  for _l in "$@"; do printf '%s\n' "$_l" >> "$CASEDIR/entrypoint"; done
  fx_base "$CASEDIR/good" tini
  fx_base "$CASEDIR/neg" sleep
  add_proc "$CASEDIR/neg" 20 sleep Z
  set_proc good "$CASEDIR/good"
  set_proc neg "$CASEDIR/neg"
}

NAME='image: the shipped entrypoint, a clean run, SIGTERM forwarded and a seen negative control pass'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  run_check --image img:test
  want_rc "$NAME" 0 &&
    want_says "$NAME" 'OK: the ENTRYPOINT of img:test starts with /usr/bin/tini' &&
    want_says "$NAME" "OK: PID 1 is 'tini'" &&
    want_says "$NAME" 'OK: docker stop ended the child with the SIGTERM the init forwarded' &&
    want_says "$NAME" 'OK: negative control' &&
    want_calls "$NAME" '^rm -f wpmgr-reaps-check-[0-9]* wpmgr-reaps-check-[0-9]*-neg$' 1 &&
    pass "$NAME"
fi

NAME='image: the container is started from the image own init, without a daemon-injected one'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  run_check --image img:test
  want_rc "$NAME" 0 &&
    want_calls "$NAME" '^run -d --init=false --name wpmgr-reaps-check-[0-9]* --entrypoint /usr/bin/tini img:test -- sleep 600$' 1 &&
    want_calls "$NAME" '^run -d --init=false --name wpmgr-reaps-check-[0-9]*-neg --entrypoint sleep img:test 600$' 1 &&
    pass "$NAME"
fi

NAME='image: an ENTRYPOINT that is the bare binary exits 1 and starts nothing'
if should_run "$NAME"; then
  image_case /usr/local/bin/media-encoder
  run_check --image img:test
  want_rc "$NAME" 1 &&
    want_says "$NAME" "starts with '/usr/local/bin/media-encoder', not 'tini'" &&
    want_calls "$NAME" '^run ' 0 &&
    pass "$NAME"
fi

NAME='image: an image with no ENTRYPOINT exits 1'
if should_run "$NAME"; then
  image_case
  run_check --image img:test
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'has no ENTRYPOINT' &&
    want_calls "$NAME" '^run ' 0 &&
    pass "$NAME"
fi

NAME='image: a docker stop that had to SIGKILL the child exits 1'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  echo 137 > "$CASEDIR/exitcode.good"
  run_check --image img:test
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'did not forward SIGTERM' &&
    pass "$NAME"
fi

NAME='image: any other exit code after docker stop exits 1'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  echo 0 > "$CASEDIR/exitcode.good"
  run_check --image img:test
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'expected 143' &&
    pass "$NAME"
fi

NAME='image: a zombie under the shipped init exits 1, and the containers are still removed'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  add_proc "$CASEDIR/good" 20 sleep Z
  run_check --image img:test
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'zombie pid=20 comm=sleep' &&
    want_calls "$NAME" '^rm -f ' 1 &&
    pass "$NAME"
fi

NAME='image: a negative control that sees no zombie exits 2, never 0'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  fx_base "$CASEDIR/neg" sleep               # the defect is invisible here
  run_check --image img:test
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'negative control' &&
    want_says "$NAME" 'cannot show the defect' &&
    want_calls "$NAME" '^rm -f ' 1 &&
    pass "$NAME"
fi

NAME='image: docker run failing exits 2'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  touch "$CASEDIR/run_fail"
  run_check --image img:test
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'could not start a container' &&
    want_calls "$NAME" '^rm -f ' 1 &&
    pass "$NAME"
fi

NAME='image: docker stop failing exits 2'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  touch "$CASEDIR/stop_fail"
  run_check --image img:test
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'docker stop' &&
    pass "$NAME"
fi

NAME='image: an image docker cannot inspect exits 2'
if should_run "$NAME"; then
  image_case /usr/bin/tini -- /usr/local/bin/media-encoder
  touch "$CASEDIR/image_inspect_fail"
  run_check --image img:test
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'docker image inspect' &&
    want_calls "$NAME" '^run ' 0 &&
    pass "$NAME"
fi

# ===========================================================================
# Result
# ===========================================================================

echo
echo "check-media-encoder-reaps self-test: $PASSED passed, $FAILED failed"
if [ "$FAILED" -gt 0 ]; then
  echo "failed cases:$FAILED_NAMES"
  exit 1
fi
if [ "$PASSED" -eq 0 ]; then
  # A suite that ran nothing is not a green suite.
  echo "no cases ran (filter: '${FILTER}')" >&2
  exit 2
fi
exit 0
