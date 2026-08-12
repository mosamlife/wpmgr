#!/usr/bin/env bash
# Regression suite for the PreToolUse guards.
#
# This exists because of the rule the guards themselves enforce: prove it fires,
# then prove it does not over-fire, and never ship a guard whose proof lived in
# a scratch directory and was thrown away. Every case below is one the shipped
# guards got wrong at least once:
#
#   - the whole apps/web/src/routes/_authed tree escalated to a mandatory opus
#     review because '_authed' contains the substring 'auth' (69 files, and 259
#     of 2670 tracked files overall)
#   - every routed path passed silently when the session cwd was a repo
#     subdirectory, because the guard built its relative path from .cwd
#   - apps/marketing, apps/tracker, docs and infra were in the routing table and
#     in no arm of the guard
#   - generated trees routed to a builder instead of being refused
#   - an unbounded wait loop was allowed by a trailing '# timeout' comment
#   - 'gh pr create' published prose with no rule attached
#   - the guard asked on 91% of every file touched in 30 days, so the per-file
#     prompt was noise; it now asks once per destination per session
#   - every deny arm was bypassable with `sed -i`, `tee` or a heredoc, because
#     the matcher is Edit|Write|NotebookEdit and a Bash write is neither
#
# Builds a throwaway git repo, feeds the guards real hook JSON, asserts the
# decision. No network, no Docker, no repo state touched.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
ROUTE="$here/route-guard.sh"
BASH_G="$here/bash-guard.sh"

command -v jq >/dev/null 2>&1 || { echo "SKIP: jq is not installed, and the guards fail open without it"; exit 0; }

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/repo"
# Physical path: on macOS /tmp is a symlink to /private/tmp, and the guard
# compares against `git rev-parse --show-toplevel`, which is always physical.
tmp=$(cd "$tmp" && pwd -P)
repo="$tmp/repo"
git -C "$repo" init -q
git -C "$repo" config user.email t@t.invalid
git -C "$repo" config user.name t

mkdir -p "$repo"/apps/api/{migrations,db,tests,internal/{site,auth,db/sqlc,api/gen,backup}} \
         "$repo"/apps/web/src/routes/_authed/sites "$repo"/apps/web/src \
         "$repo"/apps/agent/{includes,tests} "$repo"/apps/marketing/lib/content \
         "$repo"/apps/tracker/src "$repo"/apps/landing "$repo"/docs/worklog \
         "$repo"/.github/workflows "$repo"/infra "$repo"/packages/openapi-client/src
echo "-- applied" > "$repo/apps/api/migrations/20260101000000_m01_applied.sql"
# Staged BY NAME, like every commit in this project. This used to stage the
# whole tree, which CLAUDE.md forbids everywhere and which this suite exists to
# prove: a proof that breaks the rule it proves is not a proof. Asserted about
# this file at the end of the suite, so it cannot quietly come back.
git -C "$repo" add apps/api/migrations/20260101000000_m01_applied.sql >/dev/null
git -C "$repo" commit -qm init

pass=0; failed=0

# The guard remembers which destinations a session has already been asked about.
# Point that state at the throwaway tree so a developer's real session state can
# neither leak into this suite nor be clobbered by it.
export WPMGR_ROUTE_GUARD_STATE="$tmp/guard-state"

