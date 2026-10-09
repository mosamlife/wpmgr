#!/usr/bin/env bash
# scripts/check-phpunit-complete.sh
#
# Proves that a PHPUnit run finished: every test PHPUnit lists for the
# configuration was executed and is in the JUnit report the run wrote.
#
# WHY THIS EXISTS. PHPUnit's exit code says whether the tests that ran passed.
# It says nothing about the tests that never ran. A PHP fatal error raised from
# inside a test ends the whole process mid-suite, and the job's red then reads
# as that one error instead of as "this many tests never ran". An exit() with
# status 0 reached from code under test ends the process green. PHPUnit writes
# its JUnit report only when a run finishes, so in both cases the report is
# missing, and that absence is the signal this script turns into a red with a
# count.
#
# WHAT IS COMPARED. The number of tests in the list PHPUnit writes with
# --list-tests-xml (one <testCaseMethod> per test method and per data set, one
# <phptFile> per PHPT test) against the number of <testcase> elements in the
# JUnit report written with --log-junit (one per executed test, skipped and
# failed tests included). Both are counted by element, never by line and never
# by a bare word: XML escapes every "<" inside attribute values and text, so a
# "<testcase" in the document is always an element. A document counts only if
# it ends with its own closing root tag, so a report cut off mid-write is never
# read as a short run.
#
# USAGE
#   scripts/check-phpunit-complete.sh --list <tests.xml> --junit <junit.xml>
#
# Make both from the same directory and the same configuration, and delete the
# report before the run: a report left by an earlier run describes that run.
#   vendor/bin/phpunit --list-tests-xml tests.xml
#   rm -f junit.xml && vendor/bin/phpunit --log-junit junit.xml
#
# EXIT CODES. Distinct on purpose, so the CI log and the regression suite can
# tell the outcomes apart without matching on prose:
#   0  Every listed test ran.
#   1  The run is not the complete listed suite: there is no report (the run
#      did not finish), or the report holds a different number of tests than
#      the list. The message says how many never ran.
#   2  Nothing could be checked: a bad invocation, a missing, unreadable or
#      malformed list, a list naming no tests, or a report that is not a whole
#      JUnit document. Never a pass.
#
# The regression suite is scripts/check-phpunit-complete_test.sh
# (make check-phpunit-complete-test).
#
# PORTABILITY. bash 3.2 and POSIX tools; no mapfile, no associative arrays,
# no sed -i, no grep -P.

set -uo pipefail

PROG='check-phpunit-complete'

usage() {
  printf 'usage: scripts/check-phpunit-complete.sh --list <tests.xml> --junit <junit.xml>\n'
  printf '  --list   the file written by: phpunit --list-tests-xml <file>\n'
  printf '  --junit  the file written by: phpunit --log-junit <file>\n'
  printf 'exit 0 = every listed test ran\n'
  printf 'exit 1 = the run did not finish, or ran a different number of tests than listed\n'
  printf 'exit 2 = nothing could be checked (bad invocation, missing or malformed input)\n'
}

fail_setup() { printf '%s: FATAL: %s\n' "$PROG" "$1" >&2; exit 2; }

LIST=''
JUNIT=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --list)    [ "$#" -ge 2 ] || fail_setup '--list needs a value'; LIST="$2"; shift 2 ;;
    --junit)   [ "$#" -ge 2 ] || fail_setup '--junit needs a value'; JUNIT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) printf '%s: unknown argument: %s\n' "$PROG" "$1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$LIST" ]  || fail_setup '--list is required'
[ -n "$JUNIT" ] || fail_setup '--junit is required'
command -v awk >/dev/null 2>&1 || fail_setup 'awk not found on PATH'

