#!/usr/bin/env bash
# scripts/release-agent_test.sh
#
# The regression suite for scripts/release-agent.sh.
#
# WHY THIS EXISTS (GH #515). `tested` in the published manifest used to be
# `${WPMGR_AGENT_TESTED:-6.8}`. Nobody ever set the override, so every release
# published a WordPress compatibility floor of 6.8 regardless of what
# apps/agent/readme.txt actually declared. The fix makes the default the
# value parsed from readme.txt's own "Tested up to:" line, and turns a
# missing file or an unparseable line into a hard failure instead of a new
# silent default. `requires` and `requires_php` had the same defect in their
# error path (`|| echo '6.0'` / `|| echo '8.1'`); they are fixed the same way.
# This suite is the proof that survives after the terminal scrollback does.
#
# HOW IT WORKS. release-agent.sh derives its own repo root from its own
# script path (dirname "$0"/..), not from an argument or an env var, so each
# case builds a full miniature repo under a scratch dir — scripts/,
# apps/agent/readme.txt, release/wpmgr-agent.zip — and runs a COPY of the
# real script placed at <fixture>/scripts/release-agent.sh, so its repo-root
# detection lands on the fixture instead of this checkout. No mocking: the
# real script reads real files and a real zip built with the real `zip`.
#
# RUN IT:
#   scripts/release-agent_test.sh            # everything
#   scripts/release-agent_test.sh tested     # only cases matching "tested"
#
# Point it at a different implementation to prove the suite is not vacuous:
#   WPMGR_RELEASE_AGENT_SCRIPT=/tmp/guard-with-hole.sh \
#     scripts/release-agent_test.sh

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_RELEASE_AGENT_SCRIPT:-$HERE/release-agent.sh}"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  echo "no script at $GUARD" >&2
  exit 2
