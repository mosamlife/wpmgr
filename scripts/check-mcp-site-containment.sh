#!/usr/bin/env bash
# scripts/check-mcp-site-containment.sh
#
# ADR-061 A11 item 4: the containment test.
#
#   "No handler on this surface may take a site id from a request and pass it
#    anywhere but the chokepoint."
#
# The chokepoint is Repo.ResolveScopeSites (apps/api/internal/mcp/repo.go),
# documented there as "the ONE audited chokepoint of m124 obligation 2". It
# resolves a grant's stored scope into real site ids INSIDE InTenantTx, so
# `sites` RLS drops every foreign UUID -- scope_site_ids is a uuid[] and
# PostgreSQL has no foreign key over array elements, so that column accepts any
# UUID in the world, including another organisation's site id.
#
# WHAT WAS MISSING. The chokepoint existed; nothing stopped the NEXT handler
# going round it. ADR-060's freeze clause calls site scoping an auth-boundary
# item and forbids new externally-reachable surface while one is open, and the
# MCP endpoint shipped anyway. This guard is what closes the item, so it has to
# hold rather than merely exist.
#
# WHY THIS AND NOT A GREP FOR A FUNCTION NAME. A grep for "ResolveScopeSites"
# proves the string is present. It cannot see the handler that never calls it,
# which is the entire failure mode. This guard inverts that: it enumerates the
# ways a site id could reach the database on this surface, and requires each one
# to be in a committed, reasoned allowlist that is checked in BOTH directions.
# Default-closed. New surface is red until a human writes down why it is safe.
#
# THE THREE RULES.
#
#   A. CHOKEPOINT CALL SITES. Every non-test call to ResolveScopeSites or
#      NewSiteSet under apps/api is identified by file + enclosing function and
#      must appear in the allowlist. NewSiteSet is included because it is the
#      chokepoint's second half: a SiteSet built from unresolved ids (a request
#      body, a cache, the grant column read directly) is a resolution that never
#      happened, and its own doc comment says so.
#
#   B. THE STORE INTERFACE'S uuid SURFACE. Package mcp reaches the database
#      through exactly one interface, mcp.Store. Every uuid.UUID / []uuid.UUID
#      parameter on it must be allowlisted BY NAME -- not only the ones spelled
#      "site". A bypass does not announce itself:
#
#          GetSiteByID(ctx context.Context, tenantID, id uuid.UUID) (...)
#
#      is a request-supplied site id reaching the database, and a pattern
#      hunting for /site/i matches nothing in it. Requiring an entry for EVERY
#      uuid parameter is what makes the guard survive a bypass written by
#      someone who is not trying to be caught, and, more to the point, by
#      someone who has never read this file.
#
#      The interface is read with `go doc -u`, i.e. through Go's own parser,
#      NOT by grepping repo.go. A signature reformatted across lines and a
#      method moved to another file in the package are both invisible to this
#      guard by construction, which a grep over one file cannot manage.
#
#      `go doc` PARSES; IT DOES NOT TYPE-CHECK, and that was measured rather
#      than assumed. With a bypass planted in this tree -- a Store method with
#      no implementation on Repo, a handler calling a Service method that does
#      not exist -- `go build ./internal/mcp` exits 1 with three errors and
#      `go doc -u ./internal/mcp Store` exits 0 and prints the interface. That
#      is the behaviour this guard wants: it fires on a bypass IN PROGRESS,
#      before the branch compiles, rather than waiting for it to be finished.
#      It also means a non-compiling package is not by itself an exit-2 state
#      here; only a `go doc` that fails or prints something unrecognisable is.
#
#   C. THE TOOL ARGUMENT SURFACE. mcp.toolInvoker's last parameter is the
#      request-supplied tool arguments (`args json.RawMessage`). Phase 1's one
#      tool discards them (`_ json.RawMessage`) and therefore cannot name a
#      site at all. Any file that BINDS that parameter to a name is a file where
#      request-supplied arguments start being read, and must be allowlisted.
#      That is the exact moment A11's sentence becomes possible to violate, so
#      it is the moment a reviewer is made to look.
#
# WHAT THIS CANNOT CATCH -- read this before trusting it. See the block at the
# bottom of this header.
#
# WHY IT IS A SCRIPT AND NOT A CI STEP. Build-gating logic in a YAML block
# scalar is untested logic: nobody can run it, so nobody can check their work.
# scripts/check-mcp-site-containment_test.sh drives this file with fixture
# inputs and asserts exit codes, so a hole that gets reopened turns a test red.
# Same reasoning, and the same shape, as check-version-surfaces.sh,
# check-license-surfaces.sh and check-urlmap-routes.sh.
#
# RUN IT:
#   make check-mcp-containment        # reads the Store interface via go doc
#   make check-mcp-containment-test   # the guard's own regression suite
#   scripts/check-mcp-site-containment.sh --store-doc /tmp/store.txt   # offline
#
# EXIT CODES. There is deliberately no exit code that means "checked nothing,
# fine":
#   0  every site-scope surface in the tree is allowlisted, and every allowlist
#      entry still matches something.
#   1  a real containment violation, or a stale allowlist entry.
#   2  GUARD BROKEN -- missing input, unreadable file, a source that produced
#      nothing, a control pattern that no longer matches. A guard whose input
#      vanished must go red; reporting a clean bill of health from an extraction
#      that produced zero rows is this project's signature defect.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools, so it behaves the
# same on a darwin laptop with BSD grep/sed/awk and on the ubuntu CI runner with
# the GNU ones. No mapfile, no associative arrays, no sed -i, no grep -P.
#
# ---------------------------------------------------------------------------
# WHAT THIS GUARD CANNOT CATCH
#
#   1. It is a STRUCTURAL guard, not a taint analysis. It proves that every
#      channel by which a site id could reach the database has been looked at
#      by a human, and that no new one appeared unnoticed. It does not prove the
#      human looked correctly. An allowlist entry with a wrong reason passes.
#
#   2. Rule B's subject is the mcp.Store interface. A handler that reaches the
#      database WITHOUT going through Store -- a second interface, a *db.Pool
#      held directly, an sqlc.Queries built in a handler -- is outside its
#      reach. Rule D below is a partial backstop: it refuses a direct sqlc or
#      pgx import in any file of package mcp other than repo.go, which is where
#      such a second path would have to start. It is a backstop and not a proof:
#      a new Repo method wired through a NEW interface declared elsewhere in the
#      package would satisfy both rules.
#
#   3. Rule C's granularity is the FILE, not the tool. Once one file is
#      allowlisted for reading tool arguments, a second tool added to that same
#      file inherits the allowance. The allowlist reason is therefore a promise
#      about the file, and a reviewer adding a tool to an allowlisted file gets
#      no signal from this guard.
#
#   4. It checks the mcp package only, because A11 is about this surface. A site
#      id taken from a request by some OTHER package and handed to mcp is not in
#      scope here.
#
#   5. It is a compile-time, source-level check. It says nothing about whether
#      the chokepoint's own SQL is correct; that is the integration proof's job
#      (A11 item 6), and this guard is not a substitute for it.
# ---------------------------------------------------------------------------

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"

