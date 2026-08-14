#!/usr/bin/env bash
# scripts/check-go-toolchain_test.sh
#
# Builds throwaway trees and asserts check-go-toolchain.sh's exit code against
# each. Half of these cases are FAILURES it must catch; the other half are
# correct trees it must NOT block. Both halves matter equally: a guard that
# reddens correct work gets switched off, and then it guards nothing.
#
#   scripts/check-go-toolchain_test.sh      (or: make check-go-toolchain-test)
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_GO_TOOLCHAIN_SCRIPT=/tmp/guard-with-hole.sh scripts/check-go-toolchain_test.sh
#
# PORTABILITY. Same standard as scripts/check-version-surfaces_test.sh: bash 3.2
# (what macOS ships) and POSIX tools, so it runs the same on a darwin laptop
# with BSD grep/sed/awk and on the ubuntu CI runner with the GNU ones. No
# mapfile, no associative arrays, no sed -i, no grep -P, no sort -V, no \b or \s
# in any pattern, and mktemp gets an explicit template rather than relying on
# the bare -d form.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_GO_TOOLCHAIN_SCRIPT:-$HERE/check-go-toolchain.sh}"

if [ ! -f "$GUARD" ]; then
  echo "no guard script at $GUARD" >&2
  exit 2
fi

TMPROOT="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-go-toolchain.XXXXXX")" || exit 2
trap 'rm -rf "$TMPROOT"' EXIT

pass=0
fail=0

# make_tree <dir> <ci-version> <docker-tag> <gomod-go>
# Any argument given as "-" omits that file entirely.
make_tree() {
  local d="$1" civ="$2" dtag="$3" gomodv="$4"
  mkdir -p "$d/.github/workflows" "$d/infra" "$d/apps/api"
  if [[ "$civ" != "-" ]]; then
    {
      printf 'jobs:\n  build:\n    steps:\n'
      printf '      - uses: actions/setup-go@v5\n'
      printf '        with:\n'
      printf '          go-version: "%s"\n' "$civ"
    } > "$d/.github/workflows/ci.yml"
  fi
  if [[ "$dtag" != "-" ]]; then
    printf 'FROM golang:%s AS build\n' "$dtag" > "$d/infra/Dockerfile.api"
  fi
  if [[ "$gomodv" != "-" ]]; then
    printf 'module example.com/x\n\ngo %s\n' "$gomodv" > "$d/apps/api/go.mod"
  fi
}

# expect <exit-code> <name> <dir>
expect() {
  local want="$1" name="$2" dir="$3"
  local out got
  out="$("$GUARD" "$dir" 2>&1)"
  got=$?
  if [[ "$got" -eq "$want" ]]; then
    printf 'ok    %s (exit %d)\n' "$name" "$got"
    pass=$((pass + 1))
  else
    printf 'FAIL  %s: wanted exit %d, got %d\n' "$name" "$want" "$got"
    printf '%s\n' "$out" | sed 's/^/        /'
    fail=$((fail + 1))
  fi
}

# expect_says <exit-code> <name> <dir> <must-contain> [<must-not-contain>]
#
# The exit code alone is too coarse for the extraction and per-category cases:
# a guard that never looked at the line, and a guard that read it correctly,
# both exit 0. These assert on what the guard PRINTED, so the case pins the
# value it extracted and not merely the fact that it survived.
expect_says() {
  local want="$1" name="$2" dir="$3" needle="$4" absent="${5:-}"
  local out got why=""
  out="$("$GUARD" "$dir" 2>&1)"
  got=$?
  [[ "$got" -eq "$want" ]] || why="wanted exit $want, got $got"
  if [[ -z "$why" ]] && ! printf '%s\n' "$out" | grep -qF -- "$needle"; then
    why="output does not contain: $needle"
  fi
  if [[ -z "$why" && -n "$absent" ]] && printf '%s\n' "$out" | grep -qF -- "$absent"; then
    why="output must not contain: $absent"
  fi
  if [[ -z "$why" ]]; then
    printf 'ok    %s (exit %d)\n' "$name" "$got"
    pass=$((pass + 1))
  else
    printf 'FAIL  %s: %s\n' "$name" "$why"
    printf '%s\n' "$out" | sed 's/^/        /'
    fail=$((fail + 1))
  fi
}

# --- MUST FAIL: the defects this guard exists to catch ---------------------

