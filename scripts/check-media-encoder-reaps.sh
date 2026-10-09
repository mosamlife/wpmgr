#!/usr/bin/env bash
# scripts/check-media-encoder-reaps.sh
#
# Does a media-encoder container reap the processes Chromium orphans?
# (GH #290)
#
# ---------------------------------------------------------------------------
# WHY THIS EXISTS
# ---------------------------------------------------------------------------
#
# Each website screenshot starts headless Chromium. Chromium double-forks its
# crash handler and leaves helper processes behind when the browser exits, so
# the kernel hands those orphans to PID 1 of the container. Reaping them is
# PID 1's job. The media-encoder's PID 1 used to be the Go binary, which never
# waits on children it did not start, so every orphan stayed in the process
# table as a zombie until the container restarted. Each one holds a PID slot
# and nothing frees it.
#
# The fix is an init process as PID 1 (tini, infra/Dockerfile.media-encoder).
# This script is the gate that keeps it fixed: nobody reading a Dockerfile can
# tell whether the entrypoint reaps, but a running container can be asked.
#
# ---------------------------------------------------------------------------
# WHAT IT CHECKS
# ---------------------------------------------------------------------------
#
#   check-media-encoder-reaps.sh [--expect-init NAME] CONTAINER
#
#     1. /proc/1/comm inside the running container is NAME (default: tini).
#     2. An orphan is made on purpose (`docker exec ... sh -c 'sh -c "sleep 1
#        &"; sleep 3'`: the inner shell exits at once, so the sleep is
#        orphaned and handed to PID 1, and it has exited by the time the outer
#        sleep returns).
#     3. Every /proc/[0-9]*/stat inside the container is read, and processes
#        in state Z are counted. The slim runtime image has no ps, so this is
#        done with the shell. A zombie that lingers across every poll is a
#        finding; one that vanishes on a re-poll was merely caught mid-reap.
#
#   check-media-encoder-reaps.sh [--expect-init NAME] --image IMAGE
#
#     Everything a release needs, from a built image alone:
#
#     a. The image's own ENTRYPOINT starts with NAME. Without this, a revert
#        to the bare binary would still pass (b), because (b) replaces what
#        the init runs.
#     b. A throwaway container is started from the image's own init binary,
#        with a sleeper as the child in place of media-encoder. The real
#        binary needs a database and object storage to stay up, and a gate
#        that reddens whenever those are missing gets switched off. The init
#        under test is the shipped one, not a stand-in. Check 1-3 above run
#        against it.
#     c. `docker stop` must end the child with the SIGTERM tini forwarded
#        (exit 143). A PID 1 that swallows SIGTERM makes docker wait out the
#        grace period and SIGKILL it (exit 137), and that is what would cut a
#        drain off mid-job.
#     d. NEGATIVE CONTROL. A second container whose PID 1 is a plain `sleep`,
#        which never reaps, is put through the same check. It must come back
#        with the zombie finding. If it does not, this environment cannot show
#        the defect at all and the pass in (b) means nothing, so the script
#        fails instead of reporting it.
#
# EXIT STATUS. Three outcomes, never conflated:
#   0  PID 1 is the expected init and no zombie survived the orphan.
#   1  A finding: wrong PID 1, zombies, a stopped child that was SIGKILLed, or
#      an image whose ENTRYPOINT is not the init.
#   2  The check could not be made: docker missing, container not running,
#      `docker exec` failing, /proc unreadable or unparseable, output that does
#      not look like a scan, a blind negative control, bad arguments. A run
#      that learned nothing must not look like a run that found nothing.
#
# WHAT THIS DOES NOT SEE.
#   * It uses a synthetic orphan (`sleep`), not Chromium's crash handler. What
#     it proves is the property the defect turns on: whatever is PID 1 reaps a
#     process that was orphaned in the container.
#   * It runs against the image with a sleeper child, so it says nothing about
#     media-encoder's own startup or whether the binary drains on SIGTERM; (c)
#     proves only that the signal reaches the child.
#   * It does not drive a real screenshot capture.
#
# SELF-TEST: scripts/check-media-encoder-reaps_test.sh. ci.yml runs it; the
# real check runs in release.yml against the image before it is pushed.
#
# PORTABILITY. bash 3.2 (macOS) and POSIX tools; the in-container scan is
# POSIX sh (dash on the image).

set -uo pipefail

