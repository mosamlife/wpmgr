#!/usr/bin/env bash
# scripts/check-schema-sync.sh
#
# apps/api/db/schema.sql must describe the schema the migrations build, and
# apps/api/migrations/atlas.sum must name exactly the migration files on disk.
#
# ---------------------------------------------------------------------------
# WHY THIS EXISTS (GH #759)
# ---------------------------------------------------------------------------
#
# schema.sql is a hand-kept mirror of the migrations. sqlc reads it to type
# every query and `atlas migrate diff` reads it as the desired state, but the
# server never executes it: the migrations are what run, and every real
# database is built from those alone. So a migration that forgets to update
# schema.sql still builds, still generates and still passes every other
# check. It drifted until atlas could no longer read the file at all, and
# nothing said so.
#
# This guard replays every migration into one throwaway database, loads
# schema.sql into a second one, and compares what Postgres itself reports
# about the two. The migrations are the authority; schema.sql is the file
# that is wrong when they disagree.
#
# ---------------------------------------------------------------------------
# WHAT IT COMPARES
# ---------------------------------------------------------------------------
#
# Read from the system catalogs of the two databases, so ordering, quoting,
# schema-qualification, whitespace and comments in the SQL text cannot matter:
#
#   table        existence, kind (ordinary or partitioned), persistence
#   rls          the ENABLE and FORCE ROW LEVEL SECURITY flags of every table
#   column       type, nullability, default, generation, identity, collation
#   index        the full definition, by name
#   constraint   the full definition, by name (keys, checks, foreign keys with
#                their actions, exclusions), and whether it is validated
#   policy       table, name, PERMISSIVE or RESTRICTIVE, command, roles,
#                USING and WITH CHECK
#   function     signature, attributes and body (SECURITY DEFINER, search_path
#                and the rest); see "function bodies" below
#   trigger, sequence, view, type, extension, and any non-public schema
#
# Column order is not compared: a column a migration appended with ALTER TABLE
# is written in place in schema.sql ("write the end state, not the steps").
#
# FUNCTION BODIES are procedural text the catalog stores verbatim, so they are
# compared with SQL comments removed and whitespace collapsed. A comment is
# the commonest harmless difference between a migration and its mirror, and
# reddening on it would get the guard switched off. The cost: text after a
# `--` that sits inside a string literal on the same line is not compared.
#
# ---------------------------------------------------------------------------
# WHAT IT DELIBERATELY DOES NOT COMPARE
# ---------------------------------------------------------------------------
#
#   Privileges (GRANT / REVOKE) on tables and sequences. The migrations grant
#     them through ALTER DEFAULT PRIVILEGES, which schema.sql never replays,
#     so their ACLs differ by design and comparing them would be red on a
#     correct tree. They want their own guard.
#   Row data (the seed INSERTs), object comments (COMMENT ON), ownership.
#   Object classes outside the list above (rules, publications, event
#     triggers, extended statistics, operators, casts, aggregates).
#
# ---------------------------------------------------------------------------
# THE atlas.sum CHECK (needs no database)
# ---------------------------------------------------------------------------
#
# atlas.sum must list exactly the *.sql files in the migrations directory, in
# the lexical order Atlas writes them, and every recorded hash must be the one
# Atlas would compute. The hash is a running SHA-256 over each file name and
# its bytes, so one edited or missing file changes every later entry; the
# top-line sum covers them all. This is Atlas's published format, reproduced
# here so no atlas binary is needed, and it is verified against the real
# binary: scripts/check-schema-sync_test.sh carries sums that
# `atlas migrate hash` wrote. A line that is not h1: is rejected rather than
# guessed at. Regenerate with:
#   atlas migrate hash --dir file://apps/api/migrations
#
# ---------------------------------------------------------------------------
# A GUARD THAT FINDS NOTHING MUST GO RED
# ---------------------------------------------------------------------------
#
#   exit 0  schema.sql matches the migrations and atlas.sum matches the files
#   exit 1  a real finding: drift, a migration that does not apply to an empty
#           database, a schema.sql that does not load, a stale atlas.sum
#   exit 2  the guard could not do its job and says nothing about the tree:
#           no docker, a dead database, a missing or empty input, zero
#           migrations, zero tables, or a catalog that came back without rows
#           for a kind every real schema has. NEVER a pass.
#
# ---------------------------------------------------------------------------
# RUN IT
# ---------------------------------------------------------------------------
#
#   make check-schema-sync-test      scripts/check-schema-sync_test.sh
#   make check-schema-sync           scripts/check-schema-sync.sh
#   scripts/check-schema-sync.sh /path/to/some/other/tree
#
# Needs docker (image postgres:16-alpine, the one the RLS guard uses) and
# openssl; nothing else beyond a shell. The container has no network and no
# published port, keeps its data in tmpfs and is removed on exit, so a run
# leaves no volume behind. A SIGKILLed run can leave the container; remove it
# with:  docker rm -f $(docker ps -aq --filter label=wpmgr.schema-sync=1)
#
# ci.yml runs the self-test, then this, as two steps of one job, on every PR
# and push. Read a run's own "elapsed" line for what it costs; the figure is
# printed, not quoted here, because a quoted one goes stale.
#
# ENVIRONMENT
#   WPMGR_SCHEMA_SYNC_CONTAINER  reuse this running postgres container instead
#                                of starting one (the self-test does, so one
#                                container serves every case). Its databases
#                                are created and dropped per run.
#   WPMGR_SCHEMA_SYNC_IMAGE      override the image (default postgres:16-alpine)
#   WPMGR_SCHEMA_SYNC_DUMP_DIR   write the two normalised catalogs and the full
#                                difference list here, for reading what a long
#                                report truncated
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop and on an ubuntu runner. No mapfile, no associative
# arrays, no sed -i, no grep -P, no `length(array)` in awk.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<'EOF'
Usage: scripts/check-schema-sync.sh [ROOT]

