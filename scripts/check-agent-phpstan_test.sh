#!/usr/bin/env bash
# scripts/check-agent-phpstan_test.sh
#
# The regression suite for scripts/check-agent-phpstan.sh.
#
# WHAT IT TESTS AND WHAT IT DOES NOT. It tests the guard's decision: given what
# PHPStan wrote to stdout, what it wrote to stderr, and the code it exited with,
# does the guard reach the right verdict and say a useful thing about it. It
# does not re-test PHPStan. Every fixture below is a real shape observed from
# PHPStan 2.x with --error-format=json, including the one that matters most:
# an aborted run reports the syntax errors as ordinary per-file entries, leaves
# totals.errors at zero and the top level errors array empty, and puts the
# "Result is incomplete because of severe errors" warning on STDERR ONLY.
#
# HOW THE ANALYSER IS FAKED, AND WHY THAT IS HONEST. Each case builds a
# complete little agent directory with a phpstan.neon, a vendor/ tree, and a
# vendor/bin/phpstan that is a real PHP script. The guard resolves it, invokes
# it through the same php command line it uses in production, and reads the two
# streams and the exit code exactly as it would from the real analyser. Only
# the analysis itself is canned. Nothing about the guard is stubbed, no
# function is overridden, and no code path is special cased for tests.
#
# The real analyser is exercised too, but by CI running the guard against
# apps/agent for real, which is the other half of the same gate.
#
# RUN IT:
#   scripts/check-agent-phpstan_test.sh          # everything
#   scripts/check-agent-phpstan_test.sh parse    # only cases matching "parse"
#
# Point it at a different implementation to prove the suite is not vacuous
# (break a copy of the guard, watch the suite go red):
#   WPMGR_AGENT_PHPSTAN_SCRIPT=/tmp/guard-with-hole.sh \
#     scripts/check-agent-phpstan_test.sh
#
# PORTABILITY. bash 3.2 and POSIX tools, same as the guard. Needs php, because
# the thing under test needs php; a missing php stops the suite loudly rather
# than reporting a green run over nothing.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_AGENT_PHPSTAN_SCRIPT:-$HERE/check-agent-phpstan.sh}"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  echo "no guard script at $GUARD" >&2
  exit 2
fi

PHP_BIN="$(command -v php 2>/dev/null || true)"
if [ -z "$PHP_BIN" ]; then
  echo 'no php on PATH, and this suite cannot test a php tool without it.' >&2
  echo 'Refusing to report a pass over tests that did not run.' >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-agent-phpstan-test.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
SKIPPED=0
FAILED_NAMES=''

# Per case switches, reset by case_run after every case so one case can never
# leak a setting into the next. ALLOW_FINDINGS passes --allow-findings on the
# command line; ALLOW_FINDINGS_ENV sets the environment variable instead, so
# both routes into the same behaviour are covered.
ENV_PHP_BIN=''
ENV_PHPSTAN_BIN=''
ALLOW_FINDINGS=0
ALLOW_FINDINGS_ENV=0

# ---------------------------------------------------------------------------
# Fixtures: real PHPStan 2.x output shapes.
#
# @@CWD@@ is replaced by the stub with its own working directory, which is the
# agent directory the guard cd'd into. That reproduces the real thing, where
# PHPStan reports absolute realpath'd file names, and it is what lets the
# "no machine specific paths in the report" case below mean something.
# ---------------------------------------------------------------------------

JSON_CLEAN='{"totals":{"errors":0,"file_errors":0},"files":{},"errors":[]}'

