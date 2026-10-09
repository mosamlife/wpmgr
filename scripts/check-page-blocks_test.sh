#!/usr/bin/env bash
# scripts/check-page-blocks_test.sh
#
# Regression suite for scripts/check-page-blocks.sh. Proves the check goes red
# on invalid markup, on zero cases, on zero blocks, and when its own commands
# fail, and that it does not go red on valid markup. Needs node and the
# installed (or installable) wp-latest harness dir; run it BEFORE the real check
# so a guard that fails open cannot pass.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
check="$here/check-page-blocks.sh"
tmp="$(mktemp -d -t page-blocks-test.XXXXXX)"
trap 'rm -rf "$tmp"' EXIT

pass=0
failed=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { failed=$((failed + 1)); echo "FAIL $1"; }

# expect <name> <want: 0|nonzero> <env...> -- runs the check, one version dir.
expect() {
  local name="$1" want="$2"
  shift 2
  local out rc
  out="$(env PAGE_BLOCKS_VERSIONS="${VERS:-wp-latest}" "$@" "$check" 2>&1)"
  rc=$?
  if [ "$want" = 0 ] && [ "$rc" -eq 0 ]; then ok "$name"
  elif [ "$want" = nonzero ] && [ "$rc" -ne 0 ]; then ok "$name"
  else bad "$name (exit $rc, wanted $want)"; echo "$out" | sed 's/^/     | /' | head -8
  fi
  LAST_OUT="$out"
}

mk() { printf '%s' "$2" > "$tmp/$1.json"; }

good='{"cases":[{"name":"good","content":"<!-- wp:heading {\"level\":3} -->\n<h3 class=\"wp-block-heading\">Hi &amp; bye</h3>\n<!-- /wp:heading -->\n\n<!-- wp:paragraph -->\n<p>Text</p>\n<!-- /wp:paragraph -->"}]}'
# Heading level attribute disagrees with the tag: the validator must reject it.
badblock='{"cases":[{"name":"bad","content":"<!-- wp:heading {\"level\":3} -->\n<h2 class=\"wp-block-heading\">Hi</h2>\n<!-- /wp:heading -->"}]}'
# Unescaped ampersand is not what the serializer would emit.
badamp='{"cases":[{"name":"amp","content":"<!-- wp:paragraph -->\n<p>a < b</p>\n<!-- /wp:paragraph -->"}]}'
# Valid case followed by an invalid one: one bad block must fail the whole run.
onebad='{"cases":[{"name":"good","content":"<!-- wp:paragraph -->\n<p>ok</p>\n<!-- /wp:paragraph -->"},{"name":"bad","content":"<!-- wp:heading {\"level\":4} -->\n<h2 class=\"wp-block-heading\">x</h2>\n<!-- /wp:heading -->"}]}'
# Plain text with no block comments parses to a freeform block, not a core block.
freeform='{"cases":[{"name":"plain","content":"just text, no block comments"}]}'
nocases='{"cases":[]}'
emptycontent='{"cases":[{"name":"empty","content":""}]}'

mk good "$good"; mk badblock "$badblock"; mk badamp "$badamp"; mk onebad "$onebad"
mk freeform "$freeform"; mk nocases "$nocases"; mk emptycontent "$emptycontent"
printf '' > "$tmp/empty.json"
printf 'not json' > "$tmp/notjson.json"

# Must not over-fire.
expect "valid hand-written markup is accepted" 0 PAGE_BLOCKS_MARKUP="$tmp/good.json"
case "$LAST_OUT" in *"0 invalid"*) ok "green run reports its block count" ;; *) bad "green run printed no count: $LAST_OUT" ;; esac
VERS="wp-6.2 wp-latest" expect "the real generator output is accepted on 6.2 and latest" 0 VERS_UNUSED=1
case "$LAST_OUT" in *"== wp-6.2"*"== wp-latest"*) ok "both version dirs ran" ;; *) bad "both version dirs did not run: $LAST_OUT" ;; esac

# Must go red.
expect "heading level/tag mismatch is rejected" nonzero PAGE_BLOCKS_MARKUP="$tmp/badblock.json"
case "$LAST_OUT" in *INVALID*) ok "the rejection names the invalid block" ;; *) bad "no INVALID line: $LAST_OUT" ;; esac
expect "an invalid block among valid cases is rejected" nonzero PAGE_BLOCKS_MARKUP="$tmp/onebad.json"
expect "raw less-than in text is rejected" nonzero PAGE_BLOCKS_MARKUP="$tmp/badamp.json"
expect "freeform (no block comments) is rejected" nonzero PAGE_BLOCKS_MARKUP="$tmp/freeform.json"
expect "zero cases is red, not green" nonzero PAGE_BLOCKS_MARKUP="$tmp/nocases.json"
expect "empty content (zero blocks) is red" nonzero PAGE_BLOCKS_MARKUP="$tmp/emptycontent.json"
expect "an empty markup file is red" nonzero PAGE_BLOCKS_MARKUP="$tmp/empty.json"
expect "unparseable markup is red" nonzero PAGE_BLOCKS_MARKUP="$tmp/notjson.json"
expect "a missing node binary is red" nonzero PAGE_BLOCKS_MARKUP="$tmp/good.json" PAGE_BLOCKS_NODE="$tmp/no-such-node"
expect "a node that fails is red" nonzero PAGE_BLOCKS_MARKUP="$tmp/good.json" PAGE_BLOCKS_NODE=/usr/bin/false
VERS="no-such-version" expect "a missing version dir is red" nonzero PAGE_BLOCKS_MARKUP="$tmp/good.json"
VERS=" " expect "zero versions is red" nonzero PAGE_BLOCKS_MARKUP="$tmp/good.json"

echo
echo "passed: $pass, failed: $failed"
[ "$failed" -eq 0 ]
