#!/usr/bin/env bash
# scripts/check-license-surfaces.sh
#
# GH #547: the agent plugin declared MIT in its main header and GPLv2 or later
# in the wordpress.org readme.txt, live, at the same time. Nothing checked
# that the plugin's several license self-declarations agreed with each other,
# so the wp.org listing quietly contradicted the plugin header, the repo's
# own LICENSE-AGENT carve-out, and the third-party NOTICE for over a release.
#
# The owner's ruling: the agent is MIT. This guard does not encode that ruling
# as a hard-coded expectation — it encodes AGREEMENT, so it fails just as hard
# if some future edit moves every surface to a different license but misses
# one, which is the failure mode #547 actually was.
#
# WHAT IT CHECKS. Every place in apps/agent (plus the repo-root LICENSE-AGENT
# carve-out file) that names the agent's own license, by name:
#
#   1. apps/agent/wpmgr-agent.php           - the WordPress plugin header
#   2. apps/agent/mu-plugin-loader/a-wpmgr-error-trap.php      - its own header
#   3. apps/agent/mu-plugin-loader/a-wpmgr-update-watchdog.php - its own header
#   4. apps/agent/readme.txt                - the wp.org listing header
#   5. apps/agent/readme.txt                - the Description section's prose
#   6. apps/agent/composer.json             - the "license" field
#   7. apps/agent/NOTICE.md                 - "The WPMgr agent is ...-licensed"
#   8. apps/agent/README.md                 - "...-licensed WordPress plugin"
#   9. LICENSE-AGENT                        - the license-text title line
#  10. Makefile                             - the wp.org build must not
#                                              override the header at all
#
# #10 exists because of how this exact bug shipped. Fixing readme.txt to MIT
# is not sufficient on its own: Makefile's agent-zip-wporg target used to
# carry a `sed` line that rewrote the STAGED plugin header's License field to
# "GPLv2 or later" on every wp.org build, regardless of what the source
# declared. Every check above reads the committed source tree, which agreed
# on MIT throughout and would have stayed green, while the actual zip
# uploaded to wordpress.org kept shipping a header that disagreed with
# readme.txt - the literal defect #547 was filed against, reintroduced by the
# build instead of the source. `make agent-plugincheck` (the authoritative
# gate, real WordPress via Docker) is what caught it; this check is what
# keeps it caught without a Docker run.
#
# NOT checked, deliberately: the matthiasmullie/minify third-party attribution
# in readme.txt (a dependency's license, not the plugin's), and the lilliput
# and web-vitals third-party mentions in NOTICE.md. Each pattern below is
# anchored to the sentence shape THIS repo uses for the plugin's own
# declaration, specifically so it cannot match a neighbouring third-party
# credit that happens to say "MIT" too. See check-license-surfaces_test.sh's
# "over-fire" cases for the proof.
#
# THE RULE THAT MATTERS MOST: A SURFACE THAT NAMES NOTHING IS AN ERROR, NEVER
# A SKIP. A check whose pattern matches zero times because a header was
# deleted, reworded, or moved out from under the pattern must go red, not
# print nothing and exit 0 — a guard that passes because its own pattern found
# nothing is the exact defect #547 was one level up (readme.txt's own License
# header was RIGHT THERE and simply never compared to anything).
#
# RUN IT:
#   make check-licenses          or  scripts/check-license-surfaces.sh
#   make check-licenses-test     or  scripts/check-license-surfaces_test.sh
#   scripts/check-license-surfaces.sh /path/to/some/other/tree
#
# Exit code is 0 when every surface is present and they all agree, 1 when any
# surface is missing or any two disagree.

set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/check-license-surfaces.sh [ROOT]

ROOT defaults to the repository this script lives in, or
$WPMGR_LICENSE_SURFACE_ROOT when that is set.

Exit 0: every license surface is present and they all agree.
Exit 1: a surface is missing its declaration, or two surfaces disagree.
USAGE
}

case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${1:-${WPMGR_LICENSE_SURFACE_ROOT:-$(cd "$SCRIPT_DIR/.." && pwd)}}"

if [ ! -d "$ROOT" ]; then
  echo "ERROR: $ROOT is not a directory." >&2
  exit 2
fi
cd "$ROOT" || exit 2

