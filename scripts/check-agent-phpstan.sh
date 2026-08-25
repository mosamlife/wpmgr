#!/usr/bin/env bash
# scripts/check-agent-phpstan.sh
#
# Runs the agent plugin's PHPStan analysis and decides whether the result may
# be trusted. Trust is the whole point of this script: PHPStan can exit 1 while
# having analysed almost nothing, and telling that apart from "it analysed
# everything and found some things" is a decision, not a line count.
#
# WHY THIS EXISTS. apps/agent has owned PHPStan at level 8 with the WordPress
# extension, the WordPress stubs and a curated baseline for a long time, and
# nothing in .github/workflows ever invoked it. When it was finally run by hand
# it turned out to have been aborting in the parser, so the level-8 analysis
# that the plugin was believed to be held to had never produced a result at all.
# A gate that nobody runs is indistinguishable from no gate, which is why the
# invocation now lives in CI and the decision logic lives here, in a file with
# a committed regression suite, instead of in a YAML block scalar.
#
# THE FAILURE MODE THIS IS BUILT AGAINST. When PHPStan hits a syntax error it
# cannot recover from, it reports the syntax errors as ordinary file errors,
# gives up on the rest of the analysis, and says so. Read carelessly that looks
# like a small, finite list of findings, and the obvious way to make a small
# finite list of findings go away is to widen phpstan-baseline.neon. Do that and
# the exit code goes green while the analyser stays blind, permanently, over the
# entire plugin.
#
# What makes it genuinely easy to get wrong is where the signals are. With
# --error-format=json, an aborted run puts the syntax errors in the normal
# per-file structure, reports totals.errors as zero because that field counts
# only non-file errors, leaves the top level "errors" array empty, and prints
# the "Result is incomplete because of severe errors" warning to STDERR ONLY,
# where nothing reading the JSON document will ever see it. So a gate that reads
# stdout alone sees a handful of ordinary findings and no incompleteness signal
# whatsoever. Both streams have to be read.
#
# This script therefore treats an incomplete analysis as its own outcome with
# its own exit code, distinct from "the analysis completed and found things",
# and says in the failure message that the entries must not be baselined.
#
# EXIT CODES. Distinct on purpose, so CI logs and the regression suite can tell
# the outcomes apart without matching on prose:
#
#   0  The analysis completed and found nothing.
#   1  The analysis completed and reported findings. Fix them, or baseline them
#      deliberately.
#   2  The environment is not able to run the analysis: no php, no PHPStan
#      binary, no vendor tree, no config, no such directory. Nothing was
#      analysed and nothing is claimed.
#   3  The analysis ran but its result cannot be trusted: it aborted in the
#      parser, it produced nothing parseable, or it reported no findings while
#      exiting non-zero. THIS IS NOT A FINDING COUNT. Never respond to a 3 by
#      editing the baseline.
#
# Any non-zero exit fails the build. The codes exist to make the reason legible,
# not to grade severity.
#
# RUN IT:
#   make agent-phpstan          or  scripts/check-agent-phpstan.sh
#   make agent-phpstan-test     or  scripts/check-agent-phpstan_test.sh
#   scripts/check-agent-phpstan.sh /path/to/some/other/agent/dir
#
# ENVIRONMENT OVERRIDES. All optional; empty is the same as unset.
#   WPMGR_AGENT_PHP_BIN    php binary to use instead of the one on PATH.
#   WPMGR_PHPSTAN_BIN      PHPStan binary to use instead of the vendored one.
#   WPMGR_PHPSTAN_MEMORY   memory_limit for the analysis. Default below.
#
# THERE IS DELIBERATELY NO FALLBACK TO A PHPStan ON PATH. The plugin pins its
# PHPStan, its WordPress extension and its stubs through composer, and the
# config includes them by relative path out of vendor/. A PHPStan picked up from
# PATH would be a different version analysing without the extension, and would
# produce a confident answer that does not match the one CI produces. A wrong
# answer from a gate is worse than an absent one, so an absent vendored binary
# is an error that names the command to run, never a quiet substitution.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop and on the ubuntu runner. No mapfile, no associative
# arrays, no grep -P.

set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/check-agent-phpstan.sh [AGENT_DIR]

AGENT_DIR defaults to apps/agent inside the repository this script lives in,
or to $WPMGR_AGENT_DIR when that is set.

Exit 0: the analysis completed and found nothing.
Exit 1: the analysis completed and reported findings.
Exit 2: the environment cannot run the analysis (php, PHPStan, vendor, config).
Exit 3: the analysis ran but its result cannot be trusted (parse abort,
        unparseable output, or no findings alongside a non-zero exit).
