#!/usr/bin/env bash
# scripts/check-version-surfaces_test.sh
#
# The regression suite for scripts/check-version-surfaces.sh.
#
# WHY THIS EXISTS. The guard used to be 245 lines of shell inside YAML block
# scalars in .github/workflows/ci.yml with no test at all. Three review
# rounds found four real holes in it, and each round's proof was built in a
# scratch directory and thrown away, so the next person to touch the guard
# edited it blind and reopened something. Every scenario those rounds
# established is a test here now. They are regressions, not one-off proofs.
#
# HOW IT WORKS. Each case builds a complete little tree (CHANGELOG, openapi
# spec, marketing page, marketing home, agent plugin, agent readme, README,
# install guide, release workflow), mutates exactly one thing, runs the guard
# against that tree and asserts the exit code plus what the output does and
# does not say. No mocking, no stubbing: the real script reads real files.
#
# RUN IT:
#   scripts/check-version-surfaces_test.sh            # everything
#   scripts/check-version-surfaces_test.sh badge      # only cases matching "badge"
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_VERSION_SURFACE_SCRIPT=/tmp/guard-with-hole.sh \
#     scripts/check-version-surfaces_test.sh
#
# PORTABILITY. Written for bash 3.2 (what macOS ships) and POSIX tools, so it
# runs the same on a darwin laptop with BSD grep/sed/awk and on the ubuntu CI
# runner with the GNU ones. No mapfile, no associative arrays, no sed -i, no
# grep -P, no sort -V, no \b or \s in any pattern.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_VERSION_SURFACE_SCRIPT:-$HERE/check-version-surfaces.sh}"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  echo "no guard script at $GUARD" >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-version-surfaces.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
SKIPPED=0
FAILED_NAMES=''

# ---------------------------------------------------------------------------
# Tree construction
#
# The fixture CHANGELOG has DELIBERATE GAPS. 0.61.129 is missing, and so is
# everything from 0.61.101 to 0.61.109. Those are the fixture's stand-ins for
# the 24 patch versions of 0.61.x that the real CHANGELOG.md has no heading
# for, which is the shape of hole 1: the old guard looked a version up by exact
# line match, printed nothing when it was absent, and warned and passed.
#
# Distances the cases below rely on (releases newer than the version):
#   0.61.131 -> 0    0.61.130 -> 1    0.61.129 (absent) -> 2
#   0.61.128 -> 2    0.61.127 -> 3    0.61.125 -> 5      0.61.124 -> 6
#   0.61.115 (absent) -> 11           0.61.105 (absent) -> 12
# ---------------------------------------------------------------------------
FIXTURE_RELEASES='0.61.131
0.61.130
0.61.128
0.61.127
0.61.126
0.61.125
0.61.124
0.61.123
0.61.122
0.61.121
0.61.120
0.61.110
0.61.100
0.60.5
0.60.0
0.59.0'

write_changelog() {
  _dir="$1"
  {
    echo '# Changelog'
    echo
    echo 'All notable changes to WPMgr are documented here.'
    echo
    echo '## [Unreleased]'
    echo
    printf '%s\n' "$FIXTURE_RELEASES" | while IFS= read -r v; do
      [ -n "$v" ] || continue
      echo "## [$v] - 2026-08-01"
      echo '### Fixed'
      echo '- A thing.'
      echo
    done
  } >"$_dir/CHANGELOG.md"
}