Checks that apps/api/db/schema.sql matches what apps/api/migrations builds and
that apps/api/migrations/atlas.sum names exactly the migration files on disk.
ROOT defaults to the repository root.

Exit 0 in step, 1 drift found, 2 the guard could not run (never a pass).
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
esac
if [ "$#" -gt 1 ]; then usage >&2; exit 2; fi

ROOT="${1:-$(cd "$HERE/.." && pwd)}"
MIG_DIR="$ROOT/apps/api/migrations"
SCHEMA_FILE="$ROOT/apps/api/db/schema.sql"
SUM_FILE="$MIG_DIR/atlas.sum"
IMAGE="${WPMGR_SCHEMA_SYNC_IMAGE:-postgres:16-alpine}"
DUMP_DIR="${WPMGR_SCHEMA_SYNC_DUMP_DIR:-}"

TAB="$(printf '\t')"
FAILED_CHECKS=0
OWN_CONTAINER=""
CONTAINER="${WPMGR_SCHEMA_SYNC_CONTAINER:-}"
DB_MIG=""
DB_DECL=""
TMP=""

# ---------------------------------------------------------------------------
# Output and exit paths
# ---------------------------------------------------------------------------

# broken() is the exit-2 path: the guard could not do its job. It is kept
# apart from a finding so that "I found nothing because I am broken" can never
# be mistaken for "I found nothing because nothing is wrong".
broken() {
  printf 'GUARD BROKEN: %s\n' "$1" >&2
  printf 'GUARD BROKEN: refusing to report a clean result from a run that could not check anything.\n' >&2
  exit 2
}

finding() { FAILED_CHECKS=$((FAILED_CHECKS + 1)); }

# shellcheck disable=SC2329  # runs through the EXIT trap below
cleanup() {
  if [ -z "$OWN_CONTAINER" ] && [ -n "$CONTAINER" ] && [ -n "$DB_MIG" ]; then
    # A container the caller owns outlives this run, so drop what it made.
    docker exec "$CONTAINER" psql -U postgres -X -q -d postgres \
      -c "DROP DATABASE IF EXISTS \"$DB_MIG\" WITH (FORCE)" \
      -c "DROP DATABASE IF EXISTS \"$DB_DECL\" WITH (FORCE)" >/dev/null 2>&1
  fi
  if [ -n "$OWN_CONTAINER" ]; then
    docker rm -f -v "$OWN_CONTAINER" >/dev/null 2>&1
  fi
  if [ -n "$TMP" ]; then rm -rf "$TMP"; fi
  return 0
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---------------------------------------------------------------------------
# Inputs. Every missing or empty one is exit 2, before anything is started.
# ---------------------------------------------------------------------------

[ -d "$ROOT" ] || broken "$ROOT is not a directory."
TMP="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-schema-sync.XXXXXX")" || broken "could not create a temporary directory."

[ -d "$MIG_DIR" ] || broken "no migrations directory at $MIG_DIR."

MIG_LIST="$TMP/migrations.list"
( cd "$MIG_DIR" && find . -maxdepth 1 -type f -name '*.sql' ) > "$MIG_LIST.raw" \
  || broken "could not list $MIG_DIR."
sed 's|^\./||' "$MIG_LIST.raw" | LC_ALL=C sort > "$MIG_LIST" \
  || broken "could not sort the migration list."
N_MIGS="$(awk 'END { print NR + 0 }' "$MIG_LIST")"
[ "$N_MIGS" -gt 0 ] || broken "no *.sql files in $MIG_DIR. Zero migrations is not 'in sync', it is nothing to check."

