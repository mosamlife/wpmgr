#!/usr/bin/env bash
# scripts/check-page-blocks.sh
#
# Proves the agent's generated page markup (PageCreateBuilder, block editor
# output) is valid under the real @wordpress/blocks parser and validator, once
# per pinned WordPress package set: scripts/page-blocks/wp-6.2 (the minimum
# supported WordPress) and scripts/page-blocks/wp-latest (current). Each dir
# holds a package.json and a package-lock.json; installs use `npm ci`.
#
# Fails, never skips, when: php/node/npm cannot be found, the generator fails,
# a version dir is missing or its install fails, the validator finds an invalid
# block, a case parses to zero blocks, or zero cases / zero blocks are checked.
#
# Test seams (used by check-page-blocks_test.sh only):
#   PAGE_BLOCKS_MARKUP   use this markup JSON instead of running the generator
#   PAGE_BLOCKS_NODE     node binary to run
#   PAGE_BLOCKS_VERSIONS space separated version dirs (default: wp-6.2 wp-latest)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
dir="$here/page-blocks"
versions="${PAGE_BLOCKS_VERSIONS:-wp-6.2 wp-latest}"

NODE="${PAGE_BLOCKS_NODE:-$(command -v node || true)}"
NPM="$(command -v npm || true)"
if [ -z "$NODE" ] || { [ "${PAGE_BLOCKS_NODE:-}" != "" ] && [ ! -x "$NODE" ]; }; then
  echo "check-page-blocks: node not found" >&2
  exit 1
fi

markup="${PAGE_BLOCKS_MARKUP:-}"
if [ -z "$markup" ]; then
  PHP="$(command -v php || true)"
  if [ -z "$PHP" ]; then
    echo "check-page-blocks: php not found" >&2
    exit 1
  fi
  markup="$(mktemp -t page-blocks.XXXXXX)"
  trap 'rm -f "$markup"' EXIT
  "$PHP" "$dir/generate.php" "$markup"
fi
if [ ! -s "$markup" ]; then
  echo "check-page-blocks: no markup produced at $markup" >&2
  exit 1
fi

fail=0
ran=0
for v in $versions; do
  vd="$dir/$v"
  if [ ! -f "$vd/package-lock.json" ]; then
    echo "check-page-blocks: $vd/package-lock.json missing" >&2
    exit 1
  fi
  if [ ! -d "$vd/node_modules/@wordpress/blocks" ]; then
    if [ -z "$NPM" ]; then
      echo "check-page-blocks: npm not found" >&2
      exit 1
    fi
    (cd "$vd" && "$NPM" ci --legacy-peer-deps --no-audit --no-fund >/dev/null) || {
      echo "check-page-blocks: npm ci failed in $vd" >&2
      exit 1
    }
  fi
  echo "== $v"
  if (cd "$vd" && "$NODE" "$dir/validate.mjs" "$markup"); then
    :
  else
    echo "check-page-blocks: FAILED on $v" >&2
    fail=1
  fi
  ran=$((ran + 1))
done

if [ "$ran" -eq 0 ]; then
  echo "check-page-blocks: no versions were checked" >&2
  exit 1
fi
exit "$fail"
