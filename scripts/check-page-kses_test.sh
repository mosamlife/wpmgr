#!/usr/bin/env bash
# scripts/check-page-kses_test.sh
#
# Regression suite for scripts/check-page-kses.sh. Proves the check goes red on
# every way core's sanitiser can change our markup (the void-element form, a
# stripped tag, an attribute, an apostrophe, a bare ampersand, a title), on
# empty and missing input, on a bad download, on a wrong pin, and when its own
# commands fail; and that it does NOT go red on markup core keeps byte for
# byte. A case on the known-changes list is excused only while it is rewritten
# exactly as pinned: a second rewrite on it is red. The cache of cores is held
# to the same bar: a verified core is reused and never replaced, a corrupt one
# is replaced, runs that reach the publishing rename together or find the same
# corrupt core both end with one verified core, and a lock nobody will release
# is reclaimed or reported within a bound. Run it BEFORE the real check so a
# guard that fails open cannot pass.
#
# It runs against the real pinned cores, so the first run downloads them (the
# real check then reuses the same cache). A cache dir can be chosen with
# PAGE_KSES_CACHE. The cache cases that are about the cache alone use a
# miniature core and stand-ins for curl, mv and php, and need no network.
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

# --- known changes: allowed only when they happen exactly as pinned ----------------------
# The pin of a rewrite is the line the check itself prints for it, the one a person
# pastes into the known-changes list. So the honest case below is made the honest way.
expect "an unlisted rewrite is red and prints the line that would excuse it" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
said "  and that line names the version, the file, the case and a pin" "  pin: 7.1.3 plant_apos_raw.json apos-raw "
apos_line="$(printf '%s\n' "$LAST_OUT" | sed -n 's/^  pin: //p' | head -n 1)"
# (The pattern is kept in a variable: the form of [[ =~ ]] that behaves the same on
# the bash 3.2 macOS ships and on the bash 5 CI runs.)
pin_line_re='^7\.1\.3 plant_apos_raw\.json apos-raw [0-9a-f]{64}$'
if [[ $apos_line =~ $pin_line_re ]]; then
  ok "  and the pin is 64 lowercase hex characters"
else
  bad "  and the pin is 64 lowercase hex characters (got: '$apos_line')"
fi
printf '%s\n' "$apos_line" >"$tmp/known_ok.txt"
expect "a listed known change is green" 0 PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_ok.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
said "  and it is still reported" "KSES-KNOWN plant_apos_raw.json:apos-raw"
said "  and it is counted as known, not as drifted" " 1 known, 0 drifted, 0 stale"

# A second rewrite on the listed case. Same file name and case name as the listed
# one, so the list still names it; only what the case holds differs.
mkdir -p "$tmp/second"
second_json() { # second_json <file name> <json-escaped content>
  printf '{"cases":[{"name":"apos-raw","content":"%s"}]}' "$2" >"$tmp/second/$1"
}
second_json plant_apos_raw.json '<img src=\"https://example.test/a.jpg\" alt=\"Bob'"'"'s dog\" class=\"wp-image-7\" onclick=\"x()\" />'
expect "the listed rewrite plus a second one the core also makes is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_ok.txt" PAGE_KSES_MARKUP="$tmp/second/plant_apos_raw.json"
said "  and it is called drift" "KSES-DRIFT plant_apos_raw.json:apos-raw"
said "  and it prints the pin it was held to" "  pinned : ${apos_line##* }"
said "  and it is counted as drifted" " 0 known, 1 drifted, 0 stale"
drift_out="$LAST_OUT"
# The same input, listed on the honest case, has this output. Compare the outputs
# the core returns for the honest case and for the second-rewrite case: when they are
# the same bytes, only the input can tell the two apart.
expect "the honest case, for its output" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
honest_out_line="$(printf '%s\n' "$LAST_OUT" | sed -n 's/^    out: //p' | head -n 1)"
second_out_line="$(printf '%s\n' "$drift_out" | sed -n 's/^    out: //p' | head -n 1)"
if [ -n "$honest_out_line" ] && [ "$honest_out_line" = "$second_out_line" ]; then
  ok "  and the core returns the same bytes for both, so the pin must bind the input"
