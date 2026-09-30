package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// THE EXPORTED KERNEL.
//
// Work done on a connection's behalf with no token in hand -- approving an AI
// request, and dispatching an approved one -- lives in another package and
// imports this one, never the reverse. These are the pieces it needs, exported
// with the behaviour the token path already has:
//
//   - GrantVerdict and Repo.ReCheckGrantAuthorization: the grant-level verdict,
//     read as a tenant read by primary key;
//   - Service.AuthorizeGrant: the derivation Authenticate performs from a
//     verdict to an AuthorizedRequest, shared rather than copied;
//   - ToolPermitted: "may this connection call this tool", the same answer the
//     invocation gate gives;
//   - SingleSitePrincipal and ErrSiteNotInScope: the only way to build a
//     principal for one site of a connection's scope;
//   - AssertSingleSiteTx: the in-transaction check that a site-scoped
//     transaction admits exactly that one site;
//   - Service.ForbiddenByContext: whether an operator AI rule forbids a tool
//     on a site;
//   - MinAgentVersionForOriginOnlyPurge.
// ---------------------------------------------------------------------------

// ErrSiteNotInScope is SingleSitePrincipal's refusal: the site is not in the
// connection's resolved scope (or the id is nil). A caller that gets it has a
// definite answer and must act on it; it is never a principal that admits
// nothing.
var ErrSiteNotInScope = errors.New("mcp: site not in the connection's scope")

// SingleSitePrincipal builds the principal for work on ONE site of a
// connection's scope. It REFUSES a site outside auth.Sites rather than
// narrowing to an empty set: a principal whose allowed set is empty turns
// every write under it into a silent 0-row statement, and a caller that
// needed to close something would leave it open instead.
//
// Membership is proved by auth.Sites, which ResolveScopeSites produced through
// the audited chokepoint. The transaction this principal opens then admits no
// site but this one (AssertSingleSiteTx checks that from inside it).
func SingleSitePrincipal(auth AuthorizedRequest, id uuid.UUID) (domain.Principal, error) {
	if id == uuid.Nil || !auth.Sites.Allows(id) {
		return domain.Principal{}, ErrSiteNotInScope
	}
	return domain.Principal{
		TenantID:       auth.TenantID,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{id},
	}, nil
}

// AssertSingleSiteTx fails unless tx is site-scoped to exactly siteID:
// app.site_scope is 'on' and app.allowed_site_ids names siteID and nothing
// else. Every site-scoped transaction on the AI request path opens with it.
//
// It exists because the tx helper is chosen from the principal, and swapping
// RunTenantTx for InTenantTx produces identical results for any query that
// also carries its own site predicate. Only the transaction can tell the two
// apart, so the check runs inside it.
func AssertSingleSiteTx(ctx context.Context, tx pgx.Tx, siteID uuid.UUID) error {
	if siteID == uuid.Nil {
		return errors.New("assert single-site tx: nil site id")
	}
	return assertSiteScopedTx(ctx, tx, []uuid.UUID{siteID})
}

// assertSiteScopedTx is the one GUC assertion in this package: app.site_scope
// is 'on' and app.allowed_site_ids is exactly the set allowed (order-free).
// ListSitesForRead and AssertSingleSiteTx both call it.
func assertSiteScopedTx(ctx context.Context, tx pgx.Tx, allowed []uuid.UUID) error {
	var siteScope, allowedIDs string
	if err := tx.QueryRow(ctx,
		"SELECT coalesce(current_setting('app.site_scope', true), ''), "+
			"coalesce(current_setting('app.allowed_site_ids', true), '')").Scan(&siteScope, &allowedIDs); err != nil {
		return fmt.Errorf("read site-scope settings: %w", err)
	}
	if siteScope != "on" {
		return fmt.Errorf(
			"site-scoped transaction: app.site_scope is %q inside this transaction, want \"on\". "+
				"The RESTRICTIVE _site_scope policies are INERT at any other value, so the "+
				"database is enforcing nothing on the site axis. Only InScopedTenantTx sets this "+
				"GUC, and only RunTenantTx with a site-constrained principal reaches it",
			siteScope)
	}
	if !sameIDSet(allowedIDs, allowed) {
		return fmt.Errorf(
			"site-scoped transaction: app.allowed_site_ids does not equal the %d site(s) this "+
				"transaction was opened for", len(allowed))
	}
	return nil
}

// sameIDSet reports whether the comma-joined GUC value names exactly want.
func sameIDSet(guc string, want []uuid.UUID) bool {
	var got []string
	if guc != "" {
		got = strings.Split(guc, ",")
	}
	if len(got) != len(want) {
		return false
	}
	w := make([]string, len(want))
	for i, id := range want {
		w[i] = id.String()
	}
	sort.Strings(got)
	sort.Strings(w)
	for i := range got {
		if strings.TrimSpace(got[i]) != w[i] {
			return false
		}
	}
	return true
}

