#!/usr/bin/env bash
# scripts/check-version-surfaces.sh
#
# Every place in this repo that names a version to a reader, checked against
# the thing it claims to describe.
#
# WHY THIS IS A SCRIPT AND NOT A CI STEP. It used to be 245 lines of shell
# inside YAML block scalars in .github/workflows/ci.yml, 144 of them in a single
# step, with no test. Three review rounds found four real holes in it, and the
# reason each round found more is that nobody could run it, so nobody could
# check their work:
# every proof lived in a scratch directory and was thrown away. Here it is a
# file you can run, and scripts/check-version-surfaces_test.sh builds trees and
# asserts exit codes against it, so a hole that gets reopened turns a test red
# instead of waiting for the next reviewer.
#
# RUN IT (no CI, no install, nothing but a shell):
#   make check-versions          or  scripts/check-version-surfaces.sh
#   make check-versions-test     or  scripts/check-version-surfaces_test.sh
#   scripts/check-version-surfaces.sh /path/to/some/other/tree
#
# Exit code is 0 when every check passes, 1 when any check finds a real drift.
# Warnings never fail the run.
#
# WHAT IT CHECKS (five checks, all of them in one place now):
#
#   1. packages/openapi/openapi.yaml info.version equals the top CHANGELOG.md
#      entry exactly.
#   2. The newest entry on the marketing /changelog page is within 5 releases
#      of the top CHANGELOG.md entry.
#   3. The marketing hero badge (AGENT_VERSION) equals the agent plugin's
#      WPMGR_AGENT_VERSION exactly.
#   4. The agent version triple (plugin header, WPMGR_AGENT_VERSION, readme.txt
#      Stable tag) agrees with itself, and release.yml does not stamp the agent
#      zip from the git tag.
#   5. The install pins a self-hoster copies verbatim are PRESENT, agree with
#      each other, and sit within 1 release of the top CHANGELOG.md entry. Then
#      a repo-wide sweep catches any other concrete image tag or WPMGR_VERSION
#      value that has gone stale, including in files nobody declared.
#
# DISTANCE IS COUNTED IN RELEASES, never by subtracting patch numbers.
# "N releases behind" means "CHANGELOG.md lists N entries newer than this one".
# Subtracting patch numbers is only meaningful while major.minor match, so an
# earlier version of this guard skipped the compare whenever they did not,
# which meant one minor bump disabled it permanently.
#
# AND IT IS COUNTED FOR VERSIONS THE CHANGELOG HAS NO ENTRY FOR TOO. The
# earlier version looked the version up by exact line match and printed nothing
# when it was absent, which warned and passed. That was 24 blind values in
# 0.61.x alone (0.61.1, 0.61.2, 0.61.4, 0.61.5, 0.61.7, 0.61.8, 0.61.11 to
# 0.61.13, 0.61.16, 0.61.24, 0.61.48 to 0.61.52, 0.61.61, 0.61.63, 0.61.66,
# 0.61.67, 0.61.70, 0.61.72 to 0.61.74, measured on 2026-08-10 against a
# CHANGELOG whose top entry was 0.61.131): a pin left on any of them passed
# forever. Counting entries newer than the version needs no entry for the
# version itself, gives the identical answer for versions that do have one, and
# has no blind values at all.
#
# FORGIVENESS RULE, and where it stops. Something the guard cannot READ warns
# and skips, because a false red gets a guard deleted. Something the guard has
# READ and proved absent or wrong is an error. So an unparseable CHANGELOG.md
# warns and skips only the compares that need the release ordering; a missing
# pin, a missing declaration, and a value that is present but is not a release
# tag are all errors. "It parsed but I could not place it" is no longer one of
# the warn cases: see the counting rule above.

set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/check-version-surfaces.sh [ROOT]

ROOT defaults to the repository this script lives in, or
$WPMGR_VERSION_SURFACE_ROOT when that is set.

Exit 0: every version surface is current.
Exit 1: at least one surface names something untrue.
USAGE
}

case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${1:-${WPMGR_VERSION_SURFACE_ROOT:-$(cd "$SCRIPT_DIR/.." && pwd)}}"

if [ ! -d "$ROOT" ]; then
  echo "ERROR: $ROOT is not a directory." >&2
  exit 2
fi
cd "$ROOT" || exit 2

# ---------------------------------------------------------------------------
# Tolerances, in releases. One place, so the docs can quote them.
# ---------------------------------------------------------------------------
TOL_MARKETING_CHANGELOG=5
TOL_INSTALL_PINS=1
TOL_SWEEP=1

# Paths.
F_CHANGELOG='CHANGELOG.md'
F_OPENAPI='packages/openapi/openapi.yaml'
F_MK_CHANGELOG='apps/marketing/app/(marketing)/changelog/page.tsx'
F_HOME='apps/marketing/lib/content/home.ts'
F_AGENT_MAIN='apps/agent/wpmgr-agent.php'
F_AGENT_README='apps/agent/readme.txt'
F_RELEASE_WF='.github/workflows/release.yml'

