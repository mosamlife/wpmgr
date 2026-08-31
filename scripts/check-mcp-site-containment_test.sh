#!/usr/bin/env bash
# scripts/check-mcp-site-containment_test.sh
#
# The regression suite for scripts/check-mcp-site-containment.sh.
#
# WHY THIS EXISTS. A guard nobody has watched fail is not known to guard
# anything. ADR-061 A11 item 4 asks for a containment test that fails CI, and
# "fails CI" is a claim about behaviour, so it is asserted here as behaviour:
# every case below plants a real bypass in a fixture tree and asserts the exit
# code and the message.
#
# THE THREE FAMILIES OF CASE, and the third is the one that gets skipped:
#
#   bypass-*   A real containment violation. Exit 1.
#   broken-*   The guard's own input is missing, empty, moved or unparseable.
#              Exit 2, never 0. This project's signature defect is announcing
#              success over its own errors, and a containment check that
#              silently matches zero files is exactly that shape, so each of
#              those states is planted rather than reasoned about.
#   ok-*       Honest work the guard must NOT redden. A guard that fails
#              correct work gets switched off, and then it guards nothing.
#
# THE FIXTURE TREE IS HERMETIC. It needs no Go toolchain, no database and no
# network: the mcp.Store interface is supplied through --store-doc in exactly
# the format `go doc -u` emits, and the Go files are the few lines the guard's
# control patterns read. That is deliberate — the suite must be runnable on a
# machine where the real check cannot run, or it will not be run.
#
# RUN IT:
#   make check-mcp-containment-test
#   scripts/check-mcp-site-containment_test.sh            # everything
#   scripts/check-mcp-site-containment_test.sh bypass     # only matching cases
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_MCP_CONTAINMENT_GUARD=/tmp/guard-with-hole.sh \
#     scripts/check-mcp-site-containment_test.sh
#
# PORTABILITY. bash 3.2 and POSIX tools; no mapfile, no associative arrays,
# no sed -i, no grep -P.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_MCP_CONTAINMENT_GUARD:-$HERE/check-mcp-site-containment.sh}"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  printf 'check-mcp-site-containment_test: guard not found: %s\n' "$GUARD" >&2
  exit 2
fi

PASS=0
FAIL=0
WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

# --- fixtures ---------------------------------------------------------------

# A minimal apps/api tree carrying exactly what the guard's control patterns
# and Rules A, C and D read. $1 is the tree root; it is created fresh.
make_tree() {
  root="$1"
  mcp="$root/apps/api/internal/mcp"
  rm -rf "$root"
  mkdir -p "$mcp"

  # repo.go must actually reach the database. Rule D's control asserts that the
  # one file whose job that is still matches its patterns -- a fixture repo.go
  # that touched nothing would let stale patterns pass the control, which is
  # the failure the control exists to catch.
  printf '%s\n' \
    'package mcp' \
    '' \
    'import (' \
    '	"github.com/jackc/pgx/v5"' \
    '	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"' \
    ')' \
    '' \
    'func (r *Repo) ResolveScopeSites(ctx context.Context, tenantID uuid.UUID, mode string, tagIDs, siteIDs []uuid.UUID) ([]uuid.UUID, error) {' \
    '	return sqlc.New(tx).ResolveScope(ctx, tenantID)' \
    '}' \
    >"$mcp/repo.go"

  printf '%s\n' \
    'package mcp' \
    '' \
    'func NewSiteSet(ids []uuid.UUID) SiteSet {' \
    '	return SiteSet{}' \
    '}' \
    >"$mcp/model.go"

  printf '%s\n' \
    'package mcp' \
    '' \
    'type toolInvoker func(ctx context.Context, svc *Service, auth AuthorizedRequest, args json.RawMessage) (string, error)' \
    '' \
    'var listSites = toolEntry{' \
    '	invoke: func(ctx context.Context, svc *Service, auth AuthorizedRequest, _ json.RawMessage) (string, error) {' \
    '		return svc.ListSitesForModel(ctx, auth)' \
    '	},' \
    '}' \
    >"$mcp/registry.go"

  printf '%s\n' \
    'package mcp' \
    '' \
    'func (s *Service) Authenticate(ctx context.Context) (AuthorizedRequest, error) {' \
    '	ids, err := s.store.ResolveScopeSites(ctx, tok.TenantID, chk.SiteScopeMode, chk.ScopeTagIds, chk.ScopeSiteIds)' \
    '	_ = err' \
    '	return AuthorizedRequest{Sites: NewSiteSet(ids)}, nil' \
    '}' \
    >"$mcp/service.go"
}

