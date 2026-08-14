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
#
# PORTABILITY. Written for bash 3.2 (what macOS ships) and POSIX tools, so it
# runs the same on a darwin laptop with BSD grep/sed/awk and on the ubuntu CI
# runner with the GNU ones — the same standard scripts/check-version-surfaces.sh
# and its test file are written to. No mapfile, no associative arrays, no
# sed -i, no grep -P, no sort -V, no \b or \s in any pattern. To be accurate
# about why: /usr/bin/sort on this laptop is 2.3-Apple (199) and does implement
# -V, so `sort -V` is not observed to fail here. It is out because the sibling
# guard states the standard and this one is its neighbour, because leaner
# environments than a developer laptop are not guaranteed to carry it, and
# because comparing the numeric components directly (ver_le below) is three
# lines and removes the lexical-versus-numeric trap outright.

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

# ---------------------------------------------------------------------------
# THE CATEGORY REGISTRY.
#
# A toolchain declaration belongs to exactly one category, and the registry is
# the single place a category is named. It drives three things at once:
#
#   1. add_decl REFUSES a category that is not registered here;
#   2. the emptiness check loops over it, so every registered category is
#      required to be non-empty;
#   3. the summary prints one count per registered category.
#
# That is what makes the emptiness check structural rather than a one-off. A
# fourth scan (say, a `toolchain` directive, or a devcontainer image) cannot be
# added without registering its category — add_decl rejects it loudly if you
# try — and registering it is the same act that gives it an emptiness check and
# a printed count. There is no way to add a category and forget the check.
#
# The defect that made this necessary: the emptiness check used to count
# declarations GLOBALLY. Replacing the Go base image with `alpine` in all three
# container declarations (infra/Dockerfile.api, infra/Dockerfile.media-encoder,
# infra/docker-compose.dev.yml) left three surviving `go-version:` pins under
# .github/, the global count stayed above zero, and the guard printed
# `check-go-toolchain: OK` for a tree in which NOTHING is built by a pinned Go
# toolchain at all. A check that finds nothing and calls it success is the exact
# shape this guard exists to prevent, so it does not get to have it either.
# ---------------------------------------------------------------------------
CATEGORY_KEYS='workflow container'

category_desc() {
  case "$1" in
    workflow)  echo ".github/workflows — actions/setup-go 'go-version:'" ;;
    container) echo "infra — container base image 'golang:<tag>'" ;;
    *)         return 1 ;;
  esac
}

# Every declaration line the scans considered real code (not a comment), as
# "category" per line. Emptiness is judged on this, not on the valid ones, so a
# category holding only a floating tag reports the floating tag rather than the
# misleading "no declarations found".
seen=""
note_seen() {
  if ! category_desc "$1" >/dev/null 2>&1; then
    echo "FAIL: internal: unregistered category '$1' — add it to CATEGORY_KEYS" >&2
    exit 2
  fi
  seen="$seen$1"$'\n'
}
count_seen() { printf '%s' "$seen" | grep -c "^$1\$" || true; }

# Collected as "category<TAB>version<TAB>where" so every reported version keeps
# both its source and the category it is counted against.
decls=""
add_decl() {
  if ! category_desc "$1" >/dev/null 2>&1; then
    echo "FAIL: internal: unregistered category '$1' — add it to CATEGORY_KEYS" >&2
    exit 2
  fi
  decls="$decls$1"$'\t'"$2"$'\t'"$3"$'\n'
}

# An exact toolchain version: three numeric components, nothing else. Docker
# variant suffixes (-alpine, -trixie) are stripped before this is applied, so
# golang:1.26.6-trixie is exact and golang:1.26 is not.
EXACT_RE='^[0-9]+\.[0-9]+\.[0-9]+$'
# What ver_le is willing to compare. A go.mod floor of `go 1.26` is legal, so
# one to three components; anything else is refused rather than guessed at.
NUMERIC_RE='^[0-9]+(\.[0-9]+){0,2}$'

# ver_le A B — true when A <= B, comparing the numeric components one at a time.
# Missing components read as 0, so 1.26 == 1.26.0. This replaces `sort -V` (see
# PORTABILITY above) and, incidentally, is the only version comparison here that
# cannot fall into the lexical trap: sorted as text, 1.26.10 sorts BELOW 1.26.9.
ver_le() {
  local av="$1" bv="$2"
  local a1 a2 a3 b1 b2 b3
  local IFS=.
  set -- $av
  a1="${1:-0}"; a2="${2:-0}"; a3="${3:-0}"
  set -- $bv
  b1="${1:-0}"; b2="${2:-0}"; b3="${3:-0}"
  if [[ "$a1" -ne "$b1" ]]; then [[ "$a1" -lt "$b1" ]]; return $?; fi
  if [[ "$a2" -ne "$b2" ]]; then [[ "$a2" -lt "$b2" ]]; return $?; fi
  [[ "$a3" -le "$b3" ]]
}

