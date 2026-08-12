#!/usr/bin/env bash
# Scratch proof for the second PR #413 review: is the escaped-apostrophe hole a
# pre-existing gap or a regression introduced by the quote-aware splitter?
# Compares the guard at 862fda2 (tr-based split) with the one at 2c35310.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
git -C "$repo" show 862fda2:scripts/claude/bash-guard.sh > "$t/old.sh"

ask() { # ask <guard> <command>
  jq -n --arg c "$2" --arg w "$repo" '{cwd:$w, tool_input:{command:$c}}' \
    | bash "$1" 2>/dev/null | jq -r '.hookSpecificOutput.permissionDecision // "PASS"'
}

run() { printf '  old=%-6s new=%-6s  %s\n' "$(ask "$t/old.sh" "$1")" "$(ask "$here/bash-guard.sh" "$1")" "$1"; }

echo "escaped apostrophe ahead of the write:"
run "echo don\\'t; cp /tmp/x apps/api/internal/db/sqlc/db.go"
run "echo don\\'t && cp /tmp/x apps/landing/index.html"
run "echo don\\'t; rm -rf apps/api/internal/api/gen/oas.go"
echo
echo "the case the new splitter was written for:"
run "sed -i 's| #x|y|' apps/api/internal/db/sqlc/db.go"
echo
echo "bare unbalanced apostrophe, no backslash (bash would not even parse this):"
run "echo it' ; cp /tmp/x apps/api/internal/db/sqlc/db.go"
