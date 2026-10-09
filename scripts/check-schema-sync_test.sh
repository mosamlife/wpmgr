#!/usr/bin/env bash
# scripts/check-schema-sync_test.sh
#
# The regression suite for scripts/check-schema-sync.sh.
#
# WHY THIS EXISTS. A guard nobody has seen fail is not known to guard anything,
# and one that reddens correct work gets switched off. Every drift the guard
# exists to catch is planted here and must turn it red for the right reason,
# and every honest difference it must NOT block is constructed and must leave
# it green. The self-test runs before the real check in ci.yml, so a broken
# guard fails the build instead of passing by failing open.
#
# HOW IT WORKS. One small tree (three migrations, a schema.sql that mirrors
# them, an atlas.sum) is built once. Each case copies it, mutates exactly one
# thing, runs the real guard against the copy and asserts the exit code plus
# what the output does and does not say. No mocking: the real script replays
# real SQL in a real postgres. One postgres container serves every case (the
# guard's WPMGR_SCHEMA_SYNC_CONTAINER), which is what keeps the suite to a few
# seconds; the guard creates and drops its own two databases per case.
#
# THE atlas.sum VALUES ARE GROUND TRUTH, NOT THIS SUITE'S OPINION. The three
# sums below were written by the real `atlas migrate hash` over the files this
# script writes. The guard reproduces Atlas's hash chain by hand so CI needs no
# atlas binary, and a green base case is what proves the reproduction matches.
# If you change a fixture migration, regenerate its sum:
#   scripts/check-schema-sync_test.sh --emit-fixture DIR trio|label|scratch
#   atlas migrate hash --dir file://DIR      # then paste DIR/atlas.sum below
#
# EXIT CODES the cases assert: pass = 0, fail = 1 (a finding), broken = 2 (the
# guard could not do its job). The distinction is the point: "found nothing
# because broken" must never look like "found nothing because clean".
#
# RUN IT:
#   scripts/check-schema-sync_test.sh                every case
#   scripts/check-schema-sync_test.sh column         only cases matching "column"
#   scripts/check-schema-sync_test.sh --real         four plants against a scratch
#                                                    copy of this repository's own
#                                                    tree, then the tree untouched
#   WPMGR_SCHEMA_SYNC_TEST_VERBOSE=1 ...             also print what the guard said
#                                                    for every case that passed
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_SCHEMA_SYNC_SCRIPT=/tmp/guard-with-hole.sh scripts/check-schema-sync_test.sh
#
# PORTABILITY. bash 3.2 and POSIX tools. No mapfile, no associative arrays, no
# sed -i, no grep -P, no empty-array expansion.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_SCHEMA_SYNC_SCRIPT:-$HERE/check-schema-sync.sh}"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
IMAGE="${WPMGR_SCHEMA_SYNC_IMAGE:-postgres:16-alpine}"

MIG_REL="apps/api/migrations"
SCHEMA_REL="apps/api/db/schema.sql"

# ---------------------------------------------------------------------------
# The fixture. Three migrations, written so that between them they exercise
# every kind the guard compares: an extension, an enum type, tables with a
# primary key, a unique index with a predicate, a foreign key, a check, row
# level security with one permissive and one restrictive policy, a security
# definer function, a trigger, a column added by ALTER, a view, a sequence.
# They also carry things schema.sql deliberately does not mirror (seed data,
# GRANT and REVOKE), which the guard must not mind.
# ---------------------------------------------------------------------------
write_mig_1() {
  cat <<'EOF'
-- Fixture migration 1: the role, an extension, a type, three tables, and row
-- level security on one of them.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wpmgr_app') THEN
    CREATE ROLE wpmgr_app NOLOGIN NOSUPERUSER NOBYPASSRLS;
  END IF;
END
$$;

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TYPE "public"."note_state" AS ENUM ('draft', 'live');

CREATE TABLE "public"."tenants" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "name" text NOT NULL,
  "slug" citext NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "tenants_slug_key" UNIQUE ("slug")
);

CREATE TABLE "public"."audit_log" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "at" timestamptz NOT NULL DEFAULT now(),
  "what" text NOT NULL,
  PRIMARY KEY ("id")
);

CREATE TABLE "public"."notes" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "title" text NOT NULL,
  "body" text NULL,
  "state" "public"."note_state" NOT NULL DEFAULT 'draft',
  "rank" integer NOT NULL DEFAULT 0,
  PRIMARY KEY ("id"),
  CONSTRAINT "notes_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "notes_rank_check" CHECK (rank >= 0)
);
CREATE INDEX "notes_tenant_id_idx" ON "public"."notes" ("tenant_id");
CREATE UNIQUE INDEX "notes_tenant_title_key" ON "public"."notes" ("tenant_id", "title") WHERE (state = 'live');

ALTER TABLE "public"."notes" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."notes" FORCE ROW LEVEL SECURITY;
CREATE POLICY "notes_tenant_isolation" ON "public"."notes"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY "notes_live_only" ON "public"."notes" AS RESTRICTIVE FOR SELECT
  USING (state = 'live' OR current_setting('app.role', true) = 'editor');

-- Seed data is not part of the schema comparison.
INSERT INTO "public"."tenants" ("name", "slug") VALUES ('seed', 'seed');
EOF
}

write_mig_2() {
  cat <<'EOF'
-- Fixture migration 2: a column added after the fact, and a security definer
-- function with a trigger.
ALTER TABLE "public"."notes" ADD COLUMN "updated_at" timestamptz NOT NULL DEFAULT now();

CREATE OR REPLACE FUNCTION public.touch_updated_at() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $fn$
BEGIN
  -- stamp the row
  NEW.updated_at := now();
  RETURN NEW;
END;
$fn$;
REVOKE ALL ON FUNCTION public.touch_updated_at() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.touch_updated_at() TO wpmgr_app;

CREATE TRIGGER notes_touch BEFORE UPDATE ON "public"."notes"
  FOR EACH ROW EXECUTE FUNCTION public.touch_updated_at();
EOF
}

write_mig_3() {
  cat <<'EOF'
-- Fixture migration 3: a view and a sequence.
CREATE VIEW "public"."note_counts" AS
  SELECT "tenant_id", count(*) AS "n" FROM "public"."notes" GROUP BY "tenant_id";

CREATE SEQUENCE "public"."ticket_seq" START 100;
EOF
}