# strip_comment TEXT — everything before the first '#'. Run before any value is
# extracted, because a declaration's own trailing comment may legally name the
# version it moved off, and a line whose ONLY mention of the key is inside that
# comment declares nothing at all.
strip_comment() { printf '%s' "${1%%#*}"; }

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
    code="$(strip_comment "$text")"
    # After the comment is gone the key may be gone with it: that line mentioned
    # a pin, it did not set one.
    [[ "$code" == *go-version:* ]] || continue
    where="${file#"$ROOT"/}:$lineno"
    note_seen workflow
    # NON-GREEDY on purpose. `${code#*go-version:}` removes the SHORTEST
    # matching prefix, so it anchors on the FIRST `go-version:` on the line.
    # The regex this replaced led with a greedy `.*go-version:`, which anchors
    # on the LAST one: given
    #     go-version: "1.26.6"   # was go-version: "1.26.5" before the bump
    # it extracted 1.26.5 — the comment's value — and then reported OK for a
    # version that appears in no declaration in the tree, which can equally
    # well HIDE a real drift as invent one.
    after="${code#*go-version:}"
    # go-version: "1.26.6"  |  go-version: 1.26.6  |  go-version: '1.26.6'
    ver="$(printf '%s' "$after" | sed -E "s/^[[:space:]]*//; s/^[\"']//; s/[\"'].*$//; s/[[:space:]].*$//")"
    if [[ ! "$ver" =~ $EXACT_RE ]]; then
      fail "$where declares go-version '$ver', which is not an exact X.Y.Z toolchain"
      continue
    fi
    add_decl workflow "$ver" "$where (setup-go go-version: $ver)"
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
  code="$(strip_comment "$text")"
  # `FROM alpine  # was golang:1.26.5` is not a Go base image. Without this the
  # comment would be promoted into a phantom declaration.
  [[ "$code" == *golang:* ]] || continue
  where="${file#"$ROOT"/}:$lineno"
  note_seen container
  # Non-greedy for the same reason as the workflow scan, then anchored at ^ so
  # only the tag itself is taken.
  after="${code#*golang:}"
  tag="$(printf '%s' "$after" | sed -E 's/^([A-Za-z0-9._-]+).*/\1/')"
  # Strip a Debian/Alpine variant suffix; keep the version part.
  ver="$(printf '%s' "$tag" | sed -E 's/-(alpine|bookworm|trixie|bullseye)[0-9.]*$//')"
  if [[ ! "$ver" =~ $EXACT_RE ]]; then
    fail "$where uses base image golang:$tag — a floating tag. Pin the exact patch (golang:X.Y.Z)."
    continue
  fi
  add_decl container "$ver" "$where (golang:$tag)"
done < <(grep -rn "golang:" "$ROOT/infra" 2>/dev/null)

# ---------------------------------------------------------------------------
# 3. Report. Finding nothing is a failure, not a pass — and finding nothing in
#    ANY ONE category is a failure, however healthy the others look.
# ---------------------------------------------------------------------------
decl_count=$(printf '%s' "$decls" | grep -c . || true)

printf '%s' "$decls" | while IFS=$'\t' read -r c v w; do
  [[ -z "$v" ]] && continue
  info "go$v  $w"
done

info "declarations by category:"
for key in $CATEGORY_KEYS; do
  info "  $key: $(count_seen "$key")  ($(category_desc "$key"))"
done
echo "  ($decl_count declarations found)"

empty_categories=0
for key in $CATEGORY_KEYS; do
  if [[ "$(count_seen "$key")" -eq 0 ]]; then
    empty_categories=$((empty_categories + 1))
    fail "found no '$key' Go toolchain declarations under $ROOT ($(category_desc "$key"))."
    echo "      Zero in a category means that scan is broken or those paths moved," >&2
    echo "      never that the repo is consistent. The other categories cannot" >&2
    echo "      vouch for this one: with every container base image replaced by a" >&2
    echo "      non-Go image, the workflow pins alone would still count above zero" >&2
    echo "      while nothing in the tree is built by a pinned toolchain." >&2
  fi
done

if [[ "$decl_count" -eq 0 ]]; then
  echo "check-go-toolchain: $errors problem(s)" >&2
  exit 1
fi

distinct="$(printf '%s' "$decls" | cut -f2 | sort -u | grep -c . || true)"
if [[ "$distinct" -gt 1 ]]; then
  fail "Go toolchain declarations disagree. Distinct versions: $(printf '%s' "$decls" | cut -f2 | sort -u | tr '\n' ' ')"
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
  toolchain="$(printf '%s' "$decls" | cut -f2 | sort -u | head -1)"
  if [[ -z "$floor" ]]; then
    fail "apps/api/go.mod has no 'go' directive"
  elif [[ ! "$floor" =~ $NUMERIC_RE ]]; then
    # Refused rather than guessed at: ver_le compares with numeric tests, and a
    # non-numeric component would make those tests error and the comparison
    # silently produce an answer nobody checked.
    fail "apps/api/go.mod declares 'go $floor', which is not a numeric version this guard can compare"
  else
    if ! ver_le "$floor" "$toolchain"; then
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
