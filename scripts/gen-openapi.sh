#!/usr/bin/env bash
# scripts/gen-openapi.sh
#
# Regenerate both OpenAPI-derived trees from the single source of truth at
# packages/openapi/openapi.yaml:
#
#   apps/api/internal/api/gen/**            (ogen, Go types + validation)
#   packages/openapi-client/src/generated/**(@hey-api/openapi-ts, TS client)
#
# WHY THIS FILE IS NOT THREE LINES (GH #511). Until this commit it was an
# `echo` that said codegen was "wired in Phase 4" and exited 0. Both
# generators had been wired and in daily use for months. So the documented
# command reported success and regenerated nothing: you edited the spec, ran
# `make gen`, saw it pass, and committed a stale generated tree. Everything
# downstream then coded against a contract the API no longer served.
#
# The lesson this file encodes is that a generator which cannot prove it wrote
# something is indistinguishable from that `echo`. Every step below is
# therefore checked three ways: the tool is resolved before it is called, the
# call must exit 0, and the output tree must contain a file newer than a marker
# taken immediately before the call. A step that "succeeds" without touching
# its output tree is a failure here, loudly, with the tree named.
#
# RUN IT:
#   make gen                       regenerate both trees
#   make gen-check                 regenerate, then fail if the committed trees
#                                  differ from what was just generated
#   scripts/gen-openapi.sh [ROOT]  same, against another checkout
#   scripts/gen-openapi_test.sh    the test suite for this script
#
# Exit 0 only when both trees were regenerated (and, under --check, match what
# is committed). Every other outcome is a non-zero exit with a reason on stderr.
# There is no skip path and no "tool not installed, moving on" path: a missing
# tool is a hard stop, because silently skipping half of a two-tree codegen is
# how the two trees drift apart in the first place.
#
# TESTABILITY. ROOT is an argument and both generators are invoked through
# `go` and `pnpm` found on PATH, so scripts/gen-openapi_test.sh can build a
# throwaway tree, put shims on PATH, and drive every branch below — including
# the "generator exited 0 and wrote nothing" branch — without running the real
# generators or touching this repo.

set -euo pipefail

usage() {
	cat <<'USAGE'
usage: scripts/gen-openapi.sh [--check] [ROOT]

  --check   after generating, fail if the committed generated trees differ
            from what was just generated (drift detection for CI)
  ROOT      repository root to operate on (default: the repo containing this
            script)

  GEN_OPENAPI_DEADLINE  seconds each generator gets before it is killed
                         (default: 120). See run_generator in this script.
USAGE
}

CHECK=0
ROOT=""

while [ $# -gt 0 ]; do
	case "$1" in
	--check) CHECK=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		printf 'gen-openapi: unknown option: %s\n' "$1" >&2
		usage >&2
		exit 2
		;;
	*)
		if [ -n "$ROOT" ]; then
			printf 'gen-openapi: unexpected extra argument: %s\n' "$1" >&2
			exit 2
		fi
		ROOT="$1"
		;;
	esac
	shift
done

if [ -z "$ROOT" ]; then
	ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi

if [ ! -d "$ROOT" ]; then
	printf 'gen-openapi: root is not a directory: %s\n' "$ROOT" >&2
	exit 1
fi
ROOT="$(cd "$ROOT" && pwd)"

# The source of truth and the two trees derived from it. Kept as variables so
# the messages below can always name the concrete path that failed.
SPEC="$ROOT/packages/openapi/openapi.yaml"
GO_MOD_DIR="$ROOT/apps/api"
GO_OUT="$ROOT/apps/api/internal/api/gen"
TS_PKG="$ROOT/packages/openapi-client"
TS_OUT="$ROOT/packages/openapi-client/src/generated"

die() {
	printf 'gen-openapi: ERROR: %s\n' "$*" >&2
	exit 1
}

say() { printf 'gen-openapi: %s\n' "$*"; }

