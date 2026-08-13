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

# `-uall` is load-bearing, not decoration. Default porcelain COLLAPSES an
# untracked directory to one entry - `?? apps/api/internal/foo/` - and never
# names a single file inside it. An agent that created
# apps/api/internal/foo/bar.go in a directory that did not exist therefore
# produced no porcelain line carrying `bar.go`, the ownership compare below
# matched nothing, and the gate went silent about exactly the work it exists to
# catch: brand-new files in brand-new packages, which is most of what a builder
# agent writes. `-uall` expands each of those to one line per file.
#
# It cannot over-fire. Everything below is filtered through the ownership record
# first, so an unrelated untracked tree - however many files -uall now expands
# it into - still matches nothing this agent wrote and still says nothing.
# Ignored files stay invisible either way: that needs --ignored, which is not
# passed, so a gitignored node_modules is not listed by this and never was.
#
# `-z` is load-bearing for a second, independent reason. DEFAULT PORCELAIN
# QUOTES A PATH that carries a space, a double quote, a backslash, a control
# character or a non-ASCII byte: it prints `?? "has space.txt"`, wrapped in
# double quotes with C-style escapes, and `-c core.quotePath=false` does not
# stop it - that flag only unescapes the non-ASCII case, and a space is still
# quoted because the plain format is space-delimited. The ownership compare
# below matches the record's path against the porcelain path column, so EVERY
# such file failed to match, `owned` came out empty, and this script exited 0 in
# silence - reporting that the agent had committed everything when it had not,
# which is exactly how unpushed work gets reaped. `-z` prints paths raw.
#
# NUL is translated to newline because a newline inside a filename is already
# unrepresentable in the ownership record, which agent-writes.sh writes one path
# per line; so nothing this system can express is lost, and the result is
# ordinary line-oriented text that command substitution can carry (it cannot
# carry NUL - bash drops those bytes, silently on the bash 3.2 this machine
# ships).
#
# `pipefail` is set, so a git that fails is not read as a clean tree.
dirty=$(git -C "$cwd" status --porcelain -uall -z 2>/dev/null | tr '\0' '\n')
status_rc=$?
if [[ $status_rc -ne 0 ]]; then
  {
    echo "NOTE: 'git status --porcelain -uall -z' failed in '$cwd' (exit $status_rc)."
    echo "Nothing could be read, so nothing is claimed either way. Not blocking."
    echo "Check your own uncommitted paths by hand before you stop."
  } >&2
  exit 0
fi
[[ -z "$dirty" ]] && exit 0

