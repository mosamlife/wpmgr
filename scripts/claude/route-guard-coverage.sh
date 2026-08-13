#!/usr/bin/env bash
# Measures how much real work the route guard would interrupt.
#
# "Prove it fires, then prove it does not over-fire" is only half a rule without
# a number attached, and the over-fire half is the half that gets skipped. The
# first version of this guard would have prompted on nearly every tracked file
# and on nearly every file touched in the preceding month. A guard that cries
# wolf gets switched off, which is the failure this harness exists to prevent,
# so the over-fire rate is measured rather than asserted.
#
# The figures are not written down here. Run it: the body below derives them
# from the repository as it stands and prints them, and fails loudly rather than
# reporting a rate it could not measure. Two copies of one number is how a
# number goes stale, and this file already carried a figure that had drifted
# from the one in docs/harness.md.
#
# REPORT ONLY. This is deliberately NOT wired into ci.yml. Its input is the last
# N days of commits, which changes every time anything merges, so gating on it
# would redden main with zero code change - the same failure mode govulncheck
# already has here. The deterministic behaviour is pinned in
# scripts/claude/guards_test.sh instead, against a synthetic repo.
#
#   scripts/claude/route-guard-coverage.sh                 last 30 days
#   scripts/claude/route-guard-coverage.sh --since '7 days ago'
#   scripts/claude/route-guard-coverage.sh --all           every tracked file
set -uo pipefail

guard_override=""
since='30 days ago'
mode=recent
while [[ $# -gt 0 ]]; do
  case "$1" in
    --since) since="${2:?--since needs a date}"; shift 2 ;;
    --all)   mode=all; shift ;;
    --sessions) mode=sessions; shift ;;
    # Point at another copy of the guard to compare two versions on one window.
    --guard) guard_override="${2:?--guard needs a path}"; shift 2 ;;
    # Print the whole comment header, however long it grows, and stop at the
    # first line of code. The fixed line range this replaces was coupled to the
    # header's length: it spilled four lines of code into the help text, and
    # stopped only because a later comment edit happened to grow the header by
    # exactly the right number of lines. Same form as harness-reap.sh.
    # The `NR==1` arm drops the shebang, which is a comment line to awk and was
    # therefore printed as the first line of the help text. Same expression as
    # harness-reap.sh, quickstart-selfhost.sh and init-env.sh; all four are
    # asserted identical in guards_test.sh.
    -h|--help) awk 'NR==1 && /^#!/ {next} !/^#/{exit} {print}' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

root=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "not in a git repo" >&2; exit 1; }
root=$(cd "$root" && pwd -P)
guard="${guard_override:-$root/scripts/claude/route-guard.sh}"
[[ -x "$guard" ]] || [[ -f "$guard" ]] || { echo "no route-guard.sh at $guard" >&2; exit 1; }

# A measurement that silently measures nothing is the defect this repo keeps
# shipping, so every way of ending up with an empty file list is fatal.
command -v jq >/dev/null 2>&1 || { echo "FAIL: jq is not installed; the guard fails open and this would report 0%" >&2; exit 1; }

errlog=$(mktemp) || exit 1
trap 'rm -f "$errlog"' EXIT

# ---- "the guard declined" is not "the guard failed" ------------------------
# Both arms below used to collapse those two into one, with
#   ... | bash "$guard" 2>/dev/null | jq -r '... // "pass"'   and   "${d:-pass}"
# so a guard that crashed, that printed a parse error, or that answered nothing
# at all was scored as a deliberate PASS on every file - a perfect 0.0% over-fire
# rate for a guard that is not running, exit 0, in both arms. That number gets
# pasted into docs/harness.md as evidence that the guard is bearable, which is
# the decision this script exists to inform, so scoring a dead guard is worse
# than not measuring at all.
#
# Declined  = exit 0 and no output. That is the guard's real "pass" protocol.
# Failed    = a non-zero exit, or output that carries no permissionDecision.
#             Fatal, named, and non-zero out of this script.
guard_decision() { # guard_decision <payload> <label> -> prints a decision, or fails
  local out rc d
  out=$(printf '%s' "$1" | bash "$guard" 2>>"$errlog"); rc=$?
  if (( rc != 0 )); then
    printf 'FAIL: the guard exited %d on %s. A coverage number measured against a failing guard is not a coverage number.\n' "$rc" "$2" >&2
    [[ -s "$errlog" ]] && { echo "  last stderr:" >&2; tail -3 "$errlog" >&2; }
    return 1
  fi
  [[ -z "$out" ]] && { printf 'pass'; return 0; }
  d=$(jq -r '.hookSpecificOutput.permissionDecision // empty' <<<"$out" 2>/dev/null)
  if [[ -z "$d" ]]; then
    printf 'FAIL: the guard produced %d bytes on %s that carry no .hookSpecificOutput.permissionDecision:\n' "${#out}" "$2" >&2
    printf '%s\n' "${out:0:400}" >&2
    return 1
  fi
  printf '%s' "$d"
}