# The mcp.Store interface in `go doc -u` format: a tab-indented method per
# line, doc comments as `\t//`. Extra methods are appended from "$@".
make_store_doc() {
  out="$1"; shift
  {
    printf 'package mcp // import "github.com/mosamlife/wpmgr/apps/api/internal/mcp"\n'
    printf '\n'
    printf 'type Store interface {\n'
    printf '\t// A doc comment, which must not be parsed as a method.\n'
    printf '\tRegisterClient(ctx context.Context, arg sqlc.RegisterMCPOAuthClientParams) (int64, error)\n'
    printf '\tReCheckAuthorization(ctx context.Context, tenantID, tokenID uuid.UUID) (sqlc.Row, error)\n'
    printf '\tResolveScopeSites(ctx context.Context, tenantID uuid.UUID, mode string, tagIDs, siteIDs []uuid.UUID) ([]uuid.UUID, error)\n'
    printf '\tListSitesForRead(ctx context.Context, tenantID uuid.UUID, limit int32) ([]sqlc.ListSitesRow, bool, error)\n'
    for extra in "$@"; do printf '\t%s\n' "$extra"; done
    printf '}\n'
  } >"$out"
}

# The allowlist that makes the fixture tree above clean. Extra records from "$@".
make_allow() {
  out="$1"; shift
  {
    printf '# fixture allowlist\n'
    printf 'CALL internal/mcp/service.go Authenticate   # resolves the stored grant scope\n'
    printf 'PARAM ReCheckAuthorization.tenantID   # tenant of the resolved principal\n'
    printf 'PARAM ReCheckAuthorization.tokenID    # token row id\n'
    printf 'PARAM ResolveScopeSites.tenantID      # the chokepoint itself\n'
    printf 'PARAM ResolveScopeSites.tagIDs        # chokepoint input\n'
    printf 'PARAM ResolveScopeSites.siteIDs       # chokepoint input\n'
    printf 'PARAM ListSitesForRead.tenantID       # tenant of the resolved principal\n'
    for extra in "$@"; do printf '%s\n' "$extra"; done
  } >"$out"
}

# --- harness ----------------------------------------------------------------

# run_case <name> <expected-exit> <must-contain|-> <must-not-contain|-> -- <args...>
run_case() {
  name="$1"; want="$2"; needle="$3"; anti="$4"; shift 5   # shift past the '--'
  case "$name" in
    *"$FILTER"*) : ;;
    *) return 0 ;;
  esac

  out="$WORK/out.$$"
  # NEVER pipe the guard: a pipeline reports the LAST command's status, and the
  # status is the entire assertion here.
  "$GUARD" "$@" >"$out" 2>&1
  got=$?

  ok=1
  [ "$got" = "$want" ] || ok=0
  # `--` before the pattern: a needle that starts with a dash (this guard has
  # `--store-doc` in one of its messages) is otherwise read by grep as an
  # option, and the case fails for a reason that has nothing to do with the
  # guard.
  if [ "$needle" != "-" ] && ! grep -qF -- "$needle" "$out"; then ok=0; fi
  if [ "$anti" != "-" ] && grep -qF -- "$anti" "$out"; then ok=0; fi

  if [ "$ok" = 1 ]; then
    PASS=$((PASS + 1))
    printf 'ok   %-46s exit %s\n' "$name" "$got"
  else
    FAIL=$((FAIL + 1))
    printf 'FAIL %-46s exit %s (want %s)\n' "$name" "$got" "$want"
    [ "$needle" = "-" ] || printf '       wanted output containing: %s\n' "$needle"
    [ "$anti" = "-" ]   || printf '       wanted output WITHOUT:    %s\n' "$anti"
    sed 's/^/       | /' "$out"
  fi
  rm -f "$out"
}

TREE="$WORK/tree"
DOC="$WORK/store.doc"
ALLOW="$WORK/allow.txt"

# ============================================================================
# ok-* : the compliant tree, and honest work that must not be reddened.
# ============================================================================