# An honest fourth migration: one new column.
write_mig_label() {
  cat <<'EOF'
-- Fixture migration 4: an honest new migration.
ALTER TABLE "public"."notes" ADD COLUMN "label" text NULL;
EOF
}

# A fourth migration that leaves no trace: the column it adds it also drops.
write_mig_scratch() {
  cat <<'EOF'
-- Fixture migration 4 (variant): a scratch column that is added and dropped.
ALTER TABLE "public"."notes" ADD COLUMN "scratch" text NULL;
ALTER TABLE "public"."notes" DROP COLUMN "scratch";
EOF
}

# A migration that holds only a comment: not empty, and valid.
write_mig_comment() {
  cat <<'EOF'
-- Fixture migration 4 (variant): a migration that only explains itself.
EOF
}

MIG1=20260101000000_init.sql
MIG2=20260101000100_functions.sql
MIG3=20260101000200_view_and_sequence.sql
MIG4=20260101000300_label.sql
MIG4B=20260101000300_scratch_column.sql
MIG_EMPTY=20260101000300_empty.sql
MIG_COMMENT=20260101000300_comment_only.sql

# Written by the real `atlas migrate hash`; see the header.
SUM_TRIO='h1:DHup/DUU8P+6NReRhHFanI34fPZPXP3poJ1wSWr+22I=
20260101000000_init.sql h1:Scgj7c+J5cPVf9BCMo5Jho0clqJpuhowTCOvjt8JdXs=
20260101000100_functions.sql h1:BYez+/u9lqgobQ+GQao2ImbFXn+8ZdW5MWEG0JUzUyY=
20260101000200_view_and_sequence.sql h1:RpCy6fuCdlrwGDm9KdFmD0LeDUPPBZ9/n+CWEBGjglo='

# The trio plus the label migration.
SUM_LABEL='h1:woVCAFw3BC+d+/1uKLlX2o1L9wTiWtrRaPWap6wpgWg=
20260101000000_init.sql h1:Scgj7c+J5cPVf9BCMo5Jho0clqJpuhowTCOvjt8JdXs=
20260101000100_functions.sql h1:BYez+/u9lqgobQ+GQao2ImbFXn+8ZdW5MWEG0JUzUyY=
20260101000200_view_and_sequence.sql h1:RpCy6fuCdlrwGDm9KdFmD0LeDUPPBZ9/n+CWEBGjglo=
20260101000300_label.sql h1:VV6z1TffaVtRvg0yxO6O5HcLF4NhAAcRFOYQ3X7LE60='

# The trio plus the scratch-column migration.
SUM_SCRATCH='h1:420F8DLKxP1yNYC0D4+6T8ecWLe0lyisb4LvqdMI1Lc=
20260101000000_init.sql h1:Scgj7c+J5cPVf9BCMo5Jho0clqJpuhowTCOvjt8JdXs=
20260101000100_functions.sql h1:BYez+/u9lqgobQ+GQao2ImbFXn+8ZdW5MWEG0JUzUyY=
20260101000200_view_and_sequence.sql h1:RpCy6fuCdlrwGDm9KdFmD0LeDUPPBZ9/n+CWEBGjglo=
20260101000300_scratch_column.sql h1:cK4Zh3xc80LGhlImKjx7LB+5w6w1ypjcw0koWOnMls4='

# The trio plus a zero-byte migration.
SUM_EMPTY='h1:PjJx4p1wpLPT62d+To6nSDcRc1CxVxkyA+tmHaXJ/Ds=
20260101000000_init.sql h1:Scgj7c+J5cPVf9BCMo5Jho0clqJpuhowTCOvjt8JdXs=
20260101000100_functions.sql h1:BYez+/u9lqgobQ+GQao2ImbFXn+8ZdW5MWEG0JUzUyY=
20260101000200_view_and_sequence.sql h1:RpCy6fuCdlrwGDm9KdFmD0LeDUPPBZ9/n+CWEBGjglo=
20260101000300_empty.sql h1:NQxJo1pGExjbDYNprLTFox6FZr7OJasjtBccv1p7tkg='

# The trio plus a migration holding only a comment.
SUM_COMMENT='h1:eA+bF2jnWUGlFeixMvek0ciX1LLAXsYd8WhhJ6G2Rb0=
20260101000000_init.sql h1:Scgj7c+J5cPVf9BCMo5Jho0clqJpuhowTCOvjt8JdXs=
20260101000100_functions.sql h1:BYez+/u9lqgobQ+GQao2ImbFXn+8ZdW5MWEG0JUzUyY=
20260101000200_view_and_sequence.sql h1:RpCy6fuCdlrwGDm9KdFmD0LeDUPPBZ9/n+CWEBGjglo=
20260101000300_comment_only.sql h1:cEP5LKDS8b8cm/Hi0Q50AQbfXz8as9nT64LFt6rs7rg='

write_schema() {
  cat <<'EOF'
-- Fixture schema.sql: the end state the fixture migrations produce.
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TYPE "public"."note_state" AS ENUM ('draft', 'live');

CREATE TABLE "public"."tenants" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "name" text NOT NULL,
  "slug" citext NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "tenants_slug_key" UNIQUE ("slug")
);

CREATE TABLE "public"."audit_log" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "at" timestamptz NOT NULL DEFAULT now(),
  "what" text NOT NULL,
  PRIMARY KEY ("id")
);

CREATE TABLE "public"."notes" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "title" text NOT NULL,
  "body" text NULL,
  "state" "public"."note_state" NOT NULL DEFAULT 'draft',
  "rank" integer NOT NULL DEFAULT 0,
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "notes_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "notes_rank_check" CHECK (rank >= 0)
);
CREATE INDEX "notes_tenant_id_idx" ON "public"."notes" ("tenant_id");
CREATE UNIQUE INDEX "notes_tenant_title_key" ON "public"."notes" ("tenant_id", "title") WHERE (state = 'live');

ALTER TABLE "public"."notes" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."notes" FORCE ROW LEVEL SECURITY;
CREATE POLICY "notes_tenant_isolation" ON "public"."notes"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY "notes_live_only" ON "public"."notes" AS RESTRICTIVE FOR SELECT
  USING (state = 'live' OR current_setting('app.role', true) = 'editor');

CREATE OR REPLACE FUNCTION public.touch_updated_at() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $fn$
BEGIN
  -- stamp the row
  NEW.updated_at := now();
  RETURN NEW;