# count_elements <file> <root> <element>...
# Prints "<complete> <count>": complete is 1 when the document opens <root> and
# its last non-blank text is the closing </root> (or a self-closing <root/>),
# and count is the number of start tags of the named elements. Exits non-zero
# only if awk itself fails.
count_elements() {
  file="$1"; root="$2"; shift 2
  awk -v root="$root" -v names="$*" '
    BEGIN {
      n = split(names, el, " ")
      open_rx  = "<" root "([[:space:]/>]|$)"
      close_rx = "(</" root ">|<" root "([[:space:]][^<>]*)?/>)$"
    }
    {
      for (i = 1; i <= n; i++) {
        s = $0 " "
        count += gsub("<" el[i] "[[:space:]/>]", "", s)
      }
      if ($0 ~ open_rx) opened = 1
      t = $0
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", t)
      if (t != "") last = t
    }
    END { printf "%d %d\n", (opened && last ~ close_rx) ? 1 : 0, count + 0 }
  ' "$file"
}

# --- the list: what this configuration must run ------------------------------

[ -f "$LIST" ] || fail_setup "test list not found: $LIST (write it with: phpunit --list-tests-xml <file>)"
[ -r "$LIST" ] || fail_setup "test list not readable: $LIST"
[ -s "$LIST" ] || fail_setup "test list is empty: $LIST"

res="$(count_elements "$LIST" tests testCaseMethod phptFile)" || fail_setup "could not read the test list: $LIST"
list_complete="${res%% *}"
listed="${res##* }"
case "$list_complete:$listed" in
  [01]:[0-9]*) : ;;
  *) fail_setup "could not count the tests in the list: $LIST" ;;
esac
[ "$list_complete" -eq 1 ] \
  || fail_setup "the test list is not a whole phpunit --list-tests-xml document (no <tests> root, or it stops before </tests>): $LIST"
[ "$listed" -gt 0 ] \
  || fail_setup "the test list names no tests, so a run of it proves nothing: $LIST"

# --- the report: what the run executed -----------------------------------------

if [ ! -e "$JUNIT" ]; then
  printf '%s: INCOMPLETE: no JUnit report at %s\n' "$PROG" "$JUNIT" >&2
  printf 'The PHPUnit run did not finish, so none of the %s listed tests can be shown to have run.\n' "$listed" >&2
  printf 'PHPUnit writes this report when a run finishes. A PHP fatal error, an exit() or die()\n' >&2
  printf 'reached from a test, or a killed process ends the run without one; the cause is at the\n' >&2
  printf 'end of the test step output.\n' >&2
  exit 1
fi
[ -f "$JUNIT" ] || fail_setup "the JUnit report is not a regular file: $JUNIT"
[ -r "$JUNIT" ] || fail_setup "the JUnit report is not readable: $JUNIT"
[ -s "$JUNIT" ] || fail_setup "the JUnit report is empty: $JUNIT"

res="$(count_elements "$JUNIT" testsuites testcase)" || fail_setup "could not read the JUnit report: $JUNIT"
junit_complete="${res%% *}"
ran="${res##* }"
case "$junit_complete:$ran" in
  [01]:[0-9]*) : ;;
  *) fail_setup "could not count the tests in the JUnit report: $JUNIT" ;;
esac
[ "$junit_complete" -eq 1 ] \
  || fail_setup "the JUnit report is not a whole JUnit document (no <testsuites> root, or it stops before </testsuites>): $JUNIT"

# --- the verdict ---------------------------------------------------------------

if [ "$ran" -eq "$listed" ]; then
  printf '%s: OK: all %s listed tests ran and are in the JUnit report.\n' "$PROG" "$listed"
  exit 0
fi

if [ "$ran" -lt "$listed" ]; then
  printf '%s: INCOMPLETE: ran %s of %s listed tests; %s never ran.\n' \
    "$PROG" "$ran" "$listed" "$((listed - ran))" >&2
  printf 'The suite stopped before the end. Look in the test step output for what ended it\n' >&2
  printf '(a stop-on-failure setting, or a test class whose setUpBeforeClass failed).\n' >&2
  exit 1
fi

printf '%s: MISMATCH: the JUnit report has %s tests but the list has %s.\n' "$PROG" "$ran" "$listed" >&2
printf 'The list was not made from the same configuration, filter or directory as the run.\n' >&2
exit 1
