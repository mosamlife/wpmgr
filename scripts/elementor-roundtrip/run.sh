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
# verdict for another layout, a verdict that ran no agent case or no refusal, a
# verdict that ran no designed page, a verdict that ran no render probe or does
# not say how many were red, an OK that
# counts a red probe, or a verdict that is not OK; or no version ran.
#
# Exit status: 0 every version passed; 1 a version found a defect (a red probe is one);
# 2 the check could not give a verdict (a missing tool, a bad pin, a crash, a timeout).
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
#                       dumpwidgets=1 exports a core classic widget JSON document;
#                       use one RT_VERSIONS and RT_LAYOUTS=containers per dump file
#                       native=1 runs discovery and native page edits under REST context;
#                       defaults to containers and requires that layout when explicit
#                       the legacy REST pin proves native refusals, then runs every
#                       non-native phase in a fresh ordinary boot
#   RT_NATIVE_INVENTORY the pinned core widget inventory (default core_widgets.json)
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

# --- phase selection before deriving the layout or boot blueprint ---------------------
extra_args=()
if [ -n "${RT_HARNESS_ARGS-}" ]; then
  read -r -a extra_args <<<"$RT_HARNESS_ARGS" || true
fi
dump_widgets=0
native=0
for arg in ${extra_args[@]+"${extra_args[@]}"}; do
  case "$arg" in
    dumpwidgets=0) dump_widgets=0 ;;
    dumpwidgets=1) dump_widgets=1 ;;
    dumpwidgets=*) die "dumpwidgets= must be 0 or 1" ;;
    native=0) native=0 ;;
    native=1) native=1 ;;
    native=*) die "native= must be 0 or 1" ;;
  esac
done
if [ "$native" -eq 1 ] && [ "$dump_widgets" -eq 1 ]; then
  die "native=1 cannot combine with dumpwidgets=1"
fi

# --- which layouts to run ------------------------------------------------------------
layouts=()
if [ -n "${RT_LAYOUTS+x}" ]; then
  read -r -a layouts <<<"$RT_LAYOUTS" || true
  [ "${#layouts[@]}" -gt 0 ] || die "RT_LAYOUTS is set but names no layouts"
  for l in "${layouts[@]}"; do
    case "$l" in containers | sections) ;; *) die "RT_LAYOUTS: '$l' is not containers or sections" ;; esac
  done
elif [ "$native" -eq 1 ]; then
  layouts=(containers)
else
  layouts=(containers sections)
fi
if [ "$native" -eq 1 ] && [ "${layouts[*]}" != containers ]; then
  die "native=1 requires RT_LAYOUTS=containers"
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
  # shellcheck disable=SC2016 # These interpolations belong to JavaScript template literals.
  "$NODE" -e '
    const fs = require("fs");
    const bp = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    const state = process.argv[2] === "containers" ? "active" : "inactive";
    const options = { "elementor_experiment-container": state };
    const full = process.argv[4] === "1" || process.argv[5] === "1";
    if (full) options["elementor_experiment-additional_custom_breakpoints"] = "active";
    bp.steps = (bp.steps || []).concat([{ step: "setSiteOptions", options }]);
    if (full) {
      bp.steps.push({ step: "runPHP", code: `<?php
        ${process.argv[5] === "1" ? `require "/rt/harness/native.php"; rt_native_boot_context();` : ""}
        require "/wordpress/wp-load.php";
        require "/rt/harness/dump-widgets.php";
        rt_widget_dump_prepare(\\Elementor\\Plugin::$instance);
      ` });
    }
    fs.writeFileSync(process.argv[3], JSON.stringify(bp, null, 2) + "\n");
  ' "$blueprint" "$1" "$2" "$dump_widgets" "$native"
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
native_inventory="${RT_NATIVE_INVENTORY:-$repo/apps/api/internal/elementornative/core_widgets.json}"
if [ "$native" -eq 1 ]; then
  [ -s "$here/native.php" ] || die "native phase missing: $here/native.php"
  [ -s "$native_inventory" ] || die "native core inventory missing or empty: $native_inventory"
fi

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