# An aborted run. Note totals.errors is 0 and the errors array is empty: read
# from stdout alone this is indistinguishable from a couple of ordinary
# findings, which is the entire reason this guard exists.
JSON_PARSE_ABORT='{"totals":{"errors":0,"file_errors":2},"files":{"@@CWD@@/includes/cache/class-admin-bar-purge.php":{"errors":1,"messages":[{"message":"Syntax error, unexpected T_STRING, expecting T_VARIABLE on line 36","line":36,"ignorable":false,"identifier":"phpstan.parse"}]},"@@CWD@@/includes/security/class-security-policy.php":{"errors":1,"messages":[{"message":"Syntax error, unexpected T_STRING, expecting T_VARIABLE on line 41","line":41,"ignorable":false,"identifier":"phpstan.parse"}]}},"errors":[]}'

JSON_TWO_FINDINGS='{"totals":{"errors":0,"file_errors":2},"files":{"@@CWD@@/includes/class-alpha.php":{"errors":1,"messages":[{"message":"Method alpha() should return string but returns int.","line":9,"ignorable":true,"identifier":"return.type"}]},"@@CWD@@/includes/class-beta.php":{"errors":1,"messages":[{"message":"Call to an undefined method Beta::missing().","line":14,"ignorable":true,"identifier":"method.notFound"}]}},"errors":[]}'

# The same two findings, files emitted in the opposite order.
JSON_TWO_FINDINGS_REORDERED='{"totals":{"file_errors":2,"errors":0},"files":{"@@CWD@@/includes/class-beta.php":{"errors":1,"messages":[{"message":"Call to an undefined method Beta::missing().","line":14,"ignorable":true,"identifier":"method.notFound"}]},"@@CWD@@/includes/class-alpha.php":{"errors":1,"messages":[{"message":"Method alpha() should return string but returns int.","line":9,"ignorable":true,"identifier":"return.type"}]}},"errors":[]}'

# A finding whose MESSAGE TEXT quotes the incompleteness sentence. A guard that
# grepped stdout for that sentence would call this a parse abort and redden a
# build for a reason that has nothing to do with the analysis.
JSON_FINDING_QUOTING_MARKER='{"totals":{"errors":0,"file_errors":1},"files":{"@@CWD@@/includes/class-doc.php":{"errors":1,"messages":[{"message":"Result is incomplete because of severe errors is a string this method returns.","line":22,"ignorable":true,"identifier":"return.type"}]}},"errors":[]}'

# totals claims nothing is wrong while the per-file messages say otherwise.
JSON_ZERO_TOTALS_WITH_MESSAGES='{"totals":{"errors":0,"file_errors":0},"files":{"@@CWD@@/includes/class-liar.php":{"errors":1,"messages":[{"message":"Something real is wrong here.","line":3,"ignorable":true,"identifier":"return.type"}]}},"errors":[]}'

JSON_NO_TOTALS='{"files":{},"errors":[]}'

JSON_TOP_LEVEL_ERROR='{"totals":{"errors":1,"file_errors":0},"files":{},"errors":["Internal error: something went wrong in the analyser."]}'

# A per-file finding whose MESSAGE TEXT mentions an analysis error, with the
# top level "errors" array itself empty. A guard that keyed off the words
# rather than the array would misread this as the untrustworthy outcome above.
JSON_FINDING_MENTIONS_ANALYSIS_ERROR='{"totals":{"errors":0,"file_errors":1},"files":{"@@CWD@@/includes/class-doc.php":{"errors":1,"messages":[{"message":"Catches and logs an analysis error from the remote API gracefully.","line":22,"ignorable":true,"identifier":"return.type"}]}},"errors":[]}'

NOT_JSON='PHP Fatal error:  Allowed memory size of 134217728 bytes exhausted in /vendor/phpstan/phpstan/phpstan.phar on line 1'

STDERR_EMPTY=''
STDERR_NOTE='Note: Using configuration file /somewhere/apps/agent/phpstan.neon.'
STDERR_INCOMPLETE='Note: Using configuration file /somewhere/apps/agent/phpstan.neon.
 !  Result is incomplete because of severe errors.  !
   Fix these errors first and then re-run PHPStan
   to get all reported errors.'