fail=0
errors=0
err() {
  printf 'ERROR: %s\n' "$1"
  fail=1
  errors=$((errors + 1))
}
detail() { printf '  %s\n' "$1"; }
ok() { printf 'OK: %s\n' "$1"; }

# ---------------------------------------------------------------------------
# normalize LICENSE-STRING -> canonical comparison token.
#
# Strips only the decorative suffixes this repo's surfaces are actually
# written with ("-licensed", " License"), and nothing else. It must NOT strip
# a substantive middle token: "GPLv2" and "GPLv2 or later" are different
# licenses, so "or later" is never touched. The point of normalizing at all is
# so "MIT", "MIT-licensed" and "MIT License" compare equal, not so any two
# different licenses are made to look the same.
# ---------------------------------------------------------------------------
normalize() {
  printf '%s' "$1" \
    | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' \
    | sed -E 's/-[Ll]icensed$//' \
    | sed -E 's/[[:space:]][Ll]icense$//'
}

# The declarations found so far, one per line: NAME|FILE|RAW|NORMALIZED.
# Only a surface that yielded a value is added here; an absent one is an ERROR
# on the spot and contributes nothing to the agreement check below, exactly
# like check-version-surfaces.sh's "missing" branch does not join its compare.
SURFACES=''

# The License URI declarations found so far, one per line: LABEL|FILE|URI.
# Same rule as SURFACES: only a surface that passed its own checks joins this,
# so an absent or self-inconsistent URI contributes nothing to the
# cross-surface agreement check below.
URI_SURFACES=''

# add_surface NAME FILE RAW -> records RAW, or errors if RAW is empty.
add_surface() {
  _name="$1"
  _file="$2"
  _raw="$3"
  if [ -z "$_raw" ]; then
    err "$_file declares no license ($_name)."
    detail "A missing declaration is not a pass. Either restore it, or this script's pattern for $_name no longer matches the real text — fix whichever is wrong."
    return
  fi
  _norm="$(normalize "$_raw")"
  SURFACES="$SURFACES$_name|$_file|$_raw|$_norm
"
  ok "$_name ($_file): $_raw"
}

# ---------------------------------------------------------------------------
# 1-3. Plugin-style headers. wpmgr-agent.php and the two mu-plugin loaders
# each carry their own "* License:  X" docblock line. Anchored on "License:"
# with the colon immediately after the word, so "License URI:" (present on
# the same block) never matches — after "License" there is a space then "URI"
# then the colon, not a colon directly, so the substring "License:" does not
# occur in that line at all.
#
# The whitespace after the colon is [[:space:]]* (zero or more), not one or
# more. WordPress core's own header parser (get_file_data) does not require a
# space either — it matches the field name, the colon, and captures the rest
# of the line verbatim before trimming — so requiring a space here would be a
# stricter reformat-survival bar than WordPress itself enforces, and a
# formatter that collapsed "License:           MIT" to "License:MIT" would
# still be read correctly by WordPress while silently switching this guard
# off. It must not.
# ---------------------------------------------------------------------------
check_php_header() {
  _label="$1"
  _f="$2"
  if [ ! -f "$_f" ]; then
    err "$_f does not exist."
    return
  fi
  _v="$(grep -m1 -E '^[[:space:]]*\*?[[:space:]]*License:' "$_f" 2>/dev/null |
    sed -E 's/^[[:space:]]*\*?[[:space:]]*License:[[:space:]]*//')"
  add_surface "$_label" "$_f" "$_v"
}

# ---------------------------------------------------------------------------
# 4. readme.txt's wp.org listing header. Same anchor, but readme.txt's header
# lines start at column 0 with no leading "*", so the pattern is a plain
# line-start match. grep -m1 takes the FIRST such line in the file, which is
# the header — the only other "license" text in readme.txt (the
# matthiasmullie/minify attribution) never starts a line with "License:".
# ---------------------------------------------------------------------------
check_readme_header() {
  _f='apps/agent/readme.txt'
  if [ ! -f "$_f" ]; then
    err "$_f does not exist."
    return
  fi
  _v="$(grep -m1 -E '^License:' "$_f" 2>/dev/null | sed -E 's/^License:[[:space:]]*//')"
  add_surface "the readme.txt wp.org header" "$_f" "$_v"
}

