#!/usr/bin/env bash
# scripts/check-schema-drift.sh
#
# Fails when apps/api/db/schema.sql and apps/api/migrations/ describe different
# schemas: tables, columns, constraints, indexes, column comments, function
# bodies, and RLS enablement and policies.
#
# ---------------------------------------------------------------------------
# WHY THIS EXISTS
# ---------------------------------------------------------------------------
#
# The migrations are what production runs. schema.sql is sqlc's input and the
# file people read, and it fell 23 tables and 54 policies behind them, 13 of
# those policies RESTRICTIVE site_scope gates. A grep of schema.sql then
# answered "this table is not site-scoped" for tables that were. The gap was
# found by hand, one grep count at a time.
#
# This guard asks the question directly. ptah-compat, the Atlas-compatible CLI
# of Ptah (https://ptah.run), replays every migration on a throwaway database,
# reads schema.sql, and compares the two schemas. If they differ,
# `migrate diff` writes the migration that would close the gap, and that file
# is the finding: it names exactly what one side has and the other lacks.
# Unlike Atlas Community Edition, it diffs policies, so an RLS difference is a
# finding like any other.
#
# It runs on a copy of apps/api, so a local run never leaves a migration file
# or a rewritten atlas.sum in the working tree.
#
# ---------------------------------------------------------------------------
# RUN IT
# ---------------------------------------------------------------------------
#
#   ATLAS_DEV_URL=postgres://USER:PASS@localhost:5432/dev?sslmode=disable \
#   PTAH_DEV_SERVER_DISPOSABLE=1 scripts/check-schema-drift.sh
#
#   scripts/check-schema-drift_test.sh    # the self-test; no database needed
#
# ATLAS_DEV_URL is the same throwaway dev database atlas.hcl's env "local"
# already reads. PTAH_DEV_SERVER_DISPOSABLE=1 declares that whole server
# throwaway, which the replay needs because the migrations create roles in DO
# blocks. Never point it at a server that holds anything you want to keep.
#
# The ptah-compat binary is $WPMGR_PTAH_COMPAT if set, else the first of
# `command -v ptah-compat`, $(go env GOBIN)/ptah-compat and
# $(go env GOPATH)/bin/ptah-compat that exists.
#
#   exit 0  schema.sql and the migrations describe the same schema
#   exit 1  a real finding: they differ, or atlas.sum does not match the
#           migration directory
#   exit 2  the guard could not do its job (no ptah-compat, no dev database,
#           no input, or output it does not recognize). NEVER a pass.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools. No mapfile, no
# associative arrays, no sed -i, no grep -P.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
API_DIR="${WPMGR_SCHEMA_DRIFT_API_DIR:-$HERE/../apps/api}"

broken() {
  echo "schema drift guard: $*" >&2
  echo "schema drift guard: exit 2 means the check did not run. It is never a pass." >&2
  exit 2
}

resolve_ptah_compat() {
  if [ -n "${WPMGR_PTAH_COMPAT:-}" ]; then
    [ -x "$WPMGR_PTAH_COMPAT" ] || return 1
    echo "$WPMGR_PTAH_COMPAT"
    return 0
  fi
  if command -v ptah-compat >/dev/null 2>&1; then
    command -v ptah-compat
    return 0
  fi
  if command -v go >/dev/null 2>&1; then
    for dir in "$(go env GOBIN)" "$(go env GOPATH)/bin"; do
      if [ -n "$dir" ] && [ -x "$dir/ptah-compat" ]; then
        echo "$dir/ptah-compat"
        return 0
      fi
    done
  fi
  return 1
}

PTAH_COMPAT="$(resolve_ptah_compat)" ||
  broken "no ptah-compat binary. Install it: go install ptah.run/cmd/ptah-compat@v0.10.0"

[ -n "${ATLAS_DEV_URL:-}" ] ||
  broken "ATLAS_DEV_URL is not set. It names the throwaway dev database the diff replays on."

[ -f "$API_DIR/atlas.hcl" ] || broken "no atlas.hcl in $API_DIR"
[ -s "$API_DIR/db/schema.sql" ] || broken "no db/schema.sql (or it is empty) in $API_DIR"
MIGRATION_COUNT="$(find "$API_DIR/migrations" -maxdepth 1 -name '*.sql' 2>/dev/null | grep -c .)"
[ "$MIGRATION_COUNT" -gt 0 ] || broken "no migrations in $API_DIR/migrations"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-schema-drift.XXXXXX")" || broken "cannot create a work directory"
trap 'rm -rf "$WORK"' EXIT INT TERM

if ! { mkdir -p "$WORK/db" &&
  cp "$API_DIR/atlas.hcl" "$WORK/atlas.hcl" &&
  cp "$API_DIR/db/schema.sql" "$WORK/db/schema.sql" &&
  cp -R "$API_DIR/migrations" "$WORK/migrations"; }; then
  broken "cannot copy apps/api into $WORK"
fi

list_migrations() {
  (cd "$WORK/migrations" && find . -maxdepth 1 -type f | sed 's#^\./##' | sort)
}

list_migrations > "$WORK/before.txt" || broken "cannot list $WORK/migrations"

echo "schema drift guard: $("$PTAH_COMPAT" version 2>/dev/null | head -1)"
echo "schema drift guard: comparing db/schema.sql with $MIGRATION_COUNT migrations"

(cd "$WORK" && "$PTAH_COMPAT" migrate diff schema_drift --env local) > "$WORK/out.txt" 2>&1
STATUS=$?
cat "$WORK/out.txt"

if [ "$STATUS" -ne 0 ]; then
  if grep -q 'checksum mismatch' "$WORK/out.txt"; then
    echo "" >&2
    echo "DRIFT: apps/api/migrations/atlas.sum does not match the migration files." >&2
    echo "Re-hash the directory and commit atlas.sum:" >&2
    echo "  cd apps/api && ptah-compat migrate hash --dir file://migrations" >&2
    exit 1
  fi
  broken "ptah-compat migrate diff exited $STATUS"
fi

list_migrations > "$WORK/after.txt" || broken "cannot list $WORK/migrations"
NEW_FILES="$(comm -13 "$WORK/before.txt" "$WORK/after.txt" | grep '\.sql$')"

if [ -z "$NEW_FILES" ]; then
  grep -q 'synced' "$WORK/out.txt" ||
    broken "ptah-compat wrote no migration and did not report the directory synced"
  echo "OK: db/schema.sql matches the migration directory."
  exit 0
fi

echo "" >&2
echo "DRIFT: db/schema.sql and apps/api/migrations/ describe different schemas." >&2
echo "The statements below turn the migrations' schema into schema.sql's." >&2
echo "Bring schema.sql up to the migrations, or add the migration schema.sql expects:" >&2
echo "" >&2
echo "$NEW_FILES" | while read -r f; do
  echo "--- $f" >&2
  cat "$WORK/migrations/$f" >&2
done
exit 1
