#!/usr/bin/env bash
# scripts/elementor-roundtrip_test.sh
#
# Regression suite for scripts/elementor-roundtrip/run.sh and harness.php. It
# proves the round trip goes red on a missing tool, on a bad or missing pin, on a
# download that does not match its sha256 (and on a cached file that has been
# tampered with), when the Playground CLI fetches a WordPress of its own, on a
# boot that never answers, on a run that prints no verdict, a verdict without
# cases, a verdict that ran no render probe or does not say how many were red, an
# OK that counts a red probe, a verdict its exit status contradicts, or a crash
# (and that a red probe is a defect, exit 1, never a missing verdict); and, against a real
# WordPress under the Playground CLI, on a golden fixture with no cases, on a
# mounted zip that is not the pinned file, and on each planted defect: an
# unregistered widget type (which Elementor drops without saying so), a mapper
# whose tree no longer equals its golden tree, a rendered script element, a
# rendered on* attribute and an escaped text that is shown wrongly. For the
# agent's own create path it proves the check goes red on a verdict that ran no
# agent case, no refusal scenario, or another layout than the one booted, on an
# agent path handed no outline, and on each of five defects planted in a copy of
# the agent's own source (its verify skipped, an undo that says reverted and
# leaves the post, its digest re-check skipped, the container layout read as off,
# its undo guard skipped), each on the check meant to see it. On the agent's edit
# path it proves the render checks hold the page to the texts and links still on
# it after the call: a text or a link that stays and goes missing is red, a case
# with no after tree is still held to everything its operations wrote, and an
# after tree that leaves none of the written texts on the page is red. For the
# render probes it proves the check goes red on a probe whose expected declaration
# is edited, whose absent declaration is there, or whose id or tag is not on the
# page, on a probe file that cannot be used and on a phase handed no probe; for the
# pattern and page-frame probes, on a link without its nofollow, an accordion without
# its FAQ schema, a widget registered when the probe says it is not, a selector the
# harness cannot read, another theme or font family than the page has, and the
# opposite of a recorded false fact; and for the plants that act on a probe's tree,
# on a misspelt setting key, a group control without its activator, a plant that
# names no probe and a plant whose target is not in the tree. It also
# proves what it must NOT block: an honest run, a probe whose second alternative
# holds, a probe that applies from a later release (skipped out loud), a plant that
# leaves every other probe green, a cached download, a run of one
# version or one layout out of several, a blueprint that sets some other site
# option, and, in the planted run, every case that carries no plant and the
# whole agent path. Run it BEFORE the real check so a guard that fails open
# cannot pass.
#
# plants.sh, which CI runs after the round trip, plants a defect in a render probe's tree once for each
# plant in plants.txt and requires every run to come back red because of that plant. Against the stand-in
# it is proven to pass plants the probes notice, on the pins and layouts the list names and no others,
# and to fail on a plant the run passes, a plant one pin passes, a run that is red on another probe or
# on two, a red verdict with no failing check of the probe or no red probe in its tally, a run that gives
# no verdict or prints nothing or no boot count, a list that is missing, empty or malformed, and a run.sh
# that is missing or cannot be run. The committed list is held to render-probes.json.
#
# The first group runs run.sh against a stand-in for npx and file:// pins, and
# needs no network and no Playground. The second group boots a real WordPress
# under the Playground CLI for Elementor 4.3.4 (a few boots), downloads what the
# pins name on a first run and reuses the same cache after that; choose the cache
# with RT_CACHE. RT_TEST_ONLINE=0 skips the second group and says so.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
check="$here/elementor-roundtrip/run.sh"
tmp="$(mktemp -d -t elementor-roundtrip-test.XXXXXX)"
trap 'rm -rf "$tmp"' EXIT

# A developer's own seams must not leak into the cases below.
unset RT_NPX RT_NODE RT_PINS_FILE RT_BLUEPRINT RT_VERSIONS RT_FIXTURES_DIR RT_AGENT_INCLUDES
unset RT_PLANT RT_HARNESS_ARGS RT_TIMEOUT RT_TEST_KILL_SETTLE RT_ALLOW_FILE_URLS RT_LAYOUTS FAKE_MODE FAKE_ARGS_FILE FAKE_FAIL_VERSION FAKE_FAIL_LAYOUT
unset RT_PLANTS_FILE FAKE_CALLS_FILE FAKE_PLANT_LAYOUTS FAKE_PLANT_IGNORED_VERSION
real_cache="${RT_CACHE:-}"
unset RT_CACHE

pass=0
failed=0
skipped=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { failed=$((failed + 1)); echo "FAIL $1"; }

# expect <name> <want: 0|1|2|nonzero> <env...>   runs the check; output in LAST_OUT, status in LAST_RC.
expect() {
  local name="$1" want="$2"
  shift 2
  local out rc
  out="$(env "$@" "$check" 2>&1)"
  rc=$?
  LAST_OUT="$out"
  LAST_RC=$rc
  if [ "$want" = nonzero ] && [ "$rc" -ne 0 ]; then ok "$name"
  elif [ "$rc" = "$want" ]; then ok "$name"
  else bad "$name (exit $rc, wanted $want)"; echo "$out" | sed 's/^/     | /' | tail -15
  fi
}

# said <name> <text>   LAST_OUT must contain the text: a red must be red for the
# right reason, not because something else broke.
said() {
  case "$LAST_OUT" in
    *"$2"*) ok "$1" ;;
    *) bad "$1 (expected output to contain: $2)"; echo "$LAST_OUT" | sed 's/^/     | /' | tail -15 ;;
  esac
}

# not_said <name> <text>   LAST_OUT must not contain the text.
not_said() {
  case "$LAST_OUT" in
    *"$2"*) bad "$1 (output contains: $2)"; echo "$LAST_OUT" | sed 's/^/     | /' | tail -15 ;;
    *) ok "$1" ;;
  esac
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else echo "FAIL setup: no sha256 tool" >&2; exit 1
  fi
}

real_wp_url="https://downloads.wordpress.org/release/wordpress-6.8.zip"
grep -q "\"$real_wp_url\"" "$here/elementor-roundtrip/blueprint.json" || { echo "FAIL setup: blueprint.json no longer names $real_wp_url"; exit 1; }

# ---------------------------------------------------------------------------------
# Group 1: run.sh against a stand-in for npx and file:// pins
# ---------------------------------------------------------------------------------
mkdir -p "$tmp/dl"
printf 'not a wordpress, a stand-in\n' >"$tmp/dl/wordpress-6.8.zip"
printf 'not elementor 4.3.4, a stand-in\n' >"$tmp/dl/elementor.4.3.4.zip"
printf 'not elementor 3.20.4, a stand-in\n' >"$tmp/dl/elementor.3.20.4.zip"
wp_sha="$(sha256_of "$tmp/dl/wordpress-6.8.zip")"
e434_sha="$(sha256_of "$tmp/dl/elementor.4.3.4.zip")"
e3204_sha="$(sha256_of "$tmp/dl/elementor.3.20.4.zip")"
zero_sha="0000000000000000000000000000000000000000000000000000000000000000"

wp_file_url="file://$tmp/dl/wordpress-6.8.zip"
sed "s#$real_wp_url#$wp_file_url#" "$here/elementor-roundtrip/blueprint.json" >"$tmp/blueprint-good.json"

# pins <file> <wordpress sha> <4.3.4 sha> <3.20.4 sha>
pins() {
  {
    echo "# stand-in pins"
    echo "wordpress 6.8 $2 $wp_file_url"
    echo "elementor 3.20.4 $4 file://$tmp/dl/elementor.3.20.4.zip strings"
    echo "elementor 4.3.4 $3 file://$tmp/dl/elementor.4.3.4.zip as_given"
  } >"$1"
}
pins "$tmp/pins-good.txt" "$wp_sha" "$e434_sha" "$e3204_sha"
pins "$tmp/pins-bad-elementor.txt" "$wp_sha" "$zero_sha" "$e3204_sha"
pins "$tmp/pins-bad-wordpress.txt" "$zero_sha" "$e434_sha" "$e3204_sha"

# The stand-in for npx: records what it was given, then behaves as FAKE_MODE says.
cat >"$tmp/fake-npx" <<'FAKE'
#!/usr/bin/env bash
out="${FAKE_ARGS_FILE:-/dev/null}"
printf '%s\n' "$@" >"$out"
ver=""
layout=""
bp=""
zipsdir=""
plant=""
for a in "$@"; do
  case "$a" in
    elementor=*) ver="${a#elementor=}" ;;
    layout=*) layout="${a#layout=}" ;;
    plant=*) plant="${plant:+$plant }$a" ;;
    --blueprint=*) bp="${a#--blueprint=}" ;;
    --mount=*:/rt/zips) zipsdir="${a#--mount=}"; zipsdir="${zipsdir%:/rt/zips}" ;;
  esac
done
# The blueprint is a file of the run's own directory that is gone when the run ends: keep a copy per layout.
if [ -n "$bp" ] && [ -f "$bp" ] && [ "$out" != /dev/null ]; then
  cp "$bp" "$out.blueprint.$layout"
fi
if [ -n "$zipsdir" ] && [ -f "$zipsdir/elementor.zip" ]; then
  if command -v sha256sum >/dev/null 2>&1; then h="$(sha256sum "$zipsdir/elementor.zip")"; else h="$(shasum -a 256 "$zipsdir/elementor.zip")"; fi
  echo "mounted-zip-sha256=${h%% *}" >>"$out"