ME="check-media-encoder-reaps"
EXPECT_INIT="tini"
MODE=""
TARGET=""

# How many times the scan is repeated before a zombie counts, and the pause
# between looks. The orphan has already exited when the first look is taken, so
# a reaping PID 1 shows zero at once; the extra looks only absorb a reap that
# is still in progress.
POLLS="${WPMGR_REAPS_POLLS:-5}"
POLL_SLEEP="${WPMGR_REAPS_POLL_SLEEP:-1}"
STOP_GRACE="${WPMGR_REAPS_STOP_GRACE:-10}"

# The docker binary. Overridable so the self-test can substitute a fake.
DOCKER_REQ="${WPMGR_REAPS_DOCKER:-docker}"

# An orphan that has certainly exited by the time this returns: the sleep is
# 1s, the wait is 3s.
ORPHAN_SCRIPT='sh -c "sleep 1 >/dev/null 2>&1 </dev/null &"; sleep 3'

# Runs inside the container under POSIX sh: $1 is the proc root.
#
# The state is the first field AFTER THE LAST ") " in the line, never after
# the first: comm is free text and may itself contain ") Z (".
# Exit 3 means "could not scan"; the host treats it as a broken check.
# shellcheck disable=SC2016 # this text is sent to the container, not expanded here
SCAN_SCRIPT='
root=$1
[ -r "$root/1/comm" ] || { echo "cannot read $root/1/comm" >&2; exit 3; }
init=
{ read -r init; } 2>/dev/null < "$root/1/comm"
printf "init=%s\n" "$init"
scanned=0
zombies=0
saw1=0
for d in "$root"/[0-9]*; do
  pid=${d##*/}
  case $pid in *[!0-9]*) continue ;; esac
  [ -r "$d/stat" ] || continue
  line=
  { read -r line; } 2>/dev/null < "$d/stat" || [ -n "$line" ] || continue
  rest=${line##*") "}
  state=${rest%% *}
  case $state in
    [A-Za-z]) ;;
    *) echo "unparseable $d/stat: $line" >&2; exit 3 ;;
  esac
  [ "$pid" = 1 ] && saw1=1
  scanned=$((scanned + 1))
  if [ "$state" = Z ]; then
    zombies=$((zombies + 1))
    comm=${line#*"("}
    comm=${comm%")"*}
    printf "zombie pid=%s comm=%s\n" "$pid" "$comm"
  fi
done
[ "$saw1" = 1 ] || { echo "pid 1 was not scanned under $root" >&2; exit 3; }
printf "scanned=%s zombies=%s\n" "$scanned" "$zombies"
'

TMP="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-reaps.XXXXXX")" || { echo "$ME: cannot create a temp dir" >&2; exit 2; }
CLEANUP_NAMES=""

# shellcheck disable=SC2329 # runs from the EXIT trap below
cleanup() {
  if [ -n "$CLEANUP_NAMES" ] && [ -n "${DOCKER:-}" ]; then
    # shellcheck disable=SC2086 # the names are ours and contain no whitespace
    "$DOCKER" rm -f $CLEANUP_NAMES >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# broken() is the exit-2 path. It is separate from a finding so that "I
# learned nothing" can never be confused with "I found nothing".
broken() {
  printf '%s: CHECK BROKEN: %s\n' "$ME" "$1" >&2
  printf '%s: CHECK BROKEN: refusing to report a result from a check that could not be made.\n' "$ME" >&2
  exit 2
}

usage() {
  printf 'usage: %s [--expect-init NAME] CONTAINER\n       %s [--expect-init NAME] --image IMAGE\n' "$ME" "$ME" >&2
  exit 2
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

while [ $# -gt 0 ]; do
  case "$1" in
    --expect-init)
      [ $# -ge 2 ] || usage
      EXPECT_INIT="$2"
      shift 2
      ;;
    --image)
      [ $# -ge 2 ] || usage
      [ -z "$MODE" ] || usage
      MODE="image"
      TARGET="$2"
      shift 2
      ;;
    -h | --help)
      usage
      ;;
    -*)
      printf '%s: unknown option %s\n' "$ME" "$1" >&2
      usage
      ;;
    *)
      [ -z "$MODE" ] || usage
      MODE="container"
      TARGET="$1"
      shift
      ;;
  esac
done

[ -n "$MODE" ] && [ -n "$TARGET" ] || usage
[ -n "$EXPECT_INIT" ] || usage

