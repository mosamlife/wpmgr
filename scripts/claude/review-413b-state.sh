#!/usr/bin/env bash
# Scratch proof for the second PR #413 review: agent-writes.sh state validation
# and commit-gate.sh's planted-record case. Sandboxed under one mktemp -d.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
W="$here/agent-writes.sh"; C="$here/commit-gate.sh"
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT

echo "== 1. the original finding: state pointed at a stand-in home"
home="$t/stand-in-home"; mkdir -p "$home"
for f in .zsh_history .gitconfig .netrc notes.md; do
  printf 'irreplaceable\n' > "$home/$f"; touch -t 202601010000 "$home/$f"
done
jq -n '{agent_id:"a1", tool_input:{file_path:"/x/y.go"}}' \
  | WPMGR_AGENT_WRITES_STATE="$home" bash "$W"
echo "  survivors: $(ls -a1 "$home" | grep -v '^\.\{1,2\}$' | tr '\n' ' ')"

echo
echo "== 2. symlinked state path"
mkdir -p "$t/victim"; printf 'x\n' > "$t/victim/keepme"; touch -t 202601010000 "$t/victim/keepme"
ln -s "$t/victim" "$t/link"
jq -n '{agent_id:"a2", tool_input:{file_path:"/x/y.go"}}' \
  | WPMGR_AGENT_WRITES_STATE="$t/link" bash "$W"
echo "  victim now: $(ls -a1 "$t/victim" | grep -v '^\.\{1,2\}$' | tr '\n' ' ')"

echo
echo "== 3. relative state path (would land inside the repo)"
jq -n '{agent_id:"a3", tool_input:{file_path:"/x/y.go"}}' \
  | WPMGR_AGENT_WRITES_STATE="scratch-state" bash "$W"
echo "  created in cwd? $(ls -d "$here/scratch-state" 2>&1)"

echo
echo "== 4. an unmarked, world-writable pre-existing directory"
mkdir -p "$t/loose"; chmod 777 "$t/loose"
jq -n '{agent_id:"a4", tool_input:{file_path:"/x/y.go"}}' \
  | WPMGR_AGENT_WRITES_STATE="$t/loose" bash "$W"
echo "  contents: $(ls -a1 "$t/loose" | grep -v '^\.\{1,2\}$' | tr '\n' ' ')(none expected)"

echo
echo "== 5. commit-gate: a planted EMPTY record in an unmarked directory"
wt="$t/repo/.claude/worktrees/agent-x"; mkdir -p "$wt"
git -C "$wt" init -q; git -C "$wt" config user.email t@t.invalid; git -C "$wt" config user.name t
printf 'x\n' > "$wt/scratch.go"
plant="$t/planted"; mkdir -p "$plant"; : > "$plant/agent-x"
echo "--- stderr from commit-gate, planted record, NO marker:"
jq -n --arg c "$wt" '{cwd:$c, agent_id:"agent-x"}' \
  | WPMGR_AGENT_WRITES_STATE="$plant" bash "$C" 2>&1 >/dev/null | sed 's/^/    /'
echo "--- exit: $(jq -n --arg c "$wt" '{cwd:$c, agent_id:"agent-x"}' | WPMGR_AGENT_WRITES_STATE="$plant" bash "$C" >/dev/null 2>&1; printf '%s' "$?")"

echo
echo "== 6. commit-gate: a record file that is a SYMLINK inside a trusted dir"
good="$t/good"; mkdir -p "$good"; : > "$good/.wpmgr-harness-state"; chmod 700 "$good"
printf '%s\n' "$wt/scratch.go" > "$t/attacker-list"
ln -s "$t/attacker-list" "$good/agent-x"
echo "--- stderr:"
jq -n --arg c "$wt" '{cwd:$c, agent_id:"agent-x"}' \
  | WPMGR_AGENT_WRITES_STATE="$good" bash "$C" 2>&1 >/dev/null | sed 's/^/    /'
echo "--- exit: $(jq -n --arg c "$wt" '{cwd:$c, agent_id:"agent-x"}' | WPMGR_AGENT_WRITES_STATE="$good" bash "$C" >/dev/null 2>&1; printf '%s' "$?")"
