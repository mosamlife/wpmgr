#!/usr/bin/env bash
# scripts/check-assistant-ship-gate.sh
#
# ADR-061's nine-item pre-ship gate, scored against this tree.
#
# WHAT THIS IS. docs/adr/ADR-061-assistant-surface-phase-1.md carries a section
# headed "**What has to exist before v1 ships.**" with nine bullets. Until this
# script existed the gate had never been evaluated: no script, no CI job, no
# release note, no worklog, while the surface it gates has been live in
# production since 0.61.147. A gate nobody scores is not a gate, it is a
# paragraph.
#
# WHY A SCRIPT AND NOT A YAML STEP. CLAUDE.md: "Build-gating logic goes in a
# repo script with a committed test suite, never in a YAML block scalar." That
# rule had been applied to scripts/check-version-surfaces.sh and to nothing
# else. scripts/check-assistant-ship-gate_test.sh is this guard's suite; it
# builds fixture trees, plants each failure, and asserts the exit code.
#
# RUN IT (no CI, no toolchain, no network, no database):
#   make check-assistant-gate         or  scripts/check-assistant-ship-gate.sh
#   make check-assistant-gate-test    or  scripts/check-assistant-ship-gate_test.sh
#   scripts/check-assistant-ship-gate.sh /path/to/some/other/tree
#
# ------------------------------------------------------------------------------
# THREE EXIT CODES, AND THERE IS DELIBERATELY NO CODE MEANING "SCORED NOTHING,
# FINE".
#
#   0  All nine items MET.
#   1  At least one item UNMET, or at least one item UNSCOREABLE. A gate item
#      this script cannot score is red, never green and never silently skipped.
#      "I could not check it" and "it is fine" are the same output only in a
#      guard that is lying, and that failure -- announcing success over the
#      guard's own blind spot -- is this repository's signature defect.
#   2  GUARD BROKEN. The tree is not one this guard can read at all, or the ADR
#      no longer has the shape this guard was written against, or a command this
#      guard depends on is absent. Never scores anything in this state.
#
# ------------------------------------------------------------------------------
# TWO KINDS OF ITEM, AND THE OUTPUT SAYS WHICH.
#
#   [code]      Verified against the tree. The thing itself was checked: a
#               function is defined, a test exists AND asserts, a column carries
#               a default, a guard is wired into ci.yml. MET here means verified.
#
#   [artefact]  Existence and CURRENCY of a written document. A data
#               classification statement and a set of pilot kill criteria are
#               judgements, and no script can check that a judgement is good.
#               What a script CAN check is that the document exists, has the
#               sections it claims to have, and was reviewed no earlier than the
#               last change to the surface it describes. MET here means SOMEONE
#               WROTE THE DOCUMENT AND IT IS NOT STALE. It does not mean the
#               content is correct. A reader must not read a green [artefact]
#               line as verification.
#
#   [manual]    Cannot be scored from a tree at all. Item 9 is the only one:
#               "`make test-integration` run locally before merge" is an event
#               on a developer's machine that leaves no artefact in the
#               repository. This guard reports it UNSCOREABLE and exits 1. It
#               does NOT skip it, and it does NOT fudge it into a green. Closing
#               it means changing the obligation into one that leaves evidence.
#
# ------------------------------------------------------------------------------
# THE ANTI-ROT PROBE, AND WHY IT IS EXIT 2 AND NOT A WARNING.
#
# This guard hardcodes nine items derived by reading ADR-061. If the ADR's gate
# section is edited -- a tenth bullet added, the heading reworded, the section
# deleted -- then this guard is scoring a list that no longer exists, and every
# green line it prints is a claim about a document it can no longer find. So
# before scoring anything it re-reads the ADR, locates the gate section by its
# heading, and counts the top-level bullets. Anything other than nine is exit 2.
#
# The same reasoning applies to every extraction below: a control pattern that
# suddenly matches NOTHING is evidence the guard broke, not evidence the tree is
# clean. Each such probe is checked for emptiness explicitly.
#
# ------------------------------------------------------------------------------
# WHAT THE NINE ITEMS ARE, AND WHAT EACH IS SCORED AGAINST. The bullet text is
# quoted; the amendment that qualifies it is named; the check follows.
#
#  1. [code] "The one-chokepoint site resolver, with the registry test that
#     asserts every tool goes through it." Qualified by A11 (the chokepoint is
#     an application-layer function, item 4 requires a containment test that
#     FAILS CI) and A14 (the chokepoint has two call sites and the dispatcher,
#     not the call site, decides whether the restrictive policies engage).
#     Checked: the chokepoint function is defined; the containment guard script
#     and its self-test both exist; BOTH are wired into ci.yml with the
#     self-test first. A containment test that is not in CI does not fail CI,
#     and A11 item 4 says "fails CI" rather than "exists".
#
#  2. [code] "The two sanitizers -- one for text on its way to the model, one
#     for text on its way to a human -- with a planted hostile-name test on the
#     approval payload." Qualified by A13, whose ship gate is exactly the
#     planted hostile site name. Checked: a model-bound fence, a human-bound
#     sanitizer on the approval surface, and a test that plants a hostile site
#     name AND asserts the permitted-action set is unchanged.
#
#  3. [code] "The proposal state machine with the conditional approve, plus
#     proofs that the self-approval refusal and the wrong-credential-class
#     refusal actually fire." Checked: the proposal state machine is reachable
#     from Go and not merely a table; a test proves self-approval is refused; a
#     test proves a wrong credential class is refused.
#
#  4. [code] "The approve path's single transaction, with a proof that a forced
#     ledger-write failure leaves the request pending and dispatches nothing."
#     Qualified by A10: the fail-closed audit helper must EXIST as a function
#     and be the only audit path this surface uses. Checked: the fail-closed
#     helper is a function in the audit package; a test forces the append to
#     fail and asserts nothing was served or dispatched.
#
#  5. [code] "Off by default at the tenant level, and a connection whose site
#     allowlist starts empty." Checked: the allowlist columns carry an empty
#     default in the migration that creates them; a test proves the tenant
#     assistant is off until explicitly enabled.
#
#  6. [code]+[artefact] "A per-tenant kill switch reachable in one click, and a
#     data-classification statement naming exactly which fields can leave."
#     Two obligations of two different kinds, so it is scored as two halves and
#     is MET only when both hold. The kill switch is code. The
#     data-classification statement is an artefact.
#
#  7. [code] "The rate limiter and quota counter, and a documented refusal when
#     the pending queue is capped." Checked: the limiter and the counter exist;
#     a NAMED refusal exists for the capped pending queue. The ADR is ambiguous
#     about whether "documented" means a doc comment or a typed refusal on the
#     wire; per the instruction to take the stricter reading, this guard
#     requires a typed refusal a client can see, not a comment.
#
#  8. [artefact] "Agreed kill criteria for the pilot, written down before it
#     starts."
#
#  9. [manual] "`make test-integration` run locally before merge." Unscoreable;
#     see above. The guard still verifies the two facts that make the obligation
#     real -- the target exists, and ci.yml does not run that package -- because
#     if either changed the item would need rewriting rather than scoring.

