#!/usr/bin/env bash
# Scratch probe for the PR #413 review: harness-reap.sh argument handling and
# the dry-run report. Nothing here passes --apply, so nothing is deleted.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
R="$here/harness-reap.sh"

echo "== --cache-gb with a non-numeric value (dry run)"
bash "$R" --cache-gb abc 2>&1 | sed -n '/Go build cache/,/module cache/p' | sed 's/^/  /'

echo
echo "== --cache-gb with the flag last and no value (dry run)"
bash "$R" --cache-gb 2>&1 | sed -n '/Go build cache/,/module cache/p' | sed 's/^/  /'

echo
echo "== unknown flag"
bash "$R" --nope 2>&1 | sed 's/^/  /'
echo "  exit: ${PIPESTATUS[0]}"

echo
echo "== plain dry run, worktrees section"
bash "$R" --worktrees-only 2>&1 | sed 's/^/  /'
