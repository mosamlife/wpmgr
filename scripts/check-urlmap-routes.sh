#!/usr/bin/env bash
# scripts/check-urlmap-routes.sh
#
# Every path the API actually mounts is covered by a path rule in the committed
# load-balancer url-map, routing to the API backend.
#
# WHY THIS EXISTS. The url-map used to live only in GCP. Twice in one day a
# route shipped, deployed, and was unreachable, because the API mounted it and
# the load balancer did not route it:
#
#   1. POST /mcp — fell through to `defaultService` and the web SPA answered it.
#   2. The OAuth discovery documents (PR #589) — three root-mounted paths that
#      would have done the identical thing.
#
# Both failures return 200 text/html. A status-code smoke test CANNOT see them,
# because something did answer; it was just the wrong something. That is this
# project's signature defect wearing infrastructure. This guard is the check
# that would have caught both, and it needs no cloud credentials, so it runs on
# every PR rather than after a deploy when it is already too late.
#
# WHY IT IS A SCRIPT AND NOT A CI STEP. Build-gating logic in a YAML block
# scalar is untested logic: nobody can run it, so nobody can check their work.
# scripts/check-urlmap-routes_test.sh builds real fixture trees and asserts exit
# codes against this file, so a hole that gets reopened turns a test red.
#
# RUN IT:
#   make check-urlmap                 # routes from the live Gin engine
#   scripts/check-urlmap-routes.sh
#   scripts/check-urlmap-routes.sh --routes-file /tmp/routes.tsv   # offline
#
# Exit 0 when every mounted route is covered. Exit 1 on any real gap. Exit 2 on
# a broken invocation (missing input, unreadable file, a route source that
# produced nothing). There is no exit code that means "checked nothing, fine".
#
# WHERE THE ROUTE LIST COMES FROM, AND WHY NOT A HAND-KEPT ONE. A hand-kept
# list is the thing that drifts: it is written once, by the person who already
# knows about the route, and is never updated by the person who does not. The
# route source here is the real Gin engine built through server.New, dumped as
# METHOD<TAB>PATH. See --routes-cmd below.
#
# THE NIL-HANDLER TRAP, which is what makes the route source subtle.
# apps/api/internal/server/server.go guards most registrations with
# `if deps.X != nil`. apps/api/tests/contract/openapi_route_coverage_test.go's
# buildFullEngine leaves several of those nil, so those routes never appear in
# its engine.Routes(). Its allowlistLiveNotInSpec is EMPTY and it passes
# green — while the three /.well-known routes it never mounts are exactly the
# ones this guard exists to catch. A route source with a nil-handler blind spot
# would report full coverage of a surface it cannot see. The dumper this script
# calls must mount every optional handler; its own test asserts the well-known
# and /mcp routes are present, which is what stops the blind spot returning.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop with BSD grep/sed/awk and on the ubuntu CI runner with
# the GNU ones. No mapfile, no associative arrays, no sed -i, no grep -P.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"

URLMAP="$REPO_ROOT/infra/urlmap.yaml"
ALLOWLIST="$REPO_ROOT/infra/urlmap-unrouted-routes.txt"
ROUTES_FILE=""
ROUTES_CMD=""
MATCHER="main"
API_BACKEND="wpmgr-bes-api"

# The default route source. Overridable so the test suite can drive the guard
# with fixture route lists, and so a caller can dump once and check twice.
ROUTES_CMD_DEFAULT="${WPMGR_ROUTES_CMD:-go run ./cmd/dump-routes}"
ROUTES_CMD_DIR="${WPMGR_ROUTES_CMD_DIR:-$REPO_ROOT/apps/api}"

usage() {
  cat <<'EOF'
usage: check-urlmap-routes.sh [options]

  --routes-file FILE   read METHOD<TAB>PATH lines from FILE ('-' for stdin)
                       instead of running the route dumper
  --routes-cmd CMD     run CMD to produce the route list (default: the Go
                       dumper in apps/api)
  --urlmap FILE        url-map YAML to check (default: infra/urlmap.yaml)
  --allowlist FILE     deliberately-unrouted paths, with mandatory reasons
                       (default: infra/urlmap-unrouted-routes.txt)
  --matcher NAME       pathMatcher to check (default: main)
  --api-backend NAME   backend service that means "reaches the API"
                       (default: wpmgr-bes-api)
  -h, --help           this text

exit 0 = every mounted route is covered
exit 1 = a real gap (uncovered route, or a stale allowlist entry)
exit 2 = broken invocation (missing input, empty route list, unreadable file)
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --routes-file) ROUTES_FILE="${2:-}"; shift 2 ;;
    --routes-cmd)  ROUTES_CMD="${2:-}";  shift 2 ;;
    --urlmap)      URLMAP="${2:-}";      shift 2 ;;
    --allowlist)   ALLOWLIST="${2:-}";   shift 2 ;;
    --matcher)     MATCHER="${2:-}";     shift 2 ;;
    --api-backend) API_BACKEND="${2:-}"; shift 2 ;;
    -h|--help)     usage; exit 0 ;;
    *) printf 'check-urlmap-routes: unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

