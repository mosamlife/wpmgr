#!/usr/bin/env bash
# scripts/check-go-toolchain_test.sh
#
# Builds throwaway trees and asserts check-go-toolchain.sh's exit code against
# each. Half of these cases are FAILURES it must catch; the other half are
# correct trees it must NOT block. Both halves matter equally: a guard that
# reddens correct work gets switched off, and then it guards nothing.
#
#   scripts/check-go-toolchain_test.sh      (or: make check-go-toolchain-test)

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="$HERE/check-go-toolchain.sh"
TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

pass=0
fail=0

# make_tree <dir> <ci-version> <docker-tag> <gomod-go>
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

# --- MUST PASS: correct trees it must not block ----------------------------

# The state this branch puts the repo in.
make_tree "$TMPROOT/clean" "1.26.6" "1.26.6" "1.26.3"
expect 0 "all declarations agree on an exact patch" "$TMPROOT/clean"

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

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]] || exit 1
exit 0