# One empty file among valid ones counts too. A zero-byte migration applies
# nothing and still hashes into a regenerated atlas.sum, so without this a tree
# could pass with a migration that does not exist in any real sense. Refused
# here, before docker is touched. (A migration holding only comments is not
# empty and stays valid.)
while IFS= read -r name; do
  [ -s "$MIG_DIR/$name" ] || broken "migration $name is empty (zero bytes). It applies nothing, so it cannot stand for a schema change; refusing to count it."
done < "$MIG_LIST"

[ -f "$SCHEMA_FILE" ] || broken "no schema file at $SCHEMA_FILE."
[ -s "$SCHEMA_FILE" ] || broken "$SCHEMA_FILE is empty."
[ -f "$SUM_FILE" ] || broken "no atlas.sum at $SUM_FILE."
[ -s "$SUM_FILE" ] || broken "$SUM_FILE is empty."

if grep -l 'atlas:sum' "$MIG_DIR"/*.sql >/dev/null 2>&1; then
  broken "a migration carries an atlas:sum directive, which this guard does not model. Extend the guard before relying on it."
fi

command -v docker >/dev/null 2>&1 || broken "docker is not installed; the replay needs a throwaway postgres."
docker info >/dev/null 2>&1 || broken "the docker daemon is not reachable; the replay needs a throwaway postgres."
command -v openssl >/dev/null 2>&1 || broken "openssl is not installed; the atlas.sum hashes cannot be verified and must not be skipped."
command -v base64 >/dev/null 2>&1 || broken "base64 is not installed; the atlas.sum hashes cannot be verified and must not be skipped."

# ---------------------------------------------------------------------------
# The throwaway database. Started first so that postgres initialises while the
# atlas.sum check, which needs no database, runs.
#
# No network and no published port: nothing can reach it and it can collide
# with nothing. Its data directory is tmpfs, so no volume is created and the
# replay does not wait on a disk. Everything talks to it through docker exec.
# ---------------------------------------------------------------------------

dpsql() { docker exec -i "$CONTAINER" psql -U postgres -X -v ON_ERROR_STOP=1 "$@"; }

if [ -z "$CONTAINER" ]; then
  OWN_CONTAINER="wpmgr-schema-sync-$$-$RANDOM"
  if ! run_err="$(docker run -d --rm --name "$OWN_CONTAINER" --label wpmgr.schema-sync=1 \
    --network none --tmpfs /var/lib/postgresql/data:rw \
    -e POSTGRES_PASSWORD=throwaway "$IMAGE" 2>&1 >/dev/null)"; then
    OWN_CONTAINER=""
    broken "could not start the throwaway postgres ($IMAGE): $run_err"
  fi
  CONTAINER="$OWN_CONTAINER"
elif [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" != "true" ]; then
  # A container the caller named has to exist and be running now; waiting on
  # one that never will be would only turn a clear error into a long one.
  CONTAINER=""
  broken "WPMGR_SCHEMA_SYNC_CONTAINER names a container that is not running."
fi

# ---------------------------------------------------------------------------
# Check 1: atlas.sum names exactly the files on disk, and its hashes are right.
# ---------------------------------------------------------------------------

# compute_chain -- one "name<TAB>hash" line per migration, in order, then a
# "HEADER<TAB>sum" line. Atlas hashes name then bytes into ONE running SHA-256
# and records the digest after each file; the header hashes every name and
# recorded digest together.
compute_chain() {
  local acc="$TMP/chain.acc" hdr="$TMP/chain.hdr" name h
  : > "$acc"
  : > "$hdr"
  while IFS= read -r name; do
    printf '%s' "$name" >> "$acc" || return 1
    cat "$MIG_DIR/$name" >> "$acc" || return 1
    h="$(openssl dgst -sha256 -binary "$acc" | base64)" || return 1
    [ -n "$h" ] || return 1
    printf '%s\t%s\n' "$name" "$h"
    printf '%s%s' "$name" "$h" >> "$hdr" || return 1
  done < "$MIG_LIST"
  h="$(openssl dgst -sha256 -binary "$hdr" | base64)" || return 1
  [ -n "$h" ] || return 1
  printf 'HEADER\t%s\n' "$h"
}

check_atlas_sum() {
  local first bad names_ok=1 n_sum
  first="$(sed -n '1p' "$SUM_FILE")"
  case "$first" in
    h1:*) ;;
    *)
      printf 'FAIL: atlas.sum: the first line is not an h1: checksum header (got: %s).\n' "$first"
      printf '      This guard only understands Atlas h1 sums; refusing to guess at another format.\n'
      finding
      return 0
      ;;
  esac

  bad="$(awk 'NR > 1 && $0 !~ "^[^ ]+[.]sql h1:[A-Za-z0-9+/]+=*$" { n++ } END { print n + 0 }' "$SUM_FILE")"
  if [ "$bad" -gt 0 ]; then
    printf 'FAIL: atlas.sum has %s line(s) that are not "<file>.sql h1:<hash>":\n' "$bad"
    awk 'NR > 1 && $0 !~ "^[^ ]+[.]sql h1:[A-Za-z0-9+/]+=*$" { print "        line " NR ": " $0 }' "$SUM_FILE" | head -10
    finding
    return 0
  fi

  awk 'NR > 1 { print $1 }' "$SUM_FILE" > "$TMP/sum.names"
  n_sum="$(awk 'END { print NR + 0 }' "$TMP/sum.names")"
  LC_ALL=C sort "$TMP/sum.names" > "$TMP/sum.names.sorted"
  LC_ALL=C sort -u "$TMP/sum.names" > "$TMP/sum.names.uniq"
  LC_ALL=C comm -23 "$MIG_LIST" "$TMP/sum.names.uniq" > "$TMP/sum.unlisted"
  LC_ALL=C comm -13 "$MIG_LIST" "$TMP/sum.names.uniq" > "$TMP/sum.stale"
  LC_ALL=C uniq -d "$TMP/sum.names.sorted" > "$TMP/sum.dupes"

  if [ -s "$TMP/sum.unlisted" ]; then
    names_ok=0
    printf 'FAIL: atlas.sum does not list %s migration file(s) that are on disk:\n' "$(awk 'END { print NR + 0 }' "$TMP/sum.unlisted")"
    sed 's/^/        /' "$TMP/sum.unlisted" | head -20
  fi
  if [ -s "$TMP/sum.stale" ]; then
    names_ok=0
    printf 'FAIL: atlas.sum lists %s file(s) that are not on disk:\n' "$(awk 'END { print NR + 0 }' "$TMP/sum.stale")"
    sed 's/^/        /' "$TMP/sum.stale" | head -20
  fi
  if [ -s "$TMP/sum.dupes" ]; then
    names_ok=0
    printf 'FAIL: atlas.sum lists these files more than once:\n'
    sed 's/^/        /' "$TMP/sum.dupes" | head -20
  fi

  if [ "$names_ok" = "1" ] && ! cmp -s "$MIG_LIST" "$TMP/sum.names"; then
    names_ok=0
    printf 'FAIL: atlas.sum lists the right files in the wrong order (Atlas writes them lexically by file name).\n'
    printf '      first position that differs:\n'
    awk -v a="$MIG_LIST" 'FILENAME == a { want[FNR] = $0; next } want[FNR] != $0 { print "        entry " FNR ": atlas.sum has " $0 ", expected " want[FNR]; exit }' \
      "$MIG_LIST" "$TMP/sum.names"
  fi

  if [ "$names_ok" = "1" ]; then
    if ! compute_chain > "$TMP/chain.out"; then
      broken "could not compute the atlas.sum hash chain (openssl or base64 failed)."
    fi
    awk 'NR > 1 { h = $2; sub(/^h1:/, "", h); print $1 "\t" h }' "$SUM_FILE" > "$TMP/chain.recorded"
    printf 'HEADER\t%s\n' "${first#h1:}" >> "$TMP/chain.recorded"
    awk -F'\t' -v rec="$TMP/chain.recorded" '
      FILENAME == rec { R[$1] = $2; next }
      { if (!($1 in R) || R[$1] != $2) { bad++; if (first == "") first = $1 } }
      END { if (bad > 0) { print bad "\t" first } }
    ' "$TMP/chain.recorded" "$TMP/chain.out" > "$TMP/chain.bad"
    if [ -s "$TMP/chain.bad" ]; then
      names_ok=0
      printf 'FAIL: atlas.sum hashes do not match the files on disk: %s entr%s differ, the first being %s.\n' \
        "$(cut -f1 "$TMP/chain.bad")" \
        "$(if [ "$(cut -f1 "$TMP/chain.bad")" = "1" ]; then printf 'y'; else printf 'ies'; fi)" \
        "$(cut -f2 "$TMP/chain.bad")"
      printf '      The hash is a running one, so a file edited after atlas.sum was written changes every entry after it.\n'
    fi
  fi

  if [ "$names_ok" = "1" ]; then
    printf 'OK: atlas.sum lists exactly the %s migration files on disk, in order, with matching hashes.\n' "$n_sum"
  else
    printf '      Regenerate it with: atlas migrate hash --dir file://apps/api/migrations\n'
    finding
  fi
  return 0
}

check_atlas_sum

# ---------------------------------------------------------------------------
# Check 2: replay the migrations, load schema.sql, compare the catalogs.
# ---------------------------------------------------------------------------

# Wait for a REAL server. pg_isready over TCP only answers once the final
# server is up: the image first runs a bootstrap server on the unix socket
# alone and stops it again, and a query that lands on that one dies mid-run.
# Bounded: 300 polls of 0.2s, then it is broken, never an endless wait.
ready=0
i=0
while [ "$i" -lt 300 ]; do
  if docker exec "$CONTAINER" pg_isready -h 127.0.0.1 -U postgres -q >/dev/null 2>&1 \
     && docker exec "$CONTAINER" psql -U postgres -X -At -d postgres -c 'SELECT 1' >/dev/null 2>&1; then
    ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.2
done
if [ "$ready" != "1" ]; then
  logs="$(docker logs --tail 15 "$CONTAINER" 2>&1)"
  broken "the postgres in container '$CONTAINER' never became ready. Last log lines:
$logs"
fi

DB_MIG="ss_mig_$$_$RANDOM"
DB_DECL="ss_decl_$$_$RANDOM"

# The role is created up front in the cluster, as the migrations create it
# (idempotently), so both databases start from the same state. schema.sql
# documents that it loads with the role present.
cat > "$TMP/setup.sql" <<'SQL'
SELECT 'CREATE ROLE wpmgr_app NOLOGIN NOSUPERUSER NOBYPASSRLS'
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wpmgr_app') \gexec
CREATE DATABASE :"mig" TEMPLATE template0;
CREATE DATABASE :"decl" TEMPLATE template0;
SQL
if ! setup_err="$(dpsql -q -d postgres -v "mig=$DB_MIG" -v "decl=$DB_DECL" -f - < "$TMP/setup.sql" 2>&1 >/dev/null)"; then
  DB_MIG=""
  broken "could not create the two databases: $setup_err"
fi

# One psql session for the whole replay, each file in its own transaction as
# the server's runner does it, with DISCARD ALL between files so no session
# state leaks from one migration into the next. A \warn marker before each
# file names it in the error log, so a failure is attributed to a file.
: > "$TMP/replay.sql"
while IFS= read -r name; do
  {
    printf '\\warn MIGRATION %s\nBEGIN;\n' "$name"
    cat "$MIG_DIR/$name"
    printf '\n;\nCOMMIT;\nDISCARD ALL;\n'
  } >> "$TMP/replay.sql" || broken "could not assemble the replay stream."
done < "$MIG_LIST"

REPLAY_OK=1
dpsql -q -d "$DB_MIG" -f - < "$TMP/replay.sql" > "$TMP/replay.out" 2> "$TMP/replay.err"
rc=$?
if [ "$rc" -ne 0 ]; then
  if [ "$rc" -ne 3 ]; then
    broken "psql could not replay the migrations (exit $rc): $(head -5 "$TMP/replay.err")"
  fi
  REPLAY_OK=0
  printf 'FAIL: the migrations do not apply to an empty database.\n'
  awk '
    /^MIGRATION / { cur = $2; next }
    /ERROR:/ && !seen { seen = 1; printf "      migration: %s\n", cur; print "      " $0; left = 6; next }
    seen && left > 0 { print "      " $0; left-- }
  ' "$TMP/replay.err"
  finding
fi

DECL_OK=1
dpsql -q -d "$DB_DECL" -f - < "$SCHEMA_FILE" > "$TMP/load.out" 2> "$TMP/load.err"
rc=$?
if [ "$rc" -ne 0 ]; then
  if [ "$rc" -ne 3 ]; then
    broken "psql could not load schema.sql (exit $rc): $(head -5 "$TMP/load.err")"
  fi
  DECL_OK=0
  printf 'FAIL: apps/api/db/schema.sql does not load into an empty database in one pass.\n'
  grep -m 1 -B1 -A6 'ERROR:' "$TMP/load.err" | sed 's/^/      /'
  finding
fi

# The catalog: one "kind<TAB>key<TAB>definition" row per object, every field
# free of tabs and newlines, NULL rendered as empty, so a row is a line.
cat > "$TMP/catalog.sql" <<'SQL'
WITH
sch AS (
  SELECT n.oid, n.nspname
    FROM pg_namespace n
   WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
     AND n.nspname NOT LIKE 'pg\_toast%'
     AND n.nspname NOT LIKE 'pg\_temp%'
),
tbl AS (
  SELECT c.oid, c.relname, c.relkind, c.relpersistence,
         c.relrowsecurity, c.relforcerowsecurity,
         format('%I.%I', s.nspname, c.relname) AS qname
    FROM pg_class c
    JOIN sch s ON s.oid = c.relnamespace
   WHERE c.relkind IN ('r', 'p')
     AND c.relname <> 'schema_migrations'
),
rec AS (
  SELECT 'schema'::text AS kind, s.nspname::text AS key, ''::text AS def
    FROM sch s
   WHERE s.nspname <> 'public'
  UNION ALL
  SELECT 'table', qname,
         'relkind=' || relkind::text || ' persistence=' || relpersistence::text
    FROM tbl
  UNION ALL
  SELECT 'rls', qname,
         'enabled=' || relrowsecurity::text || ' forced=' || relforcerowsecurity::text
    FROM tbl
  UNION ALL
  SELECT 'column', t.qname || '.' || quote_ident(a.attname),
         format_type(a.atttypid, a.atttypmod)
         || CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE ' NULL' END
         || CASE WHEN a.attgenerated::text = '' AND d.adbin IS NOT NULL
                 THEN ' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid) ELSE '' END
         || CASE WHEN a.attgenerated::text <> '' AND d.adbin IS NOT NULL
                 THEN ' GENERATED (' || pg_get_expr(d.adbin, d.adrelid) || ')' ELSE '' END
         || CASE a.attidentity::text WHEN 'a' THEN ' IDENTITY ALWAYS'
                                     WHEN 'd' THEN ' IDENTITY BY DEFAULT' ELSE '' END
         || CASE WHEN a.attcollation <> ty.typcollation AND a.attcollation <> 0
                 THEN ' COLLATE ' || (SELECT collname FROM pg_collation WHERE oid = a.attcollation)
                 ELSE '' END
    FROM tbl t
    JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum > 0 AND NOT a.attisdropped
    JOIN pg_type ty ON ty.oid = a.atttypid
    LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
  UNION ALL
  SELECT 'index', format('%I.%I', s.nspname, ic.relname),
         pg_get_indexdef(i.indexrelid) || CASE WHEN i.indisvalid THEN '' ELSE ' (INVALID)' END
    FROM tbl t
    JOIN pg_index i ON i.indrelid = t.oid
    JOIN pg_class ic ON ic.oid = i.indexrelid
    JOIN sch s ON s.oid = ic.relnamespace
  UNION ALL
  SELECT 'constraint', t.qname || '.' || quote_ident(con.conname),
         pg_get_constraintdef(con.oid) || CASE WHEN con.convalidated THEN '' ELSE ' (NOT VALIDATED)' END
    FROM tbl t
    JOIN pg_constraint con ON con.conrelid = t.oid AND con.conparentid = 0
  UNION ALL
  SELECT 'policy', format('%I.%I.%I', p.schemaname, p.tablename, p.policyname),
         p.permissive || ' ' || p.cmd
         || ' TO ' || array_to_string(ARRAY(SELECT r FROM unnest(p.roles) AS r ORDER BY r), ',')
         || ' USING (' || coalesce(p.qual, '-') || ')'
         || ' WITH CHECK (' || coalesce(p.with_check, '-') || ')'
    FROM pg_policies p
    JOIN sch s ON s.nspname = p.schemaname
  UNION ALL
  SELECT 'function',
         format('%I.%I(%s)', s.nspname, pr.proname, pg_get_function_identity_arguments(pr.oid)),
         regexp_replace(
           regexp_replace(
             regexp_replace(pg_get_functiondef(pr.oid), '/\*.*?\*/', ' ', 'g'),
             '--[^\n]*', ' ', 'g'),
           '\s+', ' ', 'g')
    FROM pg_proc pr
    JOIN sch s ON s.oid = pr.pronamespace
   WHERE pr.prokind IN ('f', 'p')
     AND NOT EXISTS (SELECT 1 FROM pg_depend d
                      WHERE d.classid = 'pg_proc'::regclass AND d.objid = pr.oid AND d.deptype = 'e')
  UNION ALL
  SELECT 'trigger', t.qname || '.' || quote_ident(tg.tgname),
         pg_get_triggerdef(tg.oid)
    FROM tbl t
    JOIN pg_trigger tg ON tg.tgrelid = t.oid AND NOT tg.tgisinternal
  UNION ALL
  SELECT 'view', format('%I.%I', s.nspname, c.relname),
         'relkind=' || c.relkind::text || ' ' || pg_get_viewdef(c.oid)
    FROM pg_class c
    JOIN sch s ON s.oid = c.relnamespace
   WHERE c.relkind IN ('v', 'm')
  UNION ALL
  SELECT 'sequence', format('%I.%I', s.nspname, c.relname),
         format('type=%s start=%s increment=%s min=%s max=%s cycle=%s',
                format_type(sq.seqtypid, NULL), sq.seqstart, sq.seqincrement,
                sq.seqmin, sq.seqmax, sq.seqcycle)
    FROM pg_class c
    JOIN sch s ON s.oid = c.relnamespace
    JOIN pg_sequence sq ON sq.seqrelid = c.oid
  UNION ALL
  SELECT 'type', format('%I.%I', s.nspname, ty.typname),
         'typtype=' || ty.typtype::text
         || CASE WHEN ty.typtype = 'e'
                 THEN ' labels=' || (SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
                                       FROM pg_enum e WHERE e.enumtypid = ty.oid)
                 ELSE '' END
    FROM pg_type ty
    JOIN sch s ON s.oid = ty.typnamespace
   WHERE ty.typtype IN ('e', 'd', 'c')
     AND NOT EXISTS (SELECT 1 FROM pg_class rc WHERE rc.reltype = ty.oid)
     AND NOT EXISTS (SELECT 1 FROM pg_depend d
                      WHERE d.classid = 'pg_type'::regclass AND d.objid = ty.oid AND d.deptype = 'e')
  UNION ALL
  SELECT 'extension', e.extname::text, ''
    FROM pg_extension e
   WHERE e.extname <> 'plpgsql'
)
SELECT format(E'%s\t%s\t%s', kind,
              translate(key, E'\t\r\n', '   '),
              translate(def, E'\t\r\n', '   '))
  FROM rec
