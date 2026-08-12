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

# The SECOND, INDEPENDENT lock on the marker write. `set -C` makes bash open the
# redirect with O_CREAT|O_EXCL, and POSIX requires open() to FAIL when
# O_CREAT|O_EXCL names a symbolic link - dangling or not. So this refuses to
# follow a planted symlink even if the directory were somehow still writable by
# another account when it runs. Tightening the directory to 0700 first and this
# are two locks on one door; either alone closes the window, and neither is
# trusted to be the only one.
#
# DUPLICATION, DELIBERATE AND FLAGGED: this is character-for-character the same
# primitive as route-guard.sh's excl_create(), and two implementations of one
# primitive is exactly how this file inherited the ordering bug in the first
# place. It is copied rather than shared only because sharing it means editing
# route-guard.sh, which another agent owns this round. It wants extracting into
# one sourced file, in a single change that touches both callers.
excl_create() { # excl_create <path> -> create it, never following a symlink
  ( set -C; : > "$1" ) 2>/dev/null
}

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
    # `-e` FOLLOWS the link, so it is false for a DANGLING symlink and a planted
    # dangling marker fell through to "no marker" - or, on the default path, all
    # the way to the adoption branch, where only the O_EXCL lock stopped it.
    # `-L` is tested too so the first lock sees it and says what it really is.
    # Same `-e`/`-L` gap that was fixed on the reading side in commit-gate.sh.
    if [[ -e "$want/$STATE_MARKER" || -L "$want/$STATE_MARKER" ]]; then
      # The marker is the ownership claim, so the claim itself must be ours and
      # must be a real file: a symlink named $STATE_MARKER proves nothing about
      # who owns the directory it points into.
      if [[ -L "$want/$STATE_MARKER" || ! -f "$want/$STATE_MARKER" || ! -O "$want/$STATE_MARKER" ]]; then
        STATE_WHY="refusing the state directory '$want': its $STATE_MARKER is not a plain file owned by this user"
        return 1
      fi
      # Accepted, and nothing is written here, so this only closes the door:
      # whatever another account could write before, it loses now.
      chmod 700 "$want" || {
        STATE_WHY="refusing the state directory '$want': cannot chmod 700 it"; return 1; }
    elif [[ "$want" == "$STATE_DEFAULT" ]]; then
      # One adoption, and only for the DEFAULT path, which we have just proven
      # this user owns: it carries this hook's own name, so an older version of
      # this hook is the plausible author. A path the environment supplied is
      # never adopted.
      #
      # ORDER IS THE SECURITY PROPERTY HERE, and this file had it backwards -
      # it wrote the marker and chmod'd afterwards. Owning a directory does not
      # make it private: a directory this user owns can still be group- or
      # world-writable, and between "the marker is absent" and "create the
      # marker" another local account could drop a SYMLINK at that name and have
      # the redirect truncate whatever this user can write. The directory is
      # closed to 0700 BEFORE anything is written into it, and only then is the
      # marker created - and created with O_EXCL, so neither lock is trusted to
      # be the only one.
      chmod 700 "$want" || {
        STATE_WHY="refusing the state directory '$want': cannot chmod 700 it before writing its $STATE_MARKER"; return 1; }
      excl_create "$want/$STATE_MARKER" || {
        STATE_WHY="refusing the state directory '$want': cannot exclusively create its $STATE_MARKER - it already exists or is a symlink"; return 1; }
    else
      STATE_WHY="refusing the state directory '$want': no $STATE_MARKER, so it is not this harness's"
      return 1
    fi
  else
    # Ordering was already right here - mkdir, chmod, then the marker - but the
    # marker went in through a plain redirect, so it had one lock where the
    # adoption path above now has two. Same primitive, same guarantee.
    ( umask 077; mkdir -p "$want" ) || {
      STATE_WHY="cannot create the state directory '$want'"; return 1; }
    chmod 700 "$want" || {
      STATE_WHY="cannot create the state directory '$want': created it but cannot chmod 700 it"; return 1; }
    excl_create "$want/$STATE_MARKER" || {
      STATE_WHY="cannot create the state directory '$want': cannot exclusively create its $STATE_MARKER - it already exists or is a symlink"; return 1; }
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
# A symlink here is SKIPPED, never deleted, and that is deliberate after being
# asked. `rm -f` on a symlink would remove the link rather than its target, so
# deleting one would be safe in itself - but a delete loop is the last place to
# start making exceptions for symlinks, and the leak it would close is closed
# properly on the reading side instead: commit-gate.sh now refuses any record
# that is not a plain file owned by this user, so a symlink that lives here
# forever is inert rather than followed. Skipping is the safer default; the
# reader is the fix.
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

sane=$(printf '%s' "$aid" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64)

# JUDGED, not skipped. `>>` follows symlinks and bash's noclobber does not apply
# to it, so O_EXCL is not available here: the record must persist and be appended
# to across calls, which is the opposite of exclusive creation. So this is a
# check-then-use, and the thing that makes it sound is the directory, which
# resolve_state has just proved is 0700 and owned by this user - no other account
# can put a symlink here to begin with.
#
# What remains is a symlink left by something running AS this user: a stale one
# from before the marker regime, or a stray. prune_state deliberately skips
# symlinks, so such a link persists forever, and appending through it would write
# every path an agent touches into whatever it points at. commit-gate.sh already
# refuses to READ a symlinked record - that was last round - and this is the
# matching refusal on the WRITE side. Both ends of the same question now answer
# the same way.
if [[ -L "$state/$sane" ]]; then
  degraded "refusing to append to the record '$state/$sane': it is a symlink, and a record is a plain file or it is nothing. Delete it by hand if you did not put it there."
fi

# umask 077 so the record itself is 0600. The directory is already 0700, so this
# is belt and braces, but a record of every path an agent touched is not
# something to create world-readable on the way to a stricter directory.
( umask 077; printf '%s\n' "$path" >> "$state/$sane" ) 2>/dev/null \
  || degraded "cannot append to the record '$state/$sane'."

exit 0
