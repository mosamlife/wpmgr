#!/usr/bin/env bash
# scripts/plugincheck-project-name.sh [<checkout dir>]
#
# Prints the docker compose project name tools/plugincheck/run.sh runs under.
#
# docker compose scopes the containers it starts, and the ones `down -v` removes,
# to a project name. The harness used to run under the fixed name of its own
# directory, so two runs from two checkouts of this repository (several agents,
# each in its own worktree) removed each other's containers. The name here is
# derived from the checkout instead:
#
#   plugincheck-<the checkout directory's name, made safe>-<8 hex digits of the
#   sha256 of its physical path>
#
# The digits make it unique per checkout, the symlinks and dot-dot segments of
# the path do not change it, and the directory name only makes `docker ps`
# readable. COMPOSE_PROJECT_NAME, when set and not empty, is used instead; a name
# compose would refuse is refused here, before docker is touched.
#
# The checkout is the one that holds this script, or the directory given as the
# first argument (the test uses that). Exits 2, naming the cause, when the
# directory does not exist, when neither sha256sum nor shasum is installed, or
# when the override is not a valid project name.
set -euo pipefail

die() { echo "plugincheck-project-name: $*" >&2; exit 2; }

# compose accepts lowercase letters, digits, dashes and underscores, starting with a letter or a digit.
if [ -n "${COMPOSE_PROJECT_NAME:-}" ]; then
  case "$COMPOSE_PROJECT_NAME" in
    [a-z0-9]*) ;;
    *) die "COMPOSE_PROJECT_NAME '$COMPOSE_PROJECT_NAME' must start with a lowercase letter or a digit" ;;
  esac
  if [ -n "$(printf '%s' "$COMPOSE_PROJECT_NAME" | LC_ALL=C tr -d 'a-z0-9_-')" ]; then
    die "COMPOSE_PROJECT_NAME '$COMPOSE_PROJECT_NAME' may hold only lowercase letters, digits, dashes and underscores"
  fi
  printf '%s\n' "$COMPOSE_PROJECT_NAME"
  exit 0
fi

if [ $# -ge 1 ]; then
  dir="$1"
else
  case "$0" in
    */*) dir="${0%/*}/.." ;;
    *) dir=".." ;;
  esac
fi
[ -d "$dir" ] || die "not a directory: $dir"
checkout="$(cd "$dir" && pwd -P)"

if command -v sha256sum >/dev/null 2>&1; then
  digest="$(printf '%s' "$checkout" | sha256sum | cut -c1-8)"
elif command -v shasum >/dev/null 2>&1; then
  digest="$(printf '%s' "$checkout" | shasum -a 256 | cut -c1-8)"
else
  die "neither sha256sum nor shasum is installed, so a name that is unique per checkout cannot be made"
fi
case "$digest" in
  [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
  *) die "the sha256 of '$checkout' did not give 8 hex digits: '$digest'" ;;
esac

slug="$(printf '%s' "${checkout##*/}" | LC_ALL=C tr 'A-Z' 'a-z' | LC_ALL=C tr -c 'a-z0-9_-' '-' | cut -c1-24 | sed -e 's/^[-_]*//' -e 's/[-_]*$//')"
[ -n "$slug" ] || slug=checkout

printf 'plugincheck-%s-%s\n' "$slug" "$digest"