# decision <file_path> [cwd] [agent_id] [session_id]
# With no session_id the guard cannot de-duplicate and asks every time, which is
# what every assertion written before the de-duplication existed relies on.
decision() {
  local fp="$1" cwd="${2:-$repo}" aid="${3:-}" sid="${4:-}"
  local json
  json=$(jq -n --arg p "$fp" --arg c "$cwd" --arg a "$aid" --arg s "$sid" \
    '{cwd:$c, tool_input:{file_path:$p}}
     + (if $a == "" then {} else {agent_id:$a} end)
     + (if $s == "" then {} else {session_id:$s} end)')
  local out
  out=$(printf '%s' "$json" | bash "$ROUTE" 2>/dev/null \
    | jq -r '.hookSpecificOutput.permissionDecision // empty' 2>/dev/null)
  printf '%s' "${out:-pass}"
}
reason() {
  jq -n --arg p "$1" --arg c "$repo" '{cwd:$c, tool_input:{file_path:$p}}' \
    | bash "$ROUTE" 2>/dev/null | jq -r '.hookSpecificOutput.permissionDecisionReason // ""' 2>/dev/null
}

t() { # t <label> <expected> <actual>
  if [[ "$2" == "$3" ]]; then pass=$((pass+1))
  else echo "FAIL  $1: expected '$2', got '$3'"; failed=$((failed+1)); fi
}
tcontains() { # tcontains <label> <needle> <haystack>
  if grep -q -- "$2" <<<"$3"; then pass=$((pass+1))
  else echo "FAIL  $1: output did not contain '$2'"; failed=$((failed+1)); fi
}
tlacks() {
  if grep -q -- "$2" <<<"$3"; then echo "FAIL  $1: output unexpectedly contained '$2'"; failed=$((failed+1))
  else pass=$((pass+1)); fi
}

echo "== route-guard: it fires"
t "go control plane"      ask  "$(decision "$repo/apps/api/internal/site/service.go")"
t "web route"             ask  "$(decision "$repo/apps/web/src/routes/_authed/sites/index.tsx")"
t "marketing content"     ask  "$(decision "$repo/apps/marketing/lib/content/home.ts")"
t "tracker"               ask  "$(decision "$repo/apps/tracker/src/index.ts")"
t "agent php"             ask  "$(decision "$repo/apps/agent/includes/class-router.php")"
t "workflow"              ask  "$(decision "$repo/.github/workflows/ci.yml")"
t "infra"                 ask  "$(decision "$repo/infra/cloudbuild.api.yaml")"
t "docs"                  ask  "$(decision "$repo/docs/install.md")"
t "README"                ask  "$(decision "$repo/README.md")"
t "new migration"         ask  "$(decision "$repo/apps/api/migrations/20260201000000_m02_new.sql")"

echo "== route-guard: it denies what is not a judgement call"
t "applied migration"     deny "$(decision "$repo/apps/api/migrations/20260101000000_m01_applied.sql")"
t "sqlc tree"             deny "$(decision "$repo/apps/api/internal/db/sqlc/db.go")"
t "ogen tree"             deny "$(decision "$repo/apps/api/internal/api/gen/oas.go")"
t "routeTree.gen.ts"      deny "$(decision "$repo/apps/web/src/routeTree.gen.ts")"
t "generated ts client"   deny "$(decision "$repo/packages/openapi-client/src/generated/sdk.ts")"
t "dead app"              deny "$(decision "$repo/apps/landing/index.html")"
# A Write that CREATES a file in a directory that does not exist yet must still
# route. Deriving the repo root from dirname alone waves this through, and
# creating a new file in a new feature directory is a normal thing to do.
t "new dir, new file"     deny "$(decision "$repo/packages/openapi-client/src/generated/nested/deep/sdk.ts")"

echo "== route-guard: it does not over-fire"
t "inside a subagent"     pass "$(decision "$repo/apps/api/internal/site/service.go" "$repo" "agent-x")"
t "unrouted root file"    pass "$(decision "$repo/notes.txt")"
t "outside any repo"      pass "$(decision "/tmp/scratch.go")"
t "relative path"         pass "$(decision "apps/api/x.go")"
# The bug that silently disabled the whole guard: cwd in a subdirectory.
t "cwd=apps/api"          ask  "$(decision "$repo/apps/api/internal/site/service.go" "$repo/apps/api")"
t "cwd=apps/web/src"      ask  "$(decision "$repo/apps/web/src/routes/_authed/sites/index.tsx" "$repo/apps/web/src")"

echo "== route-guard: a test whose suite CI runs does not prompt"
# ci.yml runs `go test` over ./internal/..., `pnpm --filter @wpmgr/web test` and
# `composer test`. The gate on those tests is whether they pass, and CI answers
# that on every PR, so routing them buys a worktree round trip and no safety.
t "go unit test"          pass "$(decision "$repo/apps/api/internal/site/service_test.go")"
t "web test"              pass "$(decision "$repo/apps/web/src/routes/_authed/sites/index.test.tsx")"
t "agent phpunit"         pass "$(decision "$repo/apps/agent/tests/RouterTest.php")"
t "worklog"               pass "$(decision "$repo/docs/worklog/402.md")"
# The deliberate exception: ci.yml excludes this package BY NAME from Build and
# Test, and api-integration.yml is manual-dispatch, so a regression here merges
# green. It is also where the tenancy and RLS proofs live.
t "apps/api/tests"        ask  "$(decision "$repo/apps/api/tests/tenancy_test.go")"
# Narrowing the ask arm must not have narrowed a deny arm.
t "dead app test file"    deny "$(decision "$repo/apps/landing/app.test.tsx")"

echo "== route-guard: asks once per destination per session"
# The measured reason: the per-file version asked on 843 of the 926 files
# touched in 30 days. Routing changes the outcome when work STARTS, not on the
# fortieth file of the same work.
#
# The ruling is remembered only once it has actually been GIVEN. The PreToolUse
# arm cannot see the answer, so it writes nothing; the PostToolUse arm
# (--record) runs only if the tool actually ran, which is the one piece of
# evidence the hook protocol gives that the prompt was approved.
record_route() { # record_route <session_id> <abs file path>
  jq -n --arg p "$2" --arg c "$repo" --arg s "$1" \
    '{cwd:$c, session_id:$s, tool_input:{file_path:$p}}' \
    | bash "$ROUTE" --record >/dev/null 2>&1
}

t "1st Go file, session A"  ask  "$(decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessA)"
record_route sessA "$repo/apps/api/internal/site/service.go"
t "2nd Go file, session A"  pass "$(decision "$repo/apps/api/internal/site/other.go"   "$repo" "" sessA)"
t "same file again"         pass "$(decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessA)"
# A different destination is a different ruling and must still be asked.
t "then a web file"         ask  "$(decision "$repo/apps/web/src/app.tsx"              "$repo" "" sessA)"
record_route sessA "$repo/apps/web/src/app.tsx"
# ...including the escalation: an auth path is a different ruling from ordinary
# Go work by the same specialist, so it prompts even though backend-architect
# was already approved above.
t "then an auth file"       ask  "$(decision "$repo/apps/api/internal/auth/session.go" "$repo" "" sessA)"
record_route sessA "$repo/apps/api/internal/auth/session.go"
t "auth file again"         pass "$(decision "$repo/apps/api/internal/auth/token.go"   "$repo" "" sessA)"
# A different session has made no ruling yet.
t "1st Go file, session B"  ask  "$(decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessB)"
# Remembering must never weaken a deny.
t "deny is not remembered"  deny "$(decision "$repo/apps/api/internal/db/sqlc/db.go"   "$repo" "" sessA)"
t "deny again"              deny "$(decision "$repo/apps/api/internal/db/sqlc/db.go"   "$repo" "" sessA)"
# A deny arm has no ruling to record, so recording one must not create a marker
# that could later silence a genuine ask.
record_route sessA "$repo/apps/api/internal/db/sqlc/db.go"
t "deny after record"       deny "$(decision "$repo/apps/api/internal/db/sqlc/db.go"   "$repo" "" sessA)"
# TTL 0 disables the memory entirely: a long session comes back to the same area
# hours later and gets asked again.
t "ttl 0 re-asks" ask \
  "$(export WPMGR_ROUTE_GUARD_TTL_MIN=0; decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessA)"
# No session identity in the payload means no memory, and the guard falls back
# to asking rather than to silence.
t "no session id asks"      ask  "$(decision "$repo/apps/api/internal/site/deux.go")"

echo "== route-guard: a refused prompt makes it stricter, never quieter"
# The defect: the marker was persisted by the PreToolUse arm, BEFORE the human
# answered. Denying or cancelling the prompt therefore suppressed every later
# write to that destination for the whole TTL window, so refusing the guard was
# the way to switch it off. A decision is only remembered once it has been
# given, and only when it was an approval.
t "1st ask, session C"      ask  "$(decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessC)"
t "denied, so it re-asks"   ask  "$(decision "$repo/apps/api/internal/site/other.go"   "$repo" "" sessC)"
t "still asks, third time"  ask  "$(decision "$repo/apps/api/internal/site/trois.go"   "$repo" "" sessC)"
# ...and once a write to that destination actually goes through, it goes quiet.
record_route sessC "$repo/apps/api/internal/site/service.go"
t "approved, then quiet"    pass "$(decision "$repo/apps/api/internal/site/other.go"   "$repo" "" sessC)"
# Approving one destination rules on nothing else.
t "other destination asks"  ask  "$(decision "$repo/apps/web/src/other.tsx"            "$repo" "" sessC)"

echo "== route-guard state: it deletes only inside a directory it owns"
# The state root is overridable by WPMGR_ROUTE_GUARD_STATE, and the prune step
# ran `find ... -exec rm -rf` under it unconditionally. A mis-set or inherited
# value pointed that at real content. An override is honoured only when the
# guard created the directory or the directory carries the guard's own marker,
# and only entries with the exact name shape the guard writes are ever removed.
#
# NOTE ON THE PROOF: the same mechanism with the override set to '/' would have
# deleted every top-level directory on the machine, so the pre-fix behaviour was
# demonstrated against this stand-in tree, never against a real root. The '/'
# case below asserts the refusal only.
foreign="$tmp/foreign"
mkdir -p "$foreign/personal-notes" "$foreign/scratch"
touch -t 200001010000 "$foreign/personal-notes" "$foreign/scratch"
( export WPMGR_ROUTE_GUARD_STATE="$foreign"
  decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessF >/dev/null )
exists() { [[ -e "$1" ]] && printf yes || printf no; }
t "foreign dir: content kept"  yes "$(exists "$foreign/personal-notes")"
t "foreign dir: kept (2)"      yes "$(exists "$foreign/scratch")"
t "foreign dir: not adopted"   no  "$(exists "$foreign/.wpmgr-harness-state")"

# A relative override is not a state directory; it is a path relative to
# whatever directory the hook happened to be invoked from.
( export WPMGR_ROUTE_GUARD_STATE="relative-state"
  decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessG >/dev/null )
t "relative override refused"  no  "$(exists "$tmp/relative-state")"
t "relative, cwd-relative too" no  "$(exists "$repo/relative-state")"

# The filesystem root: refused outright, and never stamped as ours.
( export WPMGR_ROUTE_GUARD_STATE="/"
  decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessH >/dev/null )
t "root override refused"      no  "$(exists "/.wpmgr-harness-state")"
t "root override still asks"   ask \
  "$(export WPMGR_ROUTE_GUARD_STATE="/"; decision "$repo/apps/api/internal/site/thing.go" "$repo" "" sessH)"

# A directory the guard creates is its own, and inside it the prune removes only
# entries with the name shape the guard writes - never a bare glob.
own="$tmp/owned-state"
( export WPMGR_ROUTE_GUARD_STATE="$own"
  decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessI >/dev/null )
t "own dir created"            yes "$(exists "$own")"
t "own dir stamped"            yes "$(exists "$own/.wpmgr-harness-state")"
mkdir -p "$own/sess-stale" "$own/not a session dir"
touch -t 200001010000 "$own/sess-stale" "$own/not a session dir" "$own/.wpmgr-harness-state"
( export WPMGR_ROUTE_GUARD_STATE="$own"
  decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessJ >/dev/null )
t "stale session dir pruned"   no  "$(exists "$own/sess-stale")"
t "off-shape entry kept"       yes "$(exists "$own/not a session dir")"
t "marker survives the prune"  yes "$(exists "$own/.wpmgr-harness-state")"
t "own dir is not group/world readable" 700 "$(stat -f '%OLp' "$own" 2>/dev/null || stat -c '%a' "$own")"

echo "== route-guard state: the marker is a claim, so the claim is checked too"
# The marker is a FIXED name under a FIXED directory name, and ${TMPDIR:-/tmp}
# falls back to a shared /tmp in CI, in containers and on Linux boxes. If the
# marker's presence alone were enough, another local account could create the
# directory, drop the marker in it and plant session markers - and a planted
# marker satisfies the TTL check and SUPPRESSES the prompt, which is the quiet
# direction this guard exists to prevent. So ownership is checked for every
# accepted path, not only on the branch that adopts one.
#
# This fixture is real, not stubbed: it looks for a directory that genuinely has
# another owner and is genuinely writable by us, which is exactly the shared-/tmp
# shape. `rm` is shadowed for the duration so that a regression in the guard
# cannot prune somebody else's temp directory to prove the point.
unowned=""
for cand in /private/var/tmp /var/tmp /private/tmp /tmp; do
  [[ -d "$cand" && ! -L "$cand" && -w "$cand" && ! -O "$cand" ]] || continue
  unowned="$cand"; break
done
if [[ -z "$unowned" ]]; then
  echo "SKIP  no directory on this machine is both writable and owned by another user; the unowned-state assertions did not run"
else
  planted="$unowned/.wpmgr-harness-state"
  had_marker=$(exists "$planted")
  [[ "$had_marker" == yes ]] || : > "$planted"
  stub="$tmp/stub"; mkdir -p "$stub"
  printf '#!/bin/sh\necho "rm $*" >> %s/rm.log\nexit 0\n' "$tmp" > "$stub/rm"
  chmod +x "$stub/rm"
  rm -f "$tmp/rm.log"
  ( export WPMGR_ROUTE_GUARD_STATE="$unowned" PATH="$stub:$PATH"
    decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessU >/dev/null )
  # Pin the OWNERSHIP branch by name. Asserting only "it was refused" passed even
  # with the ownership test deleted, because `chmod 700` on a root-owned
  # directory fails and refuses it anyway - a second reason, not the one under
  # test. An assertion that survives the removal of the code it names tests
  # nothing.
  err=$( ( export WPMGR_ROUTE_GUARD_STATE="$unowned"
           jq -n --arg c "$repo" --arg p "$repo/apps/api/internal/site/service.go" --arg s sessU \
             '{cwd:$c, session_id:$s, tool_input:{file_path:$p}}' \
           | bash "$ROUTE" 2>&1 >/dev/null ) )
  tcontains "unowned dir: refused by the owner check" "not owned by this user" "$err"
  t "unowned dir: never pruned"  no  "$(exists "$tmp/rm.log")"
  t "unowned dir: no session dir" no "$(exists "$unowned/sessU")"
  t "unowned dir: still asks"    ask \
    "$(export WPMGR_ROUTE_GUARD_STATE="$unowned"; decision "$repo/apps/api/internal/site/x.go" "$repo" "" sessU)"
  # ...and a marker planted there can never silence a later prompt.
  mkdir -p "$unowned/sessU" 2>/dev/null && : > "$unowned/sessU/backend-architect-0" 2>/dev/null
  t "planted marker does not silence" ask \
    "$(export WPMGR_ROUTE_GUARD_STATE="$unowned"; decision "$repo/apps/api/internal/site/y.go" "$repo" "" sessU)"
  rm -rf "$unowned/sessU"
  [[ "$had_marker" == yes ]] || rm -f "$planted"
fi

echo "== route-guard state: a path component is validated, not merely sanitised"
# sane() is `tr -c 'a-zA-Z0-9._-'`, and '.' is in the keep-set, so the two
# components that do NOT stay inside the directory they are joined to survive it
# unchanged. Measured before the fix: session_id ".." put the destination marker
# in the state root's PARENT. Sanitising is not validating.
esc="$tmp/escape"
mkdir -p "$esc/state"
: > "$esc/state/.wpmgr-harness-state"
# The payload PostToolUse actually delivers: hook_event_name, tool_name and
# tool_response ride along with the fields the guard reads.
record_post() { # record_post <session_id> <abs file path>
  jq -n --arg p "$2" --arg c "$repo" --arg s "$1" \
    '{session_id:$s, transcript_path:"/dev/null", cwd:$c,
      hook_event_name:"PostToolUse", tool_name:"Edit",
      tool_input:{file_path:$p}, tool_response:{filePath:$p, success:true}}' \
    | bash "$ROUTE" --record >/dev/null 2>&1
}
( export WPMGR_ROUTE_GUARD_STATE="$esc/state"
  record_post ".." "$repo/apps/api/internal/site/service.go"
  record_post "."  "$repo/apps/api/internal/site/service.go" )