# PHPStan 2.x prints this whenever there are findings. It is ordinary stderr
# noise and must never be mistaken for a failure signal.
#
# The backticks below are part of the fixture text, copied from what PHPStan
# actually prints. Single quotes are what keeps them literal.
# shellcheck disable=SC2016
STDERR_INSTRUCTIONS='Instructions for interpreting errors
---------

Each error has an associated identifier, like `argument.type`.
Each error identifier has documentation at URL https://phpstan.org/error-identifiers/<identifier>
The error usually indicates a real bug or incorrect type in the code.'

# ---------------------------------------------------------------------------
# Tree construction
# ---------------------------------------------------------------------------

# The fake analyser. Reads its canned streams and exit code from files beside
# it, so a case can change the analyser's behaviour without rewriting any PHP.
write_stub_binary() {
  cat >"$1" <<'STUB'
<?php
// Stands in for vendor/bin/phpstan. Reads canned output from files beside it.
$dir = __DIR__;
$read = function ($name) use ($dir) {
    $v = @file_get_contents($dir . '/' . $name);
    return $v === false ? '' : $v;
};
$out = str_replace('@@CWD@@', getcwd(), $read('stub-stdout'));
$err = str_replace('@@CWD@@', getcwd(), $read('stub-stderr'));
$code = (int) trim($read('stub-exit'));
if ($out !== '') {
    fwrite(STDOUT, $out);
}
if ($err !== '') {
    fwrite(STDERR, $err);
}
exit($code);
STUB
  chmod +x "$1"
}

# tree NAME [CONFIG_NAME] -> path to a complete, runnable fixture agent dir.
tree() {
  _dir="$WORK/$1"
  _config="${2:-phpstan.neon}"
  mkdir -p "$_dir/includes" "$_dir/vendor/bin"
  printf 'parameters:\n    level: 8\n    paths:\n        - includes\n' >"$_dir/$_config"
  printf '{"name":"wpmgr/agent","require":{"php":">=8.1"}}\n' >"$_dir/composer.json"
  write_stub_binary "$_dir/vendor/bin/phpstan"
  # A default so a tree is always runnable; cases override with stub().
  stub "$_dir" "$JSON_CLEAN" "$STDERR_EMPTY" 0
  printf '%s' "$_dir"
}

# stub DIR STDOUT STDERR EXITCODE
stub() {
  printf '%s' "$2" >"$1/vendor/bin/stub-stdout"
  printf '%s' "$3" >"$1/vendor/bin/stub-stderr"
  printf '%s' "$4" >"$1/vendor/bin/stub-exit"
}

# ---------------------------------------------------------------------------
# Assertions
#
#   case_run NAME EXPECTED_EXIT DIR [+MUST-CONTAIN ...] [-MUST-NOT-CONTAIN ...]
#
# The exit code is asserted exactly, not as pass/fail, because telling 1 from 3
# IS the thing this guard exists to do.
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
        ENV_PHP_BIN=''
        ENV_PHPSTAN_BIN=''
        ALLOW_FINDINGS=0
        ALLOW_FINDINGS_ENV=0
        return 0
        ;;
    esac
  fi

  if [ "$ALLOW_FINDINGS" -eq 1 ]; then
    _out="$(WPMGR_AGENT_PHP_BIN="$ENV_PHP_BIN" WPMGR_PHPSTAN_BIN="$ENV_PHPSTAN_BIN" \
      "$GUARD" --allow-findings "$_dir" 2>&1)"
    _code=$?
  elif [ "$ALLOW_FINDINGS_ENV" -eq 1 ]; then
    _out="$(WPMGR_AGENT_PHP_BIN="$ENV_PHP_BIN" WPMGR_PHPSTAN_BIN="$ENV_PHPSTAN_BIN" \
      WPMGR_PHPSTAN_ALLOW_FINDINGS=1 "$GUARD" "$_dir" 2>&1)"
    _code=$?
  else
    _out="$(WPMGR_AGENT_PHP_BIN="$ENV_PHP_BIN" WPMGR_PHPSTAN_BIN="$ENV_PHPSTAN_BIN" "$GUARD" "$_dir" 2>&1)"
    _code=$?
  fi
  ENV_PHP_BIN=''
  ENV_PHPSTAN_BIN=''
  ALLOW_FINDINGS=0
  ALLOW_FINDINGS_ENV=0
  _problems=''

  if [ "$_code" -ne "$_want" ]; then
    _problems="$_problems
    expected exit $_want, got $_code"
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

