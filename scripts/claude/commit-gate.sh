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
# IT ONLY EVER CONSIDERS PATHS THIS AGENT WROTE. The first version compared
# against the whole working tree and deadlocked a read-only research agent that
# had been launched into another agent's worktree: it was told to commit or
# delete 17 files it had never touched, its brief said not to touch them,
# neither instruction could be satisfied, and it stopped and asked a human. An
# agent cannot be asked to answer for a tree it does not own. The ownership
# record is written by scripts/claude/agent-writes.sh on every PostToolUse.
#
# AND IT NEVER DEMANDS DELETION. "Commit them or delete them" is an instruction
# an agent under someone else's constraints cannot obey. It asks for a commit of
# its own paths, and says what to do instead if they are deliberate scratch.
#
# BLOCKS AT MOST ONCE. Claude Code overrides a Stop hook that blocks eight times
# in a row, and a guard that hangs and then silently stops guarding is worse
# than no guard. This exits 0 on the second pass via stop_hook_active.
#
# If the ownership record is missing - no agent_id in the payload, or the
# PostToolUse hook never ran - it REPORTS instead of blocking. A gate that
# deadlocks an agent is worse than one that reminds it.
#
# Test: scripts/claude/guards_test.sh
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

root=$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null) || exit 0
root=$(cd "$root" 2>/dev/null && pwd -P) || exit 0

dirty=$(git -C "$cwd" status --porcelain 2>/dev/null)
[[ -z "$dirty" ]] && exit 0

# ---- restrict to what this agent wrote -------------------------------------
aid=$(jq -r '.agent_id // empty' <<<"$input" 2>/dev/null)
state="${WPMGR_AGENT_WRITES_STATE:-${TMPDIR:-/tmp}/wpmgr-agent-writes}"
record=""
if [[ -n "$aid" ]]; then
  record="$state/$(printf '%s' "$aid" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64)"
fi

owned=""
scoped=0
if [[ -n "$record" && -f "$record" ]]; then
  scoped=1
  # The record holds absolute paths as the tool saw them; git speaks
  # repo-relative. Compare on the repo-relative form of both.
  while IFS= read -r w; do
    [[ -z "$w" ]] && continue
    rel="${w#"$root"/}"
    [[ "$rel" == "$w" ]] && continue        # not in this worktree at all
    # Anchored match against the porcelain path column, so a path is never
    # matched as a substring of a longer one.
    line=$(printf '%s\n' "$dirty" | awk -v p="$rel" 'substr($0,4)==p {print; exit}')
    [[ -z "$line" ]] && continue                       # written, then committed
    if ! printf '%s' "$owned" | grep -qxF -- "$line"; then
      owned="$owned$line"$'\n'
    fi
  done < <(sort -u "$record")
  owned=$(printf '%s' "$owned" | sed '/^$/d')
  # Everything this agent wrote is already committed. Nothing to say.
  [[ -z "$owned" ]] && exit 0
fi

if [[ $scoped -eq 1 ]]; then
  count=$(printf '%s\n' "$owned" | sed '/^$/d' | wc -l | tr -d ' ')
  {
    echo "You are finishing with $count uncommitted path(s) that YOU wrote:"
    echo ""
    printf '%s\n' "$owned"
    echo ""
    echo "Commit them before you stop, staging BY NAME. Any other file in this"
    echo "tree belongs to someone else: leave it exactly as it is."
    echo ""
    echo "Every agent run that returned no result in this project was recoverable"
    echo "from the working tree, and uncommitted work is also what pins a worktree"
    echo "so nothing can ever reap it."
    echo ""
    echo "If these are deliberate scratch that must NOT be committed, say so in"
    echo "your final message and stop again. This gate does not fire twice."
  } >&2
  exit 2
fi

# ---- unscoped: report, never block -----------------------------------------
# No ownership record, so there is no way to tell this agent's work from anyone
# else's. Blocking here is what caused the deadlock, so this only reminds.
{
  echo "NOTE: this worktree has uncommitted paths, and there is no record of"
  echo "which of them you wrote (no agent_id, or the PostToolUse recorder did"
  echo "not run). Not blocking."
  echo ""
  echo "If any of the following are yours, commit them by name before you stop."
  echo "If none are, ignore this and do not touch them:"
  echo ""
  printf '%s\n' "$dirty" | head -40
} >&2

exit 0
