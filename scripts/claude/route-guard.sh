#!/usr/bin/env bash
# PreToolUse hook for Edit|Write|NotebookEdit.
#
# Turns the routing table in CLAUDE.md into something that cannot be silently
# skipped. The rule was written down and loaded, and the owner still had to
# say "you are fixing in session, launch an agent" three times in one week.
# A rule skipped that often has to stop being a rule.
#
# THREE DECISIONS, deliberately different strengths:
#
#   deny  - an already-applied migration (editing it is a documented no-op), a
#           generated tree (hand-editing it caused a production 500), and the
#           dead app. None is a judgement call, so none gets a prompt.
#   ask   - the FIRST write to a routed area in this session. The owner can
#           still approve an inline edit on purpose; what is removed is doing it
#           without noticing.
#   pass  - everything else, every call from inside a subagent, tests that CI
#           actually runs, and every later write to an area already ruled on.
#
# WHY IT ASKS ONCE PER AREA AND NOT ONCE PER FILE. The first version asked on
# 843 of the 926 files touched in the preceding 30 days (91%), measured with
# scripts/claude/route-guard-coverage.sh. That is a prompt on essentially every
# main-thread edit, and a guard that cries wolf gets switched off, which is the
# failure this harness exists to prevent. Routing changes the outcome when a
# piece of work STARTS, not on the fortieth file of the same piece of work: once
# the owner has ruled "backend-architect takes this" or "I am doing this inline
# deliberately", asking again carries no new information. So the decision is
# remembered per session, per destination, for WPMGR_ROUTE_GUARD_TTL_MIN
# minutes, and a session that comes back to the same area later re-asks.
#
# FAIL-OPEN, DELIBERATELY, AND ANNOUNCED. If jq is missing this exits 0 and
# routes nothing, because blocking every edit on a fresh clone of a public repo
# is a guard that gets switched off within the hour. The compensating control is
# scripts/claude/session-brief.sh, which prints "route-guard INACTIVE" at the
# top of every session when jq is absent. Silence is what this project bans;
# a stated, visible degradation is not silence.
#
# The de-duplication needs a session identity. If neither session_id nor
# transcript_path is in the payload it does NOT fail open: it asks every time,
# which is the old behaviour, because losing the prompt is the worse failure.
#
# THIS GUARD ONLY SEES Edit/Write/NotebookEdit. A shell write reaches any of
# these paths without it. bash-guard.sh closes that for the deny cases; see its
# header for what it cannot close.
#
# TWO ARMS, AND ONLY THE SECOND ONE REMEMBERS ANYTHING.
#
#   (no argument)  PreToolUse. Classifies and emits deny/ask/pass. It writes NO
#                  marker, because at PreToolUse the human has not answered yet.
#   --record       PostToolUse. Emits nothing and records the ruling. PostToolUse
#                  runs only if the tool actually ran, which is the one piece of
#                  evidence the hook protocol gives that the prompt was approved.
#
# The defect that split them: the marker was persisted by the PreToolUse arm,
# BEFORE the answer. Denying or cancelling the prompt therefore suppressed every
# later write to that destination for the whole TTL, so refusing the guard was
# how you switched it off. A refusal must make this stricter, never quieter. A
# deny arm records nothing either: there is no ruling to remember, and a marker
# written there would silence a later genuine ask.
#
# Test: scripts/claude/guards_test.sh
# Over-fire rate: scripts/claude/route-guard-coverage.sh
set -uo pipefail

mode=run
[[ "${1:-}" == "--record" ]] && mode=record

command -v jq >/dev/null 2>&1 || exit 0

input=$(cat)

# Inside a subagent this must not fire: the specialist IS the destination.
# agent_id is present only for subagent tool calls.
[[ -n "$(jq -r '.agent_id // empty' <<<"$input" 2>/dev/null)" ]] && exit 0

emit() { # emit <decision> <reason>
  # The --record arm answers nothing and records nothing here: every caller of
  # emit above the memory block is a deny, and a deny has no ruling to remember.
  [[ "$mode" == record ]] && exit 0
  jq -n --arg d "$1" --arg r "$2" '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: $d,
      permissionDecisionReason: $r,
      additionalContext: $r
    }
  }'
  exit 0
}