make_tree "$TREE"; make_store_doc "$DOC"; make_allow "$ALLOW"
run_case "ok-clean-tree" 0 "check-mcp-site-containment: OK" "VIOLATION" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

# A tool that DISCARDS its arguments is the compliant Phase 1 form. `_` is a
# legal Go identifier, and the first version of Rule C matched it and reported
# the one compliant tool in HEAD as a violation.
run_case "ok-tool-discards-its-args" 0 "Rule C  0 file(s)" "registry.go binds" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

# A new Store method that takes NO uuid is ordinary work and must pass with no
# allowlist edit at all.
make_store_doc "$DOC" 'CountGrants(ctx context.Context, since time.Time) (int64, error)'
run_case "ok-new-store-method-without-uuid" 0 "check-mcp-site-containment: OK" "VIOLATION" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

# Tests construct SiteSets from literal ids constantly. Reddening them would
# get the guard switched off, so _test.go is excluded by design.
make_store_doc "$DOC"
printf '%s\n' 'package mcp' 'func TestX(t *testing.T) { _ = NewSiteSet([]uuid.UUID{uuid.New()}) }' \
  >"$TREE/apps/api/internal/mcp/scope_test.go"
run_case "ok-test-files-may-build-sitesets" 0 "check-mcp-site-containment: OK" "scope_test.go" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/scope_test.go"

# A doc comment inside the interface must not be parsed as a method. The
# fixture carries one; this asserts it produced no phantom parameter.
# THE OVER-FIRE THAT WAS LIVE ON main. Rule C used to key on the literal
# `auth AuthorizedRequest,` followed by a named json.RawMessage, and matched
# transport.go's writeProtocolRefusal(c *gin.Context, auth AuthorizedRequest,
# id json.RawMessage, neg Negotiation, phase string) -- where the RawMessage is
# the JSON-RPC ENVELOPE ID being echoed back into an error response, not tool
# arguments. The only convenient way to quiet that would have been
# `TOOLARGS internal/mcp/transport.go`, and Rule C's granularity is the FILE,
# so that entry would have switched the rule off for the one file that
# actually dispatches tools. The rule now keys on the toolInvoker type
# sequence, which this signature does not have.
printf '%s\n' 'package mcp' \
  'func (h *TransportHandler) writeProtocolRefusal(' \
  '	c *gin.Context, auth AuthorizedRequest, id json.RawMessage, neg Negotiation, phase string,' \
  ') {' \
  '	_ = newErrorResponse(id, codeProtocolUnsupported, "", nil)' \
  '}' >"$TREE/apps/api/internal/mcp/transport.go"
run_case "ok-jsonrpc-envelope-id-is-not-tool-args" 0 "Rule C  0 file(s)" "transport.go binds" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/transport.go"

run_case "ok-doc-comments-are-not-methods" 0 "Rule B  6 uuid parameter(s)" "VIOLATION" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

# ============================================================================
# bypass-* : real containment violations. Exit 1.
# ============================================================================

# THE CANONICAL BYPASS. A new read tool wants one site, so a Store method takes
# the site id straight from the request. It never touches the chokepoint, so
# the uuid is never resolved under `sites` RLS and a foreign site id is
# accepted -- scope_site_ids is a uuid[] with no foreign key over its elements.
make_store_doc "$DOC" 'GetSiteByID(ctx context.Context, tenantID, siteID uuid.UUID) (sqlc.Site, error)'
run_case "bypass-store-method-takes-site-id" 1 "PARAM GetSiteByID.siteID" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

# THE SAME BYPASS, WEARING A NAME NO PATTERN WOULD MATCH. This is why Rule B
# requires an entry for EVERY uuid parameter rather than hunting for /site/i:
# `id` is what a bypass is actually called when nobody is trying to hide it.
make_store_doc "$DOC" 'GetSiteByID(ctx context.Context, tenantID, id uuid.UUID) (sqlc.Site, error)'
run_case "bypass-site-id-named-id" 1 "PARAM GetSiteByID.id" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

# A slice of site ids is the same bypass in bulk.
make_store_doc "$DOC" 'ListUpdatesForSites(ctx context.Context, tenantID uuid.UUID, targets []uuid.UUID) ([]sqlc.Row, error)'
run_case "bypass-store-method-takes-site-id-slice" 1 "PARAM ListUpdatesForSites.targets" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

