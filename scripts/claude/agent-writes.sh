#!/usr/bin/env bash
# PostToolUse hook for Edit|Write|NotebookEdit.
#
# Records which files each agent actually wrote, so commit-gate.sh can tell the
# difference between "you left work uncommitted" and "somebody else's work is
# sitting in this tree".
#
# It exists because of a deadlock. A read-only research agent was launched into
# another agent's worktree, and on stopping, the SubagentStop gate saw 17
# uncommitted paths it had never touched and demanded it commit them or delete
# them. Its brief said explicitly not to touch those files. Neither instruction
# could be satisfied, so it stopped and asked a human, and its research was lost.
#
# Cheap by construction: one append per write, no jq state, no locking. Appends
# to a per-agent file are single short lines, which is the case write(2) keeps
# atomic in practice; a duplicate or interleaved line would only ever cost an
# extra path in a reminder.
#
# Test: scripts/claude/guards_test.sh
set -uo pipefail

command -v jq >/dev/null 2>&1 || exit 0

input=$(cat)

# Only a subagent has an agent_id. The main thread is not gated on stop, so
# there is nothing to record for it.
aid=$(jq -r '.agent_id // empty' <<<"$input" 2>/dev/null)
[[ -z "$aid" ]] && exit 0

path=$(jq -r '.tool_input.file_path // .tool_input.notebook_path // empty' <<<"$input" 2>/dev/null)
[[ -z "$path" ]] && exit 0

state="${WPMGR_AGENT_WRITES_STATE:-${TMPDIR:-/tmp}/wpmgr-agent-writes}"
mkdir -p "$state" 2>/dev/null || exit 0

# Agent ids outlive nothing; two days is generous.
find "$state" -mindepth 1 -maxdepth 1 -type f -mtime +2 -delete 2>/dev/null

sane=$(printf '%s' "$aid" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64)
printf '%s\n' "$path" >> "$state/$sane" 2>/dev/null

exit 0