t "'..' writes nothing above"   "state" "$(ls -A "$esc")"
t "'.' writes nothing in root"  ".wpmgr-harness-state" "$(ls -A "$esc/state")"
t "'..' still asks"             ask \
  "$(export WPMGR_ROUTE_GUARD_STATE="$esc/state"; decision "$repo/apps/api/internal/site/service.go" "$repo" "" "..")"
# ...and the real PostToolUse payload shape still records for an ordinary id.
( export WPMGR_ROUTE_GUARD_STATE="$esc/state"
  record_post sessK "$repo/apps/api/internal/site/service.go" )
t "PostToolUse shape records"   pass \
  "$(export WPMGR_ROUTE_GUARD_STATE="$esc/state"; decision "$repo/apps/api/internal/site/other.go" "$repo" "" sessK)"

echo "== settings.json: the --record arm is actually wired"
# Until something invokes --record, no ruling is ever recorded and every routed
# write prompts again. PreToolUse asks; only PostToolUse knows the tool ran.
SETTINGS="$(cd "$here/../.." && pwd)/.claude/settings.json"
t "settings.json parses" 0 "$(jq -e . "$SETTINGS" >/dev/null 2>&1; printf '%s' "$?")"
post_edit=$(jq -r '.hooks.PostToolUse[] | select(.matcher == "Edit|Write|NotebookEdit") | .hooks[].command' "$SETTINGS" 2>/dev/null)
tcontains "PostToolUse keeps agent-writes" "agent-writes.sh"        "$post_edit"
tcontains "PostToolUse records rulings"    "route-guard.sh\" --record" "$post_edit"

echo "== route-guard: escalation is by directory, not by substring"
tlacks   "_authed does not escalate"  "security-reviewer" "$(reason "$repo/apps/web/src/routes/_authed/sites/index.tsx")"
tcontains "internal/auth escalates"   "security-reviewer" "$(reason "$repo/apps/api/internal/auth/session.go")"
tcontains "internal/backup escalates" "security-reviewer" "$(reason "$repo/apps/api/internal/backup/reclaim.go")"
tlacks   "internal/site does not"     "security-reviewer" "$(reason "$repo/apps/api/internal/site/service.go")"
tcontains "routes to database-engineer" "database-engineer" "$(reason "$repo/apps/api/migrations/20260201000000_m02_new.sql")"

echo "== bash-guard"
bdec() {
  local out
  out=$(jq -n --arg c "$1" '{tool_input:{command:$c}}' | bash "$BASH_G" 2>/dev/null \
    | jq -r '.hookSpecificOutput.permissionDecision // empty' 2>/dev/null)
  printf '%s' "${out:-pass}"
}

t "until+sleep"            deny "$(bdec 'until grep -q DONE /tmp/x.log; do sleep 30; done')"
t "while true+sleep"       deny "$(bdec 'while true; do sleep 5; docker ps; done')"
# The exact bypass the previous version had.
t "until+sleep+# timeout"  deny "$(bdec 'until grep -q DONE /tmp/x.log; do sleep 30; done # timeout handled by CI')"
t "gh run watch loop"      deny "$(bdec 'until [ "$(gh run view 1 --json status -q .status)" = completed ]; do sleep 60; done')"

t "bounded seq loop"       pass "$(bdec 'for i in $(seq 1 60); do grep -q DONE /tmp/x.log && break; sleep 10; done')"
t "bare sleep"             pass "$(bdec 'sleep 5')"
t "plain make"             pass "$(bdec 'make test-integration')"
t "for over files"         pass "$(bdec 'for f in *.go; do gofmt -l "$f"; done')"

# Publishing outside this repository ASKS, it does not auto-approve. These three
# asserted 'allow' when written, which auto-approved gh pr create, gh issue
# comment and gh release create on a PUBLIC repo: the advice was injected and
# the action went ahead without a prompt. An irreversible outward-facing action
# gets a human decision, and the advice rides along with the prompt.
t "gh pr create"           ask   "$(bdec 'gh pr create --title x --body y')"
t "gh issue comment"       ask   "$(bdec 'gh issue comment 402 --body "fixed"')"
t "gh release create"      ask   "$(bdec 'gh release create v1.0.0')"
t "gh pr list"             pass  "$(bdec 'gh pr list --limit 5')"
t "git commit"             pass  "$(bdec 'git commit -m x')"

echo "== bash-guard: the shell route around the Edit/Write deny arms"
# route-guard.sh is a PreToolUse hook on Edit|Write|NotebookEdit, so a heredoc,
# `sed -i`, `tee` or `cp` reached every denied path without it ever running.
bdecw() { # bdec with a cwd, so the migration check can resolve HEAD
  local out
  out=$(jq -n --arg c "$repo" --arg c2 "$1" '{cwd:$c, tool_input:{command:$c2}}' \
        | bash "$BASH_G" 2>/dev/null \
        | jq -r '.hookSpecificOutput.permissionDecision // empty' 2>/dev/null)
  printf '%s' "${out:-pass}"
}

t "heredoc into sqlc"   deny "$(bdec 'cat > apps/api/internal/db/sqlc/db.go <<EOF
x
EOF')"
t "redirect into gen"   deny "$(bdec 'echo x > apps/api/internal/api/gen/oas.go')"
t "append into sqlc"    deny "$(bdec 'printf x >> apps/api/internal/db/sqlc/models.go')"
t "sed -i on sqlc"      deny "$(bdec "sed -i '' s/a/b/ apps/api/internal/db/sqlc/db.go")"
t "tee into sqlc"       deny "$(bdec 'echo x | tee apps/api/internal/db/sqlc/db.go')"
t "tee -a into sqlc"    deny "$(bdec 'echo x | tee -a apps/api/internal/db/sqlc/db.go')"
t "cp into sqlc"        deny "$(bdec 'cp /tmp/db.go apps/api/internal/db/sqlc/db.go')"
t "mv into routeTree"   deny "$(bdec 'mv /tmp/x apps/web/src/routeTree.gen.ts')"
t "rm the sqlc tree"    deny "$(bdec 'rm -rf apps/api/internal/db/sqlc/')"
t "python writes sqlc"  deny "$(bdec "python3 -c \"open('apps/api/internal/db/sqlc/db.go','w').write('x')\"")"
t "write into dead app" deny "$(bdec 'echo x > apps/landing/index.html')"