case "$POLLS" in *[!0-9]* | "") broken "WPMGR_REAPS_POLLS must be a whole number, got '$POLLS'" ;; esac
[ "$POLLS" -ge 1 ] || broken "WPMGR_REAPS_POLLS must be at least 1, got '$POLLS'"
case "$POLL_SLEEP" in *[!0-9]* | "") broken "WPMGR_REAPS_POLL_SLEEP must be a whole number of seconds, got '$POLL_SLEEP'" ;; esac
case "$STOP_GRACE" in *[!0-9]* | "") broken "WPMGR_REAPS_STOP_GRACE must be a whole number of seconds, got '$STOP_GRACE'" ;; esac

# A gate that cannot find its binary fails loudly. Never skipped.
DOCKER="$(command -v "$DOCKER_REQ")" || broken "docker was not found ('$DOCKER_REQ'): cannot check a container"
[ -n "$DOCKER" ] || broken "docker was not found ('$DOCKER_REQ'): cannot check a container"

# ---------------------------------------------------------------------------
# docker helpers
# ---------------------------------------------------------------------------

ERRF="$TMP/stderr"

# first_line TEXT -> TEXT's first line
first_line() {
  printf '%s\n' "$1" | sed -n '1p'
}

# running CONTAINER -> 0 only when docker says the container is running.
running() {
  _state="$("$DOCKER" inspect --format '{{.State.Running}}' "$1" 2>"$ERRF")" ||
    broken "docker inspect $1 failed: $(cat "$ERRF")"
  [ "$_state" = "true" ]
}

# scan CONTAINER
#
# Sets SCAN_INIT, SCAN_SCANNED, SCAN_ZOMBIES and SCAN_DETAIL (one line per
# zombie). Any docker failure, any non-zero exit from the in-container scan
# and any output that is not exactly a scan is a broken check.
scan() {
  _out="$("$DOCKER" exec "$1" sh -c "$SCAN_SCRIPT" wpmgr-proc-scan /proc 2>"$ERRF")" ||
    broken "could not scan /proc inside $1 (docker exec failed): $(cat "$ERRF")"

  SCAN_INIT="$(printf '%s\n' "$_out" | sed -n 's/^init=//p' | sed -n '1p')"
  _summary="$(printf '%s\n' "$_out" | grep -E '^scanned=[0-9]+ zombies=[0-9]+$')"
  [ -n "$_summary" ] || broken "the scan of $1 did not print its summary line; got: $_out"
  [ "$(printf '%s\n' "$_summary" | wc -l | tr -d ' ')" = "1" ] || broken "the scan of $1 printed more than one summary line"
  SCAN_SCANNED="$(printf '%s\n' "$_summary" | sed 's/^scanned=\([0-9]*\) zombies=.*/\1/')"
  SCAN_ZOMBIES="$(printf '%s\n' "$_summary" | sed 's/^scanned=[0-9]* zombies=\([0-9]*\)$/\1/')"
  SCAN_DETAIL="$(printf '%s\n' "$_out" | grep '^zombie ' || true)"

  [ -n "$SCAN_INIT" ] || broken "the scan of $1 did not report what PID 1 is"
  [ "$SCAN_SCANNED" -ge 1 ] || broken "the scan of $1 saw no processes at all"
  _listed=0
  if [ -n "$SCAN_DETAIL" ]; then _listed="$(printf '%s\n' "$SCAN_DETAIL" | wc -l | tr -d ' ')"; fi
  [ "$_listed" = "$SCAN_ZOMBIES" ] || broken "the scan of $1 counted $SCAN_ZOMBIES zombies but listed $_listed"
}