API_ROOT="$REPO_ROOT/apps/api"
MCP_PKG_REL="internal/mcp"
ALLOWLIST="$REPO_ROOT/infra/mcp-site-containment-allowlist.txt"
STORE_DOC=""
STORE_DOC_CMD_DEFAULT="${WPMGR_MCP_STORE_DOC_CMD:-go doc -u ./internal/mcp Store}"

# The chokepoint, and the SiteSet constructor that is its second half. Named
# once, here, so the two rules and the error messages cannot drift apart.
CHOKEPOINT="ResolveScopeSites"
SITESET_CTOR="NewSiteSet"

usage() {
  printf '%s\n' \
    'usage: check-mcp-site-containment.sh [options]' \
    '' \
    '  --api-root DIR     apps/api tree to check (default: <repo>/apps/api)' \
    '  --allowlist FILE   reviewed site-scope surface, with reasons' \
    "                     (default: infra/mcp-site-containment-allowlist.txt)" \
    '  --store-doc FILE   read the mcp.Store interface from FILE instead of' \
    '                     running `go doc` (the self-test uses this; so can you,' \
    "                     on a machine with no Go toolchain)" \
    '  -h, --help         this text' \
    '' \
    'exit 0 = every site-scope surface is allowlisted and every entry is live' \
    'exit 1 = a containment violation, or a stale allowlist entry' \
    'exit 2 = GUARD BROKEN (missing input, empty extraction, control pattern gone)'
}

