#!/usr/bin/env bash
# Install, or truthfully report, the committed git hooks.
#
# WHY THIS IS A SCRIPT AND NOT TWO LINES IN THE MAKEFILE.
# `.githooks/pre-push` is committed; `core.hooksPath` is repo-local config and
# repo-local config is NEVER committed. So the default state of every fresh
# clone is: the hook is in the tree, and git ignores it. That was measured, not
# assumed - in a fresh clone `git config --get core.hooksPath` is empty and
# `git push origin HEAD:refs/heads/main` exits 0 and lands the commit.
#
# The first version of this shipped as a Makefile line plus a `grep` in the test
# suite that asserted the STRING `git config core.hooksPath .githooks` appeared
# in two files. That suite passed, in full, inside a clone with no hook
# installed at all. A check that passes when the thing it checks is absent is
# this repository's signature defect, so what is asserted here is the hook
# RUNNING, resolved from the checkout this is invoked in.
#
#   install   point core.hooksPath at an absolute .githooks and verify it took
#   status    print INSTALLED / NOT INSTALLED / COULD NOT CHECK; never fails
#   check     the same, but exit non-zero unless it is INSTALLED (the gate)
#
# EXIT: 0 installed, 1 not installed, 2 could not check. `status` always exits 0
# so a SessionStart hook cannot be broken by it; `check` propagates.
#
# Test: scripts/claude/guards_test.sh
set -uo pipefail

mode="${1:-status}"

# --- resolution -------------------------------------------------------------
# Three states, and collapsing "could not check" into "NOT INSTALLED" is the
# defect this file exists to avoid: a definite negative from a measurement that
# never happened is worse than admitting the measurement failed. Same idiom as
# the toolchain block in session-brief.sh, for the same reason.
CANNOT=""   # non-empty: why the question could not be answered
WHY=""      # non-empty: why it is not installed
HOOKPATH="" # the resolved pre-push, when there is one

hooks_dir_for_install() {
  # An ABSOLUTE path, derived from the common dir, because a RELATIVE
  # core.hooksPath is resolved by git against the top of whatever working tree
  # is running the hook. That was the bug: `.githooks` is only present in a tree
  # checked out at or after the hook's commit, so every linked worktree, tag
  # checkout, bisect and detached HEAD from before it silently ran no hook while
  # config still said "installed". `git rev-parse --git-common-dir` is the same
  # directory from every linked worktree, and its parent is the main checkout.
  local common parent
  common=$(git rev-parse --git-common-dir 2>/dev/null) || return 1
  [ -n "$common" ] || return 1
  common=$(cd "$common" 2>/dev/null && pwd -P) || return 1
  parent=$(dirname "$common")
  [ -d "$parent/.githooks" ] || return 1
  printf '%s' "$parent/.githooks"
}

resolve() {
  if ! command -v git >/dev/null 2>&1; then
    CANNOT="git is not on PATH"; return
  fi
  if ! git rev-parse --git-dir >/dev/null 2>&1; then
    CANNOT="'$PWD' is not inside a git worktree"; return
  fi

  local hp top
  hp=$(git config --get core.hooksPath 2>/dev/null)
  if [ -z "$hp" ]; then
    WHY="core.hooksPath is unset, so git runs no hook from this repository at all"
    return
  fi
  if [ "${hp#/}" = "$hp" ]; then
    # Relative: git resolves it against the top of the working tree the hook
    # runs in, so it is resolved the same way here rather than against \$PWD.
    top=$(git rev-parse --show-toplevel 2>/dev/null)
    if [ -z "$top" ]; then
      CANNOT="core.hooksPath is the relative path '$hp' and this checkout has no working tree to resolve it against"
      return
    fi
    hp="$top/$hp"
  fi
  HOOKPATH="$hp/pre-push"

  if [ ! -e "$HOOKPATH" ]; then
    WHY="core.hooksPath resolves to '$hp', and there is no pre-push in it"
    return
  fi
  if [ ! -x "$HOOKPATH" ]; then
    WHY="'$HOOKPATH' exists but is not executable, and git skips a hook it cannot execute"
    return
  fi

  # Presence is not protection. Drive the resolved hook with the stdin
  # githooks(5) specifies and require it to actually refuse main and actually
  # allow a branch: a truncated, emptied or replaced hook is executable too.
  local zero=0000000000000000000000000000000000000000
  local sha=1111111111111111111111111111111111111111
  if printf 'refs/heads/main %s refs/heads/main %s\n' "$sha" "$zero" \
       | "$HOOKPATH" origin probe://none >/dev/null 2>&1; then
    WHY="'$HOOKPATH' runs but ALLOWS a push to refs/heads/main, so it is not the hook this repository ships"
    return
  fi
  if ! printf 'refs/heads/fix/x %s refs/heads/fix/x %s\n' "$sha" "$zero" \
       | "$HOOKPATH" origin probe://none >/dev/null 2>&1; then
    WHY="'$HOOKPATH' refuses an ordinary branch push, so it would block every branch and get switched off"
    return
  fi
}

fix_line() {
  # One command, and it is the whole fix. Printed with every negative report:
  # a warning that does not carry its remedy gets read and not acted on.
  printf 'make hooks'
}

report() {
  if [ -n "$CANNOT" ]; then
    echo "- pre-push hook: COULD NOT CHECK - $CANNOT."
    echo "  Do NOT read this as installed. Resolve it, then re-check with '$(fix_line)'."
    return 2
  fi
  if [ -n "$WHY" ]; then
    echo "- pre-push hook: NOT INSTALLED. $WHY."
    echo "  A push to main from this checkout is NOT refused by anything: branch"
    echo "  protection on main leaves enforce_admins false, so nothing server-side"
    echo "  catches it either. This is the default state of every fresh clone."
    echo "  FIX, one command:  $(fix_line)"
    return 1
  fi
  echo "- pre-push hook: INSTALLED ($HOOKPATH). A push that would land on main is refused; 'git push --no-verify' skips it."
  return 0
}

case "$mode" in
  install)
    dir=$(hooks_dir_for_install) || {
      echo "FAIL: could not resolve a .githooks directory from 'git rev-parse --git-common-dir' in '$PWD'." >&2
      echo "      Run this from inside the checkout, on a commit that carries .githooks/." >&2
      exit 2
    }
    git config core.hooksPath "$dir"
    echo "core.hooksPath = $(git config --get core.hooksPath)"
    # Verify what was just installed, through the same resolution a session
    # uses, rather than trusting that `git config` did what it was told.
    resolve
    report
    rc=$?
    [ $rc -eq 0 ] || exit "$rc"
    ;;
  status)
    resolve
    report || true
    exit 0
    ;;
  check)
    resolve
    report
    rc=$?
    if [ $rc -ne 0 ]; then
      echo ""
      echo "harness-check FAILS because the hook it ships is not running here."
      exit 1
    fi
    ;;
  *)
    echo "usage: $0 {install|status|check}" >&2
    exit 2 ;;
esac
