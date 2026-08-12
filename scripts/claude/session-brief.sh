#!/usr/bin/env bash
# SessionStart hook (startup | resume | compact).
#
# Stdout from this event is added to the session's context, so this is the one
# place the harness can tell the session something true about the machine BEFORE
# it starts a build.
#
# It exists for two measured failures.
#
# B: an agent hit "No space left on device" with 803MB free because the Go build
#    cache had reached 26GB, while 18 worktrees held 15GB. Nothing in Claude Code
#    sweeps a Go cache, a Docker volume or a testcontainer - those are outside
#    its retention model entirely - and its worktree sweep is contractually
#    forbidden to touch a worktree that holds work, which is exactly the ones
#    that accumulate. Discovering this mid-link is the expensive way.
#
# F: the guards fail open when jq is missing. Silence is the defect this project
#    keeps shipping, so the degradation is announced here, every session,
#    instead.
#
# Never blocks. SessionStart cannot block, and this must stay cheap.
set -uo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0

echo "## Machine state"

# --- guard health -----------------------------------------------------------
if ! command -v jq >/dev/null 2>&1; then
  echo "- route-guard and bash-guard are INACTIVE: jq is not installed. Routing"
  echo "  and the wait-loop deny are unenforced this session. Fix: install jq."
fi
for t in timeout sqlc atlas govulncheck; do
  command -v "$t" >/dev/null 2>&1 && continue
  case "$t" in
    timeout) echo "- timeout is not installed. Bound waits with 'for i in \$(seq 1 N)', the Bash tool's timeout parameter, or Monitor." ;;
    *) [[ -x "$(go env GOPATH 2>/dev/null)/bin/$t" ]] \
         && echo "- $t is at \$(go env GOPATH)/bin/$t, not on PATH." \
         || echo "- $t is NOT INSTALLED. A gate that needs it must fail loudly, not be skipped." ;;
  esac
done

# --- disk -------------------------------------------------------------------
avail_g=$(df -g / 2>/dev/null | awk 'NR==2{print $4}')
[[ -z "$avail_g" ]] && avail_g=$(df -BG / 2>/dev/null | awk 'NR==2{gsub(/G/,"",$4); print $4}')
if [[ -n "$avail_g" ]]; then
  echo "- disk free: ${avail_g}Gi"
  [[ "$avail_g" -lt 25 ]] 2>/dev/null && echo "  WARNING: under 25Gi. Run 'make harness-reap' before starting a build."
fi

gocache=$(go env GOCACHE 2>/dev/null || true)
[[ -n "$gocache" && -d "$gocache" ]] && echo "- Go build cache: $(du -sh "$gocache" 2>/dev/null | cut -f1)"
gomod=$(go env GOMODCACHE 2>/dev/null || true)
[[ -n "$gomod" && -d "$gomod" ]] && echo "- Go module cache: $(du -sh "$gomod" 2>/dev/null | cut -f1)"

# --- worktrees --------------------------------------------------------------
wt=$(git -C "$root" worktree list 2>/dev/null | tail -n +2 | wc -l | tr -d ' ')
br=$(git -C "$root" branch --list 'worktree-*' 2>/dev/null | wc -l | tr -d ' ')
if [[ "${wt:-0}" -gt 0 || "${br:-0}" -gt 0 ]]; then
  size=$(du -sh "$root/.claude/worktrees" 2>/dev/null | cut -f1)
  echo "- agent worktrees: ${wt} live (${size:-unknown}), ${br} worktree-* branches"
  [[ "${br:-0}" -gt "$(( ${wt:-0} + 4 ))" ]] && echo "  $(( br - wt )) orphaned branches. 'make harness-reap' lists them; 'make harness-reap-apply' deletes the merged ones."
fi

# --- volumes ----------------------------------------------------------------
if command -v docker >/dev/null 2>&1; then
  vols=$(docker volume ls -q 2>/dev/null | wc -l | tr -d ' ')
  [[ "${vols:-0}" -gt 10 ]] && echo "- ${vols} docker volumes. Orphaned testcontainer volumes accumulate; 'make harness-reap' reports them."
fi

exit 0
