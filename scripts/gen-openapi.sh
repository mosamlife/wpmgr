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
cleanup() { [ -n "$MARKER" ] && rm -f "$MARKER"; return 0; }
trap cleanup EXIT

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

	MARKER="$(mktemp "${TMPDIR:-/tmp}/gen-openapi-marker.XXXXXX")"

	say "$label: running (in $workdir): $*"
	if ! (cd "$workdir" && "$@"); then
		die "$label generator failed (see its output above).
    Working directory: $workdir
    Command: $*"
	fi

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