USAGE
}

case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR_IN="${1:-${WPMGR_AGENT_DIR:-$SCRIPT_DIR/../apps/agent}}"

# Exit codes, named so the branches below read as decisions.
EXIT_CLEAN=0
EXIT_FINDINGS=1
EXIT_ENV=2
EXIT_UNTRUSTWORTHY=3

# The marker PHPStan prints to stderr when it gave up early. Matched as plain
# ASCII on purpose: the real line is wrapped in warning emoji and may be
# colourised, and neither of those is anything to depend on.
INCOMPLETE_MARKER='Result is incomplete because of severe errors'
# The error identifier PHPStan attaches to a source file it could not parse.
# These are reported with "ignorable": false, so they cannot be baselined away
# even by someone trying to; their presence always means the run gave up.
PARSE_IDENTIFIER='phpstan.parse'

MEMORY="${WPMGR_PHPSTAN_MEMORY:-2G}"
# How many findings to print in full before summarising the remainder. Only
# affects the report, never the decision.
MAX_LISTED=50

err() { printf 'ERROR: %s\n' "$1" >&2; }
detail() { printf '  %s\n' "$1" >&2; }
ok() { printf 'OK: %s\n' "$1"; }

# ---------------------------------------------------------------------------
# Preconditions. Each one names the thing that is missing and the command that
# would fix it, and each one exits non-zero. Nothing here may fall through to
# "well, no findings then": a check that cannot run has not passed.
# ---------------------------------------------------------------------------

if [ ! -d "$AGENT_DIR_IN" ]; then
  err "the agent directory does not exist: $AGENT_DIR_IN"
  detail 'Pass a directory, or run this from a checkout that has apps/agent.'
  exit "$EXIT_ENV"
fi

# Physical path, so it matches the realpath-resolved paths PHPStan reports and
# the prefix strip below actually strips. On macOS /tmp is a symlink into
# /private/tmp, and a logical path would silently stop matching.
AGENT_DIR="$(cd "$AGENT_DIR_IN" && pwd -P)" || exit "$EXIT_ENV"

CONFIG=''
for _candidate in phpstan.neon phpstan.neon.dist; do
  if [ -f "$AGENT_DIR/$_candidate" ]; then
    CONFIG="$_candidate"
    break
  fi
done
if [ -z "$CONFIG" ]; then
  err "no PHPStan config in $AGENT_DIR (looked for phpstan.neon, phpstan.neon.dist)."
  detail 'Without a config the analysis would silently run at defaults, which is not the gate.'
  exit "$EXIT_ENV"
fi

if [ ! -d "$AGENT_DIR/vendor" ]; then
  err "$AGENT_DIR/vendor does not exist, so the analysis cannot run."
  detail 'The config includes the WordPress extension and stubs from vendor/.'
  detail 'Run: composer install --prefer-dist --no-interaction   (in the agent directory)'
  exit "$EXIT_ENV"
fi

# php, resolved at the point of use. Where a toolchain binary lives is a fact
# about the machine, so it is looked up here rather than assumed, and an empty
# answer stops the run instead of skipping it.
PHP_BIN="${WPMGR_AGENT_PHP_BIN:-}"
if [ -n "$PHP_BIN" ]; then
  if ! command -v "$PHP_BIN" >/dev/null 2>&1; then
    err "WPMGR_AGENT_PHP_BIN is set to '$PHP_BIN', which is not an executable command."
    detail 'Unset it to use the php on PATH, or point it at a real php binary.'
    exit "$EXIT_ENV"
  fi
else
  PHP_BIN="$(command -v php 2>/dev/null || true)"
  if [ -z "$PHP_BIN" ]; then
    err 'no php binary found on PATH, so the analysis cannot run.'
    detail 'Install PHP, or set WPMGR_AGENT_PHP_BIN to the binary to use.'
    detail 'This step is never skipped: an unrunnable analysis is a failure, not a pass.'
    exit "$EXIT_ENV"
  fi
fi

PHPSTAN_BIN="${WPMGR_PHPSTAN_BIN:-}"
if [ -n "$PHPSTAN_BIN" ]; then
  if [ ! -f "$PHPSTAN_BIN" ]; then
    err "WPMGR_PHPSTAN_BIN is set to '$PHPSTAN_BIN', which is not a file."
    detail 'Unset it to use the vendored PHPStan, or point it at a real one.'
    exit "$EXIT_ENV"
  fi