# Resolve a required binary or stop. `command -v` returning empty is a hard
# stop, never a shrug: a codegen step that cannot find its generator must fail
# loudly, because the alternative is a half-generated pair of trees that looks
# like a success.
require_tool() {
	local bin="$1" why="$2" resolved
	resolved="$(command -v "$bin" 2>/dev/null || true)"
	if [ -z "$resolved" ]; then
		die "required tool '$bin' was not found on PATH.
    It is needed to $why.
    PATH=$PATH
    Install '$bin' and re-run. This step is never skipped."
	fi
	printf 'gen-openapi:   %-6s %s\n' "$bin" "$resolved"
}

require_file() {
	[ -f "$1" ] || die "$2 is missing: $1"
}

require_dir() {
	[ -d "$1" ] || die "$2 is missing: $1"
}

MARKER=""
TIMEOUT_FLAG=""
cleanup() {
	[ -n "$MARKER" ] && rm -f "$MARKER"
	[ -n "$TIMEOUT_FLAG" ] && rm -f "$TIMEOUT_FLAG"
	return 0
}
trap cleanup EXIT

# Print $1 and every descendant pid of it, one per line, by walking
# `ps`-reported parent links. Used to reach a whole subprocess tree (a
# generator plus whatever it spawned) without process-group/job-control
# machinery: enabling job control (`set -m`) to get a fresh pgid needs a
# controlling terminal, and this script has none when it runs inside a pipe
# or `$(...)` -- which is exactly how its own test suite invokes it, and
# exactly how the `codegen-drift` CI job invokes it too. A bare `set -m` in
# that context does not reliably return; that was tried here and hung.
pid_tree() {
	local root="$1" pid ppid
	printf '%s\n' "$root"
	ps -Ao pid=,ppid= 2>/dev/null | while read -r pid ppid; do
		[ "$ppid" = "$root" ] && pid_tree "$pid"
	done
}

# Send $1 (a signal name) to $2 and every descendant it has *right now*. Called
# twice (TERM, then KILL after a grace period) so it re-walks the tree fresh
# each time rather than trusting a snapshot taken before the first signal.
kill_tree() {
	local sig="$1" root="$2" p
	for p in $(pid_tree "$root"); do
		kill -"$sig" "$p" 2>/dev/null || true
	done
}