# A tool that starts reading its request arguments is where a client-chosen
# site id can first enter. Rule C makes that moment red until reviewed.
make_store_doc "$DOC"
sed 's/_ json\.RawMessage/args json.RawMessage/' "$TREE/apps/api/internal/mcp/registry.go" >"$WORK/registry.new"
cp "$WORK/registry.new" "$TREE/apps/api/internal/mcp/registry.go"
run_case "bypass-tool-reads-request-arguments" 1 "internal/mcp/registry.go binds toolInvoker" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
make_tree "$TREE"

# A second, unreviewed call to the chokepoint. Not every such call is a bug --
# but every one of them is a place where something decides which sites a
# principal may see, and A11.1 says that set of places is enumerated.
printf '%s\n' 'package mcp' \
  'func (s *Service) GetSiteForModel(ctx context.Context, req Req) error {' \
  '	ids, _ := s.store.ResolveScopeSites(ctx, req.TenantID, "list", nil, req.SiteIDs)' \
  '	_ = ids' \
  '	return nil' \
  '}' >"$TREE/apps/api/internal/mcp/tools_get_site.go"
run_case "bypass-unreviewed-chokepoint-call" 1 "CALL internal/mcp/tools_get_site.go GetSiteForModel" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/tools_get_site.go"

# A SiteSet built from ids that were never resolved is a resolution that never
# happened. NewSiteSet's own doc comment says so; this makes it enforceable.
printf '%s\n' 'package mcp' \
  'func (s *Service) authFast(req Req) AuthorizedRequest {' \
  '	return AuthorizedRequest{Sites: NewSiteSet(req.SiteIDs)}' \
  '}' >"$TREE/apps/api/internal/mcp/fastpath.go"
run_case "bypass-siteset-from-unresolved-ids" 1 "CALL internal/mcp/fastpath.go authFast" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/fastpath.go"

# ---- Rules A and C must not be defeated by ordinary Go formatting ----------
#
# Every case below is VALID Go that gofmt is happy to leave alone, and every
# one of them slipped past the first version of these rules. A containment
# guard that a space defeats reports green while the property is unenforced,
# which is worse than having no guard at all.

# A space between the method name and its paren. Legal Go, and the old
# `index($0, ".ResolveScopeSites(")` test saw nothing.
printf '%s\n' 'package mcp' \
  'func (s *Service) spacedCall(ctx context.Context, req Req) error {' \
  '	ids, _ := s.store.ResolveScopeSites (ctx, req.TenantID, "list", nil, req.SiteIDs)' \
  '	_ = ids' \
  '	return nil' \
  '}' >"$TREE/apps/api/internal/mcp/spaced.go"
run_case "bypass-chokepoint-call-space-before-paren" 1 "CALL internal/mcp/spaced.go spacedCall" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/spaced.go"

printf '%s\n' 'package mcp' \
  'func (s *Service) spacedCtor(req Req) AuthorizedRequest {' \
  '	return AuthorizedRequest{Sites: NewSiteSet (req.SiteIDs)}' \
  '}' >"$TREE/apps/api/internal/mcp/spacedctor.go"
run_case "bypass-siteset-ctor-space-before-paren" 1 "CALL internal/mcp/spacedctor.go spacedCtor" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/spacedctor.go"

# The receiver dot at the end of one line, the call on the next. No single
# line contains ".ResolveScopeSites(" -- the old line-oriented matcher was
# blind to it, and the canonical calls elsewhere kept the extraction nonempty
# so it never even reached GUARD BROKEN.
printf '%s\n' 'package mcp' \
  'func (s *Service) splitCall(ctx context.Context, req Req) error {' \
  '	ids, _ := s.store.' \
  '		ResolveScopeSites(ctx, req.TenantID, "list", nil, req.SiteIDs)' \
  '	_ = ids' \
  '	return nil' \
  '}' >"$TREE/apps/api/internal/mcp/split.go"
run_case "bypass-chokepoint-call-split-across-lines" 1 "CALL internal/mcp/split.go splitCall" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/split.go"

