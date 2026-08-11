#!/usr/bin/env bash
# scripts/check-commit-hygiene_test.sh
#
# The regression suite for scripts/check-commit-hygiene.sh.
#
# WHY THIS EXISTS. The gate used to live inline in .github/workflows/ci.yml,
# where nobody could run it, and it FAILED OPEN: a `git rev-list` that failed
# produced an empty command substitution, the loop never ran, and the step
# printed "OK: no assistant trailers or session URLs in this PR's commits" and
# exited 0 having examined nothing. The two cases it could not tell apart are
# the two this suite pins hardest:
#
#   rev-list FAILS               -> exit 2, and the output must NOT say OK
#   rev-list succeeds, no output -> exit 0, a legitimately empty range
#
# HOW IT WORKS. Each case builds a real throwaway git repository with real
# commits and real messages, runs the real script against it, and asserts the
# exact exit code plus what the output does and does not say. No mocking: the
# shallow-clone case really does `git clone --depth 1`, and the missing-base
# case really does point at a sha that does not exist.
#
# RUN IT:
#   scripts/check-commit-hygiene_test.sh             # everything
#   scripts/check-commit-hygiene_test.sh empty       # only cases matching "empty"
#
# PROVE THE SUITE IS NOT VACUOUS. It ships the archived pre-fix implementation
# as a fixture. Keep a copy of it, point the suite at that copy, and watch every
# fail-open case go red while the offender cases stay green:
#   WPMGR_COMMIT_HYGIENE_FIXTURE_OUT=/tmp/old-inline-gate.sh \
#     scripts/check-commit-hygiene_test.sh >/dev/null
#   WPMGR_COMMIT_HYGIENE_SCRIPT=/tmp/old-inline-gate.sh \
#     scripts/check-commit-hygiene_test.sh
# 17 of 22 cases fail, and all 8 `failopen:` ones go from exit 2 to exit 0.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it runs the same
# on a darwin laptop with BSD grep/sed and on the ubuntu CI runner with the GNU
# ones. No mapfile, no associative arrays, no sed -i, no grep -P.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_COMMIT_HYGIENE_SCRIPT:-$HERE/check-commit-hygiene.sh}"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  echo "no guard script at $GUARD" >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-commit-hygiene-suite.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
SKIPPED=0
FAILED_NAMES=''

# ---------------------------------------------------------------------------
# Repository construction
# ---------------------------------------------------------------------------

# newrepo NAME -> prints the path
newrepo() {
  _d="$WORK/$1"
  mkdir -p "$_d"
  git -C "$_d" init -q >/dev/null 2>&1
  git -C "$_d" config user.email "suite@example.invalid"
  git -C "$_d" config user.name "Hygiene Suite"
  git -C "$_d" config commit.gpgsign false
  git -C "$_d" config gc.auto 0
  printf '%s\n' "$1" > "$_d/README"
  git -C "$_d" add -A >/dev/null 2>&1
  git -C "$_d" commit -q --no-verify -m "root commit" >/dev/null 2>&1
  printf '%s\n' "$_d"
}

# commit DIR MESSAGE  (message may be multi-line; it is passed on stdin so
# trailers and URLs survive verbatim)
commit() {
  _d="$1"
  _msg="$2"
  _n=$(( $(git -C "$_d" rev-list --count HEAD) + 1 ))
  printf 'line %s\n' "$_n" >> "$_d/README"
  git -C "$_d" add -A >/dev/null 2>&1
  printf '%s\n' "$_msg" | git -C "$_d" commit -q --no-verify -F - >/dev/null 2>&1
}

# The session URL is assembled at runtime rather than written as a literal, so
# this file itself never carries a copyable link and never trips a grep-based
# sweep over the repository.
SESSION_URL="https://claude.ai/code/""session_01EXAMPLEEXAMPLEEXAMPLE"