# case_same NAME DIR_A DIR_B: the two runs must produce identical finding lines.
case_same() {
  _name="$1"
  if [ -n "$FILTER" ]; then
    case "$_name" in
      *"$FILTER"*) : ;;
      *)
        SKIPPED=$((SKIPPED + 1))
        return 0
        ;;
    esac
  fi
  _a="$("$GUARD" "$2" 2>&1 | grep '  includes/' | sort)"
  _b="$("$GUARD" "$3" 2>&1 | grep '  includes/' | sort)"
  if [ -z "$_a" ]; then
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES  $_name
"
    printf 'FAIL %s\n    no finding lines at all, so the compare proves nothing\n' "$_name"
  elif [ "$_a" = "$_b" ]; then
    PASSED=$((PASSED + 1))
    printf 'ok   %s\n' "$_name"
  else
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES  $_name
"
    printf 'FAIL %s\n    the two reports differ:\n%s\n    ---\n%s\n' "$_name" "$_a" "$_b"
  fi
}

# ===========================================================================
# THE PARSE ABORT. The defect this guard was built for. An incomplete analysis
# must never read as a finding count, and must never be answered by baselining.
# ===========================================================================

t="$(tree parse-abort-both-signals)"
stub "$t" "$JSON_PARSE_ABORT" "$STDERR_INCOMPLETE" 1
case_run "parse: both signals present is an incomplete analysis, not a finding count" \
  3 "$t" \
  "+did not complete" "+MUST NOT BE BASELINED" \
  "+Result is incomplete because of severe errors" \
  "+phpstan.parse" \
  "-completed and reported"

# The stderr marker is the ONLY completeness signal in a JSON run, and it is on
# a stream nothing reading the report would look at.
t="$(tree parse-abort-stderr-only)"
stub "$t" "$JSON_TWO_FINDINGS" "$STDERR_INCOMPLETE" 1
case_run "parse: the stderr marker alone is enough, even when stdout looks like ordinary findings" \
  3 "$t" \
  "+did not complete" "+MUST NOT BE BASELINED" \
  "-completed and reported"

# And the identifier alone is enough, in case a future PHPStan stops printing
# the sentence or prints it somewhere else.
t="$(tree parse-abort-identifier-only)"
stub "$t" "$JSON_PARSE_ABORT" "$STDERR_NOTE" 1
case_run "parse: the phpstan.parse identifier alone is enough, with a clean stderr" \
  3 "$t" \
  "+did not complete" "+phpstan.parse" \
  "-completed and reported"

t="$(tree parse-abort-lists-files)"
stub "$t" "$JSON_PARSE_ABORT" "$STDERR_INCOMPLETE" 1
case_run "parse: the report names the files it could not parse" \
  3 "$t" \
  "+includes/cache/class-admin-bar-purge.php:36" \
  "+includes/security/class-security-policy.php:41"

t="$(tree parse-abort-no-machine-paths)"
stub "$t" "$JSON_PARSE_ABORT" "$STDERR_INCOMPLETE" 1
case_run "parse: the report is free of machine specific absolute paths" \
  3 "$t" "+includes/cache/class-admin-bar-purge.php" "-$t/includes"

