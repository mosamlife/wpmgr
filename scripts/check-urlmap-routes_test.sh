#!/usr/bin/env bash
# scripts/check-urlmap-routes_test.sh
#
# The regression suite for scripts/check-urlmap-routes.sh.
#
# WHY THIS EXISTS. A guard nobody has watched fail is not known to guard
# anything. Both incidents this guard was written for are cases in here, as
# reproductions rather than as prose: case "incident-1" is POST /mcp shipping
# unroutable, case "incident-2" is the OAuth discovery documents about to do
# the same. If either stops going red, a test goes red instead.
#
# The other half of the suite is the part that is easy to skip: the cases the
# guard must NOT redden. A guard that fails correct work gets switched off, and
# then it guards nothing.
#
# RUN IT:
#   make check-urlmap-test
#   scripts/check-urlmap-routes_test.sh            # everything
#   scripts/check-urlmap-routes_test.sh incident   # only cases matching "incident"
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_URLMAP_GUARD=/tmp/guard-with-hole.sh scripts/check-urlmap-routes_test.sh
#
# PORTABILITY. bash 3.2 and POSIX tools; no mapfile, no associative arrays,
# no sed -i, no grep -P.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_URLMAP_GUARD:-$HERE/check-urlmap-routes.sh}"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  printf 'check-urlmap-routes_test: guard not found: %s\n' "$GUARD" >&2
  exit 2
fi

PASS=0
FAIL=0
WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

# --- fixtures --------------------------------------------------------------

# A url-map in the real shape, parameterised by which path rules it carries.
# Written as a function rather than a static file so each case can express the
# exact gap it is reproducing.
make_urlmap() {
  # $1 = output path; remaining args = paths for the API rule
  out="$1"; shift
  {
    printf 'defaultService: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-web\n'
    printf 'name: wpmgr-urlmap\n'
    printf 'hostRules:\n'
    printf -- '- hosts:\n'
    printf -- '  - manage.wpmgr.app\n'
    printf '  pathMatcher: main\n'
    printf 'pathMatchers:\n'
    printf -- '- defaultService: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-web\n'
    printf '  name: main\n'
    printf '  pathRules:\n'
    printf -- '  - paths:\n'
    for p in "$@"; do printf -- '    - %s\n' "$p"; done
    printf '    service: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-api\n'
    printf -- '- defaultService: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-marketing\n'
    printf '  name: landing\n'
  } > "$out"
}

# The path rules that were live in GCP before either incident was fixed.
BASE_RULES='/api/* /agent/* /auth/* /enroll /enroll/* /healthz /readyz /metrics /rum /rum/* /webhooks/*'

make_routes() {
  out="$1"; shift
  : > "$out"
  for r in "$@"; do printf 'GET\t%s\n' "$r" >> "$out"; done
}

EMPTY_ALLOW="$WORK/allow-empty.txt"
printf '# no entries\n' > "$EMPTY_ALLOW"

# --- harness ---------------------------------------------------------------

# run_case <name> <expected-exit> <must-contain|-> <must-not-contain|-> -- <guard args...>
run_case() {
  name="$1"; want="$2"; needle="$3"; anti="$4"; shift 5   # shift past the '--'
  case "$name" in
    *"$FILTER"*) : ;;
    *) return 0 ;;
  esac

  outf="$WORK/out.$$"
  "$GUARD" "$@" > "$outf" 2>&1
  got=$?

  ok=1
  msg=""
  if [ "$got" -ne "$want" ]; then
    ok=0; msg="expected exit $want, got $got"
  fi
  if [ "$needle" != "-" ] && ! grep -qF "$needle" "$outf"; then
    ok=0; msg="$msg; output missing: $needle"
  fi
  if [ "$anti" != "-" ] && grep -qF "$anti" "$outf"; then
    ok=0; msg="$msg; output must not contain: $anti"
  fi

  if [ "$ok" -eq 1 ]; then
    PASS=$((PASS + 1)); printf 'ok   %s\n' "$name"
  else
    FAIL=$((FAIL + 1)); printf 'FAIL %s (%s)\n' "$name" "$msg"
    sed 's/^/       | /' "$outf"
  fi
}

# ===========================================================================
# A. It fires — the two real incidents, reproduced.
# ===========================================================================

# Incident 1: POST /mcp shipped, deployed, unreachable. The API mounted it; the
# url-map had no rule for it, so the web SPA answered it with 200 text/html.
make_urlmap "$WORK/um-pre-mcp.yaml" $BASE_RULES
make_routes "$WORK/rt-mcp.tsv" /healthz /api/v1/sites /mcp
run_case "incident-1-mcp-unrouted" 1 "MISSING  /mcp" - -- \
  --routes-file "$WORK/rt-mcp.tsv" --urlmap "$WORK/um-pre-mcp.yaml" --allowlist "$EMPTY_ALLOW"

# Incident 2: the three OAuth discovery documents from PR #589, against the
# url-map as it is live in GCP right now (which does have /mcp).
make_urlmap "$WORK/um-live.yaml" $BASE_RULES /mcp
make_routes "$WORK/rt-oauth.tsv" /healthz /mcp \
  /.well-known/oauth-authorization-server \
  /.well-known/oauth-protected-resource \
  /.well-known/oauth-protected-resource/mcp
