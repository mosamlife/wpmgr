#!/usr/bin/env bash
# scripts/elementor-roundtrip/run.sh
#
# Proves the Elementor trees the agent builds survive Elementor's own save and
# render, and that the agent's own create path works on a real Elementor. For each
# pinned Elementor version, and for each layout (containers and sections), it boots
# a fresh WordPress (WordPress Playground CLI: PHP and SQLite compiled to
# WebAssembly, no Docker, no database server), installs that Elementor with its
# container experiment set to the layout, and runs
# scripts/elementor-roundtrip/harness.php inside it. The agent's real classic mapper
# builds every golden-fixture and extra outline, Elementor's Document::save stores
# it as the agent's service user (no unfiltered_html), and the stored tree and the
# rendered page are held to the built tree and to the text the outline carried.
# Then the agent's ability_run command creates the same outlines (precheck, write
# under the precheck's digests, undo) and must refuse what it should refuse; the
# harness's header says what is held to what.
#
# The container experiment is set in the blueprint, before the site boots, because
# Elementor reads it once when it starts: it cannot be flipped inside a running
# request, and a flip there would not be what a site does.
#
# Everything downloaded is pinned by sha256 in scripts/elementor-roundtrip/pins.txt
# and is checked before it is used: the WordPress core, each Elementor zip. The CLI
# itself is pinned by version below.
#
# Fails, never skips, when: node, npx, curl or a sha256 tool cannot be found; the
# pins file is missing, empty or malformed; blueprint.json disagrees with the pins;
# a requested version or layout is not one the check knows; a download fails or does
# not match its sha256; the Playground CLI downloaded a WordPress of its own; a boot
# exceeds RT_TIMEOUT seconds; a run prints no verdict, a verdict without cases, a
# verdict for another layout, a verdict that ran no agent case or no refusal, or a
# verdict that is not OK; or no version ran.
#
# Exit status: 0 every version passed; 1 a version found a defect; 2 the check
# could not give a verdict (a missing tool, a bad pin, a crash, a timeout).
#
# Test seams (used by scripts/elementor-roundtrip_test.sh only):
#   RT_NPX              npx to run (default: npx on PATH)
#   RT_NODE             node to run (default: node on PATH)
#   RT_PINS_FILE        the pin table (default: pins.txt beside this script)
#   RT_BLUEPRINT        the Playground blueprint (default: blueprint.json beside this script)
#   RT_VERSIONS         space separated Elementor versions to run (default: every
#                       elementor line in the pins). Set to nothing, it is red.
#   RT_LAYOUTS          space separated layouts to boot each version with
#                       (default: containers sections). Set to nothing, it is red.
#   RT_CACHE            where verified downloads are kept between runs (default
#                       ${XDG_CACHE_HOME:-~/.cache}/wpmgr-elementor-roundtrip)
#   RT_FIXTURES_DIR     the golden fixtures (default apps/agent/tests/fixtures/ability-run)
#   RT_AGENT_INCLUDES   the agent's includes dir (default apps/agent/includes)
#   RT_PLANT            space separated plant=<kind>@<case> tokens for the harness
#   RT_HARNESS_ARGS     space separated key=value tokens appended to the harness's
#                       arguments; a later token overrides an earlier one
#   RT_TIMEOUT          seconds one boot may take (default 600)
#   RT_TEST_KILL_SETTLE seconds to wait between sending the kill at the timeout and
#                       reading the boot's status (default 0), so a test can make the
#                       killed boot certain to have been reaped before the status is read
#   RT_ALLOW_FILE_URLS  1 lets a pin name a file:// url
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

# The Playground CLI release this check runs. 3.1.55 and later need Node 24.18 or
# newer; CI and developer machines run Node 22.
CLI_VERSION="3.1.54"

die() {
  echo "elementor-roundtrip: $*" >&2
  exit 2
}

