#!/usr/bin/env bash
# scripts/check-rls-cross-tenant.sh
#
# Every RLS policy that grants access ACROSS tenants, found by the SETTING it
# tests, and checked against a committed record of what its access mode is
# meant to be.
#
# ---------------------------------------------------------------------------
# WHY THIS EXISTS (GH #470)
# ---------------------------------------------------------------------------
#
# Two defects, and this guard is aimed at both.
#
# 1. THE CONSTRUCT IS NOT CONSISTENTLY NAMED, so auditing by name produces
#    FALSE NEGATIVES -- the direction that costs a tenant boundary. The same
#    "let the cross-tenant worker through" policy ships under several suffixes.
#    Run the guard and read the NAMING section it prints: it counts the
#    variants for you rather than quoting a number that rots. Among the
#    outliers is backup_schedules_scheduler, which IS the m84/#96 fix -- so a
#    grep for '_agent' misses the very policy the post-mortem is about.
#
#    THEREFORE: this guard never greps a policy name. It enumerates from
#    pg_policies and classifies on the GUC the policy's expression tests.
#
# 2. SOME OF THOSE POLICIES ARE `FOR SELECT` WHERE THE CODE PATH WRITES.
#    CREATE POLICY defaults to FOR ALL, so a correct policy and a broken one
#    are visually identical: the clause the bug turns on is the one nobody
#    writes. Under FORCE ROW LEVEL SECURITY a FOR SELECT policy admits the
#    read and admits nothing to the UPDATE, so the write matches ZERO ROWS
#    WITH NO ERROR. PostgreSQL applies the UPDATE policy to SELECT ... FOR
#    UPDATE too, so even the locking read comes back empty.
#
#    That is the signature: not a failure, a silence. It has shipped three
#    times -- m84/#96 (backup_schedules, every schedule stopped advancing),
#    m89/#131 (update_tasks, the stale-task reaper swept nothing), and
#    GH #463 Phase 0 (update_runs, the deferred dispatcher claimed nothing).
#
# ---------------------------------------------------------------------------
# HOW IT DECIDES WHAT IS "CROSS-TENANT"
# ---------------------------------------------------------------------------
#
# A PERMISSIVE policy whose USING expression does not bind tenant_id to
# app.tenant_id grants rows outside the caller's tenant. That is a property of
# the expression, not of the name, and it is what this guard tests.
#
# RESTRICTIVE policies are excluded on purpose: a restrictive policy can only
# ever narrow the row set, never grant. site_scope policies are restrictive and
# do not reference app.tenant_id, so a classifier that ignored `permissive`
# would flag all 40-odd of them as cross-tenant grants and drown the signal.
#
# ---------------------------------------------------------------------------
# WHAT THIS GUARD DOES NOT SEE. Read this before trusting a green run.
# ---------------------------------------------------------------------------
#
# 1. A DISJUNCTIVE ESCAPE INSIDE AN OTHERWISE TENANT-BOUND PREDICATE.
#    Classification is textual, not a parse of the boolean structure. A policy
#    shaped
#
#        USING (tenant_id = current_setting('app.tenant_id')::uuid
#               OR current_setting('app.agent', true) = 'on')
#
#    grants cross-tenant access through its second disjunct, yet it does bind
#    tenant_id, so it classifies TENANT and is not audited. That is a FALSE
#    NEGATIVE, the costly direction, in a guard whose whole purpose is
#    eliminating them.
#
#    What IS caught, since the classifier now tests for a binding rather than a
#    mention: a predicate that reads app.tenant_id and constrains nothing with
#    it, such as USING (current_setting('app.tenant_id', true) <> ''). That is
#    classified CROSS, audited as the grant it is, and called out by the
#    UNBOUND TENANT SETTING section below.
#
#    No policy in this database has the disjunctive shape today -- every
#    cross-tenant grant is written as its own separate policy, which is the
#    convention worth keeping, and the reason a boolean parse has not been
#    worth building. Run the counts in that section against a live database
#    rather than trusting this paragraph.
#
# 2. IT IS BLIND TO A MISSING OR WEAKENED RESTRICTIVE POLICY, BY CONSTRUCTION.
#    Excluding restrictive policies means a dropped or narrowed
#    <table>_site_scope gate is invisible here. That is a real and separate
#    invariant of the tenant boundary -- m112 exists because four tables
#    shipped without it -- and it needs its own guard. Do not read a green run
#    from this one as "the site-scope gates are intact"; it has not looked.
#
# 3. IT AUDITS THE POLICY SET, NOT THE CALLERS. Whether a given code path
#    actually runs under InAgentTx is recorded by a human in the ledger's `ops`
#    column. The CODE PATHS section challenges the obvious contradictions, but
#    it is a heuristic and deliberately only warns.
#
# ---------------------------------------------------------------------------
# THE LEDGER: an access mode has to be a DECISION, not a default
# ---------------------------------------------------------------------------
#
# apps/api/db/rls-cross-tenant-policies.txt carries one line per cross-tenant
# policy:
#
#   table|policy|guc|cmd|ops|rationale
#
# `cmd` is the access mode the policy is MEANT to have. `ops` is the set of
# operations the code path behind it actually performs (select insert update
# delete lock). The guard reconciles the database against that file, so:
#
#   * a NEW cross-tenant policy that nobody recorded  -> error (the omission
#     this guard exists to catch),
#   * a policy whose mode drifted from what was recorded -> error,
#   * a policy whose `cmd` does not COVER its `ops` -> error. That is the
#     #96 / m89 / #463 bug, stated in the file rather than waiting to be
#     noticed in production,
#   * `ops` left unaudited ('-') on any mode narrower than ALL -> error. A
#     policy narrower than ALL has to carry the evidence that the narrowing is
#     safe; on FOR ALL every verb is admitted and the silence cannot arise,
#     so '-' is legal there and only there,
#   * a ledger line for a policy that no longer exists -> error (stale).
#
# The point of the file is that `ops` is a human judgement that had to be made
# and written down. FOR ALL is no longer inferred from a default nobody typed;
# it is asserted, and the assertion is checked against a live database.
#
# ---------------------------------------------------------------------------
# A SWEEP THAT FINDS NOTHING MUST GO RED
# ---------------------------------------------------------------------------
#
# This is the whole reason the guard is shaped the way it is. If the
# extraction returns no rows -- wrong database, migrations never applied,
# pg_policies renamed, a mangled query, a psql that printed an error onto
# stdout -- then "no cross-tenant policy is wrong" is not a clean bill of
# health, it is a broken guard reporting success over its own failure. That is
# this project's signature defect and it is the same defect one level up.
#
# So an empty extraction exits 2 with a distinct message, never 0. Exit 2 also
# covers "I could not reach a database at all": a gate that cannot find its
# input fails loudly and is never skipped.
#
#   exit 0  every cross-tenant policy is accounted for and its mode matches
#   exit 1  a real finding: unrecorded, drifted, stale, or FOR SELECT on a
#           path that writes
#   exit 2  the guard could not do its job (no database, empty extraction,
#           unreadable ledger). NEVER a pass.
#
# ---------------------------------------------------------------------------
# RUN IT
# ---------------------------------------------------------------------------
#
#   scripts/check-rls-cross-tenant_test.sh    # the regression suite; no DB needed
#   scripts/check-rls-cross-tenant.sh         # extract from a throwaway DB, analyse
#   scripts/check-rls-cross-tenant.sh --extract > /tmp/policies.txt
#   scripts/check-rls-cross-tenant.sh --from-extract /tmp/policies.txt
#
# WHERE THIS RUNS, and the boundary between the two halves.
#
#   make check-rls-cross-tenant-test  -> the self-test. Hermetic: it builds its
#     own synthetic extraction and ledger, needs no database, and proves the
#     guard's LOGIC. ci.yml runs it on every PR, as its own step inside the
#     Security audit job, and ahead of the live reconciliation wherever both
#     run -- so a broken guard fails the build rather than passing by failing
#     open. Same pattern and same reason as
#     scripts/check-version-surfaces_test.sh, which sits a few steps above it.
#
#   make check-rls-cross-tenant       -> the live reconciliation. Applies every
#     migration to a throwaway Postgres and reads real pg_policies, so it needs
#     Docker. That runs in .github/workflows/api-integration.yml, next to the
#     other RLS proofs that already need a database.
#
# WHAT THAT MEANS IN PRACTICE: the self-test gates every PR; the LIVE check
# does NOT, because api-integration.yml is manual-dispatch only -- the same
# standing limitation the m112 site-scope proofs already carry. So CI proving
# the guard's logic is sound is not CI proving THIS repository's policies
# reconcile. Run `make check-rls-cross-tenant` locally, or dispatch that
# workflow, before merging anything that adds or narrows an RLS policy.
#
# WHERE THE DATABASE COMES FROM, in order:
#   1. $WPMGR_RLS_DATABASE_URL, if set -- an already-migrated database.
#   2. otherwise a throwaway postgres container, with apps/api/migrations/*.sql
#      applied in lexical order (the same order internal/db/migrate.go uses).
#   3. otherwise exit 2. It is never skipped.
#
# Extraction needs a database; analysis does not. --from-extract analyses a
# capture, which is how the self-test runs hermetically and how CI can check
# the guard's logic without a postgres service.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop with BSD grep/sed/awk and on an ubuntu runner with
# the GNU ones. No mapfile, no associative arrays, no sed -i, no grep -P.