else
  PHPSTAN_BIN="$AGENT_DIR/vendor/bin/phpstan"
  if [ ! -f "$PHPSTAN_BIN" ]; then
    err "no PHPStan binary at vendor/bin/phpstan in $AGENT_DIR."
    detail 'A vendor/ built with --no-dev has no dev tools in it.'
    detail 'Run: composer install --prefer-dist --no-interaction   (in the agent directory)'
    detail 'There is no fallback to a PHPStan on PATH: see the header of this script.'
    exit "$EXIT_ENV"
  fi
fi

# ---------------------------------------------------------------------------
# Run the analysis.
# ---------------------------------------------------------------------------

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-agent-phpstan.XXXXXX")" || exit "$EXIT_ENV"
trap 'rm -rf "$WORK"' EXIT INT TERM

OUT_FILE="$WORK/stdout"
ERR_FILE="$WORK/stderr"

# The JSON extractor. PHPStan's report is a JSON document and picking fields out
# of it with grep would be guessing; php is already a hard requirement three
# lines above, so the document is parsed by a parser. Key order in the document
# is irrelevant to json_decode, which is what keeps this insensitive to how
# PHPStan happens to order its output.
SUMMARY_PHP="$WORK/summarise.php"
cat >"$SUMMARY_PHP" <<'SUMMARY'
<?php
// Reads a PHPStan --error-format=json document and prints a flat, tab
// separated summary for the shell. Prints STATUS=empty or STATUS=badjson
// rather than guessing, so the caller can tell "nothing came back" apart from
// "nothing was wrong".
$path = isset($argv[1]) ? $argv[1] : '';
$raw = @file_get_contents($path);
if ($raw === false || trim($raw) === '') {
    echo "STATUS=empty\n";
    exit(0);
}
$doc = json_decode($raw, true);
if (!is_array($doc) || !isset($doc['totals']) || !is_array($doc['totals'])) {
    echo "STATUS=badjson\n";
    exit(0);
}
$fileErrors = isset($doc['totals']['file_errors']) ? (int) $doc['totals']['file_errors'] : 0;
$topErrors = isset($doc['totals']['errors']) ? (int) $doc['totals']['errors'] : 0;

$flatten = function ($s) {
    return str_replace(array("\t", "\r", "\n"), ' ', (string) $s);
};

$parse = 0;
$rows = array();
if (isset($doc['files']) && is_array($doc['files'])) {
    foreach ($doc['files'] as $file => $info) {
        if (!is_array($info) || !isset($info['messages']) || !is_array($info['messages'])) {
            continue;
        }
        foreach ($info['messages'] as $m) {
            if (!is_array($m)) {
                continue;
            }
            $id = isset($m['identifier']) ? (string) $m['identifier'] : '';
            if ($id === 'phpstan.parse') {
                $parse++;
            }
            $rows[] = "F\t" . $flatten($file)
                . "\t" . (isset($m['line']) ? $flatten($m['line']) : '?')
                . "\t" . ($id === '' ? '-' : $flatten($id))
                . "\t" . (isset($m['message']) ? $flatten($m['message']) : '');
        }
    }
}

$tops = array();
if (isset($doc['errors']) && is_array($doc['errors'])) {
    foreach ($doc['errors'] as $e) {
        if (is_string($e)) {
            $tops[] = "E\t" . $flatten($e);
        } elseif (is_array($e) && isset($e['message'])) {
            $tops[] = "E\t" . $flatten($e['message']);
        } else {
            $tops[] = "E\t" . $flatten(json_encode($e));
        }
    }
}

echo "STATUS=ok\n";
echo "FILE_ERRORS=" . $fileErrors . "\n";
echo "TOP_ERRORS=" . $topErrors . "\n";
echo "PARSE_IDS=" . $parse . "\n";
echo "MSG_ROWS=" . count($rows) . "\n";
echo "TOP_ROWS=" . count($tops) . "\n";
foreach ($rows as $r) {
    echo $r . "\n";
}
foreach ($tops as $t) {
    echo $t . "\n";
}
SUMMARY

printf 'Running PHPStan over %s (config %s, memory_limit %s).\n' \
  "$(basename "$AGENT_DIR")" "$CONFIG" "$MEMORY"
printf '  php:     %s\n' "$PHP_BIN"
printf '  phpstan: %s\n' "$PHPSTAN_BIN"

# cd first so the config's relative includes and paths resolve exactly as they
# do for a developer running composer analyse by hand.
cd "$AGENT_DIR" || exit "$EXIT_ENV"