# Rule C keyed on the parameter NAME `auth`. The name is the tool author's
# choice, so renaming it produced a conforming toolInvoker that reads its
# request arguments and was invisible to the guard.
printf '%s\n' 'package mcp' \
  'var renamed = toolEntry{' \
  '	invoke: func(ctx context.Context, svc *Service, req AuthorizedRequest, args json.RawMessage) (string, error) {' \
  '		return svc.Something(ctx, req, args)' \
  '	},' \
  '}' >"$TREE/apps/api/internal/mcp/tool_renamed.go"
run_case "bypass-tool-args-renamed-auth-parameter" 1 "internal/mcp/tool_renamed.go binds toolInvoker" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/tool_renamed.go"

# The same handler with the signature wrapped across lines, which is what
# gofmt does to a long one.
printf '%s\n' 'package mcp' \
  'var wrapped = toolEntry{' \
  '	invoke: func(' \
  '		ctx context.Context,' \
  '		svc *Service,' \
  '		auth AuthorizedRequest,' \
  '		args json.RawMessage,' \
  '	) (string, error) {' \
  '		return svc.Something(ctx, auth, args)' \
  '	},' \
  '}' >"$TREE/apps/api/internal/mcp/tool_wrapped.go"
run_case "bypass-tool-args-signature-wrapped" 1 "internal/mcp/tool_wrapped.go binds toolInvoker" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/tool_wrapped.go"

# NAMED RESULTS. `(out string, err error)` is the same Go type as
# `(string, error)` and is assignable to toolInvoker, but the pattern admitted
# a name on every PARAMETER and none on either RESULT. The handler was omitted
# from FOUND_TOOLARGS and the guard reported successful containment over a live
# request-input channel.
printf '%s\n' 'package mcp' \
  'var namedResults = toolEntry{' \
  '	invoke: func(ctx context.Context, svc *Service, auth AuthorizedRequest, args json.RawMessage) (out string, err error) {' \
  '		out, err = svc.Something(ctx, auth, args)' \
  '		return out, err' \
  '	},' \
  '}' >"$TREE/apps/api/internal/mcp/tool_named_results.go"
run_case "bypass-tool-args-named-results" 1 "internal/mcp/tool_named_results.go binds toolInvoker" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/tool_named_results.go"

# The same handler with named results AND the signature wrapped, so the result
# list picks up gofmt's trailing comma on both halves at once.
printf '%s\n' 'package mcp' \
  'var namedWrapped = toolEntry{' \
  '	invoke: func(' \
  '		ctx context.Context,' \
  '		svc *Service,' \
  '		auth AuthorizedRequest,' \
  '		args json.RawMessage,' \
  '	) (' \
  '		out string,' \
  '		err error,' \
  '	) {' \
  '		return out, err' \
  '	},' \
  '}' >"$TREE/apps/api/internal/mcp/tool_named_wrapped.go"
run_case "bypass-tool-args-named-results-wrapped" 1 "internal/mcp/tool_named_wrapped.go binds toolInvoker" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/tool_named_wrapped.go"

# A second database path inside package mcp, outside repo.go, is outside the
# reach of Rule B. Rule D is the backstop.
printf '%s\n' 'package mcp' \
  'import "github.com/jackc/pgx/v5"' \
  'func (s *Service) direct(tx pgx.Tx) { _ = sqlc.New(tx) }' \
  >"$TREE/apps/api/internal/mcp/shortcut.go"
run_case "bypass-second-database-path-in-package" 1 "builds its own sqlc.Queries" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/shortcut.go"

# ---- Rule D: the three ways round the old import-plus-literal test ----------

# Executes SQL with no sqlc.New anywhere. The old inner test required that
# literal, so this file passed and Rule D printed "no second database path"
# over a second database path.
printf '%s\n' 'package mcp' \
  'import "github.com/jackc/pgx/v5"' \
  'func (s *Service) rawRead(ctx context.Context, tx pgx.Tx, siteID uuid.UUID) error {' \
  '	_, err := tx.Query(ctx, "SELECT id FROM sites WHERE id = $1", siteID)' \
  '	return err' \
  '}' >"$TREE/apps/api/internal/mcp/rawsql.go"
run_case "bypass-direct-pgx-query-without-sqlc-new" 1 "executes SQL directly" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/rawsql.go"