else
  bad "  and the core returns the same bytes for both (honest: $honest_out_line | second: $second_out_line)"
fi
# What a builder that starts to emit an attribute the core strips looks like.
second_json plant_apos_raw.json '<img src=\"https://example.test/a.jpg\" alt=\"Bob'"'"'s dog\" class=\"wp-image-7\" data-x=\"1\" />'
expect "an extra attribute the core strips from the listed case is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_ok.txt" PAGE_KSES_MARKUP="$tmp/second/plant_apos_raw.json"
said "  and it is called drift" "KSES-DRIFT plant_apos_raw.json:apos-raw"
second_json plant_apos_raw.json '<img src=\"https://example.test/a.jpg\" alt=\"Bob'"'"'s dog\" class=\"wp-image-7\" /><script>alert(1)</script>'
expect "a script tag added to the listed case is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_ok.txt" PAGE_KSES_MARKUP="$tmp/second/plant_apos_raw.json"
said "  and it is called drift" "KSES-DRIFT plant_apos_raw.json:apos-raw"
zero_pin="0000000000000000000000000000000000000000000000000000000000000000"
printf '7.1.3 plant_apos_raw.json apos-raw %s\n' "$zero_pin" >"$tmp/known_wrong_pin.txt"
expect "a listed case held to a pin that is not its rewrite is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_wrong_pin.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
said "  and it is called drift" "KSES-DRIFT plant_apos_raw.json:apos-raw"
printf '7.1.3 plant_apos_raw.json apos-raw\n' >"$tmp/known_no_pin.txt"
expect "a listed case without a pin is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_no_pin.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
printf '7.1.3 plant_apos_raw.json apos-raw %s\n' "${apos_line##* }x" >"$tmp/known_long_pin.txt"
expect "a pin of the wrong length is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_long_pin.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
printf '7.1.3 plant_apos_raw.json apos-raw %s\n' "$(printf '%s' "${apos_line##* }" | tr 'a-f' 'A-F')" >"$tmp/known_upper_pin.txt"
expect "a pin in capitals is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_upper_pin.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
printf '%s\n%s\n' "$apos_line" "7.1.3 plant_apos_raw.json apos-raw $zero_pin" >"$tmp/known_twice.txt"
expect "a case listed twice is red, even with the right pin first" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_twice.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
said "  and it says so" "a second time"
printf '6.2 plant_apos_raw.json apos-raw %s\n' "${apos_line##* }" >"$tmp/known_other_version.txt"
expect "a change listed for another version is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_other_version.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
printf '7.1.3 plant_slash_all.json slash-all %s\n' "${apos_line##* }" >"$tmp/known_wrong_case.txt"
expect "a change that is not the listed case is red" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_wrong_case.txt" PAGE_KSES_MARKUP="$tmp/plant_apos_raw.json"
printf '7.1.3 layout_ok.json layout-ok %s\n' "$zero_pin" >"$tmp/known_stale.txt"
expect "a listed case that did not change is red (stale)" nonzero PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_KNOWN_FILE="$tmp/known_stale.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "  and it says the entry is stale" "KSES-STALE layout_ok.json:layout-ok"
printf '7.1.3 no_such_file.json no-such-case %s\n' "$zero_pin" >"$tmp/known_missing_case.txt"
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
if [ -z "$(ls -A "$tmp/cache-wrongsha")" ]; then ok "  and no work dir was left behind"; else bad "  and no work dir was left behind ($(ls -A "$tmp/cache-wrongsha"))"; fi
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

# --- the cache of cores: reuse, a corrupt copy, concurrent runs, stale locks ------------------------
# A verified core is never replaced; only one that fails its check is, one run at a time.
# Real cores where the cases are about a real core. Where they are about the cache and
# nothing else, a miniature core and stand-ins for curl, mv and php, put in front of the
# real tools on PATH (which is how the script finds the real ones), so a race can be made to
# happen on purpose instead of hoped for. The mv stand-in parks a run at the rename that
# publishes a core until the test lets it go.
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}
await() { # await <file>   true once the file exists, false after 20 seconds
  local n=0
  while [ ! -e "$1" ] && [ "$n" -lt 200 ]; do
    sleep 0.1
    n=$((n + 1))
  done
  [ -e "$1" ]
}
arrivals() { # arrivals <state dir>   how many runs are parked at the rename
  ls "$1" 2>/dev/null | grep -c '^arrived\.' || true
}
spawn() { # spawn <out file> <env...>   the check in the background; its pid is left in SPAWNED
  local out="$1"
  shift
  env PAGE_KSES_KNOWN_FILE=- "$@" "$check" >"$out" 2>&1 &
  SPAWNED=$!
}
inode_of() { ls -di "$1" | awk '{print $1}'; }

