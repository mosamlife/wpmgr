#!/usr/bin/env bash
# Scratch probe for the PR #413 review. Feeds bash-guard.sh real hook JSON and
# prints the decision it returns for each constructed command. Reads only; it
# never writes to any of the paths it names.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
G="$here/bash-guard.sh"
repo=$(git -C "$here" rev-parse --show-toplevel)

d() { # d <command>
  jq -n --arg c "$1" --arg w "$repo" '{cwd:$w, tool_input:{command:$c}}' \
    | bash "$G" 2>/dev/null \
    | jq -r '.hookSpecificOutput.permissionDecision // "PASS"'
}

probe() { printf '%-8s  %s\n' "$(d "$1")" "$1"; }

echo "== generated tree: trailing slash present vs absent"
probe 'cp /tmp/models.go apps/api/internal/db/sqlc/models.go'
probe 'cp -r /tmp/newsqlc apps/api/internal/db/sqlc'
probe 'cp /tmp/models.go apps/api/internal/db/sqlc/'
probe 'mv /tmp/models.go apps/api/internal/db/sqlc'
probe 'cp -t apps/api/internal/db/sqlc /tmp/models.go'
probe 'rsync -a /tmp/newsqlc/ apps/api/internal/db/sqlc'
probe 'install -m 644 /tmp/models.go apps/api/internal/db/sqlc'
probe 'rm -rf apps/api/internal/db/sqlc'
probe 'rm -rf apps/api/internal/db/sqlc/'
probe 'cp -r /tmp/gen apps/api/internal/api/gen'
probe 'cp -r /tmp/gen packages/openapi-client/src/generated'

echo
echo "== dead app: trailing slash present vs absent"
probe 'cp /tmp/x.tsx apps/landing/index.tsx'
probe 'cp -r /tmp/newlanding apps/landing'
probe 'mv /tmp/x.tsx apps/landing'

echo
echo "== applied migration: trailing slash present vs absent"
probe 'cp /tmp/x.sql apps/api/migrations/20260531050000_m19_orgs_sharing.sql'
probe 'cp /tmp/20260531050000_m19_orgs_sharing.sql apps/api/migrations'

echo
echo "== redirection variants"
probe 'echo x > apps/api/internal/db/sqlc/db.go'
probe 'echo x >| apps/api/internal/db/sqlc/db.go'
probe 'echo x >  apps/web/src/routeTree.gen.ts'

echo
echo "== comment strip swallowing a quoted hash"
probe "sed -i 's| #x|y|' apps/api/internal/db/sqlc/db.go"
probe "sed -i 's/x/y/' apps/api/internal/db/sqlc/db.go"

echo
echo "== honest reads that must NOT be blocked"
probe 'sed -n 1,5p apps/api/internal/db/sqlc/db.go | grep -i querier'
probe 'grep -r Querier apps/api/internal/db/sqlc/ > /tmp/out'
probe 'ls apps/api/internal/db/sqlc'
probe 'git diff apps/api/internal/api/gen'
