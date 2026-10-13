#!/usr/bin/env bash
# Usage: build-widget-allowlist.sh <dump per pinned version> [output.json]
# Dumps may be JSON files or run.sh logs with one WIDGETS_JSON marker block.
# Output defaults to apps/api/internal/elementornative/core_widgets.json.
# Shared references select a definition index and the versions allowed on that widget.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
node_bin="$(command -v node || true)"
[ -n "$node_bin" ] || { echo 'widget-allowlist: node not found' >&2; exit 2; }
exec "$node_bin" "$here/build-widget-allowlist.mjs" "$@"