set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/check-assistant-ship-gate.sh [ROOT]

ROOT defaults to the repository this script lives in, or
$WPMGR_ASSISTANT_GATE_ROOT when that is set.

Exit 0: all nine ADR-061 pre-ship gate items are met.
Exit 1: at least one item is unmet or cannot be scored.
Exit 2: the guard cannot read this tree, or the ADR changed shape.
USAGE
}

case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${1:-${WPMGR_ASSISTANT_GATE_ROOT:-$(cd "$SCRIPT_DIR/.." && pwd)}}"

broken() {
  printf 'GUARD BROKEN: %s\n' "$1" >&2
  printf 'This guard scored nothing. Do not read this as a pass.\n' >&2
  exit 2
}

if [ ! -d "$ROOT" ]; then
  broken "$ROOT is not a directory."
fi
cd "$ROOT" || broken "cannot cd into $ROOT."

# ---------------------------------------------------------------------------
# Dependencies. A DoD step that cannot find its binary fails loudly; it is never
# skipped. Absent tool means the guard is broken, not that the tree is clean.
# ---------------------------------------------------------------------------
for _bin in grep awk sed; do
  command -v "$_bin" >/dev/null 2>&1 ||
    broken "required command '$_bin' is not on PATH; nothing can be scored."
done

# ---------------------------------------------------------------------------
# Paths this guard reads. Named once so a move is one edit and so the anti-rot
# probes can say which file went missing.
# ---------------------------------------------------------------------------
F_ADR='docs/adr/ADR-061-assistant-surface-phase-1.md'
F_CI='.github/workflows/ci.yml'
F_MAKEFILE='Makefile'
D_MCP='apps/api/internal/mcp'
D_AUDIT='apps/api/internal/audit'
D_API='apps/api'
D_WEB='apps/web/src'
D_MIGRATIONS='apps/api/migrations'
F_CONTAINMENT='scripts/check-mcp-site-containment.sh'
F_CONTAINMENT_TEST='scripts/check-mcp-site-containment_test.sh'
F_DATA_CLASS='docs/assistant-data-classification.md'
F_KILL_CRITERIA='docs/assistant-pilot-kill-criteria.md'

ADR_GATE_HEADING='**What has to exist before v1 ships.**'
ADR_GATE_ITEMS=9

