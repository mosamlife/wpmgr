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
# (mktemp/find/rm for the marker, cat for the drift message, git for --check,
# ps/sleep/kill for the run_generator deadline watchdog -- kill is a bash
# builtin so it needs no PATH entry, but ps and sleep are external).
for tool in bash mktemp find rm cat git ps sleep; do
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

# Install a shim. $1=tree root, $2=name (go|pnpm|git), $3=behaviour:
#   write    -- exit 0 after writing "generation A" into the output tree
#   write-b  -- exit 0 after writing "generation B", i.e. different bytes
#   nothing  -- exit 0 and touch nothing
#   fail     -- exit 3 with a message
#
# `write` and `write-b` differ by content, not by a timestamp. An earlier
# version of this file wrote `$(date)` and expected two runs to differ; both
# runs landed in the same second, the bytes matched, and the drift case passed
# by accident in the wrong direction.
write_shim() {
	local root="$1" name="$2" behaviour="$3" target="" shim="$1/bin/$2"
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
expect_output "happy" "regenerated $T/apps/api/internal/api/gen"
expect_output "happy" "regenerated $T/packages/openapi-client/src/generated"
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
expect_output "go-noop" "$T/apps/api/internal/api/gen"

printf '\ncase: ts generator exits 0 and writes nothing -- must fail\n'
T="$(make_tree ts_noop)"
write_shim "$T" go write
write_shim "$T" pnpm nothing
run_case "$T" "$T"
expect_status "ts-noop" 1
expect_output "ts-noop" "exited 0 but wrote nothing"
expect_output "ts-noop" "$T/packages/openapi-client/src/generated"

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
printf '\n'
printf 'gen-openapi_test: %d passed, %d failed\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
	exit 1
fi
exit 0