# An option with no value is exit 2, not a hang. `shift 2` with one argument
# left FAILS and shifts nothing, so `$1` is still the option and the loop
# spins forever -- a CI job that never finishes and never reports, which is the
# worst of the three outcomes because it looks like a slow runner rather than a
# broken guard. Checked before the assignment, so the arity error is reported
# rather than a default being silently substituted for the missing value.
need_value() {
  [ "$2" -ge 2 ] || {
    printf 'check-mcp-site-containment: %s requires a value\n' "$1" >&2
    usage >&2
    exit 2
  }
}

while [ $# -gt 0 ]; do
  case "$1" in
    --api-root)   need_value --api-root  $#; API_ROOT="$2";  shift 2 ;;
    --allowlist)  need_value --allowlist $#; ALLOWLIST="$2"; shift 2 ;;
    --store-doc)  need_value --store-doc $#; STORE_DOC="$2"; shift 2 ;;
    -h|--help)    usage; exit 0 ;;
    *) printf 'check-mcp-site-containment: unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

MCP_DIR="$API_ROOT/$MCP_PKG_REL"

# GUARD BROKEN is exit 2 and says so in those words, so that a reader of a CI
# log can tell "the boundary is violated" from "this check no longer works".
broken() { printf 'check-mcp-site-containment: GUARD BROKEN: %s\n' "$1" >&2; exit 2; }

VIOLATIONS=0
violation() {
  VIOLATIONS=$((VIOLATIONS + 1))
  printf 'check-mcp-site-containment: VIOLATION: %s\n' "$1" >&2
}

[ -d "$API_ROOT" ] || broken "api root does not exist: $API_ROOT"
[ -d "$MCP_DIR" ]  || broken "the mcp package is not at $MCP_DIR -- moved or renamed, which is not 'no violations'"
[ -f "$ALLOWLIST" ] || broken "allowlist not found: $ALLOWLIST"
[ -r "$ALLOWLIST" ] || broken "allowlist is not readable: $ALLOWLIST"

TMPDIR_RUN="$(mktemp -d 2>/dev/null)" || broken "could not create a temp dir"
trap 'rm -rf "$TMPDIR_RUN"' EXIT INT TERM

ALLOW_CALL="$TMPDIR_RUN/allow.call"
ALLOW_PARAM="$TMPDIR_RUN/allow.param"
ALLOW_TOOLARGS="$TMPDIR_RUN/allow.toolargs"
FOUND_CALL="$TMPDIR_RUN/found.call"
FOUND_PARAM="$TMPDIR_RUN/found.param"
FOUND_TOOLARGS="$TMPDIR_RUN/found.toolargs"
: >"$ALLOW_CALL"; : >"$ALLOW_PARAM"; : >"$ALLOW_TOOLARGS"
: >"$FOUND_CALL"; : >"$FOUND_PARAM"; : >"$FOUND_TOOLARGS"