# The exact production defect of 2026-08-14: CI pinned, image floating.
make_tree "$TMPROOT/floating" "1.26.6" "1.26" "1.26.3"
expect 1 "floating base image golang:1.26 is caught" "$TMPROOT/floating"

# CI and image both pinned, but to different patches.
make_tree "$TMPROOT/drift" "1.26.6" "1.26.5" "1.26.3"
expect 1 "CI 1.26.6 vs image 1.26.5 disagreement is caught" "$TMPROOT/drift"

# A guard that finds nothing must go red, not green.
mkdir -p "$TMPROOT/empty"
expect 1 "empty tree (zero declarations) is a failure, not a pass" "$TMPROOT/empty"

# go.mod demanding a language version above the compiler we pin.
make_tree "$TMPROOT/floor" "1.26.6" "1.26.6" "1.27.0"
expect 1 "go.mod floor above the pinned toolchain is caught" "$TMPROOT/floor"

# `latest` is floating too.
make_tree "$TMPROOT/latest" "1.26.6" "latest" "1.26.3"
expect 1 "golang:latest is caught" "$TMPROOT/latest"

# A nonexistent root is an error, not a silent pass.
expect 1 "nonexistent root fails loudly" "$TMPROOT/does-not-exist"

# --- REGRESSION: one empty CATEGORY must be red, however healthy the rest --
#
# The guard shipped counting declarations globally, so three surviving
# `go-version:` pins under .github/ masked the complete absence of the container
# half: swapping the Go base image for `alpine` in all three container
# declarations printed `check-go-toolchain: OK`. Nothing in that tree is built
# by a pinned toolchain, and the guard called it success.

make_tree "$TMPROOT/nocontainer" "1.26.6" "-" "1.26.3"
printf 'FROM alpine AS build\n' > "$TMPROOT/nocontainer/infra/Dockerfile.api"
printf 'FROM alpine AS build\n' > "$TMPROOT/nocontainer/infra/Dockerfile.media-encoder"
printf 'services:\n  dev:\n    image: alpine\n' > "$TMPROOT/nocontainer/infra/docker-compose.dev.yml"
expect_says 1 "every container base image replaced by alpine is caught" \
  "$TMPROOT/nocontainer" "found no 'container' Go toolchain declarations"

# The mirror image: the container half intact, every workflow pin gone.
make_tree "$TMPROOT/noworkflow" "-" "1.26.6" "1.26.3"
{
  printf 'jobs:\n  build:\n    steps:\n'
  printf '      - uses: actions/checkout@v4\n'
  printf '      - run: make build\n'
} > "$TMPROOT/noworkflow/.github/workflows/ci.yml"
expect_says 1 "every workflow go-version pin gone is caught" \
  "$TMPROOT/noworkflow" "found no 'workflow' Go toolchain declarations"

# --- REGRESSION: the extraction must read the declaration, not the comment -
#
# The extraction led with a greedy `.*go-version:` / `.*golang:`, which anchors
# on the LAST occurrence in the line. A trailing comment that names the version
# the line moved off therefore WON, and the guard reported agreement on a
# version present in no declaration in the tree — which can hide a real drift
# just as easily as invent one. Asserting exit 0 alone would not have caught
# this (the greedy guard also exited 0, having agreed on the wrong value), so
# both cases pin the extracted value that gets printed.

make_tree "$TMPROOT/wftrailing" "1.26.6" "1.26.6" "1.26.3"
{
  printf 'jobs:\n  build:\n    steps:\n'
  printf '      - uses: actions/setup-go@v5\n'
  printf '        with:\n'
  printf '          go-version: "1.26.6"   # was go-version: "1.26.5" before the bump\n'
} > "$TMPROOT/wftrailing/.github/workflows/ci.yml"
expect_says 0 "a trailing comment naming go-version 1.26.5 does not override the 1.26.6 pin" \
  "$TMPROOT/wftrailing" "(setup-go go-version: 1.26.6)" "1.26.5"

make_tree "$TMPROOT/dktrailing" "1.26.6" "-" "1.26.3"
printf 'FROM golang:1.26.6 AS build   # was golang:1.26.5\n' \
  > "$TMPROOT/dktrailing/infra/Dockerfile.api"
expect_says 0 "a trailing comment naming golang:1.26.5 does not override the 1.26.6 base image" \
  "$TMPROOT/dktrailing" "(golang:1.26.6)" "1.26.5"