# ---------------------------------------------------------------------------
# Assertions
#
#   run_case NAME WANT_EXIT BASE HEAD DIR [+MUST-CONTAIN ...] [-MUST-NOT-CONTAIN ...]
# ---------------------------------------------------------------------------
run_case() {
  _name="$1"
  _want="$2"
  _base="$3"
  _head="$4"
  _dir="$5"
  shift 5

  if [ -n "$FILTER" ]; then
    case "$_name" in
      *"$FILTER"*) : ;;
      *)
        SKIPPED=$((SKIPPED + 1))
        return 0
        ;;
    esac
  fi

  _out="$("$GUARD" "$_base" "$_head" "$_dir" 2>&1)"
  _code=$?
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

# ===========================================================================
# THE FAIL-OPEN. rev-list cannot be allowed to fail quietly.
#
# Every case here is a real way rev-list fails in production. In each one the
# old implementation printed OK and exited 0 over zero commits examined.
# ===========================================================================

t="$(newrepo failopen-missing-base)"
commit "$t" "feat: something ordinary"
run_case "failopen: a base sha that does not exist is an ERROR, not an empty range" \
  2 "0000000000000000000000000000000000000000" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+is not present in this checkout" "+ZERO commits" "-OK:"

t="$(newrepo failopen-missing-head)"
commit "$t" "feat: something ordinary"
run_case "failopen: a head sha that does not exist is an ERROR (the force-push case)" \
  2 "$(git -C "$t" rev-parse HEAD~1)" "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" "$t" \
  "+is not present in this checkout" "-OK:"

t="$(newrepo failopen-both-missing)"
run_case "failopen: both ends missing names both, and still refuses to pass" \
  2 "0000000000000000000000000000000000000000" "1111111111111111111111111111111111111111" "$t" \
  "+the base commit 0000000" "+and the head commit 1111111" "+are not present" "-OK:"

# A genuine shallow clone. The base commit exists upstream and is absent here,
# which is exactly what a depth-limited checkout does to this gate.
src="$(newrepo failopen-shallow-src)"
commit "$src" "one"
commit "$src" "two"
commit "$src" "three"
commit "$src" "four"
deep_base="$(git -C "$src" rev-parse HEAD~4)"
git -C "$WORK" clone -q --depth 1 "file://$src" failopen-shallow-clone >/dev/null 2>&1
shallow="$WORK/failopen-shallow-clone"
run_case "failopen: a shallow clone that cannot reach the base is an ERROR" \
  2 "$deep_base" "$(git -C "$shallow" rev-parse HEAD)" "$shallow" \
  "+shallow" "-OK:"

run_case "failopen: a directory that is not a git repository is an ERROR" \
  2 "HEAD~1" "HEAD" "$WORK" \
  "+is not a git repository" "-OK:"

run_case "failopen: a directory that does not exist at all is an ERROR" \
  2 "HEAD~1" "HEAD" "$WORK/no-such-dir" \
  "+is not a directory" "-OK:"

run_case "failopen: an empty base sha (a non-pull_request event) is an ERROR" \
  2 "" "HEAD" "$t" \
  "+no base sha was given" "-OK:"

run_case "failopen: an empty head sha is an ERROR" \
  2 "HEAD" "" "$t" \
  "+no head sha was given" "-OK:"

# ===========================================================================
# THE OTHER HALF. An enumerable range that happens to be empty is a real
# answer, and it must pass. Conflating this with the failure above is how a
# naive `set -e` fix breaks honest work.
# ===========================================================================

t="$(newrepo empty-range-identical)"
commit "$t" "feat: on the base already"
h="$(git -C "$t" rev-parse HEAD)"
run_case "empty: base == head enumerates to nothing and PASSES" \
  0 "$h" "$h" "$t" \
  "+is empty" "-examined"

# A pull request whose head is an ancestor of its base (fully merged already)
# also enumerates to nothing.
t="$(newrepo empty-range-ancestor)"
commit "$t" "feat: one"
commit "$t" "feat: two"
run_case "empty: a head already contained in the base enumerates to nothing and PASSES" \
  0 "$(git -C "$t" rev-parse HEAD)" "$(git -C "$t" rev-parse HEAD~1)" "$t" \
  "+is empty"

