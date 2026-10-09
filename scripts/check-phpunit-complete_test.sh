#!/usr/bin/env bash
# scripts/check-phpunit-complete_test.sh
#
# The regression suite for scripts/check-phpunit-complete.sh.
#
# WHY THIS EXISTS. A guard nobody has watched fail is not known to guard
# anything. Section A reproduces the run this guard was written for (a PHP
# fatal error ended the agent suite partway through, and the red read as that
# one error instead of as the tests that never ran) and every way its inputs
# can be missing or broken. Section B holds the cases it must NOT redden:
# a guard that fails correct work gets switched off, and then it guards nothing.
#
# The fixtures copy the shape PHPUnit 10.5 writes for --list-tests-xml and
# --log-junit, including the nested <testsuite> it opens for each data-provider
# method. They are generated here, so the suite needs bash and awk and no PHP.
#
# RUN IT:
#   make check-phpunit-complete-test
#   scripts/check-phpunit-complete_test.sh             # everything
#   scripts/check-phpunit-complete_test.sh provider    # only cases matching "provider"
#
# Point it at a different implementation to prove the suite is not vacuous
# (plant a defect in a copy, watch the suite go red):
#   WPMGR_PHPUNIT_COMPLETE_GUARD=/tmp/guard-with-hole.sh scripts/check-phpunit-complete_test.sh
#
# PORTABILITY. bash 3.2 and POSIX tools; no mapfile, no associative arrays,
# no sed -i, no grep -P.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_PHPUNIT_COMPLETE_GUARD:-$HERE/check-phpunit-complete.sh}"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  printf 'check-phpunit-complete_test: guard not found: %s\n' "$GUARD" >&2
  exit 2
fi

PASS=0
FAIL=0

# Output is matched on the guard's own markers, never on a bare word: a bare
# word also matches a file path the guard echoes back.
GUARD_OK='check-phpunit-complete: OK'
GUARD_INCOMPLETE='check-phpunit-complete: INCOMPLETE'

# The work directory's name always contains "OK", so a case that checks for a
# bare "OK" instead of GUARD_OK goes red on every run.
TMP_BASE="${TMPDIR:-/tmp}"
WORK="$(mktemp -d "${TMP_BASE%/}/OK-phpunit-complete-selftest.XXXXXX")" || {
  printf 'check-phpunit-complete_test: could not create a work directory under %s\n' "$TMP_BASE" >&2
  exit 2
}
# The EXIT trap deletes WORK recursively, so it is only set once WORK is known
# to be the directory just created here.
case "$WORK" in
  */OK-phpunit-complete-selftest.*) [ -d "$WORK" ] || { printf 'check-phpunit-complete_test: work directory missing: %s\n' "$WORK" >&2; exit 2; } ;;
  *) printf 'check-phpunit-complete_test: unexpected work directory: %s\n' "$WORK" >&2; exit 2 ;;
esac
trap 'rm -rf "$WORK"' EXIT INT TERM

# --- fixtures --------------------------------------------------------------

# make_list <out> <n>: a --list-tests-xml document naming n plain test methods,
# fifty to a class.
make_list() {
  awk -v n="$2" 'BEGIN {
    print "<?xml version=\"1.0\"?>"
    print "<tests>"
    for (i = 1; i <= n; i++) {
      c = int((i - 1) / 50) + 1
      if ((i - 1) % 50 == 0) {
        if (i > 1) print " </testCaseClass>"
        printf " <testCaseClass name=\"WPMgr\\Agent\\Tests\\Fixture%dTest\">\n", c
      }
      printf "  <testCaseMethod id=\"WPMgr\\Agent\\Tests\\Fixture%dTest::test_%d\" name=\"test_%d\" groups=\"default\"/>\n", c, i, i
    }
    if (n > 0) print " </testCaseClass>"
    print "</tests>"
  }' > "$1"
}