# ...and the honest cases it must NOT block. Reading, listing and grepping these
# paths is ordinary work, and the regeneration commands must obviously survive.
t "grep sqlc to a file" pass "$(bdec 'grep -r Query apps/api/internal/db/sqlc/ > /tmp/out.txt')"
t "cat a sqlc file"     pass "$(bdec 'cat apps/api/internal/db/sqlc/db.go')"
t "copy OUT of sqlc"    pass "$(bdec 'cp apps/api/internal/db/sqlc/db.go /tmp/')"
t "ls the gen tree"     pass "$(bdec 'ls -la apps/api/internal/api/gen/')"
t "sqlc regenerates"    pass "$(bdec '$(go env GOPATH)/bin/sqlc generate')"
t "ogen regenerates"    pass "$(bdec 'go generate ./internal/api/gen/...')"
t "web build"           pass "$(bdec 'pnpm -C apps/web build')"
t "sed -i elsewhere"    pass "$(bdec "sed -i '' s/a/b/ apps/api/internal/site/service.go")"
# Deleting the dead app is the correct end state, so only writing into it is refused.
t "rm the dead app"     pass "$(bdec 'rm -rf apps/landing')"

echo "== bash-guard: the destination is the write command's, not the line's"
# The bypass this closes, reported independently by both review bots: the
# destination of cp/mv/rsync/install/truncate was taken as the LAST WHITESPACE
# TOKEN of the whole command. A trailing comment or a chained command moves it,
# so `cp secret.go apps/api/internal/db/sqlc/models.go # note` made the last
# token 'note' and the write was permitted. This was reported to the owner as
# closed while these shapes were open.
t "cp + trailing comment"   deny "$(bdec 'cp secret.go apps/api/internal/db/sqlc/models.go # note')"
t "cp + comment, no space"  deny "$(bdec 'cp secret.go apps/api/internal/db/sqlc/models.go #note')"
t "mv + && chain"           deny "$(bdec 'mv /tmp/x apps/api/internal/api/gen/oas.go && echo done')"
t "; chain then cp"         deny "$(bdec 'echo hi; cp /tmp/x apps/api/internal/db/sqlc/db.go')"
t "chain then cp + comment" deny "$(bdec 'ls -la /tmp && cp /tmp/x apps/web/src/routeTree.gen.ts # swap it in')"
t "|| chain then mv"        deny "$(bdec 'test -f /tmp/x || mv /tmp/y packages/openapi-client/src/generated/sdk.ts')"
t "pipe then cp"            deny "$(bdec 'ls /tmp | head -1; cp /tmp/x apps/api/internal/db/sqlc/db.go')"
t "rsync + comment"         deny "$(bdec 'rsync -a /tmp/gen/ apps/api/internal/api/gen/ # regenerated elsewhere')"
t "install + comment"       deny "$(bdec 'install -m 644 /tmp/db.go apps/api/internal/db/sqlc/db.go # perms')"
t "truncate + comment"      deny "$(bdec 'truncate -s 0 apps/web/src/routeTree.gen.ts # blank it')"
t "cp + trailing redirect"  deny "$(bdec 'cp /tmp/x apps/api/internal/db/sqlc/db.go 2>/dev/null')"
t "cp into dead app + cmt"  deny "$(bdec 'cp /tmp/index.html apps/landing/index.html # revive it')"
t "env prefix then cp"      deny "$(bdec 'FOO=1 cp /tmp/x apps/api/internal/db/sqlc/db.go')"
t "absolute /bin/cp"        deny "$(bdec '/bin/cp /tmp/x apps/api/internal/db/sqlc/db.go # note')"

# ...and the honest cases these must not block. A protected path as the SOURCE
# of a chained copy is ordinary reading, and a guard that refuses it gets
# switched off.
t "protected path is source"    pass "$(bdec 'cp apps/api/internal/db/sqlc/db.go /tmp/x && echo done')"
t "source + comment"            pass "$(bdec 'cp apps/api/internal/db/sqlc/db.go /tmp/ # for reference')"
t "source, then unrelated cp"   pass "$(bdec 'cp apps/api/internal/api/gen/oas.go /tmp/a; cp /tmp/b /tmp/c')"
t "rsync OUT of the dead app"   pass "$(bdec 'rsync -a apps/landing/ /tmp/landing-backup/ # archive before deleting')"
t "mv the dead app aside"       pass "$(bdec 'mv apps/landing /tmp/landing-old')"
t "cp two files out of gen"     pass "$(bdec 'cp apps/api/internal/api/gen/oas.go apps/api/internal/api/gen/x.go /tmp/')"

# An applied migration is computed from HEAD, exactly as route-guard.sh does it.
t "sed -i applied mig"  deny "$(bdecw "sed -i '' s/a/b/ apps/api/migrations/20260101000000_m01_applied.sql")"
t "cp over applied mig" deny "$(bdecw 'cp /tmp/x apps/api/migrations/20260101000000_m01_applied.sql # tweak')"
t "redirect applied"    deny "$(bdecw 'echo x > apps/api/migrations/20260101000000_m01_applied.sql')"
t "new migration ok"    pass "$(bdecw 'cat > apps/api/migrations/20260201000000_m02_new.sql <<EOF
CREATE TABLE t();
EOF')"
t "reading a migration" pass "$(bdecw 'cat apps/api/migrations/20260101000000_m01_applied.sql')"

echo "== bash-guard: the migration arm never goes quiet because it cannot resolve a root"
# The hole: the arm resolved its repository from `.cwd` ALONE, and when that was
# absent from the payload or was not inside a worktree it left `root` empty, ran
# nothing, printed nothing and exited 0. The command was allowed. Any payload
# carrying no cwd was a silent route around the strongest deny in this guard, so
# every `bdec` assertion above - all of which send no cwd - proved nothing about
# this arm. The arm now resolves a second way, and refuses if it cannot.

# 1. It resolves without a cwd, from this script's own checkout. Asserted
#    against a migration that really is in THIS repository's HEAD, picked at run
#    time: hard-coding a filename here would rot the first time one is renamed,
#    and a rotted name would make this assertion pass for the wrong reason.
real_root=$(git -C "$here" rev-parse --show-toplevel 2>/dev/null || echo "")
real_mig=""
# No `head -1` on the end of this pipeline. head closes the pipe as soon as it
# has its line, grep then dies on a write error, and CI logged
# "grep: write error: Broken pipe" from exactly here. It was harmless - the
# value is still the first match - but a stderr line nobody can explain is how
# a real one gets skipped over. Take the whole list and cut the first line in
# bash, where nothing closes anything early.
if [[ -n "$real_root" ]]; then
  real_migs=$(git -C "$real_root" ls-tree -r --name-only HEAD apps/api/migrations 2>/dev/null | grep -E '\.sql$')
  real_mig=${real_migs%%$'\n'*}
fi
if [[ -n "$real_mig" ]]; then
  t "no cwd, applied mig denied"  deny "$(bdec "sed -i '' s/a/b/ $real_mig")"
  t "no cwd, redirect denied"     deny "$(bdec "echo x > $real_mig")"
else
  echo "  NOTE: no migration found in HEAD, so the no-cwd resolution is unproven here"
  failed=$((failed+1))
fi
# ...and the over-fire it must not commit. A migration filename that is NOT in
# HEAD is new work, and new work is exactly what this arm has to let through.
t "no cwd, new mig passes" pass "$(bdec 'cat > apps/api/migrations/29991231235959_not_in_head.sql <<EOF
CREATE TABLE t();
EOF')"
# A cwd that is a real directory but not a repository must fall back, not fail
# open: same answer as above.
nonrepo=$(jq -n --arg c "$tmp" --arg c2 'echo x > apps/api/migrations/29991231235959_not_in_head.sql' \
            '{cwd:$c, tool_input:{command:$c2}}' | bash "$BASH_G" 2>/dev/null \
          | jq -r '.hookSpecificOutput.permissionDecision // empty' 2>/dev/null)
t "non-repo cwd, new mig ok" pass "${nonrepo:-pass}"

# 2. When NEITHER source resolves, it refuses with a stated reason. Reproduced
#    honestly by running a copy of the guard from outside any worktree with no
#    cwd in the payload, which is the only way both sources can really fail.
nogit="$tmp/nogit"
mkdir -p "$nogit"
cp "$BASH_G" "$nogit/bash-guard.sh"
# The premise of the four assertions below is that $nogit is outside any
# worktree. If TMPDIR ever sits inside one, the guard resolves a root, the case
# under test never arises, and the assertions would pass or fail for a reason
# that has nothing to do with the code. Say so and go red rather than report a
# proof that did not happen.
if git -C "$nogit" rev-parse --show-toplevel >/dev/null 2>&1; then
  echo "FAIL  unresolvable-root setup: $nogit is inside a git worktree, so this case cannot be reached"
  failed=$((failed+1))
fi
orphan() { # orphan <command> -> the raw hook JSON, if any
  jq -n --arg c2 "$1" '{tool_input:{command:$c2}}' | bash "$nogit/bash-guard.sh" 2>/dev/null
}
# No output at all means the guard said nothing and the command is allowed;
# that is 'pass', and it must be spelled the same way `bdec` spells it or an
# allowed command reads as an empty string and every comparison misfires.
orphan_dec() {
  local out
  out=$(orphan "$1" | jq -r '.hookSpecificOutput.permissionDecision // empty' 2>/dev/null)
  printf '%s' "${out:-pass}"
}
applied_mig='echo x > apps/api/migrations/20260101000000_m01_applied.sql'
t "unresolvable root denies" deny "$(orphan_dec "$applied_mig")"
tcontains "and says why" "cannot resolve" \
  "$(orphan "$applied_mig" | jq -r '.hookSpecificOutput.permissionDecisionReason // ""' 2>/dev/null)"