# A parse abort that somehow exits 0 is still a parse abort.
t="$(tree parse-abort-exit-zero)"
stub "$t" "$JSON_PARSE_ABORT" "$STDERR_INCOMPLETE" 0
case_run "parse: an incomplete analysis that exits 0 still fails" 3 "$t" "+did not complete"

# ===========================================================================
# TOP-LEVEL ANALYSIS ERRORS. An entry in PHPStan's "errors" array is not tied
# to any file: it is the analyser saying part of the run itself failed, not
# that it examined code and found something. Same untrustworthy category as
# the parse abort above, on its own exit code, never a finding count.
# ===========================================================================

t="$(tree top-level-error)"
stub "$t" "$JSON_TOP_LEVEL_ERROR" '' 1
case_run "top-level: an analysis error is its own outcome, not a finding" \
  4 "$t" "+top-level analysis error" "+Internal error" \
  "-reported 1 finding(s)" "-OK:"

# ===========================================================================
# NOTHING USABLE CAME BACK. A guard that finds nothing goes red.
# ===========================================================================

t="$(tree empty-output-nonzero)"
stub "$t" '' "$STDERR_NOTE" 1
case_run "empty: no output at all with a non-zero exit fails" \
  3 "$t" "+produced no output at all"

# The one that would fail open: a run that wrote nothing and claimed success.
t="$(tree empty-output-zero)"
stub "$t" '' '' 0
case_run "empty: no output at all with a ZERO exit still fails, it never reads as clean" \
  3 "$t" "+produced no output at all" "-OK:"

t="$(tree not-json)"
stub "$t" "$NOT_JSON" '' 255
case_run "unparseable: a fatal error on stdout instead of a report fails" \
  3 "$t" "+could not be parsed as a JSON report" "+Allowed memory size"

t="$(tree json-without-totals)"
stub "$t" "$JSON_NO_TOTALS" '' 0
case_run "unparseable: valid JSON with no totals object fails" \
  3 "$t" "+could not be parsed as a JSON report"

t="$(tree zero-findings-nonzero-exit)"
stub "$t" "$JSON_CLEAN" "$STDERR_NOTE" 1
case_run "untrusted: zero findings alongside a non-zero exit never reads as a pass" \
  3 "$t" "+no findings but exited 1" "-OK:"

t="$(tree zero-totals-with-messages)"
stub "$t" "$JSON_ZERO_TOTALS_WITH_MESSAGES" '' 0
case_run "untrusted: totals claiming zero over a non-empty message list is believed as findings" \
  1 "$t" "+reported 1 finding(s)" "-OK:"

# ===========================================================================
# THE ENVIRONMENT CANNOT RUN THE ANALYSIS. Each one names what is missing and
# what to run, and none of them may pass.
# ===========================================================================

case_run "env: a directory that does not exist fails and names it" \
  2 "$WORK/no-such-agent-dir" "+does not exist"

t="$(tree env-no-config)"
rm -f "$t/phpstan.neon"
case_run "env: no PHPStan config at all fails and names what it looked for" \
  2 "$t" "+no PHPStan config" "+phpstan.neon.dist"

t="$(tree env-no-vendor)"
rm -rf "$t/vendor"
case_run "env: a missing vendor tree fails and names the command that fixes it" \
  2 "$t" "+vendor does not exist" "+composer install"

t="$(tree env-no-phpstan-binary)"
rm -f "$t/vendor/bin/phpstan"
case_run "env: a vendor tree with no PHPStan binary fails and names the command that fixes it" \
  2 "$t" "+no PHPStan binary" "+composer install"

t="$(tree env-bad-phpstan-override)"
ENV_PHPSTAN_BIN="$WORK/definitely-not-a-phpstan"
case_run "env: WPMGR_PHPSTAN_BIN pointing at nothing fails and names the variable" \
  2 "$t" "+WPMGR_PHPSTAN_BIN"