fi
for l in "$HOME"/.wordpress-playground/*; do
  [ -L "$l" ] && echo "cache-link=$(basename "$l") -> $(readlink "$l")" >>"$out"
done
tail_ok="cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 designed=2 probes=4 probes_failed=0"
good="rt: SUMMARY elementor=$ver layout=$layout $tail_ok failed=0"
# One line per boot: the release, the layout and the plant it was handed.
if [ -n "${FAKE_CALLS_FILE:-}" ]; then
  printf '%s %s%s\n' "$ver" "$layout" "${plant:+ $plant}" >>"$FAKE_CALLS_FILE"
fi
case "${FAKE_MODE:-ok}" in
  # The planted defects. A plant reaches the harness as plant=<kind>@<probe>, and the modes below say what the
  # harness does with it. FAKE_PLANT_LAYOUTS names the layouts the probe has a tree for (default containers): on
  # another layout the probe is skipped and the plant changes nothing. FAKE_PLANT_IGNORED_VERSION names a release
  # that renders the planted tree as if nothing had been planted.
  plantred | plantwrong | plantcollateral | plantnofail | plantnotally | plantfailothercase | plantnotapplied | plantotherkind)
    probe="${plant#*@}"
    probe="${probe%% *}"
    applies=0
    if [ -n "$plant" ]; then
      case " ${FAKE_PLANT_LAYOUTS:-containers} " in *" $layout "*) applies=1 ;; esac
      [ "${FAKE_PLANT_IGNORED_VERSION:-}" != "$ver" ] || applies=0
    fi
    if [ "$applies" -eq 0 ]; then echo "$good"; echo "rt: RESULT OK"; exit 0; fi
    own="rt: FAIL [$ver $layout probe-$probe] css: css[1] element a1b0001 .elementor-icon-box-title: want color:#2B1D16 (has no color)"
    other="rt: FAIL [$ver $layout probe-smoke-heading-colour] css: css[0] element 4ead001 .elementor-heading-title: want color:#123456 (has no color)"
    red_one="cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 designed=2 probes=4 probes_failed=1"
    case "$FAKE_MODE" in
      plantred) echo "$own"; echo "rt: note [$ver $layout probe-$probe] about: a stand-in"; echo "rt: SUMMARY elementor=$ver layout=$layout $red_one failed=1" ;;
      plantwrong) echo "$other"; echo "rt: SUMMARY elementor=$ver layout=$layout $red_one failed=1" ;;
      plantcollateral) echo "$own"; echo "$other"; echo "rt: SUMMARY elementor=$ver layout=$layout ${red_one%probes_failed=1}probes_failed=2 failed=2" ;;
      plantnofail) echo "rt: SUMMARY elementor=$ver layout=$layout $red_one failed=1" ;;
      plantnotally) echo "$own"; echo "rt: SUMMARY elementor=$ver layout=$layout $tail_ok failed=1" ;;
      plantfailothercase) echo "rt: FAIL [$ver $layout $probe] stored: tree: want 1, got 2"; echo "rt: SUMMARY elementor=$ver layout=$layout $red_one failed=1" ;;
      # The harness cannot find the probe or the setting to plant on: it is red, on the probe's own tag, and no declaration failed.
      plantnotapplied) echo "rt: FAIL [$ver $layout probe-$probe] plant: no probe is named $probe"; echo "rt: SUMMARY elementor=$ver layout=$layout $red_one failed=1" ;;
      # The probe is red on a check other than the one the list names.
      plantotherkind) echo "rt: FAIL [$ver $layout probe-$probe] html-id: the rendered page carries id=\"x\" on 0 element(s), not one"; echo "rt: SUMMARY elementor=$ver layout=$layout $red_one failed=1" ;;
    esac
    echo "rt: RESULT FAIL"; exit 1 ;;
  ok)
    if [ "${FAKE_FAIL_VERSION:-}" = "$ver" ] && { [ -z "${FAKE_FAIL_LAYOUT:-}" ] || [ "$FAKE_FAIL_LAYOUT" = "$layout" ]; }; then
      echo "rt: FAIL [$ver containers y] stored: tree: want 1, got 2"
      echo "rt: SUMMARY elementor=$ver layout=$layout $tail_ok failed=1"
      echo "rt: RESULT FAIL"
      exit 1
    fi
    echo "$good"; echo "rt: RESULT OK"; exit 0 ;;
  silent) exit 0 ;;
  zero) echo "rt: SUMMARY elementor=$ver layout=$layout cases=0 checks=0 texts=0 alts=0 agent=2 refused=3 designed=2 probes=4 probes_failed=0 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  zeroagent) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=0 refused=3 designed=2 probes=4 probes_failed=0 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  norefusal) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=0 designed=2 probes=4 probes_failed=0 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  # The designed pages: the count of pages the agent's create path made and undid (or refused for want of containers).
  nodesigned) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 probes=4 probes_failed=0 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  zerodesigned) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 designed=0 probes=4 probes_failed=0 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  # The probe verdict: the count of probes that ran, and how many of them were red.
  noprobes) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 designed=2 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  zeroprobes) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 designed=2 probes=0 probes_failed=0 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  noprobetally) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 designed=2 probes=4 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  probesredok) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 designed=2 probes=4 probes_failed=1 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  probefail) echo "rt: FAIL [$ver $layout probe-smoke-heading-colour] css: no rule for element abc1234 declares color:#123456"; echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=3 designed=2 probes=4 probes_failed=1 failed=1"; echo "rt: RESULT FAIL"; exit 1 ;;
  wronglayout) if [ "$layout" = containers ]; then other=sections; else other=containers; fi; echo "rt: SUMMARY elementor=$ver layout=$other $tail_ok failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  nolayout) echo "rt: SUMMARY elementor=$ver $tail_ok failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  oldsummary) echo "rt: SUMMARY elementor=$ver cases=3 checks=9 texts=2 alts=1 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  nosummary) echo "rt: RESULT OK"; exit 0 ;;
  exit1ok) echo "$good"; echo "rt: RESULT OK"; exit 1 ;;
  okfail) echo "$good"; echo "rt: RESULT OK"; echo "rt: RESULT FAIL"; exit 1 ;;
  fail) echo "rt: FAIL [x containers y] stored: tree: want 1, got 2"; echo "rt: SUMMARY elementor=$ver layout=$layout $tail_ok failed=1"; echo "rt: RESULT FAIL"; exit 1 ;;
  crash) echo "PHP Fatal error: boom"; exit 255 ;;
  hang) sleep 30; exit 0 ;;
  termexit) exec sleep 30 ;;
  download) mkdir -p "$HOME/.wordpress-playground"; : >"$HOME/.wordpress-playground/custom-ffffffff.zip"; echo "$good"; echo "rt: RESULT OK"; exit 0 ;;
esac
FAKE
chmod +x "$tmp/fake-npx"

offline=(RT_ALLOW_FILE_URLS=1 "RT_PINS_FILE=$tmp/pins-good.txt" "RT_BLUEPRINT=$tmp/blueprint-good.json" "RT_NPX=$tmp/fake-npx")

# --- must not over-fire ---------------------------------------------------------------
expect "an honest run passes" 0 "${offline[@]}" "RT_CACHE=$tmp/c1" "FAKE_ARGS_FILE=$tmp/args1"
said "a green run says which versions and layouts ran" "4 boot(s), 2 version(s) x 2 layout(s)"
said "a green run says every version passed" "every version passed"
said "a green run carries the probe tally of each boot" "probes=4 probes_failed=0"
args="$(cat "$tmp/args1" 2>/dev/null || true)"
case "$args" in *"layout=sections"*) ok "the layout of the boot is passed to the harness" ;; *) bad "the layout is not passed to the harness: $args" ;; esac
# Each layout boots a site whose container experiment was set in the blueprint before it started.
bp_containers="$(cat "$tmp/args1.blueprint.containers" 2>/dev/null || true)"
bp_sections="$(cat "$tmp/args1.blueprint.sections" 2>/dev/null || true)"
case "$bp_containers" in *'"elementor_experiment-container": "active"'*) ok "the containers boot sets the container experiment active" ;; *) bad "the containers blueprint does not set the experiment active: $bp_containers" ;; esac
case "$bp_sections" in *'"elementor_experiment-container": "inactive"'*) ok "the sections boot sets the container experiment inactive" ;; *) bad "the sections blueprint does not set the experiment inactive: $bp_sections" ;; esac
case "$bp_sections" in *'"installPlugin"'*'"setSiteOptions"'*) ok "  and the option is set after Elementor is installed" ;; *) bad "  the option is not set after the install step: $bp_sections" ;; esac
case "$args" in *"@wp-playground/cli@3.1.54"*) ok "the Playground CLI is run at its pinned version" ;; *) bad "the Playground CLI is not run at its pinned version: $args" ;; esac
case "$args" in *"--wp=$wp_file_url"*) ok "the WordPress url is the pinned one" ;; *) bad "the WordPress url is not the pinned one" ;; esac
case "$args" in *"/rt/agent/includes"*"/rt/fixtures"*"/rt/harness"*) ok "the agent code, the golden fixtures and the harness are mounted" ;; *) bad "a mount is missing" ;; esac
case "$args" in *"stored=as_given"*) ok "the stored form of the version is passed to the harness" ;; *) bad "the stored form is not passed" ;; esac
case "$args" in *"mounted-zip-sha256=$e434_sha"*) ok "the mounted Elementor zip is the pinned file" ;; *) bad "the mounted Elementor zip is not the pinned file" ;; esac
case "$args" in *"cache-link=custom-"*"-> $tmp/c1/zips/$wp_sha.zip"*) ok "the CLI's WordPress cache entry is a link to the verified zip" ;; *) bad "the CLI's WordPress cache entry is not a link to the verified zip: $args" ;; esac

expect "a cached download is used without fetching again" 0 "${offline[@]}" "RT_CACHE=$tmp/c1" RT_VERSIONS=4.3.4
mv "$tmp/dl" "$tmp/dl-away"
expect "a verified cache needs no network" 0 "${offline[@]}" "RT_CACHE=$tmp/c1" RT_VERSIONS=4.3.4
mv "$tmp/dl-away" "$tmp/dl"
expect "one version out of several can be chosen" 0 "${offline[@]}" "RT_CACHE=$tmp/c1" RT_VERSIONS=3.20.4 "FAKE_ARGS_FILE=$tmp/args2"
said "  and only that version ran" "2 boot(s), 1 version(s) x 2 layout(s)"
expect "one layout out of two can be chosen" 0 "${offline[@]}" "RT_CACHE=$tmp/c1" RT_VERSIONS=4.3.4 RT_LAYOUTS=sections "FAKE_ARGS_FILE=$tmp/args2b"
said "  and only that layout booted" "1 boot(s), 1 version(s) x 1 layout(s)"
case "$(cat "$tmp/args2b")" in *"layout=sections"*) ok "  and it is the one the harness was told" ;; *) bad "  the harness was not told the chosen layout" ;; esac
if [ -e "$tmp/args2b.blueprint.containers" ]; then bad "  the layout that was not chosen booted anyway"; else ok "  and the other layout did not boot"; fi
case "$(cat "$tmp/args2")" in *"stored=strings"*) ok "  and its own stored form was passed" ;; *) bad "  its own stored form was not passed" ;; esac

# --- must go red: a tool that cannot be found -------------------------------------------
mkdir -p "$tmp/bin-all"
setup_failed=0
for t in env dirname cat grep sed awk cut tail head mkdir mktemp rm cp mv ln find pkill sleep date curl node npx tr; do
  p="$(type -P "$t" || true)"
  if [ -z "$p" ] || ! ln -sf "$p" "$tmp/bin-all/$t"; then
    echo "FAIL setup: cannot link $t into the private PATH"
    setup_failed=1
  fi
done
for t in bash sha256sum shasum sha1sum; do
  p="$(type -P "$t" || true)"
  [ -z "$p" ] || ln -sf "$p" "$tmp/bin-all/$t"
done
[ -e "$tmp/bin-all/sha256sum" ] || [ -e "$tmp/bin-all/shasum" ] || { echo "FAIL setup: no sha256 tool to link"; setup_failed=1; }
[ -e "$tmp/bin-all/bash" ] || { echo "FAIL setup: cannot link bash"; setup_failed=1; }
if [ "$setup_failed" -ne 0 ]; then
  exit 1
fi
# without_path <tool...>: a PATH holding everything except the named tools.
without_path() {
  local d="$tmp/bin-without-$1" t
  rm -rf "$d"
  mkdir -p "$d"
  for t in "$tmp"/bin-all/*; do
    case " $* " in *" $(basename "$t") "*) continue ;; esac
    ln -s "$(readlink "$t")" "$d/$(basename "$t")"
  done
  echo "$d"
}
online_env=(RT_ALLOW_FILE_URLS=1 "RT_PINS_FILE=$tmp/pins-good.txt" "RT_BLUEPRINT=$tmp/blueprint-good.json")
expect "a missing npx is red" 2 "${online_env[@]}" "PATH=$(without_path npx)" "RT_CACHE=$tmp/c2"
said "  and it names npx" "npx not found"
expect "a missing node is red" 2 "${online_env[@]}" "RT_NPX=$tmp/fake-npx" "PATH=$(without_path node)" "RT_CACHE=$tmp/c2"
said "  and it names node" "node not found"
expect "a missing curl is red" 2 "${online_env[@]}" "RT_NPX=$tmp/fake-npx" "PATH=$(without_path curl)" "RT_CACHE=$tmp/c2"
said "  and it names curl" "curl not found"
expect "no sha256 tool is red" 2 "${online_env[@]}" "RT_NPX=$tmp/fake-npx" "PATH=$(without_path sha256sum shasum)" "RT_CACHE=$tmp/c2"
said "  and it names the sha256 tool" "no sha256 tool found"
expect "a named npx that is not executable is red" 2 "${offline[@]}" "RT_NPX=$tmp/no-such-npx" "RT_CACHE=$tmp/c2"
said "  and it names npx" "npx not found"

# --- must go red: a hash that does not match ----------------------------------------------
expect "an Elementor zip that does not match its pin is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-bad-elementor.txt" "RT_CACHE=$tmp/c3" RT_VERSIONS=4.3.4
said "  and it says which file" "sha256 mismatch for Elementor 4.3.4"
expect "a WordPress zip that does not match its pin is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-bad-wordpress.txt" "RT_CACHE=$tmp/c4"
said "  and it says which file" "sha256 mismatch for WordPress 6.8"
ls "$tmp/c3/zips" >"$tmp/c3.ls" 2>&1
if grep -q "$zero_sha" "$tmp/c3.ls"; then bad "a download that failed its hash was kept in the cache"; else ok "a download that failed its hash is not kept in the cache"; fi
mkdir -p "$tmp/c5/zips"
cp "$tmp/dl/elementor.4.3.4.zip" "$tmp/c5/zips/$e434_sha.zip"
printf 'tampered' >>"$tmp/c5/zips/$e434_sha.zip"
expect "a cached zip that was altered is fetched again" 0 "${offline[@]}" "RT_CACHE=$tmp/c5" RT_VERSIONS=4.3.4 "FAKE_ARGS_FILE=$tmp/args5"
case "$(cat "$tmp/args5")" in *"mounted-zip-sha256=$e434_sha"*) ok "  and the mounted zip is the pinned file again" ;; *) bad "  the altered zip was mounted" ;; esac
printf 'tampered again' >>"$tmp/c5/zips/$e434_sha.zip"
sed 's#elementor.4.3.4.zip #elementor.4.3.4.gone.zip #' "$tmp/pins-good.txt" >"$tmp/pins-gone.txt"
expect "a cached zip that was altered is red when it cannot be fetched again" 2 "${offline[@]}" "RT_CACHE=$tmp/c5" RT_VERSIONS=4.3.4 "RT_PINS_FILE=$tmp/pins-gone.txt"
said "  and it says the download failed" "download failed for Elementor 4.3.4"

# --- must go red: pins and blueprint ---------------------------------------------------------
expect "a missing pins file is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/no-such-pins.txt" "RT_CACHE=$tmp/c6"
said "  and it says the file is missing" "pins file missing"
printf '# nothing\n' >"$tmp/pins-empty.txt"
expect "a pins file with no pins is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-empty.txt" "RT_CACHE=$tmp/c6"
said "  and it says what is missing" "pins has no wordpress line"
sed "s/$e434_sha/not-a-sha/" "$tmp/pins-good.txt" >"$tmp/pins-bad-sha.txt"
expect "a pin whose hash is not 64 hex characters is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-bad-sha.txt" "RT_CACHE=$tmp/c6"
said "  and it says what is wrong" "not 64 lowercase hex characters"
sed 's/ as_given$//' "$tmp/pins-good.txt" >"$tmp/pins-no-form.txt"
expect "an Elementor pin without a stored form is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-no-form.txt" "RT_CACHE=$tmp/c6"
said "  and it says what is wrong" "an elementor line is"
sed 's/ as_given$/ whatever/' "$tmp/pins-good.txt" >"$tmp/pins-odd-form.txt"
expect "an Elementor pin with an unknown stored form is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-odd-form.txt" "RT_CACHE=$tmp/c6"
said "  and it says what is wrong" "stored form 'whatever'"
grep -v '^elementor' "$tmp/pins-good.txt" >"$tmp/pins-no-elementor.txt"
expect "pins with no Elementor are red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-no-elementor.txt" "RT_CACHE=$tmp/c6"
said "  and it says what is missing" "pins has no elementor line"
grep -v '^wordpress' "$tmp/pins-good.txt" >"$tmp/pins-no-wp.txt"
expect "pins with no WordPress are red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-no-wp.txt" "RT_CACHE=$tmp/c6"
said "  and it says what is missing" "pins has no wordpress line"
{ cat "$tmp/pins-good.txt"; grep '^elementor 4.3.4' "$tmp/pins-good.txt"; } >"$tmp/pins-dup.txt"
expect "a version pinned twice is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-dup.txt" "RT_CACHE=$tmp/c6"
said "  and it says which" "lists elementor 4.3.4 twice"
sed 's#file://#http://#' "$tmp/pins-good.txt" >"$tmp/pins-http.txt"
expect "a pin that is not an https url is red" 2 "${offline[@]}" "RT_PINS_FILE=$tmp/pins-http.txt" "RT_CACHE=$tmp/c6"
said "  and it says what is wrong" "not plain https"
expect "a file:// pin is refused unless the test seam allows it" 2 "RT_PINS_FILE=$tmp/pins-good.txt" "RT_BLUEPRINT=$tmp/blueprint-good.json" "RT_NPX=$tmp/fake-npx" "RT_CACHE=$tmp/c6"
said "  and it says what is wrong" "not plain https"
sed 's#wordpress-6.8.zip"#wordpress-6.9.zip"#' "$here/elementor-roundtrip/blueprint.json" >"$tmp/blueprint-other-wp.json"
expect "a blueprint that boots another WordPress than the pin is red" 2 "${offline[@]}" "RT_BLUEPRINT=$tmp/blueprint-other-wp.json" "RT_CACHE=$tmp/c6"
said "  and it names both" "but the pins name"
printf '{"preferredVersions":{"php":"8.3","wp":"%s"},"steps":[]}\n' "$wp_file_url" >"$tmp/blueprint-no-install.json"
expect "a blueprint that installs no Elementor is red" 2 "${offline[@]}" "RT_BLUEPRINT=$tmp/blueprint-no-install.json" "RT_CACHE=$tmp/c6"
said "  and it says what is wrong" "must pin preferredVersions"
expect "versions chosen but none named is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c6" "RT_VERSIONS="
said "  and it says so" "names no versions"
expect "a version that is not pinned is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c6" RT_VERSIONS=9.9.9
said "  and it names the version" "Elementor 9.9.9 is not pinned"
expect "layouts chosen but none named is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c6" "RT_LAYOUTS="
said "  and it says so" "names no layouts"
expect "a layout the check does not know is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c6" RT_LAYOUTS=diagonal
said "  and it names the layout" "'diagonal' is not containers or sections"
install_step='{"step":"installPlugin","pluginData":{"resource":"vfs","path":"/rt/zips/elementor.zip"},"options":{"activate":true}}'
printf '{"preferredVersions":{"php":"8.3","wp":"%s"},"steps":[%s,{"step":"setSiteOptions","options":{"elementor_experiment-container":"inactive"}}]}\n' "$wp_file_url" "$install_step" >"$tmp/blueprint-sets-experiment.json"
printf '{"preferredVersions":{"php":"8.3","wp":"%s"},"steps":[%s,{"step":"setSiteOptions","options":{"blogname":"x"}}]}\n' "$wp_file_url" "$install_step" >"$tmp/blueprint-other-option.json"
expect "a blueprint that sets the container experiment itself is red" 2 "${offline[@]}" "RT_BLUEPRINT=$tmp/blueprint-sets-experiment.json" "RT_CACHE=$tmp/c6"
said "  and it says who sets it" "leave elementor_experiment-container to this script"
expect "a blueprint that sets some other site option is not blocked" 0 "${offline[@]}" "RT_BLUEPRINT=$tmp/blueprint-other-option.json" "RT_CACHE=$tmp/c6" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers
said "  and it booted" "1 boot(s), 1 version(s) x 1 layout(s)"
mkdir -p "$tmp/fx-missing"
expect "a missing golden fixture is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c6" "RT_FIXTURES_DIR=$tmp/fx-missing"
said "  and it says what is missing" "golden fixture missing"
expect "a cache dir that is not absolute is red" 2 "${offline[@]}" "RT_CACHE=relative/dir"
said "  and it says what is wrong" "absolute path"
expect "a timeout that is not a number is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c6" RT_TIMEOUT=soon
said "  and it says what is wrong" "RT_TIMEOUT must be"

# --- must go red: a run that gives no usable verdict ------------------------------------------
for mode in silent zero zeroagent norefusal nodesigned zerodesigned noprobes zeroprobes noprobetally probesredok wronglayout nolayout oldsummary nosummary exit1ok okfail crash; do
  expect "stand-in mode '$mode' gives no usable verdict and is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers "FAKE_MODE=$mode"
  said "  and it says there was no usable verdict ($mode)" "gave no usable verdict"
done
expect "a zero-case verdict is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=zero
said "  and it shows the case count it saw" "cases='0'"
expect "a verdict that ran no agent case is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=zeroagent
said "  and it shows the agent count it saw" "agent='0'"
expect "a verdict that ran no refusal scenario is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=norefusal
said "  and it shows the refusal count it saw" "refused='0'"
# The probe verdict. A boot whose harness ran no render probe looked at nothing the designed pages rely on, and a boot
# whose summary counts a red probe cannot be an OK, whatever the harness's own result line says.
expect "probe_verdict_required: a verdict with no probe count is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=noprobes
said "  and it shows the probe count it saw" "probes=''"
expect "probe_verdict_required: a verdict that ran zero probes is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=zeroprobes
said "  and it shows the probe count it saw" "probes='0'"
expect "probe_verdict_required: a verdict that does not say how many probes were red is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=noprobetally
said "  and it shows the tally it saw" "probes_failed=''"
expect "probe_red_is_defect: a verdict that counts a red probe and says OK is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=probesredok
said "  and it shows the tally it saw" "probes_failed='1'"
expect "probe_red_is_defect: a red probe is a defect found (exit 1), not a broken run" 1 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=probefail
said "  and it names the probe" "probe-smoke-heading-colour"
said "  and it says the version failed" "Elementor 4.3.4 (containers) FAILED"
not_said "  and it is not called a missing verdict" "gave no usable verdict"
expect "a verdict for the other layout is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=wronglayout
said "  and it says the layout did not match" "layout ok=0"
expect "a harness that does not say which layout it ran is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=nolayout
said "  and it says the layout did not match" "layout ok=0"
expect "the summary of a harness that never ran the agent path is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=oldsummary
said "  and it shows nothing was counted for the agent path" "agent=''"
expect "a defect the harness reports is exit 1, not a broken run" 1 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=fail
said "  and it says the version failed" "Elementor 4.3.4 (containers) FAILED"
expect "a defect in one version is red when the other passes" 1 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_LAYOUTS=containers FAKE_FAIL_VERSION=3.20.4
said "  and the version that passed is still reported" "Elementor 4.3.4 (containers) OK"
said "  and the version that failed is named" "Elementor 3.20.4 (containers) FAILED"
expect "a defect in one layout is red when the other passes" 1 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 FAKE_FAIL_VERSION=4.3.4 FAKE_FAIL_LAYOUT=sections
said "  and the layout that passed is still reported" "Elementor 4.3.4 (containers) OK"
said "  and the layout that failed is named" "Elementor 4.3.4 (sections) FAILED"
said "  and both layouts were run" "2 boot(s), 1 version(s) x 2 layout(s)"
expect "a boot that never answers is red within the timeout" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 FAKE_MODE=hang RT_TIMEOUT=2
said "  and it says how long it waited" "no result after 2s"
# A boot that dies the instant it is told to stop can be reaped before the check has read its
# status. The wait after the kill makes that certain; the verdict must still be the timeout.
expect "a boot that exits at once when it is stopped is a timeout, not a missing verdict" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=termexit RT_TIMEOUT=2 RT_TEST_KILL_SETTLE=1
said "  and it says how long it waited" "no result after 2s"
not_said "  and it does not call the timeout a missing verdict" "gave no usable verdict"
expect "the same boot without the wait after the kill is a timeout too" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=termexit RT_TIMEOUT=2
said "  and it says how long it waited" "no result after 2s"
expect "a wait after the kill that is not a number is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_TEST_KILL_SETTLE=soon
said "  and it says what is wrong" "RT_TEST_KILL_SETTLE must be"
expect "a CLI that downloads its own WordPress is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c8" RT_VERSIONS=4.3.4 FAKE_MODE=download
said "  and it says so" "downloaded a WordPress of its own"

# ---------------------------------------------------------------------------------
# The planted defects: scripts/elementor-roundtrip/plants.sh
# ---------------------------------------------------------------------------------
# plants.sh runs run.sh once per planted defect and requires that run to come back red because of that plant. Here
# run.sh runs against the stand-in for npx: its plantred mode is a harness whose probe notices the plant, and the
# other plant modes are the harnesses the script must not trust.
plants_script="$here/elementor-roundtrip/plants.sh"
plants_node="$(command -v node || true)"
[ -n "$plants_node" ] || { echo "FAIL setup: node not found (the committed plant list is read with it)"; exit 1; }
cat >"$tmp/plants-good.txt" <<'LIST'
# a stand-in list
plant=misspell_key@pattern-icon-box containers css

plant=drop_activator@pattern-icon-box containers css
LIST
plants_env=("${offline[@]}" "RT_CACHE=$tmp/c7" "RT_PLANTS_FILE=$tmp/plants-good.txt")
# plants_expect <name> <want: 0|1|2> <env...>   expect(), through plants.sh instead of run.sh.
plants_expect() {
  local name="$1" want="$2" saved="$check"
  shift 2
  check="$plants_script"
  expect "$name" "$want" "$@"
  check="$saved"
}

# --- must not over-fire: plants the probes notice --------------------------------------------
: >"$tmp/plant-calls"
plants_expect "plants_red_passes: plants that turn the run red on every pin pass" 0 "${plants_env[@]}" FAKE_MODE=plantred "FAKE_CALLS_FILE=$tmp/plant-calls" RT_PLANT=plant=render_script@list RT_LAYOUTS=sections
said "  and it names the first plant" "plant=misspell_key@pattern-icon-box"
said "  and it names the second plant" "plant=drop_activator@pattern-icon-box"
said "  and it counts what it ran" "2 plant(s), 4 boot(s): every plant turned the run red"
want_calls="3.20.4 containers plant=drop_activator@pattern-icon-box
3.20.4 containers plant=misspell_key@pattern-icon-box
4.3.4 containers plant=drop_activator@pattern-icon-box
4.3.4 containers plant=misspell_key@pattern-icon-box"
if [ "$(LC_ALL=C sort "$tmp/plant-calls")" = "$want_calls" ]; then
  ok "  and each plant ran alone, on every pinned version and only on the layout the list names, whatever RT_PLANT and RT_LAYOUTS held"
else
  bad "  the boots were not one per plant, version and listed layout: $(tr '\n' ';' <"$tmp/plant-calls")"
fi
: >"$tmp/plant-calls2"
plants_expect "plants_one_pin: RT_VERSIONS narrows a plant run to one pin" 0 "${plants_env[@]}" FAKE_MODE=plantred RT_VERSIONS=4.3.4 "FAKE_CALLS_FILE=$tmp/plant-calls2"
said "  and it counts the narrower run" "2 plant(s), 2 boot(s): every plant turned the run red"
if [ "$(LC_ALL=C sort "$tmp/plant-calls2" | tr '\n' ';')" = "4.3.4 containers plant=drop_activator@pattern-icon-box;4.3.4 containers plant=misspell_key@pattern-icon-box;" ]; then ok "  and only that pin booted"; else bad "  other pins booted: $(tr '\n' ';' <"$tmp/plant-calls2")"; fi
printf 'plant=misspell_key@pattern-icon-box containers,sections css\n' >"$tmp/plants-both.txt"
plants_expect "plants_both_layouts: a probe with a tree in both layouts is planted in both" 0 "${plants_env[@]}" "RT_PLANTS_FILE=$tmp/plants-both.txt" FAKE_MODE=plantred "FAKE_PLANT_LAYOUTS=containers sections"
said "  and it counts a boot per layout and pin" "1 plant(s), 4 boot(s): every plant turned the run red"
plants_expect "plants_committed_list: the committed list runs and every plant in it turns the stand-in red" 0 "${offline[@]}" "RT_CACHE=$tmp/c7" FAKE_MODE=plantred
said "  and it says every plant turned the run red" "every plant turned the run red"
# The committed list is held to the committed probes, without booting anything: each plant names a probe that exists,
# that declares the plant's target, that has a tree in every layout the list names, and that runs on every pin.
if listed="$("$plants_node" -e '
  const fs = require("fs");
  const [listFile, probesFile] = process.argv.slice(1);
  const probes = JSON.parse(fs.readFileSync(probesFile, "utf8")).probes;
  const lines = fs.readFileSync(listFile, "utf8").split("\n").map((l) => l.trim()).filter((l) => l !== "" && !l.startsWith("#"));
  const bad = [];
  for (const line of lines) {
    const [token, layouts = "", check = ""] = line.split(/\s+/);
    const m = /^plant=([a-z_]+)@(.+)$/.exec(token);
    if (!m) { bad.push(line + ": not a plant token"); continue; }
    const p = probes.find((x) => x.name === m[2]);
    if (!p) { bad.push(line + ": no probe is named " + m[2]); continue; }
    if (!p.plants || !p.plants[m[1]]) { bad.push(line + ": the probe declares no target for " + m[1]); }
    if (check === "") { bad.push(line + ": no check is named"); }
    if (check === "css" && !(Array.isArray(p.css) && p.css.length > 0)) { bad.push(line + ": the probe expects no declaration, so its css check cannot fail"); }
    if (p.since) { bad.push(line + ": the probe applies from Elementor " + p.since + ", older pins skip it"); }
    for (const l of layouts.split(",")) {
      if (!p.trees || !Array.isArray(p.trees[l])) { bad.push(line + ": the probe has no " + l + " tree"); }
    }
  }
  if (lines.length === 0) { bad.push("the list holds no plant"); }
  if (bad.length > 0) { console.error(bad.join("\n")); process.exit(1); }
  console.log(lines.length);
' "$here/elementor-roundtrip/plants.txt" "$here/elementor-roundtrip/render-probes.json" 2>"$tmp/plants-list.err")"; then
  ok "plants_list_matches_probes: the committed list names $listed plant(s), each on a probe that declares it, in layouts the probe has a tree for"
else
  bad "plants_list_matches_probes: the committed list does not match render-probes.json: $(cat "$tmp/plants-list.err")"
fi

# --- must go red: a plant the run does not turn red ----------------------------------------------
plants_expect "plants_green_fails: a plant the run does not notice fails the step" 1 "${plants_env[@]}" FAKE_MODE=ok
said "  and it says the run passed with the plant in place" "the run passed with the plant in place"
said "  and it names the plant" "plant=misspell_key@pattern-icon-box"
said "  and it still ran the second plant after the first one failed" "FAIL plant=drop_activator@pattern-icon-box"
said "  and it counts the plants that did not turn the run red" "2 of 2 plant(s) did not turn the run red"
not_said "  and it is not called a missing verdict" "no verdict"
plants_expect "plants_one_pin_green_fails: a plant one pinned version renders as if nothing happened fails the step" 1 "${plants_env[@]}" FAKE_MODE=plantred FAKE_PLANT_IGNORED_VERSION=3.20.4
said "  and it names the pin and the layout" "went undetected on Elementor 3.20.4 (containers)"
not_said "  and the pin that noticed is not blamed" "undetected on Elementor 4.3.4"
plants_expect "plants_skipped_layout_fails: a plant listed on a layout its probe has no tree for stays green there and fails the step" 1 "${plants_env[@]}" "RT_PLANTS_FILE=$tmp/plants-both.txt" FAKE_MODE=plantred
said "  and it names the layout" "went undetected on Elementor 4.3.4 (sections)"
# Red is not enough: it has to be the plant that turned the run red.
plants_expect "plants_wrong_probe_fails: a run that is red on another probe is not the plant noticed" 1 "${plants_env[@]}" FAKE_MODE=plantwrong
said "  and it says the plant's probe never failed" "the css check of probe-pattern-icon-box did not fail"
said "  and it says what else was red" "failing check(s) that do not name probe-pattern-icon-box"
plants_expect "plants_collateral_fails: a plant that also breaks another probe proves nothing" 1 "${plants_env[@]}" FAKE_MODE=plantcollateral
said "  and it says what else was red" "failing check(s) that do not name probe-pattern-icon-box"
not_said "  and the plant's own probe did fail" "check of probe-pattern-icon-box did not fail"
plants_expect "plants_nofail_fails: a red verdict with no failing check on the probe is not trusted" 1 "${plants_env[@]}" FAKE_MODE=plantnofail
said "  and it says the probe never failed" "the css check of probe-pattern-icon-box did not fail"
plants_expect "plants_notally_fails: a red probe the summary does not count is not trusted" 1 "${plants_env[@]}" FAKE_MODE=plantnotally
said "  and it says what the tally was" "counts no red probe (probes_failed='0')"
plants_expect "plants_other_case_fails: a failing case that only shares the probe's name is not the probe" 1 "${plants_env[@]}" FAKE_MODE=plantfailothercase
said "  and it says the probe never failed" "the css check of probe-pattern-icon-box did not fail"
said "  and it says what else was red" "failing check(s) that do not name probe-pattern-icon-box"
# A plant the harness cannot apply makes the probe red on its own tag, and that is the harness failing, not the probe noticing.
plants_expect "plants_not_applied_fails: a plant the harness could not apply is not a plant noticed" 1 "${plants_env[@]}" FAKE_MODE=plantnotapplied
said "  and it says the plant could not be applied" "the harness could not apply the plant to probe-pattern-icon-box on Elementor 3.20.4 (containers)"
said "  and it says the declaration check never failed" "the css check of probe-pattern-icon-box did not fail"
# The probe has to fail the check the list names: a red probe on some other check is not the declarations being held.
plants_expect "plants_other_check_fails: a probe that is red on another check than the one listed is not the plant noticed" 1 "${plants_env[@]}" FAKE_MODE=plantotherkind
said "  and it says the listed check never failed" "the css check of probe-pattern-icon-box did not fail"
printf 'plant=misspell_key@pattern-icon-box containers html-id\n' >"$tmp/plants-htmlid.txt"
plants_expect "plants_listed_check: the check the list names is the one the probe is held to" 0 "${plants_env[@]}" "RT_PLANTS_FILE=$tmp/plants-htmlid.txt" FAKE_MODE=plantotherkind
said "  and it names the check that turned red" "by its html-id check"

# --- must go red: a run that gives no verdict ------------------------------------------------------
for mode in silent crash; do
  plants_expect "plants_no_verdict_fails: stand-in mode '$mode' gives run.sh no verdict, so the step is red and not green" 2 "${plants_env[@]}" RT_VERSIONS=4.3.4 "FAKE_MODE=$mode"
  said "  and it says there was no verdict ($mode)" "no verdict"
  not_said "  and it does not say every plant turned the run red ($mode)" "every plant turned the run red"
done
plants_expect "plants_hang_fails: a boot that never answers is no verdict, not a plant noticed" 2 "${plants_env[@]}" RT_VERSIONS=4.3.4 FAKE_MODE=hang RT_TIMEOUT=2
said "  and it says there was no verdict" "no verdict"

# --- must go red: a list that cannot be used ---------------------------------------------------------
printf '# nothing\n\n' >"$tmp/plants-empty.txt"
plants_expect "plants_list_empty_fails: a list with no plant is red, not green" 2 "${plants_env[@]}" "RT_PLANTS_FILE=$tmp/plants-empty.txt"
said "  and it says so" "lists no plant"
plants_expect "plants_list_missing_fails: a missing list is red" 2 "${plants_env[@]}" "RT_PLANTS_FILE=$tmp/no-such-plants.txt"
said "  and it says so" "plants list missing"
# plants_list_bad <name> <line> <message>   a list holding that one line is red and says why.
plants_list_bad() {
  printf '%s\n' "$2" >"$tmp/plants-bad.txt"
  plants_expect "plants_list_bad: $1" 2 "${plants_env[@]}" "RT_PLANTS_FILE=$tmp/plants-bad.txt"
  said "  and it says what is wrong" "$3"
}
plants_list_bad "a line without layouts and a check is red" "plant=misspell_key@pattern-icon-box" "want '<plant token> <layouts> <check>'"
plants_list_bad "a line without a check is red" "plant=misspell_key@pattern-icon-box containers" "want '<plant token> <layouts> <check>'"
plants_list_bad "a line with a fourth column is red" "plant=misspell_key@pattern-icon-box containers css extra" "want '<plant token> <layouts> <check>'"
plants_list_bad "a token that is not plant=<kind>@<probe> is red" "misspell_key@pattern-icon-box containers css" "is not plant=<kind>@<probe>"
plants_list_bad "a plant with no probe is red" "plant=misspell_key@ containers css" "is not plant=<kind>@<probe>"
plants_list_bad "a layout the check does not know is red" "plant=misspell_key@pattern-icon-box diagonal css" "'diagonal' is not containers or sections"
plants_list_bad "an empty layout is red" "plant=misspell_key@pattern-icon-box containers,,sections css" "'' is not containers or sections"
plants_list_bad "a check that is not a check name is red" "plant=misspell_key@pattern-icon-box containers CSS:" "is not a check name"

# --- must go red, and must not over-fire: the verdict plants.sh reads is the run's own -----------------
# A copy of plants.sh beside a stand-in for run.sh, so the script is held to what it is handed and not to what the real
# run.sh happens to print.
plants_dir="$tmp/plants-dir"
mkdir -p "$plants_dir"
cp "$plants_script" "$plants_dir/plants.sh"
chmod +x "$plants_dir/plants.sh"
printf 'plant=misspell_key@pattern-icon-box containers css\n' >"$tmp/plants-one.txt"
# plants_standin <name> <want> <body>   plants.sh beside a run.sh that is <body>; an empty body means no run.sh at all.
plants_standin() {
  local name="$1" want="$2" body="$3" saved="$check"
  rm -f "$plants_dir/run.sh"
  if [ -n "$body" ]; then
    printf '#!/usr/bin/env bash\n%s\n' "$body" >"$plants_dir/run.sh"
    chmod +x "$plants_dir/run.sh"
  fi
  check="$plants_dir/plants.sh"
  expect "$name" "$want" "RT_PLANTS_FILE=$tmp/plants-one.txt"
  check="$saved"
}
red_line='echo "rt: FAIL [4.3.4 containers probe-pattern-icon-box] css: css[1] element a1b0001 .elementor-icon-box-title: want color:#2B1D16 (has no color)"'
note_line='echo "rt: note [4.3.4 containers probe-pattern-icon-box] about: a stand-in"'
boot_red="echo \"elementor-roundtrip: Elementor 4.3.4 (containers) FAILED in 3s (probes='4', probes_failed='1')\" >&2"
boots_one='echo "elementor-roundtrip: 1 boot(s), 1 version(s) x 1 layout(s), in 3s"'
plants_standin "plants_standin_honest: a run.sh that prints a red boot on the planted probe and exits 1 passes" 0 "$red_line; $note_line; $boot_red; $boots_one; exit 1"
said "  and it names the plant" "plant=misspell_key@pattern-icon-box"
plants_standin "plants_standin_exit_contradicts: the same lines with exit 0 do not pass" 1 "$red_line; $note_line; $boot_red; $boots_one; exit 0"
said "  and it says the run passed with the plant in place" "the run passed with the plant in place"
plants_standin "plants_standin_no_run: no run.sh is red" 2 ""
said "  and it says so" "run.sh not found"
plants_standin "plants_standin_silent_red: a run that prints nothing and exits 1 is no verdict" 2 "exit 1"
said "  and it says nothing was printed" "printed nothing"
plants_standin "plants_standin_silent_green: a run that prints nothing and exits 0 is no verdict, not a pass" 2 "exit 0"
said "  and it says nothing was printed" "printed nothing"
plants_standin "plants_standin_no_count: a red boot line without the boot count is no verdict" 2 "$red_line; $boot_red; exit 1"
said "  and it says the count is missing" "no boot count"
plants_standin "plants_standin_no_boots: a boot count without a verdict for the boots is no verdict" 2 "$red_line; $boots_one; exit 1"
said "  and it says the boots do not add up" "boot verdict(s) for 1 boot(s)"
plants_standin "plants_standin_exit_2: a run that gave no verdict is red" 2 "$red_line; $boot_red; $boots_one; exit 2"
said "  and it says no verdict" "no verdict"
chmod -x "$plants_dir/run.sh"
check="$plants_dir/plants.sh"
expect "plants_standin_not_executable: a run.sh that cannot be run is red" 2 "RT_PLANTS_FILE=$tmp/plants-one.txt"
said "  and it says so" "not executable"
check="$here/elementor-roundtrip/run.sh"

# ---------------------------------------------------------------------------------
# Group 1b: the planted defects in the agent's own source still apply to it
# ---------------------------------------------------------------------------------
# Group 2 plants each defect below in a copy of the agent's source and requires the real round trip to turn
# red. A defect whose anchor does not occur exactly once in that source plants nothing, so every anchor is
# held to the source here as well, with no WordPress: a change to the source that moves one turns this suite
# red at once, on any machine, whether or not the real round trip runs. Group 2 runs the same table.
node_bin="$(command -v node || true)"
[ -n "$node_bin" ] || { echo "FAIL setup: node not found (the agent mutations are applied with it)"; exit 1; }
# The designed-page probes keep the compiler's tree shape and name rendered anchors.
if probe_shape="$("$node_bin" -e '
  const fs = require("fs");
  const probes = JSON.parse(fs.readFileSync(process.argv[1], "utf8")).probes;
  const names = ["card-border-shadow", "logos-wrapping-row", "background-focal-top-bottom", "pattern-pricing-card", "testimonial-without-picture"];
  const errors = [];
  const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
  const checkSettings = (value, at) => {
    if (typeof value === "number") errors.push(at + ": numeric settings must be strings");
    if (typeof value === "string" && /^#/.test(value) && !/^#[0-9A-F]{6}([0-9A-F]{2})?$/.test(value))
      errors.push(at + ": hex colours must be upper case");
    if (value !== null && typeof value === "object")
      for (const [key, child] of Object.entries(value)) checkSettings(child, at + "." + key);
  };
  for (const name of names) {
    const matches = probes.filter((p) => p.name === name);
    if (matches.length !== 1) { errors.push(name + ": expected one probe"); continue; }
    const p = matches[0], ids = new Set(), anchors = [];
    if (typeof p.about !== "string" || p.about.trim() === "") errors.push(name + ": missing about");
    const walk = (nodes) => {
      if (!Array.isArray(nodes) || nodes.length === 0) { errors.push(name + ": missing containers tree"); return; }
      for (const n of nodes) {
        if (!/^[0-9a-f]{7}$/.test(n.id) || ids.has(n.id)) errors.push(name + ": invalid or duplicate element id " + n.id);
        ids.add(n.id);
        if (!object(n.settings)) errors.push(name + ": missing settings for " + n.id);
        checkSettings(n.settings, name + "." + n.id);
        if (n.settings?._element_id) anchors.push(n.settings._element_id);
        if (!Array.isArray(n.elements)) errors.push(name + ": missing elements for " + n.id);
        else if (n.elements.length) walk(n.elements);
      }
    };
    walk(p.trees?.containers);
    if (!Array.isArray(p.css) || p.css.length === 0) errors.push(name + ": missing positive CSS expectation");
    for (const exp of p.css || []) {
      const alternatives = exp.any || [exp.decl];
      if (!ids.has(exp.el) || typeof exp.sel !== "string" || !(exp.media === null || typeof exp.media === "string")
        || alternatives.length === 0 || alternatives.some((d) => !object(d) || Object.keys(d).length === 0))
        errors.push(name + ": malformed CSS expectation");
    }
    if (!Array.isArray(p.html_ids) || p.html_ids.length === 0 || p.html_ids.some((id) => anchors.filter((a) => a === id).length !== 1))
      errors.push(name + ": html_ids must name anchors in the tree");
  }
  if (errors.length) { console.error(errors.join("\n")); process.exit(1); }
  console.log(names.join(", "));
' "$here/elementor-roundtrip/render-probes.json" 2>"$tmp/probe-shape.err")"; then
  ok "designed_probe_shape: $probe_shape"
else
  bad "designed_probe_shape: $(cat "$tmp/probe-shape.err")"
fi

# An empty testimonial media control keeps both empty strings and expects no picture markup.
if "$node_bin" -e '
  const fs = require("fs"), assert = require("assert/strict");
  const probes = JSON.parse(fs.readFileSync(process.argv[1], "utf8")).probes;
  const p = probes.find((p) => p.name === "testimonial-without-picture");
  assert.ok(p, "missing testimonial-without-picture probe");
  const nodes = (tree) => tree.flatMap((n) => [n, ...nodes(n.elements)]);
  const widgets = nodes(p.trees.containers).filter((n) => n.widgetType === "testimonial");
  assert.equal(widgets.length, 1, "expected one testimonial widget");
  const w = widgets[0];
  assert.deepEqual(w.settings.testimonial_image, { url: "", id: "" });
  assert.ok(p.css.some((e) => e.el === w.id && e.sel === ".elementor-testimonial-content" && e.decl?.color),
    "missing testimonial CSS positive control");
  assert.ok(p.html_has.some((e) => e.sel === ".elementor-testimonial-content" && e.text === w.settings.testimonial_content),
    "missing visible testimonial text");
  for (const sel of [".elementor-testimonial-image", "img", "picture"])
    assert.ok(p.html_has.some((e) => e.sel === sel && e.count === 0), "missing absence expectation for " + sel);
' "$here/elementor-roundtrip/render-probes.json" 2>"$tmp/testimonial-probe.err"; then
  ok "testimonial_without_picture: empty media value and no picture expectations"
else
  bad "testimonial_without_picture: $(cat "$tmp/testimonial-probe.err")"
fi

# mutate <name> <file under includes> <old> <new> [<old> <new> ...]: each <old> must occur exactly once, and
# each <new> must differ from it.
mutate() {
  local name="$1" rel="$2"
  shift 2
  local dir="$tmp/mut-$name"
  rm -rf "$dir"
  mkdir -p "$dir"
  cp -R "$here/../apps/agent/includes/." "$dir/" || return 1
  "$node_bin" -e '
    const fs = require("fs");
    const [file, ...pairs] = process.argv.slice(1);
    if (pairs.length === 0 || pairs.length % 2 !== 0) { console.error("want pairs of <old> <new>"); process.exit(1); }
    let s = fs.readFileSync(file, "utf8");
    for (let i = 0; i < pairs.length; i += 2) {
      const n = s.split(pairs[i]).length - 1;
      if (n !== 1) { console.error("occurs " + n + " time(s), not once: " + pairs[i]); process.exit(1); }
      if (pairs[i] === pairs[i + 1]) { console.error("the replacement is the anchor itself: " + pairs[i]); process.exit(1); }
      s = s.replace(pairs[i], () => pairs[i + 1]);
    }
    fs.writeFileSync(file, s);
  ' "$dir/$rel" "$@"
}
# mutant <name> <what is broken> <expected red line> <file under includes> <old> <new> ...
# mutant_mode=apply: the defect must apply to the source. mutant_mode=run: it must, and the real round trip
# must then be red on the line that is meant to see it.
mutant_mode=apply
mutants_seen=0
mutant() {
  local name="$1" what="$2" red="$3"
  shift 3
  mutants_seen=$((mutants_seen + 1))
  if ! mutate "$name" "$@" 2>"$tmp/mut-$name.err"; then
    bad "mutant $name does not apply to the agent source: $(cat "$tmp/mut-$name.err")"
    rm -rf "$tmp/mut-$name"
    return
  fi
  if [ "$mutant_mode" = apply ]; then
    ok "mutant $name ($what) applies to the agent source"
    rm -rf "$tmp/mut-$name"
    return
  fi
  expect "the agent path with $what turns the real round trip red" 1 "${real_c[@]}" "RT_AGENT_INCLUDES=$tmp/mut-$name"
  said "  and the check meant to see it is the one that is red" "$red"
  rm -rf "$tmp/mut-$name"
}
agent_mutants() {
  mutant verify "its verify skipped" "rt: FAIL [4.3.4 agent-containers tamper] refused:" \
    abilities/builders/class-builder-page-create.php \
    '$problem = $a->verifyCreated($postId, $doc, $principal, $requestId);' \
    '$problem = null;'
  mutant undo "an undo that says reverted and leaves the post" "rt: FAIL [4.3.4 agent-containers heading] trashed:" \
    commands/class-ability-run-command.php \
    'wp_trash_post($postId);' \
    '$postId = $postId;' \
    "return !is_object(\$after) || (string) \$after->post_status === 'trash';" \
    'return true;'
  mutant digests "its digest re-check skipped" "rt: FAIL [4.3.4 agent-containers digest] refused-preview_digest:" \
    commands/class-ability-run-command.php \
    $'$built = $this->builderBuild($spec, $adapter, $requestId);\n        if (isset($built[\'refusal\'])) {\n            return $built[\'refusal\'];\n        }\n        $precheck = $this->precheckDigest($entrySha, $inputSha, $built[\'base_fingerprint\'], $built[\'preview_digest\']);\n        if (!hash_equals($expPrev, $built[\'preview_digest\']) || !hash_equals($expPre, $precheck)) {' \
    $'$built = $this->builderBuild($spec, $adapter, $requestId);\n        if (isset($built[\'refusal\'])) {\n            return $built[\'refusal\'];\n        }\n        $precheck = $this->precheckDigest($entrySha, $inputSha, $built[\'base_fingerprint\'], $built[\'preview_digest\']);\n        if (false) {'
  # The anchor starts at the arrow: the key and the padding that aligns the array's arrows are not part of it,
  # so adding a longer key to the array does not move it.
  mutant layout "the container layout read as off" "rt: FAIL [4.3.4 agent-containers facts] layout:" \
    abilities/builders/class-elementor-facts.php \
    '=> $api->experimentActive(self::EXPERIMENT_CONTAINER),' \
    '=> false,'
  mutant guard "its undo guard skipped" "rt: FAIL [4.3.4 agent-containers person-edit] refused:" \
    commands/class-ability-run-command.php \
    '$problem = BuilderPageCreate::revertProblem($postId, $builderRow, $chain);' \
    '$problem = null;'
}
agent_mutants
if [ "$mutants_seen" -eq 0 ]; then
  bad "no agent mutant was checked against the agent source"
else
  ok "every agent mutant was checked against the agent source ($mutants_seen)"
fi

# ---------------------------------------------------------------------------------
# Group 2: a real WordPress under the Playground CLI, Elementor 4.3.4
# ---------------------------------------------------------------------------------
if [ "${RT_TEST_ONLINE:-1}" = 0 ]; then
  skipped=$((skipped + 1))
  echo "SKIP the real-WordPress cases (RT_TEST_ONLINE=0)"
else
  real=(RT_VERSIONS=4.3.4)
  if [ -n "$real_cache" ]; then
    real+=("RT_CACHE=$real_cache")
  fi

  # The planted and mutated runs below use one boot, on a containers site.
  real_c=("${real[@]}" RT_LAYOUTS=containers)

  expect "the real round trip passes on Elementor 4.3.4, on both layouts" 0 "${real[@]}"
  said "  and it ran cases on a containers site" "SUMMARY elementor=4.3.4 layout=containers cases="
  said "  and it ran cases on a sections site" "SUMMARY elementor=4.3.4 layout=sections cases="
  cases_n="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: SUMMARY .* cases=([0-9]+) .*/\1/p' | tail -n 1)"
  if [ -n "$cases_n" ] && [ "$cases_n" -gt 0 ]; then ok "  and the case count is above zero"; else bad "  the case count is not above zero (saw '$cases_n')"; fi
  agent_n="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: SUMMARY .* agent=([0-9]+) .*/\1/p' | tail -n 1)"
  if [ -n "$agent_n" ] && [ "$agent_n" -gt 0 ]; then ok "  and the agent path created and undid pages (agent=$agent_n)"; else bad "  the agent path did not run (saw '$agent_n')"; fi
  not_said "  and no case failed" "rt: FAIL"
  said "  and the saving user is the restricted principal" "unfiltered_html=no"
  said "  and the agent built the containers layout on the containers site" "rt: ok   [4.3.4 agent-containers heading]"
  said "  and the agent built the sections layout on the sections site" "rt: ok   [4.3.4 agent-sections heading]"
  said "  and the smoke probe held on a containers site" "rt: ok   [4.3.4 containers probe-smoke-heading-colour]"
  said "  and on a sections site" "rt: ok   [4.3.4 sections probe-smoke-heading-colour]"
  probes_n="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: SUMMARY .* probes=([0-9]+) .*/\1/p' | tail -n 1)"
  if [ -n "$probes_n" ] && [ "$probes_n" -gt 0 ]; then ok "  and the probe count is above zero (probes=$probes_n)"; else bad "  the probe count is not above zero (saw '$probes_n')"; fi
  said "  and the agent refused a site that rewrites the saved tree" "rt: ok   [4.3.4 agent-containers tamper]"
  said "  and the agent refused digests that are not the precheck's" "rt: ok   [4.3.4 agent-sections digest]"
  said "  and the agent refused to undo a draft a person had edited" "rt: ok   [4.3.4 agent-containers person-edit]"
  said "  and nothing the agent made was left outside the trash" "rt: ok   [4.3.4 agent-sections leftover]"
  said "  and the edit path ran its cases on a containers site" "rt: ok   [4.3.4 edit-containers set-text-every-field]"
  said "  and a batch whose later operations take earlier changes off the page passes on a containers site" "rt: ok   [4.3.4 edit-containers change-then-remove]"
  said "  and on a sections site" "rt: ok   [4.3.4 edit-sections change-then-remove]"
  said "  and the full restore put the page back" "rt: ok   [4.3.4 edit-containers restore]"
  said "  and an edit of a previewed page applied on a containers site" "rt: ok   [4.3.4 edit-containers after-view]"
  said "  and on a sections site" "rt: ok   [4.3.4 edit-sections after-view]"
  said "  and the preview had left style-cache entries for the page it edited" "rt: note [4.3.4 edit-sections after-view] the preview left style-cache entries for page"

  # One boot, five planted defects on five different cases, in both layouts.
  expect "planted defects turn the real round trip red" 1 "${real_c[@]}" "RT_PLANT=plant=unregistered_widget@heading plant=mapper_drift@text plant=render_script@list plant=render_onclick@quote plant=render_text@button"
  want="containers button render-text