# The refusal is scoped to the arm that cannot answer. An unresolvable root is
# not a licence to refuse every command in the repository.
t "unresolvable root, unrelated cmd" pass "$(orphan_dec 'go test ./...')"
t "unresolvable root, reading a mig" pass \
  "$(orphan_dec 'cat apps/api/migrations/20260101000000_m01_applied.sql')"
# The other arms still answer from outside a repository: they never needed one.
t "unresolvable root, sqlc still denied" deny \
  "$(orphan_dec 'echo x > apps/api/internal/db/sqlc/db.go')"

echo "== bash-guard: every arm reads its own command, not the whole string"
# The in-place-editor, interpreter and rm arms each ran THREE uncorrelated greps
# over the entire command - is `sed` present, is `-i` present, is the path
# present - and denied on the conjunction. Nothing tied the three to one
# command, so a read was refused whenever some LATER command in the same line
# happened to supply the missing word. `tee` had the mirror-image defect: it was
# read positionally and only its FIRST operand was checked, so a second
# destination was a live bypass of the generated-tree deny.

# tee writes to EVERY operand. Each of these reached the generated tree before.
t "tee 2nd operand"       deny "$(bdec 'echo x | tee /tmp/a apps/api/internal/db/sqlc/db.go')"
t "tee -a 2nd operand"    deny "$(bdec 'echo x | tee -a /tmp/a apps/api/internal/db/sqlc/db.go')"
t "tee 3rd operand"       deny "$(bdec 'echo x | tee /tmp/a /tmp/b apps/web/src/routeTree.gen.ts')"
t "tee into dead app"     deny "$(bdec 'echo x | tee /tmp/a apps/landing/index.html')"

# ...and the reads that the uncorrelated greps refused. Every one of these was
# denied before, and a guard that reddens correct work gets switched off.
t "sed read, grep -i"     pass "$(bdec 'sed -n 1,5p apps/api/internal/db/sqlc/db.go | grep -i querier')"
t "rm elsewhere, then ls" pass "$(bdec 'rm /tmp/scratch.go && ls -la apps/api/internal/db/sqlc/')"
t "sed -i /tmp, then cat" pass "$(bdec "sed -i '' s/a/b/ /tmp/x.go; cat apps/api/internal/db/sqlc/db.go")"
t "python /tmp, then wc"  pass "$(bdec 'python3 -c "open('"'"'/tmp/o'"'"','"'"'w'"'"').write('"'"'x'"'"')" && wc -l apps/api/internal/db/sqlc/db.go')"
t "grep -i, no editor"    pass "$(bdec 'grep -i querier apps/api/internal/db/sqlc/db.go')"

# A cp inside a comment is never executed, so it must not be read as a write.
# This is a behaviour change from the last-token version and is asserted, not
# assumed.
t "cp inside a comment"   pass "$(bdec 'echo hi # cp /tmp/x apps/api/internal/db/sqlc/db.go')"

# The same arms must still fire when the flag really does belong to the command
# that names the path.
t "sed -i on gen"         deny "$(bdec "sed -i '' s/a/b/ apps/api/internal/db/sqlc/db.go")"
t "perl -pi bundle"       deny "$(bdec 'perl -pi -e s/a/b/ apps/api/internal/api/gen/oas.go')"
t "node writeFileSync"    deny "$(bdec 'node -e "require('"'"'fs'"'"').writeFileSync('"'"'apps/web/src/routeTree.gen.ts'"'"','"'"'x'"'"')"')"
# `git rm` deletes a generated file just as `rm` does; the old arm matched the
# bare word `rm` inside it by accident, and per-segment parsing must keep it.
t "git rm a gen file"     deny "$(bdec 'git rm apps/api/internal/db/sqlc/db.go')"
t "git diff a gen tree"   pass "$(bdec 'git diff apps/api/internal/db/sqlc/')"
# Deleting the dead app stays permitted; only writing into it is refused.
t "rm the dead app still" pass "$(bdec 'rm -rf apps/landing')"

echo "== commit-gate: only ever answers for what this agent wrote"
# The deadlock this fixes: a read-only agent was launched into another agent's
# worktree, told to commit or delete 17 files it had never touched while its
# brief said not to touch them, and stopped to ask a human.
GATE="$here/commit-gate.sh"
WRITES="$here/agent-writes.sh"
export WPMGR_AGENT_WRITES_STATE="$tmp/agent-writes"

# A worktree at the path shape the gate gates on.
wt="$tmp/repo/.claude/worktrees/agent-zz"
mkdir -p "$wt"
git -C "$wt" init -q
git -C "$wt" config user.email t@t.invalid
git -C "$wt" config user.name t
echo seed > "$wt/seed.txt"
git -C "$wt" add seed.txt >/dev/null; git -C "$wt" commit -qm init
echo mine   > "$wt/mine.txt"      # written by agent-A
echo theirs > "$wt/theirs.txt"    # written by nobody in this test

record() { # record <agent_id> <abs path>
  jq -n --arg a "$1" --arg p "$2" '{agent_id:$a, tool_input:{file_path:$p}}' \
    | bash "$WRITES" 2>/dev/null
}
gate() { # gate <agent_id> -> exit code
  jq -n --arg c "$wt" --arg a "$1" \
    '{cwd:$c} + (if $a == "" then {} else {agent_id:$a} end)' \
    | bash "$GATE" >/dev/null 2>&1
  printf '%s' "$?"
}
gate_msg() {
  jq -n --arg c "$wt" --arg a "$1" '{cwd:$c, agent_id:$a}' \
    | bash "$GATE" 2>&1 >/dev/null
}

record agent-A "$wt/mine.txt"

# Agent A wrote an uncommitted file: it is blocked, and told about its own file.
t "A is blocked"           2 "$(gate agent-A)"
tcontains "A sees its file"   "mine.txt"   "$(gate_msg agent-A)"
tlacks    "A is not shown theirs" "theirs.txt" "$(gate_msg agent-A)"
# The exact deadlock: agent B wrote nothing, so it must not be blocked at all,
# however dirty the tree is.
t "B wrote nothing, passes" 0 "$(gate agent-B)"
# ...and it is never told to delete anything.
tlacks "never demands deletion" "delete" "$(gate_msg agent-A)"

# Once A commits its own file, the gate goes quiet even though the tree is still
# dirty with someone else's work.
git -C "$wt" add mine.txt >/dev/null; git -C "$wt" commit -qm mine
t "A committed, passes"     0 "$(gate agent-A)"
t "tree still dirty"        1 "$(git -C "$wt" status --porcelain | grep -c theirs.txt)"

# A file inside a directory that did not exist. Default `git status --porcelain`
# COLLAPSES an untracked directory to a single entry - `?? newpkg/` - and never
# names the file inside it, so the ownership compare matched nothing and the
# gate stayed silent about the one thing a builder agent mostly produces: new
# files in new packages. Pinned first as the porcelain fact itself, so this
# reads as a defect in git's default output rather than as an unexplained
# assertion, and so a future change to the flag is caught here and not in
# production.
mkdir -p "$wt/newpkg/deeper"
echo new > "$wt/newpkg/deeper/added.go"
t "porcelain collapses the dir" "?? newpkg/" \
  "$(git -C "$wt" status --porcelain | grep newpkg)"
t "-uall names the file"        "?? newpkg/deeper/added.go" \
  "$(git -C "$wt" status --porcelain -uall | grep added.go)"

record agent-C "$wt/newpkg/deeper/added.go"
t "C: new dir still blocks"  2 "$(gate agent-C)"
tcontains "C sees its nested file" "newpkg/deeper/added.go" "$(gate_msg agent-C)"
tcontains "C's count is right"     "1 uncommitted path"     "$(gate_msg agent-C)"

# ...and the over-fire -uall could have caused, since it turns one line into
# many: the extra lines are still filtered through the ownership record, so an
# untracked tree nobody recorded is neither counted nor shown.
mkdir -p "$wt/otherpkg/deeper"
echo other > "$wt/otherpkg/deeper/notmine.go"
tlacks "C is not shown another's new dir" "otherpkg" "$(gate_msg agent-C)"
t "C's count is still 1"        2 "$(gate agent-C)"
tcontains "still one path"      "1 uncommitted path"  "$(gate_msg agent-C)"
# The original deadlock, re-checked with the tree now full of untracked
# directories: an agent that wrote nothing is still never blocked.
t "B still passes, -uall or not" 0 "$(gate agent-B)"

# No agent_id means no ownership record, and the fallback reports rather than
# blocks. A gate that deadlocks an agent is worse than one that reminds it.
t "unscoped does not block" 0 "$(gate "")"
tcontains "unscoped reports" "Not blocking" "$(jq -n --arg c "$wt" '{cwd:$c}' | bash "$GATE" 2>&1 >/dev/null)"