t="$(tree env-bad-php-override)"
ENV_PHP_BIN="$WORK/definitely-not-a-php"
case_run "env: WPMGR_AGENT_PHP_BIN pointing at nothing fails and names the variable" \
  2 "$t" "+WPMGR_AGENT_PHP_BIN"

# ===========================================================================
# FINDINGS. A completed analysis that found things, counted at runtime.
# ===========================================================================

t="$(tree findings-two)"
stub "$t" "$JSON_TWO_FINDINGS" "$STDERR_INSTRUCTIONS" 1
case_run "findings: two findings are reported as two, with the count computed not assumed" \
  1 "$t" "+reported 2 finding(s)" \
  "+includes/class-alpha.php:9" "+includes/class-beta.php:14" \
  "+[return.type]" "-did not complete"

# The count is printed from the data, so a different number of findings prints
# a different number without anybody editing the guard.
_msgs=''
_i=1
while [ "$_i" -le 60 ]; do
  _msgs="$_msgs{\"message\":\"Finding number $_i.\",\"line\":$_i,\"ignorable\":true,\"identifier\":\"return.type\"},"
  _i=$((_i + 1))
done
_msgs="${_msgs%,}"
JSON_SIXTY="{\"totals\":{\"errors\":0,\"file_errors\":60},\"files\":{\"@@CWD@@/includes/big.php\":{\"errors\":60,\"messages\":[$_msgs]}},\"errors\":[]}"

t="$(tree findings-many)"
stub "$t" "$JSON_SIXTY" '' 1
case_run "findings: a long list prints the real total and says how many it did not list" \
  1 "$t" "+reported 60 finding(s)" "+and 10 more"

# ===========================================================================
# CLEAN, and the things that must not break it. A guard that reddens correct
# work gets switched off, and then it guards nothing.
# ===========================================================================

t="$(tree clean-quiet)"
stub "$t" "$JSON_CLEAN" '' 0
case_run "clean: an empty report with a zero exit passes" 0 "$t" "+OK:" "+0 findings"

t="$(tree clean-with-note)"
stub "$t" "$JSON_CLEAN" "$STDERR_NOTE" 0
case_run "clean: the routine 'Using configuration file' note on stderr does not redden it" \
  0 "$t" "+OK:"

t="$(tree clean-with-instructions)"
stub "$t" "$JSON_CLEAN" "$STDERR_INSTRUCTIONS" 0
case_run "clean: PHPStan's stderr instructions block does not redden it" 0 "$t" "+OK:"

t="$(tree clean-dist-config phpstan.neon.dist)"
stub "$t" "$JSON_CLEAN" '' 0
case_run "clean: a phpstan.neon.dist config is accepted" 0 "$t" "+OK:" "+phpstan.neon.dist"

# The over-fire that a stdout grep for the marker sentence would cause.
t="$(tree finding-quotes-the-marker)"
stub "$t" "$JSON_FINDING_QUOTING_MARKER" "$STDERR_INSTRUCTIONS" 1
case_run "no over-fire: a finding whose message quotes the incompleteness sentence is a finding, not an abort" \
  1 "$t" "+reported 1 finding(s)" "-did not complete" "-MUST NOT BE BASELINED"

# The equivalent over-fire for the top-level-error check: the array is what
# means it, not the words. A finding whose message text merely mentions an
# analysis error must still be read as an ordinary finding.
t="$(tree finding-mentions-analysis-error)"
stub "$t" "$JSON_FINDING_MENTIONS_ANALYSIS_ERROR" '' 1
case_run "no over-fire: a finding whose message mentions an analysis error is a finding, not a top-level error" \
  1 "$t" "+reported 1 finding(s)" "-top-level analysis error"

# Ordering is a property of the analyser, not of the codebase.
t="$(tree order-a)"
stub "$t" "$JSON_TWO_FINDINGS" '' 1
t2="$(tree order-b)"
stub "$t2" "$JSON_TWO_FINDINGS_REORDERED" '' 1
case_same "no over-fire: the report is identical whichever order the files come back in" "$t" "$t2"

