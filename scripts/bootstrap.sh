#!/usr/bin/env bash
# First-time developer setup for WPMgr.
set -euo pipefail
cd "$(dirname "$0")/.."

# node@22 is keg-only on macOS Homebrew; surface it if present.
if [ -d /opt/homebrew/opt/node@22/bin ]; then
  export PATH="/opt/homebrew/opt/node@22/bin:$PATH"
fi

# core.hooksPath is repo-local config and config is NOT committed, so a fresh
# clone has the hook in its tree and git ignoring it. This is the line that turns
# it on, and it is FIRST here on purpose: if any later step fails, the clone is
# still protected. Relative, not absolute - git resolves core.hooksPath against
# the top of the working tree, so one setting covers the main checkout and every
# linked worktree, each finding its own copy.
echo "==> Installing the pre-push hook (refuses a push that lands on main)"
git config core.hooksPath .githooks
echo "    core.hooksPath = $(git config --get core.hooksPath)"

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
