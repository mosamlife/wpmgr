#!/usr/bin/env bash
# Lints .claude/agents/*.md against the harness's own rules, and lints the
# harness against itself.
#
# The previous version had one check, aimed at the docs-writer incident (an
# agent granted no Bash and then asked to branch, commit and build). It could
# never have caught it: the committed docs-writer body is 91 words with no
# command shapes in it at all - the instruction to branch and commit came from
# the delegating brief at call time. Scanning the body for shell verbs is
# guessing. Scanning the frontmatter and the routing table is deciding.
#
# What it checks now, all decidable from files in the repo:
#   1. required frontmatter keys, and a writing agent has isolation: worktree
#   2. Write without Edit (and vice versa) - the tech-stack-researcher defect
#   3. every agent named in CLAUDE.md's routing table exists, and every agent
#      that exists is named there - the db-migration-engineer defect
#   4. no agent body names a dead app or a concrete feature branch - the
#      apps/landing and feat/performance-suite defects
#   5. no generating-model meta-narration leaked into a body
#
#   scripts/claude/agent-lint.sh              lint
#   scripts/claude/agent-lint.sh --self-test  prove each check fires and does
#                                             not over-fire
set -uo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || echo .)
status=0
fail() { echo "FAIL $*"; status=1; }

frontmatter() { awk '/^---$/{c++; next} c==1{print} c>=2{exit}' "$1"; }
body()        { awk '/^---$/{c++; next} c>=2{print}' "$1"; }

lint_file() {
  local f="$1" rc=0 fm bd tools
  fm=$(frontmatter "$f"); bd=$(body "$f")

  grep -qE '^name:'        <<<"$fm" || { echo "FAIL $f: no 'name:'"; rc=1; }
  grep -qE '^description:' <<<"$fm" || { echo "FAIL $f: no 'description:'"; rc=1; }
  grep -qE '^model:'       <<<"$fm" || { echo "FAIL $f: no 'model:'"; rc=1; }
  grep -qE '^maxTurns:'    <<<"$fm" || { echo "FAIL $f: no 'maxTurns:' ceiling"; rc=1; }

  # A colon in the name is reserved for plugin scoping and the agent silently
  # does not load.
  grep -qE '^name:[^:]*:' <<<"$fm" && { echo "FAIL $f: 'name:' contains a colon; the agent will not load"; rc=1; }

  tools=$(grep -E '^tools:' <<<"$fm" || true)
  if [[ -n "$tools" ]]; then
    # Write without Edit forces whole-file overwrites and fails outright on a
    # file the agent has not read. Edit without Write cannot create one.
    if grep -qE '\bWrite\b' <<<"$tools" && ! grep -qE '\bEdit\b' <<<"$tools"; then
      echo "FAIL $f: 'tools:' grants Write without Edit -> $tools"; rc=1
    fi
    if grep -qE '\bEdit\b' <<<"$tools" && ! grep -qE '\bWrite\b' <<<"$tools"; then
      echo "FAIL $f: 'tools:' grants Edit without Write -> $tools"; rc=1
    fi
    if ! grep -qE '\bBash\b' <<<"$tools"; then
      echo "FAIL $f: 'tools:' omits Bash. Every agent here must be able to run its own gates; omit the tools line entirely to inherit."; rc=1
    fi
  fi

  # No tools line means it can write, so it needs its own checkout.
  if [[ -z "$tools" ]] || grep -qE '^tools:.*\b(Write|Edit)\b' <<<"$fm"; then
    grep -qE '^isolation:[[:space:]]*worktree' <<<"$fm" \
      || { echo "FAIL $f: can write files but has no 'isolation: worktree'"; rc=1; }
  fi

  # Dead apps and hard-coded branches: both shipped and both stayed for months.
  # Only a COMMAND against the dead app is a failure. Saying "apps/landing is
  # dead, do not deploy it" is the correct content and must not be flagged -
  # a guard that reddens correct work gets switched off.
  grep -qE '(pnpm|npm|make|rsync|gcloud|cd|-C)[[:space:]]+[^|]*apps/landing|apps/landing/dist' <<<"$bd" \
    && { echo "FAIL $f: runs a command against apps/landing, which is dead and absent from pnpm-workspace.yaml"; rc=1; }
  grep -qE '(feat|fix|chore|debt|ci|release)/[a-z0-9][a-z0-9._-]+' <<<"$bd" \
    && { echo "FAIL $f: names a concrete branch. Describe the shape; never pin a branch."; rc=1; }
  grep -qE 'fleet-agent-for-wpmgr' <<<"$bd" && { echo "FAIL $f: uses the old wp.org slug; it is fleet-agent-site-manager"; rc=1; }

  # Generating-model narration. Two committed agents opened with it.
  grep -qiE '^(I |Here is the complete|Writing it now|Let me produce|Based on all the)' <<<"$bd" \
    && { echo "FAIL $f: body opens with generating-model narration, not instructions"; rc=1; }

  return $rc
}

