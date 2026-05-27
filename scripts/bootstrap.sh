#!/usr/bin/env bash
# First-time developer setup for WPMgr.
set -euo pipefail
cd "$(dirname "$0")/.."

# node@22 is keg-only on macOS Homebrew; surface it if present.
if [ -d /opt/homebrew/opt/node@22/bin ]; then
  export PATH="/opt/homebrew/opt/node@22/bin:$PATH"
fi

echo "==> Checking toolchain"
command -v go >/dev/null || { echo "go not found"; exit 1; }
command -v pnpm >/dev/null || { echo "pnpm not found"; exit 1; }

echo "==> Installing JS workspace deps"
pnpm install

echo "==> Installing agent (composer) deps"
if command -v composer >/dev/null; then
  (cd apps/agent && composer install)
else
  echo "composer not found — skipping agent deps"
fi

echo "==> Syncing Go workspace"
go work sync

if [ ! -f .env ]; then
  cp .env.example .env
  echo "==> Wrote .env from .env.example (edit secrets before running)"
fi

echo "==> Done. Run 'make dev' to start the stack."
