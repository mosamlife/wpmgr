#!/usr/bin/env bash
# scripts/check-schema-drift_test.sh
#
# The regression suite for scripts/check-schema-drift.sh.
#
# The guard is trusted when it says nothing is wrong, so the cases that matter
# are the silent ones: no binary, no dev database, a ptah-compat that fails,
# and a ptah-compat that exits 0 without saying the directory is synced. Each
# must go red with exit 2, never pass. The other cases prove a real
# difference and a stale atlas.sum exit 1, and a synced directory exits 0.
#
# HOW IT WORKS. Every case runs the real guard against a small copy of
# apps/api and a stub ptah-compat whose behavior the case selects. No database
# and no Go toolchain are needed.
#
# RUN IT:
#   scripts/check-schema-drift_test.sh
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_SCHEMA_DRIFT_GUARD_SCRIPT:-$HERE/check-schema-drift.sh}"
[ -f "$GUARD" ] || { echo "no guard script at $GUARD" >&2; exit 2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-schema-drift-test.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

# A miniature apps/api: the guard copies it, so the stub may write into the copy.
mkdir -p "$WORK/api/db" "$WORK/api/migrations"
echo 'env "local" {}' > "$WORK/api/atlas.hcl"
echo 'CREATE TABLE t (id int);' > "$WORK/api/db/schema.sql"
echo 'CREATE TABLE t (id int);' > "$WORK/api/migrations/20260101000000_init.sql"
echo 'h1:stub' > "$WORK/api/migrations/atlas.sum"

# The stub answers `version` and `migrate diff`, in the directory the guard
# runs it from, the way STUB_MODE says.
cat > "$WORK/ptah-compat" <<'STUB'
#!/usr/bin/env bash
[ "$1" = version ] && { echo "Version: stub"; exit 0; }
case "$STUB_MODE" in
  synced) echo "The migration directory is synced with the desired state, no changes to be made" ;;
  drift)
    echo 'ALTER TABLE "t" ADD COLUMN "x" int;' > migrations/20990101000000_schema_drift.sql
    echo "Created migration file: migrations/20990101000000_schema_drift.sql" ;;
  checksum) echo "You have a checksum error in your migration directory."; echo "Error: checksum mismatch"; exit 1 ;;
  error) echo "Error: connect: connection refused"; exit 1 ;;
  silent) ;;
esac
STUB
chmod +x "$WORK/ptah-compat"

PASSED=0
FAILED=0

# check NAME WANT_EXIT WANT_TEXT [ENV=VALUE ...]
check() {
  name="$1"; want="$2"; text="$3"; shift 3
  out="$(env WPMGR_SCHEMA_DRIFT_API_DIR="$WORK/api" "$@" "$GUARD" 2>&1)"
  got=$?
  if [ "$got" -eq "$want" ] && printf '%s' "$out" | grep -qF -- "$text"; then
    PASSED=$((PASSED + 1))
    echo "ok   $name"
  else
    FAILED=$((FAILED + 1))
    echo "FAIL $name: exit $got (want $want), output lacks or has: $text"
    printf '%s\n' "$out" | sed 's/^/     /'
  fi
}

DEV='ATLAS_DEV_URL=postgres://stub'
BIN="WPMGR_PTAH_COMPAT=$WORK/ptah-compat"

check "synced directory passes"         0 "OK: db/schema.sql matches"          "$DEV" "$BIN" STUB_MODE=synced
check "a written migration is drift"    1 'ADD COLUMN "x" int'                 "$DEV" "$BIN" STUB_MODE=drift
check "stale atlas.sum is a finding"    1 "atlas.sum does not match"           "$DEV" "$BIN" STUB_MODE=checksum
check "a failing diff never passes"     2 "exited 1"                           "$DEV" "$BIN" STUB_MODE=error
check "silence never passes"            2 "did not report the directory synced" "$DEV" "$BIN" STUB_MODE=silent
check "no dev database never passes"    2 "ATLAS_DEV_URL is not set"           ATLAS_DEV_URL= "$BIN" STUB_MODE=synced
check "no binary never passes"          2 "no ptah-compat binary"              "$DEV" WPMGR_PTAH_COMPAT="$WORK/missing" STUB_MODE=synced

# The guard must leave the real input untouched: the drift case wrote into a copy.
if [ "$(find "$WORK/api/migrations" -type f | grep -c .)" -eq 2 ]; then
  PASSED=$((PASSED + 1))
  echo "ok   the input directory is left untouched"
else
  FAILED=$((FAILED + 1))
  echo "FAIL the guard wrote into the input directory:"
  find "$WORK/api/migrations" -type f | sed 's/^/     /'
fi

echo ""
echo "$PASSED passed, $FAILED failed"
[ "$FAILED" -eq 0 ] && [ "$PASSED" -gt 0 ]