set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/check-rls-cross-tenant.sh [OPTIONS]

  (no options)        Extract from a database and analyse.
  --extract           Print the raw policy extraction to stdout and exit.
  --from-extract FILE Analyse a previously captured extraction.
  --ledger FILE       Override the ledger path.
  --root DIR          Repository root (default: the repo this script is in).
  -h, --help          This text.

Environment:
  WPMGR_RLS_DATABASE_URL   A migrated database to read pg_policies from.
                           When unset, a throwaway container is used.

Exit 0: every cross-tenant policy is accounted for.
Exit 1: an unrecorded, drifted or stale policy, or FOR SELECT on a write path.
Exit 2: the guard could not run (no database, empty extraction, no ledger).
        This is never a pass.
USAGE
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
MODE='full'
EXTRACT_FILE=''
LEDGER=''

while [ $# -gt 0 ]; do
  case "$1" in
    -h | --help) usage; exit 0 ;;
    --extract) MODE='extract'; shift ;;
    --from-extract)
      MODE='analyse'
      EXTRACT_FILE="${2:-}"
      [ -n "$EXTRACT_FILE" ] || { echo "ERROR: --from-extract needs a file" >&2; exit 2; }
      shift 2
      ;;
    --ledger)
      LEDGER="${2:-}"
      [ -n "$LEDGER" ] || { echo "ERROR: --ledger needs a file" >&2; exit 2; }
      shift 2
      ;;
    --root)
      ROOT="${2:-}"
      [ -n "$ROOT" ] || { echo "ERROR: --root needs a directory" >&2; exit 2; }
      shift 2
      ;;
    *) echo "ERROR: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -d "$ROOT" ] || { echo "ERROR: $ROOT is not a directory." >&2; exit 2; }
