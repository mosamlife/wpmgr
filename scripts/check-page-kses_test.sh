#!/usr/bin/env bash
# scripts/check-page-kses_test.sh
#
# Regression suite for scripts/check-page-kses.sh. Proves the check goes red on
# every way core's sanitiser can change our markup (the void-element form, a
# stripped tag, an attribute, an apostrophe, a bare ampersand, a title), on
# empty and missing input, on a bad download, on a wrong pin, and when its own
# commands fail; and that it does NOT go red on markup core keeps byte for
# byte. Run it BEFORE the real check so a guard that fails open cannot pass.
#
# It runs against the real pinned cores, so the first run downloads them (the
# real check then reuses the same cache). A cache dir can be chosen with
# PAGE_KSES_CACHE.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
check="$here/check-page-kses.sh"
cores="$here/page-blocks/kses-cores.txt"
tmp="$(mktemp -d -t page-kses-test.XXXXXX)"
trap 'rm -rf "$tmp"' EXIT

# A developer's own seams must not leak into the cases below.
unset PAGE_KSES_MARKUP PAGE_KSES_PHP PAGE_KSES_CORES_FILE PAGE_KSES_VERSIONS PAGE_KSES_KNOWN_FILE
unset PAGE_BLOCKS_OUTLINES PAGE_BLOCKS_MEDIA

pass=0
failed=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { failed=$((failed + 1)); echo "FAIL $1"; }

# expect <name> <want: 0|nonzero> <env...>   runs the check; output in LAST_OUT.
# The committed known-changes list names cases of the real generator output, so
# it is off unless a case sets PAGE_KSES_KNOWN_FILE itself (a later NAME=value
# wins over an earlier one).
expect() {
  local name="$1" want="$2"
  shift 2
  local out rc
  out="$(env PAGE_KSES_KNOWN_FILE=- "$@" "$check" 2>&1)"
  rc=$?
  if [ "$want" = 0 ] && [ "$rc" -eq 0 ]; then ok "$name"
  elif [ "$want" = nonzero ] && [ "$rc" -ne 0 ]; then ok "$name"
  else bad "$name (exit $rc, wanted $want)"; echo "$out" | sed 's/^/     | /' | head -12
  fi
  LAST_OUT="$out"
}

# said <name> <pattern>   LAST_OUT must contain the pattern: a red must be red
# for the right reason, not because something else broke.
said() {
  case "$LAST_OUT" in
    *"$2"*) ok "$1" ;;
    *) bad "$1 (expected output to contain: $2)"; echo "$LAST_OUT" | sed 's/^/     | /' | head -8 ;;
  esac
}

# The pins the cases below borrow.
line() { awk -v v="$1" '$1 == v { print }' "$cores"; }
sha62="$(line 6.2 | awk '{print $2}')"
url62="$(line 6.2 | awk '{print $3}')"
sha69="$(line 6.9.10 | awk '{print $2}')"
url69="$(line 6.9.10 | awk '{print $3}')"
if [ -z "$sha62" ] || [ -z "$url62" ] || [ -z "$sha69" ] || [ -z "$url69" ]; then
  echo "FAIL setup: kses-cores.txt does not list 6.2 and 6.9.10"
  exit 1
fi

# mk <file> <name> <json-escaped content> [<json-escaped title>]
mk() {
  if [ -n "${4:-}" ]; then
    printf '{"cases":[{"name":"%s","content":"%s","title":"%s"}]}' "$2" "$3" "$4" >"$tmp/$1"
  else
    printf '{"cases":[{"name":"%s","content":"%s"}]}' "$2" "$3" >"$tmp/$1"
  fi
}