# ---------------------------------------------------------------------------
# 0. The allowlist.
#
# Parsed into three sorted sets. An unrecognised KIND is exit 2 and not a
# warning: a typo'd kind silently removes an entry from the comparison, which
# turns a reviewed surface back into an unreviewed one with nothing said.
# ---------------------------------------------------------------------------
ALLOW_LINES=0
while IFS= read -r line || [ -n "$line" ]; do
  # Strip the reason column, then trailing whitespace.
  rec="${line%%#*}"
  rec="$(printf '%s' "$rec" | sed 's/[[:space:]]*$//')"
  [ -n "$rec" ] || continue
  ALLOW_LINES=$((ALLOW_LINES + 1))
  kind="$(printf '%s' "$rec" | awk '{print $1}')"
  case "$kind" in
    CALL)
      value="$(printf '%s' "$rec" | awk '{print $2" "$3}')"
      case "$value" in
        *" ") broken "malformed CALL entry in $ALLOWLIST (needs '<path> <func>'): $line" ;;
      esac
      printf '%s\n' "$value" >>"$ALLOW_CALL"
      ;;
    PARAM)
      value="$(printf '%s' "$rec" | awk '{print $2}')"
      case "$value" in
        *.*) : ;;
        *) broken "malformed PARAM entry in $ALLOWLIST (needs '<Method>.<param>'): $line" ;;
      esac
      printf '%s\n' "$value" >>"$ALLOW_PARAM"
      ;;
    TOOLARGS)
      printf '%s\n' "$(printf '%s' "$rec" | awk '{print $2}')" >>"$ALLOW_TOOLARGS"
      ;;
    *)
      broken "unrecognised allowlist kind '$kind' in $ALLOWLIST: $line"
      ;;
  esac
done <"$ALLOWLIST"

# An empty allowlist is not a pass. The chokepoint has call sites in HEAD and
# the Store interface has uuid parameters in HEAD; a file that lists none of
# them is a file that was truncated, not a tree that is clean.
[ "$ALLOW_LINES" -gt 0 ] || broken "allowlist $ALLOWLIST contains no entries -- truncated or emptied, which is not 'nothing to allow'"

sort -u "$ALLOW_CALL"     -o "$ALLOW_CALL"
sort -u "$ALLOW_PARAM"    -o "$ALLOW_PARAM"
sort -u "$ALLOW_TOOLARGS" -o "$ALLOW_TOOLARGS"

# ---------------------------------------------------------------------------
# RULE A. Chokepoint and SiteSet call sites.
#
# Identity is <path relative to apps/api> <enclosing func>. Line numbers are
# deliberately excluded: an allowlist that goes stale on every unrelated edit
# above the call gets switched off, and a switched-off guard guards nothing.
#
# The enclosing function is found by walking the file, not by regexing the call
# line, so a call inside a closure is attributed to the top-level function that
# contains it -- which is the unit a reviewer reads anyway.
#
# `_test.go` is excluded. Tests construct SiteSets from literal ids constantly
# and that is what a test is for; a rule that reddened them would be switched
# off inside a week. It is also the guard's largest honest gap and is named as
# such in the header.
# ---------------------------------------------------------------------------
GO_FILES="$TMPDIR_RUN/gofiles"
find "$API_ROOT" -name '*.go' -type f 2>/dev/null | grep -v '_test\.go$' | sort >"$GO_FILES"
[ -s "$GO_FILES" ] || broken "no non-test .go files found under $API_ROOT -- the tree moved, which is not 'no call sites'"

# Control. If neither name exists in the tree at all, the two rules below are
# measuring nothing and would report a clean surface. The chokepoint is
# DEFINED in HEAD, so zero occurrences means it was renamed or deleted.
if ! grep -q "func (r \*Repo) $CHOKEPOINT(" "$MCP_DIR/repo.go" 2>/dev/null; then
  broken "$CHOKEPOINT is not defined in $MCP_PKG_REL/repo.go -- renamed, moved or deleted. Rules A and B measure nothing until this is corrected."
fi
if ! grep -q "func $SITESET_CTOR(" "$MCP_DIR/model.go" 2>/dev/null; then
  broken "$SITESET_CTOR is not defined in $MCP_PKG_REL/model.go -- renamed, moved or deleted. Rule A measures nothing until this is corrected."
fi