[ -n "$LEDGER" ] || LEDGER="$ROOT/apps/api/db/rls-cross-tenant-policies.txt"

MIGRATIONS_DIR="$ROOT/apps/api/migrations"
API_DIR="$ROOT/apps/api"

# How many lines below an InAgentTx( call a write still counts as "plausibly
# inside that closure" for the CODE PATHS warning. Tuned against the real
# closures in this repo: org/delete_handler.go opens at :190 and deletes at
# :245 (55 lines), which is the widest genuine one. Wider than that and every
# large repo file pairs with itself; the section stops being read at all.
PROXIMITY=80

fail=0
errors=0

# count_lines FILE -- always prints a number, always exits 0.
#
# NOT `grep -c . f || echo 0`. `grep -c` prints "0" AND exits 1 when it matches
# nothing, so the `|| echo 0` fires as well and the substitution becomes the
# two-line string "0\n0", which then blows up every arithmetic test it feeds.
# The failure lands precisely on the empty-input path -- the one case this
# guard must get right -- so it is written the boring way instead.
count_lines() {
  [ -f "$1" ] || { printf '0'; return 0; }
  awk 'END { print NR + 0 }' "$1"
}

err() { printf 'ERROR: %s\n' "$1"; fail=1; errors=$((errors + 1)); }
detail() { printf '  %s\n' "$1"; }
warn() { printf 'WARN: %s\n' "$1"; }
ok() { printf 'OK: %s\n' "$1"; }

# broken() is the exit-2 path: the guard could not do its job. It is kept
# separate from err() so that "I found nothing because I am broken" can never
# be confused with "I found nothing because nothing is wrong".
broken() {
  printf 'GUARD BROKEN: %s\n' "$1" >&2
  printf 'GUARD BROKEN: refusing to report a clean bill of health from an extraction that produced nothing.\n' >&2
  exit 2
}

# ---------------------------------------------------------------------------
# Extraction
# ---------------------------------------------------------------------------
#
# One line per policy in the public schema:
#
#   table|policy|permissive|cmd|gucs|scope
#
# gucs  is the sorted, comma-joined set of current_setting() names the policy's
#       USING and WITH CHECK expressions read. This is the field the audit is
#       built on. '-' when the expression reads no setting at all.
# scope is TENANT when the USING expression binds tenant_id to app.tenant_id,
#       and CROSS when it does not.

# THE PREDICATE IS qual, FALLING BACK TO with_check.
#
# A FOR INSERT policy has NO qual at all -- there is no existing row to test, so
# PostgreSQL stores the whole condition in with_check. Reading only qual meant
# every INSERT policy presented an empty predicate and was classified as a
# cross-tenant grant, including the three RUM ingest policies whose with_check
# pins tenant_id to app.tenant_id. Mislabelling a tenant-bound policy as a
# cross-tenant grant is not a harmless over-flag: it puts a row in the ledger
# asserting a grant that does not exist, and the ledger's only value is being
# true.
#
# TENANT MEANS BOUND, NOT MENTIONED.
#
# This used to ask whether the predicate mentioned app.tenant_id anywhere, which
# is a different and much weaker question. A predicate can read app.tenant_id
# and never constrain the row's tenant to it --
#
#     USING (current_setting('app.tenant_id', true) <> '')
#
# grants every row in the table to anyone who has any tenant set, while looking
# tenant-bound to a substring test. So the test is now whether the predicate
# actually BINDS the row's tenant_id column to the setting. Anything that does
# not is classified CROSS and audited as the grant it is, rather than excluded
# from the audit for mentioning the right words.
#
# [^=] between the column and the setting keeps the match from stepping over an
# intervening comparison and pairing a tenant_id on one side with an
# app.tenant_id belonging to a different conjunct.
EXTRACT_SQL="
SELECT
  p.tablename || '|' || p.policyname || '|' || p.permissive || '|' || p.cmd || '|' ||
  coalesce((SELECT string_agg(DISTINCT m[1], ',' ORDER BY m[1])
            FROM regexp_matches(coalesce(p.qual,'') || ' ' || coalesce(p.with_check,''),
                                'current_setting\(''([a-z_.]+)''', 'g') AS m), '-')
  || '|' ||
  CASE WHEN coalesce(nullif(p.qual,''), p.with_check, '')
            ~ 'tenant_id[[:space:]]*=[^=]*app\.tenant_id'
       THEN 'TENANT' ELSE 'CROSS' END
FROM pg_policies p
WHERE p.schemaname = 'public'
ORDER BY 1;
"

