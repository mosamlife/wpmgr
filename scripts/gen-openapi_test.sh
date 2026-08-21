#!/usr/bin/env bash
# scripts/gen-openapi_test.sh
#
# Test suite for scripts/gen-openapi.sh (GH #511).
#
# The bug this guards against is a codegen entry point that exits 0 without
# generating anything. A suite that only ran the real generators could not test
# that, because the real generators do work. So each case here builds a
# throwaway repo-shaped tree and puts shims named `go` and `pnpm` on PATH that
# behave exactly as badly as we need: write nothing, exit non-zero, or work.
# That drives every branch of gen-openapi.sh in about a second, with no Go
# toolchain, no node_modules and no network, and without touching this repo.
#
# The one thing shims cannot prove is that the REAL generators regenerate the
# real trees. That is proven by running `make gen` on the real checkout and by
# the codegen-drift job in ci.yml, not here.
#
# RUN IT:
#   make gen-test        or  scripts/gen-openapi_test.sh
#
# Exit 0 when every case passes, 1 when any case fails.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNDER_TEST="$SCRIPT_DIR/gen-openapi.sh"

if [ ! -f "$UNDER_TEST" ]; then
	printf 'gen-openapi_test: script under test not found: %s\n' "$UNDER_TEST" >&2
	exit 1
fi

PASS=0
FAIL=0
# Normalised through `cd && pwd`, because TMPDIR often carries a trailing
# slash and the script under test normalises its own root the same way. Without
# this the expected and actual paths differ only by a doubled slash, which is a
# test bug that reads exactly like a product bug.
WORKROOT="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/gen-openapi-test.XXXXXX")" && pwd)"
trap 'rm -rf "$WORKROOT"' EXIT

# The missing-tool cases need a PATH that provably does NOT contain `go` or
# `pnpm` but still contains the utilities the script under test calls. It is
# built here as a directory of symlinks to exactly those utilities, rather than
# hard-coded as a list of system directories.
#
# Hard-coding it was wrong, and CI said so: an earlier version used
# "/usr/bin:/bin:/usr/sbin:/sbin", which is genuinely stripped on macOS but not
# on a GitHub ubuntu runner, where Go is installed at /usr/bin/go. The abort
# below caught it rather than letting the missing-tool cases quietly test
# nothing, which is the whole point of having it.
BASE_BIN="$WORKROOT/basebin"
mkdir -p "$BASE_BIN"

# bash runs the script under test; the rest are what the script itself calls
# (mktemp/find/rm/touch for the marker, cat for the drift message, git for
# --check, ps/sleep/kill for the run_generator deadline watchdog -- kill is a
# bash builtin so it needs no PATH entry, but ps, sleep and touch are
# external).
for tool in bash mktemp find rm cat git ps sleep touch; do
	resolved="$(command -v "$tool" 2>/dev/null || true)"
	if [ -z "$resolved" ]; then
		printf 'gen-openapi_test: FATAL: required utility %s not found on PATH.\n' "$tool" >&2
		printf '  The suite cannot build its stripped PATH without it. Aborting\n' >&2
		printf '  rather than reporting a pass that proved nothing.\n' >&2
		exit 1
	fi
	ln -sf "$resolved" "$BASE_BIN/$tool"
done

BASE_PATH="$BASE_BIN"

# Assert the stripped PATH really is stripped. If either generator resolves
# under it, every missing-tool case below would silently test the opposite of
# what it claims, so stop instead.
for tool in go pnpm; do
	if PATH="$BASE_PATH" command -v "$tool" >/dev/null 2>&1; then
		printf 'gen-openapi_test: FATAL: `%s` resolves on the stripped PATH (%s),\n' "$tool" "$BASE_PATH" >&2
		printf '  so the missing-tool cases cannot be tested honestly. Aborting\n' >&2
		printf '  rather than reporting a pass that proved nothing.\n' >&2
		exit 1
	fi
