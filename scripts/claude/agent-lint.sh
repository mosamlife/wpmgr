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
#   6. that it linted anything at all. An earlier version resolved the root to
#      "." when git failed, globbed nothing under nullglob, ran the loop zero
#      times and printed "all agent definitions ok". A lint that finds no input
#      is a failed lint, never a pass.
#
#   scripts/claude/agent-lint.sh              lint
#   scripts/claude/agent-lint.sh --self-test  prove each check fires and does
#                                             not over-fire
#
# AGENT_LINT_ROOT overrides the repository root. --self-test uses it to drive
# the tree-level checks against fixture trees; nothing else should set it.
set -uo pipefail
shopt -s nullglob

status=0
fail() { echo "FAIL $*"; status=1; }

# No silent fallback. Every path below is built from this value, so a root that
# is wrong or unknown makes every glob expand to nothing - which is exactly the
# shape that used to pass.
resolve_root() {
  local r="${AGENT_LINT_ROOT:-}"
  if [[ -z "$r" ]]; then
    r=$(git rev-parse --show-toplevel 2>/dev/null) || r=
  fi
  if [[ -z "$r" ]]; then
    echo "FAIL agent-lint: cannot resolve a repository root: 'git rev-parse --show-toplevel' failed in '$PWD' and AGENT_LINT_ROOT is unset. Refusing to lint an unknown tree." >&2
    return 1
  fi
  if [[ ! -d "$r" ]]; then
    echo "FAIL agent-lint: root '$r' is not a directory. Refusing to lint an unknown tree." >&2
    return 1
  fi
  printf '%s\n' "$r"
}

# The agent names CLAUDE.md's routing table backticks. Used twice: to cross-check
# names, and to derive a lower bound on how many definitions must exist. Derived,
# never a hard-coded count.
guard_names() { # <path to route-guard.sh>
  sed -n 's/.*agent="\([a-z-]*\).*/\1/p' "$1" \
    | sort -u \
    | grep -E -- '-(architect|engineer|reviewer|writer)$'
}

routed_names() { # <path to CLAUDE.md>
  grep -oE '`[a-z][a-z0-9]*(-[a-z0-9]+)+`' "$1" \
    | tr -d '`' \
    | grep -E -- '-(architect|engineer|reviewer|writer|researcher)$' \
    | sort -u
}

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

  # Generating-model narration. Two committed agents opened with it. Anchored on
  # the first non-empty line, because that is what the message claims: applied
  # to every line it reddens a correct body that tells the agent to print a
  # sentence starting "I ", and a check that reddens correct work gets removed.
  local opening
  opening=$(grep -m1 -vE '^[[:space:]]*$' <<<"$bd")
  grep -qiE '^(I |Here is the complete|Writing it now|Let me produce|Based on all the)' <<<"$opening" \
    && { echo "FAIL $f: body opens with generating-model narration, not instructions"; rc=1; }

  return $rc
}

