#!/usr/bin/env bash
# scripts/check-license-surfaces_test.sh
#
# The regression suite for scripts/check-license-surfaces.sh (GH #547).
#
# HOW IT WORKS. Each case builds a complete little apps/agent tree (plugin
# header, two mu-plugin headers, readme.txt, composer.json, NOTICE.md,
# README.md, LICENSE-AGENT), mutates exactly one thing, runs the guard against
# that tree and asserts the exit code plus what the output does and does not
# say. No mocking: the real script reads real files.
#
# RUN IT:
#   scripts/check-license-surfaces_test.sh            # everything
#   scripts/check-license-surfaces_test.sh notice      # only cases matching "notice"
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_LICENSE_SURFACE_SCRIPT=/tmp/guard-with-hole.sh \
#     scripts/check-license-surfaces_test.sh
#
# PORTABILITY. Written for bash 3.2 (what macOS ships) and POSIX tools, so it
# runs the same on a darwin laptop with BSD grep/sed/awk and on the ubuntu CI
# runner with the GNU ones. No mapfile, no associative arrays, no sed -i, no
# grep -P, no sort -V.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_LICENSE_SURFACE_SCRIPT:-$HERE/check-license-surfaces.sh}"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  echo "no guard script at $GUARD" >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-license-surfaces.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
SKIPPED=0
FAILED_NAMES=''

# ---------------------------------------------------------------------------
# Tree construction. Every surface starts honestly declaring MIT, including
# the third-party mentions (matthiasmullie/minify and lilliput both happen to
# be MIT upstream too), which is deliberate: it is the only way to prove the
# guard is reading the PLUGIN's declaration and not just "the word MIT
# appears somewhere in this file". The over-fire cases below flip a
# third-party mention to a DIFFERENT license and require the guard to stay
# green, which "everything is honestly MIT" cannot distinguish on its own.
# ---------------------------------------------------------------------------
write_tree() {
  _dir="$1"
  mkdir -p "$_dir/apps/agent/mu-plugin-loader"

  cat >"$_dir/apps/agent/wpmgr-agent.php" <<'PHP'
<?php
/**
 * Plugin Name:       WPMgr Agent
 * Plugin URI:        https://github.com/mosamlife/wpmgr
 * Version:           0.61.146
 * Author:            WPMgr contributors
 * License:           MIT
 * License URI:       https://opensource.org/licenses/MIT
 * Text Domain:       wpmgr-agent
 */

if (!defined('ABSPATH')) {
    exit;
}

define('WPMGR_AGENT_VERSION', '0.61.146');
PHP

  cat >"$_dir/apps/agent/mu-plugin-loader/a-wpmgr-error-trap.php" <<'PHP'
<?php
/**
 * Plugin Name: WPMgr Error Trap (mu-plugin loader)
 * Version:     1.0.0
 * Author:      WPMgr contributors
 * License:     MIT
 */
PHP

  cat >"$_dir/apps/agent/mu-plugin-loader/a-wpmgr-update-watchdog.php" <<'PHP'
<?php
/**
 * Plugin Name: WPMgr Update Watchdog (mu-plugin loader)
 * Version:     1.0.0
 * Author:      WPMgr contributors
 * License:     MIT
 */
PHP

  cat >"$_dir/apps/agent/readme.txt" <<'TXT'
=== Fleet Agent Site Manager ===
Contributors: mosamlife
Tags: backup, security, performance, updates, site management
Requires at least: 6.2
Tested up to: 7.1
Requires PHP: 8.1
Stable tag: 0.61.146
License: MIT
License URI: https://opensource.org/licenses/MIT

Securely connects this site to a WPMgr dashboard.

== Description ==

Some description prose goes here, across a few lines, before the licensing
sentence that a human reads on the wp.org listing page.

This plugin is MIT-licensed and the dashboard is AGPL-3.0. Source for both: https://github.com/mosamlife/wpmgr

== Third-party / Credits ==

**matthiasmullie/minify (MIT)**

CSS and JavaScript minification uses matthiasmullie/minify (^1.3, MIT license), a pure-PHP minification library included in the plugin's Composer dependencies. Source and license: https://github.com/matthiasmullie/minify

Copyright (c) 2012 Matthias Mullie. Licensed under the MIT License.
TXT

  cat >"$_dir/apps/agent/composer.json" <<'JSON'
{
  "name": "wpmgr/agent",
  "description": "WPMgr WordPress agent plugin",
  "type": "wordpress-plugin",
  "license": "MIT",
  "require": {
    "php": ">=8.1"
  }
}
JSON

  cat >"$_dir/apps/agent/NOTICE.md" <<'MD'
# NOTICE — WPMgr WordPress agent

The WPMgr agent is MIT-licensed (see the repository `LICENSE`).

## Third-party attribution

### lilliput (Discord, MIT)

`github.com/discord/lilliput` is MIT-licensed and used by the control-plane
`media-encoder` service for image decoding/encoding. It is not bundled with
this agent plugin.

### matthiasmullie/minify (MIT)

CSS and JS minification uses matthiasmullie/minify (^1.3, MIT), a small
pure-PHP library in the agent's Composer dependencies.
MD

  cat >"$_dir/apps/agent/README.md" <<'MD'
# apps/agent — WPMgr WordPress agent (PHP 8.0+)

MIT-licensed WordPress plugin installed on managed sites. Communicates with
the control plane over Ed25519-signed REST requests.
MD

  cat >"$_dir/LICENSE-AGENT" <<'MD'
MIT License

Copyright (c) 2026 WPMgr contributors

Applies to: apps/agent (WordPress plugin) and apps/tracker (JS web-vitals).
The rest of this repository is licensed under AGPL-3.0 (see LICENSE).

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), ...
MD

  # A trimmed stand-in for the real agent-zip-wporg target: identity rewrites
  # (Plugin Name, Text Domain) that are legitimate, and NO License rewrite —
  # the post-#547 honest state. The regression case below re-adds exactly the
  # kind of line that shipped the bug.
  cat >"$_dir/Makefile" <<'MK'