# Drop the ORIGIN half of a rename or copy. Under `-z` git emits a rename as two
# records - `R  <new path>` and then the OLD path on its own, with no XY prefix -
# so the line after an R or C entry is a bare path, and matching it as an entry
# would read its first three characters as a status code and compare the rest.
# Measured shape: `R  new name.txt<NUL>old name.txt<NUL>`.
entries=$(printf '%s\n' "$dirty" | awk '
  skip { skip = 0; next }
  { print; if (substr($0, 1, 2) ~ /[RC]/) skip = 1 }
')

# ---- restrict to what this agent wrote -------------------------------------
aid=$(jq -r '.agent_id // empty' <<<"$input" 2>/dev/null)
state="${WPMGR_AGENT_WRITES_STATE:-${TMPDIR:-/tmp}/wpmgr-agent-writes}"

# The record is only worth as much as the directory holding it. ${TMPDIR:-/tmp}
# falls back to a SHARED /tmp on Linux, in containers and on CI, where another
# local account can pre-create this directory and plant records. A planted EMPTY
# record is the worst case and the quietest: it sets scoped=1 with nothing
# owned, and this script then exits 0 at the "already committed" branch WITHOUT
# even the unscoped reminder - less said than if the record had never existed.
#
# So the directory is vouched for the same way agent-writes.sh and
# route-guard.sh vouch for it, and anything that fails falls through to the
# unscoped reminder, which is the loud direction. This side only ever READS: it
# never creates, adopts, chmods or prunes, because a Stop hook is not the place
# to be repairing state.
STATE_MARKER=".wpmgr-harness-state"
state_trusted=0
state_why=""
if [[ ! -e "$state" ]]; then
  : # No state at all is normal for an agent that has written nothing. Not a
    # finding, and saying so on every such stop would be noise.
elif [[ ! -d "$state" || -L "$state" ]]; then
  state_why="it is not a plain directory"
elif [[ ! -O "$state" ]]; then
  state_why="it is not owned by this user"
elif [[ -L "$state/$STATE_MARKER" || ! -f "$state/$STATE_MARKER" || ! -O "$state/$STATE_MARKER" ]]; then
  state_why="it carries no $STATE_MARKER file owned by this user, so it is not this harness's"
else
  state_trusted=1
fi

record=""
record_why=""
if [[ $state_trusted -eq 1 && -n "$aid" ]]; then
  record="$state/$(printf '%s' "$aid" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64)"
  # `-f` FOLLOWS SYMLINKS, and agent-writes.sh's prune deliberately never
  # removes one, so a symlink planted here persists indefinitely and this
  # reader would follow it on every stop, forever. Measured against a stand-in
  # tree, with the state directory legitimately ours - 0700, marker present:
  #
  #   symlink -> a list naming another agent's dirty file  exit 2, BLOCKED and
  #     told the agent to commit 'theirs.txt', which it never wrote. That is
  #     precisely the misattribution this ownership record exists to prevent,
  #     and the deadlock that made a read-only agent stop and ask a human.
  #   symlink -> an empty file, or /etc/hosts                exit 0, SILENT -
  #     not even the unscoped reminder. The quiet direction, and the same
  #     shape as the planted-empty-record case closed one commit ago.
  #   dangling symlink                                       already safe:
  #     -f is false, so it fell through to the reminder.
  #
  # Same discipline as the marker check above: a record is a plain file owned
  # by this user, or it is not read. `-e` alone is false for a dangling link,
  # so `-L` is tested too or that case would slip past unreported.
  if [[ -e "$record" || -L "$record" ]]; then
    if [[ -L "$record" ]]; then
      record_why="the record '$record' is a symlink, and a symlink is not evidence of what this agent wrote"
    elif [[ ! -f "$record" ]]; then
      record_why="the record '$record' is not a plain file"
    elif [[ ! -O "$record" ]]; then
      record_why="the record '$record' is not owned by this user"
    fi
    [[ -n "$record_why" ]] && record=""
  fi
fi

owned=""
scoped=0
if [[ -n "$record" && -f "$record" ]]; then
  scoped=1
  # The record holds absolute paths as the tool saw them; git speaks
  # repo-relative. Reduce the record to repo-relative form once...
  wrote=$(while IFS= read -r w; do
            [[ -z "$w" ]] && continue
            rel="${w#"$root"/}"
            [[ "$rel" == "$w" ]] && continue    # not in this worktree at all
            printf '%s\n' "$rel"
          done < <(sort -u "$record"))
  # ...then walk the PORCELAIN and ask the record about each entry, rather than
  # walking the record and searching the porcelain text. The path git prints is
  # now the authority on its own spelling, so nothing depends on reproducing
  # git's quoting rules, and `grep -qxF` is a whole-line literal match, so a
  # path is never matched as a substring or a prefix of a longer one.
  # No dedup pass: git lists each path once.
  while IFS= read -r line; do
    [[ ${#line} -lt 4 ]] && continue
    rel="${line:3}"
    printf '%s\n' "$wrote" | grep -qxF -- "$rel" || continue
    owned="$owned$line"$'\n'
  done < <(printf '%s\n' "$entries")
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
  if [[ -n "$state_why" ]]; then
    echo "NOTE: the ownership record directory '$state' was NOT trusted:"
    echo "$state_why. Nothing in it was read. Reporting instead of blocking."
    echo ""
  fi
  if [[ -n "$record_why" ]]; then
    echo "NOTE: $record_why."
    echo "It was NOT read. Delete it by hand if you did not put it there."
    echo "Reporting instead of blocking."
    echo ""
  fi
  echo "NOTE: this worktree has uncommitted paths, and there is no record of"
  echo "which of them you wrote (no agent_id, or the PostToolUse recorder did"
  echo "not run). Not blocking."
  echo ""
  echo "If any of the following are yours, commit them by name before you stop."
  echo "If none are, ignore this and do not touch them:"
  echo ""
  printf '%s\n' "$entries" | head -40
} >&2

exit 0