# ---------------------------------------------------------------------------
# Scoring state. `met`, `unmet` and `unscoreable` are counted separately because
# the three mean different things to a reader and only one of them is good.
# ---------------------------------------------------------------------------
n_met=0
n_unmet=0
n_unscoreable=0

# Reasons accumulated for the item currently being scored. An item is MET only
# when it accumulated no reasons at all, which is what stops a half-checked item
# from reading green: the default is closed.
ITEM_REASONS=()

item_reset() { ITEM_REASONS=(); }
need() { ITEM_REASONS+=("$1"); }

# report N KIND TITLE -- prints the verdict for one item and updates the counts.
report() {
  _n="$1"
  _kind="$2"
  _title="$3"
  if [ "${#ITEM_REASONS[@]}" -eq 0 ]; then
    printf 'ITEM %d  [%-8s] MET    %s\n' "$_n" "$_kind" "$_title"
    n_met=$((n_met + 1))
    if [ "$_kind" = "artefact" ]; then
      printf '    NOTE: this is an artefact check. The document exists and is current.\n'
      printf '          Nothing here says its contents are correct.\n'
    fi
    return 0
  fi
  printf 'ITEM %d  [%-8s] UNMET  %s\n' "$_n" "$_kind" "$_title"
  for _r in "${ITEM_REASONS[@]}"; do
    printf '    NEEDS: %s\n' "$_r"
  done
  n_unmet=$((n_unmet + 1))
  return 1
}

# report_unscoreable N TITLE REMEDY -- for an item no tree can answer. Counts as
# a failure. This exists so that "cannot check" can never be spelled "OK".
report_unscoreable() {
  printf 'ITEM %d  [%-8s] UNSCOREABLE  %s\n' "$1" "manual" "$2"
  printf '    This guard cannot score this item from the repository, and it is\n'
  printf '    therefore counted as NOT MET. It is not skipped.\n'
  printf '    NEEDS: %s\n' "$3"
  n_unscoreable=$((n_unscoreable + 1))
}

# ---------------------------------------------------------------------------
# Extraction helpers. Every one of these treats "matched nothing" as a fact to
# be handled by the caller, never as a silent success.
# ---------------------------------------------------------------------------

# NOTE ON grep -q AND xargs. Every helper below counts matching FILES with
# `grep -l` rather than short-circuiting with `grep -q`. `grep -q` exits as soon
# as it matches, closing the pipe under it, and xargs then prints "terminated
# with signal 13" to stderr on a check that PASSED. Noise on stderr from a
# passing guard teaches a reader to ignore this guard's stderr, and the one
# message that must never be ignored is GUARD BROKEN.

# files_matching PATTERN FIND_ARGS... -- how many files under the given find
# expression match the extended regex. Prints a number, always.
files_matching() {
  _pat="$1"
  shift
  find "$@" -exec grep -lE -- "$_pat" {} + 2>/dev/null | wc -l | tr -d '[:space:]'
}

# go_defines PATTERN DIR -- a Go declaration matching PATTERN exists somewhere
# under DIR in a non-test file. Non-test matters: a helper that exists only
# inside _test.go is not a thing the surface uses.
go_defines() {
  _pat="$1"
  _dir="$2"
  [ -d "$_dir" ] || return 1
  [ "$(files_matching "$_pat" "$_dir" -name '*.go' ! -name '*_test.go')" -gt 0 ]
}

# grep_dir PATTERN DIR [FIND_ARGS...] -- PATTERN appears in some file under DIR.
grep_dir() {
  _pat="$1"
  _dir="$2"
  shift 2
  [ -d "$_dir" ] || return 1
  [ "$(files_matching "$_pat" "$_dir" -type f "$@")" -gt 0 ]
}

# go_test_asserting NAME_PATTERN BODY_PATTERN DIR -- there is a Go test function
# whose NAME matches NAME_PATTERN in a file that also matches BODY_PATTERN.
#
# WHY BOTH. A test named for a thing is not a test of the thing. Requiring the
# file to also carry the behavioural marker is what stops a stub named
# TestSelfApprovalIsRefused, containing nothing, from closing a gate item. It is
# not proof the assertion is correct -- nothing short of mutation testing is --
# but it is strictly more than a name match, and the name match alone is the
# failure mode this repository keeps shipping.
go_test_asserting() {
  _name="$1"
  _body="$2"
  _dir="$3"
  [ -d "$_dir" ] || return 1
  _hits="$(find "$_dir" -name '*_test.go' -exec grep -lE -- "^func $_name" {} + 2>/dev/null)"
  [ -n "$_hits" ] || return 1
  # THE BODY PATTERN IS MATCHED WITH THE `func` DECLARATION LINES REMOVED.
  #
  # Without this, a stub closes the item on its own name. This suite planted
  #
  #     func TestSelfApprovalIsRefused(t *testing.T) {}
  #
  # -- an empty body -- and the guard passed it, because the body pattern
  # /Refus|refus/ matched the word "Refused" inside the function's own name. A
  # test that asserts nothing was scoring a gate item, which is the exact class
  # of defect the two-pattern design was introduced to prevent. Deleting the
  # declaration lines before the second match is what makes the body pattern
  # about the body.
  _n="$(printf '%s\n' "$_hits" | while IFS= read -r _f; do
    [ -n "$_f" ] || continue
    grep -v '^func ' "$_f" 2>/dev/null | grep -qE -- "$_body" 2>/dev/null && printf 'x\n'
  done | wc -l | tr -d '[:space:]')"
  [ "$_n" -gt 0 ]
}