path=$(jq -r '.tool_input.file_path // .tool_input.notebook_path // empty' <<<"$input" 2>/dev/null)
[[ -z "$path" ]] && exit 0

# A RELATIVE file_path used to `exit 0` right here, printing nothing, for every
# path in the routing table: a payload carrying no usable cwd was a silent route
# around every arm below, including the two hard denies. Driven at
# apps/api/internal/auth/members_handler.go - backend-architect plus a mandatory
# security review - it exited 0 with zero bytes, and the generated sqlc tree did
# the same. bash-guard.sh had already been hardened against exactly this shape
# in its migration arm; this is the same fix on this side.
#
# "Relative" only means anything against a base, so a base is resolved from two
# independent sources, and when neither answers the guard still does not fall
# silent - it judges the literal string as if it were repo-relative, which is
# the only reading under which a routed prefix means anything, and says so.
resolved=1
case "$path" in
  /*) ;;
  *)
    base=$(jq -r '.cwd // empty' <<<"$input" 2>/dev/null)
    if [[ -n "$base" && -d "$base" && ! -L "$base" ]]; then
      base=$(cd "$base" 2>/dev/null && pwd -P) || base=""
    else
      base=""
    fi
    # Second source: this hook script is committed inside the repository it
    # guards, so its own directory resolves that checkout even with no cwd at
    # all. A relative path in a payload with no cwd is repo-relative or it is
    # nothing.
    if [[ -z "$base" ]]; then
      self_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd -P) || self_dir=""
      if [[ -n "$self_dir" ]]; then
        base=$(git -C "$self_dir" rev-parse --show-toplevel 2>/dev/null) || base=""
        [[ -n "$base" ]] && { base=$(cd "$base" 2>/dev/null && pwd -P) || base=""; }
      fi
    fi
    if [[ -n "$base" ]]; then
      path="$base/$path"
    else
      # No cwd, and this script is not inside a checkout either. There is no
      # base, so there is no physical path to resolve - but there is still a
      # string, and the routing table is a table of string prefixes. Judge it,
      # and let the arms below answer. Falling through to `exit 0` here is what
      # produced zero bytes on the sqlc tree.
      resolved=0
      rel="$path"
      root=""
    fi
    ;;
esac

# Repo-relative, derived from the FILE, not from cwd. Deriving it from .cwd
# silently disabled this guard whenever the session was started in a
# subdirectory: with cwd=apps/api every control-plane .go file passed.
#
# Both sides are resolved to physical paths first: `git rev-parse` returns a
# physical path, so on any machine where the checkout sits under a symlinked
# prefix (/tmp -> /private/tmp on macOS) a raw string prefix strip fails to
# match and the guard silently routes nothing.
# Walk up to the nearest EXISTING ancestor: a Write that creates a file in a
# directory that does not exist yet is exactly the case a `dirname` check would
# wave through, and creating a new migration is that case.
if (( resolved )); then
  dir=$(dirname "$path")
  anc="$dir"
  while [[ ! -d "$anc" && "$anc" != "/" && -n "$anc" ]]; do anc=$(dirname "$anc"); done
  # These three exits are for a path that is outside every checkout on this
  # machine: there is no repository whose routing table could cover it, so
  # passing is an answer and not a silence.
  [[ -d "$anc" ]] || exit 0
  panc=$(cd "$anc" 2>/dev/null && pwd -P) || exit 0
  root=$(git -C "$panc" rev-parse --show-toplevel 2>/dev/null) || exit 0
  root=$(cd "$root" 2>/dev/null && pwd -P) || exit 0
  # Re-express the full path under the physical ancestor.
  ppath="$panc${path#"$anc"}"
  rel="${ppath#"$root"/}"
  [[ "$rel" == "$ppath" ]] && exit 0
fi

# ---- deny: generated trees ------------------------------------------------
case "$rel" in
  apps/api/internal/db/sqlc/*|apps/api/internal/api/gen/*|apps/web/src/routeTree.gen.ts|packages/openapi-client/src/generated/*)
    emit deny "$rel is GENERATED. Hand-editing the sqlc tree caused a production 500 here.

Regenerate instead:
  sqlc      \$(go env GOPATH)/bin/sqlc generate      (run in apps/api; not on PATH)
  ogen      cd apps/api && go generate ./internal/api/gen/...
  TS client pnpm -C packages/openapi-client generate
  routes    pnpm -C apps/web build

'make gen' and 'scripts/gen-openapi.sh' are STUBS and regenerate nothing."
    ;;
esac

# ---- deny: the dead app ----------------------------------------------------
# Ahead of every other arm so that nothing below can exempt part of it.
case "$rel" in
  apps/landing/*)
    emit deny "apps/landing is DEAD: last commit 2026-06-20, absent from pnpm-workspace.yaml, superseded by apps/marketing. The directory still exists on disk, so editing it succeeds at doing the wrong thing." ;;
esac

# ---- deny: an already-applied migration -----------------------------------
# internal/db/migrate.go sorts versions lexically and skips anything already in
# schema_migrations, so a database that ran this file will never read it again.
# Editing it is a silent no-op that looks like a fix.
case "$rel" in
  apps/api/migrations/*.sql)
    # With no root (the unresolved case above) this question has no answer, so
    # it is not answered: the file falls through to the database-engineer ask
    # below rather than to a deny that might be aimed at brand new work.
    if [[ -n "$root" ]] && git -C "$root" cat-file -e "HEAD:$rel" 2>/dev/null; then
      emit deny "$rel already exists in HEAD, so it has been applied and internal/db/migrate.go will never read it again:

    sort.Strings(versions) ... if applied[version] { continue }

Editing it changes nothing in any database that already ran it. A correction is
a NEW ordinal plus a converge path for databases on the earlier version. m114
and m115 exist for exactly this reason; m114's own header documents it.

Route this to database-engineer. If you believe this is the one legitimate
exception, say so and get a ruling - do not work around the hook."
    fi
    ;;
esac

# ---- pass: a test whose suite CI actually runs -----------------------------
# The gate on a test is whether it passes, and ci.yml answers that on every PR:
# `go test $(go list ./... | grep -v ... '/apps/api/tests$')`, `pnpm --filter
# @wpmgr/web test`, and `composer test` in apps/agent. Routing one of these to a
# specialist buys a worktree round trip and no safety.
#
# apps/api/tests is the deliberate exception and keeps asking: ci.yml excludes
# that package BY NAME from both Build and Test, and api-integration.yml is
# manual-dispatch, so it is the one place a regression merges green. It is also
# where the tenancy and RLS proofs live.
case "$rel" in
  apps/api/tests/*) ;;                                   # falls through, still routes
  apps/api/*_test.go)                    exit 0 ;;
  apps/web/*.test.ts|apps/web/*.test.tsx) exit 0 ;;
  apps/agent/tests/*)                    exit 0 ;;
esac

# ---- ask: routed paths ----------------------------------------------------
agent=""; why=""
case "$rel" in
  apps/api/migrations/*|apps/api/db/*)
      agent="database-engineer"
      why="a migration applies inside main() at boot, so a bad one is an outage on every install at once, and it needs a converge path for databases on the earlier version" ;;
  apps/api/*.go)
      agent="backend-architect" ; why="Go control plane" ;;
  apps/agent/*)
      agent="wp-agent-engineer" ; why="the plugin runs on hosts you cannot inspect and must pass WordPress.org Plugin Check with zero errors" ;;
  apps/web/*|apps/marketing/*|apps/tracker/*|packages/*)
      agent="frontend-architect" ; why="JS/TS surface" ;;
  .github/workflows/*|Makefile|scripts/*|infra/*)
      agent="devops-engineer" ; why="build-gating logic; it needs a committed test" ;;
  docs/worklog/*)
      # THIS WAS A SILENT PERMIT until CLAUDE.md changed. It read `docs/worklog/*) ;;`
      # with a comment saying CLAUDE.md told this session to write its decisions
      # there, so routing it to a subagent would have the router file a ticket to
      # have its own notes taken. That reasoning was sound about ROUTING and said
      # nothing about the path, and the path is now the whole problem.
      emit deny "A worklog must never enter this repository, and $rel is inside it.

CLAUDE.md, \"## Long sessions\": worklogs go to ~/.wpmgr/worklog/<issue>.md, which
is outside every checkout and every worktree. Not docs/, not any other path in
here, not committed, not pushed, not attached to an issue or a PR.

This repository is PUBLIC, and a worklog is the one artefact that routinely holds
what must not be: an unshipped finding, a defect's file:line before the fix
exists, the mechanism of a live vulnerability. On 2026-08-12 a worklog for GH #406
was written to docs/worklog/406.md while the privilege escalation it described was
live, unpatched and shipped, and while the owner's standing ruling was to disclose
nothing until the fix was deployed. It was caught before any commit.

Write it to ~/.wpmgr/worklog/ instead. Do not add a worklog path to .gitignore
either - an ignore file is itself committed, so the line publishes the thing it is
hiding. The correct location leaves nothing to ignore." ;;
  docs/*|README.md|CHANGELOG.md)
      agent="docs-writer" ; why="two ci.yml gates fail on docs prose, and four version surfaces move in lockstep" ;;
esac
[[ -z "$agent" ]] && exit 0

# ---- escalation: explicit directory prefixes, never a substring match ------
# The previous version grepped the whole path for '(auth|...|gc)'. It matched
# 'auth' inside '_authed' and escalated all 69 files under
# apps/web/src/routes/_authed to a mandatory opus review - 259 of 2670 tracked
# files in total. A guard that fires on correct work gets switched off.
sensitive=0
case "$rel" in
  apps/api/internal/auth/*|apps/api/internal/authz/*|apps/api/internal/apikey/*) sensitive=1 ;;
  apps/api/internal/agent/*|apps/api/internal/cryptbox/*|apps/api/internal/httpclient/*) sensitive=1 ;;
  apps/api/internal/org/*|apps/api/internal/sharing/*) sensitive=1 ;;
  apps/api/internal/backup/*|apps/api/internal/restore/*|apps/api/internal/dbclean/*) sensitive=1 ;;
  apps/api/internal/db/db.go|apps/api/migrations/*|apps/api/db/*) sensitive=1 ;;
  apps/agent/includes/class-connector.php|apps/agent/includes/class-signer.php) sensitive=1 ;;
  apps/agent/includes/class-keystore.php|apps/agent/includes/class-media-keystore.php) sensitive=1 ;;
  apps/agent/includes/class-replay-cache.php|apps/agent/includes/class-router.php) sensitive=1 ;;
esac
if [[ $sensitive -eq 1 ]]; then
  agent="$agent, then security-reviewer (mandatory, model opus)"
  why="$why; this path carries auth, tenancy, crypto or irreversible deletion, and nobody approves their own irreversible change"
fi

# ---- ask at most once per destination per session --------------------------
# The key is the destination, not the file: the ruling the prompt asks for is
# "who writes this area", and that answer does not change between two files in
# the same area. Sensitive paths carry their own key, so the first write to an
# auth or deletion path still prompts even if the same specialist was already
# approved for ordinary work.
ttl="${WPMGR_ROUTE_GUARD_TTL_MIN:-90}"
session=$(jq -r '.session_id // empty' <<<"$input" 2>/dev/null)
[[ -z "$session" ]] && session=$(jq -r '.transcript_path // empty' <<<"$input" 2>/dev/null)

sane() { printf '%s' "$1" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64; }

# sane() keeps '.', because a session id legitimately contains one. So the two
# components that do NOT stay inside the directory they are joined to survive it
# unchanged: `session_id: ".."` sanitises to ".." and put the marker one level
# ABOVE the state root. Sanitising is not the same as validating; a component is
# only usable once it has been checked for what it means, not only for what
# characters it contains.
component() { # component <raw> -> one safe path component on stdout, or fails
  local c
  c=$(sane "$1")
  case "$c" in
    ''|.|..)
      printf 'route-guard: %s sanitises to "%s", which does not stay inside the state directory; no state, asking every time\n' "$1" "$c" >&2
      return 1 ;;
  esac
  printf '%s' "$c"
}

# ---- the state directory, and why it is not simply whatever the env says ----
# This block deletes. WPMGR_ROUTE_GUARD_STATE is an environment variable, and the
# prune under it used to be an unconditional
#   find "$state" -mindepth 1 -maxdepth 1 -type d -mtime +2 -exec rm -rf {} +
# so a mis-set, inherited or empty-ish value pointed `rm -rf` at real content.
# With the value '/' that is one batched rm -rf over every top-level directory on
# the machine, and it destroyed this machine's home directory once.
#
# So: the guard deletes only inside a directory it can PROVE is its own, proven
# by a marker file it writes itself when it creates the directory. No marker and
# it did not create it means somebody else's directory: left completely alone,
# not adopted, not pruned, not written to. Refusing the override does not turn
# the guard off - it drops the memory, so the guard asks every time, which is the
# strict direction.
STATE_MARKER=".wpmgr-harness-state"
STATE_DEFAULT="${TMPDIR:-/tmp}/wpmgr-route-guard"

# The SECOND, INDEPENDENT reason the marker write cannot be hijacked. `set -C`
# makes bash open the redirect with O_CREAT|O_EXCL, and POSIX requires open() to
# FAIL when O_CREAT|O_EXCL names a symbolic link - dangling or not. So this
# refuses to follow a planted symlink even if the directory were somehow still
# writable by another account when it runs. Tightening the directory first and
# this are two locks on the same door; either alone would close the reported
# window, and neither is trusted to be the only one.
excl_create() { # excl_create <path> -> create it, never following a symlink
  if ! ( set -C; : > "$1" ) 2>/dev/null; then
    printf 'route-guard: refused to create %q - it already exists or is a symlink; no state, asking every time\n' "$1" >&2
    return 1
  fi
}

resolve_state() { # prints an owned state root on stdout, or fails
  local want="${WPMGR_ROUTE_GUARD_STATE:-$STATE_DEFAULT}"
  local why=""

  # A relative value is not a directory, it is "somewhere relative to whatever
  # cwd this hook happened to be invoked from". Never create one, never use one.
  case "$want" in
    /*) ;;
    *)  printf 'route-guard: WPMGR_ROUTE_GUARD_STATE=%q is not an absolute path; no state, asking every time\n' "$want" >&2
        return 1 ;;
  esac
  while [[ "$want" == */ && "$want" != "/" ]]; do want="${want%/}"; done
  if [[ "$want" == "/" ]]; then
    printf 'route-guard: WPMGR_ROUTE_GUARD_STATE is the filesystem root; refusing it, asking every time\n' >&2
    return 1
  fi

  if [[ -e "$want" ]]; then
    if [[ ! -d "$want" || -L "$want" ]]; then
      printf 'route-guard: state path %q is not a plain directory; refusing it, asking every time\n' "$want" >&2
      return 1
    fi
    # OWNERSHIP IS CHECKED FOR EVERY ACCEPTED PATH, not only for the one branch
    # that adopts. The marker is a fixed name under a fixed, predictable
    # directory name, and ${TMPDIR:-/tmp} falls back to a SHARED /tmp on CI, in
    # containers and on Linux boxes. If presence of the marker alone were enough,
    # another local account could create /tmp/wpmgr-route-guard, drop the marker
    # in it, leave it world-writable and plant session markers - and a planted
    # marker satisfies the TTL check and SUPPRESSES the route prompt. That is the
    # quiet direction, which is the one failure this guard exists to prevent.
    if [[ ! -O "$want" ]]; then
      printf 'route-guard: state path %q is not owned by this user; refusing it, asking every time\n' "$want" >&2
      return 1
    fi
    if [[ -e "$want/$STATE_MARKER" ]]; then
      # The marker is the ownership claim, so the claim itself has to be ours and
      # has to be a real file: a symlink named .wpmgr-harness-state proves
      # nothing about who owns the directory it points into.
      if [[ -L "$want/$STATE_MARKER" || ! -f "$want/$STATE_MARKER" || ! -O "$want/$STATE_MARKER" ]]; then
        printf 'route-guard: %q is not a plain file owned by this user; refusing the state directory, asking every time\n' "$want/$STATE_MARKER" >&2
        return 1
      fi
      # Accepted, so close it: whatever another account had the right to write
      # here, it loses now.
      chmod 700 "$want" || return 1
    elif [[ "$want" == "$STATE_DEFAULT" ]]; then
      # One exception, and only for the DEFAULT path, which we have just proven
      # this user owns: it carries this guard's own name, so a version of the
      # guard that predates the marker is the plausible author. Anything given by
      # the environment is never adopted.
      #
      # ORDER IS THE SECURITY PROPERTY HERE. Owning a directory does not make it
      # private - a directory this user owns can still be group- or
      # world-writable, and between "the marker is absent" and "create the
      # marker" another local account could drop a SYMLINK at that name and have
      # the redirect truncate whatever this user can write. So the directory is
      # closed to 0700 BEFORE anything is written into it, and only then is the
      # marker created. Do not move the chmod below the write.
      chmod 700 "$want" || return 1
      excl_create "$want/$STATE_MARKER" || return 1
    else
      printf 'route-guard: %q has no %s marker, so it is not this guard'"'"'s state directory; leaving it untouched and asking every time\n' "$want" "$STATE_MARKER" >&2
      return 1
    fi
  else
    # Same order on the creation path: private at birth (umask), tightened
    # explicitly in case it already existed as a race, then written to.
    ( umask 077; mkdir -p "$want" ) || return 1
    chmod 700 "$want" || return 1
    excl_create "$want/$STATE_MARKER" || return 1
  fi
  printf '%s' "$want"
}