case_run "no over-fire: --help exits 0" 0 --help

case_run "no over-fire: an unknown option is refused rather than ignored" 2 --nonsense-flag

# ===========================================================================
# ADVISORY MODE. --allow-findings exists so the gate can land before the
# backlog is triaged without inviting a one-command baseline regeneration that
# would swallow the real findings. It must relax the FINDINGS outcome and
# NOTHING ELSE. Every case below is the proof of that boundary: if any of them
# starts passing, the flag has become a way to switch the guard off.
# ===========================================================================

t="$(tree advisory-findings)"
stub "$t" "$JSON_TWO_FINDINGS" "$STDERR_INSTRUCTIONS" 1
ALLOW_FINDINGS=1
case_run "advisory: findings are reported loudly but do not fail the build" \
  0 "$t" "+ADVISORY" "+reported 2 finding(s)" \
  "+includes/class-alpha.php:9" "-OK:"

t="$(tree advisory-parse-abort)"
stub "$t" "$JSON_PARSE_ABORT" "$STDERR_INCOMPLETE" 1
ALLOW_FINDINGS=1
case_run "advisory: a parse abort STILL fails, the flag does not reach it" \
  3 "$t" "+did not complete" "+MUST NOT BE BASELINED" "-ADVISORY"

t="$(tree advisory-parse-abort-identifier)"
stub "$t" "$JSON_PARSE_ABORT" "$STDERR_NOTE" 1
ALLOW_FINDINGS=1
case_run "advisory: a parse abort seen only via the identifier STILL fails" \
  3 "$t" "+did not complete" "-ADVISORY"

t="$(tree advisory-top-level-error)"
stub "$t" "$JSON_TOP_LEVEL_ERROR" '' 1
ALLOW_FINDINGS=1
case_run "advisory: a top level analysis error STILL fails, the flag does not reach it" \
  4 "$t" "+top-level analysis error" "-ADVISORY"

t="$(tree advisory-env-top-level-error)"
stub "$t" "$JSON_TOP_LEVEL_ERROR" '' 1
ALLOW_FINDINGS_ENV=1
case_run "advisory: WPMGR_PHPSTAN_ALLOW_FINDINGS=1 does not reach a top level analysis error either" \
  4 "$t" "+top-level analysis error" "-ADVISORY"

t="$(tree advisory-zero-findings-nonzero)"
stub "$t" "$JSON_CLEAN" "$STDERR_NOTE" 1
ALLOW_FINDINGS=1
case_run "advisory: zero findings with a non-zero exit STILL fails" \
  3 "$t" "+cannot be trusted" "-ADVISORY"

t="$(tree advisory-empty-output)"
stub "$t" '' '' 1
ALLOW_FINDINGS=1
case_run "advisory: no parseable output STILL fails" \
  3 "$t" "+produced no output at all" "-ADVISORY"

t="$(tree advisory-no-vendor)"
rm -rf "$t/vendor"
ALLOW_FINDINGS=1
case_run "advisory: a missing vendor tree STILL fails" \
  2 "$t" "+vendor does not exist" "-ADVISORY"

t="$(tree advisory-clean)"
stub "$t" "$JSON_CLEAN" '' 0
ALLOW_FINDINGS=1
case_run "advisory: a genuinely clean run is still reported as clean, not as advisory" \
  0 "$t" "+OK:" "-ADVISORY"

# The env var is the same switch as the flag, so a CI step can set either.
t="$(tree advisory-env-var)"
stub "$t" "$JSON_TWO_FINDINGS" '' 1
ALLOW_FINDINGS_ENV=1
case_run "advisory: WPMGR_PHPSTAN_ALLOW_FINDINGS=1 behaves the same as the flag" \
  0 "$t" "+ADVISORY" "+reported 2 finding(s)"

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