real_mv="$(command -v mv)"
shim="$tmp/shim"
mkdir -p "$shim"
{
  printf '#!/usr/bin/env bash\nreal_mv=%s\n' "$real_mv"
  cat <<'SHIM'
# Parks the rename that publishes a core (a tree under $SHIM_CACHE/.tmp.* going to the
# cache dir or to its final name) until $SHIM_DIR/go exists. A parked run leaves
# arrived.<its pid>.<n>. SHIM_HOLD_FROM=n parks only from the n-th such rename of a run,
# so the one before it can fail the way it does in real life.
last=""
for a in "$@"; do last="$a"; done
case "$1" in
  "$SHIM_CACHE"/.tmp.*)
    if [ "$last" = "$SHIM_CACHE/" ] || [ "$last" = "$SHIM_CACHE/$SHIM_SHA" ]; then
      n=0
      [ ! -f "$SHIM_DIR/count.$PPID" ] || n="$(cat "$SHIM_DIR/count.$PPID")"
      n=$((n + 1))
      printf '%s' "$n" >"$SHIM_DIR/count.$PPID"
      if [ "$n" -ge "${SHIM_HOLD_FROM:-1}" ]; then
        : >"$SHIM_DIR/arrived.$PPID.$n"
        polls=0
        while [ ! -e "$SHIM_DIR/go" ] && [ "$polls" -lt 200 ]; do
          sleep 0.1
          polls=$((polls + 1))
        done
        if [ ! -e "$SHIM_DIR/go" ]; then
          echo "mv stand-in: nobody let the run go" >&2
          exit 1
        fi
      fi
    fi
    ;;
esac
exec "$real_mv" "$@"
SHIM
} >"$shim/mv"
chmod +x "$shim/mv"
{
  printf '#!/usr/bin/env bash\n'
  cat <<'SHIM'
# "Downloads" $SHIM_TARBALL. Logs the call first, then waits for $SHIM_CURL_WAIT to
# exist when it is set, so a run can be held in the middle of its download.
out=""
prev=""
for a in "$@"; do
  [ "$prev" != "-o" ] || out="$a"
  prev="$a"
done
if [ -z "$out" ]; then
  echo "curl stand-in: no -o given" >&2
  exit 2
fi
printf '%s\n' "$*" >>"$SHIM_DIR/curl.log"
if [ -n "${SHIM_CURL_WAIT:-}" ]; then
  polls=0
  while [ ! -e "$SHIM_CURL_WAIT" ] && [ "$polls" -lt 200 ]; do
    sleep 0.1
    polls=$((polls + 1))
  done
  if [ ! -e "$SHIM_CURL_WAIT" ]; then
    echo "curl stand-in: nobody let the download finish" >&2
    exit 1
  fi
fi
cp "$SHIM_TARBALL" "$out"
SHIM
} >"$shim/curl"
chmod +x "$shim/curl"
cat >"$tmp/fakephp" <<'SHIM'
#!/usr/bin/env bash
# Needs only the core it is handed ($2) to be there. As a reader (SHIM_READER_FLAG set)
# it holds that core until SHIM_READER_DONE exists, and fails if the core is gone or is
# a different directory by then.
core="$2"
if [ ! -f "$core/wordpress/wp-includes/kses.php" ]; then
  echo "php stand-in: no kses.php under $core" >&2
  exit 1