SQL

KINDS="schema table rls column index constraint policy function trigger sequence view type extension"
# Every real schema has these. A catalog without a row of one of them came
# from a query that matched nothing, not from a schema that has none.
REQUIRED_KINDS="table rls column index constraint policy"

# extract_catalog DB OUT WHAT -- sorted rows, or exit 2. WHAT names the input
# the database was built from, for the message when it built nothing.
extract_catalog() {
  local db="$1" out="$2" what="$3" rc
  dpsql -At -d "$db" -f - < "$TMP/catalog.sql" > "$out.raw" 2> "$out.err"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    broken "could not read the catalog of $db (psql exit $rc): $(head -5 "$out.err")"
  fi
  LC_ALL=C sort "$out.raw" > "$out" || broken "could not sort the catalog of $db."
  [ -s "$out" ] || broken "$what created nothing: the database it was loaded into has no catalog rows. Refusing to compare against nothing."
  awk -F'\t' -v kinds="$KINDS" '
    BEGIN { n = split(kinds, k, " "); for (i = 1; i <= n; i++) known[k[i]] = 1 }
    NF != 3 || !($1 in known) { bad++; if (bad <= 3) print "      " NR ": " substr($0, 1, 120) }
    END { exit (bad > 0) }
  ' "$out" > "$out.bad" || broken "the catalog of $db has malformed rows:
$(cat "$out.bad")"
}