cross_check() {
  local claude="$root/CLAUDE.md" rc=0
  [[ -f "$claude" ]] || { echo "FAIL CLAUDE.md does not exist"; return 1; }

  local -a defined=() referenced=()
  local f n
  shopt -s nullglob
  for f in "$root"/.claude/agents/*.md; do
    n=$(frontmatter "$f" | sed -n 's/^name:[[:space:]]*//p' | head -1)
    [[ -n "$n" ]] && defined+=("$n")
  done

  # Anything in a backtick-quoted `foo-bar` cell of the routing table that looks
  # like an agent name.
  while IFS= read -r n; do referenced+=("$n"); done < <(
    grep -oE '`[a-z][a-z0-9]*(-[a-z0-9]+)+`' "$claude" \
      | tr -d '`' \
      | grep -E -- '-(architect|engineer|reviewer|writer|researcher)$' \
      | sort -u
  )

  for n in "${referenced[@]}"; do
    printf '%s\n' "${defined[@]}" | grep -qx "$n" \
      || { echo "FAIL CLAUDE.md routes to '$n', which has no file in .claude/agents/"; rc=1; }
  done
  for n in "${defined[@]}"; do
    printf '%s\n' "${referenced[@]}" | grep -qx "$n" \
      || { echo "FAIL agent '$n' exists but is named nowhere in CLAUDE.md; nothing will ever route to it"; rc=1; }
  done

  # Same check for the guard, so the guard and the table cannot drift apart.
  local guard="$root/scripts/claude/route-guard.sh"
  if [[ -f "$guard" ]]; then
    while IFS= read -r n; do
      printf '%s\n' "${defined[@]}" | grep -qx "$n" \
        || { echo "FAIL route-guard.sh routes to '$n', which has no file in .claude/agents/"; rc=1; }
    done < <(sed -n 's/.*agent="\([a-z-]*\).*/\1/p' "$guard" | sort -u | grep -E -- '-(architect|engineer|reviewer|writer)$')
  fi
  return $rc
}

if [[ "${1:-}" == "--self-test" ]]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  ok=0

  mk() { printf -- '%s' "$2" > "$tmp/$1.md"; }

  good='---
name: good-engineer
description: d
model: sonnet
isolation: worktree
maxTurns: 10
---

You build things. Run go test and commit by name.
'
  mk good "$good"
  lint_file "$tmp/good.md" >/dev/null || { echo "SELF-TEST FAILED: over-fired on the known-good fixture"; exit 1; }

  # Each of these must fire, and must fire for the stated reason.
  check() { # check <name> <content> <expected substring>
    mk "$1" "$2"
    out=$(lint_file "$tmp/$1.md"); rc=$?
    if [[ $rc -eq 0 ]] || ! grep -q "$3" <<<"$out"; then
      echo "SELF-TEST FAILED: '$1' did not fire on '$3' (got: ${out:-<silence>})"; exit 1
    fi
    ok=$((ok+1))
  }

  check writeonly '---
name: a-engineer
description: d
tools: Read, Write, Grep, Bash
model: sonnet
isolation: worktree
maxTurns: 5
---
body
' "Write without Edit"

  check nobash '---
name: b-writer
description: d
tools: Read, Write, Edit, Grep
model: sonnet
isolation: worktree
maxTurns: 5
---
body
' "omits Bash"

  check noiso '---
name: c-engineer
description: d
model: sonnet
maxTurns: 5
---
body
' "isolation: worktree"

  check deadapp '---
name: d-engineer
description: d
model: sonnet
isolation: worktree
maxTurns: 5
---
Run pnpm -C apps/landing build then rsync apps/landing/dist to the bucket.
' "command against apps/landing"

  # ...and the negated mention, which is correct content, must stay silent.
  mk deadapp_ok '---
name: i-engineer
description: d
model: sonnet
isolation: worktree
maxTurns: 5
---
apps/landing is dead: absent from pnpm-workspace.yaml. Do not edit or deploy it.
'
  lint_file "$tmp/deadapp_ok.md" >/dev/null \
    || { echo "SELF-TEST FAILED: over-fired on a correct warning about apps/landing"; exit 1; }
  ok=$((ok+1))

  check branch '---
name: e-engineer
description: d
model: sonnet
isolation: worktree
maxTurns: 5
---
Commit on feat/performance-suite then tag.
' "names a concrete branch"

  check slug '---
name: f-engineer
description: d
model: sonnet
isolation: worktree
maxTurns: 5
---
Run wp plugin check fleet-agent-for-wpmgr.
' "old wp.org slug"

  check narration '---
name: g-engineer
description: d
model: sonnet
isolation: worktree
maxTurns: 5
---
I now have comprehensive grounding. Writing it now.
' "generating-model narration"

  check noturns '---
name: h-engineer
description: d
model: sonnet
isolation: worktree
---
body
' "maxTurns"

  echo "self-test ok: $ok checks fire on their fixture, none over-fires on the good one"
  exit 0
fi

shopt -s nullglob
for f in "$root"/.claude/agents/*.md; do
  lint_file "$f" || status=1
done
cross_check || status=1
[[ $status -eq 0 ]] && echo "agent-lint: all agent definitions ok"
exit $status