fail_setup() { printf 'check-urlmap-routes: FATAL: %s\n' "$1" >&2; exit 2; }

TMPDIR_RUN="$(mktemp -d 2>/dev/null)" || fail_setup "could not create a temp dir"
trap 'rm -rf "$TMPDIR_RUN"' EXIT INT TERM

ROUTES_RAW="$TMPDIR_RUN/routes.raw"
ROUTES_NORM="$TMPDIR_RUN/routes.norm"
RULES="$TMPDIR_RUN/rules"
UNCOVERED="$TMPDIR_RUN/uncovered"
ALLOW_PATTERNS="$TMPDIR_RUN/allow"

# ---------------------------------------------------------------------------
# 1. The url-map. Parsed structurally, anchored on the pathMatchers ->
#    pathRules -> service nesting, never on a first substring match: a guard
#    here once read the first substring instead of the declaration and passed
#    while both the things it was checking for were still present.
# ---------------------------------------------------------------------------

[ -n "$URLMAP" ]  || fail_setup "--urlmap was given an empty value"
[ -f "$URLMAP" ]  || fail_setup "url-map not found: $URLMAP"
[ -r "$URLMAP" ]  || fail_setup "url-map not readable: $URLMAP"

awk -v want_matcher="$MATCHER" -v want_backend="$API_BACKEND" '
  # Top-level key at column 0 opens or closes the pathMatchers block.
  /^[A-Za-z_][A-Za-z0-9_]*:/ { in_matchers = ($0 ~ /^pathMatchers:[[:space:]]*$/); next }

  !in_matchers { next }

  # A new matcher entry.
  /^-[[:space:]]/ { m++; rule = 0; collecting = 0 }

  # The matcher name (two-space indent = a key of the matcher entry).
  /^[[:space:]][[:space:]]name:[[:space:]]/ {
    n = $0; sub(/^[[:space:]]*name:[[:space:]]*/, "", n); mname[m] = n; next
  }

  # A new path rule inside pathRules.
  /^[[:space:]][[:space:]]-[[:space:]]*paths:[[:space:]]*$/ { rule++; collecting = 1; buf[m, rule] = ""; next }

  # A path list item belonging to the current rule.
  collecting && /^[[:space:]][[:space:]][[:space:]][[:space:]]-[[:space:]]*\// {
    p = $0; sub(/^[[:space:]]*-[[:space:]]*/, "", p)
    sub(/[[:space:]]+$/, "", p)
    buf[m, rule] = buf[m, rule] p "\n"
    next
  }

  # The rule closes with its service. Record which backend it points at.
  /^[[:space:]][[:space:]][[:space:]][[:space:]]service:[[:space:]]/ {
    s = $0; sub(/^[[:space:]]*service:[[:space:]]*/, "", s)
    sub(/.*\//, "", s)                  # basename of the backendServices URL
    svc[m, rule] = s; collecting = 0; next
  }

  END {
    for (k in buf) {
      split(k, parts, SUBSEP)
      mi = parts[1]
      if (mname[mi] != want_matcher) continue
      if (svc[k] != want_backend) continue
      printf "%s", buf[k]
    }
  }
' "$URLMAP" | sed '/^$/d' | sort -u > "$RULES"

RULE_COUNT="$(wc -l < "$RULES" | tr -d ' ')"
if [ "$RULE_COUNT" -eq 0 ]; then
  # The "guard that finds nothing goes green" failure, refused explicitly. An
  # empty rule set would make every route trivially uncovered OR, with the
  # comparison written the other way, make every route trivially covered.
  # Neither is a checkable state, so neither is allowed to be a pass.
  fail_setup "parsed 0 path rules for matcher '$MATCHER' -> backend '$API_BACKEND' from $URLMAP.
  Either the matcher/backend names are wrong, or the YAML shape changed and the
  parser no longer understands it. This is never a pass."
fi

# ---------------------------------------------------------------------------
# 2. The route list.
# ---------------------------------------------------------------------------

if [ -n "$ROUTES_FILE" ] && [ -n "$ROUTES_CMD" ]; then
  fail_setup "--routes-file and --routes-cmd are mutually exclusive"
fi

if [ -n "$ROUTES_FILE" ]; then
  if [ "$ROUTES_FILE" = "-" ]; then
    cat > "$ROUTES_RAW"
  else
    [ -f "$ROUTES_FILE" ] || fail_setup "routes file not found: $ROUTES_FILE"
    [ -r "$ROUTES_FILE" ] || fail_setup "routes file not readable: $ROUTES_FILE"
    cat "$ROUTES_FILE" > "$ROUTES_RAW"
  fi
else
  CMD="${ROUTES_CMD:-$ROUTES_CMD_DEFAULT}"
  # A dumper that cannot run is FATAL, never a skip. A gate that cannot find
  # its binary must fail loudly: skipping here would report success over the
  # exact condition the gate exists to detect.
  #
  # An unusable working directory is folded into the SAME failure rather than
  # checked separately, so there is one error path instead of two. An earlier
  # revision checked the directory first and reported "directory not found"
  # for an invocation whose command was what actually mattered; the message
  # names the directory below, which is the part that was worth keeping.
  ( cd "$ROUTES_CMD_DIR" 2>/dev/null && eval "$CMD" ) > "$ROUTES_RAW" 2>"$TMPDIR_RUN/routes.err"
  RC=$?
  if [ $RC -ne 0 ]; then
    printf 'check-urlmap-routes: FATAL: route dumper failed (exit %d): %s\n' "$RC" "$CMD" >&2
    printf '  in directory: %s\n' "$ROUTES_CMD_DIR" >&2
    sed 's/^/  | /' "$TMPDIR_RUN/routes.err" >&2
    exit 2
  fi
fi

# METHOD<TAB>PATH -> just the distinct paths. The load balancer routes on path
# only; it has no idea what methods a path serves, so method is dropped here.
# Anything that is not a tab-separated pair beginning with '/' in field 2 is
# ignored, and if that leaves nothing we fail below rather than pass.
awk -F'\t' 'NF >= 2 && $2 ~ /^\// { print $2 }' "$ROUTES_RAW" | sort -u > "$ROUTES_NORM"

ROUTE_COUNT="$(wc -l < "$ROUTES_NORM" | tr -d ' ')"
if [ "$ROUTE_COUNT" -eq 0 ]; then
  RAW_LINES="$(wc -l < "$ROUTES_RAW" | tr -d ' ')"
  fail_setup "the route source produced 0 usable routes (raw lines: $RAW_LINES).
  Expected METHOD<TAB>PATH lines. An empty route list would make this guard
  vacuously green over an API whose every route is unroutable, so it is fatal."
fi

# ---------------------------------------------------------------------------
# 3. The allowlist: routes deliberately NOT reachable through the load balancer.
#
# Format is a path pattern preceded by a '# reason:' line. The reason is
# MANDATORY and enforced below. If adding an entry were cheap and silent, the
# first person to add an internal route would learn to add it to the ignore
# list, and this guard would quietly stop meaning anything. A stale entry —
# one matching no live route — is an ERROR too, so the list cannot accumulate
# entries that outlive the routes they excuse.
# ---------------------------------------------------------------------------

[ -n "$ALLOWLIST" ] || fail_setup "--allowlist was given an empty value"
# A missing allowlist is a broken checkout, not "allow nothing": failing loudly
# is right, because silently treating it as empty would redden honest work and
# get the guard switched off.
[ -f "$ALLOWLIST" ] || fail_setup "allowlist not found: $ALLOWLIST"
[ -r "$ALLOWLIST" ] || fail_setup "allowlist not readable: $ALLOWLIST"

: > "$ALLOW_PATTERNS"
ALLOW_ERR=0
PREV_WAS_REASON=0
LINENO_A=0
while IFS= read -r line || [ -n "$line" ]; do
  LINENO_A=$((LINENO_A + 1))
  case "$line" in
    '#'*reason:*) PREV_WAS_REASON=1; continue ;;
    '#'*)         continue ;;
    '')           continue ;;
  esac
  trimmed="$(printf '%s' "$line" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
  [ -n "$trimmed" ] || continue
  case "$trimmed" in
    /*) : ;;
    *)  printf 'check-urlmap-routes: allowlist line %d is not a path: %s\n' "$LINENO_A" "$trimmed" >&2
        ALLOW_ERR=1; PREV_WAS_REASON=0; continue ;;
  esac
  if [ "$PREV_WAS_REASON" -ne 1 ]; then
    printf 'check-urlmap-routes: allowlist entry has no "# reason:" line above it: %s (line %d)\n' "$trimmed" "$LINENO_A" >&2
    ALLOW_ERR=1
  fi
  printf '%s\n' "$trimmed" >> "$ALLOW_PATTERNS"
  PREV_WAS_REASON=0
done < "$ALLOWLIST"

if [ "$ALLOW_ERR" -ne 0 ]; then
  printf 'check-urlmap-routes: FATAL: the allowlist is malformed. Every entry needs a "# reason:" line directly above it.\n' >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# 4. Matching, using the load balancer's own semantics.
#
#   /api/*                  prefix rule  -> matches /api/ and everything under it
#   /api/v1/backups/*/eve.. mid-path '*' -> matches exactly one path segment
#   /enroll                 exact rule   -> matches ONLY /enroll, not /enroll/x
#
# That last line is the one that matters and is easy to get wrong: an exact
# rule does not cover its own subpaths, which is why the url-map lists
# /.well-known/oauth-protected-resource and .../mcp as two separate entries.
#
# A route's {param} is rewritten to a placeholder containing no slash, so it
# matches a wildcard segment but NOT a literal one. That is deliberate and
# conservative: a literal rule genuinely does not cover every value the
# parameter can take, so reporting it uncovered is the honest answer.
# ---------------------------------------------------------------------------