write_tree() {
  _dir="$1"
  mkdir -p "$_dir/packages/openapi" \
    "$_dir/apps/marketing/app/(marketing)/changelog" \
    "$_dir/apps/marketing/lib/content" \
    "$_dir/apps/agent" \
    "$_dir/apps/web/src" \
    "$_dir/docs" \
    "$_dir/.github/workflows"

  write_changelog "$_dir"

  cat >"$_dir/packages/openapi/openapi.yaml" <<'YAML'
openapi: 3.1.0
info:
  title: WPMgr API
  version: 0.61.131
paths: {}
YAML

  cat >"$_dir/apps/marketing/app/(marketing)/changelog/page.tsx" <<'TSX'
const RELEASES = [
  {
    version: "0.61.131",
    date: "2026-08-10",
    summary: "The newest release.",
  },
  {
    version: "0.61.125 - 0.61.128",
    date: "2026-08-05",
    summary: "A grouped entry, written oldest first.",
  },
];

export default function ChangelogPage() {
  return RELEASES.length;
}
TSX

  cat >"$_dir/apps/marketing/lib/content/home.ts" <<'TS'
// Must equal WPMGR_AGENT_VERSION in apps/agent/wpmgr-agent.php, exactly.
const AGENT_VERSION = "0.61.131";

export const HOME = {
  badge: `v${AGENT_VERSION} / open source`,
  facts: [{ label: "Version", value: AGENT_VERSION }],
};
TS

  cat >"$_dir/apps/agent/wpmgr-agent.php" <<'PHP'
<?php
/**
 * Plugin Name:       Fleet Agent Site Manager
 * Description:       The agent.
 * Version:           0.61.131
 * License:           GPLv2 or later
 */

if (!defined('ABSPATH')) {
    exit;
}

define('WPMGR_AGENT_VERSION', '0.61.131');
PHP

  cat >"$_dir/apps/agent/readme.txt" <<'TXT'
=== Fleet Agent Site Manager ===
Requires at least: 6.0
Tested up to: 6.9
Stable tag: 0.61.131
License: GPLv2 or later
TXT

  cat >"$_dir/apps/web/src/app.tsx" <<'TSX'
export const App = () => null;
TSX

  cat >"$_dir/.github/workflows/release.yml" <<'YML'
name: release
jobs:
  agent:
    steps:
      - name: Build the agent zip
        run: make agent-zip
YML

  cat >"$_dir/README.md" <<'MD'
# WPMgr

Self-hosted WordPress fleet management.

<!-- wpmgr-install-pins:start (the version below is a required pin) -->
**v0.61.131**: open-source and production-usable for self-hosters.
<!-- wpmgr-install-pins:end -->

## Quick start

Pull the published images:

<!-- wpmgr-install-pins:start (required pins) -->

```bash
docker pull ghcr.io/mosamlife/wpmgr-api:v0.61.131
docker pull ghcr.io/mosamlife/wpmgr-web:v0.61.131
docker pull ghcr.io/mosamlife/wpmgr-media-encoder:v0.61.131
```

Or bring up the whole stack from the published images:

```bash
export WPMGR_VERSION=v0.61.131   # omit to track :latest
docker compose -f infra/docker-compose.yml -f infra/docker-compose.prod.yml up -d
```

<!-- wpmgr-install-pins:end -->

Full install guide: [docs/install.md](./docs/install.md).
MD

  cat >"$_dir/docs/install.md" <<'MD'
# Install

## Prebuilt images

<!-- wpmgr-install-pins:start (required pin) -->

```bash
export WPMGR_VERSION=v0.61.131   # omit to track :latest
docker compose -f infra/docker-compose.yml -f infra/docker-compose.prod.yml up -d
```

<!-- wpmgr-install-pins:end -->

Everything else is inherited from the base compose file.
MD
}

git_index() {
  _dir="$1"
  command -v git >/dev/null 2>&1 || return 0
  (
    cd "$_dir" || exit 0
    git init -q 2>/dev/null || exit 0
    git add -A 2>/dev/null || true
  )
}

# tree NAME -> path to a fresh honest tree
tree() {
  _t="$WORK/$1"
  rm -rf "$_t"
  mkdir -p "$_t"
  write_tree "$_t"
  git_index "$_t"
  printf '%s' "$_t"
}

