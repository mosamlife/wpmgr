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

# ---- the state directory ---------------------------------------------------
# $state is ENVIRONMENT-CONTROLLED and it reaches both `mkdir -p` and a delete.
# That is the chain that destroyed this machine's home directory, and this file
# had none of the checks route-guard.sh grew in response: no absolute-path test,
# no '/' refusal, no -d, no -L, no -O, no marker, and `mkdir -p` creating
# whatever it was pointed at, at 0755. Pointed at a home directory it deleted
# files there, exited 0, and printed nothing. `-type f -maxdepth 1` meant it
# could not recurse, and that was the only thing limiting the damage.
#
# ${TMPDIR:-/tmp} is per-user on macOS but falls back to a SHARED /tmp on Linux,
# in containers and on CI. On a shared /tmp another local account can
# pre-create this directory world-writable, read every path each agent writes,
# and plant records - and a planted EMPTY record is the quietest outcome of all,
# because it flips commit-gate.sh to scoped with nothing owned and drops even
# the unscoped reminder.
#
# So this is route-guard.sh's resolve_state() discipline, deliberately the same
# shape and the same marker name rather than a second answer to one question:
# absolute, not the root, a plain directory, owned by this user, and carrying a
# marker this harness wrote itself. A directory failing any of those is somebody
# else's - never created, never adopted, never pruned, never written to.
#
# Globals rather than a command substitution, because $() runs in a subshell and
# the refusal reason would not survive it.
STATE_MARKER=".wpmgr-harness-state"
STATE_DEFAULT="${TMPDIR:-/tmp}/wpmgr-agent-writes"
STATE_DIR=""
STATE_WHY=""

resolve_state() { # sets STATE_DIR, or sets STATE_WHY and returns 1
  local want="${WPMGR_AGENT_WRITES_STATE:-$STATE_DEFAULT}"

  # A relative value is not a directory, it is "somewhere relative to whatever
  # cwd this hook happened to be invoked from". Never create one, never use one.
  case "$want" in
    /*) ;;
    *) STATE_WHY="cannot create the state directory '$want': it is not an absolute path"; return 1 ;;
  esac
  while [[ "$want" == */ && "$want" != "/" ]]; do want="${want%/}"; done
  if [[ "$want" == "/" ]]; then
    STATE_WHY="cannot create the state directory: the path given is the filesystem root"
    return 1
  fi

  if [[ -e "$want" ]]; then
    if [[ ! -d "$want" || -L "$want" ]]; then
      STATE_WHY="cannot create the state directory '$want': it exists and is not a plain directory"
      return 1
    fi
    if [[ ! -O "$want" ]]; then
      STATE_WHY="refusing the state directory '$want': it is not owned by this user"
      return 1
    fi
    if [[ -e "$want/$STATE_MARKER" ]]; then
      # The marker is the ownership claim, so the claim itself must be ours and
      # must be a real file: a symlink named $STATE_MARKER proves nothing about
      # who owns the directory it points into.
      if [[ -L "$want/$STATE_MARKER" || ! -f "$want/$STATE_MARKER" || ! -O "$want/$STATE_MARKER" ]]; then
        STATE_WHY="refusing the state directory '$want': its $STATE_MARKER is not a plain file owned by this user"
        return 1
      fi
    elif [[ "$want" == "$STATE_DEFAULT" ]]; then
      # One adoption, and only for the DEFAULT path, which we have just proven
      # this user owns: it carries this hook's own name, so an older version of
      # this hook is the plausible author. A path the environment supplied is
      # never adopted.
      : > "$want/$STATE_MARKER" || {
        STATE_WHY="refusing the state directory '$want': cannot write its $STATE_MARKER"; return 1; }
    else
      STATE_WHY="refusing the state directory '$want': no $STATE_MARKER, so it is not this harness's"
      return 1
    fi
    # It is ours, so close it: whatever another account could write here before,
    # it loses now.
    chmod 700 "$want" || {
      STATE_WHY="refusing the state directory '$want': cannot chmod 700 it"; return 1; }
  else
    ( umask 077; mkdir -p "$want" ) || {
      STATE_WHY="cannot create the state directory '$want'"; return 1; }
    chmod 700 "$want" || {
      STATE_WHY="cannot create the state directory '$want': created it but cannot chmod 700 it"; return 1; }
    : > "$want/$STATE_MARKER" || {
      STATE_WHY="cannot create the state directory '$want': cannot write its $STATE_MARKER"; return 1; }
  fi
  STATE_DIR="$want"
  return 0
}

resolve_state || degraded "$STATE_WHY."
state="$STATE_DIR"

# Records older than two days, and NOTHING else. The previous
# `find "$state" -mindepth 1 -maxdepth 1 -type f -mtime +2 -delete 2>/dev/null`
# deleted every top-level file in whatever $state happened to be - proven
# against a stand-in home tree, where it removed .gitconfig, .netrc,
# .zsh_history and notes.md and exited 0 in silence.
#
# This cannot leave the directory resolve_state just proved is ours, and inside
# it deletes only names of the exact shape sane() produces, only plain files,
# never a symlink and never the marker. No 2>/dev/null and no `|| true`: a prune
# that cannot do its job says so. I wrote that `|| true` and the comment
# defending it; the argument was about noise and ignored what $state is.
prune_state() { # prune_state <owned state root>
  ( shopt -s nullglob dotglob
    local e b
    for e in "$1"/*; do
      b=${e##*/}
      [[ "$b" == "$STATE_MARKER" ]] && continue
      [[ -f "$e" && ! -L "$e" ]] || continue
      [[ "$b" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || continue
      [[ -n "$(find "$e" -maxdepth 0 -mtime +2)" ]] || continue
      rm -f -- "$e"
    done )
}
prune_state "$state"

# umask 077 so the record itself is 0600. The directory is already 0700, so this
# is belt and braces, but a record of every path an agent touched is not
# something to create world-readable on the way to a stricter directory.
sane=$(printf '%s' "$aid" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64)
( umask 077; printf '%s\n' "$path" >> "$state/$sane" ) 2>/dev/null \
  || degraded "cannot append to the record '$state/$sane'."

exit 0