# The empty-range pass must not be reachable by pretending an offender is not
# there: an empty range over a history that is full of offenders still passes,
# because the range is what is being judged.
t="$(newrepo empty-range-dirty-history)"
commit "$t" "feat: dirty

$SESSION_URL"
h="$(git -C "$t" rev-parse HEAD)"
run_case "empty: an empty range over a dirty history still PASSES (only the range is judged)" \
  0 "$h" "$h" "$t" \
  "+is empty" "-ERROR"

# ===========================================================================
# THE GATE ITSELF FIRES.
# ===========================================================================

t="$(newrepo offender-session-url)"
commit "$t" "feat: add a thing

$SESSION_URL"
run_case "fires: a session URL in the range fails" \
  1 "$(git -C "$t" rev-parse HEAD~1)" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+carries a Claude Code session URL" "+must be rewritten" "-OK:"

t="$(newrepo offender-coauthor)"
commit "$t" "feat: add a thing

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
run_case "fires: a Co-Authored-By Claude trailer fails" \
  1 "$(git -C "$t" rev-parse HEAD~1)" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+assistant attribution trailer" "-OK:"

t="$(newrepo offender-claude-session-trailer)"
commit "$t" "feat: add a thing

Claude-Session: whatever"
run_case "fires: a Claude-Session trailer fails" \
  1 "$(git -C "$t" rev-parse HEAD~1)" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+assistant attribution trailer" "-OK:"

t="$(newrepo offender-lowercase)"
commit "$t" "feat: add a thing

co-authored-by: claude opus 5 <noreply@anthropic.com>"
run_case "fires: the trailer match is case-insensitive" \
  1 "$(git -C "$t" rev-parse HEAD~1)" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+assistant attribution trailer"

# Not just the tip. The old loop read every commit too, but a fix that
# accidentally narrows to HEAD would pass this repository's worst case.
t="$(newrepo offender-mid-range)"
base="$(git -C "$t" rev-parse HEAD)"
commit "$t" "feat: clean one"
commit "$t" "feat: the bad one

$SESSION_URL"
commit "$t" "feat: clean two"
commit "$t" "feat: clean three"
run_case "fires: an offender in the MIDDLE of the range is caught, not only the tip" \
  1 "$base" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+session URL" "+the 4 commit(s) examined"

# ===========================================================================
# IT DOES NOT OVER-FIRE. A guard that fails honest work gets switched off.
# ===========================================================================

t="$(newrepo clean-range)"
base="$(git -C "$t" rev-parse HEAD)"
commit "$t" "feat: one"
commit "$t" "fix: two"
commit "$t" "docs: three"
run_case "clean: a range of ordinary commits PASSES and says how many it read" \
  0 "$base" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+examined 3 commit(s)" "-ERROR"

# The 149 commits already in main's history are the whole reason this gate
# reads a range and not a history. If it ever widens, this goes red.
t="$(newrepo clean-dirty-history-behind)"
commit "$t" "feat: an old commit from before the rule

$SESSION_URL"
commit "$t" "chore: another old one

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
base="$(git -C "$t" rev-parse HEAD)"
commit "$t" "feat: new work, clean"
commit "$t" "fix: more new work, clean"
run_case "clean: offenders BEHIND the base do not fail the pull request" \
  0 "$base" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+examined 2 commit(s)" "-ERROR"

t="$(newrepo clean-human-coauthor)"
base="$(git -C "$t" rev-parse HEAD)"
commit "$t" "feat: pairing

Co-Authored-By: A Human <human@example.invalid>"
run_case "clean: a human Co-Authored-By trailer is not an assistant trailer" \
  0 "$base" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+examined 1 commit(s)"

t="$(newrepo clean-mentions-word)"
base="$(git -C "$t" rev-parse HEAD)"
commit "$t" "docs: explain why session URLs are refused

