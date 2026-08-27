#!/usr/bin/env bash
# scripts/check-rls-cross-tenant_test.sh
#
# The regression suite for scripts/check-rls-cross-tenant.sh.
#
# WHY THIS EXISTS. The guard's whole job is to be trusted when it says nothing
# is wrong. A guard nobody can run is a guard nobody can check, and the two
# defects it exists to catch (GH #470) are both defects of SILENCE -- a policy
# that admits the read and swallows the write, and a name-based sweep that
# matches nothing and reports a clean bill of health. So the cases that matter
# most here are not the ones where the guard finds a bug. They are:
#
#   * the guard finds NOTHING and must still go red (exit 2, never 0), and
#   * the guard is shown CORRECT work and must stay green.
#
# The second is not optional politeness. A guard that reddens correct work gets
# switched off, and then it guards nothing at all. Half the cases below are
# honest configurations the guard MUST NOT block.
#
# HOW IT WORKS. Every case writes a small extraction capture and a small
# ledger, runs the real guard against them with --from-extract, and asserts the
# exit code plus what the output does and does not say. No database is needed:
# extraction and analysis are separate modes in the guard precisely so this
# suite can be hermetic and run anywhere, including a CI job with no postgres.
#
# RUN IT:
#   scripts/check-rls-cross-tenant_test.sh          # everything
#   scripts/check-rls-cross-tenant_test.sh empty    # only cases matching "empty"
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_RLS_GUARD_SCRIPT=/tmp/guard-with-hole.sh \
#     scripts/check-rls-cross-tenant_test.sh
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop with BSD grep/sed/awk and on an ubuntu runner with
# the GNU ones. No mapfile, no associative arrays, no sed -i, no grep -P.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_RLS_GUARD_SCRIPT:-$HERE/check-rls-cross-tenant.sh}"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  echo "no guard script at $GUARD" >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-rls-guard-test.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
FAILED_NAMES=''

# ---------------------------------------------------------------------------
# Fixtures
#
# A realistic-in-miniature extraction. It carries, deliberately:
#   * a tenant-bound policy      (must never be counted as a cross-tenant grant)
#   * a RESTRICTIVE site_scope   (does not mention app.tenant_id, but can only
#                                 narrow, so it must never be counted either)
#   * a FOR ALL agent policy     (the correct shape for a writing path)
#   * a FOR SELECT agent policy  (correct for a genuinely read-only path)
#   * a FOR INSERT policy        (correct for an append-only path)
#   * a FOR UPDATE policy        (correct for a consuming path, and the only
#                                 mode besides ALL that covers a locking read)
#   * a policy named nothing like the convention, to keep the naming section
#     honest
# ---------------------------------------------------------------------------

write_extract() {
  cat > "$1" <<'EXTRACT'
autologin_tokens|autologin_tokens_agent_consume|PERMISSIVE|UPDATE|app.agent|CROSS
backup_chunks|backup_chunks_agent|PERMISSIVE|SELECT|app.agent|CROSS
backup_schedules|backup_schedules_scheduler|PERMISSIVE|ALL|app.agent|CROSS
rum_events_raw|rum_events_raw_rum_ingest|PERMISSIVE|INSERT|app.rum_ingest,app.site_id,app.tenant_id|CROSS
sites|sites_site_scope|RESTRICTIVE|ALL|app.allowed_site_ids,app.site_scope|CROSS
sites|sites_tenant_isolation|PERMISSIVE|ALL|app.tenant_id|TENANT
update_runs|update_runs_agent|PERMISSIVE|ALL|app.agent|CROSS
EXTRACT
}

write_ledger() {
  cat > "$1" <<'LEDGER'
# a comment, and a blank line follow

# columns: table|policy|guc|cmd|ops|rationale
autologin_tokens|autologin_tokens_agent_consume|app.agent|UPDATE|update|the consuming UPDATE runs under InAgentTx
backup_chunks|backup_chunks_agent|app.agent|SELECT|select|reads only; every chunk write is tenant-scoped
backup_schedules|backup_schedules_scheduler|app.agent|ALL|update,lock|claims due schedules with FOR UPDATE and advances them
rum_events_raw|rum_events_raw_rum_ingest|app.rum_ingest,app.site_id,app.tenant_id|INSERT|insert|the anonymous beacon write path
update_runs|update_runs_agent|app.agent|ALL|-|FOR ALL admits every verb
LEDGER
}