count_kind() { awk -F'\t' -v k="$2" '$1 == k { n++ } END { print n + 0 }' "$1"; }

if [ "$REPLAY_OK" = "1" ] && [ "$DECL_OK" = "1" ]; then
  extract_catalog "$DB_MIG" "$TMP/mig.catalog" "Replaying the migrations"
  extract_catalog "$DB_DECL" "$TMP/decl.catalog" "apps/api/db/schema.sql"

  for k in $REQUIRED_KINDS; do
    [ "$(count_kind "$TMP/mig.catalog" "$k")" -gt 0 ] \
      || broken "replaying the migrations produced no '$k' rows. Every real schema has them, so this is a broken extraction or an empty replay, not a clean schema."
  done
  [ "$(count_kind "$TMP/decl.catalog" table)" -gt 0 ] \
    || broken "apps/api/db/schema.sql loaded but created no tables. Refusing to compare the migrations against nothing."

  # Compare, keyed on kind + key. Rows come out tagged: 1 only in the
  # migrations, 2 only in schema.sql, 3 in both with different definitions.
  cat > "$TMP/compare.awk" <<'AWK'
BEGIN {
  FS = "\t"
  n = split(kinds, order, " ")
  for (i = 1; i <= n; i++) rank[order[i]] = i
}
FILENAME == fa {
  key = $1 SUBSEP $2
  A[key] = $3
  K[key] = 1
  next
}
{
  key = $1 SUBSEP $2
  B[key] = $3
  K[key] = 1
}
END {
  for (key in K) {
    split(key, p, SUBSEP)
    r = (p[1] in rank) ? rank[p[1]] : 99
    if ((key in A) && !(key in B))      printf "1\t%02d\t%s\t%s\t%s\n", r, p[1], p[2], A[key]
    else if ((key in B) && !(key in A)) printf "2\t%02d\t%s\t%s\t%s\n", r, p[1], p[2], B[key]
    else if (A[key] != B[key])          printf "3\t%02d\t%s\t%s\t%s\t%s\n", r, p[1], p[2], A[key], B[key]
  }
}
AWK
  awk -v fa="$TMP/mig.catalog" -v kinds="$KINDS" -f "$TMP/compare.awk" \
    "$TMP/mig.catalog" "$TMP/decl.catalog" \
    | LC_ALL=C sort -t "$TAB" -k1,1n -k2,2n -k4,4 > "$TMP/diffs.tsv" \
    || broken "the catalog comparison failed."

  if [ -n "$DUMP_DIR" ]; then
    if ! { mkdir -p "$DUMP_DIR" \
        && cp "$TMP/mig.catalog" "$DUMP_DIR/migrations.catalog" \
        && cp "$TMP/decl.catalog" "$DUMP_DIR/schema.catalog" \
        && cp "$TMP/diffs.tsv" "$DUMP_DIR/differences.tsv"; }; then
      broken "could not write the dumps to $DUMP_DIR."
    fi
  fi

  N_DIFFS="$(awk 'END { print NR + 0 }' "$TMP/diffs.tsv")"
  if [ "$N_DIFFS" -gt 0 ]; then
    n1="$(awk -F'\t' '$1 == 1 { n++ } END { print n + 0 }' "$TMP/diffs.tsv")"
    n2="$(awk -F'\t' '$1 == 2 { n++ } END { print n + 0 }' "$TMP/diffs.tsv")"
    n3="$(awk -F'\t' '$1 == 3 { n++ } END { print n + 0 }' "$TMP/diffs.tsv")"
    by_kind="$(cut -f3 "$TMP/diffs.tsv" | LC_ALL=C sort | uniq -c | awk '{ printf "%s%s %s", sep, $1, $2; sep = ", " }')"
    printf 'FAIL: apps/api/db/schema.sql has drifted from the migrations: %s difference(s) (%s).\n' "$N_DIFFS" "$by_kind"
    cat > "$TMP/format.awk" <<'AWK'
