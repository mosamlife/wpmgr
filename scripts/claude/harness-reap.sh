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
#   - it refuses to run at all if it cannot enter the repository root. Every
#     action here resolves its repository from the current directory, and the
#     report and the self-protection checks all assume that directory is the
#     root; acting from one it never reached is how a delete goes wrong
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
    # silently truncates the safety notes the moment someone adds one. The
    # `NR==1` arm drops the shebang, which is a comment line to awk and was
    # therefore printed as the first line of the help text. Same expression as
    # route-guard-coverage.sh, quickstart-selfhost.sh and init-env.sh; all four
    # are asserted identical in guards_test.sh.
    -h|--help) awk 'NR==1 && /^#!/ {next} !/^#/{exit} {print}' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

root=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "not in a git repo" >&2; exit 2; }
# Never continue on a failed `cd` (SC2164). The three GIT commands below - `git
# worktree remove --force`, `git branch -d`, and the rev-parse that picks the
# base branch - resolve their repository from the CURRENT DIRECTORY, while
# the report line, the `$wt == $root` self-protection check and the ownership
# test all assume that directory is $root. Unchecked, a failed cd left every
# one of them running somewhere the script had never reached, and it did not
# even slow down: the proof in guards_test.sh plants the failure and the old
# code goes on to remove a worktree and print "1 removed" from a location it
# never verified. A script whose next four commands delete things stops when it
# cannot reach the place it is about to act on.
#
# `go clean -cache` is deliberately NOT in that list, though an earlier version
# of this comment named it as a fourth. It is not cwd-sensitive at all: it wipes
# the single machine-wide directory `go env GOCACHE` names, which is the same
# directory whatever the current one is and whatever repository - if any - the
# current one belongs to. Listing it here read as though the cd guard covered
# it. Nothing about cwd covers it; what stands between it and the cache is
# --apply plus the ceiling check in section 3.
cd "$root" || {
  echo "cannot enter the repository root '$root' (exit $?). Refusing to run." >&2
  echo "Every action below resolves its repository from the current directory," >&2
  echo "which is still $(pwd) - not the root just resolved. Nothing was touched." >&2
  exit 2
}

base=$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || echo origin/main)
git rev-parse --verify --quiet "$base" >/dev/null || base=main
git rev-parse --verify --quiet "$base" >/dev/null || { echo "cannot resolve a base branch" >&2; exit 2; }

say() { printf '%s\n' "$*"; }
act() { if [[ $APPLY -eq 1 ]]; then say "  DO   $*"; else say "  WOULD $*"; fi; }
# `du` on a path that has just been removed, or that a worktree record points at
# after the directory went, prints nothing. Say so rather than print an empty
# column: a blank size reads as zero, and zero is the one thing it does not mean.
#
# It also RETURNS du's status, so a caller that has to decide on the number can
# tell "0" from "could not measure". The Go build-cache ceiling in section 3 is
# that caller, and it was the one place in this file where neither this helper
# nor countof() was used: it read `du -sk "$gocache" | cut -f1` straight into an
# arithmetic expansion, so a failed du became the empty string, `${kb:-0}` became
# 0, 0 was below any ceiling, and the section printed "under the ceiling, leaving
# it" - a measurement that never happened, reported as a measurement.
#
# The unit is a parameter so this stays ONE du call site rather than growing a
# second helper beside it. `-s` always, never a recursive listing.
sizeof() { # sizeof <path> [du unit flag, default h] -> size on stdout, du's status
  local out rc
  out=$(du -s"${2:-h}" "$1" 2>/dev/null); rc=$?
  if [[ $rc -ne 0 || -z "$out" ]]; then printf 'size unknown'; return 1; fi
  printf '%s' "${out%%$'\t'*}"
  return 0
}

# Count something, or say plainly that it could not be counted. Verbatim the
# helper from session-brief.sh, and deliberately not a second idiom for the same
# question: `<command> | wc -l` prints a perfectly plausible 0 when the COMMAND
# failed, and 0 is indistinguishable from "there are none". In a brief that
# understates the disk you are holding. In a reaper it is worse - "0 merged, 0
# unmerged" reads as "nothing to reap here", the maintainer stops looking, and
# the orphaned branches stay.
#
# Prints the count on success and "unknown" on failure, and returns the
# command's own status so the caller can tell those two apart. `grep -c ''`
# rather than `wc -l` so that empty output counts as 0 and a final line with no
# newline still counts as 1.
countof() { # countof <command...>
  local out rc
  out=$("$@" 2>/dev/null); rc=$?
  [[ $rc -ne 0 ]] && { printf 'unknown'; return 1; }
  printf '%s' "$(printf '%s' "$out" | grep -c '')"
  return 0
}