containers heading stored
containers list render-script
containers quote render-on
containers text golden
sections button render-text
sections heading stored
sections list render-script
sections quote render-on
sections text golden"
  got="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: FAIL \[[0-9.]+ (containers|sections) ([a-z0-9-]+)\] ([a-z-]+):.*/\1 \2 \3/p' | sort -u)"
  if [ "$got" = "$(printf '%s\n' "$want" | sort -u)" ]; then
    ok "  each plant fails the check it is meant to prove, on its own case and no other"
  else
    bad "  the failing checks are not exactly the planted ones"
    echo "     want:"; printf '%s\n' "$want" | sort -u | sed 's/^/       /'
    echo "     got:"; printf '%s\n' "$got" | sed 's/^/       /'
    echo "$LAST_OUT" | grep '^rt: FAIL' | sed 's/^/     | /' | head -20
  fi
  fail_lines="$(printf '%s\n' "$LAST_OUT" | grep -c '^rt: FAIL ' || true)"
  matched_lines="$(printf '%s\n' "$LAST_OUT" | grep -c -E '^rt: FAIL \[[0-9.]+ (containers|sections) [a-z0-9-]+\] [a-z-]+:' || true)"
  if [ "$fail_lines" = "$matched_lines" ]; then ok "  and no failure is anything but a named check on a named case"; else bad "  $fail_lines failure line(s), $matched_lines of them name a check on a case"; fi
  # All five plants are in the round trip. The agent path runs on the same site and has no plant of its own here.
  not_said "  and no agent-path check is red in a run that planted nothing in the agent path" "rt: FAIL [4.3.4 agent-"
  # Only the round trip's own cases: the agent path prints "ok" lines of its own (tagged agent-<layout>),
  # the render probes print theirs under the layout with a probe- name, and the verdict's cases= counts
  # the round trip's cases alone.
  ok_n="$(printf '%s\n' "$LAST_OUT" | grep -E '^rt: ok +\[[0-9.]+ (containers|sections) ' | grep -v -c ' probe-' || true)"
  failing_cases="$(printf '%s\n' "$got" | awk '{print $1 " " $2}' | sort -u | wc -l | tr -d ' ')"
  total_n="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: SUMMARY .* cases=([0-9]+) .*/\1/p' | tail -n 1)"
  if [ -n "$total_n" ] && [ "$ok_n" -gt 0 ] && [ $((ok_n + failing_cases)) -eq "$total_n" ]; then
    ok "  and every case without a plant still passes ($ok_n of $total_n)"
  else
    bad "  cases without a plant did not all pass (ok=$ok_n failing=$failing_cases total=$total_n)"
  fi
  said "  the unregistered widget is the silent drop, reported as a difference in the stored tree" "stored: tree"

  # The edit path's render checks hold the page to what is still on it after the call: one boot, three plants on
  # three edit cases. The batch that takes its earlier changes off the page is held to the one text it leaves, the
  # case with a link that stays is held to the link, and a case with no after tree is held to every text it wrote.
  expect "planted edit render defects turn the real round trip red" 1 "${real_c[@]}" "RT_PLANT=plant=edit_render_drop_text@change-then-remove plant=edit_render_drop_link@set-text-every-field plant=edit_render_text@all-five-operations-entity-text"
  want="all-five-operations-entity-text render-text
