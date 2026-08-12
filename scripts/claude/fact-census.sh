#!/usr/bin/env bash
# Counts how many harness files restate the same fact.
#
# The harness's central argument is that a duplicated rule drifts. The first
# version shipped twelve duplicated rules: the unbounded-wait remedy was stated
# in 13 files, the apps/api/tests exclusion in 9, the dead app in 9. Every one
# of those is a place a future edit can make one copy wrong while the others
# stay right, which is the exact failure the harness was written to stop.
#
# A fact should live in ONE place that is read at the moment it is needed: a
# guard's denial message, a path-scoped rule file that loads on the matching
# path, or the agent that owns that work. `home` below records where that is.
#
# REPORT ONLY, and deliberately not wired into ci.yml. The patterns are
# deliberately loose so they cannot miss a restatement, which means they also
# match legitimate single mentions; a gate on a loose pattern reddens correct
# work and gets switched off. Read the file list it prints and judge.
#
#   scripts/claude/fact-census.sh            counts per fact
#   scripts/claude/fact-census.sh --list     counts plus the matching files
set -uo pipefail

show_list=0
[[ "${1:-}" == "--list" ]] && show_list=1

root=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "not in a git repo" >&2; exit 1; }
cd "$root" || exit 1

# The harness surface: prose a session reads, plus the scripts that enforce it.
# AGENTS.md is a symlink to CLAUDE.md and is skipped so one file is not counted
# twice.
#
# This file excludes ITSELF. It is the measuring instrument, and its patterns
# necessarily contain every string it looks for, so counting it would add one to
# every row and never go down.
files=()
for f in CLAUDE.md review.md docs/harness.md .claude/settings.json \
         .claude/rules/*.md .claude/agents/*.md scripts/claude/*.sh; do
  [[ "$f" == scripts/claude/fact-census.sh ]] && continue
  [[ -f "$f" && ! -L "$f" ]] && files+=("$f")
done
(( ${#files[@]} > 0 )) || { echo "FAIL: no harness files found. Refusing to report 0." >&2; exit 1; }

total_dupes=0

# fact <label> <home> <pattern> [second pattern that must also match]
fact() {
  local label="$1" home="$2" p1="$3" p2="${4:-}"
  local matched=() f
  for f in "${files[@]}"; do
    grep -qiE -- "$p1" "$f" 2>/dev/null || continue
    if [[ -n "$p2" ]]; then grep -qiE -- "$p2" "$f" 2>/dev/null || continue; fi
    matched+=("$f")
  done
  local n=${#matched[@]}
  printf '%-34s %2d file(s)   home: %s\n' "$label" "$n" "$home"
  (( n > 1 )) && total_dupes=$((total_dupes+1))
  if (( show_list == 1 && n > 0 )); then
    printf '    %s\n' "${matched[@]}"
  fi
}

echo "harness files scanned: ${#files[@]}"
echo ""

fact "unbounded-wait remedy"      "bash-guard.sh denial"        'timeout'            '(not|NOT) installed'
fact "ci.yml skips apps/api/tests" ".claude/rules/ci-and-build-logic.md" 'apps/api/tests'
fact "apps/landing is dead"       "route-guard.sh deny"         'apps/landing'
fact "make gen is a stub"         "route-guard.sh deny"         '(make gen|gen-openapi)' 'stub'
fact "toolchain not on PATH"      "session-brief.sh"            '(sqlc|govulncheck|atlas)' '(GOPATH|on .?PATH)'
fact "every number needs its cmd" "CLAUDE.md"                   'every number'
fact "commit before the slow step" "commit-gate.sh + CLAUDE.md" 'commit (before|first)'
fact "never git add -A"           "CLAUDE.md"                   'git add -A'
fact "no assistant trailers"      "CLAUDE.md"                   '(Co-Authored-By|Claude-Session)'
fact "wp.org slug"                ".claude/rules/wp-agent.md"   'fleet-agent-site-manager'
fact "release.yml is build-only"  ".claude/rules/ci-and-build-logic.md" 'build.only|build-only'
fact "verify, not the pipeline"   "CLAUDE.md"                   'green deploy job'

echo ""
echo "facts stated in more than one file: $total_dupes"