# ---- is the guard alive at all? --------------------------------------------
# The check above catches a guard that crashes or babbles. It cannot catch the
# one that this script was actually measuring - a guard that reads its payload,
# decides nothing and exits 0, which is indistinguishable from a pass on any
# single file. So the guard is asked three questions whose answers are fixed by
# the routing table in CLAUDE.md rather than by any count, before a rate is
# reported:
#
#   a generated tree must deny, a Go control-plane file must ask, and a file on
#   no routed path must pass.
#
# All three, or no number. Two would not be enough: "deny everything" and "ask
# nothing" are both scored 0% over-fire by a two-question probe. The paths need
# not exist - the guard walks up to the nearest existing ancestor - so this does
# not rot when a directory is renamed, and it sends no session_id, so the
# guard's per-session memory cannot answer for it.
selftest() {
  local bad=0 want got
  probe() { # probe <expected> <repo-relative path>
    got=$(guard_decision "$(jq -n --arg p "$root/$2" --arg c "$root" '{cwd:$c, tool_input:{file_path:$p}}')" "self-test $2") || return 1
    [[ "$got" == "$1" ]] && return 0
    printf 'FAIL: self-test: the guard answered %q for %s, expected %q.\n' "$got" "$2" "$1" >&2
    return 1
  }
  probe deny apps/api/internal/db/sqlc/zz_coverage_selftest.go     || bad=1
  probe ask  apps/api/internal/zzcoverage/zz_coverage_selftest.go  || bad=1
  probe pass zz-coverage-selftest.txt                              || bad=1
  if (( bad )); then
    echo "FAIL: $guard is not routing. Refusing to report a rate for it - a dead guard measures 0% over-fire, which is the best possible number." >&2
    return 1
  fi
}
selftest || exit 1