# Run one generator, from a given working directory, and prove it actually
# wrote to its output tree.
#
# The marker file is created immediately before the generator runs, so any file
# the generator writes has a strictly newer mtime. If nothing under the output
# tree is newer than the marker, the generator did not regenerate anything --
# which is exactly the GH #511 failure -- and that is an error, not a pass.
#
# This does not over-fire on a no-change run: ogen is invoked with --clean and
# openapi-ts rewrites its output directory, so both rewrite every file on every
# run even when the bytes are identical. Idempotence here means "same content",
# not "untouched files"; `--check` is what asserts the content half.
#
# The working directory is a parameter and the command runs in a subshell,
# rather than under `env -C`: `env -C` needs coreutils >= 8.33, which is newer
# than some CI images ship.
run_generator() {
	local label="$1" out="$2" workdir="$3"
	shift 3
	local ps_probe ps_probe_status ps_usable ps_pid ps_ppid
	local tick_probe tick_advanced tick_budget tick_deadline tick_now

	MARKER="$(mktemp "${TMPDIR:-/tmp}/gen-openapi-marker.XXXXXX")"
	# Left at its mktemp-assigned "now", deliberately NOT back-dated. An
	# earlier version of this back-dated MARKER to a fixed 2020 timestamp to
	# fix a real, reproduced CI flake (a live write tying with the marker's
	# creation on a coarse-grained filesystem clock, read by strict `-newer`
	# as "wrote nothing"). That "fix" was itself wrong and was reverted (PR
	# #512 review): on a real checkout, the committed tree's files already
	# have a recent mtime from `git checkout`, not 2020, so *any* pre-existing
	# file would look "newer than 2020" whether or not this run's generator
	# wrote anything -- silently reintroducing the exact GH #511 defect this
	# script exists to catch.
	#
	# A second attempt at the clock-tie flake -- sleeping one second *after*
	# an ambiguous (empty) `-newer` result, then re-checking -- was also
	# wrong and was reverted (PR #512 review, gen-openapi.sh:398). Sleeping
	# after the fact changes neither MARKER's mtime nor the write's mtime:
	# both already happened, so the second check compares the exact same two
	# unchanged values and returns the exact same (empty) answer. A tie
	# cannot be detected after it has already occurred; it can only be
	# prevented from occurring. So: prove, not assume, that the filesystem
	# clock has advanced past MARKER's own mtime before the generator is
	# allowed to start, by touching a throwaway probe file and checking it
	# against MARKER with the same `find -newer` the real check below uses.
	# Once a probe write is observably newer than MARKER, every subsequent
	# write -- including the generator's -- is guaranteed strictly newer too,
	# on any clock granularity.
	#
	# A third attempt bounded this loop by a fixed attempt count (2000
	# undelayed touch+find pairs), not by elapsed time, and that was also
	# wrong (PR #512 review, gen-openapi.sh:227): on a fast host with coarse
	# (e.g. whole-second) mtime granularity, all 2000 attempts can complete
	# well inside a single tick with no sleep between them, so the clock
	# never advances, the count is exhausted, and a perfectly healthy run
	# fails before the generator is even invoked. That is the mirror image
	# of the original GH #511-follow-up defect: a wait bounded in the wrong
	# unit is the same shape as a wait with no bound at all. Bounded by
	# wall-clock time instead, via `date +%s` (whole seconds only -- `%N`
	# subsecond formatting is a GNU date extension this script cannot rely
	# on portably), with a short sleep between attempts so the loop yields
	# instead of spinning through however many attempts it takes without
	# ever giving the clock a chance to move. Fractional `sleep` was checked
	# rather than assumed: both this machine's BSD `sleep` (`Intervals can be
	# written in any form allowed by strtod(3)`, confirmed with `sleep 0.1`)
	# and GNU coreutils `sleep` support sub-second intervals, so 0.05s here
	# is safe on both platforms this script runs on. GEN_OPENAPI_TICK_WAIT
	# overrides the budget; this is a debug/test knob, not a documented
	# public interface the way GEN_OPENAPI_DEADLINE is, so it is not
	# separately validated -- a bad value fails loudly via bash's own
	# arithmetic error rather than silently.
	tick_probe="$(mktemp "${TMPDIR:-/tmp}/gen-openapi-tick.XXXXXX")"
	tick_budget="${GEN_OPENAPI_TICK_WAIT:-5}"
	tick_advanced=0
	tick_deadline=$(($(date +%s) + tick_budget))
	while :; do
		touch "$tick_probe" 2>/dev/null || true
		if [ -n "$(find "$tick_probe" -newer "$MARKER" -print -quit 2>/dev/null)" ]; then
			tick_advanced=1
			break
		fi
		tick_now="$(date +%s)"
		if [ "$tick_now" -ge "$tick_deadline" ]; then
			break
		fi
		sleep 0.05
	done
	rm -f "$tick_probe"
	if [ "$tick_advanced" -ne 1 ]; then
		die "the filesystem clock did not observably advance past the
    write-detection marker within ${tick_budget}s, so $label cannot be
    started with a write-detection check this script can trust.
    Marker: $MARKER
    Probe:  $tick_probe (already removed)
    This filesystem's mtime granularity is coarser than this script's
    ${tick_budget}s budget for observing it advance -- a real condition
    worth investigating directly, not a generic failure. Override
    GEN_OPENAPI_TICK_WAIT for a longer budget if that is expected here."
	fi

	# A bounded, process-tree-aware deadline (GH #511 follow-up). Without
	# this, a generator -- or a child it spawns, which matters because pnpm
	# spawns its own children -- that stalls means this script cannot return,
	# and `make gen` hangs indefinitely. That is exactly the shape this
	# project's working agreement calls out: never wait on a process with an
	# unbounded loop.
	#
	# This is implemented directly in shell rather than shelled out to
	# `timeout(1)`: that binary is not installed on every machine this script
	# runs on (notably not on macOS by default), and a `timeout`-wrapped
	# command that silently fails to resolve reads as a pass, which is the
	# exact failure this file exists to prevent. `command -v timeout` was
	# checked and came back empty here, so the deadline below uses only
	# `sleep`, `kill` and `ps`, which are assumed present the same way the
	# rest of the script already assumes `find`/`mktemp`/`cat`.
	#
	# An earlier version of this tried to get the child a fresh process group
	# via job control (`set -m`) and signal `-$pgid`. That is the obvious
	# answer and it is wrong here: job control needs a controlling terminal,
	# and `set -m` inside a pipe or `$(...)` -- with no tty -- does not
	# reliably return. It hung this exact script when driven from the test
	# suite's `OUT="$(... bash gen-openapi.sh ...)"`, and `ci.yml`'s
	# `codegen-drift` job invokes this script the same way (piped, no tty), so
	# the hang was not a test-harness artifact, it would have hung CI. The
	# watchdog below instead walks `ps`'s parent-pid links (`pid_tree` /
	# `kill_tree`, defined above) to find every real descendant of the
	# generator and signals each by pid. TERM first, then KILL after a short
	# grace period, in case something in the tree traps or ignores TERM.
	#
	# Default: 120s. `time make gen` regenerates both trees, together, in
	# 3.3-4.3s warm. 120s is roughly 30-35x that for a single generator's
	# share of the work, which comfortably covers a cold CI cache (go module
	# download, pnpm install) while still turning a real hang into a bounded,
	# actionable failure within two minutes instead of never returning.
	# Overridable via GEN_OPENAPI_DEADLINE, which is how the test suite
	# proves this in seconds rather than minutes.
	local deadline="${GEN_OPENAPI_DEADLINE:-120}"

	# Validated before use: this feeds straight into `sleep "$deadline"`
	# below. A non-numeric value makes `sleep` error or return immediately,
	# the watchdog then finds the generator still alive at once, and kills a
	# perfectly healthy generator while reporting a deadline breach -- a
	# false accusation indistinguishable from a real one. `0` has the same
	# effect (`sleep 0` returns immediately too). Reject both, naming what
	# was actually passed.
	case "$deadline" in
	'' | *[!0-9]*)
		die "GEN_OPENAPI_DEADLINE must be a positive integer number of seconds, got: '$deadline'" ;;
	esac
	case "$deadline" in
	*[1-9]*) : ;;
	*) die "GEN_OPENAPI_DEADLINE must be greater than 0, got: '$deadline'" ;;
	esac

	# `sleep` backs the deadline itself (the watchdog's very first command).
	# If it is missing, the watchdog subshell dies on that line before it can
	# ever check on the generator, silently disabling the deadline rather
	# than enforcing it -- the exact unbounded-wait failure this file exists
	# to prevent, just reached through a missing dependency instead of a
	# stalled process.
	if ! command -v sleep >/dev/null 2>&1; then
		die "'sleep' was not found on PATH, so the ${deadline}s deadline for
    $label cannot be enforced. Refusing to start $label rather than run it
    with no enforceable deadline."
	fi

	# `ps -Ao pid=,ppid=` must both succeed and return data pid_tree can
	# actually parse, checked before the generator starts, not discovered
	# when the watchdog needs it. A status-only check is not enough: `ps` can
	# exit 0 and print nothing (or something that is not "PID PPID" pairs),
	# and pid_tree/kill_tree would then silently walk zero descendants --
	# kill_tree still returns success in that case, so a stalled generator's
	# children, and the watchdog's own `sleep`, would survive a "kill" that
	# never reached them. That is the exact hang already fixed once in this
	# file (see kill_tree's own comment above), reachable again by a
	# different route: a broken `ps` instead of a broken job-control model.
	ps_probe="$(ps -Ao pid=,ppid= 2>&1)" && ps_probe_status=0 || ps_probe_status=$?
	if [ "$ps_probe_status" -ne 0 ]; then
		die "'ps -Ao pid=,ppid=' failed (exit $ps_probe_status), so the deadline
    watchdog cannot find or signal a stalled generator's descendants.
    Refusing to start $label rather than start something this script could
    not safely kill.
    Output: $ps_probe"
	fi
	ps_usable=0
	while read -r ps_pid ps_ppid; do
		case "$ps_pid" in '' | *[!0-9]*) continue ;; esac
		case "$ps_ppid" in '' | *[!0-9]*) continue ;; esac
		ps_usable=1
		break
	done <<<"$ps_probe"
	if [ "$ps_usable" -ne 1 ]; then
		die "'ps -Ao pid=,ppid=' exited 0 but returned no usable PID/PPID pairs,
    so the deadline watchdog cannot find or signal a stalled generator's
    descendants. Refusing to start $label rather than start something this
    script could not safely kill.
    Output: $ps_probe"
	fi

	say "$label: running (in $workdir, ${deadline}s deadline): $*"

	# stdin is explicitly /dev/null, not inherited. A backgrounded child that
	# keeps the invoking shell's controlling terminal as its stdin can stall
	# indefinitely under that terminal's job-control arbitration even though
	# it never reads from stdin -- reproduced directly while building this:
	# the identical command hung until the deadline killed it with stdin
	# inherited, and returned in under a second with stdin as /dev/null.
	# Neither generator needs interactive input, so this is also just
	# correct for a background, deadline-guarded process.
	(cd "$workdir" && exec "$@") </dev/null &
	local pid=$!

	TIMEOUT_FLAG="$(mktemp "${TMPDIR:-/tmp}/gen-openapi-timeout.XXXXXX")"
	rm -f "$TIMEOUT_FLAG"

	(
		sleep "$deadline"
		if kill -0 "$pid" 2>/dev/null; then
			(: >"$TIMEOUT_FLAG") 2>/dev/null || true
			kill_tree TERM "$pid"
			sleep 2
			kill_tree KILL "$pid"
		fi
	) &
	local watchdog=$!

	# Deliberately `if wait; then ... else status=$?; fi`, not `if ! wait;
	# then status=$?; fi`: with `!`, the `$?` seen inside the `then` branch is
	# the *negated* status bash assigns to the `!`-prefixed pipeline, not
	# wait's own exit code, so it silently records the wrong value on every
	# failure. Caught by the go-fail case below, which expects to see "go/ogen
	# generator failed" and instead got the wrong branch entirely.
	local status=0
	if wait "$pid"; then
		status=0
	else
		status=$?
	fi

	# The job finished (or was killed) either way; stop the watchdog if it is
	# still sleeping so a fast, successful run does not wait out the deadline.
	# `kill "$watchdog"` alone is not enough: the watchdog is a subshell
	# blocked inside its own `sleep "$deadline"`, and signalling the subshell
	# terminates the subshell without touching the `sleep` process it is
	# waiting on -- that leaves `sleep` orphaned (reparented to pid 1) but
	# still running for the rest of the deadline, still holding this script's
	# inherited stdout/stderr open. Any caller reading this script's output
	# through a pipe -- exactly how `scripts/gen-openapi_test.sh` captures it,
	# and exactly how a caller doing `out=$(... scripts/gen-openapi.sh ...)`
	# would too -- then blocks reading for EOF that never comes until that
	# orphaned `sleep` finally finishes. Reproduced directly while building
	# this: the happy-path case hung past its default 120s deadline with
	# plain `kill`, and returned immediately once this used `kill_tree`.
	kill_tree TERM "$watchdog"
	wait "$watchdog" 2>/dev/null || true

	if [ -f "$TIMEOUT_FLAG" ]; then
		rm -f "$TIMEOUT_FLAG"
		TIMEOUT_FLAG=""
		die "$label generator exceeded its ${deadline}s deadline and was killed, along with
    every descendant process it had at the time.
    Working directory: $workdir
    Command: $*
    This is the unbounded-wait failure mode: the generator, or a child it
    spawned, stalled and never returned. Override GEN_OPENAPI_DEADLINE if
    $label is legitimately this slow; otherwise treat this as a hang in
    $label and investigate it directly."
	fi
	rm -f "$TIMEOUT_FLAG"
	TIMEOUT_FLAG=""

	if [ "$status" -ne 0 ]; then
		die "$label generator failed (see its output above).
    Working directory: $workdir
    Command: $*"
	fi

	# No retry needed here: the clock-advance wait above already guarantees
	# MARKER is strictly older than anything written from this point on, so
	# a single strict `-newer` check is unambiguous -- empty means the
	# generator genuinely wrote nothing.
	if [ -z "$(find "$out" -type f -newer "$MARKER" -print -quit 2>/dev/null)" ]; then
		die "$label generator exited 0 but wrote nothing under:
      $out
    That is the GH #511 failure: a codegen command that reports success and
    regenerates nothing, leaving a stale tree that looks freshly generated.
    Command: $*"
	fi

	rm -f "$MARKER"
	MARKER=""
	say "$label: regenerated $out"
}