fi
if [ -n "${SHIM_READER_FLAG:-}" ]; then
  before="$(ls -di "$core" | awk '{print $1}')"
  : >"$SHIM_READER_FLAG"
  polls=0
  while [ ! -e "$SHIM_READER_DONE" ] && [ "$polls" -lt 400 ]; do
    if [ ! -f "$core/wordpress/wp-includes/kses.php" ]; then
      echo "php stand-in: the core vanished while it was being read" >&2
      exit 1
    fi
    sleep 0.05
    polls=$((polls + 1))
  done
  if [ ! -e "$SHIM_READER_DONE" ]; then
    echo "php stand-in: the reader was never told to stop" >&2
    exit 1
  fi
  after="$(ls -di "$core" | awk '{print $1}')"
  if [ "$before" != "$after" ]; then
    echo "php stand-in: the core was replaced while it was being read (inode $before, then $after)" >&2
    exit 1
  fi
fi
echo "php stand-in: read $core"
SHIM
chmod +x "$tmp/fakephp"

mini="$tmp/mini"
mkdir -p "$mini/wordpress/wp-includes"
printf '<?php // miniature kses\n' >"$mini/wordpress/wp-includes/kses.php"
printf '<?php $wp_version = "6.2";\n' >"$mini/wordpress/wp-includes/version.php"
COPYFILE_DISABLE=1 tar -czf "$tmp/mini.tar.gz" -C "$mini" wordpress
sha_mini="$(sha256_file "$tmp/mini.tar.gz")"
printf '6.2 %s https://example.test/mini.tar.gz\n' "$sha_mini" >"$tmp/cores_mini.txt"
hermetic=(PATH="$shim:$PATH" PAGE_KSES_PHP="$tmp/fakephp" PAGE_KSES_CORES_FILE="$tmp/cores_mini.txt" PAGE_KSES_MARKUP="$tmp/layout_ok.json" SHIM_TARBALL="$tmp/mini.tar.gz" SHIM_SHA="$sha_mini")
# newcache <name>   a fresh cache dir C and state dir S; the mv stand-in is let through
newcache() {
  C="$tmp/cache-$1"
  S="$tmp/state-$1"
  mkdir -p "$C" "$S"
  : >"$S/go"
}
# hermetic runs: "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S"
corrupt_mini() { # corrupt_mini <cache>   a cached core whose marker names another sha256
  mkdir -p "$1/$sha_mini/wordpress/wp-includes"
  printf '%s' "$wrong" >"$1/$sha_mini/.verified"
  printf 'junk' >"$1/$sha_mini/wordpress/wp-includes/kses.php"
}
marker_of() { cat "$1/.verified" 2>/dev/null || true; }
only_the_core() { # only_the_core <cache> <sha>   nothing else is left in the cache: no work dir, no lock
  [ "$(ls -A "$1")" = "$2" ]
}

# A core that is verified is used as it is: the same directory, untouched, no download.
newcache reuse
expect "a cold cache is filled from the download" 0 "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S"
touch -t 200001010000 "$C/$sha_mini/.verified"
printf 'kept' >"$C/$sha_mini/sentinel"
ino_before="$(inode_of "$C/$sha_mini")"
mtime_before="$(ls -l "$C/$sha_mini/.verified" | awk '{print $6, $7, $8}')"
S2="$tmp/state-reuse-second"
mkdir -p "$S2"
: >"$S2/go"
expect "a verified core in the cache is used as it is" 0 "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S2"
if [ ! -e "$S2/curl.log" ]; then ok "  and nothing was downloaded"; else bad "  and nothing was downloaded"; fi
if [ "$(inode_of "$C/$sha_mini")" = "$ino_before" ]; then ok "  and it is the same directory (inode $ino_before)"; else bad "  and it is the same directory (inode $ino_before, now $(inode_of "$C/$sha_mini"))"; fi
if [ "$(ls -l "$C/$sha_mini/.verified" | awk '{print $6, $7, $8}')" = "$mtime_before" ]; then ok "  and its marker kept its mtime ($mtime_before)"; else bad "  and its marker kept its mtime"; fi
if [ "$(cat "$C/$sha_mini/sentinel" 2>/dev/null || true)" = "kept" ]; then ok "  and a file added to it is still there"; else bad "  and a file added to it is still there"; fi
if only_the_core "$C" "$sha_mini"; then ok "  and no work dir or lock is left"; else bad "  and no work dir or lock is left ($(ls -A "$C"))"; fi