END;
$fn$;
CREATE TRIGGER notes_touch BEFORE UPDATE ON "public"."notes"
  FOR EACH ROW EXECUTE FUNCTION public.touch_updated_at();

CREATE VIEW "public"."note_counts" AS
  SELECT "tenant_id", count(*) AS "n" FROM "public"."notes" GROUP BY "tenant_id";

CREATE SEQUENCE "public"."ticket_seq" START 100;
EOF
}

# Multi-line pieces of schema.sql that cases cut out or rearrange. Each is
# exactly the text in write_schema (a case stops if it is not found).
BLK_POL_ISO=$(cat <<'EOF'
CREATE POLICY "notes_tenant_isolation" ON "public"."notes"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
EOF
)
BLK_POL_LIVE=$(cat <<'EOF'
CREATE POLICY "notes_live_only" ON "public"."notes" AS RESTRICTIVE FOR SELECT
  USING (state = 'live' OR current_setting('app.role', true) = 'editor');
EOF
)
BLK_IDX_TENANT='CREATE INDEX "notes_tenant_id_idx" ON "public"."notes" ("tenant_id");'
BLK_IDX_TITLE=$(cat <<'EOF'
CREATE UNIQUE INDEX "notes_tenant_title_key" ON "public"."notes" ("tenant_id", "title") WHERE (state = 'live');
EOF
)
BLK_AUDIT=$(cat <<'EOF'
CREATE TABLE "public"."audit_log" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "at" timestamptz NOT NULL DEFAULT now(),
  "what" text NOT NULL,
  PRIMARY KEY ("id")
);
EOF
)
BLK_TRIGGER=$(cat <<'EOF'
CREATE TRIGGER notes_touch BEFORE UPDATE ON "public"."notes"
  FOR EACH ROW EXECUTE FUNCTION public.touch_updated_at();
EOF
)
NL=$'\n'

# --emit-fixture DIR trio|label|scratch -- write the migrations a sum is for,
# so the real atlas can hash them. Nothing else uses this mode.
if [ "${1:-}" = "--emit-fixture" ]; then
  dir="${2:?usage: --emit-fixture DIR trio|label|scratch|empty|comment}"
  which="${3:-trio}"
  mkdir -p "$dir" || exit 2
  write_mig_1 > "$dir/$MIG1"
  write_mig_2 > "$dir/$MIG2"
  write_mig_3 > "$dir/$MIG3"
  case "$which" in
    trio) : ;;
    label) write_mig_label > "$dir/$MIG4" ;;
    scratch) write_mig_scratch > "$dir/$MIG4B" ;;
    empty) : > "$dir/$MIG_EMPTY" ;;
    comment) write_mig_comment > "$dir/$MIG_COMMENT" ;;
    *) echo "unknown fixture set: $which" >&2; exit 2 ;;
  esac
  echo "wrote the '$which' fixture migrations to $dir"
  exit 0
fi

# Set to anything to print what the guard said for every case, not only the
# failing ones: how a reviewer reads the red output of each plant.
VERBOSE="${WPMGR_SCHEMA_SYNC_TEST_VERBOSE:-}"

REAL=0
FILTER=""
case "${1:-}" in
  --real) REAL=1 ;;
  *) FILTER="${1:-}" ;;
esac

if [ ! -f "$GUARD" ]; then
  echo "no guard script at $GUARD" >&2
  exit 2
