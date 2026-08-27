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
# NOT YET WIRED INTO make OR ci.yml. Both the Makefile and .github/workflows
# are devops-engineer's to edit, and this guard was written by
# database-engineer, so the wiring is handed over rather than done here. Until
# it lands, this is a local gate you have to remember to run -- run it before
# merging anything that adds or narrows an RLS policy. When it is wired, the
# self-test must run FIRST and as its own step, so a broken guard fails the
# build instead of passing by failing open (the pattern ci.yml already uses for
# scripts/check-version-surfaces_test.sh).
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

EXTRACT_SQL="
SELECT
  p.tablename || '|' || p.policyname || '|' || p.permissive || '|' || p.cmd || '|' ||
  coalesce((SELECT string_agg(DISTINCT m[1], ',' ORDER BY m[1])
            FROM regexp_matches(coalesce(p.qual,'') || ' ' || coalesce(p.with_check,''),
                                'current_setting\(''([a-z_.]+)''', 'g') AS m), '-')
  || '|' ||
  CASE WHEN coalesce(p.qual,'') ~ 'app\.tenant_id' THEN 'TENANT' ELSE 'CROSS' END
FROM pg_policies p
WHERE p.schemaname = 'public'
ORDER BY 1;
"

CONTAINER='wpmgr-rls-guard'
STARTED_CONTAINER=0

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
  psql "$1" -A -t -v ON_ERROR_STOP=1 -c "$EXTRACT_SQL" 2>/dev/null
}

extract_from_container() {
  command -v docker >/dev/null 2>&1 || broken "no WPMGR_RLS_DATABASE_URL and docker is not installed; cannot reach a database."
  docker info >/dev/null 2>&1 || broken "no WPMGR_RLS_DATABASE_URL and the docker daemon is not reachable; cannot reach a database."
  command -v psql >/dev/null 2>&1 || broken "psql is not installed; the extraction cannot run and must not be skipped."
  [ -d "$MIGRATIONS_DIR" ] || broken "no migrations directory at $MIGRATIONS_DIR."

  docker rm -f "$CONTAINER" >/dev/null 2>&1
  docker run -d --name "$CONTAINER" \
    -e POSTGRES_PASSWORD=guard -e POSTGRES_DB=guard \
    -p 55440:5432 postgres:16-alpine >/dev/null 2>&1 ||
    broken "could not start the throwaway postgres container."
  STARTED_CONTAINER=1

  local ready=0 i
  for i in $(seq 1 60); do
    if docker exec "$CONTAINER" pg_isready -U postgres -q 2>/dev/null; then ready=1; break; fi
    sleep 1
  done
  [ "$ready" = "1" ] || broken "the throwaway postgres never became ready."

  local url='postgres://postgres:guard@127.0.0.1:55440/guard?sslmode=disable'
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

if [ "$MODE" = 'analyse' ]; then
  [ -f "$EXTRACT_FILE" ] || broken "no extraction file at $EXTRACT_FILE."
  # Keep only well-formed rows; a psql error banner or a stray blank line must
  # not be counted as a policy.
  grep -E '^[a-z_0-9]+\|[^|]+\|(PERMISSIVE|RESTRICTIVE)\|[A-Z]+\|[^|]*\|(TENANT|CROSS)$' \
    "$EXTRACT_FILE" > "$ALL"
else
  # Deliberately NOT `do_extract | grep ... > "$ALL"`. A pipeline puts
  # do_extract in a subshell, which is how this leaked a postgres container on
  # every run; it would also swallow the exit status of the extraction itself.
  do_extract > "$WORK/raw.txt"
  grep -E '^[a-z_0-9]+\|[^|]+\|(PERMISSIVE|RESTRICTIVE)\|[A-Z]+\|[^|]*\|(TENANT|CROSS)$' \
    "$WORK/raw.txt" > "$ALL"
fi

TOTAL=$(count_lines "$ALL")
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

  # C: the #96 / m89 / #463 bug -- the mode does not cover what the path does.
  if [ "$l_ops" != '-' ]; then
    for op in $(printf '%s' "$l_ops" | tr ',' ' '); do
      covered=0
      case "$cmd" in
        ALL) covered=1 ;;
        SELECT) [ "$op" = 'select' ] && covered=1 ;;
        INSERT) [ "$op" = 'insert' ] && covered=1 ;;
        # PostgreSQL applies the UPDATE policy to SELECT ... FOR UPDATE, so a
        # FOR UPDATE policy is what makes a locking read return rows at all.
        UPDATE) { [ "$op" = 'update' ] || [ "$op" = 'lock' ]; } && covered=1 ;;
        DELETE) [ "$op" = 'delete' ] && covered=1 ;;
      esac
      if [ "$covered" -eq 0 ]; then
        err "$tbl.$pol is FOR $cmd, which does not cover '$op' -- and its code path performs it (ledger ops: $l_ops)."
        detail 'Under FORCE ROW LEVEL SECURITY this admits what the mode covers and admits'
        detail 'nothing else, so that operation matches zero rows WITH NO ERROR. PostgreSQL'
        detail 'applies the UPDATE policy to SELECT ... FOR UPDATE too, so even a locking'
        detail 'read comes back empty. This is the m84/#96, m89/#131 and GH #463 bug.'
        detail "Fix: a NEW migration recreating $pol with a mode that covers $l_ops,"
        detail 'with WITH CHECK mirroring USING. Never edit the applied migration.'
      fi
    done
  fi

  if [ "$l_guc" != "$gucs" ]; then
    warn "$tbl.$pol tests [$gucs] but the ledger records [$l_guc]."
    detail 'The policy expression reads a different set of settings than recorded.'
  fi
done < "$XT"

# C: stale ledger rows -- recorded, but no such cross-tenant policy any more.
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