.PHONY: agent-zip-wporg
agent-zip-wporg:
	sed -i.bak \
		-e "s|^ \* Plugin Name:.*| * Plugin Name:       Fleet Agent Site Manager|" \
		-e "s|^ \* Text Domain:.*| * Text Domain:       fleet-agent-site-manager|" \
		release/fleet-agent-site-manager/fleet-agent-site-manager.php
MK
}

# tree NAME -> path to a fresh honest tree
tree() {
  _t="$WORK/$1"
  rm -rf "$_t"
  mkdir -p "$_t"
  write_tree "$_t"
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
# Honest trees. A guard that reddens correct work gets switched off, and then
# it guards nothing.
# ===========================================================================
case_run "honest: the pristine tree passes" pass "$(tree honest-pristine)"

case_run "honest: this repository's own tree passes" pass "$REPO_ROOT"

# ===========================================================================
# DISAGREEMENT: each surface, mutated alone to a different license, must fail
# and the failure must name that surface's file.
# ===========================================================================
t="$(tree disagree-main-header)"
sub "$t/apps/agent/wpmgr-agent.php" 's/^ \* License:           MIT/ * License:           GPLv2 or later/'
case_run "disagree: the main plugin header alone diverging fails and is named" \
  fail "$t" "+more than one license" "+apps/agent/wpmgr-agent.php (the main plugin header): GPLv2 or later"

t="$(tree disagree-error-trap-header)"
sub "$t/apps/agent/mu-plugin-loader/a-wpmgr-error-trap.php" 's/^ \* License:     MIT/ * License:     GPLv2 or later/'
case_run "disagree: the error-trap mu-plugin header alone diverging fails and is named" \
  fail "$t" "+apps/agent/mu-plugin-loader/a-wpmgr-error-trap.php" "+GPLv2 or later"

t="$(tree disagree-watchdog-header)"
sub "$t/apps/agent/mu-plugin-loader/a-wpmgr-update-watchdog.php" 's/^ \* License:     MIT/ * License:     GPLv2 or later/'
case_run "disagree: the update-watchdog mu-plugin header alone diverging fails and is named" \
  fail "$t" "+apps/agent/mu-plugin-loader/a-wpmgr-update-watchdog.php" "+GPLv2 or later"

# The real GH #547 shape: both readme.txt surfaces say GPLv2 while every other
# surface says MIT. This is not a synthetic mutation — it is the literal bug.
t="$(tree disagree-readme-both-real-bug-shape)"
sub "$t/apps/agent/readme.txt" 's/^License: MIT$/License: GPLv2 or later/'
sub "$t/apps/agent/readme.txt" 's/^License URI: https:\/\/opensource\.org\/licenses\/MIT$/License URI: https:\/\/www.gnu.org\/licenses\/gpl-2.0.html/'
sub "$t/apps/agent/readme.txt" 's/^This plugin is MIT-licensed and the dashboard is AGPL-3\.0\./This plugin is GPLv2 or later and the dashboard is AGPL-3.0./'
case_run "disagree: readme.txt's header AND prose both diverging (the real #547 shape) fails, naming both" \
  fail "$t" "+the readme.txt wp.org header): GPLv2 or later" "+the readme.txt Description prose): GPLv2 or later"

t="$(tree disagree-readme-header-only)"
sub "$t/apps/agent/readme.txt" 's/^License: MIT$/License: GPLv2 or later/'
case_run "disagree: the readme.txt header alone diverging fails" \
  fail "$t" "+the readme.txt wp.org header): GPLv2 or later" "-the readme.txt Description prose): GPLv2 or later"

t="$(tree disagree-readme-prose-only)"
sub "$t/apps/agent/readme.txt" 's/^This plugin is MIT-licensed and the dashboard is AGPL-3\.0\./This plugin is GPLv2 or later and the dashboard is AGPL-3.0./'
case_run "disagree: the readme.txt Description prose alone diverging fails" \
  fail "$t" "+the readme.txt Description prose): GPLv2 or later"

t="$(tree disagree-composer)"
sub "$t/apps/agent/composer.json" 's/"license": "MIT"/"license": "GPL-2.0-or-later"/'
case_run "disagree: composer.json alone diverging fails" \
  fail "$t" "+composer.json): GPL-2.0-or-later"

t="$(tree disagree-notice)"
sub "$t/apps/agent/NOTICE.md" 's/The WPMgr agent is MIT-licensed/The WPMgr agent is GPLv2-licensed/'
case_run "disagree: NOTICE.md's own claim alone diverging fails" \
  fail "$t" "+NOTICE.md): GPLv2-licensed"

t="$(tree disagree-agent-readme-md)"
sub "$t/apps/agent/README.md" 's/^MIT-licensed WordPress plugin/GPLv2-licensed WordPress plugin/'
case_run "disagree: apps/agent/README.md alone diverging fails" \
  fail "$t" "+README.md): GPLv2"

t="$(tree disagree-license-agent)"
sub "$t/LICENSE-AGENT" 's/^MIT License$/Apache License/'
case_run "disagree: LICENSE-AGENT's title alone diverging fails" \
  fail "$t" "+LICENSE-AGENT): Apache License"

# ===========================================================================
# THE REAL BUG THIS SUITE MISSED FIRST TIME: fixing readme.txt to MIT is not
# enough on its own. The Makefile's wp.org build used to carry its own `sed`
# rewrite forcing the STAGED plugin header back to "GPLv2 or later" on every
# build, regardless of what the source said -- every surface above reads only
# the committed source tree, so all nine would have stayed green while the
# actual wp.org zip kept shipping a header that disagreed with readme.txt.
# `make agent-plugincheck` (real WordPress, via Docker) is what caught it.
# This is what keeps it caught without a Docker run.
# ===========================================================================
t="$(tree regress-makefile-license-override-reintroduced)"
cat >>"$t/Makefile" <<'MK'

.PHONY: agent-zip-wporg-broken-again
agent-zip-wporg-broken-again:
	sed -i.bak \
		-e "s|^ \* License:.*| * License:           GPLv2 or later|" \
		release/fleet-agent-site-manager/fleet-agent-site-manager.php
MK
case_run "regress: reintroducing the Makefile License override fails, quoting the offending line (GH #547's actual shape)" \
  fail "$t" "+Makefile rewrites the plugin header's License field" "+GPLv2 or later"

t="$(tree absent-makefile)"
rm -f "$t/Makefile"
case_run "file absent: no Makefile at all is an error naming the file" \
  fail "$t" "+Makefile does not exist"

t="$(tree overfire-makefile-mentions-license-once)"
cat >>"$t/Makefile" <<'MK'

# Reminder: apps/agent is MIT-licensed (see LICENSE-AGENT). Nothing here
# should rewrite that during packaging.
MK
case_run "over-fire: a Makefile comment mentioning 'License' once (not a sed rewrite) does not trip the guard" \
  pass "$t"

# ===========================================================================
# ABSENCE: a surface whose declaration disappears is an ERROR, never a silent
# pass. This is the failure mode the tracking issue calls out by name: a
# check that passes because its pattern matched nothing is the same defect
# one level up.
# ===========================================================================
t="$(tree absent-main-header)"
drop "$t/apps/agent/wpmgr-agent.php" '^ \* License:'
case_run "absent: the main plugin header's License line removed is an error, not a pass" \
  fail "$t" "+declares no license" "+the main plugin header" "-OK: the main plugin header"

t="$(tree absent-error-trap-header)"
drop "$t/apps/agent/mu-plugin-loader/a-wpmgr-error-trap.php" '^ \* License:'
case_run "absent: the error-trap header's License line removed is an error" \
  fail "$t" "+apps/agent/mu-plugin-loader/a-wpmgr-error-trap.php declares no license"

t="$(tree absent-watchdog-header)"
drop "$t/apps/agent/mu-plugin-loader/a-wpmgr-update-watchdog.php" '^ \* License:'
case_run "absent: the update-watchdog header's License line removed is an error" \
  fail "$t" "+apps/agent/mu-plugin-loader/a-wpmgr-update-watchdog.php declares no license"

t="$(tree absent-readme-header)"
drop "$t/apps/agent/readme.txt" '^License:'
case_run "absent: the readme.txt header's License line removed is an error, not a pass" \
  fail "$t" "+the readme.txt wp.org header" "+declares no license"

t="$(tree absent-readme-prose)"
drop "$t/apps/agent/readme.txt" '^This plugin is MIT-licensed and the dashboard is'
case_run "absent: the readme.txt Description prose sentence removed is an error" \
  fail "$t" "+the readme.txt Description prose" "+declares no license"

t="$(tree absent-composer)"
sub "$t/apps/agent/composer.json" 's/"license": "MIT",//'
case_run "absent: composer.json with no license field is an error, not a pass" \
  fail "$t" "+composer.json declares no license"

# The important one: deleting the plugin's OWN claim in NOTICE.md must be
# reported absent even though the file still says "is MIT-licensed" twice,
# about lilliput and about matthiasmullie. If this passed, the anchor would
# be reading any third-party credit as the plugin's own declaration.
t="$(tree absent-notice-not-fooled-by-third-party)"
drop "$t/apps/agent/NOTICE.md" 'The WPMgr agent is MIT-licensed'
case_run "absent: NOTICE.md's own claim removed is an error, and is NOT satisfied by the surviving third-party 'is MIT-licensed' mentions" \
  fail "$t" "+apps/agent/NOTICE.md declares no license"

t="$(tree absent-agent-readme-md)"
drop "$t/apps/agent/README.md" 'MIT-licensed WordPress plugin'
case_run "absent: apps/agent/README.md's opening line removed is an error" \
  fail "$t" "+apps/agent/README.md declares no license"

t="$(tree absent-license-agent-no-title)"
sub "$t/LICENSE-AGENT" 's/^MIT License$/Copyright first, no title line/'
case_run "absent: LICENSE-AGENT with no recognizable title line is an error, not a pass" \
  fail "$t" "+LICENSE-AGENT declares no license"

# ===========================================================================
# FILES THAT ARE SIMPLY ABSENT.
# ===========================================================================
t="$(tree file-absent-main-plugin)"
rm -f "$t/apps/agent/wpmgr-agent.php"
case_run "file absent: no wpmgr-agent.php at all is an error naming the file" \
  fail "$t" "+apps/agent/wpmgr-agent.php does not exist"

t="$(tree file-absent-license-agent)"
rm -f "$t/LICENSE-AGENT"
case_run "file absent: no LICENSE-AGENT at all is an error naming the file" \
  fail "$t" "+LICENSE-AGENT does not exist"

# ===========================================================================
# OVER-FIRE: the matthiasmullie/minify third-party attribution (readme.txt)
# and the lilliput third-party mention (NOTICE.md) are dependency licenses,
# not the plugin's own, and must never be read as a surface. Each case below
# flips ONLY the third-party mention to a DIFFERENT license, leaving every
# real surface honestly MIT, and requires the guard to stay green — if the
# guard's patterns were loose enough to read these, it would fail here.
# ===========================================================================
t="$(tree overfire-minify-attribution-changed)"
sub "$t/apps/agent/readme.txt" 's/matthiasmullie\/minify \(\^1\.3, MIT license\)/matthiasmullie\/minify (^1.3, GPL-3.0 license)/'
sub "$t/apps/agent/readme.txt" 's/Copyright \(c\) 2012 Matthias Mullie\. Licensed under the MIT License\./Copyright (c) 2012 Matthias Mullie. Licensed under the GPL-3.0 License./'
case_run "over-fire: the matthiasmullie/minify third-party attribution changing license does not trip the guard" \
  pass "$t"

t="$(tree overfire-minify-section-removed)"
sub "$t/apps/agent/readme.txt" '/== Third-party \/ Credits ==/,$d'
case_run "over-fire: removing the whole third-party section entirely still passes (it is not a required surface)" \
  pass "$t"

t="$(tree overfire-lilliput-changed)"
sub "$t/apps/agent/NOTICE.md" 's/is MIT-licensed and used by the control-plane/is GPL-3.0-licensed and used by the control-plane/'
case_run "over-fire: the lilliput third-party mention in NOTICE.md changing license does not trip the guard" \
  pass "$t"

# ===========================================================================
# HONEST REFORMATS. A formatter, or an editor tidying whitespace, must never
# redden CI.
# ===========================================================================
t="$(tree reformat-extra-header-spacing)"
sub "$t/apps/agent/wpmgr-agent.php" 's/^ \* License:           MIT$/ *   License:MIT/'
case_run "reformat: squeezed header spacing (License:MIT, no space before, extra indent) still reads MIT" \
  pass "$t"

t="$(tree reformat-readme-trailing-space)"
sub "$t/apps/agent/readme.txt" 's/^License: MIT$/License: MIT   /'
case_run "reformat: a trailing-space readme.txt header line still reads MIT" \
  pass "$t"

t="$(tree reformat-composer-spacing)"
sub "$t/apps/agent/composer.json" 's/"license": "MIT"/"license":      "MIT"/'
case_run "reformat: extra spacing around composer.json's license colon still reads MIT" \
  pass "$t"

t="$(tree reformat-notice-trailing-clause)"
sub "$t/apps/agent/NOTICE.md" 's/The WPMgr agent is MIT-licensed \(see the repository `LICENSE`\)\./The WPMgr agent is MIT-licensed, per the top-level LICENSE-AGENT file./'
case_run "reformat: a reworded trailing clause after 'MIT-licensed' in NOTICE.md still reads MIT" \
  pass "$t"

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