# ci_runs_in_order FIRST SECOND -- both scripts are invoked by ci.yml and FIRST
# appears before SECOND. The ordering is the requirement, not decoration: the
# self-test runs first so a guard broken into failing open cannot pass by
# reporting success over its own errors.
ci_runs_in_order() {
  [ -f "$F_CI" ] || return 1
  _a="$(grep -n -F -- "$1" "$F_CI" 2>/dev/null | head -1 | cut -d: -f1)"
  _b="$(grep -n -F -- "$2" "$F_CI" 2>/dev/null | head -1 | cut -d: -f1)"
  [ -n "$_a" ] && [ -n "$_b" ] || return 1
  [ "$_a" -lt "$_b" ]
}

# ---------------------------------------------------------------------------
# Artefact currency.
#
# An artefact is current when it was reviewed no earlier than the last change to
# the surface it describes. Both dates come from the tree, so this rots into a
# red rather than into a green: the surface moving forward makes a stale
# statement fail on its own.
#
# SURFACE_DATE is the commit date of the newest commit touching the MCP package.
# When git cannot answer -- no git, no history, a tarball -- that is GUARD
# BROKEN, because the alternative is scoring currency against nothing and
# calling every artefact current.
# ---------------------------------------------------------------------------
# SURFACE_DATE IS RESOLVED ONCE, IN THE MAIN SHELL, BEFORE ANY SCORING.
#
# The first version of this guard resolved it lazily inside `surface_date()` and
# called that function as `$(surface_date)`. Command substitution is a subshell,
# so the `exit 2` inside `broken` killed only the subshell: against a tree with
# no git history the guard printed GUARD BROKEN twice to stderr and then carried
# on scoring and exited 1, with six MET lines above the message. That is the
# precise defect this guard exists to complain about -- announcing a verdict over
# its own errors -- committed by the guard itself, and its own suite caught it.
#
# Resolving up front, in the main shell, is what makes `broken` actually
# terminate. Nothing below this point may resolve it lazily.
SURFACE_DATE=''
resolve_surface_date() {
  command -v git >/dev/null 2>&1 ||
    broken "git is not on PATH, so artefact currency cannot be measured against the surface's last change."
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 ||
    broken "$ROOT is not a git work tree, so artefact currency cannot be measured against the surface's last change."
  SURFACE_DATE="$(git log -1 --format=%cs -- "$D_MCP" 2>/dev/null)"
  [ -n "$SURFACE_DATE" ] ||
    broken "git found no commit touching $D_MCP; the surface's last-change date is unknown and artefact currency cannot be scored."
}

# check_artefact FILE HEADING_PATTERN -- FILE exists, carries a section matching
# HEADING_PATTERN with at least one line of substance under it, and carries a
# `Last reviewed: YYYY-MM-DD` line dated on or after the surface's last change.
# Every failure appends its own reason, so the output names all of them at once
# rather than one per run.
check_artefact() {
  _file="$1"
  _heading="$2"
  if [ ! -f "$_file" ]; then
    need "create $_file. It does not exist. ADR-061 requires this statement to exist before v1 ships."
    return 1
  fi
  if ! grep -Eq -- "$_heading" "$_file" 2>/dev/null; then
    need "$_file exists but has no section matching /$_heading/. The gate item is about that section's content, so the section has to be findable."
  else
    _body="$(awk -v pat="$_heading" '
      $0 ~ pat { f = 1; next }
      f && /^#/ { exit }
      f && NF { n++ }
      END { print n + 0 }' "$_file" 2>/dev/null)"
    if [ -z "$_body" ] || [ "$_body" -lt 2 ]; then
      need "$_file has the section matching /$_heading/ but fewer than 2 non-blank lines under it. A heading with nothing under it is not a statement."
    fi
  fi
  _rev="$(grep -Eo '^Last reviewed: [0-9]{4}-[0-9]{2}-[0-9]{2}$' "$_file" 2>/dev/null | head -1 | awk '{print $3}')"
  _surface="$SURFACE_DATE"
  if [ -z "$_rev" ]; then
    need "add a line reading exactly 'Last reviewed: YYYY-MM-DD' to $_file. Without a review date this guard cannot tell a current statement from one written before the surface changed, and it will not guess."
    return 1
  fi
  # String compare is correct for ISO-8601 dates and needs no date(1), whose
  # flags differ between BSD and GNU.
  if [ "$_rev" \< "$_surface" ]; then
    need "$_file says 'Last reviewed: $_rev', but $D_MCP last changed on $_surface. Re-read the statement against the current surface and move the date."
    return 1
  fi
  return 0
}