done

ok() {
	PASS=$((PASS + 1))
	printf '  ok   %s\n' "$1"
}

not_ok() {
	FAIL=$((FAIL + 1))
	printf '  FAIL %s\n' "$1"
	if [ -n "${2:-}" ]; then
		printf '       %s\n' "$2"
	fi
}

# Build a repo-shaped tree: the spec, both output trees (each seeded with an
# old file so "wrote nothing" is distinguishable from "wrote something"), and a
# shim directory.
#
# $1 = case name. Echoes the tree root.
make_tree() {
	local name="$1" root="$WORKROOT/$1"
	mkdir -p "$root/packages/openapi"
	mkdir -p "$root/apps/api/internal/api/gen"
	mkdir -p "$root/packages/openapi-client/src/generated"
	mkdir -p "$root/bin"
	printf 'openapi: 3.1.0\n' >"$root/packages/openapi/openapi.yaml"
	printf 'package gen // stale\n' >"$root/apps/api/internal/api/gen/oas_schemas_gen.go"
	printf 'export const stale = 1;\n' >"$root/packages/openapi-client/src/generated/types.gen.ts"
	# Backdate the seeded output so a generator that writes nothing leaves
	# nothing newer than the marker, on any filesystem timestamp granularity.
	touch -t 202001010000 "$root/apps/api/internal/api/gen/oas_schemas_gen.go"
	touch -t 202001010000 "$root/packages/openapi-client/src/generated/types.gen.ts"
	printf '%s' "$root"
}

# Install a shim. $1=tree root, $2=name (go|pnpm|git), $3=behaviour, $4=extra
# (only used by `hang`, see below):
#   write    -- exit 0 after writing "generation A" into the output tree
#   write-b  -- exit 0 after writing "generation B", i.e. different bytes
#   nothing  -- exit 0 and touch nothing
#   fail     -- exit 3 with a message
#   hang     -- never returns: ignores SIGTERM itself, spawns a grandchild
#               that also ignores SIGTERM, writes the grandchild's pid to
#               $4, then blocks forever. This is the GH #511-follow-up
#               shape: a stalled generator with a stalled child, and one
#               that does not die on the deadline's first (TERM) signal, so
#               reaching it proves the deadline's SIGKILL escalation and not
#               just its happy path.
#
# `write` and `write-b` differ by content, not by a timestamp. An earlier
# version of this file wrote `$(date)` and expected two runs to differ; both
# runs landed in the same second, the bytes matched, and the drift case passed
# by accident in the wrong direction.
write_shim() {
	local root="$1" name="$2" behaviour="$3" extra="${4:-}" target="" shim="$1/bin/$2"
	case "$name" in
	go) target="$root/apps/api/internal/api/gen/oas_schemas_gen.go" ;;
	pnpm) target="$root/packages/openapi-client/src/generated/types.gen.ts" ;;
	esac
	{
		printf '#!/bin/sh\n'
		printf 'printf "shim %s: %%s\\n" "$*"\n' "$name"
		case "$behaviour" in
		write) printf 'printf "generation A\\n" > "%s"\nexit 0\n' "$target" ;;
		write-b) printf 'printf "generation B, from a changed spec\\n" > "%s"\nexit 0\n' "$target" ;;
		nothing) printf 'exit 0\n' ;;
		fail) printf 'printf "shim %s: boom\\n" >&2\nexit 3\n' "$name" ;;
		hang)
			printf 'trap "" TERM\n'
			printf '( trap "" TERM; sleep 9999 ) &\n'
			printf 'printf "%%s\\n" "$!" > "%s"\n' "$extra"
			printf 'wait\n'
			;;
		esac
	} >"$shim"
	chmod +x "$shim"
}