# The second pass never blocks, so a gate can never wedge an agent.
t "stop_hook_active passes" 0 \
  "$(jq -n --arg c "$wt" '{cwd:$c, agent_id:"agent-A", stop_hook_active:true}' \
     | bash "$GATE" >/dev/null 2>&1; printf '%s' "$?")"

echo "== harness-reap: it removes only worktrees the harness created"
# The reaper fed `git worktree list` straight into `git worktree remove --force`
# with no ownership filter, so any clean worktree on a merged commit was removed
# - including one a maintainer made by hand for their own work, and including
# the main checkout when the reaper was run from inside a worktree. Ownership is
# structural: created by the harness means directly under .claude/worktrees/ of
# THIS checkout, named agent-* or wf_*. Everything else is somebody else's.
REAP="$here/harness-reap.sh"
rr="$tmp/reaprepo"
mkdir -p "$rr"
git -C "$rr" init -q
git -C "$rr" config user.email t@t.invalid
git -C "$rr" config user.name t
echo seed > "$rr/seed.txt"
git -C "$rr" add seed.txt >/dev/null
git -C "$rr" commit -qm init
git -C "$rr" branch -M main

mkdir -p "$rr/.claude/worktrees"
git -C "$rr" worktree add -q --detach "$rr/.claude/worktrees/agent-zz1" main 2>/dev/null
git -C "$rr" worktree add -q --detach "$rr/.claude/worktrees/wf_zz2-abc" main 2>/dev/null
# Named by a person, in the harness's own directory: still not the harness's.
git -C "$rr" worktree add -q --detach "$rr/.claude/worktrees/mine-by-hand" main 2>/dev/null
# A maintainer's own worktree, clean and on a merged commit, outside that tree.
git -C "$rr" worktree add -q --detach "$tmp/manual-wt" main 2>/dev/null

reap_out=$(cd "$rr" && bash "$REAP" --worktrees-only 2>&1)
reap_rm=$(printf '%s\n' "$reap_out" | grep 'worktree remove' || true)
tcontains "agent-* is removable"      "agent-zz1"     "$reap_rm"
tcontains "wf_* is removable"         "wf_zz2-abc"    "$reap_rm"
tlacks "a hand-made worktree is not"  "manual-wt"     "$reap_rm"
tlacks "an off-shape name is not"     "mine-by-hand"  "$reap_rm"
tlacks "the main checkout is not"     "force '$rr'"   "$reap_rm"
# Held back is not silence: an unowned worktree is reported, so the maintainer
# can see the disk it is holding and decide for themselves.
tcontains "unowned is reported"       "manual-wt"     "$reap_out"

# ...and the same again for real, because a dry run proves only what the script
# says it would do. This is a throwaway repo under mktemp.
(cd "$rr" && bash "$REAP" --apply --worktrees-only >/dev/null 2>&1)
t "agent-* really removed"    no  "$(exists "$rr/.claude/worktrees/agent-zz1")"
t "wf_* really removed"       no  "$(exists "$rr/.claude/worktrees/wf_zz2-abc")"
t "hand-made one survives"    yes "$(exists "$tmp/manual-wt")"
t "off-shape one survives"    yes "$(exists "$rr/.claude/worktrees/mine-by-hand")"
t "main checkout survives"    yes "$(exists "$rr/seed.txt")"

# Run from INSIDE a worktree, `git rev-parse --show-toplevel` is that worktree,
# so the main checkout was neither the skipped 'root' nor filtered out. It must
# never be listed as removable.
git -C "$rr" worktree add -q --detach "$rr/.claude/worktrees/agent-zz3" main 2>/dev/null
from_wt=$(cd "$rr/.claude/worktrees/agent-zz3" && bash "$REAP" --worktrees-only 2>&1 \
          | grep 'worktree remove' || true)
tlacks "main checkout, seen from a worktree" "force '$rr'" "$from_wt"
tlacks "the sibling by hand, from a worktree" "mine-by-hand" "$from_wt"

echo "== harness-reap: a refused action is reported, never counted as reclaimed"
# Each destructive step used to be `<command> 2>/dev/null` followed by an
# unconditional counter increment. Against a `git worktree lock`ed worktree git
# says "fatal: cannot remove a locked working tree", the script threw that away
# and printed "1 removable", and the directory sat there still holding its
# disk. A reclaim report that lies is worse than none. `git worktree lock` is
# the one refusal that is reproducible without root, a full disk or Docker, so
# it is the case pinned here.
lk="$tmp/lockrepo"
mkdir -p "$lk"
git -C "$lk" init -q
git -C "$lk" config user.email t@t.invalid
git -C "$lk" config user.name t
echo seed > "$lk/seed.txt"
git -C "$lk" add seed.txt >/dev/null
git -C "$lk" commit -qm init
git -C "$lk" branch -M main
mkdir -p "$lk/.claude/worktrees"
git -C "$lk" worktree add -q --detach "$lk/.claude/worktrees/agent-locked" main 2>/dev/null
git -C "$lk" worktree add -q --detach "$lk/.claude/worktrees/agent-ok" main 2>/dev/null
git -C "$lk" worktree lock "$lk/.claude/worktrees/agent-locked"

# A dry run attempts nothing, so it cannot have failed at anything: it must not
# invent a failure it has not met, and it must stay green.
lock_dry=$(cd "$lk" && bash "$REAP" --worktrees-only 2>&1); lock_dry_rc=$?
tlacks "a dry run claims no failure" "FAILED" "$lock_dry"
t "a dry run stays green"      0 "$lock_dry_rc"

lock_out=$(cd "$lk" && bash "$REAP" --apply --worktrees-only 2>&1); lock_rc=$?
tcontains "the refusal is reported"   "FAILED"                "$lock_out"
tcontains "git's own words survive"   "locked working tree"   "$lock_out"
tcontains "the failure is counted"    "1 failed"              "$lock_out"
# The whole defect in one assertion: two were offered, one really went.
tcontains "only the real one counts"  "1 removed"             "$lock_out"
t "the refused one is still there" yes "$(exists "$lk/.claude/worktrees/agent-locked")"
t "and a refusal goes red"          1  "$lock_rc"
# ...and the honest direction, or a reaper that reports everything as failed
# would pass the assertions above while reclaiming nothing.
t "the healthy one really went"     no "$(exists "$lk/.claude/worktrees/agent-ok")"

# Same worktree, same script, only the real obstacle removed: it must now go,
# be counted, and go green. A guard that reddens correct work gets switched off.
git -C "$lk" worktree unlock "$lk/.claude/worktrees/agent-locked"
ok_out=$(cd "$lk" && bash "$REAP" --apply --worktrees-only 2>&1); ok_rc=$?
t "unlocked, it really goes"        no "$(exists "$lk/.claude/worktrees/agent-locked")"
t "and the run goes green"           0 "$ok_rc"
tcontains "a green run says so"     "0 failed" "$ok_out"

echo "== harness-reap: it refuses to act from anywhere but the root it resolved"
# SC2164. `cd "$root"` was unchecked, and everything after it - `git worktree
# remove --force`, `git branch -d`, `go clean -cache`, and the rev-parse that
# picks the base branch - resolves its repository from the CURRENT DIRECTORY,
# not from $root. A failed cd therefore did not stop the script; it re-aimed
# every destructive command at whatever directory the caller was standing in.
#
# The failure is planted the only way a `cd` to a directory that git just
# resolved can honestly be made to fail: an exported shell function shadows the
# builtin inside the script's own process. The subshell enters $cdr FIRST, so
# the reaper starts in the right place and fails only at its own cd.
cdr="$tmp/cdrepo"
mkdir -p "$cdr"
git -C "$cdr" init -q
git -C "$cdr" config user.email t@t.invalid
git -C "$cdr" config user.name t
echo seed > "$cdr/seed.txt"
git -C "$cdr" add seed.txt >/dev/null
git -C "$cdr" commit -qm init
git -C "$cdr" branch -M main
mkdir -p "$cdr/.claude/worktrees"
git -C "$cdr" worktree add -q --detach "$cdr/.claude/worktrees/agent-cd1" main 2>/dev/null

reap_broken_cd() { # reap_broken_cd <dir> ; runs --apply with every cd failing
  (
    cd "$1" || return 9
    cd() { return 1; }
    export -f cd
    bash "$REAP" --apply --worktrees-only 2>&1
  )
}
brk_out=$(reap_broken_cd "$cdr"); brk_rc=$?
# It must stop, say so, and above all not have removed anything.
t "a failed cd goes red"           2   "$brk_rc"
tcontains "and states the reason"  "cannot enter the repository root" "$brk_out"
t "and nothing was removed"        yes "$(exists "$cdr/.claude/worktrees/agent-cd1")"
tlacks "no reclaim is claimed"     "removed," "$brk_out"