# ---------------------------------------------------------------------------
# 5. readme.txt's Description section carries its own prose sentence,
# "This plugin is X and the dashboard is Y.", read by a human on the wp.org
# listing page independently of the structured header above. Anchored on the
# literal "This plugin is ... and the dashboard is" phrase, which appears
# nowhere else in the file (the third-party section never uses this
# sentence), so it cannot pick up the minify attribution's "Licensed under
# the MIT License." sentence, which does not start with "This plugin is".
# ---------------------------------------------------------------------------
check_readme_prose() {
  _f='apps/agent/readme.txt'
  if [ ! -f "$_f" ]; then
    err "$_f does not exist."
    return
  fi
  _v="$(grep -m1 -oE 'This plugin is .+ and the dashboard is' "$_f" 2>/dev/null |
    sed -E 's/^This plugin is //; s/ and the dashboard is$//')"
  add_surface "the readme.txt Description prose" "$_f" "$_v"
}

# ---------------------------------------------------------------------------
# License URI companions.
#
# GH #547's reviewer found the next way this same bug reopens: a "License:"
# name can agree everywhere while the "License URI:" beside it still points
# at the wrong license. "License: MIT" paired with a GPL URL is not an
# improvement on an outright name mismatch -- it is the identical defect one
# field over, and a checker that only reads the name would call it clean.
#
# Two surfaces carry an explicit "License URI:" line: the plugin header and
# readme.txt's own header (the two mu-plugin loaders and readme.txt's
# Description prose never declare a URI at all, and are not asked to).
#
# Each is checked on two axes, both required:
#   SELF-CONSISTENCY  its URI must match what THAT surface's own "License:"
#                      name implies, read via expected_uri_for(). This is the
#                      half-right case above, caught even if nothing else on
#                      earth ever reads a second URI.
#   CROSS-SURFACE      its URI must equal the other URI-bearing surface's,
#                      the same agreement rule every name-only surface above
#                      is already held to.
# ---------------------------------------------------------------------------

# expected_uri_for NAME -> the canonical URI for a license this repo has
# actually declared, or empty for anything else. Empty is not a failure: it
# means self-consistency cannot be judged for an unrecognized name, so only
# the cross-surface compare still applies to that surface's URI.
expected_uri_for() {
  case "$1" in
    MIT) printf 'https://opensource.org/licenses/MIT' ;;
    'GPLv2 or later') printf 'https://www.gnu.org/licenses/gpl-2.0.html' ;;
    *) printf '' ;;
  esac
}

# check_license_uri_pair LABEL FILE NAME URI
check_license_uri_pair() {
  _label="$1"
  _file="$2"
  _name="$3"
  _uri="$4"

  # An absent NAME was already reported by this file's own license-name
  # check above (check_php_header / check_readme_header); a second "missing
  # license" error here about the same absence would only be confusing.
  [ -n "$_name" ] || return

  if [ -z "$_uri" ]; then
    err "$_file declares a License but no License URI ($_label)."
    detail "WordPress core and wordpress.org both read this field. A header or readme with a License and no License URI is incomplete, and one that used to carry a URI and silently lost it is exactly the kind of drift this guard exists to catch."
    return
  fi

  _expected="$(expected_uri_for "$_name")"
  if [ -n "$_expected" ] && [ "$_uri" != "$_expected" ]; then
    err "$_label's License URI does not match its own declared license."
    detail "$_file says \"License: $_name\" but \"License URI: $_uri\"."
    detail "Expected $_expected for $_name. A URI that disagrees with the name right beside it is the half-right state GH #547 started as -- right name, wrong link (or vice versa)."
    return
  fi

  URI_SURFACES="$URI_SURFACES$_label|$_file|$_uri
"
  ok "$_label License URI ($_file): $_uri"
}

check_main_header_license_uri() {
  _f='apps/agent/wpmgr-agent.php'
  if [ ! -f "$_f" ]; then
    return # already reported by check_php_header
  fi
  _name="$(grep -m1 -E '^[[:space:]]*\*?[[:space:]]*License:' "$_f" 2>/dev/null |
    sed -E 's/^[[:space:]]*\*?[[:space:]]*License:[[:space:]]*//')"
  _uri="$(grep -m1 -E '^[[:space:]]*\*?[[:space:]]*License URI:' "$_f" 2>/dev/null |
    sed -E 's/^[[:space:]]*\*?[[:space:]]*License URI:[[:space:]]*//')"
  check_license_uri_pair "the main plugin header" "$_f" "$_name" "$_uri"
}

