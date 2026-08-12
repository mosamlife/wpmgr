#!/usr/bin/env bash
# Scratch proof for the PR #413 review.
#
# Shows that scripts/claude/agent-writes.sh reaches `find ... -delete` with a
# completely unvalidated, environment-controlled $state - the same shape as the
# route-guard prune that destroyed the owner's home directory this morning.
#
# It NEVER points at a real path. Everything happens inside one mktemp -d that
# stands in for a home directory, and the whole thing is removed on exit.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
W="$here/agent-writes.sh"

sandbox=$(mktemp -d)
trap 'rm -rf "$sandbox"' EXIT
fake_home="$sandbox/stand-in-home"
mkdir -p "$fake_home/.local/bin"

# Plain files at the top level of the stand-in home, aged past the -mtime +2
# threshold, exactly like a real ~/.zsh_history or ~/.gitconfig.
for f in .zsh_history .gitconfig .netrc notes.md; do
  printf 'irreplaceable\n' > "$fake_home/$f"
  touch -t 202601010000 "$fake_home/$f"
done
# One subdirectory, to show what -type f does and does not reach.
printf 'x\n' > "$fake_home/.local/bin/tool"
touch -t 202601010000 "$fake_home/.local/bin/tool"

echo "BEFORE (top-level entries of the stand-in home):"
ls -a1 "$fake_home" | grep -v '^\.\{1,2\}$' | sed 's/^/  /'

echo
echo "RUN: a normal PostToolUse payload, with WPMGR_AGENT_WRITES_STATE set to the stand-in home"
jq -n '{agent_id:"agent-1", tool_input:{file_path:"/some/file.go"}}' \
  | WPMGR_AGENT_WRITES_STATE="$fake_home" bash "$W"
echo "  exit: $?   (agent-writes.sh reports success)"

echo
echo "AFTER:"
ls -a1 "$fake_home" | grep -v '^\.\{1,2\}$' | sed 's/^/  /'
echo
echo "subdirectory survives (-type f -maxdepth 1 only):"
ls -a1 "$fake_home/.local/bin" | grep -v '^\.\{1,2\}$' | sed 's/^/  /'

echo
echo "== the same env value through route-guard.sh, for contrast"
mkdir -p "$sandbox/guard-home"
printf 'irreplaceable\n' > "$sandbox/guard-home/keepme"
touch -t 202601010000 "$sandbox/guard-home/keepme"
jq -n '{session_id:"s1", cwd:"'"$here"'", tool_input:{file_path:"'"$here"'/../../apps/api/internal/site/x.go"}}' \
  | WPMGR_ROUTE_GUARD_STATE="$sandbox/guard-home" bash "$here/route-guard.sh" >/dev/null
echo "route-guard stderr above; guard-home still holds:"
ls -a1 "$sandbox/guard-home" | grep -v '^\.\{1,2\}$' | sed 's/^/  /'