# ...and the honest direction, or a reaper that refused unconditionally would
# pass every assertion above while reclaiming nothing. Same repo, same flags,
# only the sabotage removed.
ok_cd_out=$(cd "$cdr" && bash "$REAP" --apply --worktrees-only 2>&1); ok_cd_rc=$?
t "unsabotaged, it goes green"     0   "$ok_cd_rc"
t "and the worktree really went"   no  "$(exists "$cdr/.claude/worktrees/agent-cd1")"

echo "== guards_test: this suite obeys the rule it proves"
# Two fixture setups here staged everything instead of naming it. CLAUDE.md
# forbids blanket staging everywhere, and this file is the proof surface for the
# harness that teaches it, so the rule is asserted about this file rather than
# left to review.
#
# The pattern tolerates a `git -C <dir>` prefix - the form both offenders took,
# and the form a plain search for the phrase alone does not see. Its two
# boundaries differ on purpose, and each was wrong once when this was written:
# the flags end at any non-flag character, so a trailing `;` is still caught
# while `-A-not-a-flag` is not; the bare dot ends only at something that cannot
# continue a path, so a lone dot is caught while a relative path such as
# `./one/file.go` - which is naming a file - is not.
#
# Comment lines are stripped first. This paragraph would otherwise match itself,
# and, worse, a comment restating the rule is a second home for a fact that has
# one: scripts/claude/fact-census.sh counts exactly that kind of drift, and this
# block is deliberately written so it does not add to that count.
#
# `git add` only. `commit -a` is also forbidden and appears nowhere in this
# suite; folding it in would have to catch `-am` too, and a pattern that half
# covers a rule is worse than one that states its scope.
ga='git( +-C +[^ ]+)? +add +'
blanket=$ga'(-A|--all)([^A-Za-z0-9_-]|$)|'$ga'\.([^A-Za-z0-9_/.-]|$)'
t "no blanket staging in this suite" 0 \
  "$(grep -vE '^[[:space:]]*#' "$0" | grep -cE "$blanket" | tr -d ' ')"

echo "== agent-writes: a record it cannot write is announced, never swallowed"
# Both writes in the recorder used to discard their failure - `mkdir -p ...
# 2>/dev/null || exit 0`, and an append whose stderr went to /dev/null followed
# by an unconditional exit 0. This file is the sole input to commit-gate.sh, so
# a silent failure there set scoped=0 and DOWNGRADED the blocking gate to a
# reminder, with nothing said anywhere. It must announce and still exit 0: a
# PostToolUse hook that exits non-zero fails the user's tool call.
#
# Both failures are planted physically rather than with chmod, which root
# ignores and which would make this suite pass or fail on the uid it runs as.

# 1. The state directory cannot be created: its parent is a regular file.
blocker="$tmp/not-a-dir"
echo x > "$blocker"
aw_mk=$(jq -n --arg a agent-D --arg p "$wt/theirs.txt" '{agent_id:$a, tool_input:{file_path:$p}}' \
        | WPMGR_AGENT_WRITES_STATE="$blocker/state" bash "$WRITES" 2>&1 >/dev/null)
aw_mk_rc=$(jq -n --arg a agent-D --arg p "$wt/theirs.txt" '{agent_id:$a, tool_input:{file_path:$p}}' \
        | WPMGR_AGENT_WRITES_STATE="$blocker/state" bash "$WRITES" >/dev/null 2>&1; printf '%s' "$?")
t "broken state dir still exits 0"   0 "$aw_mk_rc"
tcontains "and names what failed"    "cannot create the state directory" "$aw_mk"
tcontains "and names the consequence" "NOT being written"                "$aw_mk"
tcontains "and says the gate stops blocking" "never block"               "$aw_mk"

# 2. The record file itself cannot be appended to: the path is a directory.
apst="$tmp/agent-writes-append"
mkdir -p "$apst/agent-E"
aw_ap=$(jq -n --arg a agent-E --arg p "$wt/theirs.txt" '{agent_id:$a, tool_input:{file_path:$p}}' \
        | WPMGR_AGENT_WRITES_STATE="$apst" bash "$WRITES" 2>&1 >/dev/null)
aw_ap_rc=$(jq -n --arg a agent-E --arg p "$wt/theirs.txt" '{agent_id:$a, tool_input:{file_path:$p}}' \
        | WPMGR_AGENT_WRITES_STATE="$apst" bash "$WRITES" >/dev/null 2>&1; printf '%s' "$?")
t "unwritable record still exits 0"  0 "$aw_ap_rc"
tcontains "and names that failure"   "cannot append to the record" "$aw_ap"

# ...and the over-fire, which for a hook that runs on EVERY write is the whole
# risk: a working recorder must be completely silent, or the note becomes noise
# and stops being read.
aw_ok=$(jq -n --arg a agent-F --arg p "$wt/theirs.txt" '{agent_id:$a, tool_input:{file_path:$p}}' \
        | bash "$WRITES" 2>&1 >/dev/null)
t "a working recorder says nothing"  "" "$aw_ok"

# The consequence, end to end, because the note above is a claim about
# commit-gate.sh and a claim is worth what its proof is. agent-F's write really
# was recorded, so the gate blocks; agent-G's never was, so the same gate can
# only remind - which is exactly the downgrade the recorder now announces.
t "record present, gate blocks"      2 "$(gate agent-F)"
tcontains "and names the file"       "theirs.txt" "$(gate_msg agent-F)"
t "record absent, gate only reminds" 0 "$(gate agent-G)"
tcontains "and admits it is not blocking" "Not blocking" "$(gate_msg agent-G)"

echo "== session-brief: a measurement it could not take never prints as zero"
# `git ... | wc -l` prints 0 when GIT failed, which is indistinguishable from
# "there are none", and the caller then dropped the whole worktree section
# rather than admit it could not look. Same for docker, and the disk warning -
# the one that would have caught failure B - was skipped outright when df
# printed something non-numeric.
#
# The failures are planted with shims on PATH, because that is the only way to
# make an installed tool fail on demand. The git shim passes everything else
# through to the real git: the brief exits at its own `git rev-parse` if git is
# merely missing, so "git cannot answer" and "git is absent" are different
# things and this reproduces the first.
BRIEF="$here/session-brief.sh"
realgit=$(command -v git)