# ===========================================================================
# ANTI-ROT PROBES. Nothing is scored until these pass.
# ===========================================================================
[ -f "$F_ADR" ] || broken "$F_ADR is missing. This guard scores that document's gate and has nothing to score without it."

_gate_items="$(awk -v h="$ADR_GATE_HEADING" '
  index($0, h) == 1 { f = 1; next }
  f && /^\*\*/ { exit }
  f && /^- / { n++ }
  END { print n + 0 }' "$F_ADR" 2>/dev/null)"

[ -n "$_gate_items" ] ||
  broken "could not read the gate section out of $F_ADR at all."
[ "$_gate_items" -ne 0 ] ||
  broken "found no bullets under '$ADR_GATE_HEADING' in $F_ADR. Either the heading was reworded or the section was removed. A guard that scores a list it cannot find is worse than no guard."
[ "$_gate_items" -eq "$ADR_GATE_ITEMS" ] ||
  broken "$F_ADR's gate section now has $_gate_items items; this guard was written against $ADR_GATE_ITEMS. Read the ADR, derive what changed, and update this script and its self-test together. Scoring nine checks against a different list would be a fabricated result."

for _p in "$F_CI" "$F_MAKEFILE"; do
  [ -f "$_p" ] || broken "$_p is missing; this guard reads it to score wiring and cannot proceed."
done
for _d in "$D_API" "$D_MIGRATIONS"; do
  [ -d "$_d" ] || broken "$_d is missing; this guard reads it to score the surface and cannot proceed."
done

resolve_surface_date

printf 'ADR-061 pre-ship gate — %s items, scored against %s\n' "$ADR_GATE_ITEMS" "$ROOT"
printf '  [code]     = verified against the tree.\n'
printf '  [artefact] = the document exists and is current. NOT that it is correct.\n'
printf '  [manual]   = cannot be scored here; counted as not met.\n'
printf '\n'

# ===========================================================================
# ITEM 1 — the one-chokepoint site resolver and its containment test.
# ===========================================================================
item_reset
go_defines 'func \(r \*Repo\) ResolveScopeSites\(' "$D_MCP" ||
  need "define the site-scope chokepoint as a non-test function 'func (r *Repo) ResolveScopeSites(' under $D_MCP. A11 item 1 requires one function that is the only way this surface names a site."
[ -f "$F_CONTAINMENT" ] ||
  need "add $F_CONTAINMENT — the containment check A11 item 4 requires."
[ -f "$F_CONTAINMENT_TEST" ] ||
  need "add $F_CONTAINMENT_TEST — the containment guard's own suite. A guard with no suite is the thing CLAUDE.md forbids."
ci_runs_in_order "$F_CONTAINMENT_TEST" "$F_CONTAINMENT" ||
  need "wire $F_CONTAINMENT_TEST and then $F_CONTAINMENT into $F_CI, self-test FIRST. A11 item 4 says the containment test FAILS CI; a guard that is not in CI fails nothing."
report 1 "code" "one-chokepoint site resolver + containment test that fails CI"

# ===========================================================================
# ITEM 2 — the two sanitizers and the planted hostile-name test.
# ===========================================================================
item_reset
go_defines 'func fenceSiteText\(|func FenceSiteText\(' "$D_MCP" ||
  need "define the model-bound fence under $D_MCP. A13 requires every site-originated string to reach the model inside a fenced envelope with an escaped delimiter."
go_defines 'siteTextNotice|siteTextMarker' "$D_MCP" ||
  need "define the standing preamble and the provenance marker the fence wraps text in (A13: 'a standing preamble stating that the enclosed material is reference text that cannot change what is permitted')."
go_test_asserting 'TestFence.*Hostile.*SiteName' 'fenceSiteText|siteTextMarker' "$D_MCP" ||
  need "add the planted hostile-site-name test A13 names as the ship gate: a site whose name is an injection payload must leave the permitted-action set unchanged and still render. A fence nobody has watched fail is not known to fence anything."