# Run the script under test against a tree with a shimmed PATH.
# $1=tree root; remaining args go to the script. Captures combined output in
# $OUT and the exit status in $STATUS.
run_case() {
	local root="$1"
	shift
	OUT="$(PATH="$root/bin:$BASE_PATH" bash "$UNDER_TEST" "$@" 2>&1)"
	STATUS=$?
}

expect_status() {
	local name="$1" want="$2"
	if [ "$STATUS" -eq "$want" ]; then
		ok "$name: exit $want"
	else
		not_ok "$name: expected exit $want, got $STATUS" "output was:
$OUT"
	fi
}

expect_output() {
	local name="$1" needle="$2"
	case "$OUT" in
	*"$needle"*) ok "$name: output mentions '$needle'" ;;
	*) not_ok "$name: output does not mention '$needle'" "output was:
$OUT" ;;
	esac
}

printf 'gen-openapi_test: %s\n' "$UNDER_TEST"
printf 'gen-openapi_test: stripped PATH for missing-tool cases: %s\n\n' "$BASE_PATH"

# ---------------------------------------------------------------------------
printf 'case: both generators write -- must succeed (the over-fire control)\n'
T="$(make_tree happy)"
write_shim "$T" go write
write_shim "$T" pnpm write
run_case "$T" "$T"
expect_status "happy" 0
# Trailing relative portion only, not the full "$T/..." path: $T comes from
# mktemp -d, and on macOS /tmp is a symlink to /private/tmp (mktemp itself
# resolves it, so the absolute prefix happens to match), while on Linux
# there is no such symlink and the prefixes diverge even though the script's
# behaviour is identical. See go-noop/ts-noop below for where this was
# caught: it still fails if the message stops naming the tree.
expect_output "happy" "regenerated"
expect_output "happy" "apps/api/internal/api/gen"
expect_output "happy" "packages/openapi-client/src/generated"
expect_output "happy" "OK: both trees regenerated"

# ---------------------------------------------------------------------------
printf '\ncase: `go` missing -- must fail loudly, naming go, not skip\n'
T="$(make_tree no_go)"
write_shim "$T" pnpm write
run_case "$T" "$T"
expect_status "missing-go" 1
expect_output "missing-go" "required tool 'go' was not found on PATH"
expect_output "missing-go" "This step is never skipped"

# ---------------------------------------------------------------------------
printf '\ncase: `pnpm` missing -- must fail loudly, naming pnpm, not skip\n'
T="$(make_tree no_pnpm)"
write_shim "$T" go write
run_case "$T" "$T"
expect_status "missing-pnpm" 1
expect_output "missing-pnpm" "required tool 'pnpm' was not found on PATH"

# ---------------------------------------------------------------------------
# The GH #511 defect itself, in both halves.
printf '\ncase: go generator exits 0 and writes nothing -- must fail\n'
T="$(make_tree go_noop)"
write_shim "$T" go nothing
write_shim "$T" pnpm write
run_case "$T" "$T"
expect_status "go-noop" 1
expect_output "go-noop" "exited 0 but wrote nothing"
# Trailing relative portion only, not "$T/...": $T is built from mktemp -d,
# and the absolute prefix the script prints does not portably equal it (this
# assertion was "$T/apps/api/internal/api/gen" and failed on Linux CI while
# passing locally on macOS, where /tmp happens to resolve through /private/tmp
# to the same path mktemp already returns). Still fails if the message stops
# naming which tree was empty.
expect_output "go-noop" "apps/api/internal/api/gen"

printf '\ncase: ts generator exits 0 and writes nothing -- must fail\n'
T="$(make_tree ts_noop)"
write_shim "$T" go write
write_shim "$T" pnpm nothing
run_case "$T" "$T"
expect_status "ts-noop" 1
expect_output "ts-noop" "exited 0 but wrote nothing"
# Trailing relative portion only -- see the go-noop comment above. This is
# the exact assertion that failed on Linux CI ("does not mention
# '/tmp/gen-openapi-test.FsicRA/ts_noop/packages/openapi-client/src/generated'")
# while the underlying behaviour (exit 1, message naming the tree) was
# already correct there.
expect_output "ts-noop" "packages/openapi-client/src/generated"