# --- markup core keeps byte for byte (hand written, in the form the agent emits) ---
layout_ok='<!-- wp:columns -->\n<div class=\"wp-block-columns\"><!-- wp:column -->\n<div class=\"wp-block-column\"><!-- wp:image {\"id\":42,\"sizeSlug\":\"large\",\"linkDestination\":\"none\"} -->\n<figure class=\"wp-block-image size-large\"><img src=\"https://example.test/a.jpg?w=1&amp;h=2\" alt=\"Bob&apos;s &quot;dog&quot; &amp; cat\" class=\"wp-image-42\" /></figure>\n<!-- /wp:image --></div>\n<!-- /wp:column --></div>\n<!-- /wp:columns -->\n\n<!-- wp:separator -->\n<hr class=\"wp-block-separator has-alpha-channel-opacity\" />\n<!-- /wp:separator -->\n\n<!-- wp:paragraph -->\n<p>Fish &amp; chips &#091;1&#093;</p>\n<!-- /wp:paragraph -->'
classic_ok='<p>Intro &amp; overview</p>\n\n<img src=\"https://example.test/a.jpg\" alt=\"A\" class=\"aligncenter wp-image-42 size-large\" />\n\n<blockquote><p>Quote</p><cite>Someone</cite></blockquote>\n\n<hr />\n\n<table><thead><tr><th>a</th></tr></thead><tbody><tr><td>1</td></tr></tbody></table>'
mk layout_ok.json layout-ok "$layout_ok" "Q&amp;A it's"
mk classic_ok.json classic-ok "$classic_ok"