# --- tools: resolved here, a missing one is red ---------------------------------
NPX="${RT_NPX:-$(command -v npx || true)}"
NODE="${RT_NODE:-$(command -v node || true)}"
CURL="$(command -v curl || true)"
[ -n "$NPX" ] && [ -x "$NPX" ] || die "npx not found (the Playground CLI is run through it)"
[ -n "$NODE" ] && [ -x "$NODE" ] || die "node not found (the blueprint is read with it)"
[ -n "$CURL" ] || die "curl not found (needed to fetch the pinned downloads)"
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "no sha256 tool found (sha256sum or shasum)"
fi
if command -v sha1sum >/dev/null 2>&1; then
  sha1_of() { printf '%s' "$1" | sha1sum | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha1_of() { printf '%s' "$1" | shasum -a 1 | awk '{print $1}'; }
else
  die "no sha1 tool found (sha1sum or shasum)"
fi

# --- the pin table ---------------------------------------------------------------
pins_file="${RT_PINS_FILE:-$here/pins.txt}"
[ -f "$pins_file" ] || die "pins file missing: $pins_file"
pin_name=()
pin_ver=()
pin_sha=()
pin_url=()
pin_form=()
version_re='^[0-9]+\.[0-9]+(\.[0-9]+)?$'
sha_re='^[0-9a-f]{64}$'
url_re='^https://[A-Za-z0-9._~:/?#@!$&()*+,;=%-]+$'
file_url_re='^file:///[A-Za-z0-9._~:/%-]+$'
while IFS= read -r line || [ -n "$line" ]; do
  trimmed="${line#"${line%%[![:space:]]*}"}"
  case "$trimmed" in '' | '#'*) continue ;; esac
  read -r -a f <<<"$trimmed"
  case "${f[0]}" in wordpress | elementor) ;; *) die "pins: unknown name '${f[0]}'" ;; esac
  if [ "${f[0]}" = wordpress ] && [ "${#f[@]}" -ne 4 ]; then
    die "pins: a wordpress line is <name> <version> <sha256> <url>: '$trimmed'"
  fi
  if [ "${f[0]}" = elementor ] && [ "${#f[@]}" -ne 5 ]; then
    die "pins: an elementor line is <name> <version> <sha256> <url> <stored form>: '$trimmed'"
  fi
  if [ "${f[0]}" = elementor ]; then
    case "${f[4]}" in as_given | strings) ;; *) die "pins: elementor ${f[1]} has stored form '${f[4]}', want as_given or strings" ;; esac
  fi
  [[ ${f[1]} =~ $version_re ]] || die "pins: bad version '${f[1]}'"
  [[ ${f[2]} =~ $sha_re ]] || die "pins: ${f[0]} ${f[1]} has a sha256 that is not 64 lowercase hex characters"
  if ! [[ ${f[3]} =~ $url_re ]]; then
    if [ "${RT_ALLOW_FILE_URLS:-0}" = 1 ] && [[ ${f[3]} =~ $file_url_re ]]; then
      :
    else
      die "pins: ${f[0]} ${f[1]} has a url that is not plain https: '${f[3]}'"
    fi
  fi
  if [ "${#pin_name[@]}" -gt 0 ]; then
    i=0
    while [ "$i" -lt "${#pin_name[@]}" ]; do
      if [ "${pin_name[$i]}" = "${f[0]}" ] && [ "${pin_ver[$i]}" = "${f[1]}" ]; then
        die "pins lists ${f[0]} ${f[1]} twice"
      fi
      i=$((i + 1))
    done
  fi
  pin_name+=("${f[0]}")
  pin_ver+=("${f[1]}")
  pin_sha+=("${f[2]}")
  pin_url+=("${f[3]}")
  pin_form+=("${f[4]:-}")
done <"$pins_file"

wp_ver=""
wp_sha=""
wp_url=""
all_versions=()
i=0
while [ "$i" -lt "${#pin_name[@]}" ]; do
  if [ "${pin_name[$i]}" = wordpress ]; then
    [ -z "$wp_ver" ] || die "pins lists more than one wordpress"
    wp_ver="${pin_ver[$i]}"
    wp_sha="${pin_sha[$i]}"
    wp_url="${pin_url[$i]}"
  else
    all_versions+=("${pin_ver[$i]}")
  fi
  i=$((i + 1))
done
[ -n "$wp_ver" ] || die "pins has no wordpress line"
[ "${#all_versions[@]}" -gt 0 ] || die "pins has no elementor line"

# --- which versions to run ---------------------------------------------------------
selected=()
if [ -n "${RT_VERSIONS+x}" ]; then
  read -r -a want <<<"$RT_VERSIONS" || true
  [ "${#want[@]}" -gt 0 ] || die "RT_VERSIONS is set but names no versions"
  for w in "${want[@]}"; do
    found=0
    for t in "${all_versions[@]}"; do
      [ "$t" != "$w" ] || found=1
    done
    [ "$found" -eq 1 ] || die "Elementor $w is not pinned in $pins_file"
    selected+=("$w")
  done
else
  selected=("${all_versions[@]}")
fi