# ---------------------------------------------------------------------------
printf '\ncase: generator exits non-zero -- must fail, not continue\n'
T="$(make_tree go_fail)"
write_shim "$T" go fail
write_shim "$T" pnpm write
run_case "$T" "$T"
expect_status "go-fail" 1
expect_output "go-fail" "go/ogen generator failed"

# ---------------------------------------------------------------------------
printf '\ncase: spec missing -- must fail, naming the spec\n'
T="$(make_tree no_spec)"
write_shim "$T" go write
write_shim "$T" pnpm write
rm -f "$T/packages/openapi/openapi.yaml"
run_case "$T" "$T"
expect_status "no-spec" 1
expect_output "no-spec" "the OpenAPI source of truth is missing"

# ---------------------------------------------------------------------------
printf '\ncase: output tree missing -- must fail, naming the tree\n'
T="$(make_tree no_out)"
write_shim "$T" go write
write_shim "$T" pnpm write
rm -rf "$T/packages/openapi-client/src/generated"
run_case "$T" "$T"
expect_status "no-out" 1
expect_output "no-out" "the openapi-ts output tree is missing"

# ---------------------------------------------------------------------------
printf '\ncase: root does not exist -- must fail, not generate into nowhere\n'
run_case "$WORKROOT/happy" "$WORKROOT/definitely-not-here"
expect_status "bad-root" 1
expect_output "bad-root" "root is not a directory"

# ---------------------------------------------------------------------------
printf '\ncase: unknown option -- must fail, not be ignored\n'
T="$(make_tree bad_opt)"
write_shim "$T" go write
write_shim "$T" pnpm write
run_case "$T" --nope "$T"
expect_status "bad-opt" 2
expect_output "bad-opt" "unknown option"

# ---------------------------------------------------------------------------
# --check: the drift half. Needs a real git repo, so these cases use a real
# `git` from the stripped PATH.
git_init() {
	local root="$1"
	(
		cd "$root" || exit 1
		PATH="$BASE_PATH" git init -q .
		PATH="$BASE_PATH" git config user.email t@example.com
		PATH="$BASE_PATH" git config user.name t
		PATH="$BASE_PATH" git add apps packages
		PATH="$BASE_PATH" git -c commit.gpgsign=false commit -qm seed
	) >/dev/null 2>&1
}

# This is the over-fire control for --check. The shims rewrite both files with
# the same bytes on every run, so mtimes move but content does not -- exactly
# what an idempotent generator does against an in-sync tree. It must stay green,
# or the drift job reddens correct work and gets switched off.
printf '\ncase: --check, generated content identical to committed -- must pass\n'
T="$(make_tree check_clean)"
write_shim "$T" go write
write_shim "$T" pnpm write
run_case "$T" "$T" # generate once, so the tree holds generated content
git_init "$T"      # commit that content
run_case "$T" --check "$T"
expect_status "check-clean" 0
expect_output "check-clean" "committed generated trees match a fresh generation"

# The stale-tree commit itself: what is committed is not what the spec now
# generates.
printf '\ncase: --check, generated content differs from committed -- must fail\n'
T="$(make_tree check_drift)"
write_shim "$T" go write
write_shim "$T" pnpm write
run_case "$T" "$T"
git_init "$T"
write_shim "$T" go write-b
write_shim "$T" pnpm write-b
run_case "$T" --check "$T"
expect_status "check-drift" 1
expect_output "check-drift" "do not match a fresh"
expect_output "check-drift" "make gen"

