#!/usr/bin/env bash
# scripts/check-commit-hygiene.sh
#
# Refuses a pull request whose OWN commits carry an assistant attribution
# trailer or a Claude Code session URL.
#
# WHY THE RULE EXISTS. This repository is public, and a Claude Code session URL
# in a commit message is published with it, permanently, to everyone. 149
# commits already in main's history carry one, from 2026-06-22 to 2026-07-28.
# Those are being left alone deliberately: removing them means rewriting public
# history, which changes every SHA since June, orphans the release tags that
# point at them, and breaks every existing clone and fork. So the rule starts at
# the gate instead, and this script is the gate. It reads ONLY the commits a
# pull request itself adds, never the history behind them, so the existing 149
# cannot fail anybody's unrelated work.
#
# WHY IT IS A SCRIPT AND NOT A YAML BLOCK SCALAR. It used to be inline in
# .github/workflows/ci.yml, and it FAILED OPEN. The loop was
#
#     for sha in $(git rev-list "$BASE_SHA".."$HEAD_SHA"); do
#
# under `set -uo pipefail` with no `-e`. Command substitution swallows the exit
# status, so when rev-list FAILED the substitution produced an empty string, the
# loop body never ran, the offender counter stayed at zero, and the step printed
# "OK: no assistant trailers or session URLs in this PR's commits" and exited 0.
# It announced a clean bill of health over its own error, having examined zero
# commits. rev-list fails on a force-pushed head, on a shallow clone, when the
# base commit is missing from the fetch, and on a cross-fork pull request whose
# base sha is not present locally. All four are ordinary.
#
# The two cases the old code could not tell apart, and this one must:
#
#   rev-list FAILED               -> nothing was checked. That is an ERROR.
#   rev-list SUCCEEDED, no output -> a real, legitimately empty range. PASS.
#
# The success line is therefore reachable only from a range that was actually
# enumerated, and it prints how many commits it read, so "it passed" and "it
# looked" are the same claim in the log.
#
# USAGE
#   scripts/check-commit-hygiene.sh <base-sha> <head-sha> [repo-dir]
#
#   repo-dir defaults to the current directory. Both shas may be any revision
#   git can resolve to a commit.
#
# EXIT CODES
#   0  the range was enumerated and every commit in it is clean (an enumerable
#      but empty range is clean, and says so in different words)
#   1  at least one commit carries a session URL or an assistant trailer
#   2  the range could NOT be enumerated, or a commit message could not be read,
#      so the answer is unknown. Unknown is a failure, never a pass.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop with BSD grep/sed and on the ubuntu CI runner with the
# GNU ones. No mapfile, no associative arrays, no sed -i, no grep -P.

set -uo pipefail

BASE="${1:-${BASE_SHA:-}}"
HEAD="${2:-${HEAD_SHA:-}}"
REPO="${3:-.}"

# A session URL is the one that actually matters: it is a live link published to
# a public repository and it cannot be recalled. The trailers are a house style
# rule. Both are checked, and the message says which is which.
url_re='claude\.ai/code/session'
trailer_re='^[[:space:]]*(Co-Authored-By:[[:space:]]*Claude|Claude-Session:)'

# ---------------------------------------------------------------------------
# Everything below distinguishes "checked and clean" from "could not check".
# ---------------------------------------------------------------------------

cannot_check() {
  # Print the reason, then the standing explanation of why this is fatal rather
  # than a shrug, then leave with 2.
  printf 'ERROR: %s\n' "$1"
  shift
  for _line in "$@"; do
    printf '       %s\n' "$_line"
  done
  printf '\n'
  printf '       This gate examined ZERO commits, so it cannot say whether this pull\n'
  printf '       request is clean. It fails rather than passes: a check that reports\n'
  printf '       success over its own error is worse than no check at all.\n'
  exit 2
}

if [ -z "$BASE" ]; then
  cannot_check "no base sha was given." \
    "Usage: scripts/check-commit-hygiene.sh <base-sha> <head-sha> [repo-dir]" \
    "In CI this comes from github.event.pull_request.base.sha, which is empty" \
    "on any event that is not a pull_request."
fi
if [ -z "$HEAD" ]; then
  cannot_check "no head sha was given." \
    "Usage: scripts/check-commit-hygiene.sh <base-sha> <head-sha> [repo-dir]" \
    "In CI this comes from github.event.pull_request.head.sha."
fi
if [ ! -d "$REPO" ]; then
  cannot_check "$REPO is not a directory, so there is no repository to read."