# reap_check CONTAINER
#
# The container-level check. Sets REPORT (the text to show) and returns 0 for
# clean or 1 for a finding; a check that could not be made exits 2 through
# broken().
reap_check() {
  running "$1" || broken "container $1 is not running, so there is nothing to check"

  "$DOCKER" exec "$1" sh -c "$ORPHAN_SCRIPT" wpmgr-orphan-probe >/dev/null 2>"$ERRF" ||
    broken "could not create the orphan inside $1 (docker exec failed): $(cat "$ERRF")"

  _poll=1
  while :; do
    scan "$1"
    [ "$SCAN_ZOMBIES" = "0" ] && break
    [ "$_poll" -ge "$POLLS" ] && break
    _poll=$((_poll + 1))
    sleep "$POLL_SLEEP"
  done

  REPORT=""
  _bad=0
  if [ "$SCAN_INIT" != "$EXPECT_INIT" ]; then
    _bad=1
    REPORT="FAIL: PID 1 is '$SCAN_INIT', expected '$EXPECT_INIT': nothing in the container reaps orphaned processes"
  fi
  if [ "$SCAN_ZOMBIES" != "0" ]; then
    _bad=1
    _z="FAIL: $SCAN_ZOMBIES zombie process(es) remained after an orphan was created and exited (seen on all $_poll look(s)):
$(printf '%s\n' "$SCAN_DETAIL" | sed 's/^/  /')"
    if [ -n "$REPORT" ]; then REPORT="$REPORT
$_z"; else REPORT="$_z"; fi
  fi
  if [ "$_bad" = "0" ]; then
    REPORT="OK: PID 1 is '$SCAN_INIT'; 0 zombies among $SCAN_SCANNED processes after an orphan exited"
  fi
  return "$_bad"
}

# ---------------------------------------------------------------------------
# --image: what a release needs from a built image
# ---------------------------------------------------------------------------

check_image() {
  _img="$1"

  _entry="$("$DOCKER" image inspect --format '{{range .Config.Entrypoint}}{{println .}}{{end}}' "$_img" 2>"$ERRF")" ||
    broken "docker image inspect $_img failed: $(cat "$ERRF")"
  _init_path="$(first_line "$_entry")"
  if [ -z "$_init_path" ]; then
    echo "FAIL: $_img has no ENTRYPOINT, so its PID 1 is whatever the platform runs"
    return 1
  fi
  if [ "$(basename "$_init_path")" != "$EXPECT_INIT" ]; then
    echo "FAIL: the ENTRYPOINT of $_img starts with '$_init_path', not '$EXPECT_INIT'"
    echo "      entrypoint: $(printf '%s\n' "$_entry" | tr '\n' ' ')"
    return 1
  fi
  echo "OK: the ENTRYPOINT of $_img starts with $_init_path"

  _name="wpmgr-reaps-check-$$"
  CLEANUP_NAMES="$_name $_name-neg"
  _status=0

  # b. the shipped init, with a sleeper as its child.
  "$DOCKER" run -d --init=false --name "$_name" --entrypoint "$_init_path" "$_img" -- sleep 600 >/dev/null 2>"$ERRF" ||
    broken "could not start a container from $_img: $(cat "$ERRF")"

  if reap_check "$_name"; then :; else _status=1; fi
  echo "$REPORT"

  # c. SIGTERM must reach the child.
  "$DOCKER" stop -t "$STOP_GRACE" "$_name" >/dev/null 2>"$ERRF" ||
    broken "docker stop $_name failed: $(cat "$ERRF")"
  _code="$("$DOCKER" inspect --format '{{.State.ExitCode}}' "$_name" 2>"$ERRF")" ||
    broken "could not read the exit code of $_name: $(cat "$ERRF")"
  case "$_code" in
    143)
      echo "OK: docker stop ended the child with the SIGTERM the init forwarded (exit 143)"
      ;;
    137)
      echo "FAIL: docker stop had to SIGKILL the child (exit 137): the init did not forward SIGTERM"
      _status=1
      ;;
    *)
      echo "FAIL: docker stop left the child with exit $_code, expected 143 (SIGTERM forwarded by the init)"
      _status=1
      ;;
  esac

  # d. negative control: a PID 1 that never reaps must be reported.
  "$DOCKER" run -d --init=false --name "$_name-neg" --entrypoint sleep "$_img" 600 >/dev/null 2>"$ERRF" ||
    broken "could not start the negative-control container from $_img: $(cat "$ERRF")"
  _rc=0
  reap_check "$_name-neg" || _rc=$?
  if [ "$_rc" = "1" ] && [ "$SCAN_ZOMBIES" != "0" ]; then
    echo "OK: negative control: a PID 1 that never reaps was reported ($SCAN_ZOMBIES zombie(s)), so this check can see the defect here"
  else
    broken "negative control: a PID 1 that never reaps showed no zombie, so this environment cannot show the defect and the result above proves nothing"
  fi

  return "$_status"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

RC=0
case "$MODE" in
  container)
    if reap_check "$TARGET"; then RC=0; else RC=1; fi
    echo "$REPORT"
    ;;
  image)
    check_image "$TARGET" || RC=1
    ;;
esac

exit "$RC"