# --- a PATH this fixture BUILDS, never one it inherits -----------------------
# Every session-brief.sh run below goes through $sysbin. The first version of
# this fixture prepended its shims to $PATH, so what each case resolved to
# depended on what the developer happened to have installed: on a machine whose
# shell profile puts the Go bin directory on PATH - which is the configuration
# the docs ask for, so sqlc/atlas/govulncheck run directly - nine assertions
# reddened, while CI, which has none of those binaries, stayed green. Green in
# CI and red for the developer who followed the instructions is the shape this
# project names specifically, and a suite that reddens correct work gets
# switched off.
#
# A symlink farm rather than a list of system directories: the directory holding
# jq or git may also hold go - both are /opt/homebrew/bin on this machine - so
# no subset of real directories is guaranteed to exclude the toolchain. Linking
# exactly the binaries session-brief.sh needs gives a PATH that can contain
# nothing else. `bash` is in the list because the shims' `#!/usr/bin/env bash`
# resolves it through PATH.
sysbin="$tmp/sysbin"
mkdir -p "$sysbin"
for b in bash sh git jq df awk du cut grep; do
  bp=$(command -v "$b" 2>/dev/null)
  if [[ "$bp" != /* || ! -x "$bp" ]]; then
    echo "FAIL  hermetic PATH setup: '$b' did not resolve to an executable absolute path (got '${bp:-nothing}')"
    failed=$((failed+1)); continue
  fi
  ln -sf "$bp" "$sysbin/$b"
done
# Not a restatement of the loop above: this is what stops a future edit adding
# `go`, or a whole system directory, to the farm without anyone noticing. It
# keys on the PATH just built, never on the ambient one.
# `timeout` is in this list for a reason that cost a CI run: it does not exist
# on macOS but IS real coreutils on ubuntu-latest, so with the ambient PATH the
# brief reported "- on PATH: timeout govulncheck" there and "- on PATH:
# govulncheck" here, and an assertion on the exact line passed locally and
# failed in CI. Any tool the brief looks up must be absent from the farm, so
# that whether it is "on PATH" is decided here and not by the operating system.
for tool in go sqlc atlas govulncheck docker timeout; do
  if (PATH="$sysbin"; command -v "$tool" >/dev/null 2>&1); then
    echo "FAIL  hermetic PATH setup: '$tool' is visible under the built PATH, so the fixture is not hermetic"
    failed=$((failed+1))
  fi
done

sb="$tmp/briefrepo"
mkdir -p "$sb"
git -C "$sb" init -q
git -C "$sb" config user.email t@t.invalid
git -C "$sb" config user.name t
echo seed > "$sb/seed.txt"
git -C "$sb" add seed.txt >/dev/null
git -C "$sb" commit -qm init

shim_git="$tmp/shim-git"; mkdir -p "$shim_git"
{
  echo '#!/usr/bin/env bash'
  echo 'for a in "$@"; do'
  echo '  case "$a" in worktree|branch) echo "fatal: simulated git failure" >&2; exit 128 ;; esac'
  echo 'done'
  printf 'exec %s "$@"\n' "$realgit"
} > "$shim_git/git"
chmod +x "$shim_git/git"

# An honest zero and a failed measurement must not read the same. This is the
# assertion the whole change exists for.
sb_zero=$(cd "$sb" && PATH="$sysbin" bash "$BRIEF" 2>/dev/null)
sb_fail=$(cd "$sb" && PATH="$shim_git:$sysbin" bash "$BRIEF" 2>/dev/null)
tlacks    "honest zero claims no worktrees"  "agent worktrees" "$sb_zero"
tcontains "a failed git says so"             "agent worktrees: could not measure" "$sb_fail"
tlacks    "and never reports it as zero"     "0 live"          "$sb_fail"
# The shim must really be the thing that changed the answer, not a repo that
# happened to differ: same repo, same script, one PATH entry apart.
t "the two runs really differ"  differ \
  "$(if [[ "$sb_zero" == "$sb_fail" ]]; then printf same; else printf differ; fi)"
# A brief that cannot measure still must not break the session start.
sb_fail_rc=$(cd "$sb" && PATH="$shim_git:$sysbin" bash "$BRIEF" >/dev/null 2>&1; printf '%s' "$?")
t "a failed measurement still exits 0" 0 "$sb_fail_rc"

# docker: installed but not answering is precisely what a full disk does to it,
# and reporting that as "0 volumes" hides the symptom when it is most wanted.
shim_dfail="$tmp/shim-docker-fail"; mkdir -p "$shim_dfail"
printf '%s\n' '#!/usr/bin/env bash' 'echo "Cannot connect to the Docker daemon" >&2' 'exit 1' > "$shim_dfail/docker"
chmod +x "$shim_dfail/docker"
sb_dfail=$(cd "$sb" && PATH="$shim_dfail:$sysbin" bash "$BRIEF" 2>/dev/null)
tcontains "a silent docker says so" "docker volumes: could not measure" "$sb_dfail"
# ...and the honest direction, or a brief that reported everything as
# unmeasurable would pass the assertion above while measuring nothing.
shim_dok="$tmp/shim-docker-ok"; mkdir -p "$shim_dok"
# A builtin loop, not `seq`: under the built PATH there is no seq, and a shim
# that cannot run would report zero volumes and quietly pass the wrong test.
printf '%s\n' '#!/usr/bin/env bash' 'for ((i=1;i<=12;i++)); do echo "vol$i"; done' > "$shim_dok/docker"
chmod +x "$shim_dok/docker"
sb_dok=$(cd "$sb" && PATH="$shim_dok:$sysbin" bash "$BRIEF" 2>/dev/null)
tcontains "a working docker is counted"   "12 docker volumes" "$sb_dok"
tlacks    "and is not called unmeasurable" "docker volumes: could not measure" "$sb_dok"

# df: the under-25Gi warning is the one that would have caught failure B, so a
# df that gives no number must say the warning did not run, not stay quiet.
shim_df="$tmp/shim-df"; mkdir -p "$shim_df"
printf '%s\n' '#!/usr/bin/env bash' 'echo "df: /: Operation not permitted" >&2' 'exit 1' > "$shim_df/df"
chmod +x "$shim_df/df"
sb_df=$(cd "$sb" && PATH="$shim_df:$sysbin" bash "$BRIEF" 2>/dev/null)
tcontains "a broken df says so"        "disk free: could not measure" "$sb_df"
tcontains "and says the warning did not run" "did NOT run" "$sb_df"
tlacks    "and invents no free space"  "disk free: Gi"  "$sb_df"
# The honest direction: a real df gives a number and no apology.
tcontains "a real df gives a number"   "disk free:"     "$sb_zero"
tlacks    "and no could-not-measure"   "disk free: could not measure" "$sb_zero"

echo "== session-brief: 'could not check' is not the same claim as 'NOT INSTALLED'"
# `[[ -x "$(go env GOPATH 2>/dev/null)/bin/$t" ]]` LOOKS like a lookup, but with
# go unresolvable the substitution is empty, the test degrades to `-x /bin/$t`,
# and the script asserted NOT INSTALLED for three tools that were installed and
# runnable at /Users/mosamgor/go/bin. Four states, and they must stay four:
# on PATH, in the Go bin directory, genuinely absent, and unknowable. The last
# two were collapsed into the loudest one.
#
# Every run below uses the built $sysbin PATH, which is already asserted to see
# none of go, sqlc, atlas or govulncheck. The states are therefore produced by
# this fixture and not by what the developer has installed - the whole point of
# the rewrite. The two guards that used to sit here keyed on the AMBIENT PATH
# and reddened on a machine that simply had the toolchain installed; their real
# job now belongs to the hermeticity check next to the farm, where it can catch
# a mistake in the farm rather than a fact about the developer's shell.
sbfx="$tmp/gofix"
mkdir -p "$sbfx/bin"
printf '%s\n' '#!/bin/sh' 'exit 0' > "$sbfx/bin/sqlc"   # present in the Go bin dir
chmod +x "$sbfx/bin/sqlc"
# atlas is deliberately NOT created there: genuinely absent, with a working go.

# A `go` that answers only `go env`, so the lookup is exercised without needing
# a real toolchain. govulncheck sits in the same directory, which puts it ON
# PATH - all three states in one run.
mkgo() { # mkgo <dir> <GOBIN answer> <GOPATH answer>
  mkdir -p "$1"
  {
    echo '#!/usr/bin/env bash'
    echo 'if [[ "${1:-}" == env ]]; then'
    echo '  case "${2:-}" in'
    printf '    GOBIN)  echo "%s" ;;\n' "$2"
    printf '    GOPATH) echo "%s" ;;\n' "$3"
    echo '    *) echo "" ;;'
    echo '  esac'
    echo 'fi'
    echo 'exit 0'
  } > "$1/go"
  chmod +x "$1/go"
}
shim_go="$tmp/shim-go"
mkgo "$shim_go" "" "$sbfx"
printf '%s\n' '#!/bin/sh' 'exit 0' > "$shim_go/govulncheck"
chmod +x "$shim_go/govulncheck"

tc_ok=$(cd "$sb" && PATH="$shim_go:$sysbin" bash "$BRIEF" 2>/dev/null)
tcontains "on PATH is said so"           "on PATH: govulncheck"                    "$tc_ok"
tcontains "in the Go bin dir, located"   "sqlc is at $sbfx/bin/sqlc, not on PATH"  "$tc_ok"
tcontains "genuinely absent stays loud"  "atlas is NOT INSTALLED"                  "$tc_ok"
tcontains "and says where it looked"     "looked in $sbfx/bin"                     "$tc_ok"
# The over-fire: a tool that WAS found must never also be called absent, or the
# loud message stops meaning anything.
tlacks "a located tool is not called absent"  "sqlc is NOT INSTALLED"        "$tc_ok"
tlacks "a tool on PATH is not called absent"  "govulncheck is NOT INSTALLED" "$tc_ok"
tlacks "and nothing claims it could not check" "could not check"             "$tc_ok"

# GOBIN overrides GOPATH/bin for `go install`, so a machine that sets it keeps
# its binaries where GOPATH/bin never mentions. atlas exists ONLY in the GOBIN
# directory here: if GOBIN were ignored it would come back NOT INSTALLED.
gbdir="$tmp/gobin-dir"
mkdir -p "$gbdir"
printf '%s\n' '#!/bin/sh' 'exit 0' > "$gbdir/atlas"
chmod +x "$gbdir/atlas"
shim_gobin="$tmp/shim-gobin"
mkgo "$shim_gobin" "$gbdir" "$sbfx"
tc_gb=$(cd "$sb" && PATH="$shim_gobin:$sysbin" bash "$BRIEF" 2>/dev/null)
tcontains "GOBIN beats GOPATH/bin" "atlas is at $gbdir/atlas, not on PATH" "$tc_gb"
tlacks "and GOBIN's tool is not called absent" "atlas is NOT INSTALLED"    "$tc_gb"

# The state the old code could not express at all: go unresolvable, so the
# lookup never happened and absence is not a claim this script may make. The
# built PATH simply has no go in it, which is why this needs no PATH surgery -
# the earlier version stripped the directory holding go out of the ambient
# PATH, which worked only because it was starting from a PATH it did not build.
tc_nogo=$(cd "$sb" && PATH="$sysbin" bash "$BRIEF" 2>/dev/null)
tcontains "no go: it says it could not check" "sqlc: could not check whether it is installed" "$tc_nogo"
tcontains "and names go as the reason"        "'go' is not on PATH"                           "$tc_nogo"
# The whole finding in one assertion: with no way to look, it must not assert
# absence. Note the timeout line says "not installed" in lower case and is a
# different, still-correct claim, so this needle is deliberately upper case.
tlacks "and never asserts NOT INSTALLED"      "NOT INSTALLED"                                 "$tc_nogo"

echo ""
if [[ $failed -eq 0 ]]; then
  echo "guards_test: $pass assertions passed"
  exit 0
fi
echo "guards_test: $failed FAILED, $pass passed"
exit 1
