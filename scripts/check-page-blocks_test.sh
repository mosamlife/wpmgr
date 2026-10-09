#!/usr/bin/env bash
# scripts/check-page-blocks_test.sh
#
# Regression suite for scripts/check-page-blocks.sh. Proves the check goes red
# on invalid markup, on zero cases, on zero blocks, and when its own commands
# fail, and that it does not go red on valid markup. It also proves the generator
# the check runs (scripts/page-blocks/generate.php, shared with the kses check)
# loads the agent through the agent's own class resolver: a class the builder
# starts to use is found with no change to the generator, and a class the
# resolver cannot find is red. Needs node and the installed (or installable)
# wp-latest harness dir; run it BEFORE the real check so a guard that fails open
# cannot pass.
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

# An outline case marked "refused" pins a refusal: the generator fails when
# the builder accepts it or answers another code, and a held refusal renders
# nothing. These run the real generator and builder.
plain='{"name":"plain","outline":[{"type":"paragraph","text":"ok"}]}'
printf '[%s,{"name":"held","refused":"link_invalid","outline":[{"type":"buttons","buttons":[{"text":"Go","url":"/a:b"}]}]}]' "$plain" > "$tmp/outlines_held.json"
printf '[%s,{"name":"accepted","refused":"link_invalid","outline":[{"type":"buttons","buttons":[{"text":"Go","url":"/a"}]}]}]' "$plain" > "$tmp/outlines_accepted.json"
printf '[%s,{"name":"othercode","refused":"layout_invalid","outline":[{"type":"buttons","buttons":[{"text":"Go","url":"/a:b"}]}]}]' "$plain" > "$tmp/outlines_othercode.json"
printf '[%s,{"name":"badcode","refused":true,"outline":[{"type":"buttons","buttons":[{"text":"Go","url":"/a:b"}]}]}]' "$plain" > "$tmp/outlines_badcode.json"
expect "a refusal the builder holds is accepted" 0 PAGE_BLOCKS_OUTLINES="$tmp/outlines_held.json"
case "$LAST_OUT" in *"1 expected refusals held"*) ok "the held refusal is counted" ;; *) bad "no held-refusal count: $LAST_OUT" ;; esac
expect "an outline marked refused that the builder accepts is red" nonzero PAGE_BLOCKS_OUTLINES="$tmp/outlines_accepted.json"
case "$LAST_OUT" in *"must be refused link_invalid, the builder answered ok"*) ok "the accepted refusal is named" ;; *) bad "accepted refusal not named: $LAST_OUT" ;; esac
expect "an outline refused with another code is red" nonzero PAGE_BLOCKS_OUTLINES="$tmp/outlines_othercode.json"
expect "a refused value that is not a code is red" nonzero PAGE_BLOCKS_OUTLINES="$tmp/outlines_badcode.json"

# The generator names no agent class file: it registers the agent's own class
# resolver, as the plugin's main file does, and the resolver finds whatever the
# builder uses. These run the real generator against copies of the real agent
# tree (PAGE_BLOCKS_AGENT_DIR), so a planted dependency never touches the tree
# under test. The same reference is planted twice, in a builder that is
# otherwise the real one: once with the file that declares its class in the tree
# (the resolver must find it, with no change to the generator) and once without
# (the run must go red and name the class). The two trees differ by that one
# file, so the green run cannot pass unless the planted reference ran.
php_bin="$(command -v php || true)"
if [ -z "$php_bin" ]; then
  bad "php not found, so the generator cannot be run"
else
  generator="$here/page-blocks/generate.php"
  agent_src="$here/../apps/agent"
  probe_class='\WPMgr\Agent\Abilities\Builders\DriftProbe'

  # gen <name> <want: 0|nonzero> <agent dir; empty is the real tree>
  gen() {
    local name="$1" want="$2" dir="$3" out rc
    rm -f "$tmp/gen-blocks.json" "$tmp/gen-classic.json"
    out="$(env PAGE_BLOCKS_AGENT_DIR="$dir" "$php_bin" "$generator" "$tmp/gen-blocks.json" "$tmp/gen-classic.json" 2>&1)"
    rc=$?
    if [ "$want" = 0 ] && [ "$rc" -eq 0 ]; then ok "$name"
    elif [ "$want" = nonzero ] && [ "$rc" -ne 0 ]; then ok "$name"
    else bad "$name (exit $rc, wanted $want)"; echo "$out" | sed 's/^/     | /' | head -8
    fi
    LAST_OUT="$out"
  }

  # said <name> <text>   LAST_OUT must contain the text: a red must be red for the
  # right reason, not because something else broke.
  said() {
    case "$LAST_OUT" in
      *"$2"*) ok "$1" ;;
      *) bad "$1 (expected output to contain: $2)"; echo "$LAST_OUT" | sed 's/^/     | /' | head -8 ;;
    esac
  }

  # copy_agent <new dir>   a copy of the real agent's includes/ in <new dir>.
  copy_agent() { mkdir "$1" && cp -R "$agent_src/includes" "$1/"; }

  # plant <new dir> <yes|no>   the real agent tree whose builder file also runs a
  # call on a class that is not in the tree; with yes, the file declaring it is
  # added too.
  plant() {
    copy_agent "$1" || { bad "cannot copy the agent tree to $1"; return 1; }
    printf '\n%s::touch();\n' "$probe_class" >>"$1/includes/abilities/class-page-create-builder.php" || { bad "cannot plant the call in $1"; return 1; }
    if [ "$2" = yes ]; then
      printf '%s\n' '<?php' 'declare(strict_types=1);' 'namespace WPMgr\Agent\Abilities\Builders;' 'final class DriftProbe' '{' '    public static function touch(): void' '    {' '    }' '}' >"$1/includes/abilities/builders/class-drift-probe.php" || { bad "cannot plant the class in $1"; return 1; }
    fi
  }

  # Must not over-fire.
  gen "the generator loads the real agent through its class resolver" 0 ""
  if printf '%s\n' "$LAST_OUT" | grep -Eq 'wrote [1-9][0-9]* block cases and [1-9][0-9]* classic cases'; then ok "the real run reports its cases"; else bad "the real run reported no cases: $LAST_OUT"; fi
  if [ -s "$tmp/gen-blocks.json" ] && [ -s "$tmp/gen-classic.json" ]; then ok "the real run wrote both markup files"; else bad "the real run did not write both markup files"; fi
  if plant "$tmp/agent_new_class" yes; then
    gen "a class the builder starts to use is found with no change to the generator" 0 "$tmp/agent_new_class"
  fi

  # Must go red.
  if plant "$tmp/agent_missing_class" no; then
    gen "a class the resolver cannot find is red" nonzero "$tmp/agent_missing_class"
    said "the missing class is named" "DriftProbe"
  fi
  if copy_agent "$tmp/agent_no_builder" && rm "$tmp/agent_no_builder/includes/abilities/class-page-create-builder.php"; then
    gen "an agent tree without the builder is red" nonzero "$tmp/agent_no_builder"
    said "the generator says it cannot load the builder" "the agent class resolver cannot load WPMgr\\Agent\\Abilities\\PageCreateBuilder"
  else
    bad "cannot make the agent tree without the builder"
  fi
  gen "an agent directory that does not exist is red" nonzero "$tmp/no-such-agent"
  said "the generator names the resolver file it did not find" "class resolver is not at $tmp/no-such-agent/includes/class-autoloader.php"
fi

echo
echo "passed: $pass, failed: $failed"
[ "$failed" -eq 0 ]