run_case "incident-2-oauth-discovery-unrouted" 1 "MISSING  /.well-known/oauth-authorization-server" - -- \
  --routes-file "$WORK/rt-oauth.tsv" --urlmap "$WORK/um-live.yaml" --allowlist "$EMPTY_ALLOW"

# The RFC 9728 path-inserted form is a DISTINCT rule. A url-map that lists only
# the parent must still go red for the /mcp child, because an exact path rule
# does not cover its subpaths. This is the single subtlest thing in the file.
make_urlmap "$WORK/um-parent-only.yaml" $BASE_RULES /mcp /.well-known/oauth-protected-resource
make_routes "$WORK/rt-child.tsv" /.well-known/oauth-protected-resource/mcp
run_case "exact-rule-does-not-cover-subpath" 1 "MISSING  /.well-known/oauth-protected-resource/mcp" - -- \
  --routes-file "$WORK/rt-child.tsv" --urlmap "$WORK/um-parent-only.yaml" --allowlist "$EMPTY_ALLOW"

# A brand-new top-level surface — the general shape of both incidents.
make_urlmap "$WORK/um-base.yaml" $BASE_RULES
make_routes "$WORK/rt-new.tsv" /healthz /graphql
run_case "planted-new-toplevel-route" 1 "MISSING  /graphql" - -- \
  --routes-file "$WORK/rt-new.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$EMPTY_ALLOW"

# A rule pointing at the WEB backend does not count as coverage. This is the
# "match the structure, not the first substring" case: /graphql is present in
# the YAML, just routed to the wrong service.
{
  printf 'defaultService: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-web\n'
  printf 'name: wpmgr-urlmap\n'
  printf 'pathMatchers:\n'
  printf -- '- defaultService: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-web\n'
  printf '  name: main\n'
  printf '  pathRules:\n'
  printf -- '  - paths:\n'
  printf -- '    - /graphql\n'
  printf '    service: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-web\n'
  printf -- '  - paths:\n'
  printf -- '    - /api/*\n'
  printf '    service: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-api\n'
} > "$WORK/um-wrong-backend.yaml"
make_routes "$WORK/rt-graphql.tsv" /graphql
run_case "path-routed-to-web-backend-is-not-coverage" 1 "MISSING  /graphql" - -- \
  --routes-file "$WORK/rt-graphql.tsv" --urlmap "$WORK/um-wrong-backend.yaml" --allowlist "$EMPTY_ALLOW"

# ===========================================================================
# B. It does not over-fire — cases that must stay green.
# ===========================================================================

# The committed url-map against a route set covering every surface it serves.
make_routes "$WORK/rt-full.tsv" /healthz /readyz /metrics /mcp \
  /.well-known/oauth-authorization-server \
  /.well-known/oauth-protected-resource \
  /.well-known/oauth-protected-resource/mcp \
  /api/v1/sites /api/v1/sites/{id} /api/v1/backups/{id}/events \
  /auth/login /agent/v1/checkin /enroll /enroll/{token} \
  /rum /rum/ingest /webhooks/razorpay
run_case "committed-urlmap-covers-every-surface" 0 "OK" "MISSING" -- \
  --routes-file "$WORK/rt-full.tsv" --urlmap "$REPO_ROOT/infra/urlmap.yaml" \
  --allowlist "$REPO_ROOT/infra/urlmap-unrouted-routes.txt"

# A {param} segment must match a prefix rule. If it did not, roughly every
# route in the API would be reported missing and the guard would be deleted
# on its first run.
make_routes "$WORK/rt-params.tsv" /api/v1/sites/{siteId}/backups/{backupId}/restore
run_case "params-match-prefix-rule" 0 "OK" "MISSING" -- \
  --routes-file "$WORK/rt-params.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$EMPTY_ALLOW"

# A mid-path wildcard rule matches exactly one segment.
make_urlmap "$WORK/um-mid.yaml" '/api/v1/backups/*/events'
make_routes "$WORK/rt-mid.tsv" /api/v1/backups/{id}/events
run_case "mid-path-wildcard-matches-one-segment" 0 "OK" "MISSING" -- \
  --routes-file "$WORK/rt-mid.tsv" --urlmap "$WORK/um-mid.yaml" --allowlist "$EMPTY_ALLOW"

# The deliberate-exclusion path: an internal route WITH a reason is green.
printf '# reason: in-cluster admin probe, never exposed publicly\n/internal/debug\n' > "$WORK/allow-ok.txt"
make_routes "$WORK/rt-internal.tsv" /healthz /internal/debug
run_case "allowlisted-internal-route-is-green" 0 "OK" "MISSING" -- \
  --routes-file "$WORK/rt-internal.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$WORK/allow-ok.txt"

# ===========================================================================
# C. The allowlist cannot become a silent ignore list.
# ===========================================================================