# The same direct pgx call with the context argument on the next line -- what
# gofmt does to a long one. A line-oriented PGX_OPS_RE did not match it, the
# file was dropped from DBPATH_HITS, and the guard reported successful
# containment over a live database path.
printf '%s\n' 'package mcp' \
  'import "github.com/jackc/pgx/v5"' \
  'func (s *Service) rawSplit(ctx context.Context, tx pgx.Tx, siteID uuid.UUID) error {' \
  '	row := tx.QueryRow(' \
  '		ctx,' \
  '		"SELECT id FROM sites WHERE id = $1",' \
  '		siteID,' \
  '	)' \
  '	return row.Scan(&siteID)' \
  '}' >"$TREE/apps/api/internal/mcp/rawsplit.go"
run_case "bypass-direct-pgx-query-split-across-lines" 1 "executes SQL directly" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/rawsplit.go"

# sqlc.New takes a DBTX. Handed a *pgxpool.Pool, the file never imports the
# root pgx package, and the old OUTER grep skipped the constructor check
# entirely.
printf '%s\n' 'package mcp' \
  'import "github.com/jackc/pgx/v5/pgxpool"' \
  'func (s *Service) poolPath(pool *pgxpool.Pool) {' \
  '	_ = sqlc.New(pool)' \
  '}' >"$TREE/apps/api/internal/mcp/poolpath.go"
run_case "bypass-sqlc-new-with-pgxpool-dbtx" 1 "builds its own sqlc.Queries" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/poolpath.go"

# The same constructor reached through an import alias. Keying on the literal
# string "sqlc.New(" made renaming the import a way round the rule.
printf '%s\n' 'package mcp' \
  'import (' \
  '	"github.com/jackc/pgx/v5"' \
  '	q "github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"' \
  ')' \
  'func (s *Service) aliased(tx pgx.Tx) { _ = q.New(tx) }' \
  >"$TREE/apps/api/internal/mcp/aliased.go"
run_case "bypass-sqlc-new-via-import-alias" 1 "builds its own q.Queries" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/aliased.go"

# A reviewed exception. The Store implementation is allowed to be split across
# more than one file, and before DBPATH existed that was unrepresentable: the
# only exception was the hard-coded name repo.go.
printf '%s\n' 'package mcp' \
  'import "github.com/jackc/pgx/v5"' \
  'func getGrantTx(ctx context.Context, tx pgx.Tx, grantID uuid.UUID) error {' \
  '	_ = sqlc.New(tx)' \
  '	return nil' \
  '}' >"$TREE/apps/api/internal/mcp/repo_status.go"
make_allow "$ALLOW" 'DBPATH internal/mcp/repo_status.go   # Repo split across files; reached only from Repo.Snapshot inside RunTenantTx'
run_case "ok-dbpath-reviewed-repo-split" 0 "check-mcp-site-containment: OK" "VIOLATION" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
rm -f "$TREE/apps/api/internal/mcp/repo_status.go"

# Both directions, as for every other kind. A DBPATH entry whose file stopped
# touching the database means the guard is holding a door open on a room that
# has moved.
run_case "bypass-stale-allowlist-dbpath-entry" 1 "stale allowlist entry: DBPATH internal/mcp/repo_status.go" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
make_allow "$ALLOW"

# Rule D's own control. If repo.go -- the one file whose job database access is
# -- stops matching, the patterns are stale and Rule D would report a clean
# package whatever the package contained.
cp "$TREE/apps/api/internal/mcp/repo.go" "$WORK/repo.orig"
printf '%s\n' 'package mcp' \
  'func (r *Repo) ResolveScopeSites(ctx context.Context, tenantID uuid.UUID, mode string, tagIDs, siteIDs []uuid.UUID) ([]uuid.UUID, error) {' \
  '	return nil, nil' \
  '}' >"$TREE/apps/api/internal/mcp/repo.go"
run_case "broken-rule-d-patterns-match-nothing-in-repo" 2 "Rule D's database-access patterns match nothing" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
cp "$WORK/repo.orig" "$TREE/apps/api/internal/mcp/repo.go"

# A DBPATH entry is a real allowlist kind, not a typo that silently drops a
# record from the comparison.
make_allow "$ALLOW" 'DBPTH internal/mcp/typo.go   # misspelled kind'
run_case "broken-allowlist-dbpath-kind-typo" 2 "unrecognised allowlist kind" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
make_allow "$ALLOW"