# make_junit <out> <n>: a --log-junit document holding n executed tests, in
# the same fifty-to-a-class layout.
make_junit() {
  awk -v n="$2" 'BEGIN {
    print "<?xml version=\"1.0\" encoding=\"UTF-8\"?>"
    print "<testsuites>"
    printf "  <testsuite name=\"/repo/apps/agent/phpunit.xml.dist\" tests=\"%d\" assertions=\"%d\" errors=\"0\" failures=\"0\" skipped=\"0\" time=\"1.000000\">\n", n, n
    printf "    <testsuite name=\"WPMgr Agent\" tests=\"%d\" assertions=\"%d\" errors=\"0\" failures=\"0\" skipped=\"0\" time=\"1.000000\">\n", n, n
    for (i = 1; i <= n; i++) {
      c = int((i - 1) / 50) + 1
      if ((i - 1) % 50 == 0) {
        if (i > 1) print "      </testsuite>"
        printf "      <testsuite name=\"WPMgr\\Agent\\Tests\\Fixture%dTest\" file=\"/repo/apps/agent/tests/Fixture%dTest.php\" tests=\"50\" assertions=\"50\" errors=\"0\" failures=\"0\" skipped=\"0\" time=\"0.100000\">\n", c, c
      }
      printf "        <testcase name=\"test_%d\" file=\"/repo/apps/agent/tests/Fixture%dTest.php\" line=\"%d\" class=\"WPMgr\\Agent\\Tests\\Fixture%dTest\" classname=\"WPMgr.Agent.Tests.Fixture%dTest\" assertions=\"1\" time=\"0.000100\"/>\n", i, c, i + 10, c, c
    }
    if (n > 0) print "      </testsuite>"
    print "    </testsuite>"
    print "  </testsuite>"
    print "</testsuites>"
  }' > "$1"
}

# The size of the recorded agent run that stopped at a fatal error, and the
# test it stopped on.
make_list  "$WORK/list-3475.xml"  3475
make_junit "$WORK/junit-3475.xml" 3475
make_junit "$WORK/junit-1769.xml" 1769
make_junit "$WORK/junit-3474.xml" 3474

# --- harness ---------------------------------------------------------------

# run_case <name> <expected-exit> <must-contain|-> <must-not-contain|-> -- <guard args...>
run_case() {
  name="$1"; want="$2"; needle="$3"; anti="$4"; shift 5   # shift past the '--'
  case "$name" in
    *"$FILTER"*) : ;;
    *) return 0 ;;
  esac

  outf="$WORK/out.txt"
  "$GUARD" "$@" > "$outf" 2>&1
  got=$?

  ok=1
  msg=""
  if [ "$got" -ne "$want" ]; then
    ok=0; msg="expected exit $want, got $got"
  fi
  if [ "$needle" != "-" ] && ! grep -qF -- "$needle" "$outf"; then
    ok=0; msg="$msg; output missing: $needle"
  fi
  if [ "$anti" != "-" ] && grep -qF -- "$anti" "$outf"; then
    ok=0; msg="$msg; output must not contain: $anti"
  fi

  if [ "$ok" -eq 1 ]; then
    PASS=$((PASS + 1)); printf 'ok   %s\n' "$name"
  else
    FAIL=$((FAIL + 1)); printf 'FAIL %s (%s)\n' "$name" "$msg"
    sed 's/^/       | /' "$outf"
  fi
}

# ===========================================================================
# A. It fires.
# ===========================================================================

# What PHPUnit 10.5 leaves after a PHP fatal error or an exit() reached from a
# test: it creates the report file when the run starts and writes the report
# into it only when the run finishes, so the file is there and empty.
: > "$WORK/junit-zero-bytes.xml"
run_case "empty-report-means-the-run-did-not-finish" 1 "did not finish" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-zero-bytes.xml"

# No report file at all: the run never started.
run_case "missing-report-means-the-run-did-not-finish" 1 "did not finish" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/no-such-junit.xml"

# The recorded run: a fatal error ended the suite at test 1769 of 3475. A run
# that stops early but still writes its report, a stop-on-failure setting for
# instance, holds that many test cases.
run_case "run-that-stopped-partway-is-incomplete" 1 "ran 1769 of 3475 listed tests; 1706 never ran" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-1769.xml"