# Wait for either a Playground process or an ordinary-run subprocess under the same bounded deadline.
await_child() {
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
}

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
    phase_mounts=()
    if [ "$native" -eq 1 ]; then
      mkdir -p "$boot/native-catalogue"
      cp "$native_inventory" "$boot/native-catalogue/core_widgets.json" || die "cannot stage native core inventory"
      phase_mounts+=("--mount=$boot/native-catalogue:/rt/native-catalogue")
    fi
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
      ${phase_mounts[@]+"${phase_mounts[@]}"} \
      --verbosity=quiet \
      -- /rt/harness/harness.php "elementor=$v" "wp=$wp_ver" "php=$php_ver" "zip_sha256=$sha" "stored=$form" "layout=$layout" ${plants[@]+"${plants[@]}"} ${extra_args[@]+"${extra_args[@]}"} \
      >"$log" 2>&1 &
    child=$!
    set -e
    # The timeout is decided from the clock, recorded before the kill is sent, and the status
    # comes from that record: a boot that is reaped the moment it is stopped has no process
    # left to look at, and looking would turn a timeout into a missing verdict.
    await_child
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
    if [ "$dump_widgets" -eq 1 ]; then
      if [ "$rc" -eq 0 ] && [ "$ok_lines" -eq 1 ] && [ "$fail_lines" -eq 0 ] \
        && "$NODE" "$here/build-widget-allowlist.mjs" --check-dump "$log" "$v"; then
        echo "elementor-roundtrip: Elementor $v ($layout) widget dump OK"
      else
        echo "elementor-roundtrip: Elementor $v ($layout) gave no usable widget dump (exit $rc)" >&2
        broken=1
      fi
      continue
    fi
    if [ "$native" -eq 1 ]; then
      summary="$(grep '^rt: NATIVE_SUMMARY ' "$log" || true)"
      summaries="$(grep -c '^rt: NATIVE_SUMMARY ' "$log" || true)"
      native_ok=1
      native_kind="native"
      case "$summary" in *" elementor=$v layout=containers "*) ;; *) native_ok=0 ;; esac
      # Legacy evidence is accepted only from a separate ordinary boot.
      [ "$(grep -c '^rt: SUMMARY ' "$log" || true)" -eq 0 ] || native_ok=0
      positive_fields="widgets controls edits css texts refused registration_checks"
      case "$summary" in
        *" mode=unavailable "*)
          native_kind="native unavailable; non-native"
          positive_fields="widgets edits refused registration_checks"
          [ "$v" = 3.20.4 ] || native_ok=0
          widgets="$(printf '%s\n' "$summary" | sed -n 's/.* widgets=\([0-9][0-9]*\) .*/\1/p')"
          unavailable="$(printf '%s\n' "$summary" | sed -n 's/.* unavailable=\([0-9][0-9]*\) .*/\1/p')"
          [ -n "$unavailable" ] && [ "$unavailable" = "$widgets" ] || native_ok=0
          case "$summary" in *" controls=0 "*" ops=- css=0 texts=0 "*) ;; *) native_ok=0 ;; esac
          ;;
        *) case "$summary" in *" ops=add_native,duplicate,move,remove,set_native "*) ;; *) native_ok=0 ;; esac ;;
      esac
      for field in $positive_fields; do
        value="$(printf '%s\n' "$summary" | sed -n "s/.* $field=\([0-9][0-9]*\) .*/\1/p")"
        if [ -z "$value" ] || [ "$value" -le 0 ]; then native_ok=0; fi
      done
      native_failed="$(printf '%s\n' "$summary" | sed -n 's/.* failed=\([0-9][0-9]*\)$/\1/p')"
      check_fails="$(grep -c '^rt: FAIL ' "$log" || true)"
      if [ "$rc" -eq 0 ] && [ "$ok_lines" -eq 1 ] && [ "$fail_lines" -eq 0 ] && [ "$summaries" -eq 1 ] \
        && [ "$native_ok" -eq 1 ] && [ "$native_failed" = 0 ] && [ "$check_fails" -eq 0 ]; then
        if [ "$native_kind" != native ]; then
          legacy_args=()
          for arg in ${extra_args[@]+"${extra_args[@]}"}; do
            case "$arg" in native=*) ;; *) legacy_args+=("$arg") ;; esac
          done
          legacy_log="$boot/legacy.log"
          echo "elementor-roundtrip: Elementor $v ($layout) native controls unavailable; starting a fresh ordinary boot"
          set +e
          env RT_CACHE="$cache" RT_VERSIONS="$v" RT_LAYOUTS="$layout" RT_PLANT='' \
            RT_HARNESS_ARGS="${legacy_args[*]-}" "$here/run.sh" >"$legacy_log" 2>&1 &
          child=$!
          set -e
          await_child
          legacy_rc=$rc
          cat "$legacy_log"
          legacy_boots="$(grep -c '^== Elementor ' "$legacy_log" || true)"
          ran=$((ran + legacy_boots))
          legacy="$(grep '^rt: SUMMARY ' "$legacy_log" || true)"
          legacy_ok=1
          [ "$(grep -c '^rt: SUMMARY ' "$legacy_log" || true)" -eq 1 ] || legacy_ok=0
          case "$legacy" in *" elementor=$v layout=containers "*) ;; *) legacy_ok=0 ;; esac
          for field in cases agent refused designed probes texts; do
            value="$(printf '%s\n' "$legacy" | sed -n "s/.* $field=\([0-9][0-9]*\) .*/\1/p")"
            if [ -z "$value" ] || [ "$value" -le 0 ]; then legacy_ok=0; fi
          done
          case "$legacy" in *" probes_failed=0 failed=0") ;; *) legacy_ok=0 ;; esac
          legacy_ok_lines="$(grep -c '^rt: RESULT OK$' "$legacy_log" || true)"
          legacy_fail_lines="$(grep -c '^rt: RESULT FAIL$' "$legacy_log" || true)"
          legacy_check_fails="$(grep -c '^rt: FAIL ' "$legacy_log" || true)"
          legacy_failed="$(printf '%s\n' "$legacy" | sed -n 's/.* failed=\([0-9][0-9]*\)$/\1/p')"
          case "$legacy_failed" in '' | *[!0-9]*) legacy_failed=0 ;; esac
          wall=$(($(date +%s) - start))
          if [ "$legacy_rc" -eq 0 ] && [ "$legacy_ok" -eq 1 ] && [ "$legacy_ok_lines" -eq 1 ] \
            && [ "$legacy_fail_lines" -eq 0 ] && [ "$legacy_check_fails" -eq 0 ]; then
            echo "elementor-roundtrip: Elementor $v ($layout) $native_kind OK in ${wall}s ($summary)"
          elif [ "$legacy_rc" -eq 1 ] && [ "$legacy_fail_lines" -eq 1 ] && [ "$legacy_ok_lines" -eq 0 ] \
            && [ "$legacy_failed" -gt 0 ] && [ "$legacy_check_fails" -gt 0 ]; then
            echo "elementor-roundtrip: Elementor $v ($layout) $native_kind FAILED in ${wall}s ($legacy)" >&2
            bad=1
          else
            echo "elementor-roundtrip: Elementor $v ($layout) gave no usable legacy verdict from the fresh ordinary boot (exit $legacy_rc)" >&2
            broken=1
          fi
          continue
        fi
        echo "elementor-roundtrip: Elementor $v ($layout) $native_kind OK in ${wall}s ($summary)"
      elif [ "$rc" -eq 1 ] && [ "$fail_lines" -eq 1 ] && [ "$ok_lines" -eq 0 ] && [ "$summaries" -eq 1 ] \
        && [ -n "$native_failed" ] && [ "$native_failed" -gt 0 ] && [ "$check_fails" -gt 0 ]; then
        echo "elementor-roundtrip: Elementor $v ($layout) native FAILED in ${wall}s ($summary)" >&2
        bad=1
      else
        echo "elementor-roundtrip: Elementor $v ($layout) gave no usable native verdict (exit $rc, summary='$summary')" >&2
        broken=1
      fi
      continue
    fi
    summary="$(grep '^rt: SUMMARY ' "$log" | tail -n 1 || true)"
    cases="$(printf '%s\n' "$summary" | sed -n 's/.* cases=\([0-9][0-9]*\) .*/\1/p')"
    agent="$(printf '%s\n' "$summary" | sed -n 's/.* agent=\([0-9][0-9]*\) .*/\1/p')"
    refused="$(printf '%s\n' "$summary" | sed -n 's/.* refused=\([0-9][0-9]*\) .*/\1/p')"
    # The designed pages the agent's create path made and undid (a containers site), or refused for want of
    # containers (a sections site): a boot that ran none looked at nothing the designed pages rely on.
    designed="$(printf '%s\n' "$summary" | sed -n 's/.* designed=\([0-9][0-9]*\) .*/\1/p')"
    # The render probes: how many ran, and how many of those were red. " probes=" cannot match
    # inside " probes_failed=", so the two reads are independent.
    probes="$(printf '%s\n' "$summary" | sed -n 's/.* probes=\([0-9][0-9]*\) .*/\1/p')"
    probes_failed="$(printf '%s\n' "$summary" | sed -n 's/.* probes_failed=\([0-9][0-9]*\) .*/\1/p')"
    layout_ok=0
    case "$summary" in *" layout=$layout "*) layout_ok=1 ;; esac
    # An OK needs probes that ran and none red: a boot that probed nothing proved no render fact,
    # and a summary that counts a red probe contradicts an OK whatever the result line says.
    if [ "$rc" -eq 0 ] && [ "$ok_lines" -eq 1 ] && [ "$fail_lines" -eq 0 ] && [ "$layout_ok" -eq 1 ] \
      && [ -n "$cases" ] && [ "$cases" -gt 0 ] && [ -n "$agent" ] && [ "$agent" -gt 0 ] && [ -n "$refused" ] && [ "$refused" -gt 0 ] \
      && [ -n "$designed" ] && [ "$designed" -gt 0 ] \
      && [ -n "$probes" ] && [ "$probes" -gt 0 ] && [ -n "$probes_failed" ] && [ "$probes_failed" -eq 0 ]; then
      echo "elementor-roundtrip: Elementor $v ($layout) OK in ${wall}s ($summary)"
    elif [ "$rc" -eq 1 ] && [ "$fail_lines" -eq 1 ] && [ "$ok_lines" -eq 0 ]; then
      # A red probe is a defect found, the same as any other red check: exit 1, not a missing verdict.
      echo "elementor-roundtrip: Elementor $v ($layout) FAILED in ${wall}s (probes='${probes}', probes_failed='${probes_failed}')" >&2
      bad=1
    else
      echo "elementor-roundtrip: Elementor $v ($layout) gave no usable verdict (exit $rc, ${ok_lines} OK line(s), ${fail_lines} FAIL line(s), layout ok=${layout_ok}, cases='${cases}', agent='${agent}', refused='${refused}', designed='${designed}', probes='${probes}', probes_failed='${probes_failed}')" >&2
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