# --- one planted defect per file: each is something a sanitiser rewrites ---
# (Plain assignments with unquoted variables: the form that means the same on
# the bash 3.2 macOS ships and on the bash 5 CI runs.)
from=' />'
to='/>'
slash_all=${layout_ok//$from/$to}
slash_img=${layout_ok/$from/$to}
slash_classic=${classic_ok//$from/$to}
mk plant_slash_all.json slash-all "$slash_all"
mk plant_slash_img.json slash-img "$slash_img"
mk plant_slash_classic.json slash-classic "$slash_classic"
mk plant_script.json script "${layout_ok}\n\n<script>alert(1)</script>"
mk plant_onclick.json onclick '<p onclick=\"x()\">Hi</p>'
mk plant_bare_amp.json bare-amp '<p>fish & chips</p>'
mk plant_javascript_href.json js-href '<p><a href=\"javascript:alert(1)\">x</a></p>'
# Raw apostrophe and numeric apostrophe in an attribute: core 7.x rewrites both.
mk plant_apos_raw.json apos-raw '<img src=\"https://example.test/a.jpg\" alt=\"Bob'"'"'s dog\" class=\"wp-image-7\" />'
mk plant_apos_num.json apos-num '<img src=\"https://example.test/a.jpg\" alt=\"Bob&#039;s dog\" class=\"wp-image-7\" />'
# A title that title_save_pre would strip a tag from (core keeps a few inline
# tags in a title, such as b and em, but not a div).
mk plant_title.json title-tag '<p>fine</p>' '<div>Boxed</div> title'

printf '{"cases":[]}' >"$tmp/zero_cases.json"
mk empty_content.json empty ''
printf '' >"$tmp/empty_file.json"
printf 'not json' >"$tmp/not_json.json"
printf '{"cases":[{"content":"<p>x</p>"}]}' >"$tmp/no_name.json"
printf '{"cases":[{"name":"t","content":"<p>x</p>","title":""}]}' >"$tmp/empty_title.json"

# --- must not over-fire --------------------------------------------------------------
expect "layout markup core keeps is accepted on every pinned core" 0 PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "a green run reports how many cases it checked" " case(s), 0 changed"
said "a green run reports how many cores it checked" "core(s) checked, no byte changed"
expect "classic markup core keeps is accepted on every pinned core" 0 PAGE_KSES_MARKUP="$tmp/classic_ok.json"
expect "several markup files are all checked" 0 PAGE_KSES_MARKUP="$tmp/layout_ok.json $tmp/classic_ok.json"
said "both files are counted" "2 file(s)"
expect "a raw apostrophe in an attribute is accepted before core 7" 0 PAGE_KSES_VERSIONS="6.2 6.9.10" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"

# --- must go red: the defects a sanitiser rewrites -------------------------------------
for v in 6.2 6.9.10 7.1.3; do
  expect "void element without the space is red on $v" nonzero PAGE_KSES_VERSIONS="$v" PAGE_KSES_MARKUP="$tmp/plant_slash_all.json"
  said "  and it is red because kses changed it ($v)" "KSES-CHANGED plant_slash_all.json:slash-all"
done
expect "only the img void element without the space is red" nonzero PAGE_KSES_MARKUP="$tmp/plant_slash_img.json"
expect "classic void elements without the space are red" nonzero PAGE_KSES_MARKUP="$tmp/plant_slash_classic.json"
expect "a script tag is red" nonzero PAGE_KSES_MARKUP="$tmp/plant_script.json"
expect "an on* attribute is red" nonzero PAGE_KSES_MARKUP="$tmp/plant_onclick.json"
expect "a bare ampersand is red" nonzero PAGE_KSES_MARKUP="$tmp/plant_bare_amp.json"
expect "a javascript: href is red" nonzero PAGE_KSES_MARKUP="$tmp/plant_javascript_href.json"
expect "a raw apostrophe in an attribute is red on core 7" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
said "  and it names the case" "KSES-CHANGED plant_apos_raw.json:apos-raw"
expect "a numeric apostrophe in an attribute is red on core 7" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_MARKUP="$tmp/plant_apos_num.json"
expect "a title kses would change is red" nonzero PAGE_KSES_MARKUP="$tmp/plant_title.json"
said "  and the title is what changed" "title_save_pre"

# --- known changes: allowed only when they really happen ------------------------------
printf '7.1.3 plant_apos_raw.json apos-raw\n' >"$tmp/known_ok.txt"
expect "a listed known change is green" 0 PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_ok.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
said "  and it is still reported" "KSES-KNOWN plant_apos_raw.json:apos-raw"
printf '6.2 plant_apos_raw.json apos-raw\n' >"$tmp/known_other_version.txt"
expect "a change listed for another version is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_other_version.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
printf '7.1.3 plant_slash_all.json slash-all\n' >"$tmp/known_wrong_case.txt"
expect "a change that is not the listed case is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_wrong_case.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
printf '7.1.3 layout_ok.json layout-ok\n' >"$tmp/known_stale.txt"
expect "a listed case that did not change is red (stale)" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_stale.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "  and it says the entry is stale" "KSES-STALE layout_ok.json:layout-ok"
printf '7.1.3 no_such_file.json no-such-case\n' >"$tmp/known_missing_case.txt"
expect "a listed case that is not in the markup is red (stale)" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_missing_case.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
printf '7.1.3 only-two-fields\n' >"$tmp/known_malformed.txt"
expect "a malformed known-changes line is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_malformed.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
expect "a missing known-changes file is red" nonzero PAGE_KSES_KNOWN_FILE="$tmp/no-such-known.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
# Real generator output on the one core it is clean on: the committed list has
# nothing to say there, so a setting that wrongly fell back to it would be green.
expect "an empty known-changes setting is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE=""
said "  and it says the file is missing" "known-changes file missing"
expect "known changes can be switched off explicitly" 0 PAGE_KSES_KNOWN_FILE="-" PAGE_KSES_MARKUP="$tmp/layout_ok.json"

# --- must go red: empty and missing input -----------------------------------------------
expect "zero cases is red, not green" nonzero PAGE_KSES_MARKUP="$tmp/zero_cases.json"
expect "a case with empty content is red (kses keeps nothing unchanged)" nonzero PAGE_KSES_MARKUP="$tmp/empty_content.json"
expect "a case with no name is red" nonzero PAGE_KSES_MARKUP="$tmp/no_name.json"
expect "a case with an empty title is red" nonzero PAGE_KSES_MARKUP="$tmp/empty_title.json"
expect "an empty markup file is red" nonzero PAGE_KSES_MARKUP="$tmp/empty_file.json"
expect "unparseable markup is red" nonzero PAGE_KSES_MARKUP="$tmp/not_json.json"
expect "a missing markup file is red" nonzero PAGE_KSES_MARKUP="$tmp/no-such-file.json"
# On the one core the generator's output is clean on its own, so a wrongly green
# result here cannot be mistaken for the red the real markup earns on 6.2.
expect "an empty markup setting is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_MARKUP=""
expect "one good file and one empty file is red" nonzero PAGE_KSES_MARKUP="$tmp/layout_ok.json $tmp/empty_file.json"

# --- must go red: the tools and the version list -----------------------------------------
expect "a missing php is red" nonzero PAGE_KSES_PHP="$tmp/no-such-php" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "  and it says php is the problem" "php not found"
expect "a php that fails is red" nonzero PAGE_KSES_PHP=/usr/bin/false PAGE_KSES_MARKUP="$tmp/layout_ok.json"
expect "no versions selected is red" nonzero PAGE_KSES_VERSIONS="" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
expect "blank versions selected is red" nonzero PAGE_KSES_VERSIONS="  " PAGE_KSES_MARKUP="$tmp/layout_ok.json"
expect "a version that is not pinned is red" nonzero PAGE_KSES_VERSIONS="9.9.9" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
expect "one pinned and one unknown version is red" nonzero PAGE_KSES_VERSIONS="7.1.3 9.9.9" PAGE_KSES_MARKUP="$tmp/layout_ok.json"

# --- must go red: the pin table ---------------------------------------------------------------
printf '# only a comment\n' >"$tmp/cores_empty.txt"
expect "a cores file with no cores is red" nonzero PAGE_KSES_CORES_FILE="$tmp/cores_empty.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
expect "a missing cores file is red" nonzero PAGE_KSES_CORES_FILE="$tmp/no-such-cores.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
printf '6.2 notahash %s\n' "$url62" >"$tmp/cores_badsha.txt"
expect "a pin that is not a sha256 is red" nonzero PAGE_KSES_CORES_FILE="$tmp/cores_badsha.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
printf 'six %s %s\n' "$sha62" "$url62" >"$tmp/cores_badver.txt"
expect "a version that is not a version is red" nonzero PAGE_KSES_CORES_FILE="$tmp/cores_badver.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
printf '6.2 %s http://example.test/wordpress.tar.gz\n' "$sha62" >"$tmp/cores_http.txt"
expect "a plain http url is red" nonzero PAGE_KSES_CORES_FILE="$tmp/cores_http.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
printf '6.2 %s %s\n6.2 %s %s\n' "$sha62" "$url62" "$sha62" "$url62" >"$tmp/cores_dup.txt"
expect "the same version twice is red" nonzero PAGE_KSES_CORES_FILE="$tmp/cores_dup.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
printf '6.2 %s\n' "$sha62" >"$tmp/cores_nourl.txt"
expect "a pin without a url is red" nonzero PAGE_KSES_CORES_FILE="$tmp/cores_nourl.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"

# --- must go red: downloads, with an empty cache so nothing is trusted from before --------------
wrong="0000000000000000000000000000000000000000000000000000000000000000"
printf '6.2 %s %s\n' "$wrong" "$url62" >"$tmp/cores_wrongsha.txt"
expect "a download that does not match its pin is red" nonzero PAGE_KSES_CACHE="$tmp/cache-wrongsha" PAGE_KSES_CORES_FILE="$tmp/cores_wrongsha.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "  and it says the sha256 differs" "sha256 mismatch"
if [ -e "$tmp/cache-wrongsha/$wrong" ]; then bad "  and nothing from the mismatching download was kept"; else ok "  and nothing from the mismatching download was kept"; fi
printf '6.2 %s https://127.0.0.1:9/wordpress.tar.gz\n' "$sha62" >"$tmp/cores_unreachable.txt"
expect "a download that fails is red" nonzero PAGE_KSES_CACHE="$tmp/cache-unreachable" PAGE_KSES_CORES_FILE="$tmp/cores_unreachable.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "  and it says the download failed" "download failed"
# A cache dir that was never verified is not trusted, even if it looks like a core.
mkdir -p "$tmp/cache-unverified/$sha62/wordpress/wp-includes"
: >"$tmp/cache-unverified/$sha62/wordpress/wp-includes/kses.php"
expect "an unverified cache dir is not trusted" nonzero PAGE_KSES_CACHE="$tmp/cache-unverified" PAGE_KSES_CORES_FILE="$tmp/cores_unreachable.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "  and it tried to fetch instead" "download failed"
# A fallback url is used when the first one fails; the pin still decides.
printf '6.2 %s https://127.0.0.1:9/wordpress.tar.gz %s\n' "$sha62" "$url62" >"$tmp/cores_fallback.txt"
expect "a working fallback url is used when the first fails" 0 PAGE_KSES_CACHE="$tmp/cache-fallback" PAGE_KSES_CORES_FILE="$tmp/cores_fallback.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
# The pin names one release; the tarball must be that release.
printf '6.2 %s %s\n' "$sha69" "$url69" >"$tmp/cores_mislabel.txt"
expect "a pin whose tarball is a different release is red" nonzero PAGE_KSES_CORES_FILE="$tmp/cores_mislabel.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "  and it says which version it found" "core reports version"

# --- must go red: a core that does not carry the save chain it is supposed to test -----------------
# A copy of the verified 7.1.3 subset whose default filters file is empty: the
# comparison would still run, over nothing, so the guard must refuse to.
cache_dir="${PAGE_KSES_CACHE:-${XDG_CACHE_HOME:-${HOME:-}/.cache}/wpmgr-page-kses}"
sha73="$(line 7.1.3 | awk '{print $2}')"
if [ -f "$cache_dir/$sha73/wordpress/wp-includes/default-filters.php" ]; then
  mkdir -p "$tmp/cache-nochain"
  cp -R "$cache_dir/$sha73" "$tmp/cache-nochain/$sha73"
  : >"$tmp/cache-nochain/$sha73/wordpress/wp-includes/default-filters.php"
  expect "a core without its default save filters is red" nonzero PAGE_KSES_CACHE="$tmp/cache-nochain" PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
  said "  and it says the save chain is incomplete" "save chain of WP 7.1.3 has no"
else
  bad "setup: the 7.1.3 core is not in the cache after the runs above"
fi

# --- the generator in the loop: the real builder, both editors -----------------------------------
printf '[{"name":"plain","classic":true,"outline":[{"type":"heading","level":2,"text":"Hi & bye"},{"type":"paragraph","text":"Body [1]."}]}]' >"$tmp/outlines_ok.json"
expect "generated block and classic markup is checked" 0 PAGE_BLOCKS_OUTLINES="$tmp/outlines_ok.json"
said "  and both files were checked" "2 file(s)"
printf '[{"name":"blocks-only","outline":[{"type":"paragraph","text":"x"}]}]' >"$tmp/outlines_noclassic.json"
expect "no classic case at all is red (classic would go unchecked)" nonzero PAGE_BLOCKS_OUTLINES="$tmp/outlines_noclassic.json"
said "  and the generator refuses to write an empty classic file" "no cases to write"
printf '[{"name":"nope","outline":[{"type":"nonsense"}]}]' >"$tmp/outlines_refused.json"
expect "an outline the builder refuses is red" nonzero PAGE_BLOCKS_OUTLINES="$tmp/outlines_refused.json"
said "  and the generator is what failed" "the generator failed"
printf '[]' >"$tmp/outlines_none.json"
expect "an outlines file with no cases is red" nonzero PAGE_BLOCKS_OUTLINES="$tmp/outlines_none.json"
expect "a missing outlines file is red" nonzero PAGE_BLOCKS_OUTLINES="$tmp/no-such-outlines.json"
printf '[{"name":"img","classic":true,"outline":[{"type":"image","attachment_id":777,"alt":"x"}]}]' >"$tmp/outlines_unknown_media.json"
expect "an image whose id the media map lacks is red" nonzero PAGE_BLOCKS_OUTLINES="$tmp/outlines_unknown_media.json"

echo
echo "passed: $pass, failed: $failed"
[ "$failed" -eq 0 ]