# The boundary: one test short is as red as half the suite.
run_case "one-test-short-is-incomplete" 1 "ran 3474 of 3475 listed tests; 1 never ran" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-3474.xml"

# A well-formed report that holds no tests at all.
printf '<?xml version="1.0" encoding="UTF-8"?>\n<testsuites/>\n' > "$WORK/junit-empty-root.xml"
run_case "report-with-no-tests-is-incomplete" 1 "ran 0 of 3475 listed tests" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-empty-root.xml"

# A report with more tests than the list: the two do not describe one run.
make_list "$WORK/list-10.xml" 10
make_junit "$WORK/junit-12.xml" 12
run_case "more-tests-than-listed-is-a-mismatch" 1 "MISMATCH: the JUnit report has 12 tests but the list has 10" "$GUARD_OK" -- \
  --list "$WORK/list-10.xml" --junit "$WORK/junit-12.xml"

# A report cut off part way through writing, at a line boundary and inside a
# tag. Neither may be read as a short run, or as a complete one.
head -n 2000 "$WORK/junit-3475.xml" > "$WORK/junit-cut-lines.xml"
run_case "report-cut-off-at-a-line-is-fatal" 2 "not a whole JUnit document" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-cut-lines.xml"
head -c 50000 "$WORK/junit-3475.xml" > "$WORK/junit-cut-bytes.xml"
run_case "report-cut-off-inside-a-tag-is-fatal" 2 "not a whole JUnit document" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-cut-bytes.xml"

# Something after the closing root: not a document PHPUnit wrote.
{ cat "$WORK/junit-3475.xml"; printf 'PHP Fatal error:  late output\n'; } > "$WORK/junit-trailing.xml"
run_case "report-with-trailing-output-is-fatal" 2 "not a whole JUnit document" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-trailing.xml"

# A report that holds only a line break is not something PHPUnit writes.
printf '\n' > "$WORK/junit-blank.xml"
run_case "blank-report-is-fatal" 2 "not a whole JUnit document" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-blank.xml"

# A directory where the report should be.
mkdir "$WORK/junit-dir.xml"
run_case "report-that-is-a-directory-is-fatal" 2 "not a regular file" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-dir.xml"

# The two arguments swapped: the list read as a report, and the reverse.
run_case "list-given-as-the-report-is-fatal" 2 "not a whole JUnit document" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/list-3475.xml"
run_case "report-given-as-the-list-is-fatal" 2 "not a whole phpunit --list-tests-xml document" "$GUARD_OK" -- \
  --list "$WORK/junit-3475.xml" --junit "$WORK/junit-3475.xml"

# A list naming no tests: a run of nothing proves nothing.
printf '<?xml version="1.0"?>\n<tests/>\n' > "$WORK/list-none.xml"
run_case "list-naming-no-tests-is-fatal" 2 "names no tests" "$GUARD_OK" -- \
  --list "$WORK/list-none.xml" --junit "$WORK/junit-empty-root.xml"
make_list "$WORK/list-zero.xml" 0
run_case "list-with-an-empty-root-is-fatal" 2 "names no tests" "$GUARD_OK" -- \
  --list "$WORK/list-zero.xml" --junit "$WORK/junit-empty-root.xml"

# The text form of the list (phpunit --list-tests) instead of the XML form.
{
  printf 'PHPUnit 10.5.0 by Sebastian Bergmann and contributors.\n\nAvailable test(s):\n'
  printf ' - WPMgr\\Agent\\Tests\\FixtureTest::test_1\n'
  printf ' - WPMgr\\Agent\\Tests\\FixtureTest::test_2\n'
} > "$WORK/list-text.txt"
run_case "text-list-instead-of-xml-is-fatal" 2 "not a whole phpunit --list-tests-xml document" "$GUARD_OK" -- \
  --list "$WORK/list-text.txt" --junit "$WORK/junit-3475.xml"

# A list cut off part way through.
head -n 1000 "$WORK/list-3475.xml" > "$WORK/list-cut.xml"
run_case "list-cut-off-is-fatal" 2 "not a whole phpunit --list-tests-xml document" "$GUARD_OK" -- \
  --list "$WORK/list-cut.xml" --junit "$WORK/junit-3475.xml"