BEGIN { FS = "\t"; cap = 100 }
function firstdiff(a, b,    i, m) {
  m = length(a)
  if (length(b) < m) m = length(b)
  for (i = 1; i <= m; i++) if (substr(a, i, 1) != substr(b, i, 1)) return i
  return m + 1
}
function clip(s, pos,    st, out) {
  st = pos - 45
  if (st < 1) st = 1
  out = substr(s, st, 110)
  if (st > 1) out = "..." out
  if (st + 110 <= length(s)) out = out "..."
  return out
}
function trunc(s) {
  if (length(s) > 200) return substr(s, 1, 200) "..."
  return s
}
function flush() {
  if (hidden > 0) printf "    ... and %d more in this group not shown (set WPMGR_SCHEMA_SYNC_DUMP_DIR to write them all)\n", hidden
  hidden = 0
}
{
  if ($1 != cur) {
    flush()
    cur = $1
    shown = 0
    if (cur == 1) printf "\n  In the migrations but MISSING from db/schema.sql (%d):\n", n1
    if (cur == 2) printf "\n  In db/schema.sql but NOT produced by the migrations (%d):\n", n2
    if (cur == 3) printf "\n  In both but DIFFERENT (%d):\n", n3
  }
  if (shown >= cap) { hidden++; next }
  shown++
  printf "    %-10s %s\n", $3, $4
  if ($1 == 3) {
    if (length($5) <= 160 && length($6) <= 160) {
      printf "        migrations: %s\n        schema.sql: %s\n", $5, $6
    } else {
      p = firstdiff($5, $6)
      printf "        migrations: %s\n        schema.sql: %s\n", clip($5, p), clip($6, p)
    }
  } else {
    printf "        %s\n", trunc($5)
  }
}
END { flush() }
AWK
    awk -v n1="$n1" -v n2="$n2" -v n3="$n3" -f "$TMP/format.awk" "$TMP/diffs.tsv"
    printf '\n  The migrations are what every real database is built from, so db/schema.sql is the file to\n'
    printf '  correct: write the end state the migrations produce (see the header of apps/api/db/schema.sql).\n'
    finding
  else
    summary=""
    for k in $KINDS; do
      c="$(count_kind "$TMP/mig.catalog" "$k")"
      if [ "$c" -gt 0 ]; then summary="$summary$c $k, "; fi
    done
    printf 'OK: apps/api/db/schema.sql matches what the %s migrations build.\n' "$N_MIGS"
    printf '    compared: %s\n' "${summary%, }"
  fi
fi

elapsed="$SECONDS"
if [ "$FAILED_CHECKS" -gt 0 ]; then
  printf '\nFAIL: %s check(s) failed. elapsed: %ss\n' "$FAILED_CHECKS" "$elapsed"
  exit 1
fi
printf '\nOK: schema.sql and atlas.sum are in step with the migrations. elapsed: %ss\n' "$elapsed"
exit 0
