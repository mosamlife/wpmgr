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
git -C "$repo" add -A >/dev/null
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
t "1st Go file, session A"  ask  "$(decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessA)"
t "2nd Go file, session A"  pass "$(decision "$repo/apps/api/internal/site/other.go"   "$repo" "" sessA)"
t "same file again"         pass "$(decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessA)"
# A different destination is a different ruling and must still be asked.
t "then a web file"         ask  "$(decision "$repo/apps/web/src/app.tsx"              "$repo" "" sessA)"
# ...including the escalation: an auth path is a different ruling from ordinary
# Go work by the same specialist, so it prompts even though backend-architect
# was already approved above.
t "then an auth file"       ask  "$(decision "$repo/apps/api/internal/auth/session.go" "$repo" "" sessA)"
t "auth file again"         pass "$(decision "$repo/apps/api/internal/auth/token.go"   "$repo" "" sessA)"
# A different session has made no ruling yet.
t "1st Go file, session B"  ask  "$(decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessB)"
# Remembering must never weaken a deny.
t "deny is not remembered"  deny "$(decision "$repo/apps/api/internal/db/sqlc/db.go"   "$repo" "" sessA)"
t "deny again"              deny "$(decision "$repo/apps/api/internal/db/sqlc/db.go"   "$repo" "" sessA)"
# TTL 0 disables the memory entirely: a long session comes back to the same area
# hours later and gets asked again.
t "ttl 0 re-asks" ask \
  "$(export WPMGR_ROUTE_GUARD_TTL_MIN=0; decision "$repo/apps/api/internal/site/service.go" "$repo" "" sessA)"
# No session identity in the payload means no memory, and the guard falls back
# to asking rather than to silence.
t "no session id asks"      ask  "$(decision "$repo/apps/api/internal/site/deux.go")"

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

# An applied migration is computed from HEAD, exactly as route-guard.sh does it.
t "sed -i applied mig"  deny "$(bdecw "sed -i '' s/a/b/ apps/api/migrations/20260101000000_m01_applied.sql")"
t "redirect applied"    deny "$(bdecw 'echo x > apps/api/migrations/20260101000000_m01_applied.sql')"
t "new migration ok"    pass "$(bdecw 'cat > apps/api/migrations/20260201000000_m02_new.sql <<EOF
CREATE TABLE t();
EOF')"
t "reading a migration" pass "$(bdecw 'cat apps/api/migrations/20260101000000_m01_applied.sql')"

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
git -C "$wt" add -A >/dev/null; git -C "$wt" commit -qm init
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

# No agent_id means no ownership record, and the fallback reports rather than
# blocks. A gate that deadlocks an agent is worse than one that reminds it.
t "unscoped does not block" 0 "$(gate "")"
tcontains "unscoped reports" "Not blocking" "$(jq -n --arg c "$wt" '{cwd:$c}' | bash "$GATE" 2>&1 >/dev/null)"

# The second pass never blocks, so a gate can never wedge an agent.
t "stop_hook_active passes" 0 \
  "$(jq -n --arg c "$wt" '{cwd:$c, agent_id:"agent-A", stop_hook_active:true}' \
     | bash "$GATE" >/dev/null 2>&1; printf '%s' "$?")"

echo ""
if [[ $failed -eq 0 ]]; then
  echo "guards_test: $pass assertions passed"
  exit 0
fi
echo "guards_test: $failed FAILED, $pass passed"
exit 1