fi
command -v docker >/dev/null 2>&1 || { echo "docker is not installed: this suite cannot run, which is not a pass." >&2; exit 2; }
docker info >/dev/null 2>&1 || { echo "the docker daemon is not reachable: this suite cannot run, which is not a pass." >&2; exit 2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-schema-sync-test.XXXXXX")" || exit 2
SHARED=""
cleanup() {
  if [ -n "$SHARED" ]; then docker rm -f -v "$SHARED" >/dev/null 2>&1; fi
  rm -rf "${WORK:?}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

PASSED=0
FAILED=0
SKIPPED=0
FAILED_NAMES=''

# ---------------------------------------------------------------------------
# One postgres for every case.
# ---------------------------------------------------------------------------
SHARED="wpmgr-schema-sync-test-$$-$RANDOM"
if ! start_err="$(docker run -d --rm --name "$SHARED" --label wpmgr.schema-sync=1 \
  --network none --tmpfs /var/lib/postgresql/data:rw \
  -e POSTGRES_PASSWORD=throwaway "$IMAGE" 2>&1 >/dev/null)"; then
  SHARED=""
  echo "could not start the shared postgres ($IMAGE): $start_err" >&2
  exit 2
fi
ready=0
i=0
while [ "$i" -lt 300 ]; do
  if docker exec "$SHARED" pg_isready -h 127.0.0.1 -U postgres -q >/dev/null 2>&1 \
     && docker exec "$SHARED" psql -U postgres -X -At -d postgres -c 'SELECT 1' >/dev/null 2>&1; then
    ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.2
done
if [ "$ready" != "1" ]; then
  echo "the shared postgres never became ready." >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# Tree construction and mutation (portable: no sed -i)
# ---------------------------------------------------------------------------
build_base() {
  local d="$WORK/base"
  mkdir -p "$d/$MIG_REL" "$d/apps/api/db" || exit 2
  write_mig_1 > "$d/$MIG_REL/$MIG1"
  write_mig_2 > "$d/$MIG_REL/$MIG2"
  write_mig_3 > "$d/$MIG_REL/$MIG3"
  printf '%s\n' "$SUM_TRIO" > "$d/$MIG_REL/atlas.sum"
  write_schema > "$d/$SCHEMA_REL"
}

tree() { # tree NAME -- a fresh copy of the base tree; prints its path
  local d="${WORK:?}/${1:?}"
  rm -rf "${d:?}"
  cp -R "$WORK/base" "$d" || exit 2
  printf '%s' "$d"
}

setup_error() {
  echo "SETUP ERROR: $1" >&2
  echo "             a case whose mutation did not apply would test nothing, so the suite stops here." >&2
  exit 2
}

# replace_lit FILE OLD NEW -- replace the first occurrence of the LITERAL text
# OLD. The suite stops if OLD is absent: a plant that does not plant would
# leave a "must stay green" case passing without having tested anything.
#
# awk does the replacement, with both strings passed through the environment:
# `-v` would process backslash escapes, and bash's own ${var/pat/rep} keeps or
# drops nested quote characters depending on the bash version, which turned
# every mutation into a syntax error on one of them. The whole file is one
# record (RS is a byte that is not in any fixture), so multi-line text matches.
replace_lit() {
  local f="$1"
  if ! OLD="$2" NEW="$3" awk -v RS='\001' '
    BEGIN { old = ENVIRON["OLD"]; new = ENVIRON["NEW"] }
    { i = index($0, old); if (i == 0) exit 3; printf "%s%s%s", substr($0, 1, i - 1), new, substr($0, i + length(old)) }
  ' "$f" > "$f.replaced"; then
    rm -f "${f:?}.replaced"
    setup_error "the text to replace is not in $f: $2"
  fi
  mv "$f.replaced" "$f" || setup_error "could not rewrite $f"
}
delete_lit() { replace_lit "$1" "$2" ""; }
append_text() { printf '%s\n' "$2" >> "$1"; }

# ---------------------------------------------------------------------------
# Assertions
#
#   case_run NAME pass|fail|broken TREE [+MUST-CONTAIN] [-MUST-NOT-CONTAIN]
#            [env:VAR=value]
#
# Assertions test the output with a here-string, not a pipe: under pipefail a
# `printf | grep -q` reports failure when grep exits early and printf takes
# SIGPIPE, which would read a found needle as a missing one on long output.
# ---------------------------------------------------------------------------
case_run() {
  local name="$1" want="$2" dir="$3" a needle envs="" out code problems="" wantcode
  shift 3

  if [ -n "$FILTER" ]; then
    case "$name" in
      *"$FILTER"*) : ;;
      *) SKIPPED=$((SKIPPED + 1)); return 0 ;;
    esac
  fi
  case "$want" in
    pass) wantcode=0 ;;
    fail) wantcode=1 ;;
    broken) wantcode=2 ;;
    *) setup_error "unknown expectation '$want' for: $name" ;;
  esac

  for a in "$@"; do
    case "$a" in
      env:*) envs="$envs ${a#env:}" ;;
    esac
  done

  # shellcheck disable=SC2086  # the env words are deliberate, and contain no spaces
  out="$(env WPMGR_SCHEMA_SYNC_CONTAINER="$SHARED" $envs "$GUARD" "$dir" 2>&1)"
  code=$?

  if [ "$code" -ne "$wantcode" ]; then
    problems="$problems
    expected exit $wantcode, got $code"
  fi
  for a in "$@"; do
    case "$a" in
      +*)
        needle="${a#+}"
        if ! grep -qF -- "$needle" <<< "$out"; then
          problems="$problems
    expected the output to contain: $needle"
        fi
        ;;
      -*)
        needle="${a#-}"
        if grep -qF -- "$needle" <<< "$out"; then
          problems="$problems
    expected the output NOT to contain: $needle"
        fi
        ;;
    esac
  done

  if [ -n "$problems" ]; then
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES  $name
"
    printf 'FAIL %s%s\n' "$name" "$problems"
    printf '%s\n' "$out" | sed 's/^/      | /'
  else
    PASSED=$((PASSED + 1))
    printf 'ok   %s\n' "$name"
    if [ -n "$VERBOSE" ]; then
      printf '%s\n' "$out" | sed 's/^/      | /'
    fi
  fi
}

build_base

S="$SCHEMA_REL"
M="$MIG_REL"

# ===========================================================================
# THE BASELINE. If this is not green nothing below means anything, and it is
# also the proof that the guard's atlas.sum arithmetic matches the real Atlas.
# ===========================================================================
if [ "$REAL" = "0" ]; then

t="$(tree base-green)"
case_run "base: the fixture tree is in step" pass "$t" \
  "+atlas.sum lists exactly the 3 migration files" \
  "+compared:" "+3 table" "+2 policy" "+1 function" "+1 trigger" "+1 view" "+1 sequence" "+1 extension" "+1 type" \
  "-FAIL" "-GUARD BROKEN"

# ===========================================================================
# FIRES: each real drift planted alone, red, for the right reason.
# ===========================================================================
t="$(tree col-missing)"
delete_lit "$t/$S" $'  "body" text NULL,\n'
case_run "fires: a column missing from schema.sql" fail "$t" \
  "+MISSING from db/schema.sql (1)" "+public.notes.body" "+text NULL" "-NOT produced"

t="$(tree col-extra)"
replace_lit "$t/$S" $'  "rank" integer NOT NULL DEFAULT 0,\n' $'  "rank" integer NOT NULL DEFAULT 0,\n  "extra" text NULL,\n'
case_run "fires: a column in schema.sql the migrations never created" fail "$t" \
  "+NOT produced by the migrations (1)" "+public.notes.extra"

t="$(tree col-type)"
replace_lit "$t/$S" '"rank" integer NOT NULL' '"rank" bigint NOT NULL'
case_run "fires: a column whose type differs" fail "$t" \
  "+DIFFERENT (1)" "+public.notes.rank" "+migrations: integer NOT NULL" "+schema.sql: bigint NOT NULL"

t="$(tree col-null)"
replace_lit "$t/$S" '"body" text NULL' '"body" text NOT NULL'
case_run "fires: a column whose nullability differs" fail "$t" \
  "+public.notes.body" "+migrations: text NULL" "+schema.sql: text NOT NULL"

t="$(tree col-default)"
replace_lit "$t/$S" '"rank" integer NOT NULL DEFAULT 0' '"rank" integer NOT NULL DEFAULT 1'
case_run "fires: a column whose default differs" fail "$t" \
  "+public.notes.rank" "+DEFAULT 0" "+DEFAULT 1"

t="$(tree idx-missing)"
delete_lit "$t/$S" "$BLK_IDX_TENANT$NL"
case_run "fires: an index missing from schema.sql" fail "$t" \
  "+MISSING from db/schema.sql (1)" "+public.notes_tenant_id_idx"

t="$(tree idx-predicate)"
replace_lit "$t/$S" "WHERE (state = 'live')" "WHERE (state = 'draft')"
case_run "fires: an index whose predicate differs" fail "$t" \
  "+DIFFERENT (1)" "+public.notes_tenant_title_key"

t="$(tree con-check-missing)"
delete_lit "$t/$S" $',\n  CONSTRAINT "notes_rank_check" CHECK (rank >= 0)'
case_run "fires: a CHECK constraint missing from schema.sql" fail "$t" \
  "+MISSING from db/schema.sql (1)" "+public.notes.notes_rank_check"