# --no-ansi so the stderr marker is plain text. Nothing between the command and
# reading $?: a single intervening builtin would overwrite it, which is a bug
# this script was written with in mind.
"$PHP_BIN" -d memory_limit="$MEMORY" "$PHPSTAN_BIN" analyse \
  --no-progress --no-ansi --error-format=json --configuration "$CONFIG" \
  >"$OUT_FILE" 2>"$ERR_FILE"
PHPSTAN_EXIT=$?

# ---------------------------------------------------------------------------
# Interpret it.
# ---------------------------------------------------------------------------

# rel PATH -> the path with the agent directory prefix removed, so the report
# is identical on a laptop and on a runner and never leaks a home directory.
# Falls back to cutting at /apps/agent/ and then to printing what it was given;
# it must never fail, because a path it cannot shorten is still a path worth
# showing.
rel() {
  case "$1" in
    "$AGENT_DIR"/*) printf '%s' "${1#"$AGENT_DIR"/}" ;;
    */apps/agent/*) printf '%s' "${1#*/apps/agent/}" ;;
    *) printf '%s' "$1" ;;
  esac
}

SUMMARY_OUT="$("$PHP_BIN" "$SUMMARY_PHP" "$OUT_FILE" 2>/dev/null)"
STATUS="$(printf '%s\n' "$SUMMARY_OUT" | sed -n 's/^STATUS=//p' | sed -n '1p')"

# The stderr marker is read before anything else is believed. It is the only
# thing either stream says about completeness, and it is only ever on stderr.
#
# Matched against stderr alone, never against stdout: a finding's message text
# could quote this sentence, and a gate that reddened on a quoted sentence in a
# report would be failing for a reason unrelated to the analysis.
INCOMPLETE=0
if grep -qF "$INCOMPLETE_MARKER" "$ERR_FILE" 2>/dev/null; then
  INCOMPLETE=1
fi

FILE_ERRORS=0
TOP_ERRORS=0
PARSE_IDS=0
MSG_ROWS=0
TOP_ROWS=0
if [ "$STATUS" = ok ]; then
  FILE_ERRORS="$(printf '%s\n' "$SUMMARY_OUT" | sed -n 's/^FILE_ERRORS=//p' | sed -n '1p')"
  TOP_ERRORS="$(printf '%s\n' "$SUMMARY_OUT" | sed -n 's/^TOP_ERRORS=//p' | sed -n '1p')"
  PARSE_IDS="$(printf '%s\n' "$SUMMARY_OUT" | sed -n 's/^PARSE_IDS=//p' | sed -n '1p')"
  MSG_ROWS="$(printf '%s\n' "$SUMMARY_OUT" | sed -n 's/^MSG_ROWS=//p' | sed -n '1p')"
  TOP_ROWS="$(printf '%s\n' "$SUMMARY_OUT" | sed -n 's/^TOP_ROWS=//p' | sed -n '1p')"
fi
: "${FILE_ERRORS:=0}" "${TOP_ERRORS:=0}" "${PARSE_IDS:=0}" "${MSG_ROWS:=0}" "${TOP_ROWS:=0}"

# print_findings KIND: the finding rows, agent relative, sorted. Sorted so the
# report is byte identical between runs whatever order PHPStan walked the files
# in; ordering is a property of the analyser, not a property of the codebase.
print_findings() {
  _kind="$1"
  # Sorted BEFORE the cap, so the listed subset is the first N of a stable
  # order rather than the first N of whatever order the analyser emitted.
  printf '%s\n' "$SUMMARY_OUT" | grep '^F	' | while IFS='	' read -r _tag _file _line _id _msg; do
    printf '  %s:%s  [%s] %s\n' "$(rel "$_file")" "$_line" "$_id" "$_msg"
  done | sort | sed -n "1,${MAX_LISTED}p"
  if [ "$MSG_ROWS" -gt "$MAX_LISTED" ]; then
    printf '  ... and %s more %s not listed.\n' "$((MSG_ROWS - MAX_LISTED))" "$_kind"
  fi
  printf '%s\n' "$SUMMARY_OUT" | grep '^E	' | while IFS='	' read -r _tag _msg; do
    printf '  [analysis error] %s\n' "$_msg"
  done | sort
}