# ---------------------------------------------------------------------------
# Mutation helpers (portable: no sed -i, which differs between BSD and GNU)
# ---------------------------------------------------------------------------
sub() { # sub FILE SED-EXPR
  sed -E "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

drop() { # drop FILE ERE  (delete every matching line)
  grep -vE "$2" "$1" >"$1.tmp"
  mv "$1.tmp" "$1"
}

reindex() { # pick up files a case added after its tree was built
  # ONLY ever touches a throwaway tree under $WORK. An earlier version did not
  # check, and the case that runs the guard against this repository staged the
  # whole working tree as a side effect.
  case "$1" in
    "$WORK"/*) : ;;
    *) return 0 ;;
  esac
  command -v git >/dev/null 2>&1 || return 0
  [ -d "$1/.git" ] || return 0
  (cd "$1" && git add -A 2>/dev/null || true)
}

# ---------------------------------------------------------------------------
# Assertions
#
#   case NAME pass|fail DIR [+MUST-CONTAIN ...] [-MUST-NOT-CONTAIN ...]
# ---------------------------------------------------------------------------
case_run() {
  _name="$1"
  _want="$2"
  _dir="$3"
  shift 3

  if [ -n "$FILTER" ]; then
    case "$_name" in
      *"$FILTER"*) : ;;
      *)
        SKIPPED=$((SKIPPED + 1))
        return 0
        ;;
    esac
  fi

  reindex "$_dir"
  _out="$("$GUARD" "$_dir" 2>&1)"
  _code=$?
  _problems=''

  if [ "$_want" = pass ] && [ "$_code" -ne 0 ]; then
    _problems="$_problems
    expected exit 0, got $_code"
  fi
  if [ "$_want" = fail ] && [ "$_code" -eq 0 ]; then
    _problems="$_problems
    expected a non-zero exit, got 0"
  fi

  for _a in "$@"; do
    case "$_a" in
      +*)
        _needle="${_a#+}"
        if ! printf '%s\n' "$_out" | grep -qF "$_needle"; then
          _problems="$_problems
    expected the output to contain: $_needle"
        fi
        ;;
      -*)
        _needle="${_a#-}"
        if printf '%s\n' "$_out" | grep -qF "$_needle"; then
          _problems="$_problems
    expected the output NOT to contain: $_needle"
        fi
        ;;
    esac
  done

  if [ -n "$_problems" ]; then
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES  $_name
"
    printf 'FAIL %s%s\n' "$_name" "$_problems"
    printf '%s\n' "$_out" | sed 's/^/      | /'
  else
    PASSED=$((PASSED + 1))
    printf 'ok   %s\n' "$_name"
  fi
}

# ===========================================================================
# HOLE 1: staleness had 24 blind values.
#
# releases_behind used to look the version up by exact line match in
# CHANGELOG.md and print nothing when it was absent, which warned and exited 0.
# A pin could sit on a real old release forever. Distance is now counted as
# "how many entries are newer than this one", which needs no entry for the
# version itself.
# ===========================================================================
t="$(tree hole1-pin-absent-near)"
sub "$t/README.md" 's/v0\.61\.131/v0.61.129/g'
sub "$t/docs/install.md" 's/v0\.61\.131/v0.61.129/g'
case_run "hole1: a pin on 0.61.129, which has no CHANGELOG heading, is 2 behind and fails" \
  fail "$t" "+2 releases behind" "-skipping"

t="$(tree hole1-pin-absent-far)"
sub "$t/README.md" 's/v0\.61\.131/v0.61.105/g'
sub "$t/docs/install.md" 's/v0\.61\.131/v0.61.105/g'
case_run "hole1: a pin on 0.61.105, deep inside a CHANGELOG gap, is 12 behind and fails" \
  fail "$t" "+12 releases behind"

t="$(tree hole1-marketing-absent-stale)"
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.131"/"0.61.115"/'
case_run "hole1: a marketing entry on 0.61.115, absent from the CHANGELOG, is 11 behind and fails" \
  fail "$t" "+11 releases behind"

t="$(tree hole1-marketing-absent-in-tolerance)"
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.131"/"0.61.129"/'
case_run "hole1: an absent version still INSIDE tolerance passes (the counting rule does not over-fire)" \
  pass "$t" "+2 releases behind"

t="$(tree hole1-changelog-unreadable-warns)"
: >"$t/CHANGELOG.md"
case_run "hole1: an unreadable CHANGELOG still warns and skips only the distance compares" \
  pass "$t" "+WARN"

# ===========================================================================
# HOLE 2: the badge check switched itself off on ordinary edits.
#
# The old pattern was ^(export )?const AGENT_VERSION[[:space:]]*=, so a type
# annotation, a `let`, or an indent all produced a WARN and exit 0 while the
# step comment claimed immunity to exactly that. Each of these carries a badge
# on 0.61.127 against an agent on 0.61.131 and must FAIL.
# ===========================================================================
t="$(tree hole2-type-annotation)"
sub "$t/apps/marketing/lib/content/home.ts" 's/^const AGENT_VERSION = "0\.61\.131";/const AGENT_VERSION: string = "0.61.127";/'
case_run "hole2: a TypeScript type annotation does not disable the badge check" \
  fail "$t" "+hero badge names agent 0.61.127" "-skipping"

t="$(tree hole2-let)"
sub "$t/apps/marketing/lib/content/home.ts" 's/^const AGENT_VERSION = "0\.61\.131";/let AGENT_VERSION = "0.61.127";/'
case_run "hole2: let instead of const does not disable the badge check" \
  fail "$t" "+hero badge names agent 0.61.127"

t="$(tree hole2-indented)"
sub "$t/apps/marketing/lib/content/home.ts" 's/^const AGENT_VERSION = "0\.61\.131";/  const AGENT_VERSION = "0.61.127";/'
case_run "hole2: an indented declaration does not disable the badge check" \
  fail "$t" "+hero badge names agent 0.61.127"

t="$(tree hole2-export-const)"
sub "$t/apps/marketing/lib/content/home.ts" 's/^const AGENT_VERSION = "0\.61\.131";/export const AGENT_VERSION = "0.61.127";/'
case_run "hole2: an export keyword does not disable the badge check" \
  fail "$t" "+hero badge names agent 0.61.127"

t="$(tree hole2-single-quotes)"
sub "$t/apps/marketing/lib/content/home.ts" "s/^const AGENT_VERSION = \"0\\.61\\.131\";/const AGENT_VERSION = '0.61.127';/"
case_run "hole2: a quote-style change does not disable the badge check" \
  fail "$t" "+hero badge names agent 0.61.127"

t="$(tree hole2-declaration-missing)"
drop "$t/apps/marketing/lib/content/home.ts" 'AGENT_VERSION[[:space:]]*='
case_run "hole2: a home.ts with no AGENT_VERSION at all is an error, not a warning" \
  fail "$t" "+declares no AGENT_VERSION"

t="$(tree hole2-declaration-without-version)"
sub "$t/apps/marketing/lib/content/home.ts" 's/^const AGENT_VERSION = "0\.61\.131";/const AGENT_VERSION = FALLBACK_VERSION;/'
case_run "hole2: a declaration that names no version literal is an error, not a warning" \
  fail "$t" "+names no MAJOR.MINOR.PATCH"

# The agent triple had the same defect: an anchored define pattern that the
# WordPress coding standard's own spacing does not match.
t="$(tree hole2-agent-wpcs-spacing-drift)"
sub "$t/apps/agent/wpmgr-agent.php" "s/^define\\('WPMGR_AGENT_VERSION', '0\\.61\\.131'\\);/define( 'WPMGR_AGENT_VERSION', '0.61.130' );/"
case_run "hole2: WPCS define( ) spacing does not disable the agent triple check" \
  fail "$t" "+agent version disagrees with itself"

t="$(tree hole2-agent-constant-missing)"
drop "$t/apps/agent/wpmgr-agent.php" "WPMGR_AGENT_VERSION"
case_run "hole2: an agent with no WPMGR_AGENT_VERSION is an error, not a warning" \
  fail "$t" "+declares no WPMGR_AGENT_VERSION"

# ===========================================================================
# HOLE 3: commented-out pins satisfied presence.
#
# It was pure grep with no markdown or shell awareness, so an HTML-commented
# pin and a #-commented line inside a fenced block both counted, and CI then
# printed that every required pin was present over a block no reader could
# copy from.
# ===========================================================================
t="$(tree hole3-html-comment)"
sub "$t/docs/install.md" 's|^export WPMGR_VERSION=v0\.61\.131.*$|<!-- export WPMGR_VERSION=v0.61.131 -->|'
case_run "hole3: a pin inside an HTML comment does not satisfy presence" \
  fail "$t" "+docs/install.md is missing a required install pin" "-every required install pin is present"

t="$(tree hole3-shell-comment-in-fence)"
sub "$t/docs/install.md" 's|^export WPMGR_VERSION=v0\.61\.131.*$|# export WPMGR_VERSION=v0.61.131|'
case_run "hole3: a pin commented out with # inside a fenced block does not satisfy presence" \
  fail "$t" "+docs/install.md is missing a required install pin"

t="$(tree hole3-commented-pull-line)"
sub "$t/README.md" 's|^docker pull ghcr\.io/mosamlife/wpmgr-web:v0\.61\.131$|# docker pull ghcr.io/mosamlife/wpmgr-web:v0.61.131|'
case_run "hole3: a commented-out docker pull does not satisfy presence" \
  fail "$t" "+the docker pull tag for the web image"

t="$(tree hole3-multiline-html-comment)"
sub "$t/README.md" 's|^Or bring up the whole stack from the published images:$|<!-- the block below is parked|'
sub "$t/README.md" 's|^Full install guide.*$|-->\nFull install guide.|'
case_run "hole3: a multi-line HTML comment hides the pins it wraps" \
  fail "$t" "+missing a required install pin"

t="$(tree hole3-trailing-comment-is-fine)"
sub "$t/README.md" 's|^docker pull ghcr\.io/mosamlife/wpmgr-api:v0\.61\.131$|docker pull ghcr.io/mosamlife/wpmgr-api:v0.61.131   # the control plane|'
case_run "hole3: a trailing comment beside a live pin keeps the pin" \
  pass "$t" "+every required install pin is present"

t="$(tree hole3-pin-in-prose-not-code)"
sub "$t/docs/install.md" 's|^export WPMGR_VERSION=v0\.61\.131.*$||'
sub "$t/docs/install.md" 's|^Everything else is inherited.*$|Set WPMGR_VERSION=v0.61.131 before composing.|'
case_run "hole3: a pin that exists only as prose does not satisfy a copyable code pin" \
  fail "$t" "+missing a required install pin"

# ===========================================================================
# HOLE 4: the exact-count surplus rule blocked honest edits.
#
# Appending an ordinary Upgrading section with a second CURRENT pin exited 1,
# and a README sentence naming the prior release exited 1 with a FALSE claim
# that the pins disagreed. Required pins are now anchored to an explicit
# region, the count inside it is a minimum, and prose outside it is free.
# ===========================================================================
t="$(tree hole4-upgrading-section)"
cat >>"$t/docs/install.md" <<'MD'

## Upgrading

Pull the new images and restart:

```bash
export WPMGR_VERSION=v0.61.131
docker compose -f infra/docker-compose.yml -f infra/docker-compose.prod.yml up -d
```
MD
case_run "hole4: an honest Upgrading section with a second current pin passes" \
  pass "$t" "+every required install pin is present"

# Reported verbatim by the review: a README line that BEGINS with a bold
# version, which is the shape the status-line pattern matches, exited 1 with two
# errors, one of them a false claim that the pins disagreed.
t="$(tree hole4-prose-names-prior-release)"
sub "$t/README.md" 's|^Self-hosted WordPress fleet management\.$|Self-hosted WordPress fleet management.\n\n**v0.61.130** was the prior release.|'
case_run "hole4: a prose line beginning with a bold prior version does not redden CI" \
  pass "$t" "-name more than one version" "-carries more copies"

t="$(tree hole4-second-region-is-allowed)"
cat >>"$t/README.md" <<'MD'

## Upgrading

<!-- wpmgr-install-pins:start -->

```bash
export WPMGR_VERSION=v0.61.131
docker pull ghcr.io/mosamlife/wpmgr-api:v0.61.131
```

<!-- wpmgr-install-pins:end -->
MD
case_run "hole4: a second marked region with extra current pins passes" \
  pass "$t" "+every required install pin is present"

t="$(tree hole4-surplus-stale-still-caught)"
cat >>"$t/README.md" <<'MD'

## Upgrading

<!-- wpmgr-install-pins:start -->

```bash
export WPMGR_VERSION=v0.19.0
```

<!-- wpmgr-install-pins:end -->
MD
case_run "hole4: a surplus pin that is STALE is still caught, by disagreement and by distance" \
  fail "$t" "+name more than one version" "+releases behind"

t="$(tree hole4-region-missing)"
drop "$t/docs/install.md" 'wpmgr-install-pins'
case_run "hole4: a file with a required pin and no region marker is an error" \
  fail "$t" "+carries no <!-- wpmgr-install-pins:start --> region"

t="$(tree hole4-region-unbalanced)"
drop "$t/docs/install.md" 'wpmgr-install-pins:end'
case_run "hole4: unbalanced region markers are an error" \
  fail "$t" "+markers"

# ===========================================================================
# The three smaller findings the same review raised.
# ===========================================================================
t="$(tree tail-rc-suffix)"
sub "$t/README.md" 's|wpmgr-api:v0\.61\.131|wpmgr-api:v0.61.131-rc1|'
case_run "tail: a -rc1 suffix is not a released tag and no longer reads as current" \
  fail "$t" "+which is not a released tag"

t="$(tree tail-missing-leading-v)"
sub "$t/docs/install.md" 's|^export WPMGR_VERSION=v0\.61\.131|export WPMGR_VERSION=0.61.131|'
case_run "tail: a pin without the leading v is reported as malformed, not as missing" \
  fail "$t" "+without the leading v" "-missing a required install pin"

t="$(tree tail-sweep-new-file-stale-tag)"
cat >"$t/docs/deploy.md" <<'MD'
# Deploy

```bash
docker pull ghcr.io/mosamlife/wpmgr-api:v0.19.0
```
MD
case_run "tail: a NEW file nobody declared, carrying a stale image tag, is caught by the sweep" \
  fail "$t" "+docs/deploy.md" "+releases behind"

t="$(tree tail-sweep-new-file-stale-export)"
cat >"$t/docs/deploy.md" <<'MD'
# Deploy

```bash
export WPMGR_VERSION=v0.19.0
```
MD
case_run "tail: a NEW file carrying a stale WPMGR_VERSION is caught by the sweep" \
  fail "$t" "+docs/deploy.md"

t="$(tree tail-sweep-opt-out)"
cat >"$t/docs/history.md" <<'MD'
# History

The first public image was `ghcr.io/mosamlife/wpmgr-api:v0.19.0`. <!-- wpmgr-version-ignore -->
MD
case_run "tail: a line marked wpmgr-version-ignore is left alone by the sweep" \
  pass "$t"

t="$(tree tail-sweep-placeholders)"
cat >"$t/docs/compose.md" <<'MD'
# Compose

```yaml
services:
  api:
    image: ghcr.io/mosamlife/wpmgr-api:${WPMGR_VERSION:-latest}
```

Set `WPMGR_VERSION=vX.Y.Z`, or leave it empty with `WPMGR_VERSION=` to track
`ghcr.io/mosamlife/wpmgr-api:latest`.
MD
case_run "tail: placeholders, variables and :latest are not version claims" \
  pass "$t"

t="$(tree tail-sweep-newer-than-changelog)"
cat >"$t/docs/deploy.md" <<'MD'
# Deploy

```bash
docker pull ghcr.io/mosamlife/wpmgr-api:v0.99.0
```
MD
case_run "tail: a swept tag newer than the top CHANGELOG entry is an error" \
  fail "$t" "+newer than the top"

t="$(tree tail-sweep-without-git)"
cat >"$t/docs/deploy.md" <<'MD'
# Deploy

```bash
docker pull ghcr.io/mosamlife/wpmgr-api:v0.19.0
```
MD
rm -rf "$t/.git"
case_run "tail: the sweep still works in a tree that is not a git checkout" \
  fail "$t" "+docs/deploy.md"

# ===========================================================================
# Regressions from the earlier rounds. Each of these was a real finding.
# ===========================================================================
t="$(tree regress-openapi-mismatch)"
sub "$t/packages/openapi/openapi.yaml" 's/version: 0\.61\.131/version: 0.61.130/'
case_run "regress: openapi info.version drifting from the top CHANGELOG entry fails" \
  fail "$t" "+info.version is 0.61.130"

t="$(tree regress-openapi-quoted)"
sub "$t/packages/openapi/openapi.yaml" 's/version: 0\.61\.131/version: "0.61.131"/'
case_run "regress: a quoted openapi info.version still parses" \
  pass "$t" "+info.version matches"

t="$(tree regress-marketing-six-behind)"
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.131"/"0.61.124"/'
case_run "regress: a marketing changelog 6 releases behind fails at tolerance 5" \
  fail "$t" "+6 releases behind"

t="$(tree regress-marketing-five-behind)"
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.131"/"0.61.125"/'
case_run "regress: a marketing changelog exactly 5 releases behind passes" \
  pass "$t" "+5 releases behind"

t="$(tree regress-marketing-grouped-reads-newest)"
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.131"/"0.61.120 - 0.61.131"/'
case_run "regress: a grouped newest entry is measured from its NEWEST end, not the first number on the line" \
  pass "$t" "+0 releases behind"

t="$(tree regress-marketing-empty)"
drop "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 'version: "'
case_run "regress: a marketing changelog page with no entries at all is an error" \
  fail "$t" "+carries no version"

t="$(tree regress-surface-newer-than-changelog)"
sub "$t/README.md" 's/v0\.61\.131/v0.61.140/g'
sub "$t/docs/install.md" 's/v0\.61\.131/v0.61.140/g'
case_run "regress: an install pin naming a version newer than the CHANGELOG fails" \
  fail "$t" "+newer than the top"

t="$(tree regress-one-pull-line-deleted)"
drop "$t/README.md" 'wpmgr-web:v0'
case_run "regress: a README that lost one of its three pull commands fails, naming the file and the pin" \
  fail "$t" "+README.md is missing a required install pin" "+the docker pull tag for the web image"

t="$(tree regress-all-pins-deleted)"
drop "$t/README.md" 'wpmgr-(api|web|media-encoder):v0|WPMGR_VERSION|^\*\*v0'
drop "$t/docs/install.md" 'WPMGR_VERSION'
case_run "regress: deleting every pin is an error, never a warning that returns success" \
  fail "$t" "+not one install pin could be read"

t="$(tree regress-reformatted-stale-pin)"
sub "$t/docs/install.md" 's|^export WPMGR_VERSION=v0\.61\.131.*$|export WPMGR_VERSION="v0.19.0"|'
case_run "regress: a reformatted STALE pin fails as stale, not as absent" \
  fail "$t" "+releases behind" "-missing a required install pin"

t="$(tree regress-pins-disagree)"
sub "$t/docs/install.md" 's/v0\.61\.131/v0.61.130/'
case_run "regress: README and docs/install.md naming different releases fails" \
  fail "$t" "+name more than one version"

t="$(tree regress-changelog-unparseable-badge-still-checked)"
: >"$t/CHANGELOG.md"
sub "$t/apps/marketing/lib/content/home.ts" 's/"0\.61\.131"/"0.61.127"/'
case_run "regress: an unparseable CHANGELOG does not switch off the badge check" \
  fail "$t" "+hero badge names agent 0.61.127"

t="$(tree regress-changelog-unparseable-pins-still-checked)"
: >"$t/CHANGELOG.md"
drop "$t/README.md" 'wpmgr-api:v0'
case_run "regress: an unparseable CHANGELOG does not switch off the pin presence check" \
  fail "$t" "+missing a required install pin"

t="$(tree regress-release-yml-stamps-version)"
sub "$t/.github/workflows/release.yml" 's|run: make agent-zip|run: make agent-zip VERSION=${{ github.ref_name }}|'
case_run "regress: release.yml stamping the agent zip from the git tag fails" \
  fail "$t" "+passes VERSION to agent-zip"

t="$(tree regress-agent-stable-tag-drift)"
sub "$t/apps/agent/readme.txt" 's/^Stable tag: 0\.61\.131/Stable tag: 0.61.130/'
case_run "regress: the readme.txt Stable tag drifting from the plugin fails" \
  fail "$t" "+agent version disagrees with itself"

t="$(tree regress-agent-header-drift)"
sub "$t/apps/agent/wpmgr-agent.php" 's/^ \* Version:           0\.61\.131/ * Version:           0.61.130/'
case_run "regress: the plugin header drifting from the constant fails" \
  fail "$t" "+agent version disagrees with itself"

t="$(tree regress-minor-bump-keeps-comparing)"
sub "$t/CHANGELOG.md" 's/^## \[Unreleased\]$/## [Unreleased]\n\n## [0.62.5] - 2026-08-11\n### Fixed\n- e\n\n## [0.62.4] - 2026-08-11\n### Fixed\n- d\n\n## [0.62.3] - 2026-08-11\n### Fixed\n- c\n\n## [0.62.2] - 2026-08-11\n### Fixed\n- b\n\n## [0.62.1] - 2026-08-11\n### Fixed\n- a\n\n## [0.62.0] - 2026-08-11\n### Added\n- A minor bump./'
sub "$t/packages/openapi/openapi.yaml" 's/version: 0\.61\.131/version: 0.62.5/'
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.131"/"0.62.5"/'
case_run "regress: a minor bump does not disable the distance compare (pins 6 behind across 0.61 to 0.62 fail)" \
  fail "$t" "+6 releases behind"

t="$(tree honest-minor-bump)"
sub "$t/CHANGELOG.md" 's/^## \[Unreleased\]$/## [Unreleased]\n\n## [0.62.0] - 2026-08-11\n### Added\n- A minor bump./'
sub "$t/packages/openapi/openapi.yaml" 's/version: 0\.61\.131/version: 0.62.0/'
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.131"/"0.62.0"/'
sub "$t/README.md" 's/v0\.61\.131/v0.62.0/g'
sub "$t/docs/install.md" 's/v0\.61\.131/v0.62.0/g'
case_run "honest: a minor bump with every surface moved passes" pass "$t"

t="$(tree honest-major-bump)"
sub "$t/CHANGELOG.md" 's/^## \[Unreleased\]$/## [Unreleased]\n\n## [1.0.0] - 2026-08-11\n### Added\n- A major bump./'
sub "$t/packages/openapi/openapi.yaml" 's/version: 0\.61\.131/version: 1.0.0/'
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.131"/"1.0.0"/'
sub "$t/README.md" 's/v0\.61\.131/v1.0.0/g'
sub "$t/docs/install.md" 's/v0\.61\.131/v1.0.0/g'
case_run "honest: a major bump with every surface moved passes" pass "$t"

# ===========================================================================
# Honest trees. Every one of these must stay green, because a guard that
# reddens an honest tree gets deleted, and then it guards nothing.
# ===========================================================================
case_run "honest: the pristine tree passes" pass "$(tree honest-pristine)"

t="$(tree honest-control-plane-burst)"
sub "$t/apps/marketing/lib/content/home.ts" 's/"0\.61\.131"/"0.61.127"/'
sub "$t/apps/agent/wpmgr-agent.php" 's/0\.61\.131/0.61.127/g'
sub "$t/apps/agent/readme.txt" 's/0\.61\.131/0.61.127/'
case_run "honest: a control-plane-only burst with a truthfully frozen agent badge passes" \
  pass "$t" "+names agent 0.61.127"

t="$(tree honest-unreleased-only)"
sub "$t/CHANGELOG.md" 's/^## \[Unreleased\]$/## [Unreleased]\n### Fixed\n- Something not yet released./'
case_run "honest: an [Unreleased] section with entries under it passes" pass "$t"

t="$(tree honest-frontend-only)"
cat >"$t/apps/web/src/app.tsx" <<'TSX'
export const App = () => <div>changed</div>;
TSX
case_run "honest: a frontend-only change passes" pass "$t"

t="$(tree honest-release-pr-mid-bump)"
sub "$t/README.md" 's/v0\.61\.131/v0.61.130/g'
sub "$t/docs/install.md" 's/v0\.61\.131/v0.61.130/g'
case_run "honest: a release PR one release behind the new entry passes at tolerance 1" \
  pass "$t" "+1 release behind"

t="$(tree honest-two-behind-fails)"
sub "$t/README.md" 's/v0\.61\.131/v0.61.128/g'
sub "$t/docs/install.md" 's/v0\.61\.131/v0.61.128/g'
case_run "honest: two releases behind is past tolerance and fails (the window does not stretch)" \
  fail "$t" "+2 releases behind"

t="$(tree honest-grouped-eight-wide)"
sub "$t/apps/marketing/app/(marketing)/changelog/page.tsx" 's/"0\.61\.125 - 0\.61\.128"/"0.61.120 - 0.61.128"/'
case_run "honest: a grouped entry naming eight versions passes when the page itself is current" pass "$t"

case_run "honest: this repository's own tree passes" pass "$REPO_ROOT"

# ===========================================================================
# The ten harmless reformats. A formatter must never redden CI, and must never
# silently switch a check off either: each of these still READS its pin, which
# is why a reformatted stale pin (above) still fails as stale.
# ===========================================================================
t="$(tree reformat-01-no-export)"
sub "$t/README.md" 's/^export WPMGR_VERSION=/WPMGR_VERSION=/'
case_run "reformat 01: dropping the export keyword" pass "$t" "+every required install pin is present"

t="$(tree reformat-02-double-quotes)"
sub "$t/README.md" 's|^export WPMGR_VERSION=v0\.61\.131.*$|export WPMGR_VERSION="v0.61.131"|'
case_run "reformat 02: double quotes around the value" pass "$t" "+every required install pin is present"

t="$(tree reformat-03-single-quotes)"
sub "$t/docs/install.md" "s|^export WPMGR_VERSION=v0\\.61\\.131.*\$|export WPMGR_VERSION='v0.61.131'|"
case_run "reformat 03: single quotes around the value" pass "$t" "+every required install pin is present"

t="$(tree reformat-04-spaces-around-equals)"
sub "$t/docs/install.md" 's|^export WPMGR_VERSION=v0\.61\.131.*$|export WPMGR_VERSION = v0.61.131|'
case_run "reformat 04: spaces around the equals sign" pass "$t" "+every required install pin is present"

t="$(tree reformat-05-indented)"
sub "$t/docs/install.md" 's|^export WPMGR_VERSION=v0\.61\.131.*$|    export WPMGR_VERSION=v0.61.131|'
case_run "reformat 05: an indented pin line" pass "$t" "+every required install pin is present"

t="$(tree reformat-06-blockquoted-status-line)"
sub "$t/README.md" 's|^\*\*v0\.61\.131\*\*|> **v0.61.131**|'
case_run "reformat 06: a blockquoted status line" pass "$t" "+every required install pin is present"

t="$(tree reformat-07-tilde-fence)"
sub "$t/docs/install.md" 's|^```bash$|~~~bash|; s|^```$|~~~|'
case_run "reformat 07: a tilde fence instead of backticks" pass "$t" "+every required install pin is present"

t="$(tree reformat-08-wpcs-define)"
sub "$t/apps/agent/wpmgr-agent.php" "s/^define\\('WPMGR_AGENT_VERSION', '0\\.61\\.131'\\);/define( 'WPMGR_AGENT_VERSION', '0.61.131' );/"
case_run "reformat 08: WPCS spacing in the agent define" pass "$t" "+agent version triple agrees"

t="$(tree reformat-09-badge-annotated)"
sub "$t/apps/marketing/lib/content/home.ts" 's/^const AGENT_VERSION = "0\.61\.131";/export const AGENT_VERSION: string = "0.61.131";/'
case_run "reformat 09: an exported, type-annotated badge constant" pass "$t" "+names agent 0.61.131"

t="$(tree reformat-10-badge-let-indented)"
sub "$t/apps/marketing/lib/content/home.ts" 's/^const AGENT_VERSION = "0\.61\.131";/  let AGENT_VERSION = "0.61.131";/'
case_run "reformat 10: an indented let badge constant" pass "$t" "+names agent 0.61.131"

# ===========================================================================
# Files that are simply absent.
# ===========================================================================
t="$(tree absent-readme)"
rm -f "$t/README.md"
case_run "absent: no README.md at all is an error naming the file" \
  fail "$t" "+README.md does not exist"

t="$(tree absent-install-guide)"
rm -f "$t/docs/install.md"
case_run "absent: no docs/install.md at all is an error naming the file" \
  fail "$t" "+docs/install.md does not exist"

t="$(tree absent-openapi)"
rm -f "$t/packages/openapi/openapi.yaml"
case_run "absent: no openapi.yaml is an error" fail "$t" "+openapi.yaml does not exist"

t="$(tree absent-agent-readme)"
rm -f "$t/apps/agent/readme.txt"
case_run "absent: no agent readme.txt is an error" fail "$t" "+readme.txt does not exist"

# ---------------------------------------------------------------------------
printf '\n'
if [ -n "$FILTER" ]; then
  printf 'filter: %s (%s skipped)\n' "$FILTER" "$SKIPPED"
fi
printf '%s passed, %s failed\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf 'failing cases:\n%s' "$FAILED_NAMES"
  exit 1
fi
exit 0