t="$(tree con-fk-action)"
replace_lit "$t/$S" 'ON UPDATE NO ACTION ON DELETE CASCADE' 'ON UPDATE NO ACTION ON DELETE NO ACTION'
case_run "fires: a foreign key whose ON DELETE action differs" fail "$t" \
  "+DIFFERENT (1)" "+public.notes.notes_tenant_id_fkey" "+ON DELETE CASCADE"

t="$(tree pol-missing)"
delete_lit "$t/$S" "$BLK_POL_LIVE$NL"
case_run "fires: an RLS policy missing from schema.sql" fail "$t" \
  "+MISSING from db/schema.sql (1)" "+policy" "+public.notes.notes_live_only" "+RESTRICTIVE SELECT"

t="$(tree pol-using)"
replace_lit "$t/$S" "= 'editor'" "= 'admin'"
case_run "fires: an RLS policy whose USING expression differs" fail "$t" \
  "+DIFFERENT (1)" "+public.notes.notes_live_only" "+editor" "+admin"

t="$(tree pol-permissive)"
replace_lit "$t/$S" ' AS RESTRICTIVE FOR SELECT' ' FOR SELECT'
case_run "fires: a RESTRICTIVE policy written as PERMISSIVE" fail "$t" \
  "+DIFFERENT (1)" "+public.notes.notes_live_only" "+RESTRICTIVE" "+PERMISSIVE"

t="$(tree pol-renamed)"
replace_lit "$t/$S" '"notes_live_only"' '"notes_visible_only"'
case_run "fires: a renamed policy reads as one missing and one extra" fail "$t" \
  "+MISSING from db/schema.sql (1)" "+NOT produced by the migrations (1)" "+notes_live_only" "+notes_visible_only"

t="$(tree rls-force)"
delete_lit "$t/$S" $'ALTER TABLE "public"."notes" FORCE ROW LEVEL SECURITY;\n'
case_run "fires: FORCE ROW LEVEL SECURITY missing from schema.sql" fail "$t" \
  "+DIFFERENT (1)" "+rls" "+public.notes" "+migrations: enabled=true forced=true" "+schema.sql: enabled=true forced=false"

t="$(tree rls-enable)"
delete_lit "$t/$S" $'ALTER TABLE "public"."notes" ENABLE ROW LEVEL SECURITY;\n'
case_run "fires: ENABLE ROW LEVEL SECURITY missing from schema.sql" fail "$t" \
  "+DIFFERENT (1)" "+rls" "+schema.sql: enabled=false forced=true"

t="$(tree tbl-missing)"
delete_lit "$t/$S" "$BLK_AUDIT$NL"
case_run "fires: a whole table missing from schema.sql" fail "$t" \
  "+MISSING from db/schema.sql" "+table" "+public.audit_log" "+public.audit_log.what"

t="$(tree tbl-extra)"
append_text "$t/$S" 'CREATE TABLE "public"."orphans" ("id" uuid NOT NULL, PRIMARY KEY ("id"));'
case_run "fires: a table in schema.sql the migrations never created" fail "$t" \
  "+NOT produced by the migrations" "+public.orphans"

t="$(tree fn-body)"
replace_lit "$t/$S" 'NEW.updated_at := now();' 'NEW.updated_at := clock_timestamp();'
case_run "fires: a function whose body differs" fail "$t" \
  "+DIFFERENT (1)" "+function" "+public.touch_updated_at()" "+clock_timestamp"

t="$(tree fn-secdef)"
delete_lit "$t/$S" $'SECURITY DEFINER\n'
case_run "fires: a function that lost SECURITY DEFINER" fail "$t" \
  "+DIFFERENT (1)" "+public.touch_updated_at()" "+SECURITY DEFINER"

t="$(tree trg-missing)"
delete_lit "$t/$S" "$BLK_TRIGGER$NL"
case_run "fires: a trigger missing from schema.sql" fail "$t" \
  "+MISSING from db/schema.sql (1)" "+trigger" "+public.notes.notes_touch"

t="$(tree seq-start)"
replace_lit "$t/$S" 'START 100' 'START 200'
case_run "fires: a sequence whose start differs" fail "$t" \
  "+DIFFERENT (1)" "+public.ticket_seq"

t="$(tree enum-label)"
replace_lit "$t/$S" "('draft', 'live')" "('draft', 'live', 'archived')"
case_run "fires: an enum type with an extra label" fail "$t" \
  "+DIFFERENT (1)" "+public.note_state" "+labels=draft,live" "+labels=draft,live,archived"

t="$(tree view-def)"
replace_lit "$t/$S" 'count(*) AS "n"' 'count(*) AS "total"'
case_run "fires: a view whose definition differs" fail "$t" \
  "+DIFFERENT (1)" "+public.note_counts"

t="$(tree ext-extra)"
replace_lit "$t/$S" 'CREATE EXTENSION IF NOT EXISTS citext;' $'CREATE EXTENSION IF NOT EXISTS citext;\nCREATE EXTENSION IF NOT EXISTS pg_trgm;'
case_run "fires: an extension in schema.sql the migrations never installed" fail "$t" \
  "+NOT produced by the migrations (1)" "+extension" "+pg_trgm"

# The #759 shape: the migration history moved on, atlas.sum was kept honest,
# and schema.sql was not updated.
t="$(tree migration-ahead)"
write_mig_label > "$t/$M/$MIG4"
printf '%s\n' "$SUM_LABEL" > "$t/$M/atlas.sum"
case_run "fires: a new migration with an honest atlas.sum and a schema.sql nobody updated" fail "$t" \
  "+MISSING from db/schema.sql (1)" "+public.notes.label" "-FAIL: atlas.sum" "+OK: atlas.sum lists exactly the 4 migration files"

# ===========================================================================
# atlas.sum: names, order, hashes.
# ===========================================================================
t="$(tree sum-unlisted)"
write_mig_label > "$t/$M/$MIG4"
replace_lit "$t/$S" $'  "updated_at" timestamptz NOT NULL DEFAULT now(),\n' $'  "updated_at" timestamptz NOT NULL DEFAULT now(),\n  "label" text NULL,\n'
case_run "fires: an extra migration that atlas.sum does not name" fail "$t" \
  "+atlas.sum does not list 1 migration file(s) that are on disk" "+$MIG4" "+atlas migrate hash"