# --- Outcome: the analysis gave up. Checked before any count is reported, so
# --- that no reader is ever handed a number that looks actionable.
if [ "$INCOMPLETE" -eq 1 ] || [ "$PARSE_IDS" -gt 0 ]; then
  err 'PHPStan did not complete: the analysis aborted on files it could not parse.'
  detail ''
  detail 'THIS IS NOT A FINDING COUNT, AND THESE MUST NOT BE BASELINED.'
  detail 'PHPStan stopped early, so the files below are the ones it choked on,'
  detail 'not the problems in this codebase. Everything it never reached is'
  detail 'unanalysed and unreported. Baselining these would make the exit code'
  detail 'green and leave the analyser blind over the whole plugin.'
  detail ''
  if [ "$INCOMPLETE" -eq 1 ]; then
    detail "Signal: PHPStan printed \"$INCOMPLETE_MARKER\" on stderr."
  fi
  if [ "$PARSE_IDS" -gt 0 ]; then
    detail "Signal: $PARSE_IDS finding(s) carry the $PARSE_IDENTIFIER identifier."
  fi
  detail ''
  detail 'Files it could not parse:'
  printf '%s\n' "$SUMMARY_OUT" | grep '^F	' | while IFS='	' read -r _tag _file _line _id _msg; do
    case "$_id" in
      "$PARSE_IDENTIFIER") printf '    %s:%s  %s\n' "$(rel "$_file")" "$_line" "$_msg" ;;
    esac
  done | sort >&2
  detail ''
  detail 'Usual cause: the parser is being asked to read the source as an older'
  detail 'PHP language level than the source is written in, so modern syntax'
  detail 'reads as a syntax error. Compare the phpVersion in the PHPStan config'
  detail 'against the minimum php that composer.json requires; a phpVersion'
  detail 'below it makes every use of newer syntax an unparseable file.'
  detail 'Fixing the config is a change to the agent plugin, not to this gate.'
  exit "$EXIT_UNTRUSTWORTHY"
fi

# --- Outcome: nothing usable came back.
if [ "$STATUS" = empty ]; then
  err "PHPStan produced no output at all (it exited $PHPSTAN_EXIT)."
  detail 'A completed run always writes a JSON document, even a clean one.'
  detail 'No document means the analysis did not run, so nothing is claimed about the code.'
  if [ -s "$ERR_FILE" ]; then
    detail 'What it wrote to stderr:'
    sed 's/^/    /' "$ERR_FILE" >&2
  else
    detail 'It wrote nothing to stderr either.'
  fi
  exit "$EXIT_UNTRUSTWORTHY"
fi

if [ "$STATUS" != ok ]; then
  err "PHPStan output could not be parsed as a JSON report (it exited $PHPSTAN_EXIT)."
  detail 'Expected a JSON document with a totals object, from --error-format=json.'
  detail 'First part of what it actually wrote to stdout:'
  head -c 600 "$OUT_FILE" | sed 's/^/    /' >&2
  printf '\n' >&2
  if [ -s "$ERR_FILE" ]; then
    detail 'What it wrote to stderr:'
    sed 's/^/    /' "$ERR_FILE" >&2
  fi
  exit "$EXIT_UNTRUSTWORTHY"
fi

# The count is computed here and printed from the computation. It is never
# written down anywhere, in this script or in the workflow that calls it.
FINDINGS=$((FILE_ERRORS + TOP_ERRORS))
ROWS=$((MSG_ROWS + TOP_ROWS))
# If the totals and the reported messages disagree, believe whichever is worse.
# A totals block that says zero over a non-empty message list is exactly the
# shape of a zero that must not read as a pass.
if [ "$ROWS" -gt "$FINDINGS" ]; then
  FINDINGS="$ROWS"
fi

# --- Outcome: zero findings, but the run failed. Never a pass.
if [ "$FINDINGS" -eq 0 ] && [ "$PHPSTAN_EXIT" -ne 0 ]; then
  err "PHPStan reported no findings but exited $PHPSTAN_EXIT, so the result cannot be trusted."
  detail 'A clean analysis exits 0. A non-zero exit with an empty report means'
  detail 'the run failed for a reason it did not express as a finding.'
  if [ -s "$ERR_FILE" ]; then
    detail 'What it wrote to stderr:'
    sed 's/^/    /' "$ERR_FILE" >&2
  else
    detail 'It wrote nothing to stderr.'
  fi
  exit "$EXIT_UNTRUSTWORTHY"
fi

# --- Outcome: the analysis completed and found things.
if [ "$FINDINGS" -gt 0 ]; then
  err "PHPStan completed and reported $FINDINGS finding(s)."
  print_findings 'finding(s)' >&2
  detail ''
  detail 'The analysis completed, so this is a real count over the whole plugin.'
  detail 'Fix them at the source. Baseline only what is deliberately accepted.'
  exit "$EXIT_FINDINGS"
fi

# --- Outcome: clean.
ok "PHPStan completed over the agent plugin and reported 0 findings (exit $PHPSTAN_EXIT)."
exit "$EXIT_CLEAN"
