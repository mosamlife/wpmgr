#!/usr/bin/env bash
# Scratch proof for the PR #413 review: the permissions and validation the two
# state directories get. Sandboxed under one mktemp -d, removed on exit.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT

echo "umask in this shell: $(umask)"
echo

echo "== agent-writes.sh creates its state directory"
jq -n '{agent_id:"a1", tool_input:{file_path:"/x/y.go"}}' \
  | WPMGR_AGENT_WRITES_STATE="$t/aw" bash "$here/agent-writes.sh"
ls -ld "$t/aw"
ls -la "$t/aw" | sed 's/^/  /'

echo
echo "== route-guard.sh creates its state directory"
jq -n --arg c "$repo" --arg p "$repo/apps/api/internal/site/x.go" \
  '{session_id:"s1", cwd:$c, tool_input:{file_path:$p}}' \
  | WPMGR_ROUTE_GUARD_STATE="$t/rg" bash "$here/route-guard.sh" >/dev/null
ls -ld "$t/rg"
ls -la "$t/rg" | sed 's/^/  /'

echo
echo "== a pre-existing, group/world-writable state dir: does either script refuse it?"
mkdir -p "$t/loose-aw" "$t/loose-rg"
chmod 777 "$t/loose-aw" "$t/loose-rg"
jq -n '{agent_id:"a2", tool_input:{file_path:"/x/y.go"}}' \
  | WPMGR_AGENT_WRITES_STATE="$t/loose-aw" bash "$here/agent-writes.sh"
echo "agent-writes exit: $?  contents:"; ls -la "$t/loose-aw" | sed 's/^/  /'
jq -n --arg c "$repo" --arg p "$repo/apps/api/internal/site/z.go" \
  '{session_id:"s2", cwd:$c, tool_input:{file_path:$p}}' \
  | WPMGR_ROUTE_GUARD_STATE="$t/loose-rg" bash "$here/route-guard.sh" >/dev/null
echo "route-guard left loose-rg as:"; ls -lad "$t/loose-rg"; ls -la "$t/loose-rg" | sed 's/^/  /'

echo
echo "== destination-key collisions after sane()'s tail -c 64"
for a in database-engineer backend-architect wp-agent-engineer frontend-architect devops-engineer docs-writer; do
  for s in 0 1; do
    if [ "$s" = 1 ]; then k="$a, then security-reviewer (mandatory, model opus)|$s"; else k="$a|$s"; fi
    printf '%s' "$k" | tr -c 'a-zA-Z0-9._-' '-' | tail -c 64
    printf '\n'
  done
done | sort > "$t/keys"
echo "keys generated: $(grep -c '' "$t/keys")   distinct: $(sort -u "$t/keys" | grep -c '')"
cat "$t/keys" | sed 's/^/  /'