fi
command -v zip >/dev/null 2>&1 || { echo "zip is required to build fixture packages" >&2; exit 2; }
command -v unzip >/dev/null 2>&1 || { echo "unzip is required (release-agent.sh depends on it too)" >&2; exit 2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-release-agent.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
SKIPPED=0
FAILED_NAMES=''

# ---------------------------------------------------------------------------
# Fixture construction
# ---------------------------------------------------------------------------

# write_plugin_php DIR VERSION_LINE REQUIRES_LINE REQUIRES_PHP_LINE
#   Each *_LINE is the literal header line, or "" to omit it entirely, so a
#   case can simulate a plugin header that is missing one field.
#   VERSION_LINE additionally controls the WPMGR_AGENT_VERSION define; pass
#   "" to omit both the header Version line and the define.
write_plugin_php() {
  _dir="$1"; _version="$2"; _requires="$3"; _requires_php="$4"
  mkdir -p "$_dir/wpmgr-agent"
  {
    echo '<?php'
    echo '/**'
    echo ' * Plugin Name: WPMgr Agent'
    [ -n "$_version" ] && echo " * Version:     $_version"
    [ -n "$_requires" ] && echo " * Requires at least: $_requires"
    [ -n "$_requires_php" ] && echo " * Requires PHP:      $_requires_php"
    echo ' */'
    echo ''
    echo "if (!defined('ABSPATH')) { exit; }"
    if [ -n "$_version" ]; then
      echo ""
      echo "define('WPMGR_AGENT_VERSION', '$_version');"
    fi
  } >"$_dir/wpmgr-agent/wpmgr-agent.php"
}

# build_zip DIR -> zips $_dir/wpmgr-agent/ into $_dir/release/wpmgr-agent.zip
# with the mandatory "wpmgr-agent/" top-level entry.
build_zip() {
  _dir="$1"
  mkdir -p "$_dir/release"
  rm -f "$_dir/release/wpmgr-agent.zip"
  ( cd "$_dir" && zip -X -q -r release/wpmgr-agent.zip wpmgr-agent )
}

# write_readme FILE -> writes a readme.txt declaring "Tested up to: 7.1" to FILE.
write_readme() {
  _file="$1"
  {
    echo '=== WPMgr Agent ==='
    echo 'Requires at least: 6.2'
    echo 'Tested up to: 7.1'
    echo 'Requires PHP: 8.1'
    echo 'Stable tag: 1.2.3'
  } >"$_file"
}

# fixture NAME -> path to a fresh scratch repo root with:
#   scripts/release-agent.sh   (a copy of $GUARD, so repo-root detection
#                                lands here, not on this checkout)
#   apps/agent/readme.txt      (the WORKING-TREE copy; default: "Tested up to: 7.1")
#   wpmgr-agent/readme.txt     (the ARCHIVE copy that actually gets zipped —
#                                release-agent.sh reads THIS one, mirroring
#                                `make agent-zip`'s rsync of apps/agent/ into
#                                release/wpmgr-agent/. Starts identical to the
#                                working-tree copy; a case that wants to prove
#                                the two may diverge edits only one of them.)
#   release/wpmgr-agent.zip    (default: version 1.2.3, requires 6.2/8.1)
fixture() {
  _t="$WORK/$1"
  rm -rf "$_t"
  mkdir -p "$_t/scripts" "$_t/apps/agent"
  cp "$GUARD" "$_t/scripts/release-agent.sh"
  chmod +x "$_t/scripts/release-agent.sh"
  write_readme "$_t/apps/agent/readme.txt"
  write_plugin_php "$_t" "1.2.3" "6.2" "8.1"
  cp "$_t/apps/agent/readme.txt" "$_t/wpmgr-agent/readme.txt"
  build_zip "$_t"
  printf '%s' "$_t"
}

sub() { # sub FILE SED-EXPR
  sed -E "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# run NAME pass|fail FIXTURE_DIR [ENV=VAL ...] -- [+MUST-CONTAIN ...] [-MUST-NOT-CONTAIN ...]
#
# Runs the fixture's own copy of the script with --dry-run against
# <fixture>/out/latest.json. On a `pass` case the manifest must exist; on a
# `fail` case it must NOT exist (a refusal must not leave a half-written
# manifest behind).
# ---------------------------------------------------------------------------
run_case() {
  _name="$1"; _want="$2"; _dir="$3"; shift 3

  if [ -n "$FILTER" ]; then
    case "$_name" in
      *"$FILTER"*) : ;;
      *) SKIPPED=$((SKIPPED + 1)); return 0 ;;
    esac
  fi

  _envs=()
  while [ "$#" -gt 0 ] && [ "$1" != '--' ]; do
    _envs+=("$1")
    shift
  done
  [ "$#" -gt 0 ] && shift # drop the --

  _out="$_dir/out/latest.json"
  rm -rf "$_dir/out"
  # "${_envs[@]+"${_envs[@]}"}" (not the plain "${_envs[@]}") because bash 3.2
  # — what macOS ships, and what this suite must run under — treats expanding
  # an EMPTY array under `set -u` as an unbound-variable error.
  _result="$(env "${_envs[@]+"${_envs[@]}"}" "$_dir/scripts/release-agent.sh" --dry-run --out "$_out" 2>&1)"
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
  if [ "$_want" = pass ] && [ ! -f "$_out" ]; then
    _problems="$_problems
    expected a manifest to be written at $_out, found none"
  fi
  if [ "$_want" = fail ] && [ -f "$_out" ]; then
    _problems="$_problems
    expected NO manifest on a refusal, but $_out exists"
  fi

  for _a in "$@"; do
    case "$_a" in
      +*)
        _needle="${_a#+}"
        if ! printf '%s\n' "$_result" | grep -qF "$_needle"; then
          _problems="$_problems
    expected the output to contain: $_needle"
        fi
        ;;
      -*)
        _needle="${_a#-}"
        if printf '%s\n' "$_result" | grep -qF "$_needle"; then
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
    printf '%s\n' "$_result" | sed 's/^/      | /'
  else
    PASSED=$((PASSED + 1))
    printf 'ok   %s\n' "$_name"
  fi
}

# ===========================================================================
# GH #515: the core defect and its fix.
# ===========================================================================
t="$(fixture honest-tested-parsed-from-readme)"
run_case "honest: tested is parsed from readme.txt's Tested up to line, not defaulted" \
  pass "$t" -- '+"tested": "7.1"' -'"tested": "6.8"'

t="$(fixture tested-override-wins-and-skips-readme)"
rm -f "$t/apps/agent/readme.txt" "$t/wpmgr-agent/readme.txt" # prove the override needs no readme at all, in the archive or the working tree
build_zip "$t"
run_case "tested: an explicit WPMGR_AGENT_TESTED override wins and never touches readme.txt" \
  pass "$t" "WPMGR_AGENT_TESTED=6.5" -- '+"tested": "6.5"'

t="$(fixture tested-readme-missing-fails-loud)"
rm -f "$t/wpmgr-agent/readme.txt" # the ARCHIVE copy is what release-agent.sh reads
build_zip "$t"
run_case "tested: a missing readme.txt in the archive with no override is a hard error, not a fallback" \
  fail "$t" -- '+missing' '+readme.txt' '+refusing to publish a guessed'

