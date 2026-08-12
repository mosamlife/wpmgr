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
# Test: scripts/claude/guards_test.sh
# Over-fire rate: scripts/claude/route-guard-coverage.sh
set -uo pipefail

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

repeat_note=""
if [[ -n "$session" && "$ttl" =~ ^[0-9]+$ && "$ttl" -gt 0 ]]; then
  state="${WPMGR_ROUTE_GUARD_STATE:-${TMPDIR:-/tmp}/wpmgr-route-guard}"
  # Sessions are per-run and never read again; two days is generous.
  find "$state" -mindepth 1 -maxdepth 1 -type d -mtime +2 -exec rm -rf {} + 2>/dev/null

  sane() { printf '%s' "$1" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64; }
  sdir="$state/$(sane "$session")"
  marker="$sdir/$(sane "${agent}|${sensitive}")"

  if [[ -n "$(find "$marker" -mmin "-$ttl" 2>/dev/null)" ]]; then
    exit 0   # already ruled on, this session, this destination
  fi
  mkdir -p "$sdir" 2>/dev/null && : > "$marker" 2>/dev/null
  repeat_note="
This asks once per destination per session (${ttl} minutes). Later writes to
this same area will not prompt, so make the ruling now rather than waving it
through."
fi

emit ask "CLAUDE.md routes this file to a specialist: ${agent}.
Reason: ${why}.
Path: ${rel}

Launch the subagent instead of editing here, and give it one job sized to finish
in about ten minutes, with the key files pre-resolved and the
commit-before-the-slow-test clause.

If this genuinely does not need a specialist, say so out loud and get a ruling -
do not decide it silently.${repeat_note}"
