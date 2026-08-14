#!/usr/bin/env bash
# scripts/check-go-toolchain.sh
#
# Every place in this repo that decides WHICH GO TOOLCHAIN COMPILES A BINARY,
# checked against every other place.
#
# WHY THIS EXISTS. On 2026-08-14 the Security audit job went red on main with
# zero code change: govulncheck reads a live advisory database, and go1.26.5
# picked up 7 stdlib advisories overnight. Fixing it surfaced a second, quieter
# defect: CI pinned `go-version: "1.26.5"` while infra/Dockerfile.api built
# `FROM golang:1.26`, a floating tag. So CI tested one toolchain and the shipped
# image was compiled by whatever that tag resolved to at build time. Those can
# differ in either direction and NOTHING DETECTED IT. The only way anyone
# learned which toolchain production actually ran was pulling the published
# image and reading the binary:
#
#   docker cp <container>:/usr/local/bin/wpmgr ./wpmgr && go version -m ./wpmgr
#
# That is not a check, that is an autopsy. This script is the check.
#
# WHY PINNED AND NOT FLOATING. A floating `golang:1.26` gets patch fixes for
# free, which is the honest argument for it. It also means rebuilding the same
# git sha twice can produce two different binaries, so "which build is in
# production" stops being answerable from the repo — and answering that question
# is a first-class concern here (the version chip exists for it). We pin, and we
# accept the cost of a bump per patch release, because the mechanism that
# NOTICES a patch release already exists and is proven: the govulncheck step in
# ci.yml went red and named `Fixed in: go1.26.6` unprompted. Pinning plus a live
# advisory feed gets deliberate upgrades AND automatic notification. Floating
# gets automatic upgrades and no record.
#
# Note what that buys and what it does not: govulncheck fires on SECURITY patch
# releases, not on every patch release. A non-security patch can sit unadopted.
# That is the accepted cost, and it is the cheap direction to be wrong in.
#
# RUN IT (no CI, no install, nothing but a shell):
#   make check-go-toolchain          or  scripts/check-go-toolchain.sh
#   make check-go-toolchain-test     or  scripts/check-go-toolchain_test.sh
#   scripts/check-go-toolchain.sh /path/to/some/other/tree
#
# Exit 0 when every declaration agrees. Exit 1 on drift, on a floating tag, and
# on FINDING NOTHING — a toolchain guard that scans a tree with no declarations
# in it has not passed, it has failed to look.

set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/check-go-toolchain.sh [ROOT]

  ROOT  repository root to check (default: the repo this script lives in)
USAGE
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
esac

ROOT="${1:-}"
if [[ -z "$ROOT" ]]; then
  ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi

if [[ ! -d "$ROOT" ]]; then
  echo "FAIL: root is not a directory: $ROOT" >&2
  exit 1
fi

errors=0
fail() { echo "FAIL: $*" >&2; errors=$((errors + 1)); }
info() { echo "  $*"; }

# Collected as "version<TAB>where" so every reported version keeps its source.
decls=""
add_decl() { decls+="$1"$'\t'"$2"$'\n'; }

# An exact toolchain version: three numeric components, nothing else. Docker
# variant suffixes (-alpine, -trixie) are stripped before this is applied, so
# golang:1.26.6-trixie is exact and golang:1.26 is not.
EXACT_RE='^[0-9]+\.[0-9]+\.[0-9]+$'

echo "Go toolchain declarations under $ROOT"