# Docker resources are PER RUN, never fixed.
#
# A fixed container name and a fixed host port make two overlapping runs fight:
# the second `docker run` fails on the name, and the cleanup trap of whichever
# finishes first removes the other's database out from under it. That stopped
# being hypothetical when the guard landed in two workflows -- ci.yml runs the
# self-test and api-integration.yml runs the live check -- and it is just as
# easy to hit locally by running `make check-rls-cross-tenant` in two shells.
# A fixed port also collides with anything already on it, including this
# repo's own compose stack, and that failure surfaces as "postgres never
# became ready" rather than "your port is busy".
#
# The port can be pinned with WPMGR_RLS_GUARD_PORT when a sandbox needs a known
# one; otherwise a free port is found and used.
CONTAINER="wpmgr-rls-guard-$$"
STARTED_CONTAINER=0

# free_port -- a port nothing is listening on, or empty if we cannot tell.
# Asking the kernel for an ephemeral port and then using it is a race in
# principle; in practice the window is microseconds and the alternative (a
# hard-coded port) collides deterministically rather than rarely. `docker run`
# still fails loudly if it loses that race, and broken() reports it.
free_port() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()' 2>/dev/null && return 0
  # No python3: fall back to a port derived from the PID, which at least
  # differs between concurrent runs.
  printf '%d' $(( 55440 + ($$ % 2000) ))
}

GUARD_PORT="${WPMGR_RLS_GUARD_PORT:-}"

# Remove the throwaway container unconditionally, not "if we started one".
#
# The flag cannot be trusted: extraction used to run as `do_extract | grep ...`,
# and every element of a bash pipeline runs in a SUBSHELL, so the
# STARTED_CONTAINER=1 assignment happened in a child and the parent's trap saw
# 0 and cleaned up nothing. Every full run leaked a postgres container. The
# pipeline is gone (see below) but the flag stays untrusted, because a leaked
# container is a disk-exhaustion bug that shows up as somebody else's build
# dying, days later, nowhere near here.
#
# Removing a container that does not exist is a no-op, so this is safe to call
# on every exit path including the ones that never started one.
cleanup_container() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1
  return 0
}
trap cleanup_container EXIT INT TERM

extract_from_url() {
  command -v psql >/dev/null 2>&1 || broken "psql is not installed; the extraction cannot run and must not be skipped."
  # psql's stderr is captured rather than discarded, and its exit status is
  # checked. Both matter for the operator, not for correctness: without them an
  # unreachable database, a bad password or a missing relation all arrived here
  # as an empty result and were reported as "produced 0 well-formed rows",
  # which fails closed but sends the reader looking for a policy problem that
  # does not exist. A gate that cannot reach its input should say so.
  local err_file out rc
  err_file="$(mktemp "${TMPDIR:-/tmp}/wpmgr-rls-psql.XXXXXX")" || return 1
  out=$(psql "$1" -A -t -v ON_ERROR_STOP=1 -c "$EXTRACT_SQL" 2>"$err_file")
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf 'GUARD BROKEN: could not read pg_policies from the database (psql exit %d).\n' "$rc" >&2
    sed 's/^/  /' "$err_file" >&2
    rm -f "$err_file"
    printf 'GUARD BROKEN: this is a gate that could not reach its input, not a clean result.\n' >&2
    exit 2
  fi
  rm -f "$err_file"
  printf '%s\n' "$out"
}