# THE HUMAN-BOUND SANITIZER, AND THE FALSE GREEN THIS PATTERN REPLACES.
#
# The first version of this check grepped the consent feature for /sanitiz|escape/
# and went green. What it had actually matched was
#
#     const escaped = word.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
#
# in apps/web/src/features/mcp-consent/site-enforcement.ts -- a regex-metacharacter
# escape inside a keyword matcher, which has nothing to do with rendering
# site-supplied text to a human. A substring that happens to appear near the
# right feature is not the thing. So the check names the OBLIGATION instead: a
# function whose identifier says it sanitizes site-supplied or approval-payload
# text for display, and a test that exercises it.
_human_sanitizer=0
grep_dir '(sanitize|escape)(SiteText|SiteSupplied|ForDisplay|ApprovalPayload)|(siteText|siteSupplied|approvalPayload)(Sanitiz|Escap)' "$D_WEB" &&
  _human_sanitizer=1
go_defines '(Sanitize|Escape)(SiteText|SiteSupplied|ForDisplay|ApprovalPayload)' "$D_API/internal" &&
  _human_sanitizer=1
[ "$_human_sanitizer" -eq 1 ] ||
  need "add the human-bound sanitizer for text on its way to a human. ADR-061 asks for TWO sanitizers; only the model-bound fence exists. The approval surface renders a site-supplied name to the operator who is about to authorise something, which is the exact moment a crafted name is worth the attacker's effort. Name it for what it does — sanitizeSiteTextForDisplay or similar — so it cannot be confused with the regex-metacharacter escape already in mcp-consent/site-enforcement.ts, which is not this."
report 2 "code" "model-bound fence + human-bound sanitizer + planted hostile-name test"

# ===========================================================================
# ITEM 3 — the proposal state machine and its two refusal proofs.
# ===========================================================================
item_reset
grep_dir 'assistant_update_proposals' "$D_MIGRATIONS" -name '*.sql' ||
  need "create the assistant_update_proposals table with its state CHECK. The proposal state machine has no schema."
# The table is not the state machine. A machine nobody drives from Go is a
# schema, and the gate item is about the approve path.
#
# THIS PROBE WAS WRITTEN TWICE, AND THE FIRST VERSION FAILED OPEN TWO WAYS.
#
# It read:
#
#     if [ -d "$D_API/internal" ]; then
#       find ... -print0 | xargs -0 grep -Eq '...' || need "..."
#     fi
#
# 1. The `if` made a MISSING DIRECTORY a pass. Delete or rename
#    apps/api/internal and this requirement was not scored at all, while item 3
#    could still report MET off its other checks. The anti-rot probes verify
#    $D_API and $D_MIGRATIONS only, so nothing else catches the move. That is
#    this guard's own headline rule -- a guard that finds nothing must go red,
#    not green -- broken inside the guard.
# 2. `xargs -0 grep -Eq` is the shape the NOTE above rejects. xargs runs grep
#    once per batch and `grep -q` exits 1 on a batch with no match, so xargs
#    exits 123 on a large tree whenever ANY batch misses. That is a false UNMET
#    even when the reference exists.
#
# grep_dir answers both: it returns 1 for an absent directory, which reaches
# `need` and reddens, and it counts matching FILES with `grep -l` over a single
# `find -exec ... +`, so no batch can vote against the others.
grep_dir 'assistant_update_proposals|AssistantUpdateProposal' "$D_API/internal" \
  -name '*.go' ! -path '*/db/sqlc/*' ||
  need "drive the proposal state machine from Go. assistant_update_proposals is referenced only by the generated sqlc models, so the conditional approve exists as a table and not as a code path."
go_test_asserting 'Test.*Self.*Approv' 'Refus|refus|Deni|deni' "$D_API" ||
  need "add the self-approval refusal proof: a test named Test...SelfApprov... asserting the proposer cannot approve its own proposal. ADR-061 asks for the proof to be watched going red and green, not for the refusal to be asserted in prose."
go_test_asserting 'Test.*Credential.*Class|Test.*WrongCredential|Test.*ActorClass' 'Refus|refus|Deni|deni' "$D_API" ||
  need "add the wrong-credential-class refusal proof. Decision 2's disjoint credential classes are the whole of the out-of-band approval design, and nothing currently proves the refusal fires."
report 3 "code" "proposal state machine + self-approval and credential-class refusal proofs"

# ===========================================================================
# ITEM 4 — the approve path's single transaction and the forced ledger failure.
# ===========================================================================
item_reset
go_defines 'func .*FailClosed|func .*AppendFailClosed' "$D_AUDIT" ||
  need "build the fail-closed audit helper in $D_AUDIT as A10 requires. A10 is explicit that the fail-closed branch 'exists as a sentence in a doc comment and not as a function', and that the helper 'becomes the only audit path this surface may use'. A surface that can reach the fail-open helper will eventually reach it."
