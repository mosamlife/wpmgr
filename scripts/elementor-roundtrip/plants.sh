#!/usr/bin/env bash
# scripts/elementor-roundtrip/plants.sh
#
# Proves the render probes can fail. For each defect listed in plants.txt it runs run.sh with that one plant
# (RT_PLANT), on every pinned Elementor and on the layouts the list names, and requires the run to come back
# red because of the plant:
#
#   - run.sh exits 1, a defect found. Exit 0 is a plant the probes did not notice, and exit 2 is a run that gave
#     no verdict: neither passes.
#   - every boot is red. A plant that one release renders as if nothing had been planted is a probe that cannot
#     fail there.
#   - every red boot counts a red probe (probes_failed above zero) and prints a failing check of the kind the
#     list names for the planted probe. A plant the harness could not apply (a probe or a setting it cannot find)
#     is red too, but it is not the probe noticing the plant, so it does not pass.
#   - every failing check of the run names the planted probe. A run that is red for another reason, or that the
#     plant also broke elsewhere, proves nothing about the plant.
#
# plants.txt holds one plant per line, '<plant token> <layouts> <check>': the token is plant=<kind>@<probe> as
# run.sh hands it to the harness; the layouts are those the probe has a tree for (containers, sections, or
# containers,sections), and a probe is skipped on a layout it has no tree for, so a plant listed there changes
# nothing and the run stays green, which is reported as a plant that went undetected; the check is the kind of
# failing check the plant must turn red on the probe, the word before the colon in 'rt: FAIL [..] css: ..'.
#
# Fails, never skips, when: run.sh cannot be run; awk, grep, sed, mktemp or tr cannot be found; the list is
# missing, lists no plant or holds a line that is not '<plant token> <layouts> <check>'; a run prints nothing,
# no boot count or fewer boot verdicts than boots; or a plant is not turned red as above. Every plant is run,
# and every one that fails is named, before the script exits.
#
# Exit status: 0 every plant turned the run red; 1 a plant did not (the probes cannot fail, or they fail for some
# other reason); 2 no verdict (a missing tool or list, a run that gave no verdict).
#
# RT_VERSIONS, RT_TIMEOUT, RT_CACHE and the other RT_* settings run.sh reads pass through to every run. RT_PLANT
# and RT_LAYOUTS are set for each run by this script, whatever they held.
#
# Test seam (used by scripts/elementor-roundtrip_test.sh only):
#   RT_PLANTS_FILE   the plant list (default: plants.txt beside this script)
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
run="$here/run.sh"
list="${RT_PLANTS_FILE:-$here/plants.txt}"

die() {
  echo "plants: $*" >&2
  exit 2
}

for tool in awk grep sed mktemp tr; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool not found"
done
[ -f "$run" ] || die "run.sh not found: $run"
[ -x "$run" ] || die "run.sh is not executable: $run"
[ -f "$list" ] || die "plants list missing: $list"

# --- the list --------------------------------------------------------------------------
token_re='^plant=[a-z_]+@[A-Za-z0-9][A-Za-z0-9._-]*$'
check_re='^[a-z][a-z-]*$'
tokens=()
lays=()
checks=()
n=0
while IFS= read -r line || [ -n "$line" ]; do
  n=$((n + 1))
  trimmed="${line#"${line%%[![:space:]]*}"}"
  case "$trimmed" in '' | '#'*) continue ;; esac
  read -r -a f <<<"$trimmed"
  [ "${#f[@]}" -eq 3 ] || die "$list line $n: want '<plant token> <layouts> <check>': '$trimmed'"
  [[ ${f[0]} =~ $token_re ]] || die "$list line $n: '${f[0]}' is not plant=<kind>@<probe>"
  [[ ${f[2]} =~ $check_re ]] || die "$list line $n: check '${f[2]}' is not a check name such as css"
  rest="${f[1]},"
  while [ -n "$rest" ]; do
    l="${rest%%,*}"
    rest="${rest#*,}"
    case "$l" in
      containers | sections) ;;
      *) die "$list line $n: layout '$l' is not containers or sections" ;;
    esac
  done
  tokens+=("${f[0]}")
  lays+=("${f[1]}")
  checks+=("${f[2]}")
done <"$list"
[ "${#tokens[@]}" -gt 0 ] || die "$list lists no plant"

# --- a work dir that is removed only when it is the one made here --------------------------
work="$(mktemp -d "${TMPDIR:-/tmp}/elementor-roundtrip-plants.XXXXXX")" || die "cannot create a work dir"
case "$work" in
  */elementor-roundtrip-plants.??????) ;;
  *) die "unexpected work dir: $work" ;;