while IFS= read -r f; do
  rel="${f#"$API_ROOT"/}"
  awk -v rel="$rel" -v chk="$CHOKEPOINT" -v ctor="$SITESET_CTOR" '
    /^func / {
      fn = $0
      sub(/^func +/, "", fn)
      sub(/^\([^)]*\) */, "", fn)     # strip the receiver
      sub(/[ (].*$/, "", fn)          # keep the identifier
      cur = fn
      next
    }
    {
      # The call forms. `.Chokepoint(` is the method-call form and excludes the
      # declaration and the interface line, exactly as ADR-061 Decision 4
      # spells out for InScopedTenantTx. The constructor has no receiver, so it
      # is matched bare and its own `func NewSiteSet(` declaration is excluded
      # by the /^func / branch above having already consumed that line.
      if (index($0, "." chk "(") > 0 || index($0, ctor "(") > 0) {
        if (cur == "") cur = "(file scope)"
        print rel " " cur
      }
    }
  ' "$f" >>"$FOUND_CALL"
done <"$GO_FILES"

sort -u "$FOUND_CALL" -o "$FOUND_CALL"

# Zero call sites is GUARD BROKEN and not a pass. The control greps above prove
# both functions are DEFINED; if nothing CALLS them, either the surface was
# rewritten wholesale or this extraction stopped working, and both need a human.
[ -s "$FOUND_CALL" ] || broken "found zero calls to $CHOKEPOINT / $SITESET_CTOR under $API_ROOT, yet both are defined -- the extraction produced nothing and cannot report containment"

while IFS= read -r entry; do
  if ! grep -qxF "$entry" "$ALLOW_CALL"; then
    set -- $entry
    violation "unreviewed site-scope call site: $MCP_PKG_REL-adjacent file '$1', function '$2'
    Rule A (ADR-061 A11.1): every call to $CHOKEPOINT or $SITESET_CTOR is part of
    the site-scope boundary and must be reviewed. If this call resolves a scope
    that came from the REQUEST rather than from the stored grant, it is the
    violation A11 item 4 exists to catch -- a site id the client chose being
    turned into an allowed set. If it is legitimate, add to $ALLOWLIST:
        CALL $1 $2   # why this is not a request-supplied site id"
  fi
done <"$FOUND_CALL"

while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! grep -qxF "$entry" "$FOUND_CALL"; then
    violation "stale allowlist entry: CALL $entry matches no call site in the tree.
    A stale entry means the guard is watching a surface that has moved. Delete
    it, or correct it to name where the call went."
  fi
done <"$ALLOW_CALL"

# ---------------------------------------------------------------------------
# RULE B. Every uuid parameter on the mcp.Store interface.
#
# Read through Go's own parser, after the package type-checks, rather than by
# grepping repo.go: a wrapped signature, a method moved to another file in the
# package, or a package that stopped compiling must not be able to make this
# rule quietly match less.
# ---------------------------------------------------------------------------
STORE_DOC_FILE="$TMPDIR_RUN/store.doc"
if [ -n "$STORE_DOC" ]; then
  [ -f "$STORE_DOC" ] || broken "--store-doc file does not exist: $STORE_DOC"
  [ -r "$STORE_DOC" ] || broken "--store-doc file is not readable: $STORE_DOC"
  cat "$STORE_DOC" >"$STORE_DOC_FILE"
else
  # A DoD step that cannot find its binary fails loudly, never skips.
  command -v go >/dev/null 2>&1 || broken "the go toolchain is not on PATH, so the Store interface cannot be read. Install Go, or pass --store-doc FILE. This check is NOT skippable."
  ( cd "$API_ROOT" && GOWORK=off $STORE_DOC_CMD_DEFAULT ) >"$STORE_DOC_FILE" 2>"$TMPDIR_RUN/store.err"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf 'check-mcp-site-containment: `%s` failed (exit %d) in %s:\n' "$STORE_DOC_CMD_DEFAULT" "$rc" "$API_ROOT" >&2
    sed 's/^/    /' "$TMPDIR_RUN/store.err" >&2
    broken "could not read the mcp.Store interface. A package that does not compile is not a package with no violations."
  fi