# ---------------------------------------------------------------------------
# GH #511 follow-up: run_generator must not wait forever on a stalled
# generator, or on a child it spawned. The `go` shim here ignores SIGTERM and
# spawns a grandchild that also ignores SIGTERM, so reaching a failure here
# proves the deadline's SIGKILL escalation, not just its TERM happy path.
# GEN_OPENAPI_DEADLINE overrides the production default (120s) so this proves
# the mechanism in a few seconds instead of two minutes.
printf '\ncase: generator hangs -- must fail within the deadline, not hang\n'
T="$(make_tree hang)"
GRANDCHILD_PID_FILE="$WORKROOT/hang.grandchild.pid"
rm -f "$GRANDCHILD_PID_FILE"
write_shim "$T" go hang "$GRANDCHILD_PID_FILE"
write_shim "$T" pnpm write
START="$(date +%s)"
OUT="$(GEN_OPENAPI_DEADLINE=2 PATH="$T/bin:$BASE_PATH" bash "$UNDER_TEST" "$T" 2>&1)"
STATUS=$?
ELAPSED=$(($(date +%s) - START))
expect_status "hang" 1
expect_output "hang" "exceeded its 2s deadline"
expect_output "hang" "go/ogen"
# Grace period in run_generator's watchdog is 2s (TERM, wait, then KILL), so
# with a 2s deadline this must return in single-digit seconds, not the 9999s
# the shim would otherwise sleep for.
if [ "$ELAPSED" -le 20 ]; then
	ok "hang: returned in ${ELAPSED}s, not hung (2s deadline, ignored SIGTERM)"
else
	not_ok "hang: took ${ELAPSED}s to return, expected well under 20s" "output was:
$OUT"
fi

# Process-tree cleanup: the grandchild the hung shim spawned -- which also
# ignores SIGTERM, forcing the SIGKILL escalation -- must not survive. This is
# the concrete proof that the kill reaches the whole descendant tree, not just
# the direct go/pnpm invocation. Demonstrated portably with plain `kill -0`
# polling (works on both the BSD ps/kill this suite otherwise assumes and on
# Linux); it is not a process-group proof, because run_generator deliberately
# does not use process groups (see the comment in run_generator on why job
# control hung this exact script when piped, which is how this suite invokes
# it).
if [ ! -s "$GRANDCHILD_PID_FILE" ]; then
	not_ok "hang: grandchild pid file was never written" "the hang shim did not run as expected"
else
	GC_PID="$(cat "$GRANDCHILD_PID_FILE")"
	GONE=0
	for _ in 1 2 3 4 5 6 7 8; do
		if ! kill -0 "$GC_PID" 2>/dev/null; then
			GONE=1
			break
		fi
		sleep 1
	done
	if [ "$GONE" -eq 1 ]; then
		ok "hang: grandchild pid $GC_PID did not survive the deadline (descendant-tree kill)"
	else
		not_ok "hang: grandchild pid $GC_PID is still alive after the deadline" "descendant-tree kill did not reach it"
	fi
fi

# ---------------------------------------------------------------------------
# PR #512 review, finding 1: pid_tree/kill_tree rely on `ps -Ao pid=,ppid=`
# succeeding with output it can parse. If `ps` is broken, kill_tree still
# returns success after walking zero descendants -- the watchdog's kill would
# silently do nothing. run_generator must refuse to start a generator it
# cannot safely deadline-kill, not proceed and hope. Two failure shapes, since
# the reviewer named them separately: outright failure, and exit 0 with
# unusable output (a status-only check would wrongly accept the latter).
printf '\ncase: ps exits non-zero -- must refuse to start, not proceed\n'
T="$(make_tree ps_broken)"
write_shim "$T" go write
write_shim "$T" pnpm write
printf '#!/bin/sh\nprintf "ps: simulated failure\\n" >&2\nexit 1\n' >"$T/bin/ps"
chmod +x "$T/bin/ps"
run_case "$T" "$T"
expect_status "ps-broken" 1
expect_output "ps-broken" "'ps -Ao pid=,ppid=' failed"