# Both directions. An entry that matches nothing means the guard is watching a
# surface that has moved, which is indistinguishable from watching nothing.
make_allow "$ALLOW" 'CALL internal/mcp/gone.go VanishedFunc   # was here once'
run_case "bypass-stale-allowlist-call-entry" 1 "stale allowlist entry: CALL internal/mcp/gone.go" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

make_allow "$ALLOW" 'PARAM DeletedMethod.tenantID   # method no longer exists'
run_case "bypass-stale-allowlist-param-entry" 1 "stale allowlist entry: PARAM DeletedMethod.tenantID" - -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

make_allow "$ALLOW"

# ============================================================================
# broken-* : the guard's own input is missing, empty or moved. Exit 2, never 0.
# ============================================================================

run_case "broken-allowlist-missing" 2 "allowlist not found" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$WORK/nope.txt" --store-doc "$DOC"

printf '# every entry commented out\n' >"$WORK/empty-allow.txt"
run_case "broken-allowlist-emptied" 2 "contains no entries" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$WORK/empty-allow.txt" --store-doc "$DOC"

make_allow "$WORK/badkind.txt" 'PARM Typo.tenantID   # kind is misspelled'
run_case "broken-allowlist-unrecognised-kind" 2 "unrecognised allowlist kind" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$WORK/badkind.txt" --store-doc "$DOC"

run_case "broken-store-doc-file-missing" 2 "--store-doc file does not exist" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$WORK/nope.doc"

: >"$WORK/empty.doc"
run_case "broken-store-doc-empty" 2 "produced NO OUTPUT" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$WORK/empty.doc"

printf 'some other output entirely\n' >"$WORK/wrong.doc"
run_case "broken-store-doc-interface-renamed" 2 "does not contain 'type Store interface'" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$WORK/wrong.doc"

# The chokepoint gone from the interface is not "no site-scope surface".
printf '%s\n' 'type Store interface {' '	ListTagIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)' '}' \
  >"$WORK/nochoke.doc"
run_case "broken-store-doc-lost-the-chokepoint" 2 "no longer declares ResolveScopeSites" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$WORK/nochoke.doc"

# An interface the parser can see but whose method lines it cannot read. This
# is the shape that would otherwise pass on ANY tree.
printf '%s\n' 'type Store interface {' '  ResolveScopeSites(ctx context.Context) error' '}' \
  >"$WORK/unparseable.doc"
run_case "broken-store-doc-no-methods-parsed" 2 "parsed ZERO methods" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$WORK/unparseable.doc"

# An interface that parses but yields no uuid parameters at all. HEAD has
# several, so this is a parse failure wearing the shape of a clean surface --
# the exact defect that would have made the whole of Rule B vacuous.
printf '%s\n' 'type Store interface {' '	ResolveScopeSites(ctx context.Context, mode string) error' '}' \
  >"$WORK/nouuid.doc"
run_case "broken-store-doc-zero-uuid-parameters" 2 "found ZERO uuid parameters" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$WORK/nouuid.doc"

run_case "broken-api-root-missing" 2 "api root does not exist" "containment: OK" -- \
  --api-root "$WORK/no-such-tree" --allowlist "$ALLOW" --store-doc "$DOC"

# The package moved. A file-path guard whose files have moved reports a clean
# tree unless it checks, and this is that check.
mv "$TREE/apps/api/internal/mcp" "$TREE/apps/api/internal/assistant"
run_case "broken-mcp-package-moved" 2 "the mcp package is not at" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
mv "$TREE/apps/api/internal/assistant" "$TREE/apps/api/internal/mcp"

# The chokepoint renamed in the source. Rules A and B are then measuring
# nothing, which must not read as "nothing to report".
sed 's/ResolveScopeSites/ResolveSitesForScope/' "$TREE/apps/api/internal/mcp/repo.go" >"$WORK/repo.new"
cp "$WORK/repo.new" "$TREE/apps/api/internal/mcp/repo.go"
run_case "broken-chokepoint-renamed-in-source" 2 "is not defined in internal/mcp/repo.go" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
make_tree "$TREE"