Mentions Claude Code and sessions in prose, carries neither a link nor a trailer."
run_case "clean: prose that merely mentions the tool is not an offence" \
  0 "$base" "$(git -C "$t" rev-parse HEAD)" "$t" \
  "+examined 1 commit(s)" "-ERROR"

# ===========================================================================
# THE SUITE IS NOT VACUOUS.
#
# This writes the pre-fix implementation out verbatim and runs it against the
# same missing-base repository the first case uses. It exits 0 and prints OK.
# That is the defect, recorded, and it is why the cases above assert on the
# absence of "OK:" and not merely on an exit code.
#
# The fixture is written inside the suite's own scratch directory and removed
# with it. Set WPMGR_COMMIT_HYGIENE_FIXTURE_OUT to keep a copy somewhere, which
# is how you feed it back in via WPMGR_COMMIT_HYGIENE_SCRIPT.
# ===========================================================================
OLD_GATE="${WPMGR_COMMIT_HYGIENE_FIXTURE_OUT:-$WORK/old-inline-gate.sh}"
cat > "$OLD_GATE" <<'OLDEOF'
#!/usr/bin/env bash
# ARCHIVED: the pre-fix .github/workflows/ci.yml inline step, verbatim except
# for reading its shas from argv and cd-ing to a repo. DO NOT RESTORE.
set -uo pipefail
BASE_SHA="$1"; HEAD_SHA="$2"; cd "$3" || exit 9
url_re='claude\.ai/code/session'
trailer_re='^[[:space:]]*(Co-Authored-By:[[:space:]]*Claude|Claude-Session:)'
bad=0
for sha in $(git rev-list "$BASE_SHA".."$HEAD_SHA"); do
  body="$(git log -1 --format='%B' "$sha")"
  if printf '%s' "$body" | grep -qiE "$url_re"; then
    echo "ERROR: $(git log -1 --format='%h %s' "$sha") carries a Claude Code session URL."
    bad=1
  fi
  if printf '%s' "$body" | grep -qiE "$trailer_re"; then
    echo "ERROR: $(git log -1 --format='%h %s' "$sha") carries an assistant attribution trailer."
    bad=1
  fi
done
if [ "$bad" -ne 0 ]; then exit 1; fi
echo "OK: no assistant trailers or session URLs in this PR's commits."
OLDEOF
chmod +x "$OLD_GATE"

t="$(newrepo notvacuous-old-implementation)"
commit "$t" "feat: something ordinary"
old_out="$("$OLD_GATE" "0000000000000000000000000000000000000000" "$(git -C "$t" rev-parse HEAD)" "$t" 2>&1)"
old_code=$?
if [ "$old_code" -eq 0 ] && printf '%s\n' "$old_out" | grep -qF "OK: no assistant trailers"; then
  PASSED=$((PASSED + 1))
  printf 'ok   %s\n' "not vacuous: the archived inline gate really did exit 0 and print OK on a broken base"
else
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES  not vacuous: archived inline gate no longer reproduces the fail-open
"
  printf 'FAIL %s\n    the archived fixture exited %s and said: %s\n' \
    "not vacuous: the archived inline gate should reproduce the fail-open" "$old_code" "$old_out"
fi

# And the same fixture still catches a real offender, which is the point: the
# bug was never that it could not see, it was that it could not tell blindness
# from an all-clear.
t="$(newrepo notvacuous-old-still-fires)"
commit "$t" "feat: bad

$SESSION_URL"
"$OLD_GATE" "$(git -C "$t" rev-parse HEAD~1)" "$(git -C "$t" rev-parse HEAD)" "$t" >/dev/null 2>&1
old_code=$?
if [ "$old_code" -eq 1 ]; then
  PASSED=$((PASSED + 1))
  printf 'ok   %s\n' "not vacuous: the archived inline gate did catch offenders it could see (exit 1)"
else
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES  not vacuous: archived inline gate no longer catches a visible offender
"
  printf 'FAIL %s (exit %s)\n' "not vacuous: archived inline gate should exit 1 on a visible offender" "$old_code"
fi

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