# A line whose ONLY mention of a Go base image is in its trailing comment
# declares nothing. Promoting it would invent a phantom 1.26.5 declaration and
# redden this correct tree as a disagreement.
make_tree "$TMPROOT/phantom" "1.26.6" "1.26.6" "1.26.3"
printf 'FROM alpine AS runtime   # was golang:1.26.5 until the static build landed\n' \
  > "$TMPROOT/phantom/infra/Dockerfile.runtime"
expect_says 0 "a golang: mention in a trailing comment is not a declaration" \
  "$TMPROOT/phantom" "check-go-toolchain: OK" "1.26.5"

# --- REGRESSION: version comparison is numeric, not lexical ----------------
#
# Sorted as text, 1.26.10 sorts BELOW 1.26.9. These two cases fail in opposite
# directions under a lexical compare: the first passes when it must not, the
# second reddens correct work.

make_tree "$TMPROOT/tenfloor" "1.26.9" "1.26.9" "1.26.10"
expect 1 "go.mod floor 1.26.10 above toolchain 1.26.9 is caught (not lexically 'lower')" \
  "$TMPROOT/tenfloor"

make_tree "$TMPROOT/tentool" "1.26.10" "1.26.10" "1.26.9"
expect 0 "go.mod floor 1.26.9 below toolchain 1.26.10 is fine (not lexically 'higher')" \
  "$TMPROOT/tentool"

# A two-component floor is legal in go.mod; missing components read as 0.
make_tree "$TMPROOT/shortfloor" "1.26.6" "1.26.6" "1.26"
expect 0 "a two-component go.mod floor (go 1.26) is accepted as 1.26.0" "$TMPROOT/shortfloor"

# A floor the comparison cannot evaluate must be refused, not guessed at: the
# numeric tests would error on it and produce an answer nobody checked.
make_tree "$TMPROOT/badfloor" "1.26.6" "1.26.6" "-"
printf 'module example.com/x\n\ngo 1.26rc1\n' > "$TMPROOT/badfloor/apps/api/go.mod"
expect 1 "a non-numeric go.mod floor is refused, not silently compared" "$TMPROOT/badfloor"

# --- MUST PASS: correct trees it must not block ----------------------------

# The state this branch puts the repo in.
make_tree "$TMPROOT/clean" "1.26.6" "1.26.6" "1.26.3"
expect 0 "all declarations agree on an exact patch" "$TMPROOT/clean"

# Both categories are populated and both counts are printed. This is the
# positive half of the per-category emptiness check: it must report what it
# found, per category, rather than only a global total.
expect_says 0 "the summary prints a per-category count for workflow" \
  "$TMPROOT/clean" "workflow: 1"
expect_says 0 "the summary prints a per-category count for container" \
  "$TMPROOT/clean" "container: 1"

# Debian variant suffixes are a base-image choice, not a version disagreement.
make_tree "$TMPROOT/variant" "1.26.6" "1.26.6-trixie" "1.26.3"
expect 0 "golang:1.26.6-trixie is exact, not floating" "$TMPROOT/variant"

# The go.mod floor is a language minimum and is NOT required to equal the
# toolchain. Requiring equality would redden the repo on every patch bump.
make_tree "$TMPROOT/floorlow" "1.26.6" "1.26.6" "1.25.0"
expect 0 "go.mod floor well below the toolchain is fine" "$TMPROOT/floorlow"

# A comment naming a base image is prose, not a declaration. This guard's own
# first run went red here, on infra/Dockerfile.media-encoder's trixie comment.
make_tree "$TMPROOT/comment" "1.26.6" "1.26.6" "1.26.3"
printf '# CRITICAL: stay on trixie to match golang:1.26 (also trixie).\n' \
  >> "$TMPROOT/comment/infra/Dockerfile.api"
expect 0 "a comment mentioning golang:1.26 does not trip the guard" "$TMPROOT/comment"

# A workflow comment naming an old pin is prose too. ci.yml carries exactly
# such a note about the 1.26.5 it moved off, so this is the live case.
make_tree "$TMPROOT/wfcomment" "1.26.6" "1.26.6" "1.26.3"
printf '      # this job pinned go-version: "1.26.5" until the 1.26.6 bump\n' \
  >> "$TMPROOT/wfcomment/.github/workflows/ci.yml"
expect 0 "a workflow comment naming an old go-version does not trip the guard" "$TMPROOT/wfcomment"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]] || exit 1
exit 0
