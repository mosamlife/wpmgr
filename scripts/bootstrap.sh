#!/usr/bin/env bash
# First-time developer setup for WPMgr.
set -euo pipefail
cd "$(dirname "$0")/.."

# node@22 is keg-only on macOS Homebrew; surface it if present.
if [ -d /opt/homebrew/opt/node@22/bin ]; then
  export PATH="/opt/homebrew/opt/node@22/bin:$PATH"
fi

# core.hooksPath is repo-local config and config is NOT committed, so a fresh
# clone has the hook in its tree and git ignoring it. This is the step that turns
# it on, and it is FIRST here on purpose: if any later step fails, the clone is
# still protected.
#
# It installs an ABSOLUTE path. A relative one was tried and measured wrong: git
# resolves a relative core.hooksPath against the top of whichever working tree
# is running the hook, so it only finds .githooks in a tree checked out at or
# after the hook's commit. Across the checkouts on this machine the hook was
# present in 1 of 10 by that rule, with config reading "installed" in all ten.
echo "==> Installing the pre-push hook (refuses a push that lands on main)"
scripts/claude/git-hooks.sh install

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