# --- which layouts to run ------------------------------------------------------------
layouts=()
if [ -n "${RT_LAYOUTS+x}" ]; then
  read -r -a layouts <<<"$RT_LAYOUTS" || true
  [ "${#layouts[@]}" -gt 0 ] || die "RT_LAYOUTS is set but names no layouts"
  for l in "${layouts[@]}"; do
    case "$l" in containers | sections) ;; *) die "RT_LAYOUTS: '$l' is not containers or sections" ;; esac
  done
else
  layouts=(containers sections)
fi

# pin_of <name> <version> <field: sha|url|form>
pin_of() {
  local k=0
  while [ "$k" -lt "${#pin_name[@]}" ]; do
    if [ "${pin_name[$k]}" = "$1" ] && [ "${pin_ver[$k]}" = "$2" ]; then
      case "$3" in
        sha) echo "${pin_sha[$k]}" ;;
        url) echo "${pin_url[$k]}" ;;
        form) echo "${pin_form[$k]}" ;;
      esac
      return 0
    fi
    k=$((k + 1))
  done
  die "no pin for $1 $2"
}

# --- the blueprint must agree with the pins -----------------------------------------
blueprint="${RT_BLUEPRINT:-$here/blueprint.json}"
[ -f "$blueprint" ] || die "blueprint missing: $blueprint"
bp_facts="$("$NODE" -e '
  const bp = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
  const pv = bp.preferredVersions || {};
  const steps = bp.steps || [];
  const inst = steps.filter(s => s.step === "installPlugin");
  const ok = inst.length === 1 && inst[0].pluginData && inst[0].pluginData.resource === "vfs"
    && inst[0].pluginData.path === "/rt/zips/elementor.zip" && inst[0].options && inst[0].options.activate === true;
  // The container experiment is the one site option this script sets, per layout.
  const sets = steps.some(s => s.step === "setSiteOptions" && s.options
    && Object.prototype.hasOwnProperty.call(s.options, "elementor_experiment-container"));
  if (!ok || sets || typeof pv.php !== "string" || typeof pv.wp !== "string") { process.exit(3); }
  console.log(pv.php + " " + pv.wp);
' "$blueprint")" || die "blueprint.json must pin preferredVersions.php and .wp, install /rt/zips/elementor.zip, activated, and leave elementor_experiment-container to this script"
php_ver="${bp_facts%% *}"
bp_wp_url="${bp_facts#* }"
[ "$bp_wp_url" = "$wp_url" ] || die "blueprint.json boots $bp_wp_url but the pins name $wp_url"
[[ $php_ver =~ ^[0-9]+\.[0-9]+$ ]] || die "blueprint.json php version is not x.y: '$php_ver'"

# derive_blueprint <layout> <dest>   the blueprint plus the step that sets Elementor's
# container experiment for the layout, written to <dest>. A site is set up this way
# before it boots; nothing in the running request changes it.
derive_blueprint() {
  "$NODE" -e '
    const fs = require("fs");
    const bp = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    const state = process.argv[2] === "containers" ? "active" : "inactive";
    bp.steps = (bp.steps || []).concat([{ step: "setSiteOptions", options: { "elementor_experiment-container": state } }]);
    fs.writeFileSync(process.argv[3], JSON.stringify(bp, null, 2) + "\n");
  ' "$blueprint" "$1" "$2"
}

# --- inputs on disk ------------------------------------------------------------------
fixtures_dir="${RT_FIXTURES_DIR:-$repo/apps/agent/tests/fixtures/ability-run}"
agent_includes="${RT_AGENT_INCLUDES:-$repo/apps/agent/includes}"
[ -d "$fixtures_dir" ] || die "fixtures dir missing: $fixtures_dir"
[ -d "$agent_includes" ] || die "agent includes dir missing: $agent_includes"
for fx in elementor-classic-containers.json elementor-classic-sections.json; do
  [ -s "$fixtures_dir/$fx" ] || die "golden fixture missing or empty: $fixtures_dir/$fx"
done
[ -s "$here/harness.php" ] || die "harness missing: $here/harness.php"
[ -s "$here/cases-extra.json" ] || die "extra cases missing: $here/cases-extra.json"

timeout_s="${RT_TIMEOUT-600}"
case "$timeout_s" in '' | *[!0-9]* | 0) die "RT_TIMEOUT must be a whole number of seconds, 1 or more: '$timeout_s'" ;; esac
kill_settle="${RT_TEST_KILL_SETTLE-0}"
case "$kill_settle" in '' | *[!0-9]*) die "RT_TEST_KILL_SETTLE must be a whole number of seconds: '$kill_settle'" ;; esac

