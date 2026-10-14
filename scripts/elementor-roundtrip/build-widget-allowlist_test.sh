#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
node_bin="$(command -v node || true)"
php_bin="$(command -v php || true)"
[ -n "$node_bin" ] || { echo 'widget-allowlist_test: node not found' >&2; exit 2; }
[ -n "$php_bin" ] || { echo 'widget-allowlist_test: php not found' >&2; exit 2; }
"$php_bin" "$here/dump-widgets_test.php"
"$php_bin" "$here/dump-widgets-contract_test.php"
"$node_bin" "$here/build-widget-allowlist_test.mjs"