check_readme_header_license_uri() {
  _f='apps/agent/readme.txt'
  if [ ! -f "$_f" ]; then
    return # already reported by check_readme_header
  fi
  _name="$(grep -m1 -E '^License:' "$_f" 2>/dev/null | sed -E 's/^License:[[:space:]]*//')"
  _uri="$(grep -m1 -E '^License URI:' "$_f" 2>/dev/null | sed -E 's/^License URI:[[:space:]]*//')"
  check_license_uri_pair "the readme.txt wp.org header" "$_f" "$_name" "$_uri"
}

# ---------------------------------------------------------------------------
# 6. composer.json's own "license" field, read as JSON-ish text (no jq
# dependency, matching the rest of this repo's guards).
# ---------------------------------------------------------------------------
check_composer() {
  _f='apps/agent/composer.json'
  if [ ! -f "$_f" ]; then
    err "$_f does not exist."
    return
  fi
  _v="$(grep -m1 -oE '"license"[[:space:]]*:[[:space:]]*"[^"]+"' "$_f" 2>/dev/null |
    sed -E 's/^.*:[[:space:]]*"//; s/"$//')"
  add_surface "composer.json" "$_f" "$_v"
}

# ---------------------------------------------------------------------------
# 7. NOTICE.md's own prose declaration. Anchored on the literal subject "The
# WPMgr agent is", which is what this repo's NOTICE.md uses for its own
# license claim. NOTICE.md also carries third-party attributions (lilliput,
# matthiasmullie/minify) that say "is MIT-licensed" about THEMSELVES, not
# about the WPMgr agent, so a looser "is X-licensed" pattern would double-read
# this file; the anchor on the subject is what keeps this to the one sentence
# that is actually the plugin's own claim.
# ---------------------------------------------------------------------------
check_notice() {
  _f='apps/agent/NOTICE.md'
  if [ ! -f "$_f" ]; then
    err "$_f does not exist."
    return
  fi
  _v="$(grep -m1 -oE 'The WPMgr agent is [A-Za-z0-9.+-]+-licensed' "$_f" 2>/dev/null |
    sed -E 's/^The WPMgr agent is //')"
  add_surface "NOTICE.md" "$_f" "$_v"
}

# ---------------------------------------------------------------------------
# 8. apps/agent/README.md's opening line. Anchored at the start of the file's
# first real line ("X-licensed WordPress plugin ..."), which is this repo's
# one-line description of what the plugin is and under what terms.
# ---------------------------------------------------------------------------
check_agent_readme_md() {
  _f='apps/agent/README.md'
  if [ ! -f "$_f" ]; then
    err "$_f does not exist."
    return
  fi
  _v="$(grep -m1 -oE '^[A-Za-z0-9.+-]+-licensed WordPress plugin' "$_f" 2>/dev/null |
    sed -E 's/-licensed WordPress plugin$//')"
  add_surface "README.md" "$_f" "$_v"
}

# ---------------------------------------------------------------------------
# 9. LICENSE-AGENT, the repo-root file that carves apps/agent (and
# apps/tracker) out of the repository's AGPL default. Standard license texts
# open with a title line naming themselves ("MIT License", "Apache License,
# Version 2.0", ...); this reads that title line and requires it look like
# one, so a title-less or emptied file is caught as absent rather than
# silently comparing "" to nothing.
# ---------------------------------------------------------------------------
check_license_agent() {
  _f='LICENSE-AGENT'
  if [ ! -f "$_f" ]; then
    err "$_f does not exist."
    return
  fi
  _v="$(sed -n '1p' "$_f" 2>/dev/null | grep -E '^[A-Za-z0-9.(), -]+ License[[:space:]]*$' | sed -E 's/[[:space:]]+$//' || true)"
  add_surface "LICENSE-AGENT" "$_f" "$_v"
}