# ---------------------------------------------------------------------------
# 1. GitHub Actions: actions/setup-go `go-version:`
# ---------------------------------------------------------------------------
wf_dir="$ROOT/.github/workflows"
if [[ -d "$wf_dir" ]]; then
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    file="${line%%:*}"
    rest="${line#*:}"
    lineno="${rest%%:*}"
    text="${rest#*:}"
    # Comments discuss pins as well as set them — ci.yml's own history note
    # names the version it moved off. Same trap as the Dockerfile scan below:
    # test the `#` on the TEXT, not on grep -n's file:line: prefix.
    [[ "$text" =~ ^[[:space:]]*# ]] && continue
    # go-version: "1.26.6"  |  go-version: 1.26.6  |  go-version: '1.26.6'
    ver="$(printf '%s' "$text" | sed -E "s/.*go-version:[[:space:]]*[\"']?([^\"'[:space:]]+)[\"']?.*/\1/")"
    where="${file#"$ROOT"/}:$lineno"
    if [[ ! "$ver" =~ $EXACT_RE ]]; then
      fail "$where declares go-version '$ver', which is not an exact X.Y.Z toolchain"
      continue
    fi
    add_decl "$ver" "$where (setup-go)"
  done < <(grep -rn "go-version:" "$wf_dir" 2>/dev/null)
fi

# ---------------------------------------------------------------------------
# 2. Container base images: golang:<tag> in Dockerfiles and compose files
# ---------------------------------------------------------------------------
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  file="${line%%:*}"
  rest="${line#*:}"
  lineno="${rest%%:*}"
  text="${rest#*:}"
  # Comments mention base images too ("stay on trixie to match golang:1.26.6").
  # The `#` has to be tested on the TEXT, not on the grep -n output, which is
  # prefixed with file:line: and so never starts with `#`. Getting this wrong
  # made this guard's own first run red on a comment.
  [[ "$text" =~ ^[[:space:]]*# ]] && continue
  tag="$(printf '%s' "$text" | sed -E 's/.*golang:([A-Za-z0-9._-]+).*/\1/')"
  where="${file#"$ROOT"/}:$lineno"
  # Strip a Debian/Alpine variant suffix; keep the version part.
  ver="$(printf '%s' "$tag" | sed -E 's/-(alpine|bookworm|trixie|bullseye)[0-9.]*$//')"
  if [[ ! "$ver" =~ $EXACT_RE ]]; then
    fail "$where uses base image golang:$tag — a floating tag. Pin the exact patch (golang:X.Y.Z)."
    continue
  fi
  add_decl "$ver" "$where (golang:$tag)"
done < <(grep -rn "golang:" "$ROOT/infra" 2>/dev/null | grep -v '^\s*#')

# ---------------------------------------------------------------------------
# 3. Report. Finding nothing is a failure, not a pass.
# ---------------------------------------------------------------------------
decl_count=$(printf '%s' "$decls" | grep -c . || true)

if [[ "$decl_count" -eq 0 ]]; then
  echo "FAIL: found no Go toolchain declarations at all under $ROOT." >&2
  echo "      This guard scans .github/workflows (go-version:) and infra (golang:)." >&2
  echo "      Zero declarations means the scan is broken or the paths moved," >&2
  echo "      never that the repo is consistent." >&2
  exit 1
fi

printf '%s' "$decls" | while IFS=$'\t' read -r v w; do
  [[ -z "$v" ]] && continue
  info "go$v  $w"
done

echo "  ($decl_count declarations found)"

distinct="$(printf '%s' "$decls" | cut -f1 | sort -u | grep -c . || true)"
if [[ "$distinct" -gt 1 ]]; then
  fail "Go toolchain declarations disagree. Distinct versions: $(printf '%s' "$decls" | cut -f1 | sort -u | tr '\n' ' ')"
  echo "      CI would test one toolchain while the image ships another." >&2
fi

# ---------------------------------------------------------------------------
# 4. The go.mod LANGUAGE floor must not exceed the toolchain that builds it.
#    These are deliberately NOT required to be equal: `go 1.26.3` in go.mod is
#    the minimum language version a consumer needs, not the compiler we ship
#    with. Raising it in lockstep with every patch release would push a floor
#    onto everyone building from source for no security gain — the binaries this
#    repo ships are governed by the pins above, not by the go directive.
# ---------------------------------------------------------------------------
gomod="$ROOT/apps/api/go.mod"
if [[ -f "$gomod" ]]; then
  floor="$(grep -E '^go [0-9]' "$gomod" | head -1 | awk '{print $2}')"
  if [[ -z "$floor" ]]; then
    fail "apps/api/go.mod has no 'go' directive"
  else
    toolchain="$(printf '%s' "$decls" | cut -f1 | sort -u | head -1)"
    lowest="$(printf '%s\n%s\n' "$floor" "$toolchain" | sort -V | head -1)"
    if [[ "$lowest" != "$floor" ]]; then
      fail "apps/api/go.mod requires go $floor but the pinned toolchain is $toolchain (floor is above the compiler)"
    else
      info "go.mod language floor go$floor <= toolchain go$toolchain (ok; these are not required to be equal)"
    fi
  fi
fi

if [[ "$errors" -gt 0 ]]; then
  echo "check-go-toolchain: $errors problem(s)" >&2
  exit 1
fi

echo "check-go-toolchain: OK"
exit 0