fi
if ! git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1; then
  cannot_check "$REPO is not a git repository (or git is unavailable)."
fi

# Resolve each end of the range on its own, so the message can name WHICH one is
# missing. rev-list would only say the range is bad.
resolvable() {
  git -C "$REPO" rev-parse --verify --quiet "$1^{commit}" >/dev/null 2>&1
}

absent=''
n_absent=0
if ! resolvable "$BASE"; then
  absent="the base commit $BASE"
  n_absent=1
fi
if ! resolvable "$HEAD"; then
  if [ "$n_absent" -eq 1 ]; then
    absent="$absent and the head commit $HEAD"
    n_absent=2
  else
    absent="the head commit $HEAD"
    n_absent=1
  fi
fi
if [ "$n_absent" -gt 0 ]; then
  verb="is"
  [ "$n_absent" -eq 2 ] && verb="are"
  cannot_check "$absent $verb not present in this checkout." \
    "The usual causes, all of them ordinary:" \
    "  - the branch was force-pushed after the event fired, so the head sha is gone;" \
    "  - the clone is shallow and does not reach the base commit (set fetch-depth: 0);" \
    "  - the pull request comes from a fork whose commits were never fetched here." \
    "Fetch the missing commit and run again."
fi

# Belt and braces. Both ends resolve, but rev-list can still fail on a damaged
# object store or a repository-level error, and its status must not be swallowed
# by a command substitution the way the old inline loop swallowed it.
errfile="$(mktemp "${TMPDIR:-/tmp}/wpmgr-commit-hygiene.XXXXXX")" || {
  cannot_check "could not create a temporary file to capture git's stderr."
}
trap 'rm -f "$errfile"' EXIT INT TERM

rc=0
shas="$(git -C "$REPO" rev-list "$BASE".."$HEAD" 2>"$errfile")" || rc=$?

if [ "$rc" -ne 0 ]; then
  printf 'ERROR: could not enumerate this pull request'"'"'s commits.\n'
  printf '       git rev-list %s..%s exited %s.\n' "$BASE" "$HEAD" "$rc"
  if [ -s "$errfile" ]; then
    sed 's/^/       git: /' "$errfile"
  fi
  printf '\n'
  printf '       This gate examined ZERO commits, so it cannot say whether this pull\n'
  printf '       request is clean. It fails rather than passes: a check that reports\n'
  printf '       success over its own error is worse than no check at all.\n'
  exit 2
fi

# rev-list SUCCEEDED. From here on the answer is known, and an empty result is a
# real answer: the range legitimately contains no commits. It passes, and it
# says so in words that cannot be mistaken for the examined-and-clean case.
if [ -z "$shas" ]; then
  printf 'OK: the range %s..%s is empty, so this pull request adds no commits of its own.\n' "$BASE" "$HEAD"
  exit 0
fi

bad=0
examined=0
for sha in $shas; do
  body="$(git -C "$REPO" log -1 --format='%B' "$sha")" || {
    cannot_check "could not read the commit message of $sha." \
      "The range enumerated but this object would not be read, so the answer for" \
      "this pull request is incomplete."
  }
  examined=$((examined + 1))

  if printf '%s\n' "$body" | grep -qiE "$url_re"; then
    printf 'ERROR: %s\n' "$(git -C "$REPO" log -1 --format='%h %s' "$sha")"
    printf '       carries a Claude Code session URL. This repository is public, so that\n'
    printf '       link would be published with the commit and cannot be taken back.\n'
    bad=$((bad + 1))
  fi
  if printf '%s\n' "$body" | grep -qiE "$trailer_re"; then
    printf 'ERROR: %s\n' "$(git -C "$REPO" log -1 --format='%h %s' "$sha")"
    printf '       carries an assistant attribution trailer, which this project does not use.\n'
    bad=$((bad + 1))
  fi
done

if [ "$bad" -ne 0 ]; then
  printf '\n'
  printf '%s of the %s commit(s) examined must be rewritten. Reword and force-push:\n' "$bad" "$examined"
  printf '  git rebase -i %s     # reword each one\n' "$BASE"
  printf '  git push --force-with-lease\n'
  printf '\n'
  printf 'Only this pull request'"'"'s commits are read, so nothing already on main is involved.\n'
  exit 1
fi

# The only path to this line runs through a rev-list that succeeded and a loop
# that read every commit it returned. The count is the evidence.
printf 'OK: examined %s commit(s); none carry an assistant trailer or a session URL.\n' "$examined"
exit 0