say "root: $ROOT"
say "spec: $SPEC"

require_file "$SPEC" "the OpenAPI source of truth"
require_dir "$GO_MOD_DIR" "the Go module directory"
require_dir "$GO_OUT" "the ogen output tree"
require_dir "$TS_PKG" "the TypeScript client package"
require_dir "$TS_OUT" "the openapi-ts output tree"

say "tools:"
require_tool go "run ogen and regenerate $GO_OUT"
require_tool pnpm "run @hey-api/openapi-ts and regenerate $TS_OUT"

# Go: driven through the //go:generate directive in
# apps/api/internal/api/gen/generate.go so the ogen flags live in exactly one
# place. Changing the directive changes what this script runs; there is no
# second copy of the command here to drift out of sync with it.
run_generator "go/ogen" "$GO_OUT" "$GO_MOD_DIR" \
	go generate ./internal/api/gen

# TypeScript: driven through the package's own `generate` script so the
# openapi-ts flags live in packages/openapi-client/openapi-ts.config.ts, again
# in exactly one place.
run_generator "ts/openapi-ts" "$TS_OUT" "$TS_PKG" \
	pnpm generate

if [ "$CHECK" -eq 0 ]; then
	say "OK: both trees regenerated from $SPEC"
	exit 0
fi

# --check: the committed trees must equal what was just generated. This is the
# check that catches the stale-tree commit itself, rather than trusting every
# contributor to have run the command. It is deliberately scoped to the two
# generated trees, so an unrelated dirty file in the working tree cannot redden
# it.
require_tool git "compare the committed generated trees against a fresh generation"

if ! (cd "$ROOT" && git rev-parse --git-dir >/dev/null 2>&1); then
	die "--check needs a git checkout, and $ROOT is not one."
fi

say "check: comparing committed trees against the fresh generation"

# --exit-code is what fails; --stat gives a reviewer the shape of the drift.
if (cd "$ROOT" && git diff --exit-code --stat -- "$GO_OUT" "$TS_OUT"); then
	say "OK: committed generated trees match a fresh generation"
	exit 0
fi

printf '\n' >&2
cat >&2 <<EOF
gen-openapi: ERROR: the committed generated trees do not match a fresh
    generation from packages/openapi/openapi.yaml. The spec and the generated
    clients have drifted apart.

    Fix it by committing the regeneration:

      make gen
      git add apps/api/internal/api/gen packages/openapi-client/src/generated
      git commit

    The full drift is printed above as a diffstat.
EOF
exit 1