// GrantVerdict is the grant-level authorization verdict: the grant's scope and
// capability columns, and whether it may act now. It is read by primary key,
// with no token, by Repo.ReCheckGrantAuthorization, and it is what
// Service.AuthorizeGrant derives an AuthorizedRequest from.
//
// Found is false when no such grant exists in the tenant. Authorized is then
// false too; callers treat both as inactive.
type GrantVerdict struct {
	Found bool

	GrantID       uuid.UUID
	GrantName     string
	GrantStatus   string
	SiteScopeMode string
	ScopeTagIDs   []uuid.UUID
	ScopeSiteIDs  []uuid.UUID
	// ClientID is set when the grant came from the OAuth sign-in path.
	ClientID     *string
	Capabilities []string
	OauthScopes  []string
	SetupClient  *string

	AbsoluteExpired       bool
	IdleExpired           bool
	TenantAssistantPaused bool
	Authorized            bool
}

func grantVerdictFromGrantRow(r sqlc.ReCheckMCPGrantAuthorizationInTenantTxRow) GrantVerdict {
	return GrantVerdict{
		Found:                 true,
		GrantID:               r.GrantID,
		GrantName:             r.GrantName,
		GrantStatus:           r.GrantStatus,
		SiteScopeMode:         r.SiteScopeMode,
		ScopeTagIDs:           r.ScopeTagIds,
		ScopeSiteIDs:          r.ScopeSiteIds,
		ClientID:              r.ClientID,
		Capabilities:          r.GrantCapabilities,
		OauthScopes:           r.GrantOauthScopes,
		SetupClient:           r.GrantSetupClient,
		AbsoluteExpired:       r.GrantAbsoluteExpired,
		IdleExpired:           r.GrantIdleExpired,
		TenantAssistantPaused: r.TenantAssistantPaused,
		Authorized:            r.Authorized,
	}
}

func grantVerdictFromRequestRow(r sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow) GrantVerdict {
	return GrantVerdict{
		Found:           true,
		GrantID:         r.GrantID,
		GrantName:       r.GrantName,
		GrantStatus:     r.GrantStatus,
		SiteScopeMode:   r.SiteScopeMode,
		ScopeTagIDs:     r.ScopeTagIds,
		ScopeSiteIDs:    r.ScopeSiteIds,
		ClientID:        r.ClientID,
		Capabilities:    r.GrantCapabilities,
		OauthScopes:     r.GrantOauthScopes,
		SetupClient:     r.GrantSetupClient,
		AbsoluteExpired: r.GrantAbsoluteExpired,
		IdleExpired:     r.GrantIdleExpired,
		Authorized:      r.Authorized,
	}
}

// ReCheckGrantAuthorization reads the grant-level verdict under InTenantTx, by
// primary key. It must be a tenant read and never the caller's principal:
// mcp_grants_site_scope_select refuses every grant row to a site-scoped
// session, so under an approver's or a site principal this would read nothing
// and every grant would look absent.
//
// No such grant is not an error: it returns a verdict with Found and
// Authorized false. last_used_at is never touched.
func (r *Repo) ReCheckGrantAuthorization(ctx context.Context, tenantID, grantID uuid.UUID) (GrantVerdict, error) {
	var out GrantVerdict
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).ReCheckMCPGrantAuthorizationInTenantTx(ctx,
			sqlc.ReCheckMCPGrantAuthorizationInTenantTxParams{TenantID: tenantID, GrantID: grantID})
		if err != nil {
			return err
		}
		out = grantVerdictFromGrantRow(row)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return GrantVerdict{}, nil
	}
	if err != nil {
		return GrantVerdict{}, fmt.Errorf("re-check mcp grant authorization: %w", err)
	}
	return out, nil
}

// ErrGrantNotAuthorized is AuthorizeGrant's refusal for a verdict that is not
// authorized (absent, revoked, expired, idle-expired, or the organisation's
// assistant is paused).
var ErrGrantNotAuthorized = errors.New("mcp: the connection is not authorized")

// AuthorizeGrant derives the AuthorizedRequest a verdict confers: the site
// scope through the audited chokepoint, and the capability set as the grant's
// stored column narrowed under the organisation's ceiling for the grant's own
// scopes. Authenticate calls it for every token request; work done on a
// connection's behalf without a token calls it with a verdict from
// Repo.ReCheckGrantAuthorization.
//
// It refuses an unauthorized verdict itself, so no caller can derive scope
// and capabilities for a grant that may not act.
//
// TokenID is left zero: no token carried this derivation. Authenticate sets it.
func (s *Service) AuthorizeGrant(ctx context.Context, tenantID uuid.UUID, v GrantVerdict) (AuthorizedRequest, error) {
	if !v.Found || !v.Authorized {
		return AuthorizedRequest{}, ErrGrantNotAuthorized
	}
	return s.authorizeGrant(ctx, tenantID, v)
}

// ToolPermitted reports whether auth may call the registered tool name: the
// tool is inside the organisation's ceiling and the grant holds its
// capability. It is the invocation gate's own answer (withinOrgCeiling, then
// capabilityHeld). It does not consult the server's write-tools switch, which
// is applied to the transport's live surface and separately by the callers
// that act on it.
func ToolPermitted(name string, auth AuthorizedRequest) bool {
	for _, e := range registryTools() {
		if e.Name == name {
			return withinOrgCeiling(e, auth) && capabilityHeld(e, auth)
		}
	}
	return false
}