prune_state() { # prune_state <owned state root>
  # Only entries with the exact name shape sane() produces, and only directories,
  # so the marker file and anything a human parked in here survive. A bare
  # `-type d` glob is what made the old prune a general-purpose deleter. No
  # 2>/dev/null either: a prune that cannot do its job says so.
  ( shopt -s nullglob dotglob
    local e b
    for e in "$1"/*; do
      b=${e##*/}
      [[ "$b" == "$STATE_MARKER" ]] && continue
      [[ -d "$e" && ! -L "$e" ]] || continue
      [[ "$b" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || continue
      # Sessions are per-run and never read again; two days is generous.
      [[ -n "$(find "$e" -maxdepth 0 -mtime +2)" ]] || continue
      rm -rf -- "$e"
    done )
}

repeat_note=""
if [[ -n "$session" && "$ttl" =~ ^[0-9]+$ && "$ttl" -gt 0 ]] && state=$(resolve_state); then
  prune_state "$state"
fi

# Both halves of the key are validated, not merely sanitised, before either one
# is joined to a path. A failure here drops the memory and falls through to the
# ask, which is the strict direction.
if [[ -n "${state:-}" ]] && sess_c=$(component "$session") \
   && dest_c=$(component "${agent}|${sensitive}"); then
  sdir="$state/$sess_c"
  marker="$sdir/$dest_c"

  if [[ "$mode" == record ]]; then
    # PostToolUse: the tool ran, so the ruling was given and was an approval.
    # The same order as the state root, for the same reason: private at birth,
    # and never write through a symlink. This one lives inside a directory
    # resolve_state has already proven is ours and forced to 0700, so no other
    # account can create anything here - but the session marker is refreshed on
    # every record, so it cannot use O_EXCL and gets an explicit refusal instead.
    ( umask 077; mkdir -p "$sdir" ) || exit 0
    [[ -L "$marker" || ( -e "$marker" && ! -f "$marker" ) ]] && exit 0
    : > "$marker"
    exit 0
  fi

  # A marker only silences the prompt if THIS user wrote it. The state root is
  # ownership-checked above, but a marker planted while it was still loose would
  # outlive that check, so the file actually being trusted is checked directly.
  if [[ -d "$sdir" && ! -L "$sdir" && -O "$sdir" \
        && -f "$marker" && ! -L "$marker" && -O "$marker" \
        && -n "$(find "$marker" -mmin "-$ttl" 2>/dev/null)" ]]; then
    exit 0   # already ruled on, this session, this destination
  fi
  repeat_note="
This asks once per destination per session (${ttl} minutes) once you have let a
write through. Later writes to this same area will not prompt, so make the
ruling now rather than waving it through."
fi

# Nothing to record: no session identity, no TTL, or no state directory this
# guard owns. Recording is best-effort; asking is not.
[[ "$mode" == record ]] && exit 0

emit ask "CLAUDE.md routes this file to a specialist: ${agent}.
Reason: ${why}.
Path: ${rel}

Launch the subagent instead of editing here, and give it one job sized to finish
in about ten minutes, with the key files pre-resolved and the
commit-before-the-slow-test clause.

If this genuinely does not need a specialist, say so out loud and get a ruling -
do not decide it silently.${repeat_note}"