# No reason line -> fatal, not a silent skip.
printf '/internal/debug\n' > "$WORK/allow-noreason.txt"
make_routes "$WORK/rt-internal2.tsv" /internal/debug
run_case "allowlist-entry-without-reason-is-fatal" 2 'no "# reason:" line' - -- \
  --routes-file "$WORK/rt-internal2.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$WORK/allow-noreason.txt"

# A plain comment is not a reason.
printf '# internal thing\n/internal/debug\n' > "$WORK/allow-comment.txt"
run_case "plain-comment-is-not-a-reason" 2 'no "# reason:" line' - -- \
  --routes-file "$WORK/rt-internal2.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$WORK/allow-comment.txt"

# An entry that excuses no live route is stale and fails, so the list cannot
# accumulate excuses that outlive their routes.
printf '# reason: route was removed in a refactor and nobody cleaned this up\n/gone/route\n' > "$WORK/allow-stale.txt"
make_routes "$WORK/rt-nostale.tsv" /healthz
run_case "stale-allowlist-entry-fails" 1 "STALE  /gone/route" - -- \
  --routes-file "$WORK/rt-nostale.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$WORK/allow-stale.txt"

# ===========================================================================
# D. Finding nothing goes RED, never green. This project's signature defect is
#    announcing success over its own errors, so every empty/missing input is a
#    named case here.
# ===========================================================================

run_case "missing-urlmap-is-fatal" 2 "url-map not found" "OK" -- \
  --routes-file "$WORK/rt-full.tsv" --urlmap "$WORK/does-not-exist.yaml" --allowlist "$EMPTY_ALLOW"

run_case "missing-routes-file-is-fatal" 2 "routes file not found" "OK" -- \
  --routes-file "$WORK/nope.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$EMPTY_ALLOW"

run_case "missing-allowlist-is-fatal" 2 "allowlist not found" "OK" -- \
  --routes-file "$WORK/rt-full.tsv" --urlmap "$REPO_ROOT/infra/urlmap.yaml" --allowlist "$WORK/nope.txt"

# An empty route list is the vacuous-green trap: zero routes trivially satisfy
# "every route is covered".
: > "$WORK/rt-empty.tsv"
run_case "empty-route-list-is-fatal" 2 "produced 0 usable routes" "OK" -- \
  --routes-file "$WORK/rt-empty.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$EMPTY_ALLOW"

# Route source produced output, but none of it parses as METHOD<TAB>PATH — a
# dumper that changed its format, or printed a banner and exited.
printf 'building...\nno routes today\n' > "$WORK/rt-garbage.tsv"
run_case "unparseable-route-output-is-fatal" 2 "produced 0 usable routes" "OK" -- \
  --routes-file "$WORK/rt-garbage.tsv" --urlmap "$WORK/um-base.yaml" --allowlist "$EMPTY_ALLOW"

# A url-map with no rule pointing at the API backend at all. Depending on which
# way the comparison is written this is either "everything missing" or
# "everything fine"; neither is a checkable state, so it is fatal.
{
  printf 'name: wpmgr-urlmap\n'
  printf 'pathMatchers:\n'
  printf -- '- defaultService: https://www.googleapis.com/compute/v1/projects/p/global/backendServices/wpmgr-bes-web\n'
  printf '  name: main\n'
} > "$WORK/um-norules.yaml"
run_case "urlmap-with-no-api-rules-is-fatal" 2 "parsed 0 path rules" "OK" -- \
  --routes-file "$WORK/rt-full.tsv" --urlmap "$WORK/um-norules.yaml" --allowlist "$EMPTY_ALLOW"

# A matcher name that does not exist — a typo in the invocation must not pass.
run_case "unknown-matcher-is-fatal" 2 "parsed 0 path rules" "OK" -- \
  --routes-file "$WORK/rt-full.tsv" --urlmap "$REPO_ROOT/infra/urlmap.yaml" \
  --allowlist "$REPO_ROOT/infra/urlmap-unrouted-routes.txt" --matcher nosuchmatcher

# A backend name that does not exist, likewise.
run_case "unknown-backend-is-fatal" 2 "parsed 0 path rules" "OK" -- \
  --routes-file "$WORK/rt-full.tsv" --urlmap "$REPO_ROOT/infra/urlmap.yaml" \
  --allowlist "$REPO_ROOT/infra/urlmap-unrouted-routes.txt" --api-backend wpmgr-bes-nope

# A route dumper that fails must be fatal, never a skip. "A gate that cannot
# find its binary must fail loudly."
run_case "failing-route-dumper-is-fatal" 2 "route dumper failed" "OK" -- \
  --routes-cmd 'exit 7' --urlmap "$WORK/um-base.yaml" --allowlist "$EMPTY_ALLOW"

run_case "missing-route-dumper-binary-is-fatal" 2 "route dumper failed" "OK" -- \
  --routes-cmd 'wpmgr-no-such-binary-xyz' --urlmap "$WORK/um-base.yaml" --allowlist "$EMPTY_ALLOW"

# ===========================================================================

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
[ "$PASS" -gt 0 ] || { printf 'no cases ran (filter %s matched nothing)\n' "$FILTER" >&2; exit 2; }
exit 0