# The same with a real pinned core, a url nothing answers on, and the real php.
if [ -f "$cache_dir/$sha73/.verified" ]; then
  mkdir -p "$tmp/cache-reuse-real"
  cp -R "$cache_dir/$sha73" "$tmp/cache-reuse-real/$sha73"
  printf '7.1.3 %s https://127.0.0.1:9/wordpress.tar.gz\n' "$sha73" >"$tmp/cores_unreachable73.txt"
  ino_real="$(inode_of "$tmp/cache-reuse-real/$sha73")"
  expect "a verified real core is used without any download" 0 PAGE_KSES_CACHE="$tmp/cache-reuse-real" PAGE_KSES_CORES_FILE="$tmp/cores_unreachable73.txt" PAGE_KSES_VERSIONS="7.1.3" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
  if [ "$(inode_of "$tmp/cache-reuse-real/$sha73")" = "$ino_real" ]; then ok "  and it is the same directory"; else bad "  and it is the same directory"; fi
else
  bad "setup: the 7.1.3 core is not in the cache for the reuse case"
fi

# A cached core that fails its check (its marker names another sha256) is replaced.
mkdir -p "$tmp/cache-corrupt/$sha62/wordpress/wp-includes"
printf '%s' "$wrong" >"$tmp/cache-corrupt/$sha62/.verified"
printf 'junk' >"$tmp/cache-corrupt/$sha62/wordpress/wp-includes/kses.php"
printf 'x' >"$tmp/cache-corrupt/$sha62/left-by-the-bad-copy"
expect "a cached core with the wrong sha256 in its marker is replaced and the check passes" 0 PAGE_KSES_CACHE="$tmp/cache-corrupt" PAGE_KSES_VERSIONS="6.2" PAGE_KSES_MARKUP="$tmp/layout_ok.json"
if [ "$(marker_of "$tmp/cache-corrupt/$sha62")" = "$sha62" ]; then ok "  and the core now carries the pin"; else bad "  and the core now carries the pin"; fi
if [ ! -e "$tmp/cache-corrupt/$sha62/left-by-the-bad-copy" ] && [ "$(wc -c <"$tmp/cache-corrupt/$sha62/wordpress/wp-includes/kses.php")" -gt 1000 ]; then ok "  and it is the downloaded core, not the bad copy"; else bad "  and it is the downloaded core, not the bad copy"; fi
if only_the_core "$tmp/cache-corrupt" "$sha62"; then ok "  and no work dir, lock or old copy is left"; else bad "  and no work dir, lock or old copy is left ($(ls -A "$tmp/cache-corrupt"))"; fi

# Two runs, one empty cache, both holding a finished download at the rename that publishes it.
newcache race-cold
rm -f "$S/go"
spawn "$tmp/race_cold_a.out" "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S"
pa=$SPAWNED
spawn "$tmp/race_cold_b.out" "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S"
pb=$SPAWNED
n=0
while [ "$(arrivals "$S")" -lt 2 ] && [ "$n" -lt 200 ]; do
  sleep 0.1
  n=$((n + 1))
done
if [ "$(arrivals "$S")" -eq 2 ]; then ok "two runs are at the publishing rename together"; else bad "two runs are at the publishing rename together (arrivals: $(arrivals "$S"))"; fi
: >"$S/go"
wait "$pa"
rc_a=$?
wait "$pb"
rc_b=$?
if [ "$rc_a" -eq 0 ] && [ "$rc_b" -eq 0 ]; then ok "  and both runs pass (exit $rc_a, $rc_b)"; else bad "  and both runs pass (exit $rc_a, $rc_b)"; sed 's/^/     | /' "$tmp/race_cold_a.out" "$tmp/race_cold_b.out" | head -12; fi
if [ "$(marker_of "$C/$sha_mini")" = "$sha_mini" ]; then ok "  and the cache holds a verified core"; else bad "  and the cache holds a verified core"; fi
if [ ! -e "$C/$sha_mini/core" ] && [ ! -e "$C/$sha_mini/$sha_mini" ]; then ok "  and one core was not put inside the other"; else bad "  and one core was not put inside the other ($(ls -A "$C/$sha_mini"))"; fi
if only_the_core "$C" "$sha_mini"; then ok "  and no work dir or lock is left"; else bad "  and no work dir or lock is left ($(ls -A "$C"))"; fi