go_test_asserting 'Test.*Audit.*Fail.*Closed|Test.*AuditAppendFails|Test.*WhenTheAuditAppendFails' 'Append|append' "$D_MCP" ||
  need "add the forced-ledger-failure proof under $D_MCP: inject a failing append and assert nothing was served. ADR-061 says this one needs the fault injected rather than reasoned about."
go_test_asserting 'Test.*Audit.*Fail.*Closed|Test.*CannotCommit' 'pending|Pending|dispatch|Dispatch' "$D_API/tests" ||
  need "extend the forced-ledger-failure proof to the approve path: assert that a failed ledger write leaves the request PENDING and DISPATCHES NOTHING. The existing fail-closed proofs cover the tool-call read path; the gate bullet is about the approve path's single transaction."
report 4 "code" "approve path single transaction + forced ledger-write failure proof"

# ===========================================================================
# ITEM 5 — off by default, and an allowlist that starts empty.
# ===========================================================================
item_reset
grep_dir '"scope_site_ids" +uuid\[\] +NOT NULL +DEFAULT +.\{\}.' "$D_MIGRATIONS" -name '*.sql' ||
  need "give scope_site_ids an empty default in its creating migration, so a connection's site allowlist starts empty and a credential leaked from a half-configured connection reads nothing."
grep_dir '"scope_tag_ids" +uuid\[\] +NOT NULL +DEFAULT +.\{\}.' "$D_MIGRATIONS" -name '*.sql' ||
  need "give scope_tag_ids an empty default in its creating migration, for the same reason as scope_site_ids."
# THE ALTERNATION HERE IS ERE, NOT BRE, AND THE DIFFERENCE IS A DEAD CHECK.
#
# This pattern reached `go_test_asserting`, which greps with `grep -lE`, written
# as `\(Default\|Disabled\|Off\)`. Under an EXTENDED regex `\(`, `\|` and `\)`
# are the LITERAL characters, so that alternative asked for a test whose name
# contains the seven-character text "(Default|Disabled|Off)" -- which no Go
# identifier may contain. It could not match anything, ever. It survived because
# the second alternative, Test.*NoAssistantControl, is valid ERE and does match,
# so item 5 read MET and the dead half was invisible. A check that can never
# fire is indistinguishable from one that always passes.
go_test_asserting 'Test.*Assistant.*(Default|Disabled|Off)|Test.*NoAssistantControl' 'enabl|Enabl' "$D_API" ||
  need "add the tenant default-off proof: a test asserting the assistant is off for an organisation that has never enabled it, and that reading it writes no enablement."
report 5 "code" "off by default at the tenant + connection allowlist starts empty"

# ===========================================================================
# ITEM 6 — the kill switch (code) and the data-classification statement
# (artefact). Two halves of two kinds; MET needs both.
# ===========================================================================
item_reset
grep_dir 'assistant_enabled|assistant_paused' "$D_MIGRATIONS" -name '*.sql' ||
  need "add the per-tenant assistant kill-switch columns to a migration."
go_defines 'func .*Pause|func .*pauseAssistant' "$D_API/internal/tenant" ||
  need "add the API path that engages the per-tenant kill switch."
# THE UI HALF, AND THE FALSE GREEN THIS PATTERN REPLACES.
#
# The first version grepped apps/web/src for /assistant/ and went green on
# mcp-consent files that mention the assistant while containing no control at
# all. Bare /assistant/ is a word this product says everywhere. The check now
# requires the identifier to name the ACTION -- pause, resume, disable, kill --
# adjacent to the assistant, in either order, because that is the control and
# not the topic.
grep_dir '[Aa]ssistant[A-Za-z]*(Pause|Resume|Disable|Kill)|(pause|resume|disable|kill)[A-Za-z]*[Aa]ssistant|assistant/(pause|resume)' "$D_WEB" ||
  need "add the one-click kill-switch control to the dashboard under $D_WEB. ADR-061 says 'reachable in one click'; the m130 API route exists but nothing in the dashboard calls it, so today the switch is reachable only by curl. The UI is how this gets used under the pressure it exists for, and that pressure is not the moment to look up an endpoint."
check_artefact "$F_DATA_CLASS" '^#+ .*[Ff]ields.*leave'
report 6 "mixed" "per-tenant kill switch in one click + data-classification statement"

# ===========================================================================
# ITEM 7 — the rate limiter, the quota counter, and the capped-queue refusal.
# ===========================================================================
item_reset
go_defines 'func newToolCallLimiter\(|func .*ToolCallLimiter\(' "$D_MCP" ||
  need "add the per-tenant and per-grant rate limiter on the tool-call path."