printf '\ncase: ps exits 0 but returns no usable PID/PPID pairs -- must refuse to start\n'
T="$(make_tree ps_unusable)"
write_shim "$T" go write
write_shim "$T" pnpm write
printf '#!/bin/sh\nexit 0\n' >"$T/bin/ps"
chmod +x "$T/bin/ps"
run_case "$T" "$T"
expect_status "ps-unusable" 1
expect_output "ps-unusable" "no usable PID/PPID pairs"

# Over-fire control: a working `ps` (the real one, from BASE_PATH -- every
# other case above already runs with it) must not be blocked by this
# preflight. The `happy` case already proves this; restated here so the
# ps-broken/ps-unusable pair has an explicit positive counterpart beside it.
printf '\ncase: ps works normally -- preflight must not block a real run\n'
T="$(make_tree ps_ok)"
write_shim "$T" go write
write_shim "$T" pnpm write
run_case "$T" "$T"
expect_status "ps-ok" 0
expect_output "ps-ok" "OK: both trees regenerated"

# ---------------------------------------------------------------------------
# PR #512 review, finding 2: GEN_OPENAPI_DEADLINE is used unvalidated. A
# non-numeric value makes `sleep` error/return immediately, the watchdog then
# finds the generator still alive at once, and kills a healthy generator
# while reporting a deadline breach -- a false accusation indistinguishable
# from a real one. `0` has the same effect. Both must be refused, naming the
# offending value.
printf '\ncase: GEN_OPENAPI_DEADLINE is non-numeric -- must be refused, naming it\n'
T="$(make_tree deadline_nan)"
write_shim "$T" go write
write_shim "$T" pnpm write
OUT="$(GEN_OPENAPI_DEADLINE=soon PATH="$T/bin:$BASE_PATH" bash "$UNDER_TEST" "$T" 2>&1)"
STATUS=$?
expect_status "deadline-nan" 1
expect_output "deadline-nan" "GEN_OPENAPI_DEADLINE must be a positive integer"
expect_output "deadline-nan" "got: 'soon'"

printf '\ncase: GEN_OPENAPI_DEADLINE=0 -- must be refused, naming it\n'
T="$(make_tree deadline_zero)"
write_shim "$T" go write
write_shim "$T" pnpm write
OUT="$(GEN_OPENAPI_DEADLINE=0 PATH="$T/bin:$BASE_PATH" bash "$UNDER_TEST" "$T" 2>&1)"
STATUS=$?
expect_status "deadline-zero" 1
expect_output "deadline-zero" "GEN_OPENAPI_DEADLINE must be greater than 0"
expect_output "deadline-zero" "got: '0'"

# Over-fire control: a valid, positive-integer deadline must still run
# normally. The `hang` case above (GEN_OPENAPI_DEADLINE=2) already proves a
# valid override is accepted and enforced; this proves a valid override still
# lets a healthy run succeed rather than merely being parsed.
printf '\ncase: GEN_OPENAPI_DEADLINE is a valid positive integer -- must run normally\n'
T="$(make_tree deadline_valid)"
write_shim "$T" go write
write_shim "$T" pnpm write
OUT="$(GEN_OPENAPI_DEADLINE=30 PATH="$T/bin:$BASE_PATH" bash "$UNDER_TEST" "$T" 2>&1)"
STATUS=$?
expect_status "deadline-valid" 0
expect_output "deadline-valid" "OK: both trees regenerated"