cross_check() { # <root> <agent file>...
  local root="$1"; shift
  local claude="$root/CLAUDE.md" rc=0
  [[ -f "$claude" ]] || { echo "FAIL CLAUDE.md does not exist at '$claude'"; return 1; }

  local -a defined=() referenced=()
  local f n
  # The files come from lint_tree, which has already refused an empty set. This
  # function never globs, so it cannot silently examine nothing.
  for f in "$@"; do
    n=$(frontmatter "$f" | sed -n 's/^name:[[:space:]]*//p' | head -1)
    [[ -n "$n" ]] && defined+=("$n")
  done

  # Anything in a backtick-quoted `foo-bar` cell of the routing table that looks
  # like an agent name.
  while IFS= read -r n; do referenced+=("$n"); done < <(routed_names "$claude")

  # bash 3.2 (every macOS) treats "${empty[@]}" under set -u as an unbound
  # variable and kills the shell mid-lint; bash 5 (ubuntu-latest, where CI runs)
  # expands it to nothing. The +"" form behaves the same on both.
  for n in ${referenced[@]+"${referenced[@]}"}; do
    printf '%s\n' ${defined[@]+"${defined[@]}"} | grep -qx "$n" \
      || { echo "FAIL CLAUDE.md routes to '$n', which has no file in .claude/agents/"; rc=1; }
  done
  for n in ${defined[@]+"${defined[@]}"}; do
    printf '%s\n' ${referenced[@]+"${referenced[@]}"} | grep -qx "$n" \
      || { echo "FAIL agent '$n' exists but is named nowhere in CLAUDE.md; nothing will ever route to it"; rc=1; }
  done

  # Same check for the guard, so the guard and the table cannot drift apart.
  # Both ways of comparing nothing are errors. The guard is part of the harness,
  # so it is not optional; and an extraction that yields no names means the
  # spelling guard_names depends on has changed, not that the guard routes
  # nowhere. Either way this check was passing while comparing nothing, which is
  # the same defect as linting zero files.
  local guard="$root/scripts/claude/route-guard.sh"
  if [[ ! -f "$guard" ]]; then
    echo "FAIL agent-lint: no route guard at '$guard'. It is part of the harness, so its destinations cannot be compared against .claude/agents/ and this check would otherwise pass having compared nothing."
    rc=1
    return $rc
  fi

  local -a routed=()
  while IFS= read -r n; do routed+=("$n"); done < <(guard_names "$guard")
  if (( ${#routed[@]} == 0 )); then
    echo "FAIL agent-lint: extracted no agent names from '$guard'. It is expected to name each destination as agent=\"<name>\"; that spelling has changed, so this check was comparing nothing."
    rc=1
  fi
  for n in ${routed[@]+"${routed[@]}"}; do
    printf '%s\n' ${defined[@]+"${defined[@]}"} | grep -qx "$n" \
      || { echo "FAIL route-guard.sh routes to '$n', which has no file in .claude/agents/"; rc=1; }
  done
  return $rc
}

# The whole tree, and the only place the agent files are collected. Everything
# it can fail on - unreadable root, missing directory, empty directory, a set
# smaller than the routing table - must exit non-zero and must never reach the
# success line.
lint_tree() { # <root>
  local root="$1" rc=0 dir f
  dir="$root/.claude/agents"

  if [[ ! -d "$dir" ]]; then
    echo "FAIL agent-lint: nothing to lint: expected a directory of agent definitions at '$dir' (root '$root'), which does not exist."
    return 1
  fi

  local -a files=()
  for f in "$dir"/*.md; do files+=("$f"); done
  if (( ${#files[@]} == 0 )); then
    echo "FAIL agent-lint: nothing to lint: '$dir' exists but contains no *.md agent definitions."
    return 1
  fi

  # Lower bound, derived from CLAUDE.md's routing table at read time. Never a
  # literal: the number of agents changes, and a hard-coded one goes stale.
  local claude="$root/CLAUDE.md" want=0
  if [[ -f "$claude" ]]; then
    want=$(routed_names "$claude" | wc -l | tr -d '[:space:]')
    [[ -n "$want" ]] || want=0
  fi
  if (( ${#files[@]} < want )); then
    echo "FAIL agent-lint: found ${#files[@]} agent definition(s) in '$dir', but CLAUDE.md's routing table names $want; the set is incomplete."
    rc=1
  fi

  for f in "${files[@]}"; do
    lint_file "$f" || rc=1
  done
  cross_check "$root" "${files[@]}" || rc=1

  if (( rc == 0 )); then
    echo "agent-lint: ${#files[@]} agent definitions ok in $dir"
  fi
  return $rc
}

if [[ "${1:-}" == "--self-test" ]]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  ok=0     # checks proven to fire on a fixture that deserves it
  quiet=0  # correct inputs proven not to be flagged

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
  quiet=$((quiet+1))

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
  quiet=$((quiet+1))

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

  # ...and a body that legitimately tells the agent to print a line starting
  # "I ", further down. The message says "opens with", so this must stay silent.
  mk narration_ok '---
name: j-engineer
description: d
model: sonnet
isolation: worktree
maxTurns: 5
---
Run the gate. If the binary is missing, print exactly:
I cannot find govulncheck on PATH.
and exit 1.
'
  lint_file "$tmp/narration_ok.md" >/dev/null \
    || { echo "SELF-TEST FAILED: over-fired on an instruction to print a line starting \"I \""; exit 1; }
  quiet=$((quiet+1))

  check noturns '---
name: h-engineer
description: d
model: sonnet
isolation: worktree
---
body
' "maxTurns"

  # --- the tree walk itself -------------------------------------------------
  # "It linted zero files" is a property of the walk, not of any one file, so
  # these drive lint_tree and resolve_root. Every one must exit non-zero and
  # none may print the success line.
  check_tree() { # check_tree <name> <root> <expected substring>
    local out rc
    out=$(lint_tree "$2" 2>&1); rc=$?
    if [[ $rc -eq 0 ]]; then
      echo "SELF-TEST FAILED: '$1' exited 0 over a tree it could not lint (got: ${out:-<silence>})"; exit 1
    fi
    if ! grep -q "$3" <<<"$out"; then
      echo "SELF-TEST FAILED: '$1' did not fire on '$3' (got: ${out:-<silence>})"; exit 1
    fi
    if grep -q 'definitions ok' <<<"$out"; then
      echo "SELF-TEST FAILED: '$1' printed the success line while failing (got: $out)"; exit 1
    fi
    ok=$((ok+1))
  }

  check_root() { # check_root <name> <expected substring> <root override, or - for none>
    local out rc
    if [[ "$3" == "-" ]]; then
      out=$( (unset AGENT_LINT_ROOT; export GIT_DIR="$tmp/no-such-git-dir"; cd "$tmp"; resolve_root) 2>&1 ); rc=$?
    else
      out=$( (export AGENT_LINT_ROOT="$3"; resolve_root) 2>&1 ); rc=$?
    fi
    if [[ $rc -eq 0 ]]; then
      echo "SELF-TEST FAILED: '$1' resolved a root it should have refused (got: ${out:-<silence>})"; exit 1
    fi
    if ! grep -q "$2" <<<"$out"; then
      echo "SELF-TEST FAILED: '$1' did not fire on '$2' (got: ${out:-<silence>})"; exit 1
    fi
    ok=$((ok+1))
  }

  # These two fixtures carry a CLAUDE.md with no agent names in it on purpose:
  # the empty-set check has to stand on its own, not be propped up by
  # cross_check happening to notice the routing table has no files behind it.
  mkdir -p "$tmp/t-missing"
  printf -- '%s' 'A CLAUDE.md with no routing table.' > "$tmp/t-missing/CLAUDE.md"
  check_tree missing_dir "$tmp/t-missing" 'nothing to lint: expected a directory'

  mkdir -p "$tmp/t-empty/.claude/agents"
  printf -- '%s' 'A CLAUDE.md with no routing table.' > "$tmp/t-empty/CLAUDE.md"
  printf -- '%s' 'not an agent' > "$tmp/t-empty/.claude/agents/README.txt"
  check_tree empty_dir "$tmp/t-empty" 'exists but contains no'

  # A whole fixture tree: agents, a routing table and a route guard that names
  # its destinations the way the real one does.
  mktree() { # mktree <name> <CLAUDE.md text> <route-guard.sh text, or - for none>
    local d="$tmp/$1"
    mkdir -p "$d/.claude/agents" "$d/scripts/claude"
    printf -- '%s' "$2" > "$d/CLAUDE.md"
    printf -- '%s' "$good" > "$d/.claude/agents/good.md"
    [[ "$3" == "-" ]] || printf -- '%s' "$3" > "$d/scripts/claude/route-guard.sh"
  }
  guard_ok='case "$p" in
  apps/api/*) agent="good-engineer" ;;
esac
'

  # A set smaller than the routing table. The bound is read from the fixture's
  # own CLAUDE.md, so nothing here is a hard-coded number.
  mktree t-short 'Routing: `good-engineer`, `other-engineer`, `third-writer`.' "$guard_ok"
  check_tree short_set "$tmp/t-short" 'the set is incomplete'

  # The guard file itself is not optional: absent, there is nothing to compare
  # the routing table against, and the check used to skip and pass.
  mktree t-noguard 'Routing: `good-engineer` builds things.' -
  check_tree guard_missing "$tmp/t-noguard" 'no route guard at'

  # Present, but no longer spelled agent="...". The extraction yields nothing and
  # the loop used to run zero times, green.
  mktree t-guardspelling 'Routing: `good-engineer` builds things.' 'case "$p" in
  apps/api/*) route_to good-engineer ;;
esac
'
  check_tree guard_extracts_nothing "$tmp/t-guardspelling" 'extracted no agent names'

  # Both of those fixtures are correct in every other respect, so each must go
  # red for exactly one reason. An assertion that passes because something else
  # failed proves nothing about the check it is named after.
  for n in t-noguard t-guardspelling; do
    out=$(lint_tree "$tmp/$n" 2>&1)
    if [[ "$(grep -c '^FAIL' <<<"$out")" -ne 1 ]]; then
      echo "SELF-TEST FAILED: '$n' went red for more than the guard check (got: $out)"; exit 1
    fi
  done

  check_root unresolvable_root 'is not a directory' "$tmp/no-such-root"
  check_root no_git_root 'cannot resolve a repository root' -

  # ...and the honest tree it must not block: one routed agent, one file, one
  # guard that names it.
  mktree t-ok 'Routing: `good-engineer` builds things.' "$guard_ok"
  out=$(lint_tree "$tmp/t-ok" 2>&1); rc=$?
  if [[ $rc -ne 0 ]] || ! grep -q "^agent-lint: 1 agent definitions ok" <<<"$out"; then
    echo "SELF-TEST FAILED: over-fired on a complete tree (rc=$rc, got: ${out:-<silence>})"; exit 1
  fi
  quiet=$((quiet+1))

  # A root that does resolve must be returned as-is and nothing printed to stderr.
  out=$( (export AGENT_LINT_ROOT="$tmp/t-ok"; resolve_root) 2>&1 ); rc=$?
  if [[ $rc -ne 0 ]] || [[ "$out" != "$tmp/t-ok" ]]; then
    echo "SELF-TEST FAILED: resolve_root refused a real directory (rc=$rc, got: ${out:-<silence>})"; exit 1
  fi
  quiet=$((quiet+1))

  echo "self-test ok: $ok checks fire on their fixture, $quiet correct inputs stay silent"
  exit 0
fi

root=$(resolve_root) || exit 2
lint_tree "$root" || status=1
exit $status