# --- cache: absolute, and never somewhere a cleanup could hurt -------------------------
cache="${RT_CACHE:-${XDG_CACHE_HOME:-${HOME:-}/.cache}/wpmgr-elementor-roundtrip}"
case "$cache" in
  /*) ;;
  *) die "cache dir must be an absolute path: '$cache'" ;;
esac
case "$cache" in
  / | /tmp | /var | /usr | /etc | "${HOME:-/nonexistent}") die "refusing to use '$cache' as the cache dir" ;;
esac
mkdir -p "$cache/zips" "$cache/run" || die "cannot create the cache dir $cache"

run_dir="$(mktemp -d "$cache/run/rt.XXXXXX")" || die "cannot create a run dir under $cache"
case "$run_dir" in "$cache"/run/rt.*) ;; *) die "unexpected run dir: $run_dir" ;; esac
child=""
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  if [ -n "$child" ]; then
    pkill -P "$child" 2>/dev/null || true
    kill "$child" 2>/dev/null || true
  fi
  rm -rf "$run_dir"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# --- verified downloads ---------------------------------------------------------------
# fetch <label> <sha256> <url>   leaves $cache/zips/<sha256>.zip, a file whose sha256 is the pin.
fetch() {
  local label="$1" sha="$2" url="$3" dest="$cache/zips/$2.zip" part got
  if [ -f "$dest" ] && [ "$(sha256_of "$dest")" = "$sha" ]; then
    return 0
  fi
  rm -f "$dest"
  part="$run_dir/download.$sha"
  "$CURL" -fsSL --retry 2 --retry-delay 2 --connect-timeout 20 --max-time 300 -o "$part" "$url" \
    || die "download failed for $label: $url"
  got="$(sha256_of "$part")"
  if [ "$got" != "$sha" ]; then
    rm -f "$part"
    die "sha256 mismatch for $label: got $got, pinned $sha ($url)"
  fi
  mv "$part" "$dest" || die "cannot store the verified $label in $cache/zips"
}

fetch "WordPress $wp_ver" "$wp_sha" "$wp_url"
for v in "${selected[@]}"; do
  fetch "Elementor $v" "$(pin_of elementor "$v" sha)" "$(pin_of elementor "$v" url)"
done

# --- the Playground CLI's own caches live in the cache dir ------------------------------
# The CLI looks for the WordPress zip it was asked for by the name
# custom-<first 8 hex of sha1(url)>.zip under ~/.wordpress-playground and downloads
# nothing when that file exists. The file is a link to the zip whose sha256 was
# checked above; after each boot the directory must hold no regular file of that
# kind, which is what a download by the CLI would leave.
export HOME="$cache/home"
pg_cache="$HOME/.wordpress-playground"
mkdir -p "$pg_cache" || die "cannot create $pg_cache"
wp_link="$pg_cache/custom-$(sha1_of "$wp_url" | cut -c1-8).zip"
ln -sfn "$cache/zips/$wp_sha.zip" "$wp_link" || die "cannot link the pinned WordPress zip into the CLI cache"
export npm_config_cache="$cache/npm"
export npm_config_update_notifier=false npm_config_fund=false npm_config_audit=false

plants=()
if [ -n "${RT_PLANT-}" ]; then
  read -r -a plants <<<"$RT_PLANT" || true
fi
extra_args=()
if [ -n "${RT_HARNESS_ARGS-}" ]; then
  read -r -a extra_args <<<"$RT_HARNESS_ARGS" || true
fi

# --- one boot per version and layout ------------------------------------------------------
bad=0     # a boot found a defect
broken=0  # a boot could not give a verdict
ran=0
total_start="$(date +%s)"
for v in "${selected[@]}"; do
  sha="$(pin_of elementor "$v" sha)"
  form="$(pin_of elementor "$v" form)"
  for layout in "${layouts[@]}"; do
    boot="$run_dir/boot-$v-$layout"
    mkdir -p "$boot/zips" "$boot/tmp"
    cp "$cache/zips/$sha.zip" "$boot/zips/elementor.zip" || die "cannot stage the Elementor $v zip"
    derive_blueprint "$layout" "$boot/blueprint.json" || die "cannot write the $layout blueprint"
    log="$boot/out.log"
    export TMPDIR="$boot/tmp"
    echo "== Elementor $v, $layout layout, on WordPress $wp_ver, PHP $php_ver"
    start="$(date +%s)"
    set +e
    "$NPX" --yes "@wp-playground/cli@$CLI_VERSION" php \
      --php="$php_ver" \
      --wp="$wp_url" \
      --blueprint="$boot/blueprint.json" \
      --mount="$boot/zips:/rt/zips" \
      --mount="$agent_includes:/rt/agent/includes" \
      --mount="$fixtures_dir:/rt/fixtures" \
      --mount="$here:/rt/harness" \
      --verbosity=quiet \
      -- /rt/harness/harness.php "elementor=$v" "wp=$wp_ver" "php=$php_ver" "zip_sha256=$sha" "stored=$form" "layout=$layout" ${plants[@]+"${plants[@]}"} ${extra_args[@]+"${extra_args[@]}"} \
      >"$log" 2>&1 &
    child=$!
    set -e
    # The timeout is decided from the clock, recorded before the kill is sent, and the status
    # comes from that record: a boot that is reaped the moment it is stopped has no process
    # left to look at, and looking would turn a timeout into a missing verdict.
    waited=0
    timed_out=0
    while kill -0 "$child" 2>/dev/null; do
      if [ "$waited" -ge "$timeout_s" ]; then
        timed_out=1
        pkill -P "$child" 2>/dev/null || true
        kill "$child" 2>/dev/null || true
        [ "$kill_settle" -eq 0 ] || sleep "$kill_settle"
        break
      fi
      sleep 1
      waited=$((waited + 1))
    done
    set +e
    if [ "$timed_out" -eq 1 ]; then
      wait "$child" 2>/dev/null
      rc=124
    else
      wait "$child"
      rc=$?
    fi
    set -e
    child=""
    wall=$(($(date +%s) - start))
    ran=$((ran + 1))
    cat "$log"

    # The CLI must not have fetched a WordPress of its own.
    if [ -n "$(find "$pg_cache" -maxdepth 1 -name 'custom-*.zip' ! -type l 2>/dev/null)" ]; then
      echo "elementor-roundtrip: Elementor $v ($layout): the Playground CLI downloaded a WordPress of its own" >&2
      broken=1
      continue
    fi
    if [ "$rc" -eq 124 ]; then
      echo "elementor-roundtrip: Elementor $v ($layout): no result after ${timeout_s}s" >&2
      broken=1
      continue
    fi

    # The verdict is the harness's own lines, and an exit status that agrees with them.
    # An OK needs the layout it was asked for, cases, an agent case and a refusal: a run
    # that looked at nothing on the agent's path is not a pass.
    ok_lines="$(grep -c '^rt: RESULT OK$' "$log" || true)"
    fail_lines="$(grep -c '^rt: RESULT FAIL$' "$log" || true)"
    summary="$(grep '^rt: SUMMARY ' "$log" | tail -n 1 || true)"
    cases="$(printf '%s\n' "$summary" | sed -n 's/.* cases=\([0-9][0-9]*\) .*/\1/p')"
    agent="$(printf '%s\n' "$summary" | sed -n 's/.* agent=\([0-9][0-9]*\) .*/\1/p')"
    refused="$(printf '%s\n' "$summary" | sed -n 's/.* refused=\([0-9][0-9]*\) .*/\1/p')"
    layout_ok=0
    case "$summary" in *" layout=$layout "*) layout_ok=1 ;; esac
    if [ "$rc" -eq 0 ] && [ "$ok_lines" -eq 1 ] && [ "$fail_lines" -eq 0 ] && [ "$layout_ok" -eq 1 ] \
      && [ -n "$cases" ] && [ "$cases" -gt 0 ] && [ -n "$agent" ] && [ "$agent" -gt 0 ] && [ -n "$refused" ] && [ "$refused" -gt 0 ]; then
      echo "elementor-roundtrip: Elementor $v ($layout) OK in ${wall}s ($summary)"
    elif [ "$rc" -eq 1 ] && [ "$fail_lines" -eq 1 ] && [ "$ok_lines" -eq 0 ]; then
      echo "elementor-roundtrip: Elementor $v ($layout) FAILED in ${wall}s" >&2
      bad=1
    else
      echo "elementor-roundtrip: Elementor $v ($layout) gave no usable verdict (exit $rc, ${ok_lines} OK line(s), ${fail_lines} FAIL line(s), layout ok=${layout_ok}, cases='${cases}', agent='${agent}', refused='${refused}')" >&2
      broken=1
    fi
  done
done

if [ "$ran" -eq 0 ]; then
  die "no version ran"
fi
echo "elementor-roundtrip: $ran boot(s), ${#selected[@]} version(s) x ${#layouts[@]} layout(s), in $(($(date +%s) - total_start))s"
if [ "$broken" -ne 0 ]; then
  exit 2
fi
if [ "$bad" -ne 0 ]; then
  exit 1
fi
echo "elementor-roundtrip: every version passed"
exit 0