t="$(fixture tested-line-absent-fails-loud)"
sub "$t/wpmgr-agent/readme.txt" '/^Tested up to:/d'
build_zip "$t"
run_case "tested: an archive readme.txt with no Tested up to line is a hard error" \
  fail "$t" -- "+could not parse a 'Tested up to:'"

t="$(fixture tested-line-unparseable-fails-loud)"
sub "$t/wpmgr-agent/readme.txt" 's/^Tested up to: .*/Tested up to: latest/'
build_zip "$t"
run_case "tested: a non-numeric Tested up to value is a hard error, not silently coerced" \
  fail "$t" -- "+could not parse a 'Tested up to:'"

t="$(fixture tested-tolerates-case-and-spacing)"
sub "$t/wpmgr-agent/readme.txt" 's/^Tested up to: 7\.1/tested up to:   7.2   /'
build_zip "$t"
run_case "tested: label case and extra whitespace do not disable the parse (no over-fire)" \
  pass "$t" -- '+"tested": "7.2"'

t="$(fixture tested-old-default-never-appears)"
sub "$t/wpmgr-agent/readme.txt" 's/^Tested up to: 7\.1/Tested up to: 6.3/'
build_zip "$t"
run_case "regress: the old 6.8 literal cannot reappear when the archive says something else" \
  pass "$t" -- '+"tested": "6.3"' -'"tested": "6.8"'

# ===========================================================================
# GH #515 follow-up (this fix): the manifest must describe the ARCHIVE, not
# the working tree it happened to be published from. version/requires/
# requires_php already read the zip's own plugin file; tested now reads the
# zip's own readme.txt the same way. Prove the working tree can diverge from
# the archive WITHOUT moving the manifest.
# ===========================================================================
t="$(fixture tested-archive-is-source-not-working-tree)"
sub "$t/apps/agent/readme.txt" 's/^Tested up to: 7\.1/Tested up to: 9.9/' # working tree only; archive untouched, zip NOT rebuilt
run_case "tested: a working-tree readme.txt that disagrees with the archive is ignored" \
  pass "$t" -- '+"tested": "7.1"' -'"tested": "9.9"'

# ===========================================================================
# Siblings found during the audit: requires / requires_php / version had the
# same "silent fallback on parse failure" shape in their error path.
# ===========================================================================
t="$(fixture requires-missing-fails-loud)"
write_plugin_php "$t" "1.2.3" "" "8.1"
build_zip "$t"
run_case "requires: a plugin header missing 'Requires at least' is a hard error, not 6.0" \
  fail "$t" -- "+could not parse 'Requires at least'"

t="$(fixture requires-php-missing-fails-loud)"
write_plugin_php "$t" "1.2.3" "6.2" ""
build_zip "$t"
run_case "requires_php: a plugin header missing 'Requires PHP' is a hard error, not 8.1" \
  fail "$t" -- "+could not parse 'Requires PHP'"

t="$(fixture version-missing-fails-loud-with-message)"
write_plugin_php "$t" "" "6.2" "8.1"
build_zip "$t"
run_case "version: a missing WPMGR_AGENT_VERSION define fails WITH the die message (not a bare pipefail exit)" \
  fail "$t" -- "+could not parse WPMGR_AGENT_VERSION"

# ===========================================================================
# Honest trees: the fix must not over-fire on a normal release.
# ===========================================================================
t="$(fixture honest-complete-manifest)"
run_case "honest: a normal release produces every manifest field" \
  pass "$t" -- \
  '+"slug": "wpmgr-agent"' \
  '+"plugin": "wpmgr-agent/wpmgr-agent.php"' \
  '+"version": "1.2.3"' \
  '+"min_version": "0.0.0"' \
  '+"requires": "6.2"' \
  '+"requires_php": "8.1"' \
  '+"tested": "7.1"' \
  '+"sections"' \
  '+"description"'

t="$(fixture honest-min-version-override-still-works)"
run_case "honest: WPMGR_AGENT_MIN_VERSION still overrides independently of the tested fix" \
  pass "$t" "WPMGR_AGENT_MIN_VERSION=0.55.0" -- '+"min_version": "0.55.0"'

t="$(fixture honest-missing-zip-still-an-error)"
rm -f "$t/release/wpmgr-agent.zip"
run_case "honest: the pre-existing missing-zip guard still fires" \
  fail "$t" -- "+run 'make agent-zip' first"

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