# An enumeration that failed must not be walked as if it were empty. Prints
# nothing; returns the command's status and leaves the output in $enum_out.
# Separate from countof() because the reaper needs the LIST, not just its size,
# and `while read < <(cmd)` throws the status of `cmd` away entirely.
enum_out=""
enumerate() { # enumerate <command...>
  local rc
  enum_out=$("$@" 2>/dev/null); rc=$?
  return $rc
}

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
# The same status-discarding shape as section 2. This one is *nearly* covered
# already - the identical enumeration runs above to resolve $main_wt and exits 2
# if it comes back empty - but that is a different invocation, so a git that
# fails only on the second call would still walk an empty sweep and report a
# clean "0 removed, 0 held back, 0 not ours, 0 failed".
enumerate git worktree list --porcelain; sweep_ok=$?
if [[ $sweep_ok -ne 0 ]]; then
  failures=$((failures+1))
  say "  FAILED enumerate worktrees - 'git worktree list' exited $sweep_ok. Nothing was swept. This is NOT 'there are none'."
  removed=unknown; kept=unknown; foreign=unknown; wt_failed=unknown
fi
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
  # These two decide whether the worktree gets deleted, and both used to fail
  # OPEN TOWARDS DELETION, which is the worst direction available: a failing
  # `git status` produced no output, `wc -l` turned that into 0, 0 read as
  # "clean", and the worktree was removed on the strength of a measurement that
  # never happened. A failing `rev-parse` left $head empty, and the merged-check
  # is guarded by `[[ -n "$head" ]]`, so the "is it merged" question was skipped
  # rather than answered. Unmeasured is not clean and it is not merged.
  dirty=$(countof git -C "$wt" status --porcelain); dirty_ok=$?
  head=$(git -C "$wt" rev-parse HEAD 2>/dev/null); head_ok=$?
  if [[ $dirty_ok -ne 0 || $head_ok -ne 0 ]]; then
    say "  KEEP   $name - could not read its state (status: $dirty_ok, rev-parse: $head_ok). Nothing is deleted on the strength of a measurement that failed."
    # Both columns, deliberately: it was held back AND it is a failure. Counting
    # it only as "held back" left the section line reading "0 failed" while the
    # trailer at the bottom said one action had failed, and a report that
    # contradicts itself at a glance is one nobody finishes reading.
    kept=$((kept+1)); wt_failed=$((wt_failed+1)); failures=$((failures+1)); continue
  fi
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
done < <(printf '%s' "$enum_out" | awk '/^worktree /{print $2}')
act "git worktree prune"
attempt "git worktree prune" git worktree prune
say "  $removed $did, $kept held back, $foreign not ours (listed above; remove those by hand if you want the space), $wt_failed failed"

# ---- 2. orphaned worktree-* branches --------------------------------------
say "== worktree-* branches merged into $base"
# A branch that is still checked out in a live worktree is skipped: `git branch
# -d` would refuse it anyway, and listing it as removable is noise that trains
# the reader to skim this output.
# Enumerate first and keep git's exit status, all three times. Every one of
# these used to be read straight into a loop or a `wc -l`, which discards it: a
# git that failed produced zero iterations and a count of 0, and this section
# then printed "0 merged, 0 unmerged, 0 failed" - byte-identical to a repository
# that honestly has no worktree-* branches. The reaper is destructive, so "I
# could not look" must never be delivered as "there is nothing to reap".
enumerate git worktree list --porcelain; co_ok=$?
checked_out=$(printf '%s' "$enum_out" | awk '/^branch /{sub("refs/heads/","",$2); print $2}')
enumerate git branch --list 'worktree-*' --merged "$base" --format='%(refname:short)'; merged_ok=$?
merged_list="$enum_out"
unmerged=$(countof git branch --list 'worktree-*' --no-merged "$base"); unmerged_ok=$?

n=0; skipped=0; br_failed=0
if [[ $co_ok -ne 0 || $merged_ok -ne 0 || $unmerged_ok -ne 0 ]]; then
  # Counted as a failure, so the exit-1 trailer fires and this cannot be
  # mistaken for a clean run. Nothing is deleted from a list we do not trust:
  # the checked-out set is what stops the reaper offering a branch that a live
  # worktree is sitting on, and without it every delete is a guess.
  failures=$((failures+1))
  say "  FAILED enumerate worktree-* branches - git exited non-zero (worktree list: $co_ok, merged: $merged_ok, unmerged: $unmerged_ok). No branch was inspected and none was deleted."
  say "         This is NOT 'there are none'. The reaper could not look, so the orphaned branches may all still be there; re-run it once git works."
  n=unknown; br_failed=unknown