t="$(tree sum-stale)"
printf '%s\n' "$SUM_LABEL" > "$t/$M/atlas.sum"
case_run "fires: atlas.sum naming a migration file that is not on disk" fail "$t" \
  "+atlas.sum lists 1 file(s) that are not on disk" "+$MIG4"

t="$(tree sum-edited)"
printf '%s\n' '-- an edit made after atlas.sum was written' >> "$t/$M/$MIG3"
case_run "fires: a migration edited after atlas.sum was written" fail "$t" \
  "+atlas.sum hashes do not match the files on disk" "+$MIG3" "-does not list" "-not on disk"

t="$(tree sum-header)"
{
  echo 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='
  sed -n '2,$p' "$t/$M/atlas.sum"
} > "$t/$M/atlas.sum.new"
mv "$t/$M/atlas.sum.new" "$t/$M/atlas.sum"
case_run "fires: a corrupted top-line checksum in atlas.sum" fail "$t" \
  "+atlas.sum hashes do not match the files on disk" "+HEADER"

t="$(tree sum-order)"
{
  sed -n '1p' "$t/$M/atlas.sum"
  sed -n '3p' "$t/$M/atlas.sum"
  sed -n '2p' "$t/$M/atlas.sum"
  sed -n '4p' "$t/$M/atlas.sum"
} > "$t/$M/atlas.sum.new"
mv "$t/$M/atlas.sum.new" "$t/$M/atlas.sum"
case_run "fires: atlas.sum listing the right files in the wrong order" fail "$t" \
  "+wrong order" "+entry 1"

t="$(tree sum-dupe)"
sed -n '2p' "$t/$M/atlas.sum" >> "$t/$M/atlas.sum"
case_run "fires: atlas.sum naming a file twice" fail "$t" \
  "+more than once" "+$MIG1"

t="$(tree sum-format)"
printf '%s\n' 'not a sum line' >> "$t/$M/atlas.sum"
case_run "fires: a line in atlas.sum that is not a sum entry" fail "$t" \
  "+not \"<file>.sql h1:<hash>\"" "+not a sum line"

t="$(tree sum-version)"
replace_lit "$t/$M/atlas.sum" 'h1:' 'h2:'
case_run "fires: an atlas.sum header the guard does not understand is refused, not guessed at" fail "$t" \
  "+not an h1: checksum header"

# ===========================================================================
# A guard that finds nothing must go red (exit 2, never 0).
# ===========================================================================
t="$(tree no-migrations-empty-dir)"
rm -f "$t/$M"/*
case_run "broken: an empty migrations directory" broken "$t" \
  "+GUARD BROKEN" "+no *.sql files" "-are in step"

t="$(tree no-migrations-only-sum)"
rm -f "$t/$M"/*.sql
case_run "broken: a migrations directory holding only atlas.sum" broken "$t" \
  "+GUARD BROKEN" "+no *.sql files" "-are in step"

t="$(tree no-migrations-dir)"
rm -rf "${t:?}/$M"
case_run "broken: no migrations directory at all" broken "$t" \
  "+GUARD BROKEN" "+no migrations directory" "-are in step"

t="$(tree no-schema)"
rm -f "$t/$S"
case_run "broken: a missing schema.sql" broken "$t" \
  "+GUARD BROKEN" "+no schema file" "-are in step"

t="$(tree empty-schema)"
: > "$t/$S"
case_run "broken: a zero-byte schema.sql" broken "$t" \
  "+GUARD BROKEN" "+is empty" "-are in step"

t="$(tree comment-schema)"
printf '%s\n' '-- nothing here but a comment' '/* and another */' > "$t/$S"
case_run "broken: a schema.sql that is only comments creates nothing" broken "$t" \
  "+GUARD BROKEN" "+schema.sql created nothing" "-are in step"

t="$(tree extension-only-schema)"
printf '%s\n' 'CREATE EXTENSION IF NOT EXISTS citext;' > "$t/$S"
case_run "broken: a schema.sql that creates an extension but no tables" broken "$t" \
  "+GUARD BROKEN" "+created no tables" "-are in step"

t="$(tree no-sum)"
rm -f "$t/$M/atlas.sum"
case_run "broken: a missing atlas.sum" broken "$t" \
  "+GUARD BROKEN" "+no atlas.sum" "-are in step"

t="$(tree no-policies)"
delete_lit "$t/$M/$MIG1" "$BLK_POL_ISO$NL$BLK_POL_LIVE$NL"
case_run "broken: a replay that yields no policy rows is a broken extraction, not a clean schema" broken "$t" \
  "+GUARD BROKEN" "+no 'policy' rows" "-OK: apps/api/db/schema.sql matches"

t="$(tree dead-container)"
case_run "broken: a postgres container that does not exist" broken "$t" \
  "env:WPMGR_SCHEMA_SYNC_CONTAINER=wpmgr-schema-sync-no-such-container" "+GUARD BROKEN" "-are in step"

t="$(tree dead-docker)"
case_run "broken: a docker daemon that is not reachable" broken "$t" \
  "env:DOCKER_HOST=unix:///nonexistent/docker.sock" "+GUARD BROKEN" "+docker daemon is not reachable" "-are in step"

# ===========================================================================
# An empty migration among valid ones (bot review of #868, gap 3). The sum is
# regenerated over the zero-byte file, so atlas.sum cannot object; only an
# explicit emptiness check can.
# ===========================================================================
t="$(tree empty-file-before-docker)"
: > "$t/$M/$MIG_EMPTY"
printf '%s\n' "$SUM_EMPTY" > "$t/$M/atlas.sum"
case_run "broken: [empty-file] a zero-byte migration is refused before docker is touched" broken "$t" \
  "env:DOCKER_HOST=unix:///nonexistent/docker.sock" \
  "+GUARD BROKEN" "+migration $MIG_EMPTY is empty" "-docker daemon" "-are in step"

t="$(tree empty-file)"
: > "$t/$M/$MIG_EMPTY"
printf '%s\n' "$SUM_EMPTY" > "$t/$M/atlas.sum"
case_run "broken: [empty-file] one zero-byte migration among valid ones, atlas.sum regenerated over it" broken "$t" \
  "+GUARD BROKEN" "+migration $MIG_EMPTY is empty" "-are in step"