esac
cleanup() {
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# --- the verdict on one plant ----------------------------------------------------------------
# judge <token> <check> <run.sh exit status> <the run's output file>
# Sets verdict (ok, defect or none: no verdict), boots, and problems, one entry for each thing wrong.
verdict=ok
boots=0
problems=()
judge() {
  local token="$1" check="$2" rc="$3" out="$4" probe tag verdicts nv line v l pf named unapplied stats other first
  probe="${token#*@}"
  tag="probe-$probe"
  verdict=ok
  boots=0
  problems=()

  if [ ! -s "$out" ]; then
    verdict=none
    problems+=("no verdict: run.sh printed nothing (exit $rc)")
    return
  fi
  case "$rc" in
    0 | 1) ;;
    *)
      verdict=none
      problems+=("no verdict: run.sh exited $rc")
      return
      ;;
  esac
  boots="$(sed -n -E 's/^elementor-roundtrip: ([0-9]+) boot\(s\), .*/\1/p' "$out" | tail -n 1)"
  if [ -z "$boots" ] || [ "$boots" -eq 0 ]; then
    boots=0
    verdict=none
    problems+=("no verdict: run.sh printed no boot count (exit $rc)")
    return
  fi
  if [ "$rc" -eq 0 ]; then
    verdict=defect
    problems+=("the run passed with the plant in place (exit 0): the probes cannot fail")
    return
  fi

  # One verdict line for each boot.
  verdicts="$work/verdicts"
  grep -E '^elementor-roundtrip: Elementor [0-9][0-9.]* \((containers|sections)\) (OK|FAILED) in [0-9]+s' "$out" >"$verdicts"
  nv="$(grep -c '' "$verdicts")"
  if [ "$nv" -ne "$boots" ]; then
    verdict=none
    problems+=("no verdict: run.sh printed $nv boot verdict(s) for $boots boot(s)")
    return
  fi
  while IFS= read -r line; do
    v="${line#elementor-roundtrip: Elementor }"
    v="${v%% *}"
    l="${line#*\(}"
    l="${l%%\)*}"
    case "$line" in
      *") OK in "*)
        verdict=defect
        problems+=("the plant went undetected on Elementor $v ($l): the boot passed")
        ;;
      *)
        pf="$(printf '%s\n' "$line" | sed -n -E "s/.*probes_failed='([0-9]+)'.*/\1/p")"
        if [ -z "$pf" ] || [ "$pf" -eq 0 ]; then
          verdict=defect
          problems+=("Elementor $v ($l) is red but counts no red probe (probes_failed='$pf')")
        fi
        named="$(awk -v want="rt: FAIL [$v $l $tag] $check: " 'index($0, want) == 1 { n++ } END { print n + 0 }' "$out")"
        if [ "$named" -eq 0 ]; then
          verdict=defect
          problems+=("Elementor $v ($l) is red but the $check check of $tag did not fail")
        fi
        unapplied="$(awk -v want="rt: FAIL [$v $l $tag] plant: " 'index($0, want) == 1 { print; exit }' "$out")"
        if [ -n "$unapplied" ]; then
          verdict=defect
          problems+=("the harness could not apply the plant to $tag on Elementor $v ($l): $unapplied")
        fi
        ;;
    esac
  done <"$verdicts"

  # A failing check is the plant's only when it is a check of the planted probe, whatever else the run did.
  stats="$(awk -v want="$tag" '
    /^rt: FAIL / {
      t = $0
      if (!sub(/^rt: FAIL \[/, "", t)) { bad++; if (first == "") first = $0; next }
      sub(/\].*$/, "", t)
      k = split(t, w, " ")
      if (k != 3 || w[3] != want) { bad++; if (first == "") first = $0 }
    }
    END { print bad + 0; print first }
  ' "$out")"
  other="${stats%%$'\n'*}"
  if [ "$other" -gt 0 ]; then
    first="${stats#*$'\n'}"
    verdict=defect
    problems+=("$other failing check(s) that do not name $tag, first: $first")
  fi
}

# --- run every plant --------------------------------------------------------------------------
idx=0
total_boots=0
defects=0
nones=0
while [ "$idx" -lt "${#tokens[@]}" ]; do
  token="${tokens[$idx]}"
  lay="${lays[$idx]}"
  want_check="${checks[$idx]}"
  out="$work/out.$idx"
  echo "plants: running $token on $lay"
  RT_PLANT="$token" RT_LAYOUTS="${lay//,/ }" "$run" >"$out" 2>&1
  rc=$?
  judge "$token" "$want_check" "$rc" "$out"
  if [ "$verdict" = ok ]; then
    total_boots=$((total_boots + boots))
    echo "plants: ok   $token ($lay): red on all $boots boot(s) by its $want_check check, as it must"
    grep '^rt: FAIL ' "$out" | head -n 12 | sed 's/^/plants:   /'
  else
    if [ "$verdict" = none ]; then
      nones=$((nones + 1))
    else
      defects=$((defects + 1))
    fi
    for p in "${problems[@]}"; do
      echo "plants: FAIL $token ($lay): $p" >&2
    done
    echo "plants: the output of that run, last lines:" >&2
    tail -n 40 "$out" | sed 's/^/plants:   | /' >&2
  fi
  idx=$((idx + 1))
done

failed=$((defects + nones))
if [ "$failed" -ne 0 ]; then
  echo "plants: $failed of ${#tokens[@]} plant(s) did not turn the run red ($defects not noticed or red for another reason, $nones without a verdict)" >&2
  if [ "$nones" -ne 0 ]; then
    exit 2
  fi
  exit 1
fi
echo "plants: ${#tokens[@]} plant(s), $total_boots boot(s): every plant turned the run red"
exit 0