fi

[ -s "$STORE_DOC_FILE" ] || broken "the mcp.Store interface extraction produced NO OUTPUT. Rule B cannot report containment on an empty interface."
grep -q 'type Store interface' "$STORE_DOC_FILE" \
  || broken "the extraction does not contain 'type Store interface' -- the interface was renamed, or the doc format changed. Rule B is measuring nothing."
grep -q "$CHOKEPOINT(" "$STORE_DOC_FILE" \
  || broken "the mcp.Store interface no longer declares $CHOKEPOINT. Either the chokepoint left the interface, or the extraction is wrong; both mean Rule B's baseline is gone."

# Method lines: one tab, an upper-case identifier, an open paren. Doc comments
# in the extraction start `\t//`, blank lines separate, and the closing brace is
# at column 0, so this selects declarations and nothing else.
METHODS="$TMPDIR_RUN/store.methods"
grep -E '^	[A-Z][A-Za-z0-9_]*\(' "$STORE_DOC_FILE" | sed 's/^	//' >"$METHODS"
[ -s "$METHODS" ] || broken "parsed ZERO methods out of the mcp.Store interface. The doc format changed; Rule B would pass on any tree at all in this state."

# For each method, every parameter binding whose type is uuid.UUID or
# []uuid.UUID. Go groups names: `tenantID, grantID, tokenID uuid.UUID` binds
# three. Each is emitted as Method.name.
#
# THE PARSE IS SEGMENT-BASED AND NOT A REGEX OVER THE WHOLE SIGNATURE. The
# obvious regex -- `<name>( *, *<name>)* +uuid.UUID` -- matched the TYPE half of
# a qualified name: in `ctx context.Context, tenantID uuid.UUID` it started at
# `Context` and reported a parameter called `Context`. It reported `string` out
# of `mode string, tagIDs, siteIDs []uuid.UUID` for the same reason. Both are
# noise that a reviewer would allowlist to make the guard quiet, and a guard
# that has been made quiet is off. So: cut the parameter list at its matching
# paren, flatten nested func(...) types into the same comma-separated stream,
# and walk the segments the way the Go grammar reads them -- bare segments are
# names sharing the type of the segment that terminates the group.
awk '
  function emit_group(m, acc, n, i, names) {
    n = split(acc, names, "\001")
    for (i = 1; i <= n; i++) if (names[i] != "") print m "." names[i]
  }
  {
    line = $0
    m = line
    sub(/\(.*$/, "", m)                       # method name

    # 1. The parameter list, cut at the paren that closes it.
    start = index(line, "(")
    if (start == 0) next
    depth = 0; params = ""
    for (i = start; i <= length(line); i++) {
      c = substr(line, i, 1)
      if (c == "(") { depth++; if (depth == 1) continue }
      else if (c == ")") { depth--; if (depth == 0) break }
      params = params c
    }
    if (params == "") next

    # 2. Flatten nested parens so callback parameters -- `mkCode func(grantID
    #    uuid.UUID) sqlc.X` -- are walked too. They are part of the reviewed
    #    surface: a callback taking a site id is a site id crossing the
    #    boundary in the other direction.
    gsub(/[()]/, ",", params)

    # 3. Walk the segments.
    n = split(params, segs, ",")
    acc = ""
    for (i = 1; i <= n; i++) {
      s = segs[i]
      gsub(/^[ \t]+|[ \t]+$/, "", s)
      if (s == "") continue
      if (s ~ /^[A-Za-z_][A-Za-z0-9_]*$/) {
        # A bare identifier: either a grouped name awaiting its type, or an
        # unnamed parameter whose type happens to be unqualified. Carrying it
        # is the safe direction -- a spurious entry is visible, a dropped one
        # is not.
        acc = (acc == "" ? s : acc "\001" s)
        continue
      }
      # `name type`. Only the first two tokens matter.
      nm = s; ty = s
      sub(/[ \t].*$/, "", nm)
      sub(/^[^ \t]+[ \t]+/, "", ty)
      if (ty == "uuid.UUID" || ty == "[]uuid.UUID") {
        if (nm ~ /^[A-Za-z_][A-Za-z0-9_]*$/ && nm !~ /\./) {
          acc = (acc == "" ? nm : acc "\001" nm)
          emit_group(m, acc)
        }
      }
      acc = ""
    }
  }
' "$METHODS" | sort -u >"$FOUND_PARAM"

# Zero uuid parameters on an interface that HEAD declares with many is an
# extraction failure, not a clean surface.
[ -s "$FOUND_PARAM" ] || broken "found ZERO uuid parameters on mcp.Store. HEAD declares several, so this is a parse failure, and a parse failure that reports containment is the defect this guard exists to prevent."

while IFS= read -r entry; do
  if ! grep -qxF "$entry" "$ALLOW_PARAM"; then
    meth="${entry%%.*}"
    parm="${entry#*.}"
    loc="$(grep -n "$meth(" "$MCP_DIR/repo.go" 2>/dev/null | head -1 | cut -d: -f1)"
    [ -n "$loc" ] || loc="?"
    violation "unreviewed uuid parameter on the mcp.Store database boundary:
    $MCP_PKG_REL/repo.go:$loc  $meth(... $parm ...)
    Rule B (ADR-061 A11.4): '$parm' is a uuid this surface hands to the database.
    If it is a SITE ID that came from a request, this is the bypass: A11 says no
    handler may pass a request-supplied site id anywhere but $CHOKEPOINT, and a
    Store method taking one goes round it. Route it through $CHOKEPOINT and
    filter with SiteSet.Allows. If it is not a site id, add to $ALLOWLIST:
        PARAM $entry   # what this id is and where it comes from"
  fi
done <"$FOUND_PARAM"

while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! grep -qxF "$entry" "$FOUND_PARAM"; then
    violation "stale allowlist entry: PARAM $entry is not on the mcp.Store interface.
    The method or the parameter was renamed or removed. Delete the entry, or
    correct it -- an allowlist naming things that no longer exist is not
    protecting the things that do."
  fi
done <"$ALLOW_PARAM"

# ---------------------------------------------------------------------------
# RULE C. The tool argument surface.
#
# toolInvoker's last parameter is the request's own tool arguments. Phase 1's
# single tool discards it. The first file that binds it to a name is the first
# file where a client-chosen site id can enter, and A11 item 4's sentence
# becomes violable there and nowhere earlier.
# ---------------------------------------------------------------------------
INVOKER_DECL='type toolInvoker func('
grep -qF "$INVOKER_DECL" "$MCP_DIR/registry.go" 2>/dev/null \
  || broken "the toolInvoker declaration is not in $MCP_PKG_REL/registry.go. Rule C's subject moved or was renamed; it is matching nothing."

# Bind = the args parameter has a name. `_ json.RawMessage` is a discard and is
# the compliant form. The type declaration itself names the parameter and is
# excluded, since a type is not a handler.
find "$MCP_DIR" -name '*.go' -type f 2>/dev/null | grep -v '_test\.go$' | sort >"$TMPDIR_RUN/mcpfiles"
[ -s "$TMPDIR_RUN/mcpfiles" ] || broken "no non-test .go files in $MCP_DIR -- the package moved, which is not 'no tool arguments'"

while IFS= read -r f; do
  rel="${f#"$API_ROOT"/}"
  # `_ json.RawMessage` is the compliant discard and must NOT match. A single
  # underscore is a legal Go identifier, so `[A-Za-z_][A-Za-z0-9_]*` matched it
  # and reported the one compliant tool in HEAD as a violation. The alternation
  # below admits `_foo` (a real, if unusual, binding) and excludes bare `_`.
  grep -nE 'auth AuthorizedRequest,[ ]*(_[A-Za-z0-9_]+|[A-Za-z][A-Za-z0-9_]*)[ ]+json\.RawMessage' "$f" 2>/dev/null \
    | grep -v '^[0-9]*:type ' \
    | while IFS= read -r hit; do
        printf '%s\n' "$rel"
      done
done <"$TMPDIR_RUN/mcpfiles" | sort -u >"$FOUND_TOOLARGS"

# NOTE: an empty result here is CORRECT and is not GUARD BROKEN -- the compliant
# state of this rule is zero matches. That is why the control grep above (the
# toolInvoker declaration must exist) is load-bearing: it is what separates
# "no tool reads its arguments" from "this rule stopped matching anything".

while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! grep -qxF "$entry" "$ALLOW_TOOLARGS"; then
    violation "unreviewed tool-argument surface: $entry binds toolInvoker's
    request arguments to a name, so a client can now put values in them.
    Rule C (ADR-061 A11.4): if any of those values is a site id, it MUST reach
    $CHOKEPOINT and be filtered through the resolved SiteSet -- it must never be
    handed to the database or compared against a grant column directly. Review
    it, then add to $ALLOWLIST:
        TOOLARGS $entry   # which arguments this file reads, and how site ids in them are contained"
  fi
done <"$FOUND_TOOLARGS"

while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! grep -qxF "$entry" "$FOUND_TOOLARGS"; then
    violation "stale allowlist entry: TOOLARGS $entry no longer reads tool arguments.
    Delete the entry so the file goes back to being default-closed."
  fi
done <"$ALLOW_TOOLARGS"

# ---------------------------------------------------------------------------
# RULE D. The backstop named in gap 2 of the header.
#
# Rule B's subject is the Store interface. A file in package mcp that imports
# the sqlc package or pgx directly is a file that could talk to the database
# without going through Store at all, which is where a second, unwatched path
# would begin. repo.go is the one file whose job that is.
# ---------------------------------------------------------------------------
while IFS= read -r f; do
  rel="${f#"$API_ROOT"/}"
  case "$rel" in
    "$MCP_PKG_REL/repo.go") continue ;;
  esac
  if grep -qE '"github\.com/jackc/pgx/v5"' "$f" 2>/dev/null; then
    # pgx types appear in callback signatures the Store interface hands out
    # (onCreated func(tx pgx.Tx, ...)), so an import alone is not a finding
    # unless the file also builds its own queries.
    if grep -qE 'sqlc\.New\(' "$f" 2>/dev/null; then
      violation "$rel builds its own sqlc.Queries.
    Rule D (backstop for ADR-061 A11.4): package mcp reaches the database
    through the Store interface, which is what Rule B watches. A second path
    built here is outside that watch, and a site id could travel it. Move the
    query behind a Store method so it is covered."
    fi
  fi
done <"$TMPDIR_RUN/mcpfiles"

# ---------------------------------------------------------------------------
# Report.
# ---------------------------------------------------------------------------
n_call="$(grep -c . "$FOUND_CALL")"
n_param="$(grep -c . "$FOUND_PARAM")"
n_toolargs="$(grep -c . "$FOUND_TOOLARGS")"

if [ "$VIOLATIONS" -gt 0 ]; then
  printf '\ncheck-mcp-site-containment: FAILED with %d violation(s).\n' "$VIOLATIONS" >&2
  printf 'ADR-061 A11 item 4: no handler on the assistant surface may take a site id\n' >&2
  printf 'from a request and pass it anywhere but %s.\n' "$CHOKEPOINT" >&2
  printf 'Allowlist: %s\n' "$ALLOWLIST" >&2
  exit 1
fi

printf 'check-mcp-site-containment: OK\n'
printf '  Rule A  %s chokepoint / %s call site(s), all reviewed\n' "$n_call" "$SITESET_CTOR"
printf '  Rule B  %s uuid parameter(s) on the mcp.Store boundary, all reviewed\n' "$n_param"
printf '  Rule C  %s file(s) reading tool arguments (0 is the Phase 1 state)\n' "$n_toolargs"
printf '  Rule D  no second database path in package mcp\n'
exit 0
