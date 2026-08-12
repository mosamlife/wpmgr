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

path=$(jq -r '.tool_input.file_path // .tool_input.notebook_path // empty' <<<"$input" 2>/dev/null)
[[ -z "$path" ]] && exit 0
case "$path" in /*) ;; *) exit 0 ;; esac

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
dir=$(dirname "$path")
anc="$dir"
while [[ ! -d "$anc" && "$anc" != "/" && -n "$anc" ]]; do anc=$(dirname "$anc"); done
[[ -d "$anc" ]] || exit 0
panc=$(cd "$anc" 2>/dev/null && pwd -P) || exit 0
root=$(git -C "$panc" rev-parse --show-toplevel 2>/dev/null) || exit 0
root=$(cd "$root" 2>/dev/null && pwd -P) || exit 0
# Re-express the full path under the physical ancestor.
ppath="$panc${path#"$anc"}"
rel="${ppath#"$root"/}"
[[ "$rel" == "$ppath" ]] && exit 0

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
    if git -C "$root" cat-file -e "HEAD:$rel" 2>/dev/null; then
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
  docs/worklog/*) ;;                                     # see below
  docs/*|README.md|CHANGELOG.md)
      agent="docs-writer" ; why="two ci.yml gates fail on docs prose, and four version surfaces move in lockstep" ;;
esac
# docs/worklog is where CLAUDE.md instructs THIS session to write its decisions
# as they are made. Routing that to a subagent would have the router file a
# ticket to have its own notes taken.
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
    elif [[ "$want" == "$STATE_DEFAULT" ]]; then
      # One exception, and only for the DEFAULT path, which we have just proven
      # this user owns: it carries this guard's own name, so a version of the
      # guard that predates the marker is the plausible author. Anything given by
      # the environment is never adopted.
      : > "$want/$STATE_MARKER" || return 1
    else
      printf 'route-guard: %q has no %s marker, so it is not this guard'"'"'s state directory; leaving it untouched and asking every time\n' "$want" "$STATE_MARKER" >&2
      return 1
    fi
    # It is ours, so close it: anything another account already had the right to
    # write here, it loses now.
    chmod 700 "$want" || return 1
  else
    ( umask 077; mkdir -p "$want" ) || return 1
    chmod 700 "$want" || return 1
    : > "$want/$STATE_MARKER" || return 1
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
    mkdir -p "$sdir" && : > "$marker"
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
