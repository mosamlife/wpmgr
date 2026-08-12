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
# --- toolchain --------------------------------------------------------------
# Four states, and the old code collapsed two of them into the loudest one.
#
# `[[ -x "$(go env GOPATH 2>/dev/null)/bin/$t" ]]` reads as a lookup, but when
# `go` is not resolvable the substitution is EMPTY, the test degrades to
# `-x /bin/$t`, that is false, and the script asserted "NOT INSTALLED". That is
# a definite negative from a measurement that never happened: on this machine it
# reported sqlc, atlas and govulncheck all missing while all three sat in
# /Users/mosamgor/go/bin and ran. Same defect as the `wc -l` two sections down,
# and it matters more, because CLAUDE.md's Delivery section names this script as
# the authority for where the toolchain lives. An authority that guesses is
# worse than the hard-coded prose it replaced.
#
# GOBIN is asked before GOPATH because `go install` honours GOBIN when it is
# set, so a machine that sets it keeps its binaries somewhere GOPATH/bin never
# mentions. `command -v` stays the first question: a tool on PATH needs no
# directory at all. Both were considered and both are in.
lookup_dir=""
lookup_why=""
if ! command -v go >/dev/null 2>&1; then
  lookup_why="'go' is not on PATH, so its install directory cannot be asked for"
else
  lookup_dir=$(go env GOBIN 2>/dev/null)
  if [[ -z "$lookup_dir" ]]; then
    gopath=$(go env GOPATH 2>/dev/null)
    [[ -n "$gopath" ]] && lookup_dir="$gopath/bin"
  fi
  [[ -z "$lookup_dir" ]] && lookup_why="'go env GOBIN' and 'go env GOPATH' both came back empty"
fi

on_path=""
for t in timeout sqlc atlas govulncheck; do
  if command -v "$t" >/dev/null 2>&1; then
    on_path="$on_path $t"
    continue
  fi
  case "$t" in
    timeout) echo "- timeout is not installed. Bound waits with 'for i in \$(seq 1 N)', the Bash tool's timeout parameter, or Monitor." ;;
    *)
      if [[ -n "$lookup_why" ]]; then
        # The one sentence this may never say here is NOT INSTALLED. With no
        # way to look, absence is a claim the script is not in a position to
        # make, and it is the claim a session would act on.
        echo "- $t: could not check whether it is installed - $lookup_why. Resolve 'go' and re-check before trusting this either way; do NOT read it as absent."
      elif [[ -x "$lookup_dir/$t" ]]; then
        echo "- $t is at $lookup_dir/$t, not on PATH."
      else
        # Still as loud as it ever was. This message is why a missing binary
        # cannot be silently skipped, and it now carries where it looked, so
        # the next reader can check the answer rather than believe it.
        echo "- $t is NOT INSTALLED (looked in $lookup_dir). A gate that needs it must fail loudly, not be skipped."
      fi ;;
  esac
done
on_path="${on_path# }"
[[ -n "$on_path" ]] && echo "- on PATH: $on_path"

# Take a measurement, or say plainly that it could not be taken. Never let a
# failure print as a number that means something else.
#
# harness-reap.sh's sizeof() is the pattern - it prints "size unknown" rather
# than a blank column, because a blank size reads as zero and zero is the one
# thing it does not mean. A COUNT is the worse case: `git ... | wc -l` prints a
# perfectly plausible 0 when GIT ITSELF failed, 0 is indistinguishable from
# "there are none", and the caller below then dropped the whole section rather
# than admit it could not look.
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

# --- disk -------------------------------------------------------------------
# This is the warning that would have caught failure B - "No space left on
# device" with 803MB free - so it is the last one that may go quiet. It used to
# print nothing at all when both df forms failed, and `[[ "$avail_g" -lt 25 ]]`
# carried a `2>/dev/null` that swallowed bash's own error on a non-numeric
# value, silently skipping the warning. Both now say what happened instead.
avail_g=$(df -g / 2>/dev/null | awk 'NR==2{print $4}')
[[ -z "$avail_g" ]] && avail_g=$(df -BG / 2>/dev/null | awk 'NR==2{gsub(/G/,"",$4); print $4}')
if [[ "$avail_g" =~ ^[0-9]+$ ]]; then
  echo "- disk free: ${avail_g}Gi"
  [[ "$avail_g" -lt 25 ]] && echo "  WARNING: under 25Gi. Run 'make harness-reap' before starting a build."
else
  echo "- disk free: could not measure. Neither 'df -g /' nor 'df -BG /' gave a number (got '${avail_g}'), so the under-25Gi warning did NOT run this session. Check it by hand before a build: disk exhaustion has killed a build here mid-link."
fi

gocache=$(go env GOCACHE 2>/dev/null || true)
[[ -n "$gocache" && -d "$gocache" ]] && echo "- Go build cache: $(du -sh "$gocache" 2>/dev/null | cut -f1)"
gomod=$(go env GOMODCACHE 2>/dev/null || true)
[[ -n "$gomod" && -d "$gomod" ]] && echo "- Go module cache: $(du -sh "$gomod" 2>/dev/null | cut -f1)"

# --- worktrees --------------------------------------------------------------
# `git worktree list` always prints the main checkout first, so the agent
# worktrees are one fewer than the lines.
wt=$(countof git -C "$root" worktree list); wt_ok=$?
br=$(countof git -C "$root" branch --list 'worktree-*'); br_ok=$?
[[ $wt_ok -eq 0 ]] && wt=$(( wt > 0 ? wt - 1 : 0 ))
if [[ $wt_ok -ne 0 || $br_ok -ne 0 ]]; then
  # A repo that honestly has none says nothing at all, three lines down. These
  # two states must never read the same, which is the whole point of the change.
  echo "- agent worktrees: could not measure. git failed in '$root', so this session does not know how much disk the worktrees are holding. Run 'make harness-reap' to see the real state."
elif [[ "$wt" -gt 0 || "$br" -gt 0 ]]; then
  size=$(du -sh "$root/.claude/worktrees" 2>/dev/null | cut -f1)
  echo "- agent worktrees: ${wt} live (${size:-size unknown}), ${br} worktree-* branches"
  [[ "$br" -gt "$(( wt + 4 ))" ]] && echo "  $(( br - wt )) orphaned branches. 'make harness-reap' lists them; 'make harness-reap-apply' deletes the merged ones."
fi

# --- volumes ----------------------------------------------------------------
if command -v docker >/dev/null 2>&1; then
  vols=$(countof docker volume ls -q); vols_ok=$?
  if [[ $vols_ok -ne 0 ]]; then
    # Exactly the shape failure B produced: docker installed, daemon or
    # snapshotter erroring because the disk had already filled. Reporting that
    # as "0 volumes" hides the symptom at the moment it is most wanted.
    echo "- docker volumes: could not measure. docker is installed but did not answer, which is itself what a full disk does to it. Orphaned testcontainer volumes accumulate unseen; 'make harness-reap' reports them."
  elif [[ "$vols" -gt 10 ]]; then
    echo "- ${vols} docker volumes. Orphaned testcontainer volumes accumulate; 'make harness-reap' reports them."
  fi
fi

exit 0