pattern_to_regex() {
  # Escape ERE metacharacters, then re-expand the wildcards.
  printf '%s' "$1" | sed '
    s/[][\.^$+?(){}|]/\\&/g
    s@/\*$@/__TRAILING__@
    s@\*@[^/]+@g
    s@/__TRAILING__@/.*@
  '
}

: > "$UNCOVERED"
while IFS= read -r route; do
  [ -n "$route" ] || continue
  # {id} -> a slash-free placeholder.
  probe="$(printf '%s' "$route" | sed 's/{[^}]*}/_PARAM_/g')"
  covered=0
  while IFS= read -r rule; do
    [ -n "$rule" ] || continue
    rx="^$(pattern_to_regex "$rule")\$"
    if printf '%s\n' "$probe" | grep -Eq "$rx"; then covered=1; break; fi
  done < "$RULES"
  [ "$covered" -eq 1 ] || printf '%s\n' "$route" >> "$UNCOVERED"
done < "$ROUTES_NORM"

# ---------------------------------------------------------------------------
# 5. Verdict.
# ---------------------------------------------------------------------------

STATUS=0
REAL_GAPS="$TMPDIR_RUN/gaps"
: > "$REAL_GAPS"
USED_ALLOW="$TMPDIR_RUN/used"
: > "$USED_ALLOW"