# NewSiteSet renamed. Same reasoning: Rule A's second half goes silent.
sed 's/func NewSiteSet(/func MakeSiteSet(/' "$TREE/apps/api/internal/mcp/model.go" >"$WORK/model.new"
cp "$WORK/model.new" "$TREE/apps/api/internal/mcp/model.go"
run_case "broken-siteset-ctor-renamed-in-source" 2 "is not defined in internal/mcp/model.go" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
make_tree "$TREE"

# Rule C's subject gone. Its compliant state is ZERO matches, so without this
# control an absent toolInvoker and a compliant one look identical.
sed 's/^type toolInvoker func(/type toolRunner func(/' "$TREE/apps/api/internal/mcp/registry.go" >"$WORK/reg.new"
cp "$WORK/reg.new" "$TREE/apps/api/internal/mcp/registry.go"
run_case "broken-toolinvoker-declaration-gone" 2 "toolInvoker declaration is not in" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"
make_tree "$TREE"

# No non-test Go under the api root at all.
mkdir -p "$WORK/hollow/apps/api/internal/mcp"
cp "$TREE/apps/api/internal/mcp/repo.go"     "$WORK/hollow/apps/api/internal/mcp/repo_test.go"
cp "$TREE/apps/api/internal/mcp/model.go"    "$WORK/hollow/apps/api/internal/mcp/model_test.go"
run_case "broken-no-non-test-go-files" 2 "no non-test .go files" "containment: OK" -- \
  --api-root "$WORK/hollow/apps/api" --allowlist "$ALLOW" --store-doc "$DOC"

run_case "broken-unknown-argument" 2 "unknown argument" "containment: OK" -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc "$DOC" --wat

# An option given no value used to make `shift 2` fail, leaving the same option
# in $1 and spinning the while loop forever: a CI job that never finishes and
# never reports.
#
# THE CASE FOR A HANG MUST NOT ITSELF HANG. run_case waits for the guard to
# exit, so using it here would wedge the suite on a regression instead of
# reporting one. This variant backgrounds the guard, polls a FIXED number of
# times, and calls a still-running guard a failure -- the bounded form the
# project rule asks for, and the only shape that can assert termination.
run_case_bounded() {
  name="$1"; want="$2"; needle="$3"; limit="$4"; shift 5
  case "$name" in
    *"$FILTER"*) : ;;
    *) return 0 ;;
  esac

  out="$WORK/out.bounded.$$"
  "$GUARD" "$@" >"$out" 2>&1 &
  pid=$!
  waited=0
  while [ "$waited" -lt "$limit" ]; do
    sleep 1
    waited=$((waited + 1))
    kill -0 "$pid" 2>/dev/null || break
  done

  if kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
    FAIL=$((FAIL + 1))
    printf 'FAIL %-46s DID NOT TERMINATE within %ss\n' "$name" "$limit"
    printf '       the argument loop is spinning: `shift 2` with no value left\n'
    rm -f "$out"
    return 0
  fi

  wait "$pid" 2>/dev/null
  got=$?
  ok=1
  [ "$got" = "$want" ] || ok=0
  if [ "$needle" != "-" ] && ! grep -qF -- "$needle" "$out"; then ok=0; fi

  if [ "$ok" = 1 ]; then
    PASS=$((PASS + 1))
    printf 'ok   %-46s exit %s\n' "$name" "$got"
  else
    FAIL=$((FAIL + 1))
    printf 'FAIL %-46s exit %s (want %s)\n' "$name" "$got" "$want"
    [ "$needle" = "-" ] || printf '       wanted output containing: %s\n' "$needle"
    sed 's/^/       | /' "$out"
  fi
  rm -f "$out"
}

run_case_bounded "broken-api-root-without-value" 2 "--api-root requires a value" 8 -- \
  --api-root

run_case_bounded "broken-allowlist-without-value" 2 "--allowlist requires a value" 8 -- \
  --api-root "$TREE/apps/api" --allowlist

run_case_bounded "broken-store-doc-without-value" 2 "--store-doc requires a value" 8 -- \
  --api-root "$TREE/apps/api" --allowlist "$ALLOW" --store-doc

# ============================================================================

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
[ "$PASS" -gt 0 ] || { printf 'check-mcp-site-containment_test: ZERO cases ran (filter %q matched nothing)\n' "$FILTER" >&2; exit 2; }
exit 0
