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
#   install   copy .githooks/pre-push into $GIT_COMMON_DIR/hooks, point
#             core.hooksPath at that same directory, and verify by running it
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
STALE=""    # non-empty: the installed copy differs from the committed source
NOCMP=""    # non-empty: why the staleness comparison could not be made

common_hooks_dir() {
  # $GIT_COMMON_DIR/hooks. This is git's own default hooks directory, it is the
  # SAME directory from every linked worktree, and - the whole point - it is not
  # in any working tree, so no checkout, tag, bisect or detached HEAD can take
  # it away. Pointing core.hooksPath at the tracked `.githooks` instead was
  # measured wrong twice: relative, git resolved it against whichever tree ran
  # the hook; absolute, it still named a directory that is only present in a
  # tree checked out at or after the hook's commit, so `git checkout <older>`
  # in the main checkout silently disarmed every worktree while config kept
  # reading "installed".
  local common
  common=$(git rev-parse --git-common-dir 2>/dev/null) || return 1
  [ -n "$common" ] || return 1
  common=$(cd "$common" 2>/dev/null && pwd -P) || return 1
  printf '%s' "$common/hooks"
}

hook_source() {
  # The committed hook, from the working tree this was invoked in first - so
  # `make hooks` works from a worktree even when the main checkout sits on a
  # commit that predates `.githooks` - and from the main checkout second.
  local top common
  top=$(git rev-parse --show-toplevel 2>/dev/null)
  if [ -n "$top" ] && [ -r "$top/.githooks/pre-push" ]; then
    printf '%s' "$top/.githooks/pre-push"; return 0
  fi
  common=$(git rev-parse --git-common-dir 2>/dev/null) || return 1
  common=$(cd "$common" 2>/dev/null && pwd -P) || return 1
  if [ -r "$(dirname "$common")/.githooks/pre-push" ]; then
    printf '%s' "$(dirname "$common")/.githooks/pre-push"; return 0
  fi
  return 1
}

resolve() {
  if ! command -v git >/dev/null 2>&1; then
    CANNOT="git is not on PATH"; return
  fi
  if ! git rev-parse --git-dir >/dev/null 2>&1; then
    CANNOT="'$PWD' is not inside a git worktree"; return
  fi

  local hp top from src
  hp=$(git config --get core.hooksPath 2>/dev/null)
  if [ -z "$hp" ]; then
    # Unset is not "no hook": git falls back to $GIT_COMMON_DIR/hooks, which is
    # exactly where `install` puts it, so this reads the default rather than
    # calling it absent. Saying "core.hooksPath is unset, so git runs no hook"
    # here would be a definite negative about a directory never looked at.
    hp=$(common_hooks_dir) || {
      CANNOT="core.hooksPath is unset and 'git rev-parse --git-common-dir' failed, so git's default hooks directory cannot be located"
      return
    }
    from="git's default, core.hooksPath unset"
  else
    from="core.hooksPath"
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
  fi
  HOOKPATH="$hp/pre-push"

  if [ ! -e "$HOOKPATH" ]; then
    WHY="git resolves hooks to '$hp' ($from), and there is no pre-push in it"
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

  # STALENESS. Installing by copy buys a hook no checkout can remove, and the
  # price is a second copy that can drift from `.githooks/pre-push`. So it is
  # compared, every time this runs, against the committed source - and when
  # there is no source to compare against, that is SAID rather than passed over,
  # because "not compared" and "identical" must never print the same.
  #
  # Drift does not change the exit code. What `check` gates on is the two probes
  # above: the installed hook refuses main and permits a branch. A copy that
  # still does both is protecting the repository whatever else it says, and
  # reddening every worktree that edits `.githooks/pre-push` on a branch would
  # redden correct work - which is how a gate gets switched off.
  if src=$(hook_source); then
    cmp -s "$src" "$HOOKPATH" || STALE="$src"
  else
    NOCMP="no readable .githooks/pre-push in this working tree or the main checkout"
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
  if [ -n "$STALE" ]; then
    echo "  STALE: it differs from the committed '$STALE'. It still refuses main - that was just measured - but it is not the shipped text. Refresh with '$(fix_line)'."
  elif [ -n "$NOCMP" ]; then
    echo "  Staleness was NOT checked: $NOCMP. Do not read this line as 'up to date'."
  fi
  return 0
}

case "$mode" in
  install)
    dir=$(common_hooks_dir) || {
      echo "FAIL: 'git rev-parse --git-common-dir' gave no answer in '$PWD', so there is no repository to install into." >&2
      echo "      Run this from inside a checkout of this repository." >&2
      exit 2
    }
    src=$(hook_source) || {
      echo "FAIL: no readable .githooks/pre-push in this working tree or in the main checkout, so there is nothing to install." >&2
      echo "      Check out a commit that carries .githooks/ in either tree and run '$(fix_line)' again." >&2
      exit 2
    }
    mkdir -p "$dir" || { echo "FAIL: could not create '$dir'." >&2; exit 2; }
    cp "$src" "$dir/pre-push" || { echo "FAIL: could not copy '$src' to '$dir/pre-push'." >&2; exit 2; }
    chmod +x "$dir/pre-push" || { echo "FAIL: could not make '$dir/pre-push' executable." >&2; exit 2; }
    echo "installed $src -> $dir/pre-push"
    # core.hooksPath is set to the same directory git would use by default. It
    # is not needed for a clone that never had it, and it is the repair for one
    # that does: an earlier version of this script pointed it at the tracked
    # .githooks, and leaving that value in place would send git looking at a
    # directory that disappears on `git checkout <older commit>`.
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
