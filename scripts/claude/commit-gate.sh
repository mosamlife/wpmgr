#!/usr/bin/env bash
# SubagentStop hook.
#
# Makes the one mitigation this project measured as working - commit before the
# slow step, so an interrupted run keeps its work - mechanical instead of
# advisory. Of 247 workflow agents in one week, 15 returned no result, and the
# work was recoverable from the working tree EVERY time. Exactly one brief in
# 247 contained the commit-first instruction, and that agent delivered.
#
# It also unblocks failure B. Claude Code's periodic sweep skips any worktree
# holding "changed or untracked files, or unpushed commits", so a subagent that
# stops with a dirty tree creates a worktree the sweep is contractually
# forbidden to reap, forever. Committing converts dirty files into commits; the
# branch is then reapable by `make harness-reap` once it is merged.
#
# BLOCKS AT MOST ONCE. Claude Code overrides a Stop hook that blocks eight times
# in a row, and a guard that hangs and then silently stops guarding is worse
# than no guard. This exits 0 on the second pass via stop_hook_active.
set -uo pipefail

command -v jq >/dev/null 2>&1 || exit 0

input=$(cat)

# Second pass: we already asked. Let it stop.
[[ "$(jq -r '.stop_hook_active // false' <<<"$input" 2>/dev/null)" == "true" ]] && exit 0

cwd=$(jq -r '.cwd // empty' <<<"$input" 2>/dev/null)
[[ -z "$cwd" || ! -d "$cwd" ]] && exit 0

git -C "$cwd" rev-parse --git-dir >/dev/null 2>&1 || exit 0

# Only gate inside an agent worktree. A reviewer or a read-only agent in the
# main checkout has no business committing, and blocking it would be the
# over-fire that gets this switched off.
case "$cwd" in
  */.claude/worktrees/*) ;;
  *) exit 0 ;;
esac

dirty=$(git -C "$cwd" status --porcelain 2>/dev/null | head -40)
[[ -z "$dirty" ]] && exit 0

count=$(git -C "$cwd" status --porcelain 2>/dev/null | wc -l | tr -d ' ')

{
  echo "You are finishing with $count uncommitted path(s) in your worktree:"
  echo ""
  printf '%s\n' "$dirty"
  echo ""
  echo "Commit them before you stop, staging BY NAME - never 'git add -A'."
  echo ""
  echo "Every agent run that returned no result in this project was recoverable"
  echo "from the working tree, and uncommitted work is also what pins a worktree"
  echo "so nothing can ever reap it."
  echo ""
  echo "If these files are deliberate scratch that must NOT be committed, delete"
  echo "them and stop again. This gate does not fire twice."
} >&2

exit 2