# ---- session simulation ----------------------------------------------------
# The per-file rate above is stateless, so it measures the guard as if every
# write were the first one. What a person actually experiences is prompts per
# session, and the guard remembers a destination once it has been ruled on. This
# replays the same window with each COMMIT DAY treated as one session, which is
# the closest honest stand-in for "a day's work in one session".
if [[ "$mode" == sessions ]]; then
  simstate=$(mktemp -d) || exit 1
  simerr="$errlog"
  trap 'rm -rf "$simstate"; rm -f "$errlog"' EXIT
  # The guard memoises only inside a directory it can prove is its own, and the
  # proof is a marker file it writes when it creates its default one. A bare
  # `mktemp -d` carries no marker, so the guard refused this directory on every
  # call, remembered nothing, and this block reported the STATELESS rate under a
  # per-session heading. The marker is created here because that is what a real
  # session's state directory has; the guard is not weakened to accommodate a
  # measurement.
  : > "$simstate/.wpmgr-harness-state" \
    || { echo "FAIL: cannot write the state marker in $simstate" >&2; exit 1; }
  export WPMGR_ROUTE_GUARD_STATE="$simstate"
  export WPMGR_ROUTE_GUARD_TTL_MIN=100000   # one simulated day is one session

  writes=0; prompts=0; days=0; lastday=""
  while IFS= read -r line; do
    case "$line" in
      "D "*) d="${line#D }"
             [[ "$d" != "$lastday" ]] && { days=$((days+1)); lastday="$d"; }
             continue ;;
      "") continue ;;
    esac
    [[ -z "${lastday:-}" ]] && continue
    writes=$((writes+1))
    payload=$(jq -n --arg p "$root/$line" --arg c "$root" --arg s "sim-$lastday" \
                '{cwd:$c, session_id:$s, tool_input:{file_path:$p}}')
    dec=$(guard_decision "$payload" "$line") || exit 1
    if [[ "$dec" == "ask" ]]; then
      prompts=$((prompts+1))
      # settings.json wires PostToolUse to `route-guard.sh --record`, so what
      # records a ruling in a real session is the approved write running. A
      # replay that only calls the PreToolUse arm memoises nothing however valid
      # its state directory is, and measures the stateless guard again.
      if ! printf '%s' "$payload" | bash "$guard" --record >/dev/null 2>>"$simerr"; then
        echo "FAIL: the guard's --record arm failed on $line, so the memoisation this arm reports on is not happening." >&2
        exit 1
      fi
    fi
  done < <(git -C "$root" log --since="$since" --date=short --format='D %ad' --name-only -- .)

  (( writes > 0 )) || { echo "FAIL: no writes replayed. Refusing to report 0." >&2; exit 1; }

  # Two ways this replay can be measuring the stateless guard while printing a
  # per-session heading. Both are fatal: a wrong number here reads as evidence
  # that the guard is bearable, which is the decision it exists to inform.
  if grep -q 'asking every time' "$simerr"; then
    echo "FAIL: the guard refused this simulation's state directory, so nothing was memoised and the rate would be the stateless one:" >&2
    grep -m1 'asking every time' "$simerr" >&2
    exit 1
  fi
  recorded=$(find "$simstate" -type f ! -name '.wpmgr-harness-state' 2>/dev/null | wc -l | tr -d '[:space:]')
  if (( prompts > 0 )) && (( recorded == 0 )); then
    echo "FAIL: $prompts prompts replayed but nothing was recorded under $simstate, so the guard is not memoising and this is the stateless rate. Refusing to report it per session." >&2
    exit 1
  fi

  echo "route-guard session simulation: commits since '$since', one session per commit day"
  echo "  simulated sessions : $days"
  echo "  writes replayed    : $writes"
  echo "  rulings memoised   : $recorded"
  echo "  prompts            : $prompts ($(awk -v n="$prompts" -v d="$writes" 'BEGIN{printf "%.1f", 100*n/d}')%)"
  echo "  prompts per session: $(awk -v n="$prompts" -v d="$days" 'BEGIN{printf "%.1f", (d==0?0:n/d)}')"
  exit 0
fi

# bash 3.2 is what macOS ships and what this runs on, so no `mapfile`.
files=()
if [[ "$mode" == all ]]; then
  label="every tracked file"
  while IFS= read -r f; do files+=("$f"); done < <(git -C "$root" ls-files)
else
  label="files changed since '$since'"
  while IFS= read -r f; do files+=("$f"); done < <(
    git -C "$root" log --since="$since" --name-only --pretty=format: -- . \
      | sed '/^$/d' | sort -u)
fi

total=${#files[@]}
(( total > 0 )) || { echo "FAIL: no files matched ($label). Refusing to report 0%." >&2; exit 1; }

ask=0; deny=0; passn=0
declare -a asked=()
for f in "${files[@]}"; do
  d=$(guard_decision "$(jq -n --arg p "$root/$f" --arg c "$root" '{cwd:$c, tool_input:{file_path:$p}}')" "$f") || exit 1
  case "$d" in
    ask)  ask=$((ask+1));  asked+=("$f") ;;
    deny) deny=$((deny+1)) ;;
    pass) passn=$((passn+1)) ;;
    # Not `*)`. A decision this script does not know how to score is not a pass:
    # `allow` would be counted as silence, and a future fourth value would be
    # counted as whatever the last case happened to be.
    *)    echo "FAIL: the guard answered '$d' on $f, which this script cannot score." >&2; exit 1 ;;
  esac
done

pct() { awk -v n="$1" -v d="$2" 'BEGIN{ printf "%.1f", (d==0 ? 0 : 100*n/d) }'; }

echo "route-guard coverage: $label"
echo "  files measured : $total"
echo "  ask            : $ask ($(pct "$ask" "$total")%)"
echo "  deny           : $deny ($(pct "$deny" "$total")%)"
echo "  pass           : $passn ($(pct "$passn" "$total")%)"
echo ""
echo "top directories that would prompt:"
if (( ask > 0 )); then
  printf '%s\n' "${asked[@]}" | awk -F/ '{ print (NF>2 ? $1"/"$2"/"$3 : $0) }' \
    | sort | uniq -c | sort -rn | head -12 | sed 's/^/  /'
else
  echo "  (none)"
fi