change-then-remove render-text
set-text-every-field render-link"
  got="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: FAIL \[[0-9.]+ edit-containers ([a-z0-9-]+)\] ([a-z-]+):.*/\1 \2/p' | sort -u)"
  if [ "$got" = "$(printf '%s\n' "$want" | sort -u)" ]; then
    ok "  each plant fails the check it is meant to prove, on its own edit case and no other"
  else
    bad "  the failing edit checks are not exactly the planted ones"
    echo "     want:"; printf '%s\n' "$want" | sort -u | sed 's/^/       /'
    echo "     got:"; printf '%s\n' "$got" | sed 's/^/       /'
    echo "$LAST_OUT" | grep '^rt: FAIL' | sed 's/^/     | /' | head -20
  fi
  said "  the text the batch leaves on the page is the one it is held to" "[4.3.4 edit-containers change-then-remove] render-text: 1 text(s) not shown as written, first: Find us"
  said "  and a link that stays on the page is held to it" "[4.3.4 edit-containers set-text-every-field] render-link: no anchor carries the link https://example.com/book"
  not_said "  and every plant was applied" "] plant:"

  # The edit after a view: a page is previewed, then edited, and the edit must apply. Each plant below runs on its own
  # boot and the case must be red on one check of it and on nothing else in the whole run; a plant that was not applied
  # would say so with a "plant" check of its own, which is not allowed to appear.
  # red_only <label> <check>   the one failure of the last run is that check of the after-view edit case.
  red_only() {
    local label="$1" check="$2" got n
    got="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: FAIL \[[0-9.]+ edit-containers ([a-z0-9-]+)\] ([a-z-]+):.*/\1 \2/p' | sort -u)"
    n="$(printf '%s\n' "$LAST_OUT" | grep -c '^rt: FAIL ' || true)"
    if [ "$got" = "after-view $check" ] && [ "$n" = 1 ]; then
      ok "$label"
    else
      bad "$label (want exactly one failure, after-view $check; got $n failure line(s))"
      echo "     got:"; printf '%s\n' "$got" | sed 's/^/       /'
      echo "$LAST_OUT" | grep '^rt: FAIL' | sed 's/^/     | /' | head -20
    fi
  }
  # While the edit is saved, the style-cache entry of ANOTHER page is cleared: the write must not be applied.
  expect "a plant that clears another page's style-cache entry while an edit is saved turns the edit-after-view case red" 1 "${real_c[@]}" "RT_PLANT=plant=edit_view_other_cache@after-view"
  red_only "  and it is red on the write of that case, on no other check and on no other case" write
  said "  and the write was refused as a change outside the page" "[4.3.4 edit-containers after-view] write: the write was answered ok=false outcome=refused code=side_effect_detected"
  said "  and the case had looked at a page that held an entry" "rt: note [4.3.4 edit-containers after-view] the preview left style-cache entries for page"
  not_said "  and the plant was applied" "] plant:"
  # A look that leaves no style-cache entry for the page would make the edit one of a page nobody looked at.
  expect "a look that leaves no style-cache entry for the page turns the edit-after-view case red" 1 "${real_c[@]}" "RT_PLANT=plant=edit_view_no_styles@after-view"
  red_only "  and it is red on the look and on no other check and no other case" viewed
  said "  and it says the preview left no entry" "[4.3.4 edit-containers after-view] viewed: the preview left no style-cache entry for page"
  not_said "  and the plant was applied" "] plant:"

  # The agent path must look at something: handed no outline, it is red, and names what never ran.
  expect "an agent path handed no outline is red" 1 "${real_c[@]}" "RT_PLANT=plant=agent_no_cases@all"
  said "  and it says the agent path ran no case" "the agent path ran no case"
  said "  and it says which refusal scenario never ran" "did not run the refusal scenario tamper"

  # The defects in the AGENT's own create path (the table in Group 1b), planted in a copy of its source that the
  # harness loads instead (RT_AGENT_INCLUDES). Each must turn the real round trip red, on the check that is meant
  # to see it.
  mutant_mode=run
  agent_mutants

  # The render probes. A probe that is edited is edited in a copy of the harness directory, whose run.sh then
  # mounts the copy: the probe the check runs is the edited one, and the committed file is not touched. An edit
  # that no longer applies to the probe file is a failure of this suite, not a pass.
  # probe_copy <dir> <js>   a copy of the harness directory; <js> edits d, the parsed render-probes.json.
  probe_copy() {
    local dir="$1" js="$2"
    rm -rf "$dir"
    mkdir -p "$dir"
    cp "$here"/elementor-roundtrip/* "$dir/" || return 1
    "$node_bin" -e '
      const fs = require("fs");
      const file = process.argv[1];
      const d = JSON.parse(fs.readFileSync(file, "utf8"));
      new Function("d", process.argv[2])(d);
      fs.writeFileSync(file, JSON.stringify(d, null, 2) + "\n");
    ' "$dir/render-probes.json" "$js"
  }
  # probe_expect <name> <want> <dir>   expect(), run through the run.sh of that copy, on one containers boot.
  probe_expect() {
    local name="$1" want="$2" dir="$3" saved="$check"
    check="$dir/run.sh"
    expect "$name" "$want" "${real_c[@]}" "RT_FIXTURES_DIR=$here/../apps/agent/tests/fixtures/ability-run" "RT_AGENT_INCLUDES=$here/../apps/agent/includes"
    check="$saved"
  }
  # probe_case <dir> <what> <js>   makes the copy, or fails the suite.
  probe_case() {
    if probe_copy "$1" "$3" 2>"$1.err"; then return 0; fi
    bad "the probe file cannot be edited to $2: $(cat "$1.err" 2>/dev/null)"
    return 1
  }

  expect "a probe phase handed no probe is red" 1 "${real_c[@]}" "RT_PLANT=plant=probes_no_cases@all"
  said "  and it says the probe phase ran no probe" "FAIL the probe phase ran no probe"
  said "  and the summary counts no probe" "probes=0 probes_failed=0"

  if probe_case "$tmp/probes-edited" "expect another colour" 'd.probes[0].css[0].decl.color = "#654321";'; then
    probe_expect "a probe whose expected declaration is edited turns the real round trip red" 1 "$tmp/probes-edited"
    said "  and the red check is that probe's declaration, with what the element does carry" "rt: FAIL [4.3.4 containers probe-smoke-heading-colour] css: css[0] element 4ead001 .elementor-heading-title: want color:#654321 (has #123456)"
    said "  and the summary counts the one red probe, however many probes ran" "probes_failed=1 failed="
    fail_n="$(printf '%s\n' "$LAST_OUT" | grep -c '^rt: FAIL ' || true)"
    if [ "$fail_n" = 1 ]; then ok "  and nothing else is red"; else bad "  $fail_n failure line(s), wanted exactly the probe's"; fi
  fi

  if probe_case "$tmp/probes-absent" "list a declaration that is there as absent" 'd.probes[0].css_absent = [{ el: "4ead001", sel: ".elementor-heading-title", decl: { color: "#123456" } }];'; then
    probe_expect "a probe whose absent declaration is there turns the real round trip red" 1 "$tmp/probes-absent"
    said "  and it names the declaration" "css-absent: css_absent[0] element 4ead001 .elementor-heading-title is styled after all: color:#123456"
  fi

  if probe_case "$tmp/probes-html" "expect an id and a tag the page does not carry" 'd.probes[0].html_ids = ["no-such-anchor"]; d.probes[0].html_tags = { "smoke-heading": "section" };'; then
    probe_expect "a probe whose id or tag is not on the page turns the real round trip red" 1 "$tmp/probes-html"
    said "  and it names the missing id" 'html-id: the rendered page carries id="no-such-anchor" on 0 element(s), not one'
    said "  and it names the wrong tag" 'html-tag: the element with id="smoke-heading" is div, not section'
  fi

  if probe_case "$tmp/probes-nocss" "expect nothing" 'delete d.probes[0].css;'; then
    probe_expect "a probe that expects no declaration cannot be used and is red" 2 "$tmp/probes-nocss"
    said "  and it says what is wrong" "expects no declaration"
    not_said "  and nothing ran before the file was refused" "rt: ok   ["
  fi

  # What the probe phase must NOT block: a declaration Elementor writes in another property for another release
  # (the first alternative does not hold, the second does), and an absent declaration that is absent.
  if probe_case "$tmp/probes-any" "give the colour two alternatives" 'd.probes[0].css[0] = { el: "4ead001", sel: ".elementor-heading-title", media: null, any: [{ color: "#654321" }, { color: "#123456" }] }; d.probes[0].css_absent = [{ el: "4ead001", sel: ".elementor-heading-title", decl: { "text-decoration": "underline" } }];'; then
    probe_expect "a probe whose second alternative holds and whose absent declaration is absent is not blocked" 0 "$tmp/probes-any"
    said "  and it says which alternative held" "css[0] 4ead001 holds by alternative 2 of 2"
    said "  and the probe passed" "rt: ok   [4.3.4 containers probe-smoke-heading-colour]"
  fi

  # The pattern probes: what each looks at is the element, the text or the registry, and each goes red when the
  # thing it looks at is taken away. A mutation names its probe, so the suite does not depend on their order.
  # pattern_case <dir> <what> <probe> <js>   probe_case, with p bound to the probe of that name.
  pattern_case() {
    probe_case "$1" "$2" "const p = d.probes.find((x) => x.name === '$3'); if (!p) { throw new Error('no probe $3'); } $4"
  }
  if pattern_case "$tmp/probes-nofollow" "drop the nofollow member of the link" pattern-link-nofollow 'p.trees.containers[0].elements[0].settings.link.nofollow = "";'; then
    probe_expect "pattern_nofollow_red: a link that loses its nofollow turns the real round trip red" 1 "$tmp/probes-nofollow"
    said "  and the red check is the anchor with the nofollow rel, on its own probe" "rt: FAIL [4.3.4 containers probe-pattern-link-nofollow] html-has: html_has[0] a.elementor-button[href='https://example.com/partner'][rel~=nofollow] matches 0 element(s), wanted 1"
    fail_n="$(printf '%s\n' "$LAST_OUT" | grep -c '^rt: FAIL ' || true)"
    if [ "$fail_n" = 1 ]; then ok "  and nothing else is red"; else bad "  $fail_n failure line(s), wanted exactly the probe's"; fi
  fi
  if pattern_case "$tmp/probes-faq" "drop faq_schema from the accordion" pattern-accordion-faq 'delete p.trees.containers[0].elements[0].settings.faq_schema;'; then
    probe_expect "pattern_faq_schema_red: an accordion without faq_schema turns the real round trip red" 1 "$tmp/probes-faq"
    said "  and the red check is the missing JSON-LD script" "rt: FAIL [4.3.4 containers probe-pattern-accordion-faq] html-has: html_has[1] script[type='application/ld+json'] matches 0 element(s), wanted 1"
  fi
  if pattern_case "$tmp/probes-widget" "say the accordion is not registered" pattern-accordion-faq 'p.widgets = { accordion: false };'; then
    probe_expect "pattern_widget_red: a registered widget the probe says is absent turns the real round trip red" 1 "$tmp/probes-widget"
    said "  and it names the widget" "widget: the widget type accordion is registered on this site, the probe says it is not registered"
  fi
  if pattern_case "$tmp/probes-selector" "use a selector the harness does not read" pattern-icon-box 'p.html_has[0].sel = "h3:hover";'; then
    probe_expect "pattern_selector_unreadable: a selector the harness cannot read is no verdict, never a pass" 2 "$tmp/probes-selector"
    said "  and it names the syntax" "uses syntax the harness does not read"
    not_said "  and nothing ran before the file was refused" "rt: ok   ["
  fi
  # The page-frame probes: each fact the probes record is a fact the check holds the page to, so the opposite claim
  # is red, and a theme or a font family the site does not have is red.
  if pattern_case "$tmp/probes-frame-theme" "name another block theme" frame-header-footer 'p.theme = "twentytwentyfour";'; then
    probe_expect "frame_theme_red: a probe that needs another block theme than the active one turns the real round trip red" 1 "$tmp/probes-frame-theme"
    said "  and it names both themes" "the active theme is twentytwentyfive (a block theme), the probe needs the block theme twentytwentyfour"
  fi
  if pattern_case "$tmp/probes-frame-parts" "claim the theme's header is shown on the header and footer template" frame-header-footer 'p.html_has[0] = { sel: "header.wp-block-template-part" };'; then
    probe_expect "frame_header_footer_recorded_false: the opposite of the recorded fact turns the real round trip red" 1 "$tmp/probes-frame-parts"
    said "  and the red check is the theme's header part, on its own probe" "rt: FAIL [4.3.4 containers probe-frame-header-footer] html-has: html_has[0] header.wp-block-template-part matches 0 element(s), wanted 1"
  fi
  if pattern_case "$tmp/probes-frame-font" "name a font family the page never asks for" frame-google-font 'p.fonts = ["Lobster"];'; then
    probe_expect "frame_font_red: a Google family the page does not use is not found queued" 1 "$tmp/probes-frame-font"
    said "  and it names the family and what is queued" "font: no stylesheet for the family Lobster is queued for Google's font service (queued: "
    said "  and the family the page does use was queued all the same" "Poppins"
  fi
  # The plants that act on a probe's tree: a misspelt setting key, and a group control that loses its activator. Each is
  # applied to the probe that names its target, must turn that probe red on the declarations it stood for, and must leave
  # every other probe green; a plant that names no probe is red, since it would change nothing.
  expect "plant_misspell_red: a misspelt setting key turns the real round trip red" 1 "${real_c[@]}" "RT_PLANT=plant=misspell_key@pattern-icon-box"
  said "  and the red check is the colour the key stood for, on its own probe" "rt: FAIL [4.3.4 containers probe-pattern-icon-box] css: css[1] element a1b0001 .elementor-icon-box-title: want color:#2B1D16 (has no color)"
  fail_n="$(printf '%s\n' "$LAST_OUT" | grep -c '^rt: FAIL ' || true)"
  if [ "$fail_n" = 1 ]; then ok "  and nothing else is red"; else bad "  $fail_n failure line(s), wanted exactly the probe's"; fi
  said "  and the summary counts the one red probe" "probes_failed=1"
  not_said "  and the plant was applied" "] plant:"
  expect "plant_activator_red: a group control that loses its activator turns the real round trip red" 1 "${real_c[@]}" "RT_PLANT=plant=drop_activator@pattern-icon-box"
  said "  and the red check is the group's declarations, on its own probe" "rt: FAIL [4.3.4 containers probe-pattern-icon-box] css: css[1] element a1b0001 .elementor-icon-box-title: want font-size:22px (has no font-size); font-weight:600 (has no font-weight)"
  not_said "  and the colour, which the group does not own, is still there" "want color:#2B1D16"
  fail_n="$(printf '%s\n' "$LAST_OUT" | grep -c '^rt: FAIL ' || true)"
  if [ "$fail_n" = 1 ]; then ok "  and nothing else is red"; else bad "  $fail_n failure line(s), wanted exactly the probe's"; fi
  expect "a plant that names no probe is red" 1 "${real_c[@]}" "RT_PLANT=plant=misspell_key@no-such-probe"
  said "  and it says so" "plant: no probe is named no-such-probe"
  if pattern_case "$tmp/probes-plant-target" "point the plant at a setting the element does not have" pattern-icon-box 'p.plants.misspell_key.key = "no_such_setting";'; then
    RT_PLANT="plant=misspell_key@pattern-icon-box" probe_expect "a plant whose target is not in the tree is red, not a pass" 1 "$tmp/probes-plant-target"
    said "  and it says the element has no such setting" "plant: the element a1b0001 has no setting no_such_setting to plant misspell_key on"
  fi
  # What it must NOT block: a probe that applies from a later release is skipped, out loud, and is not counted as run.
  if pattern_case "$tmp/probes-since" "make the nested accordion apply from a later release" pattern-accordion-nested 'p.since = "9.9.9";'; then
    probe_expect "pattern_since_skip: a probe that applies from a later release is skipped, not red" 0 "$tmp/probes-since"
    said "  and it says it was skipped, and why" "rt: note [4.3.4 containers probe-pattern-accordion-nested] skipped: the probe applies from Elementor 9.9.9"
    not_said "  and it is not reported as run" "rt: ok   [4.3.4 containers probe-pattern-accordion-nested]"
  fi

  mkdir -p "$tmp/fx-empty"
  empty='{"note":"none","elementor_versions":["4.3.4"],"request_id":"11111111-2222-4333-8444-777777777777","media":{"5":{"url":"https://example.com/a.png","alt":"Library alt text"}},"cases":[]}'
  printf '%s\n' "$empty" >"$tmp/fx-empty/elementor-classic-containers.json"
  printf '%s\n' "$empty" >"$tmp/fx-empty/elementor-classic-sections.json"
  expect "golden fixtures with no cases are red" 2 "${real_c[@]}" "RT_FIXTURES_DIR=$tmp/fx-empty"
  said "  and the harness says why" "holds no cases"

  # An edit case whose golden after tree leaves none of the texts its operations wrote on the page would look at
  # nothing, so it is red and says why.
  mkdir -p "$tmp/fx-noafter"
  cp -R "$here/../apps/agent/tests/fixtures/ability-run/." "$tmp/fx-noafter/"
  if "$node_bin" -e '
    const fs = require("fs");
    const [file, name] = process.argv.slice(1);
    const d = JSON.parse(fs.readFileSync(file, "utf8"));
    const hit = d.cases.filter((c) => c.name === name);
    if (hit.length !== 1) { console.error("case " + name + " matched " + hit.length + " time(s)"); process.exit(1); }
    hit[0].after_tree = [];
    fs.writeFileSync(file, JSON.stringify(d));
  ' "$tmp/fx-noafter/elementor-edit-cases.json" set-text-every-field-containers 2>"$tmp/fx-noafter.err"; then
    expect "an edit case that leaves none of its written texts on the page is red" 1 "${real_c[@]}" "RT_FIXTURES_DIR=$tmp/fx-noafter"
    said "  and it says the page was held to none of them" "[4.3.4 edit-containers set-text-every-field] render-text: every text the operations wrote is off the page after the call"
    not_said "  and no other edit case is red for it" "rt: FAIL [4.3.4 edit-containers change-then-remove]"
  else
    bad "the edit fixture cannot be mutated to leave nothing on the page: $(cat "$tmp/fx-noafter.err")"
  fi

  expect "a mounted Elementor zip that is not the pinned file is red" 2 "${real_c[@]}" "RT_HARNESS_ARGS=zip_sha256=$zero_sha"
  said "  and the harness says why" "is not the pinned file"

  expect "a stored form that is not the version's is red" 1 "${real_c[@]}" "RT_HARNESS_ARGS=stored=strings"
  said "  and the difference is in a stored boolean" "isInner"
fi

echo
echo "elementor-roundtrip_test: $pass passed, $failed failed, $skipped group(s) skipped"
if [ "$failed" -ne 0 ]; then
  exit 1
fi
exit 0