else
  while IFS= read -r b; do
    [[ -z "$b" ]] && continue
    if printf '%s\n' "$checked_out" | grep -qx "$b"; then skipped=$((skipped+1)); continue; fi
    act "git branch -d '$b'"
    if attempt "delete branch '$b'" git branch -d "$b"; then
      n=$((n+1))
    else
      br_failed=$((br_failed+1))
    fi
  done < <(printf '%s\n' "$merged_list")
  [[ $skipped -gt 0 ]] && say "  $skipped still checked out in a live worktree, skipped"
fi
say "  $n merged ($didbr), $unmerged unmerged (left alone - inspect them by hand), $br_failed failed"

# ---- 3. Go build cache -----------------------------------------------------
if [[ $WORKTREES_ONLY -eq 1 ]]; then
say "== Go build cache and docker volumes skipped (--worktrees-only)"
else
say "== Go build cache (ceiling ${CACHE_GB}G; module cache is never touched)"
gocache=$(go env GOCACHE 2>/dev/null || true)
if [[ -z "$gocache" ]]; then
  say "  'go env GOCACHE' returned nothing - no Go toolchain on PATH, or it failed."
  say "  The cache was NOT measured and NOT cleaned. This is not 'no cache to clean'."
  failures=$((failures+1))
elif [[ ! -d "$gocache" ]]; then
  say "  $gocache is not a directory - nothing to measure or clean"
else
  kb=$(sizeof "$gocache" k); kbrc=$?
  say "  $gocache = $(sizeof "$gocache")"
  # A ceiling test is a decision, and a decision needs a number. `du` fails on a
  # cache being written underneath it, on a permission it cannot cross, on an
  # unreadable mount - and the empty string it leaves behind used to arrive at
  # this comparison as 0, which is under every ceiling. Refuse to decide instead,
  # and say so loudly: this counts as a failure, so the script exits 1 and the
  # run cannot be read as "checked, nothing to do".
  if [[ $kbrc -ne 0 ]] || ! [[ "$kb" =~ ^[0-9]+$ ]]; then
    say "  COULD NOT MEASURE the build cache: 'du -sk' gave '$kb' (exit $kbrc)."
    say "  The ceiling check is SKIPPED and nothing was cleaned - a size that was"
    say "  never read is not a size below it. Measure it by hand:"
    say "    du -sh '$gocache'"
    failures=$((failures+1))
  else
    gb=$(( kb / 1024 / 1024 ))
    if [[ "$gb" -ge "$CACHE_GB" ]]; then
      act "go clean -cache   (reclaims ~${gb}G; the next build is slow, nothing is lost)"
      # Same rule as the worktrees: the note claims the cache was reclaimed, so
      # it may only be written when the clean actually returned 0.
      attempt "go clean -cache" go clean -cache && reclaimed_note="${reclaimed_note} build-cache"
    else
      say "  ${gb}G is under the ${CACHE_GB}G ceiling, leaving it"
    fi
  fi
fi
gomod=$(go env GOMODCACHE 2>/dev/null || true)
[[ -n "$gomod" && -d "$gomod" ]] && say "  module cache $(sizeof "$gomod") - reported only, never cleaned here"

# ---- 4. testcontainer volumes ---------------------------------------------
say "== docker testcontainer volumes"
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  # No `mapfile`: macOS ships bash 3.2 and this script must run on the machine
  # it was written for. That is the same class of defect as prescribing
  # `timeout`, which is also absent here.
  vols=""
  # `docker info` above proves the daemon answers, which is not the same as
  # `docker volume ls` working: the failure this script was written for was a
  # daemon that was up and erroring on a missing snapshot. That printed nothing,
  # and nothing arrived here as "none labelled org.testcontainers=true" - the
  # exact reading a maintainer hunting 17 orphaned volumes must not be given.
  enumerate docker volume ls -q --filter label=org.testcontainers=true; vol_ok=$?
  while IFS= read -r v; do [[ -n "$v" ]] && vols="$vols $v"; done \
    < <(printf '%s\n' "$enum_out")
  vols="${vols# }"
  if [[ $vol_ok -ne 0 ]]; then
    failures=$((failures+1))
    say "  FAILED enumerate volumes - 'docker volume ls' exited $vol_ok while the daemon was answering 'docker info'. This is NOT 'there are none'; no volume was pruned."
  elif [[ -z "$vols" ]]; then
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