t="$(tree comment-only-migration)"
write_mig_comment > "$t/$M/$MIG_COMMENT"
printf '%s\n' "$SUM_COMMENT" > "$t/$M/atlas.sum"
case_run "honest: [empty-file] a migration holding only a comment is not empty and stays valid" pass "$t" \
  "+atlas.sum lists exactly the 4 migration files" "+compared:" "-FAIL" "-GUARD BROKEN"

# ===========================================================================
# Findings that are not drift: the two inputs do not load.
# ===========================================================================
t="$(tree schema-syntax)"
replace_lit "$t/$S" 'CREATE TABLE "public"."audit_log" (' 'CREATE TABEL "public"."audit_log" ('
case_run "fires: a schema.sql that does not load" fail "$t" \
  "+does not load into an empty database" "+syntax error"

t="$(tree schema-order)"
delete_lit "$t/$S" $'CREATE INDEX "notes_tenant_id_idx" ON "public"."notes" ("tenant_id");\n'
replace_lit "$t/$S" 'CREATE TYPE "public"."note_state"' $'CREATE INDEX "notes_tenant_id_idx" ON "public"."notes" ("tenant_id");\nCREATE TYPE "public"."note_state"'
case_run "fires: a statement that precedes what it references (schema.sql must load in one pass)" fail "$t" \
  "+does not load into an empty database" "+does not exist"

t="$(tree migration-error)"
append_text "$t/$M/$MIG3" 'SELECT * FROM table_that_does_not_exist;'
case_run "fires: a migration that does not apply to an empty database, named in the report" fail "$t" \
  "+do not apply to an empty database" "+migration: $MIG3" "+table_that_does_not_exist"

# ===========================================================================
# HONEST CASES: differences the guard must NOT block. A guard that reddens
# correct work gets switched off, and then it guards nothing. Each is asserted
# green AND asserted to have compared something (the "compared:" line), so a
# guard that passed by comparing nothing could not satisfy these.
# ===========================================================================
t="$(tree honest-whitespace)"
# Join a statement onto one line first (the anchor would not survive the awk
# pass below), then tabs for indentation, trailing spaces, blank lines.
replace_lit "$t/$S" "$BLK_POL_ISO" "${BLK_POL_ISO//$NL/    }"
awk '{ sub(/^  /, "\t"); printf "%s   \n", $0; if ($0 ~ /;[ \t]*$/) print "" }' "$t/$S" > "$t/$S.new" && mv "$t/$S.new" "$t/$S"
case_run "honest: whitespace-only differences (tabs, trailing spaces, blank lines, joined lines)" pass "$t" \
  "+compared:" "+3 table" "-FAIL"

t="$(tree honest-comments)"
replace_lit "$t/$S" '-- stamp the row' '-- a different comment here, which says nothing the code does not'
replace_lit "$t/$S" 'CREATE TYPE "public"."note_state" AS ENUM' $'/* a block comment\n   over two lines */\nCREATE TYPE "public"."note_state" /* inline */ AS ENUM'
replace_lit "$t/$S" '  "tenant_id" uuid NOT NULL,' $'  -- the owning tenant\n  "tenant_id" uuid NOT NULL, -- trailing note'
replace_lit "$t/$S" "USING (state = 'live' OR" "USING (/* visible rows */ state = 'live' OR"
append_text "$t/$S" '-- end of file'
case_run "honest: comment-only differences, including inside a function body and a policy" pass "$t" \
  "+compared:" "+1 function" "+2 policy" "-FAIL"

t="$(tree honest-comment-removed)"
delete_lit "$t/$S" $'  -- stamp the row\n'
case_run "honest: a comment present in the migration's function body and absent from schema.sql" pass "$t" \
  "+compared:" "-FAIL"

t="$(tree honest-order)"
# Policies swapped, indexes swapped, the sequence moved to the top, a column
# moved.
replace_lit "$t/$S" "$BLK_POL_ISO$NL$BLK_POL_LIVE" "$BLK_POL_LIVE$NL$BLK_POL_ISO"
replace_lit "$t/$S" "$BLK_IDX_TENANT$NL$BLK_IDX_TITLE" "$BLK_IDX_TITLE$NL$BLK_IDX_TENANT"
delete_lit "$t/$S" $'CREATE SEQUENCE "public"."ticket_seq" START 100;\n'
replace_lit "$t/$S" 'CREATE EXTENSION IF NOT EXISTS citext;' $'CREATE SEQUENCE "public"."ticket_seq" START 100;\nCREATE EXTENSION IF NOT EXISTS citext;'
replace_lit "$t/$S" $'  "title" text NOT NULL,\n  "body" text NULL,\n' $'  "body" text NULL,\n  "title" text NOT NULL,\n'
case_run "honest: statements and columns in a different order" pass "$t" \
  "+compared:" "+3 table" "+2 policy" "-FAIL"

t="$(tree honest-quoting)"
sed -e 's/"public"\.//g' -e 's/"//g' "$t/$S" > "$t/$S.new" && mv "$t/$S.new" "$t/$S"
case_run "honest: unquoted, unqualified identifiers" pass "$t" \
  "+compared:" "+3 table" "-FAIL"

t="$(tree honest-alter-constraints)"
delete_lit "$t/$S" $',\n  CONSTRAINT "notes_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,\n  CONSTRAINT "notes_rank_check" CHECK (rank >= 0)'
append_text "$t/$S" 'ALTER TABLE "public"."notes" ADD CONSTRAINT "notes_rank_check" CHECK (rank >= 0);'
append_text "$t/$S" 'ALTER TABLE "public"."notes" ADD CONSTRAINT "notes_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE;'
case_run "honest: constraints added by ALTER TABLE rather than inline" pass "$t" \
  "+compared:" "+constraint" "-FAIL"

t="$(tree honest-privileges)"
append_text "$t/$S" 'GRANT SELECT ON "public"."notes" TO wpmgr_app;'
append_text "$t/$S" 'REVOKE ALL ON FUNCTION public.touch_updated_at() FROM PUBLIC;'
case_run "honest: privileges are not compared (GRANT and REVOKE differ either way)" pass "$t" \
  "+compared:" "-FAIL"

t="$(tree honest-seed)"
append_text "$t/$S" "INSERT INTO \"public\".\"tenants\" (\"name\", \"slug\") VALUES ('another', 'another');"
case_run "honest: seed rows in schema.sql that the migrations do not insert" pass "$t" \
  "+compared:" "-FAIL"