# ---------------------------------------------------------------------------
# Harness
# ---------------------------------------------------------------------------

run_guard() {
  # run_guard EXTRACT LEDGER -> sets OUT and RC
  OUT="$("$GUARD" --from-extract "$1" --ledger "$2" --root "$WORK/emptyroot" 2>&1)"
  RC=$?
}

pass() { PASSED=$((PASSED + 1)); printf 'ok   %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES
  - $1"
  printf 'FAIL %s\n' "$1"
  printf '     %s\n' "$2"
  printf '     --- guard output (rc=%s) ---\n' "$RC"
  printf '%s\n' "$OUT" | sed 's/^/     /'
  printf '     --- end ---\n'
}

want_rc() {
  # want_rc NAME EXPECTED
  if [ "$RC" = "$2" ]; then return 0; fi
  fail "$1" "expected exit $2, got $RC"
  return 1
}

want_says() {
  if printf '%s' "$OUT" | grep -qF "$2"; then return 0; fi
  fail "$1" "expected the output to mention: $2"
  return 1
}

want_silent_about() {
  if printf '%s' "$OUT" | grep -qF "$2"; then
    fail "$1" "expected the output NOT to mention: $2"
    return 1
  fi
  return 0
}

should_run() {
  [ -z "$FILTER" ] && return 0
  case "$1" in *"$FILTER"*) return 0 ;; esac
  return 1
}

mkdir -p "$WORK/emptyroot"

# ===========================================================================
# GROUP 1 -- the baseline. Correct work stays green.
# ===========================================================================

NAME='baseline: a correct extraction and a matching ledger pass'
if should_run "$NAME"; then
  write_extract "$WORK/e1"
  write_ledger "$WORK/l1"
  run_guard "$WORK/e1" "$WORK/l1"
  want_rc "$NAME" 0 &&
    want_says "$NAME" 'every cross-tenant policy is recorded' &&
    pass "$NAME"
fi

NAME='baseline: the tenant-bound policy is not counted as a cross-tenant grant'
if should_run "$NAME"; then
  write_extract "$WORK/e2"
  write_ledger "$WORK/l2"
  run_guard "$WORK/e2" "$WORK/l2"
  # 7 extracted, 5 cross-tenant: sites_tenant_isolation is tenant-bound and
  # sites_site_scope is RESTRICTIVE, so neither is a grant.
  want_rc "$NAME" 0 &&
    want_says "$NAME" 'Extracted 7 policies; 5 are cross-tenant grants.' &&
    pass "$NAME"
fi

NAME='baseline: a RESTRICTIVE policy that never mentions app.tenant_id is not a grant'
if should_run "$NAME"; then
  write_extract "$WORK/e3"
  write_ledger "$WORK/l3"
  run_guard "$WORK/e3" "$WORK/l3"
  # sites_site_scope is in NO ledger row. If the classifier wrongly treated a
  # restrictive policy as a grant, the guard would demand a ledger row for it.
  want_rc "$NAME" 0 &&
    want_silent_about "$NAME" 'sites_site_scope' &&
    pass "$NAME"
fi

# ===========================================================================
# GROUP 2 -- A SWEEP THAT FINDS NOTHING MUST GO RED.
#
# This is the requirement the guard exists to satisfy at one level up. Every
# case here would, in a naive implementation, print nothing, find nothing and
# exit 0 -- reporting a clean tenant boundary from a broken run.
# ===========================================================================

NAME='empty: an extraction with no rows exits 2, never 0'
if should_run "$NAME"; then
  : > "$WORK/e-empty"
  write_ledger "$WORK/l4"
  run_guard "$WORK/e-empty" "$WORK/l4"
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'GUARD BROKEN' &&
    want_says "$NAME" '0 well-formed rows' &&
    want_silent_about "$NAME" 'OK: every cross-tenant policy' &&
    pass "$NAME"
fi

NAME='empty: an extraction of only malformed lines exits 2, not 0'
if should_run "$NAME"; then
  printf 'psql: error: connection to server failed\nFATAL: database does not exist\n' > "$WORK/e-junk"
  write_ledger "$WORK/l5"
  run_guard "$WORK/e-junk" "$WORK/l5"
  # A psql error banner on stdout must never be counted as a policy row.
  want_rc "$NAME" 2 &&
    want_says "$NAME" '0 well-formed rows' &&
    pass "$NAME"