extract_from_container() {
  command -v docker >/dev/null 2>&1 || broken "no WPMGR_RLS_DATABASE_URL and docker is not installed; cannot reach a database."
  docker info >/dev/null 2>&1 || broken "no WPMGR_RLS_DATABASE_URL and the docker daemon is not reachable; cannot reach a database."
  command -v psql >/dev/null 2>&1 || broken "psql is not installed; the extraction cannot run and must not be skipped."
  [ -d "$MIGRATIONS_DIR" ] || broken "no migrations directory at $MIGRATIONS_DIR."

  [ -n "$GUARD_PORT" ] || GUARD_PORT=$(free_port)
  [ -n "$GUARD_PORT" ] || broken "could not allocate a host port for the throwaway postgres."

  docker rm -f "$CONTAINER" >/dev/null 2>&1
  local run_err
  run_err=$(docker run -d --name "$CONTAINER" \
    -e POSTGRES_PASSWORD=guard -e POSTGRES_DB=guard \
    -p "127.0.0.1:${GUARD_PORT}:5432" postgres:16-alpine 2>&1 >/dev/null)
  if [ $? -ne 0 ]; then
    printf 'GUARD BROKEN: could not start the throwaway postgres container on port %s:\n  %s\n' \
      "$GUARD_PORT" "$run_err" >&2
    printf 'GUARD BROKEN: set WPMGR_RLS_GUARD_PORT to a free port, or point WPMGR_RLS_DATABASE_URL at a migrated database.\n' >&2
    exit 2
  fi
  STARTED_CONTAINER=1
  printf 'started throwaway postgres %s on 127.0.0.1:%s\n' "$CONTAINER" "$GUARD_PORT" >&2

  local ready=0 i
  for i in $(seq 1 60); do
    if docker exec "$CONTAINER" pg_isready -U postgres -q 2>/dev/null; then ready=1; break; fi
    sleep 1
  done
  [ "$ready" = "1" ] || broken "the throwaway postgres never became ready."

  local url="postgres://postgres:guard@127.0.0.1:${GUARD_PORT}/guard?sslmode=disable"
  local f applied=0 out
  # Lexical order, exactly what internal/db/migrate.go's sort.Strings does.
  for f in $(ls "$MIGRATIONS_DIR"/*.sql 2>/dev/null | sort); do
    out=$(psql "$url" -v ON_ERROR_STOP=1 -q -f "$f" 2>&1)
    if [ $? -ne 0 ]; then
      printf 'GUARD BROKEN: migration %s failed to apply:\n%s\n' "$f" "$out" >&2
      exit 2
    fi
    applied=$((applied + 1))
  done
  [ "$applied" -gt 0 ] || broken "no migration files matched $MIGRATIONS_DIR/*.sql."
  printf 'applied %d migration files to the throwaway database\n' "$applied" >&2

  extract_from_url "$url"
}

do_extract() {
  if [ -n "${WPMGR_RLS_DATABASE_URL:-}" ]; then
    extract_from_url "$WPMGR_RLS_DATABASE_URL"
  else
    extract_from_container
  fi
}

if [ "$MODE" = 'extract' ]; then
  raw="$(do_extract)"
  # Even in --extract mode an empty result is a broken run, not an empty file
  # for somebody downstream to mistake for "no policies".
  printf '%s\n' "$raw" | grep -q '.' || broken "the extraction returned no rows."
  printf '%s\n' "$raw"
  exit 0
fi

# ---------------------------------------------------------------------------
# Analysis
# ---------------------------------------------------------------------------

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-rls-guard.XXXXXX")" || exit 2
cleanup_all() { cleanup_container; rm -rf "$WORK"; }
trap cleanup_all EXIT INT TERM

ALL="$WORK/all.txt"

RAW="$WORK/raw.txt"
WELLFORMED='^[a-z_0-9]+\|[^|]+\|(PERMISSIVE|RESTRICTIVE)\|[A-Z]+\|[^|]*\|(TENANT|CROSS)$'

if [ "$MODE" = 'analyse' ]; then
  [ -f "$EXTRACT_FILE" ] || broken "no extraction file at $EXTRACT_FILE."
  cp "$EXTRACT_FILE" "$RAW"
else
  # Deliberately NOT `do_extract | grep ... > "$ALL"`. A pipeline puts
  # do_extract in a subshell, which is how this leaked a postgres container on
  # every run; it would also swallow the exit status of the extraction itself.
  do_extract > "$RAW"
fi

# Keep only well-formed rows; a psql error banner or a stray blank line must
# not be counted as a policy.
grep -E "$WELLFORMED" "$RAW" > "$ALL"

TOTAL=$(count_lines "$ALL")

# EVERY dropped row is accounted for. This guard used to filter silently and
# fail only when EVERY row was malformed, so appending two junk lines to a real
# 240-row extraction printed "Extracted 239 policies" and passed -- one policy
# vanished from a tenant-boundary audit and nothing said so.
#
# That is this project's signature defect ("announcing success over its own
# errors") occurring one level up, inside the guard written to stop it. A
# policy this guard cannot parse is a policy it is not auditing, and an audit
# that quietly skips rows is worth less than no audit, because it is believed.
#
# So: any discrepancy between the non-blank input and what parsed is fatal.
RAW_ROWS=$(grep -c '[^[:space:]]' "$RAW" 2>/dev/null)
[ -n "$RAW_ROWS" ] || RAW_ROWS=0
if [ "$RAW_ROWS" -ne "$TOTAL" ]; then
  printf 'GUARD BROKEN: %d non-blank line(s) in the extraction, but only %d parsed as policies.\n' \
    "$RAW_ROWS" "$TOTAL" >&2
  printf 'The %d line(s) below were not audited. A policy this guard cannot parse is a\n' \
    "$((RAW_ROWS - TOTAL))" >&2
  printf 'policy it is not checking, and silently skipping it is the defect this guard exists to catch.\n' >&2
  grep -v -E "$WELLFORMED" "$RAW" | grep '[^[:space:]]' | sed 's/^/  /' >&2
  exit 2
fi

if [ "$TOTAL" -eq 0 ]; then
  broken "the policy extraction produced 0 well-formed rows."
fi

# The cross-tenant grants: PERMISSIVE (a RESTRICTIVE policy can only narrow,
# never grant) and not bound to app.tenant_id.
XT="$WORK/crosstenant.txt"
awk -F'|' '$3 == "PERMISSIVE" && $6 == "CROSS"' "$ALL" > "$XT"
XT_COUNT=$(count_lines "$XT")

if [ "$XT_COUNT" -eq 0 ]; then
  broken "extracted $TOTAL policies but classified 0 of them as cross-tenant grants. This repository has cross-tenant policies, so a zero here means the classifier stopped working, not that the boundary got safer."
fi

printf 'Extracted %d policies; %d are cross-tenant grants.\n' "$TOTAL" "$XT_COUNT"
printf '  (query: pg_policies, classified on the GUC each expression tests)\n'
echo

# ---------------------------------------------------------------------------
# UNBOUND TENANT SETTING -- reads app.tenant_id, constrains nothing with it
#
# This fires on the actual danger, NOT on the mere presence of a second
# setting. An earlier version errored on any permissive policy whose predicate
# read app.tenant_id alongside another app.* GUC, which reddens a perfectly
# ordinary shape: a policy that binds tenant_id correctly AND reads a second
# setting (a site scope, a role flag) is honest and narrower, not dangerous.
# The over-fire test below pins that.
#
# The dangerous shape is a predicate that READS app.tenant_id without BINDING
# the row's tenant_id to it:
#
#     USING (current_setting('app.tenant_id', true) <> '')
#
# which hands every row in the table to anyone with any tenant set, while
# reading as tenant-scoped to a human skimming for the setting's name.
#
# Such a policy is already classified CROSS by the extraction above and so is
# audited as the grant it is -- it does not escape. This section exists to say
# so out loud, because "cross-tenant grant that mentions app.tenant_id" is far
# more likely to be a broken tenant policy than an intended grant, and the
# ledger row someone would otherwise add to silence the audit would enshrine
# the bug as a decision.
# ---------------------------------------------------------------------------
MIXED="$WORK/mixed.txt"
awk -F'|' '$3 == "PERMISSIVE" && $6 == "CROSS" && $5 ~ /app\.tenant_id/' "$ALL" > "$MIXED"
MIXED_COUNT=$(count_lines "$MIXED")
if [ "$MIXED_COUNT" -gt 0 ]; then
  echo '--- UNBOUND TENANT SETTING: reads app.tenant_id but binds nothing to it ---'
  while IFS='|' read -r tbl pol perm cmd gucs scope; do
    [ -n "$tbl" ] || continue
    err "$tbl.$pol reads app.tenant_id (settings: $gucs) but does not bind the row's tenant_id to it."
    detail 'A predicate that reads the setting without constraining the column grants'
    detail 'every row in the table to any caller who has a tenant set, while reading as'
    detail 'tenant-scoped to anyone skimming for the name.'
    detail 'It is being audited as a cross-tenant grant, which is what it is. Do NOT'
    detail 'silence that by adding a ledger row: if the policy was meant to be'
    detail 'tenant-scoped, the fix is a NEW migration binding tenant_id to the setting.'
  done < "$MIXED"
  echo
fi

# ---------------------------------------------------------------------------
# NAMING -- evidence for why this guard does not grep names
# ---------------------------------------------------------------------------
echo '--- NAMING: why a name-based audit under-reports ---'
AGENT_POLICIES="$WORK/agent.txt"
awk -F'|' '$5 ~ /app\.agent/' "$XT" > "$AGENT_POLICIES"
AGENT_COUNT=$(count_lines "$AGENT_POLICIES")
if [ "$AGENT_COUNT" -gt 0 ]; then
  CONVENTIONAL=$(awk -F'|' '$2 == $1 "_agent"' "$AGENT_POLICIES" | grep -c . )
  VARIANTS=$(awk -F'|' '{ n = $2; t = $1 "_"; if (index(n, t) == 1) n = substr(n, length(t) + 1); print n }' \
    "$AGENT_POLICIES" | sort -u | grep -c .)
  printf '%d policies test app.agent, under %d distinct name suffixes.\n' "$AGENT_COUNT" "$VARIANTS"
  printf 'A grep for the "<table>_agent" convention would find %d of %d and miss %d:\n' \
    "$CONVENTIONAL" "$AGENT_COUNT" "$((AGENT_COUNT - CONVENTIONAL))"
  awk -F'|' '$2 != $1 "_agent" { printf "  %-32s %-36s %s\n", $1, $2, $4 }' "$AGENT_POLICIES"
else
  warn 'no policy tests app.agent; if that is a surprise, the classifier is what changed.'
fi
echo

# ---------------------------------------------------------------------------
# LEDGER reconciliation
# ---------------------------------------------------------------------------
[ -f "$LEDGER" ] || broken "no ledger at $LEDGER; every cross-tenant policy has to be recorded somewhere for this to mean anything."

LEDGER_ROWS="$WORK/ledger.txt"
grep -v '^[[:space:]]*#' "$LEDGER" | grep -E '^[a-z_0-9]+\|' > "$LEDGER_ROWS"
LEDGER_COUNT=$(count_lines "$LEDGER_ROWS")
if [ "$LEDGER_COUNT" -eq 0 ]; then
  broken "the ledger at $LEDGER parsed to 0 rows."
fi

echo '--- LEDGER: is every cross-tenant policy an explicit decision? ---'
printf 'ledger rows: %d\n' "$LEDGER_COUNT"

# A: every cross-tenant grant in the database is recorded, with the right mode.
while IFS='|' read -r tbl pol perm cmd gucs scope; do
  [ -n "$tbl" ] || continue
  lrow=$(awk -F'|' -v t="$tbl" -v p="$pol" '$1 == t && $2 == p { print; exit }' "$LEDGER_ROWS")
  if [ -z "$lrow" ]; then
    err "$tbl.$pol grants cross-tenant access (tests: $gucs) and is in no ledger row."
    detail "It is FOR $cmd in the database. Decide whether that is right, then record it in:"
    detail "  ${LEDGER#"$ROOT"/}"
    detail "  format: table|policy|guc|expected_cmd|writes|rationale"
    continue
  fi
  l_guc=$(printf '%s' "$lrow" | cut -d'|' -f3)
  l_cmd=$(printf '%s' "$lrow" | cut -d'|' -f4)
  l_ops=$(printf '%s' "$lrow" | cut -d'|' -f5)

  if [ "$l_cmd" != "$cmd" ]; then
    err "$tbl.$pol is FOR $cmd in the database but the ledger records FOR $l_cmd."
    detail 'Either the policy changed without the decision being revisited, or the'
    detail 'ledger was edited without the migration. Both are the thing this catches.'
  fi

  # B: an unaudited operation set is only ever legal on FOR ALL.
  if [ "$l_ops" = '-' ] && [ "$cmd" != 'ALL' ]; then
    err "$tbl.$pol is FOR $cmd but its ledger row leaves ops unaudited ('-')."
    detail 'A mode narrower than ALL has to carry the evidence that the narrowing is'
    detail 'safe. Record which of select/insert/update/delete/lock the path performs.'
    continue
  fi

  if [ "$l_guc" != "$gucs" ]; then
    warn "$tbl.$pol tests [$gucs] but the ledger records [$l_guc]."
    detail 'The policy expression reads a different set of settings than recorded.'
  fi
done < "$XT"

# ---------------------------------------------------------------------------
# C: the #96 / m89 / #463 bug -- PER TABLE, not per policy.
#
# PostgreSQL ORs PERMISSIVE policies together per table, PER COMMAND. A row is
# admitted for operation `op` if ANY permissive policy on that table whose mode
# covers `op` admits it. So the question is never "does THIS policy cover the
# operation" -- it is "does anything on this table cover it".
#
# Getting that wrong is not a harmless extra warning. A table carrying a
# deliberately SPLIT pair -- one FOR SELECT policy for the read and one FOR
# UPDATE policy for the write -- is CORRECT, and a per-policy check reddens it.
# autologin_tokens is exactly that shape today (autologin_tokens_agent for the
# read, autologin_tokens_agent_consume for the consuming UPDATE). Worse, the
# remedy such an error suggests is "widen this policy", which is a migration
# that widens a grant across the tenant boundary to silence a false positive.
#
# Splitting a policy is also precisely the remedy a future author reaches for
# when fixing a REAL #96 finding, so the guard has to understand the shape it
# is telling people to produce.
#
# The check therefore compares, for each table:
#     ops_needed  = union of the ledger `ops` across that table's cross-tenant
#                   policies -- every operation some cross-tenant path performs
#     cmds_have   = the set of modes those policies actually carry
# and requires every op in ops_needed to be covered by at least one mode.
#
# #96 DETECTION SURVIVES because in the m84 case no sibling covered the write:
# backup_schedules carried one cross-tenant policy, FOR SELECT, while the
# scheduler claimed rows with FOR UPDATE and advanced them. cmds_have={SELECT}
# does not cover update or lock, so it still reddens. Proven by test.
# ---------------------------------------------------------------------------

# cmd_covers MODE OP -- does a policy of this mode admit this operation?
cmd_covers() {
  case "$1" in
    ALL) return 0 ;;
    SELECT) [ "$2" = 'select' ] && return 0 ;;
    INSERT) [ "$2" = 'insert' ] && return 0 ;;
    # PostgreSQL applies the UPDATE policy to SELECT ... FOR UPDATE/FOR SHARE,
    # so a FOR UPDATE policy is what makes a locking read return rows at all.
    # That is the exact mechanism of Issue #96.
    UPDATE) { [ "$2" = 'update' ] || [ "$2" = 'lock' ]; } && return 0 ;;
    DELETE) [ "$2" = 'delete' ] && return 0 ;;
  esac
  return 1
}

for tbl in $(awk -F'|' '{ print $1 }' "$XT" | sort -u); do
  # Modes this table's cross-tenant policies actually carry, from the database.
  cmds_have=$(awk -F'|' -v t="$tbl" '$1 == t { print $4 }' "$XT" | sort -u)
  [ -n "$cmds_have" ] || continue

  # Every operation any cross-tenant path performs on this table, from the
  # ledger. '-' contributes nothing: it is only legal on FOR ALL, which the
  # per-policy check above already enforces.
  ops_needed=$(awk -F'|' -v t="$tbl" '$1 == t && $5 != "-" { gsub(/,/, "\n", $5); print $5 }' \
    "$LEDGER_ROWS" | sed '/^$/d' | sort -u)
  [ -n "$ops_needed" ] || continue

  for op in $ops_needed; do
    covered=0
    for c in $cmds_have; do
      if cmd_covers "$c" "$op"; then covered=1; break; fi
    done
    [ "$covered" -eq 1 ] && continue

    pol_list=$(awk -F'|' -v t="$tbl" '$1 == t { printf "%s (FOR %s) ", $2, $4 }' "$XT")
    err "$tbl: a cross-tenant path performs '$op', and no cross-tenant policy on the table covers it."
    detail "policies present: $pol_list"
    detail "modes present:    $(printf '%s' "$cmds_have" | tr '\n' ' ')"
    detail 'Under FORCE ROW LEVEL SECURITY the operation matches zero rows WITH NO'
    detail 'ERROR -- not a failure, a silence. PostgreSQL applies the UPDATE policy to'
    detail 'SELECT ... FOR UPDATE too, so even a locking read comes back empty.'
    detail 'This is the m84/#96, m89/#131 and GH #463 bug.'
    detail "Fix, in a NEW migration (never edit an applied one): either widen one of"
    detail "the policies above to cover '$op', or add a sibling policy FOR that command."
    detail 'A sibling is the tighter choice -- it grants exactly the one extra verb.'
  done
done

# D: stale ledger rows -- recorded, but no such cross-tenant policy any more.
while IFS='|' read -r tbl pol guc cmd wr rationale; do
  [ -n "$tbl" ] || continue
  if ! awk -F'|' -v t="$tbl" -v p="$pol" '$1 == t && $2 == p { found = 1 } END { exit !found }' "$XT"; then
    if awk -F'|' -v t="$tbl" -v p="$pol" '$1 == t && $2 == p { found = 1 } END { exit !found }' "$ALL"; then
      err "$tbl.$pol is in the ledger but is no longer a cross-tenant grant."
      detail 'It still exists, so it was narrowed or made RESTRICTIVE. Drop the ledger row.'
    else
      err "$tbl.$pol is in the ledger but no such policy exists in the database."
      detail 'A dropped policy leaves a stale decision behind. Drop the ledger row.'
    fi
  fi
done < "$LEDGER_ROWS"

echo

# ---------------------------------------------------------------------------
# CODE PATHS -- the mode next to what actually touches the table
# ---------------------------------------------------------------------------
#
# This section is deliberately a WARNING, never an error. It works by
# file-level co-occurrence (a file that writes to table T and also mentions
# InAgentTx), which is a heuristic: the write may sit in a function that never
# runs under InAgentTx. Making that an error would redden correct work, and a
# guard that reddens correct work gets switched off, and then it guards
# nothing. So it prompts a human to re-decide; the ledger holds the decision.
echo '--- CODE PATHS: read-only cross-tenant policies whose table is written near InAgentTx ---'
CHALLENGED=0
if [ -d "$API_DIR" ]; then
  while IFS='|' read -r tbl pol perm cmd gucs scope; do
    [ -n "$tbl" ] || continue
    # Only SELECT-only policies can exhibit the silence; ALL/INSERT/UPDATE/DELETE
    # either admit the write or are checked by the ops coverage above.
    [ "$cmd" = 'SELECT' ] || continue
    lrow=$(awk -F'|' -v t="$tbl" -v p="$pol" '$1 == t && $2 == p { print; exit }' "$LEDGER_ROWS")
    [ -n "$lrow" ] || continue
    # Only challenge rows that claim the path never writes.
    case "$(printf '%s' "$lrow" | cut -d'|' -f5)" in
      *insert* | *update* | *delete* | *lock* | '-') continue ;;
    esac

    # PERMISSIVE policies are OR'd together, so if ANY cross-tenant policy on
    # this table is FOR ALL, a cross-tenant write is admitted by that one and
    # this SELECT-only policy is not what stands in its way. Warning here would
    # be simply wrong, not merely noisy: sites has sites_agent (FOR ALL)
    # alongside sites_client_read, and site_perf_config has
    # site_perf_config_agent alongside site_perf_config_rum_lookup.
    if awk -F'|' -v t="$tbl" '$1 == t && $4 == "ALL" { found = 1 } END { exit !found }' "$XT"; then
      continue
    fi

    # Only files that can CONTAIN a closure. db/query/*.sql is SQL text and
    # internal/db/sqlc/** is generated: an "InAgentTx" in either is a comment,
    # never a transaction, so pairing a write there with it is pure noise.
    hits=$(grep -rlEi "(INSERT INTO|UPDATE|DELETE FROM)[[:space:]]+\"?(public\.)?${tbl}\"?([[:space:]]|\"|$)" \
      --include='*.go' "$API_DIR" 2>/dev/null |
      grep -v '/internal/db/sqlc/' | grep -v '_test\.go$' | sort -u)
    for f in $hits; do
      grep -q 'InAgentTx' "$f" 2>/dev/null || continue
      # Proximity, not co-occurrence. A 3000-line repo file has many closures
      # and many writes and they always co-occur; what matters is whether a
      # write sits INSIDE one. Approximate the closure by asking whether an
      # InAgentTx call opens within PROXIMITY lines above the write.
      agent_lines=$(grep -n 'InAgentTx(' "$f" 2>/dev/null | cut -d: -f1)
      [ -n "$agent_lines" ] || continue
      write_lines=$(grep -nEi "(INSERT INTO|UPDATE|DELETE FROM)[[:space:]]+\"?(public\.)?${tbl}\"?([[:space:]]|\"|$)" \
        "$f" 2>/dev/null | cut -d: -f1)
      for w in $write_lines; do
        for a in $agent_lines; do
          if [ "$a" -lt "$w" ] && [ $((w - a)) -le "$PROXIMITY" ]; then
            warn "$tbl.$pol is FOR SELECT and recorded read-only, but ${f#"$ROOT"/}:$w writes to $tbl, $((w - a)) lines below an InAgentTx at :$a."
            detail 'Confirm that write does not run inside that closure -- or that it sets'
            detail "app.tenant_id first. If neither, the ledger's ops are wrong and this"
            detail "policy's mode has to widen in a NEW migration."
            CHALLENGED=$((CHALLENGED + 1))
            break 2
          fi
        done
      done
    done
  done < "$XT"
fi
if [ "$CHALLENGED" -eq 0 ]; then
  ok 'no read-only cross-tenant policy has a write to its table in a file that also uses InAgentTx.'
else
  printf '  (%d challenge(s). These are WARNINGS: file-level co-occurrence is a heuristic,\n' "$CHALLENGED"
  printf '   and a guard that reddens correct work gets switched off. The ledger holds the decision.)\n'
fi
echo

# ---------------------------------------------------------------------------
# Verdict
# ---------------------------------------------------------------------------
printf '%d cross-tenant policies checked against %d ledger rows.\n' "$XT_COUNT" "$LEDGER_COUNT"
if [ "$fail" -eq 0 ]; then
  ok "every cross-tenant policy is recorded with a matching access mode ($errors errors)."
  exit 0
fi
printf 'FAILED with %d error(s).\n' "$errors"
exit 1