# RATE LIMITER AND QUOTA COUNTER ARE TWO THINGS, and an earlier draft of this
# check conflated them: it looked for a "register limit" and would have counted
# newRegistrationLimiter as the quota counter. It is not. A rate limiter is a
# token bucket in memory that forgets; a quota counter is a persisted count
# against an allowance. The ADR lists both, so both are required.
go_defines 'func newRegistrationLimiter\(|func .*RegistrationLimiter\(' "$D_MCP" ||
  need "add the registration-path rate limiter."
go_defines '[Qq]uota' "$D_MCP" ||
  need "add the quota counter. The tool-call and registration rate limiters exist and are in-memory token buckets, which forget across a restart and across a revision; nothing counts consumption against a per-tenant allowance. ADR-061 lists the limiter and the counter as two things because they answer two different questions."
grep_dir 'pending_queue_capped|PendingQueueCapped|pending_capped' "$D_MCP" -name '*.go' ||
  need "add a NAMED, typed refusal for a capped pending queue, visible to a client on the wire. ADR-061 says 'a documented refusal when the pending queue is capped'; read strictly, a doc comment is not a refusal a caller can act on, and this guard takes the stricter reading."
report 7 "code" "rate limiter + quota counter + typed refusal when the pending queue is capped"

# ===========================================================================
# ITEM 8 — the pilot kill criteria.
# ===========================================================================
item_reset
check_artefact "$F_KILL_CRITERIA" '^#+ .*[Kk]ill criteria'
report 8 "artefact" "agreed pilot kill criteria, written down before the pilot starts"

# ===========================================================================
# ITEM 9 — the local integration run. Unscoreable, and therefore red.
# ===========================================================================
# The two facts that make the obligation real are still checked, because if
# either changed the item would need rewriting rather than scoring, and this
# guard must not quietly keep asking for something that no longer applies.
# THE PROBE MUST LOOK AT WHAT ci.yml RUNS, NOT WHAT IT MENTIONS. ci.yml
# discusses `make test-integration` in two comments explaining why that package
# is NOT run there, and a bare mention-grep read those two comments as evidence
# that it is. Anchoring on `run:` is the difference between the workflow's
# behaviour and the workflow's prose.
_ctx=''
grep -Eq '^\.PHONY: test-integration|^test-integration:' "$F_MAKEFILE" 2>/dev/null ||
  _ctx=" (WARNING: no 'test-integration' target found in $F_MAKEFILE — this item's premise has stopped holding; re-derive it from the ADR before acting on this line.)"

if grep -Eq '^[[:space:]]*run:.*test-integration' "$F_CI" 2>/dev/null; then
  # THE ONE WAY THIS ITEM CLOSES. "Somebody ran it locally" leaves no artefact
  # and no script can score it. "CI runs it on every PR" is the same obligation
  # discharged by a mechanism that does leave evidence, and it is strictly
  # stronger: it holds for the merge nobody remembered to run it before. So when
  # ci.yml actually invokes the package, the item is MET as [code] -- verified,
  # not taken on trust.
  item_reset
  report 9 "code" "the integration package is run by $F_CI, so the obligation is discharged mechanically"
else
  report_unscoreable 9 "\`make test-integration\` run locally before merge$_ctx" \
    "this is an event on a developer's machine and leaves no artefact, so no script can score it, and this guard will not pretend otherwise. It closes exactly one way: run the integration package in $F_CI, which discharges the same obligation by a mechanism that leaves evidence and that also covers the merge nobody remembered. That is a real decision — .claude/rules/ci-and-build-logic.md records why that package is not in ci.yml today — so take it deliberately rather than to clear this line."
fi

# ===========================================================================
# Verdict.
# ===========================================================================
printf '\n'
printf 'MET %d / %d.  UNMET %d.  UNSCOREABLE %d.\n' \
  "$n_met" "$ADR_GATE_ITEMS" "$n_unmet" "$n_unscoreable"

if [ "$n_unmet" -eq 0 ] && [ "$n_unscoreable" -eq 0 ]; then
  printf 'Every ADR-061 pre-ship gate item is met.\n'
  printf 'Items marked [artefact] mean the document exists and is current, not that it is right.\n'
  exit 0
fi

printf '\n'
printf 'The ADR-061 pre-ship gate is NOT met. ADR-060 carries the only absolute in\n'
printf 'that record — no new externally-reachable surface ships while an\n'
printf 'auth-boundary item is open — and ADR-061 A9 applies it to this surface by\n'
printf 'name: the Phase-1 gate work closes BEFORE the MCP endpoint ships.\n'
exit 1
