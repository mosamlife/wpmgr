#!/usr/bin/env bash
# Scratch probe for the PR #413 review: how harness-reap.sh's ceiling test
# behaves when --cache-gb is not a number. Nothing destructive.
set -uo pipefail
gb=5
for CACHE_GB in 10 abc "" "0x10"; do
  if [[ "$gb" -ge "$CACHE_GB" ]] 2>/dev/null; then r="CLEANS"; else r="leaves it"; fi
  printf 'gb=%s ceiling=%-6q -> %s\n' "$gb" "$CACHE_GB" "$r"
done
echo
echo "and the arithmetic context is live:"
CACHE_GB='q[$(echo INJECTED-COMMAND-RAN >&2)]'
[[ "$gb" -ge "$CACHE_GB" ]] 2>/dev/null && echo "  comparison returned true"