: > "$WORK/list-zero-bytes.xml"
run_case "empty-list-file-is-fatal" 2 "test list is empty" "$GUARD_OK" -- \
  --list "$WORK/list-zero-bytes.xml" --junit "$WORK/junit-3475.xml"

run_case "missing-list-is-fatal" 2 "test list not found" "$GUARD_OK" -- \
  --list "$WORK/no-such-list.xml" --junit "$WORK/junit-3475.xml"

# Invocation errors.
run_case "missing-junit-argument-is-fatal" 2 "--junit is required" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml"
run_case "missing-list-argument-is-fatal" 2 "--list is required" "$GUARD_OK" -- \
  --junit "$WORK/junit-3475.xml"
run_case "flag-without-a-value-is-fatal" 2 "--junit needs a value" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit
run_case "unknown-argument-is-fatal" 2 "unknown argument: --strict" "$GUARD_OK" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-3475.xml" --strict
run_case "no-arguments-is-fatal" 2 "--list is required" "$GUARD_OK" --

# ===========================================================================
# B. It does not over-fire. Each of these is a complete run.
# ===========================================================================

run_case "complete-run-is-green" 0 "$GUARD_OK: all 3475 listed tests ran" "$GUARD_INCOMPLETE" -- \
  --list "$WORK/list-3475.xml" --junit "$WORK/junit-3475.xml"

# Data providers: the list names each data set as its own <testCaseMethod>,
# numbered or named, and the report nests one <testsuite> per provider method
# with a <testcase> per data set. Five sets plus one plain method.
{
  printf '<?xml version="1.0"?>\n<tests>\n'
  printf ' <testCaseClass name="WPMgr\\Agent\\Tests\\ProviderTest">\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\ProviderTest::test_plain" name="test_plain" groups="default"/>\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\ProviderTest::test_numbered#0" name="test_numbered" groups="default" dataSet="#0"/>\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\ProviderTest::test_numbered#1" name="test_numbered" groups="default" dataSet="#1"/>\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\ProviderTest::test_named#known code exec" name="test_named" groups="default" dataSet="&quot;known code exec&quot;"/>\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\ProviderTest::test_named#own namespace, denied" name="test_named" groups="default" dataSet="&quot;own namespace, denied&quot;"/>\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\ProviderTest::test_named#shell segment" name="test_named" groups="default" dataSet="&quot;shell segment&quot;"/>\n'
  printf ' </testCaseClass>\n</tests>\n'
} > "$WORK/list-provider.xml"
{
  printf '<?xml version="1.0" encoding="UTF-8"?>\n<testsuites>\n'
  printf '  <testsuite name="WPMgr Agent" tests="6" assertions="6" errors="0" failures="0" skipped="0" time="0.01">\n'
  printf '    <testsuite name="WPMgr\\Agent\\Tests\\ProviderTest" file="/repo/tests/ProviderTest.php" tests="6" assertions="6" errors="0" failures="0" skipped="0" time="0.01">\n'
  printf '      <testcase name="test_plain" file="/repo/tests/ProviderTest.php" line="10" class="WPMgr\\Agent\\Tests\\ProviderTest" classname="WPMgr.Agent.Tests.ProviderTest" assertions="1" time="0.001"/>\n'
  printf '      <testsuite name="WPMgr\\Agent\\Tests\\ProviderTest::test_numbered" tests="2" assertions="2" errors="0" failures="0" skipped="0" time="0.002">\n'
  printf '        <testcase name="test_numbered with data set #0" file="/repo/tests/ProviderTest.php" line="20" class="WPMgr\\Agent\\Tests\\ProviderTest" classname="WPMgr.Agent.Tests.ProviderTest" assertions="1" time="0.001"/>\n'
  printf '        <testcase name="test_numbered with data set #1" file="/repo/tests/ProviderTest.php" line="20" class="WPMgr\\Agent\\Tests\\ProviderTest" classname="WPMgr.Agent.Tests.ProviderTest" assertions="1" time="0.001"/>\n'
  printf '      </testsuite>\n'
  printf '      <testsuite name="WPMgr\\Agent\\Tests\\ProviderTest::test_named" tests="3" assertions="3" errors="0" failures="0" skipped="0" time="0.003">\n'
  printf '        <testcase name="test_named with data set &quot;known code exec&quot;" file="/repo/tests/ProviderTest.php" line="30" class="WPMgr\\Agent\\Tests\\ProviderTest" classname="WPMgr.Agent.Tests.ProviderTest" assertions="1" time="0.001"/>\n'
  printf '        <testcase name="test_named with data set &quot;own namespace, denied&quot;" file="/repo/tests/ProviderTest.php" line="30" class="WPMgr\\Agent\\Tests\\ProviderTest" classname="WPMgr.Agent.Tests.ProviderTest" assertions="1" time="0.001"/>\n'
  printf '        <testcase name="test_named with data set &quot;shell segment&quot;" file="/repo/tests/ProviderTest.php" line="30" class="WPMgr\\Agent\\Tests\\ProviderTest" classname="WPMgr.Agent.Tests.ProviderTest" assertions="1" time="0.001"/>\n'
  printf '      </testsuite>\n'
  printf '    </testsuite>\n  </testsuite>\n</testsuites>\n'
} > "$WORK/junit-provider.xml"
run_case "data-provider-cases-count-one-each" 0 "$GUARD_OK: all 6 listed tests ran" "$GUARD_INCOMPLETE" -- \
  --list "$WORK/list-provider.xml" --junit "$WORK/junit-provider.xml"

