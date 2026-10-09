#!/usr/bin/env bash
# scripts/elementor-roundtrip_test.sh
#
# Regression suite for scripts/elementor-roundtrip/run.sh and harness.php. It
# proves the round trip goes red on a missing tool, on a bad or missing pin, on a
# download that does not match its sha256 (and on a cached file that has been
# tampered with), when the Playground CLI fetches a WordPress of its own, on a
# boot that never answers, on a run that prints no verdict, a verdict without
# cases, a verdict its exit status contradicts, or a crash; and, against a real
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
# its undo guard skipped), each on the check meant to see it. It also
# proves what it must NOT block: an honest run, a cached download, a run of one
# version or one layout out of several, a blueprint that sets some other site
# option, and, in the planted run, every case that carries no plant and the
# whole agent path. Run it BEFORE the real check so a guard that fails open
# cannot pass.
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
for a in "$@"; do
  case "$a" in
    elementor=*) ver="${a#elementor=}" ;;
    layout=*) layout="${a#layout=}" ;;
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
tail_ok="cases=3 checks=9 texts=2 alts=1 agent=2 refused=3"
good="rt: SUMMARY elementor=$ver layout=$layout $tail_ok failed=0"
case "${FAKE_MODE:-ok}" in
  ok)
    if [ "${FAKE_FAIL_VERSION:-}" = "$ver" ] && { [ -z "${FAKE_FAIL_LAYOUT:-}" ] || [ "$FAKE_FAIL_LAYOUT" = "$layout" ]; }; then
      echo "rt: FAIL [$ver containers y] stored: tree: want 1, got 2"
      echo "rt: SUMMARY elementor=$ver layout=$layout $tail_ok failed=1"
      echo "rt: RESULT FAIL"
      exit 1
    fi
    echo "$good"; echo "rt: RESULT OK"; exit 0 ;;
  silent) exit 0 ;;
  zero) echo "rt: SUMMARY elementor=$ver layout=$layout cases=0 checks=0 texts=0 alts=0 agent=2 refused=3 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  zeroagent) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=0 refused=3 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  norefusal) echo "rt: SUMMARY elementor=$ver layout=$layout cases=3 checks=9 texts=2 alts=1 agent=2 refused=0 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
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
for mode in silent zero zeroagent norefusal wronglayout nolayout oldsummary nosummary exit1ok okfail crash; do
  expect "stand-in mode '$mode' gives no usable verdict and is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers "FAKE_MODE=$mode"
  said "  and it says there was no usable verdict ($mode)" "gave no usable verdict"
done
expect "a zero-case verdict is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=zero
said "  and it shows the case count it saw" "cases='0'"
expect "a verdict that ran no agent case is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=zeroagent
said "  and it shows the agent count it saw" "agent='0'"
expect "a verdict that ran no refusal scenario is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 RT_LAYOUTS=containers FAKE_MODE=norefusal
said "  and it shows the refusal count it saw" "refused='0'"
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
  said "  and the agent refused a site that rewrites the saved tree" "rt: ok   [4.3.4 agent-containers tamper]"
  said "  and the agent refused digests that are not the precheck's" "rt: ok   [4.3.4 agent-sections digest]"
  said "  and the agent refused to undo a draft a person had edited" "rt: ok   [4.3.4 agent-containers person-edit]"
  said "  and nothing the agent made was left outside the trash" "rt: ok   [4.3.4 agent-sections leftover]"

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
  # and the verdict's cases= counts the round trip's cases alone.
  ok_n="$(printf '%s\n' "$LAST_OUT" | grep -c -E '^rt: ok +\[[0-9.]+ (containers|sections) ' || true)"
  failing_cases="$(printf '%s\n' "$got" | awk '{print $1 " " $2}' | sort -u | wc -l | tr -d ' ')"
  total_n="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: SUMMARY .* cases=([0-9]+) .*/\1/p' | tail -n 1)"
  if [ -n "$total_n" ] && [ "$ok_n" -gt 0 ] && [ $((ok_n + failing_cases)) -eq "$total_n" ]; then
    ok "  and every case without a plant still passes ($ok_n of $total_n)"
  else
    bad "  cases without a plant did not all pass (ok=$ok_n failing=$failing_cases total=$total_n)"
  fi
  said "  the unregistered widget is the silent drop, reported as a difference in the stored tree" "stored: tree"

  # The agent path must look at something: handed no outline, it is red, and names what never ran.
  expect "an agent path handed no outline is red" 1 "${real_c[@]}" "RT_PLANT=plant=agent_no_cases@all"
  said "  and it says the agent path ran no case" "the agent path ran no case"
  said "  and it says which refusal scenario never ran" "did not run the refusal scenario tamper"

  # Defects in the AGENT's own create path, planted in a copy of its source that the
  # harness loads instead (RT_AGENT_INCLUDES). Each must turn the real round trip red,
  # on the check that is meant to see it. A mutation that no longer applies to the
  # source is a failure of this suite, not a pass: update it with the source.
  node_bin="$(command -v node || true)"
  [ -n "$node_bin" ] || { echo "FAIL setup: node not found (the agent mutations are applied with it)"; exit 1; }
  # mutate <name> <file under includes> <old> <new> [<old> <new> ...]: each <old> must occur exactly once.
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
      let s = fs.readFileSync(file, "utf8");
      for (let i = 0; i < pairs.length; i += 2) {
        const n = s.split(pairs[i]).length - 1;
        if (n !== 1) { console.error("occurs " + n + " time(s), not once: " + pairs[i]); process.exit(1); }
        s = s.replace(pairs[i], () => pairs[i + 1]);
      }
      fs.writeFileSync(file, s);
    ' "$dir/$rel" "$@"
  }
  # mutant <name> <what is broken> <expected red line> <file under includes> <old> <new> ...
  mutant() {
    local name="$1" what="$2" red="$3"
    shift 3
    if ! mutate "$name" "$@" 2>"$tmp/mut-$name.err"; then
      bad "mutant $name does not apply to the agent source: $(cat "$tmp/mut-$name.err")"
      return
    fi
    expect "the agent path with $what turns the real round trip red" 1 "${real_c[@]}" "RT_AGENT_INCLUDES=$tmp/mut-$name"
    said "  and the check meant to see it is the one that is red" "$red"
    rm -rf "$tmp/mut-$name"
  }
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
  mutant layout "the container layout read as off" "rt: FAIL [4.3.4 agent-containers facts] layout:" \
    abilities/builders/class-elementor-facts.php \
    "'containers'       => \$api->experimentActive(self::EXPERIMENT_CONTAINER)," \
    "'containers'       => false,"
  mutant guard "its undo guard skipped" "rt: FAIL [4.3.4 agent-containers person-edit] refused:" \
    commands/class-ability-run-command.php \
    '$problem = BuilderPageCreate::revertProblem($postId, $builderRow);' \
    '$problem = null;'

  mkdir -p "$tmp/fx-empty"
  empty='{"note":"none","elementor_versions":["4.3.4"],"request_id":"11111111-2222-4333-8444-777777777777","media":{"5":{"url":"https://example.com/a.png","alt":"Library alt text"}},"cases":[]}'
  printf '%s\n' "$empty" >"$tmp/fx-empty/elementor-classic-containers.json"
  printf '%s\n' "$empty" >"$tmp/fx-empty/elementor-classic-sections.json"
  expect "golden fixtures with no cases are red" 2 "${real_c[@]}" "RT_FIXTURES_DIR=$tmp/fx-empty"
  said "  and the harness says why" "holds no cases"

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
