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
#   scripts/claude/harness-reap.sh                   REPORT ONLY (default)
#   scripts/claude/harness-reap.sh --apply           actually delete
#   scripts/claude/harness-reap.sh --apply --cache-gb 8
#   scripts/claude/harness-reap.sh --worktrees-only  worktrees and their
#                                                    branches; no Go cache,
#                                                    no Docker
#
# Safety, because this deletes things:
#   - dry run by default; --apply is the only thing that removes anything
#   - a worktree is removed only if THIS HARNESS CREATED IT. Ownership is
#     structural, not a heuristic: the directory sits directly under
#     `.claude/worktrees/` of the main checkout and is named `agent-*` or
#     `wf_*`. A worktree a person made by hand, one with an off-shape name in
#     that same directory, and the main checkout itself are all somebody
#     else's and are never removed. Unowned ones are still REPORTED with their
#     size, because held back is not silence: the maintainer decides.
#     The earlier version fed `git worktree list` straight into
#     `git worktree remove --force`, which removed anything clean and merged -
#     and, run from inside a worktree, offered up the main checkout, because
#     `git rev-parse --show-toplevel` resolves to the worktree.
#   - a worktree is removed only if it is also clean AND its branch is merged
#     into the default branch. Never on mtime; mtime lies.
#   - the worktree the reaper is running inside is never removed, however
#     clean it is: `git worktree remove --force` on your own cwd deletes the
#     ground you are standing on
#   - a branch is deleted only with `git branch -d` (merged-only), never -D
#   - the Go MODULE cache is never touched: it is 4GB of re-downloadable
#     dependencies whose removal costs a slow rebuild for nothing
#   - only volumes with the testcontainers label are pruned, never a bare
#     `docker volume prune`
#   - nothing counts itself done. Every destructive command is checked for its
#     exit status, a refusal is printed with the tool's own words, a failed
#     action is never counted as reclaimed, and the script exits 1 if anything
#     failed. It used to run each of them as `... 2>/dev/null` and then
#     increment the counter unconditionally, so a `git worktree lock`ed
#     worktree was reported as removed while it sat there holding its disk
set -uo pipefail

APPLY=0
CACHE_GB=10
WORKTREES_ONLY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) APPLY=1 ;;
    --cache-gb) CACHE_GB="${2:-10}"; shift ;;
    --worktrees-only) WORKTREES_ONLY=1 ;;
    # Print the whole comment header, however long it grows. A fixed line range
    # silently truncates the safety notes the moment someone adds one.
    -h|--help) awk '!/^#/{exit} {print}' "$0"; exit 0 ;;
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
# `du` on a path that has just been removed, or that a worktree record points at
# after the directory went, prints nothing. Say so rather than print an empty
# column: a blank size reads as zero, and zero is the one thing it does not mean.
sizeof() { local s; s=$(du -sh "$1" 2>/dev/null | cut -f1); printf '%s' "${s:-size unknown}"; }

# Every destructive command goes through here, and nothing counts itself done.
#
# Each site used to be `<destructive command> 2>/dev/null` followed by an
# unconditional counter increment, so a refusal from git or docker was thrown
# away and reported as a success: against a `git worktree lock`ed worktree the
# script printed "1 removable" while the directory was still on disk, and git's
# own "fatal: cannot remove a locked working tree" went to /dev/null. That is
# the failure this project keeps having - announcing success over its own
# errors - and a reclaim report that lies is worse than none, because the disk
# it claims to have freed is still gone.
#
# Returns 0 when the caller may count the action, 1 when it may not. In a dry
# run nothing is attempted, so nothing can have failed and the counters mean
# "removable" rather than "removed".
failures=0
attempt() { # attempt <label> <cmd> [args...]
  local label=$1; shift
  [[ $APPLY -eq 1 ]] || return 0
  local out rc
  out=$("$@" 2>&1); rc=$?
  [[ $rc -eq 0 ]] && return 0
  failures=$((failures+1))
  # Fold to one line so a multi-line git error cannot break up the report, and
  # never print an empty reason: a command that failed silently still has to
  # say so, or the reader assumes the blank means nothing happened.
  say "  FAILED $label - exit $rc: $(printf '%s' "${out:-(no output)}" | tr '\n' ' ' | sed 's/  */ /g')"
  return 1
}
# The counters are "what could go" in a dry run and "what actually went" under
# --apply. Same numbers, different claim; say which one you are making.
if [[ $APPLY -eq 1 ]]; then did=removed; didbr=deleted; else did=removable; didbr=deletable; fi

# `git worktree list` puts the MAIN worktree first, always - that is the one
# fact that survives being run from inside a worktree, where
# `git rev-parse --show-toplevel` gives the worktree instead. Everything the
# harness creates lives directly under `.claude/worktrees/` of that checkout.
main_wt=$(git worktree list --porcelain 2>/dev/null | sed -n 's/^worktree //p' | head -1)
[[ -n "$main_wt" ]] || { echo "cannot resolve the main checkout from 'git worktree list'; refusing to remove anything" >&2; exit 2; }
harness_dir="$main_wt/.claude/worktrees"

