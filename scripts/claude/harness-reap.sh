#!/usr/bin/env bash
# Reclaim the disk that agent runs leak. Failure B: an agent hit "No space left
# on device" with 803MB free because the Go build cache had reached 26GB, 18
# worktrees held 15GB, and 17 orphaned testcontainer volumes were still around.
# `docker system df` errored on a missing snapshot, which is what running out of
# disk mid-build does. All of it was cleaned by hand, and all of it regrew.
#
# Nothing else reaps any of this. Claude Code's retention sweep covers only
# paths under ~/.claude, and its worktree sweep is documented to SKIP any
# worktree holding "changed or untracked files, or unpushed commits" - which is
# precisely the worktrees an agent did work in. The ones that accumulate are the
# ones the sweep may never touch.
#
#   scripts/claude/harness-reap.sh            REPORT ONLY (default)
#   scripts/claude/harness-reap.sh --apply    actually delete
#   scripts/claude/harness-reap.sh --apply --cache-gb 8
#
# Safety, because this deletes things:
#   - dry run by default; --apply is the only thing that removes anything
#   - a worktree is removed only if it is clean AND its branch is merged into
#     the default branch. Never on mtime; mtime lies.
#   - a branch is deleted only with `git branch -d` (merged-only), never -D
#   - the Go MODULE cache is never touched: it is 4GB of re-downloadable
#     dependencies whose removal costs a slow rebuild for nothing
#   - only volumes with the testcontainers label are pruned, never a bare
#     `docker volume prune`
set -uo pipefail

APPLY=0
CACHE_GB=10
while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) APPLY=1 ;;
    --cache-gb) CACHE_GB="${2:-10}"; shift ;;
    -h|--help) sed -n '1,30p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

root=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "not in a git repo" >&2; exit 2; }
cd "$root"

base=$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || echo origin/main)
git rev-parse --verify --quiet "$base" >/dev/null || base=main
git rev-parse --verify --quiet "$base" >/dev/null || { echo "cannot resolve a base branch" >&2; exit 2; }

say() { printf '%s\n' "$*"; }
act() { if [[ $APPLY -eq 1 ]]; then say "  DO   $*"; else say "  WOULD $*"; fi; }

reclaimed_note=""

# ---- 1. worktrees ----------------------------------------------------------
say "== agent worktrees (base: $base)"
removed=0; kept=0
while IFS= read -r wt; do
  [[ -z "$wt" ]] && continue
  case "$wt" in "$root") continue ;; esac
  name=$(basename "$wt")
  if [[ ! -d "$wt" ]]; then
    say "  stale  $name (directory gone)"; continue
  fi
  dirty=$(git -C "$wt" status --porcelain 2>/dev/null | wc -l | tr -d ' ')
  head=$(git -C "$wt" rev-parse HEAD 2>/dev/null || echo "")
  if [[ "${dirty:-1}" -ne 0 ]]; then
    say "  KEEP   $name - $dirty uncommitted path(s). Commit or discard it by hand."
    kept=$((kept+1)); continue
  fi
  if [[ -n "$head" ]] && ! git merge-base --is-ancestor "$head" "$base" 2>/dev/null; then
    ahead=$(git rev-list --count "$base..$head" 2>/dev/null || echo "?")
    say "  KEEP   $name - $ahead commit(s) not in $base. Push or merge it first."
    kept=$((kept+1)); continue
  fi
  act "git worktree remove --force '$wt'"
  [[ $APPLY -eq 1 ]] && git worktree remove --force "$wt" 2>/dev/null
  removed=$((removed+1))
done < <(git worktree list --porcelain 2>/dev/null | awk '/^worktree /{print $2}')
act "git worktree prune"
[[ $APPLY -eq 1 ]] && git worktree prune
say "  $removed removable, $kept held back"

# ---- 2. orphaned worktree-* branches --------------------------------------
say "== worktree-* branches merged into $base"
# A branch that is still checked out in a live worktree is skipped: `git branch
# -d` would refuse it anyway, and listing it as removable is noise that trains
# the reader to skim this output.
checked_out=$(git worktree list --porcelain 2>/dev/null | awk '/^branch /{sub("refs/heads/","",$2); print $2}')
n=0; skipped=0
while IFS= read -r b; do
  [[ -z "$b" ]] && continue
  if printf '%s\n' "$checked_out" | grep -qx "$b"; then skipped=$((skipped+1)); continue; fi
  act "git branch -d '$b'"
  [[ $APPLY -eq 1 ]] && git branch -d "$b" >/dev/null 2>&1
  n=$((n+1))
done < <(git branch --list 'worktree-*' --merged "$base" --format='%(refname:short)' 2>/dev/null)
[[ $skipped -gt 0 ]] && say "  $skipped still checked out in a live worktree, skipped"
unmerged=$(git branch --list 'worktree-*' --no-merged "$base" 2>/dev/null | wc -l | tr -d ' ')
say "  $n merged (deletable), $unmerged unmerged (left alone - inspect them by hand)"

# ---- 3. Go build cache -----------------------------------------------------
say "== Go build cache (ceiling ${CACHE_GB}G; module cache is never touched)"
gocache=$(go env GOCACHE 2>/dev/null || true)
if [[ -n "$gocache" && -d "$gocache" ]]; then
  kb=$(du -sk "$gocache" 2>/dev/null | cut -f1)
  gb=$(( ${kb:-0} / 1024 / 1024 ))
  say "  $gocache = $(du -sh "$gocache" 2>/dev/null | cut -f1)"
  if [[ "$gb" -ge "$CACHE_GB" ]]; then
    act "go clean -cache   (reclaims ~${gb}G; the next build is slow, nothing is lost)"
    [[ $APPLY -eq 1 ]] && go clean -cache
    reclaimed_note="${reclaimed_note} build-cache"
  else
    say "  under the ceiling, leaving it"
  fi
fi
gomod=$(go env GOMODCACHE 2>/dev/null || true)
[[ -n "$gomod" && -d "$gomod" ]] && say "  module cache $(du -sh "$gomod" 2>/dev/null | cut -f1) - reported only, never cleaned here"

# ---- 4. testcontainer volumes ---------------------------------------------
say "== docker testcontainer volumes"
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  # No `mapfile`: macOS ships bash 3.2 and this script must run on the machine
  # it was written for. That is the same class of defect as prescribing
  # `timeout`, which is also absent here.
  vols=""
  while IFS= read -r v; do [[ -n "$v" ]] && vols="$vols $v"; done \
    < <(docker volume ls -q --filter label=org.testcontainers=true 2>/dev/null)
  vols="${vols# }"
  if [[ -z "$vols" ]]; then
    say "  none labelled org.testcontainers=true"
  else
    nvol=$(printf '%s\n' $vols | wc -l | tr -d ' ')
    say "  ${nvol} labelled volume(s)"
    act "docker volume rm $vols"
    [[ $APPLY -eq 1 ]] && docker volume rm $vols >/dev/null 2>&1
  fi
  docker system df >/dev/null 2>&1 || say "  NOTE: 'docker system df' is erroring. That is what running out of disk mid-build does to the containerd snapshotter; it needs a Docker Desktop reset, which this script will not do for you."
else
  say "  docker unreachable, skipped"
fi

# ---- 5. result -------------------------------------------------------------
say "== disk"
df -h / | tail -1
if [[ $APPLY -eq 0 ]]; then
  say ""
  say "REPORT ONLY. Re-run with --apply to perform the actions above."
fi
exit 0