# ---------------------------------------------------------------------------
# PR #512 review, finding 3: a back-dated MARKER (tried, then reverted) fixed
# the clock-tie flake but broke the thing this whole script exists to catch --
# on a real checkout, a pre-existing file already has a recent mtime from
# `git checkout`, not the test's year-2020 seed stamp, so it would look
# "newer than 2020" whether or not the generator wrote anything this run. The
# other no-op cases above (go-noop, ts-noop) do not catch this: their seed
# files are backdated to 2020 by make_tree, which happened to also satisfy
# the (wrong) back-dated-marker fix. This case seeds the file at "now",
# like a real checkout, so it is the one that would have gone wrongly green
# under that fix.
printf '\ncase: go writes nothing, pre-existing file has a fresh (checkout-like) mtime -- must still fail\n'
T="$(make_tree go_noop_fresh)"
write_shim "$T" go nothing
write_shim "$T" pnpm write
touch "$T/apps/api/internal/api/gen/oas_schemas_gen.go"
run_case "$T" "$T"
expect_status "go-noop-fresh" 1
expect_output "go-noop-fresh" "exited 0 but wrote nothing"

# ---------------------------------------------------------------------------
# PR #512 review, gen-openapi.sh:398: a marker/write timestamp TIE, deliberately
# constructed with `touch -r`, exactly as this suite's own comment on
# write_shim's write/write-b split already warns is fragile if left to real
# timing ("both runs landed in the same second"). The finding was that
# sleeping *after* the tie is detected changes neither timestamp, so it
# cannot resolve one -- the write and the marker both already exist. This
# case proves that directly: the OLD approach (sleep after an ambiguous
# result) leaves the tie unresolved; the NEW approach (this script's actual
# mechanism: wait for an observable clock advance BEFORE the write can
# happen) resolves it. It is a mechanism proof, not a run through
# run_generator: a real coarse-grained filesystem clock cannot be forced onto
# this machine to make run_generator hit a tie honestly, and the earlier
# version of this suite already learned that lesson the hard way (the
# write/write-b split above exists because a *different* timing assumption
# failed silently in the same spot).
printf '\ncase: a deliberately tied marker/write -- sleep-after does not resolve it, wait-before does\n'
TIE_DIR="$WORKROOT/tie_mechanism"
mkdir -p "$TIE_DIR"
TIE_MARKER="$TIE_DIR/marker"
touch "$TIE_MARKER"
TIE_WRITE="$TIE_DIR/write"
touch -r "$TIE_MARKER" "$TIE_WRITE" # force an exact tie with the marker

if [ -n "$(find "$TIE_WRITE" -newer "$TIE_MARKER" -print -quit 2>/dev/null)" ]; then
	not_ok "tie-setup" "touch -r did not produce a tie; this case cannot test anything"
else
	sleep 1
	if [ -n "$(find "$TIE_WRITE" -newer "$TIE_MARKER" -print -quit 2>/dev/null)" ]; then
		not_ok "tie-sleep-after" "sleep-after resolved the tie; expected it not to (this was the bug)"
	else
		ok "tie-sleep-after: sleep after the tie leaves it unresolved, as gen-openapi.sh:398 found"
	fi

	TIE_MARKER2="$TIE_DIR/marker2"
	touch "$TIE_MARKER2"
	TIE_PROBE="$TIE_DIR/probe"
	TIE_ADVANCED=0
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		touch "$TIE_PROBE"
		if [ -n "$(find "$TIE_PROBE" -newer "$TIE_MARKER2" -print -quit 2>/dev/null)" ]; then
			TIE_ADVANCED=1
			break
		fi
	done
	if [ "$TIE_ADVANCED" -ne 1 ]; then
		not_ok "tie-wait-before" "clock never observably advanced in 10 attempts on this machine"
	else
		TIE_WRITE_AFTER="$TIE_DIR/write_after"
		touch "$TIE_WRITE_AFTER" # the "generator" writes only after the wait
		if [ -n "$(find "$TIE_WRITE_AFTER" -newer "$TIE_MARKER2" -print -quit 2>/dev/null)" ]; then
			ok "tie-wait-before: waiting for an observable advance before the write resolves the tie"
		else
			not_ok "tie-wait-before" "write after the wait was still not detected as newer"
		fi
	fi
fi

# ---------------------------------------------------------------------------
printf '\n'
printf 'gen-openapi_test: %d passed, %d failed\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
	exit 1
fi
exit 0