# A run is reading the cached core when a second run, which started on an empty cache and
# is still downloading, gets to the end of its download. The core under the reader must not move.
newcache read
S="$tmp/state-read"
spawn "$tmp/read_b.out" "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S" SHIM_CURL_WAIT="$S/b.go"
pb=$SPAWNED
if await "$S/curl.log"; then ok "a second run is held in the middle of its download"; else bad "a second run is held in the middle of its download"; fi
spawn "$tmp/read_a.out" "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S" SHIM_READER_FLAG="$S/a.reading" SHIM_READER_DONE="$S/b.done"
pa=$SPAWNED
if await "$S/a.reading"; then ok "  and another run is reading the core it published"; else bad "  and another run is reading the core it published"; fi
: >"$S/b.go"
wait "$pb"
rc_b=$?
: >"$S/b.done"
wait "$pa"
rc_a=$?
if [ "$rc_b" -eq 0 ]; then ok "  and the late run passes (exit $rc_b)"; else bad "  and the late run passes (exit $rc_b)"; sed 's/^/     | /' "$tmp/read_b.out" | head -8; fi
if [ "$rc_a" -eq 0 ]; then ok "  and the reader never lost its core (exit $rc_a)"; else bad "  and the reader never lost its core (exit $rc_a)"; sed 's/^/     | /' "$tmp/read_a.out" | head -8; fi
if only_the_core "$C" "$sha_mini"; then ok "  and no work dir or lock is left"; else bad "  and no work dir or lock is left ($(ls -A "$C"))"; fi

# Two runs find the same corrupt core and both go to replace it.
newcache race-corrupt
rm -f "$S/go"
corrupt_mini "$C"
spawn "$tmp/race_corrupt_a.out" "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S"
pa=$SPAWNED
spawn "$tmp/race_corrupt_b.out" "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S"
pb=$SPAWNED
n=0
while [ "$(arrivals "$S")" -lt 2 ] && [ "$n" -lt 200 ]; do
  sleep 0.1
  n=$((n + 1))
done
if [ "$(arrivals "$S")" -eq 2 ]; then ok "two runs are at the publishing rename with a corrupt core in the way"; else bad "two runs are at the publishing rename with a corrupt core in the way (arrivals: $(arrivals "$S"))"; fi
: >"$S/go"
wait "$pa"
rc_a=$?
wait "$pb"
rc_b=$?
if [ "$rc_a" -eq 0 ] && [ "$rc_b" -eq 0 ]; then ok "  and both runs pass (exit $rc_a, $rc_b)"; else bad "  and both runs pass (exit $rc_a, $rc_b)"; sed 's/^/     | /' "$tmp/race_corrupt_a.out" "$tmp/race_corrupt_b.out" | head -12; fi
if [ "$(marker_of "$C/$sha_mini")" = "$sha_mini" ] && grep -q 'miniature kses' "$C/$sha_mini/wordpress/wp-includes/kses.php"; then ok "  and the corrupt core was replaced by a verified one"; else bad "  and the corrupt core was replaced by a verified one"; fi
if [ ! -e "$C/$sha_mini/core" ] && [ ! -e "$C/$sha_mini/$sha_mini" ]; then ok "  and one core was not put inside the other"; else bad "  and one core was not put inside the other ($(ls -A "$C/$sha_mini"))"; fi
if only_the_core "$C" "$sha_mini"; then ok "  and no work dir, lock or old copy is left"; else bad "  and no work dir, lock or old copy is left ($(ls -A "$C"))"; fi

# A run that is stopped while it holds the lock gives it back.
newcache lock-term
rm -f "$S/go"
corrupt_mini "$C"
spawn "$tmp/lock_term.out" "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S" SHIM_HOLD_FROM=2
pt=$SPAWNED
if await "$S/arrived.$pt.2"; then ok "a run is parked while it replaces the corrupt core"; else bad "a run is parked while it replaces the corrupt core"; fi
if [ -d "$C/.lock.$sha_mini" ] && [ "$(cat "$C/.lock.$sha_mini/pid" 2>/dev/null || true)" = "$pt" ]; then ok "  and it holds the lock, with its pid in it"; else bad "  and it holds the lock, with its pid in it"; fi
kill -TERM "$pt"
: >"$S/go"
wait "$pt"
rc_t=$?
if [ "$rc_t" -eq 143 ]; then ok "  and the run stops on TERM (exit $rc_t)"; else bad "  and the run stops on TERM (exit $rc_t)"; fi
if [ ! -e "$C/.lock.$sha_mini" ]; then ok "  and the lock is gone"; else bad "  and the lock is gone"; fi
if [ -z "$(ls -A "$C" | grep '^\.tmp\.' || true)" ]; then ok "  and no work dir is left"; else bad "  and no work dir is left"; fi