while IFS= read -r route; do
  [ -n "$route" ] || continue
  excused=0
  while IFS= read -r pat; do
    [ -n "$pat" ] || continue
    rx="^$(pattern_to_regex "$pat")\$"
    probe="$(printf '%s' "$route" | sed 's/{[^}]*}/_PARAM_/g')"
    if printf '%s\n' "$probe" | grep -Eq "$rx"; then
      excused=1; printf '%s\n' "$pat" >> "$USED_ALLOW"; break
    fi
  done < "$ALLOW_PATTERNS"
  [ "$excused" -eq 1 ] || printf '%s\n' "$route" >> "$REAL_GAPS"
done < "$UNCOVERED"

GAP_COUNT="$(wc -l < "$REAL_GAPS" | tr -d ' ')"
if [ "$GAP_COUNT" -gt 0 ]; then
  STATUS=1
  printf '\ncheck-urlmap-routes: %s route(s) mounted by the API are NOT routed to %s.\n' "$GAP_COUNT" "$API_BACKEND" >&2
  printf 'These fall through to the matcher default and are answered by the web SPA:\n' >&2
  printf 'a 200 text/html that looks like success and is not.\n\n' >&2
  sed 's/^/  MISSING  /' "$REAL_GAPS" >&2
  printf '\nFix: add each path to a pathRule in %s that points at %s.\n' "$URLMAP" "$API_BACKEND" >&2
  printf 'An exact path does not cover its subpaths — add /foo and /foo/* if both are served.\n' >&2
  printf 'If a route is deliberately not public, add it to %s WITH a "# reason:" line.\n\n' "$ALLOWLIST" >&2
fi

# A stale allowlist entry is a real failure: it means the guard is carrying an
# excuse for a route that no longer exists, which is how an ignore list grows
# until it excuses something that matters.
STALE=0
while IFS= read -r pat; do
  [ -n "$pat" ] || continue
  if ! grep -qxF "$pat" "$USED_ALLOW" 2>/dev/null; then
    if [ "$STALE" -eq 0 ]; then
      printf 'check-urlmap-routes: stale allowlist entries in %s — they excuse no live route:\n' "$ALLOWLIST" >&2
    fi
    STALE=1; STATUS=1
    printf '  STALE  %s\n' "$pat" >&2
  fi
done < "$ALLOW_PATTERNS"
[ "$STALE" -eq 0 ] || printf 'Remove them: the route they were added for is gone.\n\n' >&2

ALLOW_COUNT="$(wc -l < "$ALLOW_PATTERNS" | tr -d ' ')"
if [ "$STATUS" -eq 0 ]; then
  printf 'check-urlmap-routes: OK — %s mounted route(s) checked against %s path rule(s) in %s; %s deliberately unrouted.\n' \
    "$ROUTE_COUNT" "$RULE_COUNT" "$MATCHER" "$ALLOW_COUNT"
fi
exit "$STATUS"
