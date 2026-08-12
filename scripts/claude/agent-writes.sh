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

# A failure to write this record is NOT harmless, and both sites below used to
# discard theirs: `mkdir -p ... 2>/dev/null || exit 0`, and an append whose
# stderr went to /dev/null followed by an unconditional `exit 0`.
#
# This file is the SOLE input to commit-gate.sh. With no record the gate sets
# scoped=0 and takes its "report, never block" branch, so a recorder that fails
# quietly DOWNGRADES the blocking gate to a reminder and nothing anywhere says
# so - the agent stops, the work stays uncommitted, and the worktree is then one
# Claude Code's own sweep is contractually forbidden to reap. A read-only
# TMPDIR, a full disk, or $state already existing as a plain file all do it.
#
# It reports on stderr and still exits 0, deliberately and in that order. This
# is a PostToolUse hook: a non-zero exit fails the user's tool call, which would
# punish the session for a broken temp directory. Stderr survives to the
# transcript, so the degradation is announced instead of silent - the same
# contract session-brief.sh uses to announce a missing jq.
degraded() { # degraded <what failed>
  printf 'agent-writes: %s\n' "$1" >&2
  printf 'agent-writes: the ownership record is NOT being written, so commit-gate.sh cannot tell your files from anyone else'"'"'s and will only remind, never block. Commit your own paths BY NAME before you stop.\n' >&2
  exit 0
}

mkdir -p "$state" 2>/dev/null || degraded "cannot create the state directory '$state'."

# Best effort, and quiet on purpose: this is a GC sweep of records older than
# two days. Its only failure cost is that stale records linger, and a hook that
# shouted about a failed cleanup on every single write would train the reader to
# skip everything this script says - including the line above, which matters.
# Do not "fix" this to match the reporting either side of it; the consequences
# are not the same.
find "$state" -mindepth 1 -maxdepth 1 -type f -mtime +2 -delete 2>/dev/null || true

sane=$(printf '%s' "$aid" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64)
printf '%s\n' "$path" >> "$state/$sane" 2>/dev/null \
  || degraded "cannot append to the record '$state/$sane'."

exit 0
