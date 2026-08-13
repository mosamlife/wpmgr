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

PROBE_SECONDS=5

probe_hook() {
  # probe_hook <one stdin line> -> 0 the hook allowed it, 1 it refused,
  #                                2 it did not answer within $PROBE_SECONDS.
  #
  # `read -t` is a bash builtin and needs no `timeout`, no `sleep` and no `seq`,
  # which matters because the suite's hermetic PATH farm contains none of them.
  # On expiry the read fd is closed, so a hook that later writes its status gets
  # SIGPIPE; one that never writes is left running rather than waited on, and
  # that is the deliberate trade - this script's job is to answer inside the
  # session budget, not to reap someone else's process.
  local line="$1" status
  exec 9< <(printf '%s\n' "$line" | "$HOOKPATH" origin probe://none >/dev/null 2>&1; printf '%s\n' "$?")
  if read -t "$PROBE_SECONDS" -r status <&9; then
    exec 9<&-
    [ "$status" = 0 ] && return 0
    return 1
  fi
  exec 9<&-
  return 2
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

  # RECOGNITION COMES BEFORE EXECUTION, and this order is the whole point.
  # `status` runs from SessionStart on every start, resume and compact, and the
  # probes below EXECUTE the resolved pre-push. Before this ordering, the file
  # executed was whatever `core.hooksPath` happened to point at - measured: a
  # clone with `core.hooksPath=.husky` ran husky's pre-push, which received the
  # synthetic refs on stdin; a foreign hook that slept 9s took `status` to
  # 18.197s and session-brief.sh to 23.310s, past the 20s SessionStart budget in
  # .claude/settings.json, so the session lost the very report this produces.
  #
  # So: a hook is EXECUTED only when this installer can show it wrote it. Two
  # ways to show that, and either is sufficient:
  #   - it is byte-identical to the committed .githooks/pre-push, i.e. it is
  #     literally this repository's own code; or
  #   - its blob id equals the one `install` recorded in wpmgr.installedHookBlob.
  # The second is not redundant: it is what keeps a source edit on a branch from
  # reddening the gate. Editing .githooks/pre-push makes the installed copy
  # differ from the source, and that copy is still ours and still protecting the
  # repository - it reports INSTALLED and STALE, exactly as before.
  local blob recorded
  if src=$(hook_source); then
    cmp -s "$src" "$HOOKPATH" || STALE="$src"
  else
    NOCMP="no readable .githooks/pre-push in this working tree or the main checkout"
  fi
  if [ -n "$STALE" ] || [ -n "$NOCMP" ]; then
    recorded=$(git config --get wpmgr.installedHookBlob 2>/dev/null)
    blob=$(git hash-object "$HOOKPATH" 2>/dev/null)
    if [ -z "$recorded" ] || [ -z "$blob" ] || [ "$recorded" != "$blob" ]; then
      CANNOT="'$HOOKPATH' is not a hook this installer wrote - it matches neither the committed .githooks/pre-push nor the copy recorded in wpmgr.installedHookBlob, so it was NOT executed and whether main is protected here is UNKNOWN. Nothing was changed or removed. Look at that file; if it is your own tooling, keep it and integrate the refusal from .githooks/pre-push by hand, otherwise run '$(fix_line)', which preserves it as pre-push.pre-wpmgr.<stamp> before installing"
      HOOKPATH=""; STALE=""; NOCMP=""
      return
    fi
  fi

  # Presence is not protection. Drive the resolved hook with the stdin
  # githooks(5) specifies and require it to actually refuse main and actually
  # allow a branch: a truncated, emptied or replaced hook is executable too.
  #
  # BOUNDED, because this runs inside a 20s SessionStart budget. `timeout` and
  # `gtimeout` are BOTH absent on this machine (`command -v` finds neither), so
  # the bound is bash's own `read -t` against a process substitution: no
  # external binary, and none of `until` / `while true`, which the bash guard
  # denies and which is the shape that has killed most agents here.
  local zero=0000000000000000000000000000000000000000
  local sha=1111111111111111111111111111111111111111
  probe_hook "refs/heads/main $sha refs/heads/main $zero"
  case $? in
    0) WHY="'$HOOKPATH' runs but ALLOWS a push to refs/heads/main, so it is not the hook this repository ships"; return ;;
    2) CANNOT="'$HOOKPATH' did not finish within ${PROBE_SECONDS}s when asked about refs/heads/main, so it was abandoned rather than waited on; whether it refuses main is UNKNOWN"
       HOOKPATH=""; STALE=""; NOCMP=""; return ;;
  esac
  probe_hook "refs/heads/fix/x $sha refs/heads/fix/x $zero"
  case $? in
    1) WHY="'$HOOKPATH' refuses an ordinary branch push, so it would block every branch and get switched off"; return ;;
    2) CANNOT="'$HOOKPATH' did not finish within ${PROBE_SECONDS}s when asked about an ordinary branch, so it was abandoned rather than waited on"
       HOOKPATH=""; STALE=""; NOCMP=""; return ;;
  esac

  # STALENESS. Installing by copy buys a hook no checkout can remove, and the
  # price is a second copy that can drift from `.githooks/pre-push`. It is
  # compared above, every time this runs, against the committed source - and
  # when there is no source to compare against, that is SAID rather than passed
  # over, because "not compared" and "identical" must never print the same.
  #
  # Drift does not change the exit code. What `check` gates on is the two probes
  # above: the installed hook refuses main and permits a branch. A copy that
  # still does both is protecting the repository whatever else it says, and
  # reddening every worktree that edits `.githooks/pre-push` on a branch would
  # redden correct work - which is how a gate gets switched off.
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

    # PRESERVE, NEVER CLOBBER. This ran from `scripts/bootstrap.sh` and from
    # `make hooks`, i.e. on the ordinary onboarding path, and it overwrote
    # whatever pre-push was already there without a word. Measured in a throwaway
    # clone: a hand-written pre-push went from 3 lines to 132, `install` exited 0
    # and printed nothing about it, no backup existed, and `grep -rl` found the
    # old text nowhere on disk. `.git/hooks` is untracked and this Mac has no
    # Time Machine and no APFS snapshots, so that was unrecoverable.
    #
    # WHY PRESERVE RATHER THAN REFUSE, since both were on the table: refusing
    # puts a hard stop in `scripts/bootstrap.sh`, so the first thing a new
    # contributor with husky meets is a failed bootstrap, and the fix a hurried
    # person reaches for is deleting their own hook. A copy costs a few hundred
    # bytes, is announced in the output with the exact command that restores it,
    # and leaves nothing destroyed. The half that must never be silent is the
    # ANNOUNCEMENT, not the refusal.
    backup=""
    if [ -e "$dir/pre-push" ] && ! cmp -s "$src" "$dir/pre-push"; then
      backup="$dir/pre-push.pre-wpmgr.$(date +%Y%m%d%H%M%S).$$"
      cp -p "$dir/pre-push" "$backup" || {
        echo "FAIL: '$dir/pre-push' already exists, differs from '$src', and could not be copied to '$backup'." >&2
        echo "      Nothing was overwritten. Move that hook aside yourself, then run '$(fix_line)' again." >&2
        exit 2; }
    fi

    cp "$src" "$dir/pre-push" || { echo "FAIL: could not copy '$src' to '$dir/pre-push'." >&2; exit 2; }
    chmod +x "$dir/pre-push" || { echo "FAIL: could not make '$dir/pre-push' executable." >&2; exit 2; }
    echo "installed $src -> $dir/pre-push"
    if [ -n "$backup" ]; then
      echo "PRESERVED the pre-push that was already there -> $backup"
      echo "  It was not this repository's hook. It is no longer what git runs."
      echo "  Read it, and if it is yours, fold it into '$dir/pre-push' by hand."
      echo "  RESTORE IT ENTIRELY WITH:  cp '$backup' '$dir/pre-push'"
    fi

    # Record what was installed, so `status` can tell a hook this installer
    # wrote from a foreign one WITHOUT executing the foreign one to find out.
    if blob=$(git hash-object "$dir/pre-push" 2>/dev/null) && [ -n "$blob" ]; then
      git config wpmgr.installedHookBlob "$blob"
    fi

    # core.hooksPath is set to the same directory git would use by default. It
    # is not needed for a clone that never had it, and it is the repair for one
    # that does: an earlier version of this script pointed it at the tracked
    # .githooks, and leaving that value in place would send git looking at a
    # directory that disappears on `git checkout <older commit>`.
    #
    # A NON-MATCHING PRIOR VALUE IS SOMEONE ELSE'S TOOLING and it is not
    # destroyed in silence either. Measured: a clone with `core.hooksPath=.husky`
    # had it redirected here, and a real push to a local bare remote stopped
    # printing husky's line - the file survived, git simply no longer resolved
    # it. It is recorded and named now, with the command that puts it back.
    prev=$(git config --get core.hooksPath 2>/dev/null)
    prev_resolved="$prev"
    if [ -n "$prev" ] && [ "${prev#/}" = "$prev" ]; then
      prev_top=$(git rev-parse --show-toplevel 2>/dev/null)
      [ -n "$prev_top" ] && prev_resolved="$prev_top/$prev"
    fi
    git config core.hooksPath "$dir"
    echo "core.hooksPath = $(git config --get core.hooksPath)"
    if [ -n "$prev" ] && [ "$prev_resolved" != "$dir" ]; then
      git config wpmgr.previousHooksPath "$prev"
      echo "core.hooksPath WAS '$prev' (resolving to '$prev_resolved')."
      if [ -e "$prev_resolved/pre-push" ]; then
        echo "  '$prev_resolved/pre-push' STILL EXISTS and was not touched, but git will NOT run it any more."
      fi
      echo "  Saved as wpmgr.previousHooksPath."
      echo "  REVERT WITH:  git config core.hooksPath \"\$(git config --get wpmgr.previousHooksPath)\""
      echo "  Reverting turns this repository's push-to-main refusal back off."
    fi
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