fi

NAME='empty: rows extracted but none classified cross-tenant exits 2'
if should_run "$NAME"; then
  cat > "$WORK/e-notenant" <<'EOF'
sites|sites_tenant_isolation|PERMISSIVE|ALL|app.tenant_id|TENANT
update_runs|update_runs_tenant_isolation|PERMISSIVE|ALL|app.tenant_id|TENANT
EOF
  write_ledger "$WORK/l6"
  run_guard "$WORK/e-notenant" "$WORK/l6"
  # This is the subtle one. The extraction worked, so a naive guard would say
  # "no cross-tenant policy is wrong" and exit 0. But this repository HAS
  # cross-tenant policies, so zero means the classifier broke.
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'classified 0 of them as cross-tenant grants' &&
    want_silent_about "$NAME" 'OK: every cross-tenant policy' &&
    pass "$NAME"
fi

NAME='empty: a missing extraction file exits 2'
if should_run "$NAME"; then
  write_ledger "$WORK/l7"
  run_guard "$WORK/nope-does-not-exist" "$WORK/l7"
  want_rc "$NAME" 2 && want_says "$NAME" 'GUARD BROKEN' && pass "$NAME"
fi

NAME='empty: a missing ledger exits 2 rather than passing with nothing to check'
if should_run "$NAME"; then
  write_extract "$WORK/e8"
  run_guard "$WORK/e8" "$WORK/nope-no-ledger"
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'no ledger at' &&
    want_silent_about "$NAME" 'OK: every cross-tenant policy' &&
    pass "$NAME"
fi

NAME='empty: a ledger that parses to zero rows exits 2'
if should_run "$NAME"; then
  write_extract "$WORK/e9"
  printf '# every line here is a comment\n# so the ledger parses to nothing\n' > "$WORK/l-empty"
  run_guard "$WORK/e9" "$WORK/l-empty"
  want_rc "$NAME" 2 &&
    want_says "$NAME" 'parsed to 0 rows' &&
    pass "$NAME"
fi

# ===========================================================================
# GROUP 3 -- the real defects. The guard must go red.
# ===========================================================================

NAME='red: FOR SELECT on a path that updates is the #96 bug and fails'
if should_run "$NAME"; then
  write_extract "$WORK/e10"
  write_ledger "$WORK/l10"
  # backup_chunks_agent is FOR SELECT in the database. Record that its path
  # actually updates -- the exact contradiction that shipped three times.
  ed_tmp="$WORK/l10.tmp"
  sed 's/^backup_chunks|backup_chunks_agent|app.agent|SELECT|select|/backup_chunks|backup_chunks_agent|app.agent|SELECT|update|/' \
    "$WORK/l10" > "$ed_tmp" && mv "$ed_tmp" "$WORK/l10"
  run_guard "$WORK/e10" "$WORK/l10"
  want_rc "$NAME" 1 &&
    want_says "$NAME" "is FOR SELECT, which does not cover 'update'" &&
    want_says "$NAME" 'zero rows WITH NO ERROR' &&
    want_says "$NAME" 'Never edit the applied migration.' &&
    pass "$NAME"
fi

NAME='red: FOR SELECT on a path that takes a row lock fails'
if should_run "$NAME"; then
  write_extract "$WORK/e11"
  write_ledger "$WORK/l11"
  ed_tmp="$WORK/l11.tmp"
  sed 's/^backup_chunks|backup_chunks_agent|app.agent|SELECT|select|/backup_chunks|backup_chunks_agent|app.agent|SELECT|lock|/' \
    "$WORK/l11" > "$ed_tmp" && mv "$ed_tmp" "$WORK/l11"
  run_guard "$WORK/e11" "$WORK/l11"
  # PostgreSQL applies the UPDATE policy to SELECT ... FOR UPDATE, so a
  # read-only policy makes even the locking read return nothing. That is
  # precisely how Issue #96 stopped every backup schedule advancing.
  want_rc "$NAME" 1 &&
    want_says "$NAME" "does not cover 'lock'" &&
    pass "$NAME"
fi