# The marker that delimits a required install-pin region in a markdown file.
PIN_MARKER='wpmgr-install-pins'
# A line carrying this opts out of the repo-wide sweep.
SWEEP_OPT_OUT='wpmgr-version-ignore'

fail=0
# errors counts every error raised; fail is only whether any were. A caller that
# wants to know "did MY section raise anything" must compare the counter, since
# the flag saturates at 1 and then reads the same before and after.
errors=0
err() {
  printf 'ERROR: %s\n' "$1"
  fail=1
  errors=$((errors + 1))
}
detail() { printf '  %s\n' "$1"; }
warn() { printf 'WARN: %s\n' "$1"; }
ok() { printf 'OK: %s\n' "$1"; }

# ---------------------------------------------------------------------------
# Version helpers
# ---------------------------------------------------------------------------

is_semver() {
  case "$1" in
    '') return 1 ;;
  esac
  printf '%s' "$1" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'
}

# ver_cmp A B -> 1 when A is newer, -1 when older, 0 when equal. Compares every
# component numerically, so it is correct across a minor or a major bump.
ver_cmp() {
  awk -v a="$1" -v b="$2" 'BEGIN{
    na=split(a,x,"."); nb=split(b,y,".");
    n=(na>nb)?na:nb;
    for(i=1;i<=n;i++){
      xi=(i<=na)?x[i]+0:0; yi=(i<=nb)?y[i]+0:0;
      if(xi>yi){print 1; exit}
      if(xi<yi){print -1; exit}
    }
    print 0}'
}

# The released versions, newest first. Position in this list is the unit that
# "N releases behind" is measured in, everywhere in this script.
RELEASES="$(grep -oE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' "$F_CHANGELOG" 2>/dev/null | tr -d '#[] ' || true)"
TOP="$(printf '%s\n' "$RELEASES" | sed -n '1p')"

# releases_behind VERSION -> how many CHANGELOG entries are newer than VERSION.
# For a version the CHANGELOG lists this is exactly its index in the list. For
# one it does not list it is still the truth: that many releases have shipped
# since. Never prints nothing for a version that parses.
releases_behind() {
  printf '%s\n' "$RELEASES" | awk -v v="$1" '
    BEGIN{ split(v,t,".") }
    NF {
      split($0,x,".")
      for (i = 1; i <= 3; i++) {
        if (x[i]+0 > t[i]+0) { c++; break }
        if (x[i]+0 < t[i]+0) { break }
      }
    }
    END { print c+0 }'
}

rel_word() { if [ "$1" -eq 1 ]; then printf 'release'; else printf 'releases'; fi; }

# compare_to_top LABEL VERSION TOLERANCE REMEDY
# Errors when VERSION is newer than the top entry (a reader cannot look it up)
# or further behind than TOLERANCE. Warns and skips only when the CHANGELOG
# itself could not be read.
compare_to_top() {
  _label="$1"
  _v="$2"
  _tol="$3"
  _fix="$4"
  if [ -z "$TOP" ]; then
    warn "$F_CHANGELOG lists no released version, so the distance for $_label ($_v) cannot be measured; skipping that compare."
    return 0
  fi
  if [ "$(ver_cmp "$_v" "$TOP")" = "1" ]; then
    err "$_label names $_v, which is newer than the top $F_CHANGELOG entry ($TOP)."
    detail "Nothing may name a release the changelog has no entry for. $_fix"
    return 1
  fi
  _behind="$(releases_behind "$_v")"
  if [ "$_behind" -gt "$_tol" ]; then
    err "$_label names $_v, which is $_behind $(rel_word "$_behind") behind the top $F_CHANGELOG entry ($TOP). Tolerance is $_tol."
    detail "$_fix"
    return 1
  fi
  ok "$_label ($_v): $_behind $(rel_word "$_behind") behind $TOP, tolerance $_tol."
  return 0
}

