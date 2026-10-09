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
# rendered on* attribute and an escaped text that is shown wrongly. It also
# proves what it must NOT block: an honest run, a cached download, a run of one
# version out of several, and, in the planted run, every case that carries no
# plant. Run it BEFORE the real check so a guard that fails open cannot pass.
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
unset RT_PLANT RT_HARNESS_ARGS RT_TIMEOUT RT_ALLOW_FILE_URLS FAKE_MODE FAKE_ARGS_FILE FAKE_FAIL_VERSION
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
zipsdir=""
for a in "$@"; do
  case "$a" in
    elementor=*) ver="${a#elementor=}" ;;
    --mount=*:/rt/zips) zipsdir="${a#--mount=}"; zipsdir="${zipsdir%:/rt/zips}" ;;
  esac
done
if [ -n "$zipsdir" ] && [ -f "$zipsdir/elementor.zip" ]; then
  if command -v sha256sum >/dev/null 2>&1; then h="$(sha256sum "$zipsdir/elementor.zip")"; else h="$(shasum -a 256 "$zipsdir/elementor.zip")"; fi
  echo "mounted-zip-sha256=${h%% *}" >>"$out"
fi
for l in "$HOME"/.wordpress-playground/*; do
  [ -L "$l" ] && echo "cache-link=$(basename "$l") -> $(readlink "$l")" >>"$out"
done
good="rt: SUMMARY elementor=$ver cases=3 checks=9 texts=2 alts=1 failed=0"
case "${FAKE_MODE:-ok}" in
  ok)
    if [ "${FAKE_FAIL_VERSION:-}" = "$ver" ]; then
      echo "rt: FAIL [$ver containers y] stored: tree: want 1, got 2"
      echo "rt: SUMMARY elementor=$ver cases=3 checks=9 texts=2 alts=1 failed=1"
      echo "rt: RESULT FAIL"
      exit 1
    fi
    echo "$good"; echo "rt: RESULT OK"; exit 0 ;;
  silent) exit 0 ;;
  zero) echo "rt: SUMMARY elementor=$ver cases=0 checks=0 texts=0 alts=0 failed=0"; echo "rt: RESULT OK"; exit 0 ;;
  nosummary) echo "rt: RESULT OK"; exit 0 ;;
  exit1ok) echo "$good"; echo "rt: RESULT OK"; exit 1 ;;
  okfail) echo "$good"; echo "rt: RESULT OK"; echo "rt: RESULT FAIL"; exit 1 ;;
  fail) echo "rt: FAIL [x containers y] stored: tree: want 1, got 2"; echo "rt: SUMMARY elementor=$ver cases=3 checks=9 texts=2 alts=1 failed=1"; echo "rt: RESULT FAIL"; exit 1 ;;
  crash) echo "PHP Fatal error: boom"; exit 255 ;;
  hang) sleep 30; exit 0 ;;
  download) mkdir -p "$HOME/.wordpress-playground"; : >"$HOME/.wordpress-playground/custom-ffffffff.zip"; echo "$good"; echo "rt: RESULT OK"; exit 0 ;;
esac
FAKE
chmod +x "$tmp/fake-npx"

offline=(RT_ALLOW_FILE_URLS=1 "RT_PINS_FILE=$tmp/pins-good.txt" "RT_BLUEPRINT=$tmp/blueprint-good.json" "RT_NPX=$tmp/fake-npx")

# --- must not over-fire ---------------------------------------------------------------
expect "an honest run passes" 0 "${offline[@]}" "RT_CACHE=$tmp/c1" "FAKE_ARGS_FILE=$tmp/args1"
said "a green run says which versions ran" "2 version(s)"
said "a green run says every version passed" "every version passed"
args="$(cat "$tmp/args1" 2>/dev/null || true)"
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
said "  and only that version ran" "1 version(s)"
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
mkdir -p "$tmp/fx-missing"
expect "a missing golden fixture is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c6" "RT_FIXTURES_DIR=$tmp/fx-missing"
said "  and it says what is missing" "golden fixture missing"
expect "a cache dir that is not absolute is red" 2 "${offline[@]}" "RT_CACHE=relative/dir"
said "  and it says what is wrong" "absolute path"
expect "a timeout that is not a number is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c6" RT_TIMEOUT=soon
said "  and it says what is wrong" "RT_TIMEOUT must be"

# --- must go red: a run that gives no usable verdict ------------------------------------------
for mode in silent zero nosummary exit1ok okfail crash; do
  expect "stand-in mode '$mode' gives no usable verdict and is red" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 "FAKE_MODE=$mode"
  said "  and it says there was no usable verdict ($mode)" "gave no usable verdict"
done
expect "a zero-case verdict is red even though it says OK" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 FAKE_MODE=zero
said "  and it shows the case count it saw" "cases='0'"
expect "a defect the harness reports is exit 1, not a broken run" 1 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 FAKE_MODE=fail
said "  and it says the version failed" "Elementor 4.3.4 FAILED"
expect "a defect in one version is red when the other passes" 1 "${offline[@]}" "RT_CACHE=$tmp/c7" FAKE_FAIL_VERSION=3.20.4
said "  and the version that passed is still reported" "Elementor 4.3.4 OK"
said "  and the version that failed is named" "Elementor 3.20.4 FAILED"
expect "a boot that never answers is red within the timeout" 2 "${offline[@]}" "RT_CACHE=$tmp/c7" RT_VERSIONS=4.3.4 FAKE_MODE=hang RT_TIMEOUT=2
said "  and it says how long it waited" "no result after 2s"
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

  expect "the real round trip passes on Elementor 4.3.4" 0 "${real[@]}"
  said "  and it ran cases" "SUMMARY elementor=4.3.4 cases="
  cases_n="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: SUMMARY .* cases=([0-9]+) .*/\1/p' | tail -n 1)"
  if [ -n "$cases_n" ] && [ "$cases_n" -gt 0 ]; then ok "  and the case count is above zero"; else bad "  the case count is not above zero (saw '$cases_n')"; fi
  not_said "  and no case failed" "rt: FAIL"
  said "  and the saving user is the restricted principal" "unfiltered_html=no"

  # One boot, five planted defects on five different cases, in both layouts.
  expect "planted defects turn the real round trip red" 1 "${real[@]}" "RT_PLANT=plant=unregistered_widget@heading plant=mapper_drift@text plant=render_script@list plant=render_onclick@quote plant=render_text@button"
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
  ok_n="$(printf '%s\n' "$LAST_OUT" | grep -c '^rt: ok ' || true)"
  failing_cases="$(printf '%s\n' "$got" | awk '{print $1 " " $2}' | sort -u | wc -l | tr -d ' ')"
  total_n="$(printf '%s\n' "$LAST_OUT" | sed -n -E 's/^rt: SUMMARY .* cases=([0-9]+) .*/\1/p' | tail -n 1)"
  if [ -n "$total_n" ] && [ "$ok_n" -gt 0 ] && [ $((ok_n + failing_cases)) -eq "$total_n" ]; then
    ok "  and every case without a plant still passes ($ok_n of $total_n)"
  else
    bad "  cases without a plant did not all pass (ok=$ok_n failing=$failing_cases total=$total_n)"
  fi
  said "  the unregistered widget is the silent drop, reported as a difference in the stored tree" "stored: tree"

  mkdir -p "$tmp/fx-empty"
  empty='{"note":"none","elementor_versions":["4.3.4"],"request_id":"11111111-2222-4333-8444-777777777777","media":{"5":{"url":"https://example.com/a.png","alt":"Library alt text"}},"cases":[]}'
  printf '%s\n' "$empty" >"$tmp/fx-empty/elementor-classic-containers.json"
  printf '%s\n' "$empty" >"$tmp/fx-empty/elementor-classic-sections.json"
  expect "golden fixtures with no cases are red" 2 "${real[@]}" "RT_FIXTURES_DIR=$tmp/fx-empty"
  said "  and the harness says why" "holds no cases"

  expect "a mounted Elementor zip that is not the pinned file is red" 2 "${real[@]}" "RT_HARNESS_ARGS=zip_sha256=$zero_sha"
  said "  and the harness says why" "is not the pinned file"

  expect "a stored form that is not the version's is red" 1 "${real[@]}" "RT_HARNESS_ARGS=stored=strings"
  said "  and the difference is in a stored boolean" "isInner"
fi

echo
echo "elementor-roundtrip_test: $pass passed, $failed failed, $skipped group(s) skipped"
if [ "$failed" -ne 0 ]; then
  exit 1
fi
exit 0