NAME='red: a new unrecorded cross-tenant policy fails'
if should_run "$NAME"; then
  write_extract "$WORK/e12"
  write_ledger "$WORK/l12"
  printf 'smtp_settings|smtp_settings_mailer|PERMISSIVE|ALL|app.agent|CROSS\n' >> "$WORK/e12"
  run_guard "$WORK/e12" "$WORK/l12"
  # The omission the guard exists to catch: somebody adds a cross-tenant
  # policy and nobody records what its access mode is meant to be.
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'is in no ledger row' &&
    want_says "$NAME" 'smtp_settings_mailer' &&
    pass "$NAME"
fi

NAME='red: a policy whose mode drifted from the ledger fails'
if should_run "$NAME"; then
  write_extract "$WORK/e13"
  write_ledger "$WORK/l13"
  ed_tmp="$WORK/e13.tmp"
  sed 's/^update_runs|update_runs_agent|PERMISSIVE|ALL|/update_runs|update_runs_agent|PERMISSIVE|SELECT|/' \
    "$WORK/e13" > "$ed_tmp" && mv "$ed_tmp" "$WORK/e13"
  run_guard "$WORK/e13" "$WORK/l13"
  # GH #463 Phase 0 in miniature: update_runs_agent narrowed to FOR SELECT
  # while the dispatcher still claims rows.
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'is FOR SELECT in the database but the ledger records FOR ALL' &&
    pass "$NAME"
fi

NAME='red: a stale ledger row for a policy that no longer exists fails'
if should_run "$NAME"; then
  write_extract "$WORK/e14"
  write_ledger "$WORK/l14"
  printf 'gone_table|gone_policy|app.agent|ALL|-|dropped in some later migration\n' >> "$WORK/l14"
  run_guard "$WORK/e14" "$WORK/l14"
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'no such policy exists in the database' &&
    pass "$NAME"
fi

NAME='red: a ledger row for a policy that stopped being cross-tenant fails'
if should_run "$NAME"; then
  write_extract "$WORK/e15"
  write_ledger "$WORK/l15"
  # The policy still exists, but is now RESTRICTIVE -- it can no longer grant.
  ed_tmp="$WORK/e15.tmp"
  sed 's/^backup_chunks|backup_chunks_agent|PERMISSIVE|/backup_chunks|backup_chunks_agent|RESTRICTIVE|/' \
    "$WORK/e15" > "$ed_tmp" && mv "$ed_tmp" "$WORK/e15"
  run_guard "$WORK/e15" "$WORK/l15"
  want_rc "$NAME" 1 &&
    want_says "$NAME" 'is no longer a cross-tenant grant' &&
    pass "$NAME"
fi

NAME='red: an unaudited ops set on a mode narrower than ALL fails'
if should_run "$NAME"; then
  write_extract "$WORK/e16"
  write_ledger "$WORK/l16"
  ed_tmp="$WORK/l16.tmp"
  sed 's/^backup_chunks|backup_chunks_agent|app.agent|SELECT|select|/backup_chunks|backup_chunks_agent|app.agent|SELECT|-|/' \
    "$WORK/l16" > "$ed_tmp" && mv "$ed_tmp" "$WORK/l16"
  run_guard "$WORK/e16" "$WORK/l16"
  # A mode narrower than ALL must carry the evidence that narrowing is safe.
  # Letting '-' through here is how a FOR SELECT policy gets waved past.
  want_rc "$NAME" 1 &&
    want_says "$NAME" "leaves ops unaudited" &&
    pass "$NAME"
fi

# ===========================================================================
# GROUP 4 -- OVER-FIRE. Correct work the guard must NOT block.
#
# A guard that reddens honest work gets switched off, and then it guards
# nothing. Each case below is a shape that is genuinely right.
# ===========================================================================

NAME='green: a genuinely read-only path correctly using FOR SELECT stays green'
if should_run "$NAME"; then
  write_extract "$WORK/e20"
  write_ledger "$WORK/l20"
  run_guard "$WORK/e20" "$WORK/l20"
  # backup_chunks_agent is FOR SELECT with ops=select. This is the honest
  # read-only case and it must not be flagged just for being FOR SELECT.
  want_rc "$NAME" 0 &&
    want_silent_about "$NAME" 'ERROR' &&
    pass "$NAME"
fi