# Skipped, failed and errored tests ran. Whether they passed is the test
# step's verdict, not this one. Their messages carry escaped markup that names
# both elements this guard counts; an escaped "&lt;testcase" is text, not an
# element, and must not be counted.
{
  printf '<?xml version="1.0"?>\n<tests>\n <testCaseClass name="WPMgr\\Agent\\Tests\\OutcomeTest">\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\OutcomeTest::test_skipped" name="test_skipped" groups="default"/>\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\OutcomeTest::test_failed" name="test_failed" groups="default"/>\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\OutcomeTest::test_errored#&lt;testCaseMethod&gt;" name="test_errored" groups="default" dataSet="&quot;&lt;testCaseMethod&gt;&quot;"/>\n'
  printf ' </testCaseClass>\n</tests>\n'
} > "$WORK/list-outcomes.xml"
{
  printf '<?xml version="1.0" encoding="UTF-8"?>\n<testsuites>\n'
  printf '  <testsuite name="WPMgr\\Agent\\Tests\\OutcomeTest" file="/repo/tests/OutcomeTest.php" tests="3" assertions="1" errors="1" failures="1" skipped="1" time="0.01">\n'
  printf '    <testcase name="test_skipped" file="/repo/tests/OutcomeTest.php" line="10" class="WPMgr\\Agent\\Tests\\OutcomeTest" classname="WPMgr.Agent.Tests.OutcomeTest" assertions="0" time="0.001">\n'
  printf '      <skipped/>\n'
  printf '    </testcase>\n'
  printf '    <testcase name="test_failed" file="/repo/tests/OutcomeTest.php" line="20" class="WPMgr\\Agent\\Tests\\OutcomeTest" classname="WPMgr.Agent.Tests.OutcomeTest" assertions="1" time="0.001">\n'
  printf '      <failure type="PHPUnit\\Framework\\ExpectationFailedException">WPMgr\\Agent\\Tests\\OutcomeTest::test_failed\n'
  printf 'Failed asserting that two strings are identical.\n'
  printf '&lt;testcase name="a"/&gt; &lt;testcase&gt; &lt;testsuites&gt; &lt;/testsuites&gt;\n'
  printf '\n/repo/tests/OutcomeTest.php:22</failure>\n'
  printf '    </testcase>\n'
  printf '    <testcase name="test_errored with data set &quot;&lt;testCaseMethod&gt;&quot;" file="/repo/tests/OutcomeTest.php" line="30" class="WPMgr\\Agent\\Tests\\OutcomeTest" classname="WPMgr.Agent.Tests.OutcomeTest" assertions="0" time="0.001">\n'
  printf '      <error type="Error">WPMgr\\Agent\\Tests\\OutcomeTest::test_errored\n'
  printf 'Error: Call to undefined function &lt;testcase&gt;()\n'
  printf '\n/repo/tests/OutcomeTest.php:32</error>\n'
  printf '      <system-out>&lt;testcase name="printed by the test"/&gt;</system-out>\n'
  printf '    </testcase>\n'
  printf '  </testsuite>\n</testsuites>\n'
} > "$WORK/junit-outcomes.xml"
run_case "skipped-failed-and-errored-tests-ran" 0 "$GUARD_OK: all 3 listed tests ran" "$GUARD_INCOMPLETE" -- \
  --list "$WORK/list-outcomes.xml" --junit "$WORK/junit-outcomes.xml"