# ---------------------------------------------------------------------------
# Markdown awareness
#
# md_actionable FILE [REGION-MARKER]
#
# Prints the lines of FILE a reader can actually act on, as LINE|INFENCE|TEXT.
# Everything a reader cannot copy and run is dropped:
#
#   * HTML comment spans, including ones that open and close on different
#     lines. <!-- export WPMGR_VERSION=v0.61.131 --> is not an install step,
#     and before this the guard counted it as one.
#   * Inside a fenced code block, a line whose first non-blank character is #.
#     That is a shell comment. A commented-out docker pull satisfied the old
#     grep and CI then printed that every required pin was present, over a
#     block from which no reader could copy one.
#
# Outside a fence, a leading # is a markdown heading, so the rule is applied
# only inside fences. A trailing comment on a live line (the "# omit to track
# :latest" note beside the export) keeps its line, because the pin in front of
# it is still real.
#
# With REGION-MARKER set, only lines between <!-- MARKER:start --> and
# <!-- MARKER:end --> are emitted. Fence state is tracked across the whole file
# either way, so a region that begins mid-document still knows whether it is
# inside a code block.
# ---------------------------------------------------------------------------
md_actionable() {
  awk -v region="${2:-}" '
    function strip_html(s,   out, p) {
      out = ""
      while (length(s) > 0) {
        if (incomment) {
          p = index(s, "-->")
          if (p == 0) { s = ""; break }
          s = substr(s, p + 3); incomment = 0
        } else {
          p = index(s, "<!--")
          if (p == 0) { out = out s; break }
          out = out substr(s, 1, p - 1)
          s = substr(s, p + 4); incomment = 1
        }
      }
      return out
    }
    {
      line = $0
      if (line ~ /^[[:space:]]*(```|~~~)/) { infence = !infence; next }
      ismarker = 0
      if (region != "") {
        if (index(line, region ":start") > 0) { inregion = 1; ismarker = 1 }
        else if (index(line, region ":end") > 0) { inregion = 0; ismarker = 1 }
      }
      txt = strip_html(line)
      if (ismarker) next
      if (region != "" && !inregion) next
      if (infence && txt ~ /^[[:space:]]*#/) next
      if (txt ~ /^[[:space:]]*$/) next
      printf "%d|%d|%s\n", NR, (infence ? 1 : 0), txt
    }
  ' "$1" 2>/dev/null
}

# first_decl FILE PATTERN -> the first line matching PATTERN that is not itself
# a // or * comment. Used for the TypeScript and PHP declarations, so a
# commented-out example above the real declaration cannot be read instead.
first_decl() {
  grep -nE "$2" "$1" 2>/dev/null |
    sed -E 's/^([0-9]+):/\1|/' |
    awk -F'|' '{ t=$0; sub(/^[0-9]+\|/, "", t); if (t !~ /^[[:space:]]*(\/\/|\*|#)/) { print; exit } }'
}

# ---------------------------------------------------------------------------
# 1. openapi.yaml info.version, against the top CHANGELOG entry, exactly.
# ---------------------------------------------------------------------------
check_openapi() {
  if [ ! -f "$F_OPENAPI" ]; then
    err "$F_OPENAPI does not exist, so the spec version cannot be checked."
    return
  fi
  spec="$(grep -m1 -E '^[[:space:]]+version:[[:space:]]*"?[0-9]' "$F_OPENAPI" 2>/dev/null |
    grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)"
  if [ -z "$spec" ]; then
    err "$F_OPENAPI declares no info.version that reads as MAJOR.MINOR.PATCH."
    detail "The spec version is the anchor the changelog is reconciled against; it cannot be absent."
    return
  fi
  if [ -z "$TOP" ]; then
    warn "$F_CHANGELOG lists no released version; $F_OPENAPI info.version is $spec but there is nothing to compare it to."
    return
  fi
  if [ "$spec" != "$TOP" ]; then
    err "$F_OPENAPI info.version is $spec but the top $F_CHANGELOG entry is $TOP."
    detail "Bump openapi.yaml info.version alongside the changelog."
    return
  fi
  ok "$F_OPENAPI info.version matches the top $F_CHANGELOG entry ($TOP)."
}

# ---------------------------------------------------------------------------
# 2. The marketing /changelog page, within 5 releases of the top entry.
#
# The newest entry may group several releases into one, written oldest first:
#   version: "0.61.49 - 0.61.53"
# Take the NEWEST version named in that entry, not the first one on the line.
# Reading the first number measures the distance from the older end, so a page
# that truthfully covers up to the top release reads as stale as soon as a
# group is wider than the tolerance. Measured on 2026-08-10, the widest group
# the page carries is version: "0.61.41 - 0.61.48": it names eight versions and
# its two ends sit six releases apart in CHANGELOG.md's list, which is past the
# tolerance of five, so reading the first number would have reddened an honest
# page.
# ---------------------------------------------------------------------------
check_marketing_changelog() {
  if [ ! -f "$F_MK_CHANGELOG" ]; then
    err "$F_MK_CHANGELOG does not exist, so the published changelog cannot be checked."
    return
  fi
  entry="$(grep -m1 -oE 'version: "[^"]+"' "$F_MK_CHANGELOG" 2>/dev/null || true)"
  if [ -z "$entry" ]; then
    err "$F_MK_CHANGELOG carries no version: \"...\" entry."
    detail "The page is the published release history; an empty one is a finding, not a parse failure."
    return
  fi
  newest=''
  for v in $(printf '%s\n' "$entry" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true); do
    if [ -z "$newest" ] || [ "$(ver_cmp "$v" "$newest")" = "1" ]; then newest="$v"; fi
  done
  if [ -z "$newest" ]; then
    err "$F_MK_CHANGELOG newest entry ($entry) names no MAJOR.MINOR.PATCH version."
    return
  fi
  compare_to_top "the marketing changelog's newest entry" "$newest" "$TOL_MARKETING_CHANGELOG" \
    "Backfill $F_MK_CHANGELOG (grouped entries are fine)." || true
}

# ---------------------------------------------------------------------------
# 3. The marketing hero badge, against the agent it names.
#
# AGENT_VERSION in apps/marketing/lib/content/home.ts drives the hero badge and
# the provenance fact row, both of which tell visitors which agent version
# wordpress.org serves. Its reference is the agent plugin, NOT the changelog.
#
# Comparing it to the changelog was wrong, and expensively so. The agent
# version deliberately does not track the repo release version: it moves only
# when the agent itself changes. 0.61.128 through 0.61.130 shipped between
# 2026-08-07 and 2026-08-10 with the agent frozen at 0.61.127, and through all
# of it a badge reading 0.61.127 told visitors the exact truth. Measured
# against the changelog it looked three releases stale and would have reddened
# main on an honest tree. A guard that cries wolf gets switched off.
#
# Against the agent version the rule is exact equality: there is nothing
# legitimate for a tolerance to forgive. The agent bump lands in the release
# commit, so the badge belongs in that same commit. At the 0.61.131 release
# commit the agent moved from 0.61.127 to 0.61.131 and the badge did not, which
# is the drift this check exists for.
#
# NOT DERIVED FROM apps/agent/wpmgr-agent.php AT BUILD TIME, deliberately.
# infra/Dockerfile.marketing copies only the workspace manifests, packages/ and
# apps/marketing/ into the build context, so a read outside those paths does not
# resolve in the image that ships. apps/marketing/scripts/sync-openapi.mjs
# already demonstrates the failure: its CHANGELOG.md read takes its catch on
# every production build, silently.
#
# THE PATTERN MUST SURVIVE A REFORMAT. The previous one required the line to
# begin with `const AGENT_VERSION`, so a TypeScript type annotation
# (const AGENT_VERSION: string = ...), `let`, or indentation all turned the
# check off with a WARN nobody reads, while the step comment claimed immunity
# to exactly that. A guard a formatter can silently disable guards nothing. So
# the anchor is the IDENTIFIER, wherever on the line it sits, and a file that
# declares it but names no version is an error rather than a skip.
# ---------------------------------------------------------------------------
read_agent_constant() {
  rec="$(first_decl "$F_AGENT_MAIN" "define[[:space:]]*\\([[:space:]]*['\"]WPMGR_AGENT_VERSION['\"]")"
  printf '%s' "$rec" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1
}

check_badge() {
  if [ ! -f "$F_HOME" ]; then
    err "$F_HOME does not exist, so the marketing hero badge cannot be checked."
    return
  fi
  if [ ! -f "$F_AGENT_MAIN" ]; then
    err "$F_AGENT_MAIN does not exist, so the badge has nothing to be checked against."
    return
  fi
  # Tolerates: export, const, let, var, an indent, a type annotation, any quote
  # style. Requires: the identifier AGENT_VERSION being assigned.
  decl="$(first_decl "$F_HOME" '(^|[^A-Za-z0-9_])AGENT_VERSION[[:space:]]*(:[^=]*)?=')"
  if [ -z "$decl" ]; then
    err "$F_HOME declares no AGENT_VERSION."
    detail "The hero badge and the provenance fact row both read it. A file that does not declare it renders no version at all."
    return
  fi
  badge="$(printf '%s' "$decl" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)"
  bline="${decl%%|*}"
  if [ -z "$badge" ]; then
    err "$F_HOME:$bline declares AGENT_VERSION but names no MAJOR.MINOR.PATCH version."
    detail "The badge renders whatever this is. It has to be a version literal the comparison can read."
    return
  fi
  agent="$(read_agent_constant)"
  if [ -z "$agent" ]; then
    err "$F_AGENT_MAIN declares no WPMGR_AGENT_VERSION naming a MAJOR.MINOR.PATCH version."
    detail "The plugin reads that constant at runtime; it cannot be absent or unreadable."
    return
  fi
  if [ "$badge" != "$agent" ]; then
    err "the marketing hero badge names agent $badge but $F_AGENT_MAIN is on $agent."
    detail "The badge and the provenance fact row tell visitors which agent version wordpress.org serves,"
    detail "so they must name the agent release this repo carries. Bump AGENT_VERSION in $F_HOME."
    return
  fi
  ok "the marketing hero badge names agent $badge, matching $F_AGENT_MAIN."
}

# ---------------------------------------------------------------------------
# 4. The agent version triple, and the release asset stamping.
#
# apps/agent/wpmgr-agent.php carries its version twice (the plugin header
# WordPress reads and the WPMGR_AGENT_VERSION constant the agent code reads)
# and apps/agent/readme.txt carries it a third time as the wordpress.org Stable
# tag. Nothing here compares them to the changelog, deliberately:
# `make agent-zip VERSION=` stamps a staged copy and never the source, so the
# checked-in values legitimately sit at the last released version rather than
# the one being prepared. What is never legitimate is the three disagreeing.
# Self-update compares the constant against the version the release manifest
# advertises while WordPress reads the header, so a mismatch produces an update
# that either never offers itself or offers itself forever, with no error
# message anywhere.
#
# The patterns tolerate a reformat for the same reason the badge one does. The
# WordPress coding standard writes define( 'X', 'y' ); with spaces inside the
# parentheses, which the previous anchored pattern did not match, so running a
# formatter over the agent would have switched this check off.
# ---------------------------------------------------------------------------
check_agent_triple() {
  if [ ! -f "$F_AGENT_MAIN" ] || [ ! -f "$F_AGENT_README" ]; then
    [ -f "$F_AGENT_MAIN" ] || err "$F_AGENT_MAIN does not exist."
    [ -f "$F_AGENT_README" ] || err "$F_AGENT_README does not exist."
    return
  fi
  header="$(grep -m1 -oE '^[[:space:]]*\*?[[:space:]]*Version:[[:space:]]*[0-9]+\.[0-9]+\.[0-9]+' "$F_AGENT_MAIN" 2>/dev/null |
    grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)"
  constant="$(read_agent_constant)"
  stable="$(grep -m1 -iE '^[[:space:]]*Stable tag:[[:space:]]*[0-9]+\.[0-9]+\.[0-9]+' "$F_AGENT_README" 2>/dev/null |
    grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)"

  missing=0
  [ -n "$header" ] || {
    err "$F_AGENT_MAIN carries no plugin-header Version: line naming MAJOR.MINOR.PATCH."
    missing=1
  }
  [ -n "$constant" ] || {
    err "$F_AGENT_MAIN declares no WPMGR_AGENT_VERSION naming MAJOR.MINOR.PATCH."
    missing=1
  }
  [ -n "$stable" ] || {
    err "$F_AGENT_README carries no Stable tag: line naming MAJOR.MINOR.PATCH."
    missing=1
  }
  if [ "$missing" -eq 1 ]; then
    detail "WordPress reads the header, the plugin reads the constant, wordpress.org reads the Stable tag. None of the three is optional."
    return
  fi
  if [ "$header" != "$constant" ] || [ "$header" != "$stable" ]; then
    err "the agent version disagrees with itself."
    detail "$F_AGENT_MAIN plugin header:       $header"
    detail "$F_AGENT_MAIN WPMGR_AGENT_VERSION: $constant"
    detail "$F_AGENT_README Stable tag:        $stable"
    detail "All three must name the same version, or self-update version comparison breaks silently."
    return
  fi
  ok "the agent version triple agrees ($header)."
}

# The agent plugin carries its own WPMGR_AGENT_VERSION, which changes only when
# the agent changes. release.yml fires on EVERY tag, including the many that
# touch only the control plane or the dashboard, so stamping the release asset
# with the tag published identical agent code under a different number each
# time. The object-storage channel reads the source version instead, so the two
# channels drifted apart, and because the agent's downgrade guard refuses
# anything not strictly newer, a site that installed a tag-stamped asset would
# refuse every later real release. Passing VERSION there is what caused that.
check_release_stamp() {
  if [ ! -f "$F_RELEASE_WF" ]; then
    warn "$F_RELEASE_WF does not exist; skipping the agent asset stamping check."
    return
  fi
  hits="$(grep -nE 'make[[:space:]]+agent-zip[^#]*VERSION=' "$F_RELEASE_WF" || true)"
  if [ -n "$hits" ]; then
    err "$F_RELEASE_WF passes VERSION to agent-zip."
    printf '%s\n' "$hits" | sed "s|^|  $F_RELEASE_WF:|"
    detail "The published asset would carry the git tag instead of WPMGR_AGENT_VERSION, which desynchronises the GitHub and"
    detail "object-storage channels and can strand sites permanently behind the agent's downgrade guard."
    return
  fi
  ok "$F_RELEASE_WF lets the agent zip carry its own source version."
}

# ---------------------------------------------------------------------------
# 5. The install pins a self-hoster copies verbatim.
#
# README.md links docs/install.md as the full install guide directly under its
# own pull block, so a reader crosses between them mid-install. Guarding only
# README.md is why docs/install.md was still handing people v0.19.0, 190
# releases behind, one link away from the block this guard was written to
# protect. They are checked as one set: present, in agreement, and within
# TOL_INSTALL_PINS of the top changelog entry.
#
# REQUIRED PIN VERSUS INCIDENTAL MENTION: THE REGION IS THE ANSWER.
# An earlier version pattern-guessed. It required an exact COUNT of every
# version-shaped match in the whole file, which made ordinary honest edits
# fail: appending an "## Upgrading" section with a second current pin exited 1,
# and a README sentence reading "**v0.61.130** was the prior release." exited 1
# with two errors, one of which claimed the pins disagreed when nothing was
# stale. A guard that reddens CI over true prose gets deleted.
#
# So a required pin is one that sits inside an explicitly marked region:
#
#   <!-- wpmgr-install-pins:start ... -->
#   ```bash
#   docker pull ghcr.io/mosamlife/wpmgr-api:v0.61.131
#   ```
#   <!-- wpmgr-install-pins:end -->
#
# Inside a region the count is a MINIMUM, so an extra current pin never fails.
# Outside a region nothing is required, and prose may name any version it
# likes. What stops a stale version hiding outside a region is the sweep below,
# which reads concrete image tags and WPMGR_VERSION values everywhere in the
# repo, declared or not.
#
# A file with a required pin and NO region at all is an error: the markers are
# how the file says where the copyable install steps are, and without them the
# required set silently becomes empty, which is the hole this replaced.
#
# ANCHOR VERSUS DECORATION, unchanged and still the point. The anchor is what
# the reader acts on: the variable name WPMGR_VERSION, the image path
# ghcr.io/mosamlife/wpmgr-api, the leading v on the tag. Decoration is anything
# a formatter may touch without changing what gets copied: quotes, backticks,
# an export keyword, spacing, indentation, a blockquote marker. The patterns
# require the first and ignore the second, so a reformatted pin is still READ,
# and a pin that is still read is a pin whose version is still COMPARED. That
# is why WPMGR_VERSION="v0.19.0" fails as a stale pin rather than passing as an
# absent one.
#
# A pin whose value is present but is not a release tag is an ERROR, not a
# miss. v0.61.131-rc1 is not a published tag and used to read as current
# because the version was extracted from the head of the string and the tail
# ignored. 0.61.131 without the leading v is not a tag that exists in GHCR and
# used to be reported as a MISSING pin, which sent the reader looking for a
# line that was right there.
#
# infra/docker-compose.prod.yml is deliberately not in this set. Its functional
# default, image: ...:${WPMGR_VERSION:-latest}, names no version and needs no
# guard.
#
# Fields are pipe separated: FILE|MIN|CONTEXT|DESCRIPTION|FIND|STRIP
#   CONTEXT is code (must sit in a fenced block, because that is what a reader
#   copies), text (must not), or any.
#   FIND is matched against the line with a space prepended, which is how the
#   patterns get a word boundary at line start without needing an alternation.
#   STRIP is a sed -E script that removes the anchor and leaves the value.
# ---------------------------------------------------------------------------
pin_specs() {
  cat <<'SPECS'
README.md|1|text|the release status line under the title|^[[:space:]>]*\*\*[[:space:]]*v?[0-9][0-9A-Za-z.+-]*[[:space:]]*\*\*|s/^[[:space:]>]*\*\*[[:space:]]*//; s/[[:space:]]*\*\*.*$//
README.md|1|code|the docker pull tag for the api image|ghcr\.io/mosamlife/wpmgr-api:[^[:space:]"'`,)]*|s/^.*wpmgr-api://
README.md|1|code|the docker pull tag for the web image|ghcr\.io/mosamlife/wpmgr-web:[^[:space:]"'`,)]*|s/^.*wpmgr-web://
README.md|1|code|the docker pull tag for the media-encoder image|ghcr\.io/mosamlife/wpmgr-media-encoder:[^[:space:]"'`,)]*|s/^.*wpmgr-media-encoder://
README.md|1|code|the WPMGR_VERSION export in the compose quick start|[^A-Za-z0-9_]WPMGR_VERSION[[:space:]]*=[[:space:]]*["'`]*[^[:space:]"'`#]*|s/^.*WPMGR_VERSION[[:space:]]*=[[:space:]]*["'`]*//
docs/install.md|1|code|the WPMGR_VERSION export in the install guide|[^A-Za-z0-9_]WPMGR_VERSION[[:space:]]*=[[:space:]]*["'`]*[^[:space:]"'`#]*|s/^.*WPMGR_VERSION[[:space:]]*=[[:space:]]*["'`]*//
SPECS
}

# The real pin loop. Kept separate from the spec list so the specs can live in
# a quoted heredoc (backticks and $ inside the patterns must stay literal).
run_install_pins() {
  pin_versions=''
  pin_where=''
  checked_region=''
  specs="$(pin_specs)"
  while IFS='|' read -r pfile pmin pctx pdesc pfind pstrip; do
    [ -n "${pfile:-}" ] || continue
    check_one_pin "$pfile" "$pmin" "$pctx" "$pdesc" "$pfind" "$pstrip"
  done <<EOF
$specs
EOF

  uniq_versions="$(printf '%s\n' "$pin_versions" | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -u || true)"
  n_uniq="$(printf '%s' "$uniq_versions" | grep -c . || true)"

  if [ "${n_uniq:-0}" -eq 0 ]; then
    err "not one install pin could be read from README.md or docs/install.md."
    detail "The install instructions are the copy a self-hoster runs verbatim. Unpinned, they hand out whatever :latest happens to be that day."
    return
  fi
  if [ "$n_uniq" -gt 1 ]; then
    err "the required install pins name more than one version:"
    printf '%s' "$pin_where" | sed 's|^|  |'
    detail "The GHCR pull tags, the WPMGR_VERSION exports and the README status line must all name the same release."
  fi
  for v in $uniq_versions; do
    compare_to_top "the install instructions" "$v" "$TOL_INSTALL_PINS" \
      "Update the ghcr.io pull tags, the WPMGR_VERSION exports and the README status line in README.md and docs/install.md." || true
  done
  if [ "$fail" -eq 0 ]; then
    ok "every required install pin is present, inside its marked region, and names v$uniq_versions."
  fi
}

check_one_pin() {
  pfile="$1"
  pmin="$2"
  pctx="$3"
  pdesc="$4"
  pfind="$5"
  pstrip="$6"

  if [ ! -f "$pfile" ]; then
    err "$pfile does not exist, so its required install pin ($pdesc) is missing."
    return
  fi

  case " $checked_region " in
    *" $pfile "*) : ;;
    *)
      checked_region="$checked_region $pfile"
      starts="$(grep -c "$PIN_MARKER:start" "$pfile" 2>/dev/null || true)"
      ends="$(grep -c "$PIN_MARKER:end" "$pfile" 2>/dev/null || true)"
      starts="${starts:-0}"
      ends="${ends:-0}"
      if [ "$starts" -eq 0 ]; then
        err "$pfile carries no <!-- $PIN_MARKER:start --> region."
        detail "The markers are how this file declares which lines a self-hoster copies. Without them nothing in it is required."
      elif [ "$starts" -ne "$ends" ]; then
        err "$pfile has $starts $PIN_MARKER:start markers and $ends $PIN_MARKER:end markers."
        detail "Unbalanced markers swallow the rest of the file or none of it. Close every region you open."
      fi
      ;;
  esac

  found=0
  actionable="$(md_actionable "$pfile" "$PIN_MARKER")"
  while IFS= read -r rec; do
    [ -n "$rec" ] || continue
    lno="${rec%%|*}"
    rest="${rec#*|}"
    infence="${rest%%|*}"
    text="${rest#*|}"
    case "$pctx" in
      code) [ "$infence" = "1" ] || continue ;;
      text) [ "$infence" = "0" ] || continue ;;
    esac
    hits="$(printf '%s\n' " $text" | grep -oE "$pfind" 2>/dev/null || true)"
    [ -n "$hits" ] || continue
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      found=$((found + 1))
      value="$(printf '%s\n' "$hit" | sed -E "$pstrip")"
      bare="${value#v}"
      if [ "$value" = "v$bare" ] && is_semver "$bare"; then
        pin_versions="$pin_versions$bare
"
        pin_where="$pin_where$pfile:$lno: $pdesc names v$bare
"
      elif is_semver "$value"; then
        err "$pfile:$lno names $value without the leading v ($pdesc)."
        detail "An image tag without the v does not exist in GHCR, and WPMGR_VERSION is substituted into one. Write v$value."
      else
        err "$pfile:$lno names \"$value\", which is not a released tag ($pdesc)."
        detail "A required pin has to read vMAJOR.MINOR.PATCH exactly. A suffix such as -rc1 is not a published release."
      fi
    done <<EOF
$hits
EOF
  done <<EOF
$actionable
EOF

  if [ "$found" -lt "$pmin" ]; then
    err "$pfile is missing a required install pin: $pdesc."
    detail "expected at least $pmin inside the $PIN_MARKER region, found $found. Self-hosters copy this line verbatim, so it has to be present AND current."
    detail "it must match: $pfind"
    detail "Decoration is tolerated (quotes, backticks, an export keyword, extra spacing, indentation). The name in the pattern and the leading v are not."
    detail "A commented-out copy does not count, inside an HTML comment or behind a # in a shell block: a reader cannot run either."
  fi
}

# ---------------------------------------------------------------------------
# The repo-wide sweep.
#
# The declared pin set is a whitelist, so a NEW file naming a stale tag was
# never checked by it at all. This reads every tracked file for the two things
# that hand a reader a concrete version (a wpmgr image tag and a WPMGR_VERSION
# value) and holds them to the same tolerance, whether anything declared them
# or not.
#
# Skipped: CHANGELOG.md, which is a historical record where every entry is
# meant to name the version it shipped in, and this script and its test, which
# carry the patterns as data. A single line elsewhere can opt out by carrying
# the word wpmgr-version-ignore, for the rare doc that must quote an old tag on
# purpose.
#
# Placeholders are not findings: ${WPMGR_VERSION:-latest}, :latest, vX.Y.Z and
# an empty assignment all pass through untouched. Only a concrete version is
# compared, and a concrete-looking value that is not a release tag is an error.
# ---------------------------------------------------------------------------
SWEEP_RE='ghcr\.io/mosamlife/wpmgr-[A-Za-z0-9_-]+:[^[:space:]"'"'"'`,)]+|WPMGR_VERSION[[:space:]]*=[[:space:]]*["'"'"'`]*[^[:space:]"'"'"'`#]*'

list_files() {
  out=''
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    out="$(git ls-files 2>/dev/null || true)"
  fi
  if [ -z "$out" ]; then
    out="$(find . \
      \( -name .git -o -name node_modules -o -name vendor -o -name dist -o -name .next -o -name build \) -prune -o \
      -type f -print 2>/dev/null | sed 's|^\./||' || true)"
  fi
  printf '%s\n' "$out"
}

sweep() {
  files="$(list_files |
    grep -v -e '^scripts/check-version-surfaces' -e '^CHANGELOG\.md$' || true)"
  [ -n "$files" ] || return 0

  hits="$(printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 grep -HInoE "$SWEEP_RE" 2>/dev/null || true)"
  [ -n "$hits" ] || return 0

  swept=0
  errors_before="$errors"
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    hfile="${hit%%:*}"
    hrest="${hit#*:}"
    hline="${hrest%%:*}"
    hmatch="${hrest#*:}"
    [ -f "$hfile" ] || continue

    # A line may opt out on purpose.
    if sed -n "${hline}p" "$hfile" 2>/dev/null | grep -q "$SWEEP_OPT_OUT"; then
      continue
    fi

    case "$hmatch" in
      *ghcr.io/mosamlife/wpmgr-*) value="${hmatch#*:}" ;;
      *) value="$(printf '%s\n' "$hmatch" | sed -E 's/^.*WPMGR_VERSION[[:space:]]*=[[:space:]]*["'"'"'`]*//')" ;;
    esac

    # Placeholders and variables are not version claims.
    case "$value" in
      '' | latest | *'$'* | *'{'* | *X.Y.Z*) continue ;;
    esac
    # Anything that does not look like it is trying to be a version is not one.
    printf '%s' "$value" | grep -qE '^v?[0-9]' || continue

    bare="${value#v}"
    if [ "$value" = "v$bare" ] && is_semver "$bare"; then
      swept=$((swept + 1))
      # Quiet on success: this runs over every tracked file, and a per-mention
      # OK line would bury the checks above in noise.
      if [ -n "$TOP" ]; then
        if [ "$(ver_cmp "$bare" "$TOP")" = "1" ]; then
          err "$hfile:$hline names v$bare, which is newer than the top $F_CHANGELOG entry ($TOP)."
          detail "  the line reads: $hmatch"
          detail "Nothing may hand a reader a tag that was never released."
        else
          behind="$(releases_behind "$bare")"
          if [ "$behind" -gt "$TOL_SWEEP" ]; then
            err "$hfile:$hline still names v$bare, $behind $(rel_word "$behind") behind the top $F_CHANGELOG entry ($TOP). Tolerance is $TOL_SWEEP."
            detail "  the line reads: $hmatch"
            detail "Bump it, or mark the line $SWEEP_OPT_OUT if it must name an old release on purpose."
          fi
        fi
      fi
    elif is_semver "$value"; then
      err "$hfile:$hline names $value without the leading v ($hmatch)."
      detail "An image tag without the v does not exist in GHCR. Write v$value, or mark the line $SWEEP_OPT_OUT."
    else
      err "$hfile:$hline names \"$value\", which is not a released tag ($hmatch)."
      detail "A concrete tag has to read vMAJOR.MINOR.PATCH. A suffix such as -rc1 is not a published release."
    fi
  done <<EOF
$hits
EOF
  if [ "$errors" -eq "$errors_before" ] && [ -n "$TOP" ]; then
    ok "swept the tree: $swept concrete version mentions outside $F_CHANGELOG, all within $TOL_SWEEP release of $TOP."
  fi
  return 0
}

# ---------------------------------------------------------------------------
main() {
  if [ -z "$TOP" ]; then
    warn "could not parse any released version from $F_CHANGELOG. Every compare that needs the release ordering will be skipped; every other check still runs."
  fi
  check_openapi
  check_marketing_changelog
  check_badge
  check_agent_triple
  check_release_stamp
  run_install_pins
  sweep

  if [ "$fail" -ne 0 ]; then
    printf '\n%s\n' "Version surface check FAILED. See docs/process/docs-changelog-sop.md section 5 for what each surface is and how to bump it."
    return 1
  fi
  printf '\n%s\n' "Version surfaces are consistent."
  return 0
}

main
exit "$?"