# Owned == created by this harness. Structural, both halves required:
# directly under $harness_dir (a nested path is not "directly under"), AND an
# agent-* or wf_* name. `mine-by-hand` sitting in that directory is a person's.
harness_made() {
  [[ "$(dirname "$1")" == "$harness_dir" ]] || return 1
  case "$(basename "$1")" in agent-*|wf_*) return 0 ;; *) return 1 ;; esac
}

reclaimed_note=""

# ---- 1. worktrees ----------------------------------------------------------
say "== agent worktrees (base: $base, harness dir: $harness_dir)"
removed=0; kept=0; foreign=0; wt_failed=0
while IFS= read -r wt; do
  [[ -z "$wt" ]] && continue
  # The main checkout is not leaked disk and is never a reap target, whether or
  # not we are standing in it.
  [[ "$wt" == "$main_wt" ]] && continue
  name=$(basename "$wt")
  if ! harness_made "$wt"; then
    say "  OTHER  $name - not created by this harness ($(sizeof "$wt")): $wt"
    foreign=$((foreign+1)); continue
  fi
  if [[ ! -d "$wt" ]]; then
    say "  stale  $name (directory gone)"; continue
  fi
  if [[ "$wt" == "$root" ]]; then
    say "  KEEP   $name - the reaper is running inside it. Re-run from the main checkout."
    kept=$((kept+1)); continue
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
  if attempt "remove worktree '$name'" git worktree remove --force "$wt"; then
    removed=$((removed+1))
  else
    wt_failed=$((wt_failed+1))
  fi
done < <(git worktree list --porcelain 2>/dev/null | awk '/^worktree /{print $2}')
act "git worktree prune"
attempt "git worktree prune" git worktree prune
say "  $removed $did, $kept held back, $foreign not ours (listed above; remove those by hand if you want the space), $wt_failed failed"

# ---- 2. orphaned worktree-* branches --------------------------------------
say "== worktree-* branches merged into $base"
# A branch that is still checked out in a live worktree is skipped: `git branch
# -d` would refuse it anyway, and listing it as removable is noise that trains
# the reader to skim this output.
checked_out=$(git worktree list --porcelain 2>/dev/null | awk '/^branch /{sub("refs/heads/","",$2); print $2}')
n=0; skipped=0; br_failed=0
while IFS= read -r b; do
  [[ -z "$b" ]] && continue
  if printf '%s\n' "$checked_out" | grep -qx "$b"; then skipped=$((skipped+1)); continue; fi
  act "git branch -d '$b'"
  if attempt "delete branch '$b'" git branch -d "$b"; then
    n=$((n+1))
  else
    br_failed=$((br_failed+1))
  fi
done < <(git branch --list 'worktree-*' --merged "$base" --format='%(refname:short)' 2>/dev/null)
[[ $skipped -gt 0 ]] && say "  $skipped still checked out in a live worktree, skipped"
unmerged=$(git branch --list 'worktree-*' --no-merged "$base" 2>/dev/null | wc -l | tr -d ' ')
say "  $n merged ($didbr), $unmerged unmerged (left alone - inspect them by hand), $br_failed failed"

# ---- 3. Go build cache -----------------------------------------------------
if [[ $WORKTREES_ONLY -eq 1 ]]; then
say "== Go build cache and docker volumes skipped (--worktrees-only)"
else
say "== Go build cache (ceiling ${CACHE_GB}G; module cache is never touched)"
gocache=$(go env GOCACHE 2>/dev/null || true)
if [[ -n "$gocache" && -d "$gocache" ]]; then
  kb=$(du -sk "$gocache" 2>/dev/null | cut -f1)
  gb=$(( ${kb:-0} / 1024 / 1024 ))
  say "  $gocache = $(du -sh "$gocache" 2>/dev/null | cut -f1)"
  if [[ "$gb" -ge "$CACHE_GB" ]]; then
    act "go clean -cache   (reclaims ~${gb}G; the next build is slow, nothing is lost)"
    # Same rule as the worktrees: the note claims the cache was reclaimed, so
    # it may only be written when the clean actually returned 0.
    attempt "go clean -cache" go clean -cache && reclaimed_note="${reclaimed_note} build-cache"
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
    # $vols is deliberately unquoted: it splits into one argument per volume.
    # `docker volume rm` exits non-zero if ANY of them is still in use, and it
    # names which - the sentence that used to go to /dev/null.
    if attempt "docker volume rm (${nvol})" docker volume rm $vols; then
      [[ $APPLY -eq 1 ]] && say "  ${nvol} removed"
    fi
  fi
  docker system df >/dev/null 2>&1 || say "  NOTE: 'docker system df' is erroring. That is what running out of disk mid-build does to the containerd snapshotter; it needs a Docker Desktop reset, which this script will not do for you."
else
  say "  docker unreachable, skipped"
fi
fi  # end --worktrees-only skip

# ---- 5. result -------------------------------------------------------------
say "== disk"
df -h / | tail -1
if [[ $APPLY -eq 0 ]]; then
  say ""
  say "REPORT ONLY. Re-run with --apply to perform the actions above."
fi
# Red on any refused action. A reclaim that half-worked and exited 0 is the
# shape that gets read as "done" and closed; the counts above already exclude
# the failures, and this makes them impossible to walk past.
if [[ $failures -gt 0 ]]; then
  say ""
  say "$failures action(s) FAILED - each is printed above with the exact error. Nothing was silently skipped, and nothing that failed was counted as reclaimed."
  exit 1
fi
exit 0