# Locks that nobody is going to release. A corrupt core is what makes a run take the lock.
dead_pid="$(sh -c 'echo $$')"
if kill -0 "$dead_pid" 2>/dev/null; then bad "setup: pid $dead_pid is still running"; fi

newcache lock-dead
corrupt_mini "$C"
mkdir "$C/.lock.$sha_mini"
printf '%s\n' "$dead_pid" >"$C/.lock.$sha_mini/pid"
expect "a lock left by a run that is gone is removed and the core is replaced" 0 "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S" PAGE_KSES_LOCK_WAIT=5
said "  and it says it removed the lock" "removed the lock"
if [ "$(marker_of "$C/$sha_mini")" = "$sha_mini" ] && only_the_core "$C" "$sha_mini"; then ok "  and the core is replaced and no lock is left"; else bad "  and the core is replaced and no lock is left ($(ls -A "$C"))"; fi

newcache lock-live
corrupt_mini "$C"
mkdir "$C/.lock.$sha_mini"
printf '%s\n' "$$" >"$C/.lock.$sha_mini/pid"
started=$SECONDS
expect "a lock held by a run that is alive is waited on for the bound and then reported" nonzero "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S" PAGE_KSES_LOCK_WAIT=2
waited=$((SECONDS - started))
said "  and it names the lock and who holds it" "held by pid $$, which is running"
said "  and it says how to clear it" "rm -rf $C/.lock.$sha_mini"
if [ "$waited" -lt 30 ]; then ok "  and it did not wait without a bound (${waited}s for 2 polls)"; else bad "  and it did not wait without a bound (${waited}s for 2 polls)"; fi
if [ -d "$C/.lock.$sha_mini" ] && [ "$(marker_of "$C/$sha_mini")" = "$wrong" ]; then ok "  and it left the lock and the core alone"; else bad "  and it left the lock and the core alone"; fi

newcache lock-empty
corrupt_mini "$C"
mkdir "$C/.lock.$sha_mini"
expect "a lock with no owner recorded in it is reported, not guessed at" nonzero "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S" PAGE_KSES_LOCK_WAIT=2
said "  and it says no owner is recorded" "no owner is recorded"
if [ -d "$C/.lock.$sha_mini" ]; then ok "  and it left the lock alone"; else bad "  and it left the lock alone"; fi

newcache lock-stuck-reclaim
corrupt_mini "$C"
mkdir "$C/.lock.$sha_mini" "$C/.lock.$sha_mini.reclaim"
printf '%s\n' "$dead_pid" >"$C/.lock.$sha_mini/pid"
expect "a dead owner whose reclaim lock is stuck is reported" nonzero "${hermetic[@]}" PAGE_KSES_CACHE="$C" SHIM_CACHE="$C" SHIM_DIR="$S" PAGE_KSES_LOCK_WAIT=2
said "  and it says the owner is gone" "left by pid $dead_pid, which is gone"
said "  and it names the reclaim lock to clear" "$C/.lock.$sha_mini.reclaim"

expect "a lock wait of 0 is red" nonzero PAGE_KSES_LOCK_WAIT=0 PAGE_KSES_MARKUP="$tmp/layout_ok.json"
said "  and it says what the setting must be" "PAGE_KSES_LOCK_WAIT must be a whole number"
expect "a lock wait that is not a number is red" nonzero PAGE_KSES_LOCK_WAIT=soon PAGE_KSES_MARKUP="$tmp/layout_ok.json"
expect "an empty lock wait is red" nonzero PAGE_KSES_LOCK_WAIT= PAGE_KSES_MARKUP="$tmp/layout_ok.json"

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