t="$(tree honest-new-migration)"
write_mig_label > "$t/$M/$MIG4"
printf '%s\n' "$SUM_LABEL" > "$t/$M/atlas.sum"
replace_lit "$t/$S" $'  "updated_at" timestamptz NOT NULL DEFAULT now(),\n' $'  "updated_at" timestamptz NOT NULL DEFAULT now(),\n  "label" text NULL,\n'
case_run "honest: a new migration, schema.sql updated, atlas.sum re-hashed" pass "$t" \
  "+atlas.sum lists exactly the 4 migration files" "+compared:" "-FAIL"

t="$(tree honest-dropped-column)"
write_mig_scratch > "$t/$M/$MIG4B"
printf '%s\n' "$SUM_SCRATCH" > "$t/$M/atlas.sum"
case_run "honest: a migration that adds and drops a column leaves no trace for schema.sql to mirror" pass "$t" \
  "+atlas.sum lists exactly the 4 migration files" "+compared:" "-scratch" "-FAIL"

# ===========================================================================
# Hygiene: the guard cleans up after itself.
# ===========================================================================
if [ -z "$FILTER" ] || [ "${FILTER#hygiene}" != "$FILTER" ]; then
  left="$(docker exec "$SHARED" psql -U postgres -X -At -d postgres -c "SELECT count(*) FROM pg_database WHERE datname LIKE 'ss\_%'" 2>&1)"
  if [ "$left" = "0" ]; then
    PASSED=$((PASSED + 1))
    printf 'ok   %s\n' "hygiene: no scratch database is left in a shared container after any case"
  else
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES  hygiene: scratch databases left behind
"
    printf 'FAIL hygiene: %s scratch database(s) left in the shared container\n' "$left"
  fi

  # A guard that starts its own container must remove it, however it ends.
  before="$(docker ps -aq --filter label=wpmgr.schema-sync=1 | wc -l | tr -d ' ')"
  t="$(tree hygiene-own-container)"
  env -u WPMGR_SCHEMA_SYNC_CONTAINER "$GUARD" "$t" > "$WORK/own.out" 2>&1
  own_code=$?
  # A comment-only schema.sql is refused only AFTER the container has started,
  # so this is the failure path that must still remove it.
  printf '%s\n' '-- nothing here' > "$t/$S"
  env -u WPMGR_SCHEMA_SYNC_CONTAINER "$GUARD" "$t" > "$WORK/own2.out" 2>&1
  own2_code=$?
  after="$(docker ps -aq --filter label=wpmgr.schema-sync=1 | wc -l | tr -d ' ')"
  if [ "$own_code" = "0" ] && [ "$own2_code" = "2" ] && [ "$before" = "$after" ]; then
    PASSED=$((PASSED + 1))
    printf 'ok   %s\n' "hygiene: a guard that starts its own postgres removes it (clean run and failed run)"
  else
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES  hygiene: own container
"
    printf 'FAIL hygiene: own container (clean run exit %s, expected 0; failed run exit %s, expected 2; labelled containers before %s after %s)\n' \
      "$own_code" "$own2_code" "$before" "$after"
    sed 's/^/      | /' "$WORK/own.out"
  fi
fi

fi # REAL = 0

# ===========================================================================
# --real: the four mandated plants against a scratch copy of THIS repository's
# own migrations and schema.sql, then the untouched tree. Slower, so opt-in.
# ===========================================================================
if [ "$REAL" = "1" ]; then
  [ -d "$REPO_ROOT/$MIG_REL" ] || { echo "no $MIG_REL under $REPO_ROOT" >&2; exit 2; }
  mkdir -p "$WORK/real-src/$MIG_REL" "$WORK/real-src/apps/api/db" || exit 2
  cp "$REPO_ROOT/$MIG_REL"/*.sql "$REPO_ROOT/$MIG_REL/atlas.sum" "$WORK/real-src/$MIG_REL/" || exit 2
  cp "$REPO_ROOT/$SCHEMA_REL" "$WORK/real-src/$SCHEMA_REL" || exit 2

  rtree() { rm -rf "${WORK:?}/${1:?}"; cp -R "$WORK/real-src" "$WORK/$1" || exit 2; printf '%s' "$WORK/$1"; }

  t="$(rtree real-green)"
  case_run "real tree: untouched, as it stands" pass "$t" "+compared:" "+OK: atlas.sum lists exactly" "-FAIL"

  t="$(rtree real-column)"
  delete_lit "$t/$SCHEMA_REL" $'    php_version text        NOT NULL DEFAULT \'\',\n'
  case_run "real tree, plant 1: a column missing from schema.sql" fail "$t" \
    "+MISSING from db/schema.sql (1)" "+public.sites.php_version"

  t="$(rtree real-policy)"
  delete_lit "$t/$SCHEMA_REL" "CREATE POLICY sites_tenant_isolation ON sites
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
"
  case_run "real tree, plant 2: an RLS policy missing from schema.sql" fail "$t" \
    "+MISSING from db/schema.sql (1)" "+public.sites.sites_tenant_isolation"

  t="$(rtree real-extra-migration)"
  printf '%s\n' '-- planted: a migration atlas.sum has never heard of' 'SELECT 1;' > "$t/$MIG_REL/29991231000000_planted.sql"
  case_run "real tree, plant 3: an extra migration that atlas.sum does not name" fail "$t" \
    "+atlas.sum does not list 1 migration file(s) that are on disk" "+29991231000000_planted.sql"

  t="$(rtree real-empty-migrations)"
  rm -f "$t/$MIG_REL"/*
  case_run "real tree, plant 4: an empty migrations directory" broken "$t" \
    "+GUARD BROKEN" "+no *.sql files" "-are in step"
fi

# ---------------------------------------------------------------------------
printf '\n'
if [ -n "$FILTER" ]; then
  printf 'filter: %s (%s skipped)\n' "$FILTER" "$SKIPPED"
fi
printf '%s passed, %s failed, elapsed: %ss\n' "$PASSED" "$FAILED" "$SECONDS"
if [ "$FAILED" -ne 0 ]; then
  printf 'failing cases:\n%s' "$FAILED_NAMES"
  exit 1
fi
if [ "$PASSED" -eq 0 ]; then
  echo "no case ran: a suite that checked nothing is not a pass." >&2
  exit 2
fi
exit 0