# PHPT tests: listed as <phptFile>, reported as a <testcase> like any other.
{
  printf '<?xml version="1.0"?>\n<tests>\n'
  printf ' <testCaseClass name="WPMgr\\Agent\\Tests\\MixedTest">\n'
  printf '  <testCaseMethod id="WPMgr\\Agent\\Tests\\MixedTest::test_one" name="test_one" groups="default"/>\n'
  printf ' </testCaseClass>\n'
  printf ' <phptFile path="/repo/tests/phpt/boot.phpt"/>\n'
  printf '</tests>\n'
} > "$WORK/list-phpt.xml"
{
  printf '<?xml version="1.0" encoding="UTF-8"?>\n<testsuites>\n'
  printf '  <testsuite name="WPMgr Agent" tests="2" assertions="2" errors="0" failures="0" skipped="0" time="0.01">\n'
  printf '    <testcase name="test_one" file="/repo/tests/MixedTest.php" line="10" class="WPMgr\\Agent\\Tests\\MixedTest" classname="WPMgr.Agent.Tests.MixedTest" assertions="1" time="0.001"/>\n'
  printf '    <testcase name="/repo/tests/phpt/boot.phpt" file="/repo/tests/phpt/boot.phpt" assertions="1" time="0.002"/>\n'
  printf '  </testsuite>\n</testsuites>\n'
} > "$WORK/junit-phpt.xml"
run_case "phpt-tests-are-counted" 0 "$GUARD_OK: all 2 listed tests ran" "$GUARD_INCOMPLETE" -- \
  --list "$WORK/list-phpt.xml" --junit "$WORK/junit-phpt.xml"

# Layout must not matter: both documents on a single line, and both with CRLF
# line endings. Elements are counted, not lines.
tr -d '\n' < "$WORK/list-3475.xml"  > "$WORK/list-one-line.xml"
tr -d '\n' < "$WORK/junit-3475.xml" > "$WORK/junit-one-line.xml"
run_case "single-line-documents-are-green" 0 "$GUARD_OK: all 3475 listed tests ran" "$GUARD_INCOMPLETE" -- \
  --list "$WORK/list-one-line.xml" --junit "$WORK/junit-one-line.xml"
run_case "single-line-report-cut-short-is-still-red" 1 "ran 1769 of 3475 listed tests" "$GUARD_OK" -- \
  --list "$WORK/list-one-line.xml" --junit "$WORK/junit-1769.xml"
awk '{ printf "%s\r\n", $0 }' "$WORK/list-3475.xml"  > "$WORK/list-crlf.xml"
awk '{ printf "%s\r\n", $0 }' "$WORK/junit-3475.xml" > "$WORK/junit-crlf.xml"
run_case "crlf-documents-are-green" 0 "$GUARD_OK: all 3475 listed tests ran" "$GUARD_INCOMPLETE" -- \
  --list "$WORK/list-crlf.xml" --junit "$WORK/junit-crlf.xml"

# ===========================================================================

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
[ "$PASS" -gt 0 ] || { printf 'no cases ran (filter %s matched nothing)\n' "$FILTER" >&2; exit 2; }
exit 0