NAME='green: an append-only path correctly using FOR INSERT stays green'
if should_run "$NAME"; then
  write_extract "$WORK/e21"
  write_ledger "$WORK/l21"
  run_guard "$WORK/e21" "$WORK/l21"
  # FOR INSERT covers insert and nothing else, and the RUM beacon path only
  # inserts. Demanding FOR ALL here would widen a public write path.
  want_rc "$NAME" 0 &&
    want_silent_about "$NAME" 'rum_events_raw_rum_ingest' &&
    pass "$NAME"
fi

NAME='green: FOR UPDATE covers a locking read, so it must not be flagged'
if should_run "$NAME"; then
  write_extract "$WORK/e22"
  write_ledger "$WORK/l22"
  ed_tmp="$WORK/l22.tmp"
  sed 's/^autologin_tokens|autologin_tokens_agent_consume|app.agent|UPDATE|update|/autologin_tokens|autologin_tokens_agent_consume|app.agent|UPDATE|update,lock|/' \
    "$WORK/l22" > "$ed_tmp" && mv "$ed_tmp" "$WORK/l22"
  run_guard "$WORK/e22" "$WORK/l22"
  # PostgreSQL applies the UPDATE policy to SELECT ... FOR UPDATE, so FOR
  # UPDATE genuinely covers a lock. Insisting on ALL here would be wrong.
  want_rc "$NAME" 0 &&
    want_silent_about "$NAME" 'ERROR' &&
    pass "$NAME"
fi

NAME='green: FOR ALL with an unaudited ops set stays green'
if should_run "$NAME"; then
  write_extract "$WORK/e23"
  write_ledger "$WORK/l23"
  run_guard "$WORK/e23" "$WORK/l23"
  # update_runs_agent is FOR ALL with ops='-'. Every verb is admitted, so the
  # silence cannot arise and the path need not be audited verb by verb.
  want_rc "$NAME" 0 &&
    want_silent_about "$NAME" 'update_runs_agent' &&
    pass "$NAME"
fi

NAME='green: a FOR ALL path recorded with several ops stays green'
if should_run "$NAME"; then
  write_extract "$WORK/e24"
  write_ledger "$WORK/l24"
  run_guard "$WORK/e24" "$WORK/l24"
  # backup_schedules_scheduler is FOR ALL with ops=update,lock -- the m84/#96
  # fix itself. ALL covers everything, so this is the correct shape.
  want_rc "$NAME" 0 &&
    want_silent_about "$NAME" 'backup_schedules_scheduler is FOR' &&
    pass "$NAME"
fi

NAME='green: an unconventionally named policy is still audited, not skipped'
if should_run "$NAME"; then
  write_extract "$WORK/e25"
  write_ledger "$WORK/l25"
  run_guard "$WORK/e25" "$WORK/l25"
  # backup_schedules_scheduler does not match the "<table>_agent" convention.
  # The naming section must SAY so -- that is the evidence for why the audit
  # is built on the setting and not the name.
  want_rc "$NAME" 0 &&
    want_says "$NAME" 'backup_schedules_scheduler' &&
    want_says "$NAME" 'would find' &&
    pass "$NAME"
fi

# ===========================================================================
# GROUP 5 -- the naming section, which is the evidence for the method.
# ===========================================================================

NAME='naming: the count of variants is computed, never hard-coded'
if should_run "$NAME"; then
  write_extract "$WORK/e30"
  write_ledger "$WORK/l30"
  # Four app.agent policies here, under three suffixes: _agent (update_runs),
  # _agent_consume, _scheduler, and backup_chunks_agent -- so two match the
  # convention and two do not.
  run_guard "$WORK/e30" "$WORK/l30"
  want_rc "$NAME" 0 &&
    want_says "$NAME" '4 policies test app.agent, under 3 distinct name suffixes.' &&
    want_says "$NAME" 'would find 2 of 4 and miss 2' &&
    pass "$NAME"
fi

# ===========================================================================
# Verdict
# ===========================================================================

echo
printf '%d passed, %d failed\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf 'failing cases:%s\n' "$FAILED_NAMES"
  exit 1
fi
if [ "$PASSED" -eq 0 ]; then
  # The suite's own version of the rule it is testing: running no cases is not
  # a pass.
  echo 'no cases ran; that is a broken suite, not a clean one.' >&2
  exit 2
fi
exit 0