# ---------------------------------------------------------------------------
# 10. Makefile's wp.org build must not carry its own hard-coded rewrite of the
# plugin header's License field. This is not a "declares X" surface like the
# nine above it -- there is nothing legitimate for a packaging step to say
# here, so the only correct state is ABSENT, and its presence is itself the
# finding, independent of what value it hard-codes. A rewrite line always
# contains the literal field name "License:" twice on one line: once as the
# sed match (the field it targets) and once inside the literal replacement
# text (the value it forces), which is what the pattern below keys on. It
# does not fire on the plain-English Makefile comment describing the target,
# because a comment states "License" and a value only once, not twice, and
# never inside a sed s/// replacement.
# ---------------------------------------------------------------------------
#
# grep's exit code is read explicitly, not swallowed into "no match" the way
# add_surface's callers do (there is nothing to normalize into a license value
# here). grep exits 1 for a clean "no match", which is the PASS case for this
# check, and 0 when it found a hit. Anything else (2+) means the pattern
# itself failed to run at all -- on this exact check, in an earlier revision,
# an invalid empty-alternation group made both BSD grep and the ugrep-based
# wrapper this repo's own shell uses error out, and a trailing `2>/dev/null ||
# true` on the capture made that failure read as "found nothing", which
# reported OK forever regardless of what the Makefile said. That is the same
# defect this whole guard exists to catch, one level deeper: never let a
# broken check's own tool error look like a clean pass.
# ---------------------------------------------------------------------------
check_makefile_no_license_override() {
  _f='Makefile'
  if [ ! -f "$_f" ]; then
    err "$_f does not exist."
    return
  fi
  _hit="$(grep -nE '\*[[:space:]]*(License|License URI):.*\*[[:space:]]*(License|License URI):' "$_f")"
  _rc=$?
  if [ "$_rc" -eq 0 ]; then
    err "$_f rewrites the plugin header's License field during a build:"
    printf '%s\n' "$_hit" | sed 's/^/  /'
    detail "This is how GH #547 actually shipped a mismatched wp.org zip even after every source surface agreed on MIT: the build pipeline silently re-declared the header to a different license at package time. The source's declared license is the single source of truth; nothing may override it during packaging."
    return
  fi
  if [ "$_rc" -gt 1 ]; then
    err "$_f: the pattern this check uses to find a License override failed to run (grep exit $_rc), instead of cleanly finding zero or one matches."
    detail "A broken pattern must not read as a clean pass. Fix the ERE in check_makefile_no_license_override before trusting this check again."
    return
  fi
  ok "$_f's wp.org build does not override the plugin header's License field."
}

# ---------------------------------------------------------------------------
main() {
  check_php_header "the main plugin header" 'apps/agent/wpmgr-agent.php'
  check_php_header "the a-wpmgr-error-trap.php mu-plugin header" 'apps/agent/mu-plugin-loader/a-wpmgr-error-trap.php'
  check_php_header "the a-wpmgr-update-watchdog.php mu-plugin header" 'apps/agent/mu-plugin-loader/a-wpmgr-update-watchdog.php'
  check_readme_header
  check_readme_prose
  check_composer
  check_notice
  check_agent_readme_md
  check_license_agent
  check_makefile_no_license_override
  check_main_header_license_uri
  check_readme_header_license_uri

  uniq_licenses="$(printf '%s' "$SURFACES" | awk -F'|' 'NF{print $4}' | sort -u)"
  n_uniq="$(printf '%s' "$uniq_licenses" | grep -c . || true)"

  if [ "${n_uniq:-0}" -gt 1 ]; then
    err "the agent declares more than one license:"
    printf '%s' "$SURFACES" | awk -F'|' 'NF{printf "  %s (%s): %s\n", $2, $1, $3}'
    detail "Every surface above must name the SAME license. Pick one (see the owner's ruling in the tracking issue) and fix every surface that disagrees with it."
  fi

  uniq_uris="$(printf '%s' "$URI_SURFACES" | awk -F'|' 'NF{print $3}' | sort -u)"
  n_uniq_uris="$(printf '%s' "$uniq_uris" | grep -c . || true)"

  if [ "${n_uniq_uris:-0}" -gt 1 ]; then
    err "the agent declares more than one License URI:"
    printf '%s' "$URI_SURFACES" | awk -F'|' 'NF{printf "  %s (%s): %s\n", $2, $1, $3}'
    detail "Every License URI above must point at the SAME license text as every other surface's License name."
  fi

  if [ "$fail" -ne 0 ]; then
    printf '\n%s\n' "License surface check FAILED."
    return 1
  fi
  printf '\nLicense surfaces agree: %s\n' "$uniq_licenses"
  return 0
}

main
exit "$?"
