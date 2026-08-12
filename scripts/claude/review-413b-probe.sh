#!/usr/bin/env bash
# Scratch probe for the second PR #413 review (862fda2..2c35310).
# Attacks the quote-aware splitter and the new BOUND/LEADB boundaries.
# Reads only; it never writes to any path it names.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
G="$here/bash-guard.sh"
repo=$(git -C "$here" rev-parse --show-toplevel)

d() { jq -n --arg c "$1" --arg w "$repo" '{cwd:$w, tool_input:{command:$c}}' \
      | bash "$G" 2>/dev/null | jq -r '.hookSpecificOutput.permissionDecision // "PASS"'; }
probe() { printf '%-8s  %s\n' "$(d "$1")" "$1"; }

echo "== the six shapes the fix targets (all must deny)"
probe 'cp -r /tmp/newsqlc apps/api/internal/db/sqlc'
probe 'mv /tmp/models.go apps/api/internal/db/sqlc'
probe 'cp -t apps/api/internal/db/sqlc /tmp/models.go'
probe 'cp -r /tmp/newlanding apps/landing'
probe 'cp /tmp/20260531050000_m19_orgs_sharing.sql apps/api/migrations'
probe 'echo x >| apps/api/internal/db/sqlc/db.go'

echo
echo "== quote-aware splitter: an apostrophe that bash escapes, awk does not"
probe "echo don\\'t; cp /tmp/x apps/api/internal/db/sqlc/db.go"
probe "echo don\\'t && cp /tmp/x apps/api/internal/db/sqlc/db.go"
probe "echo don\\'t && cp /tmp/x apps/landing/index.html"
probe "echo don\\'t && cp /tmp/x apps/api/migrations/20260531050000_m19_orgs_sharing.sql"
probe "echo it\\'s fine; echo x > apps/api/internal/db/sqlc/db.go"
echo "  (control: the same chain with a balanced quote)"
probe "echo \"don't\"; cp /tmp/x apps/api/internal/db/sqlc/db.go"
probe "echo dont; cp /tmp/x apps/api/internal/db/sqlc/db.go"

echo
echo "== other splitter shapes"
probe "sed -i 's| #x|y|' apps/api/internal/db/sqlc/db.go"
probe "echo \$'\\n'; cp /tmp/x apps/api/internal/db/sqlc/db.go"
probe "awk '{print \$1}' /tmp/f; cp /tmp/x apps/api/internal/db/sqlc/db.go"
probe "grep \"it's\" /tmp/f; cp /tmp/x apps/api/internal/db/sqlc/db.go"

echo
echo "== over-fire hunt: honest work that must NOT be blocked"
probe 'cp /tmp/x apps/api/internal/db/sqlcx/db.go'
probe 'cp /tmp/x apps/landing-old/index.html'
probe 'cp /tmp/x apps/api/migrations-notes.txt'
probe 'cp /tmp/x.sql apps/api/migrations/20260813000000_m40_new.sql'
probe 'cp /tmp/20260813000000_m40_new.sql apps/api/migrations'
probe 'cp apps/api/migrations/20260531050000_m19_orgs_sharing.sql /tmp/x.sql'
probe 'cp apps/api/internal/db/sqlc/db.go /tmp/'
probe 'mv apps/landing /tmp/landing-old'
probe 'rm -rf apps/landing'
probe 'sed -n 1,5p apps/api/internal/db/sqlc/db.go | grep -i querier'
probe 'git diff apps/api/internal/api/gen'
probe 'ls apps/api/internal/db/sqlc'
probe 'tar czf /tmp/mig.tgz apps/api/migrations'
probe 'cp -r apps/api/migrations /tmp/migrations-backup'
probe 'cp -r apps/api/internal/db/sqlc /tmp/sqlc-backup'
probe 'rsync -a apps/api/migrations/ /tmp/mig/'
probe 'mkdir -p /tmp/x && cp docs/a.md /tmp/x'
probe 'go test ./... 2>&1 | tee /tmp/out.txt'
probe 'cp /tmp/a.go apps/api/internal/site/service.go'
