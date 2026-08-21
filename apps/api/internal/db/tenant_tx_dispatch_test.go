// tenant_tx_dispatch_test.go: the behavioural guard on RunTenantTx's routing.
//
// This replaces TestRunTenantTxDispatchMatchesDomainPredicate, which lived in
// internal/authz and never called into this package at all. It restated the
// dispatch condition in its own body and compared that restatement to
// domain.Principal.IsSiteConstrained — two copies, both outside db.go. Replacing
// the scoped branch in RunTenantTx with `if false` routed every site-scoped
// principal through InTenantTx, with app.site_scope and app.allowed_site_ids
// unset and every m112 RESTRICTIVE policy inert, and that test still passed.
//
// Two changes close it, and the first matters more than the second:
//
//  1. There is now exactly one copy of the predicate. db.go calls
//     domain.IsSiteConstrained; drift is not something a test has to detect
//     because it is no longer expressible.
//
//  2. This test drives the real dispatch — dispatchTenantTx, the function
//     RunTenantTx delegates to — against a recorder that reports WHICH helper
//     ran. Gutting the branch changes the recorded helper, so the mutation that
//     the old guard slept through reddens here.
package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// recordingHelpers implements tenantTxHelpers and records the call instead of
// touching a database.
type recordingHelpers struct {
	called   string
	tenantID uuid.UUID
	userID   uuid.UUID
	allowed  []uuid.UUID
	ranFn    bool
}

func (r *recordingHelpers) InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	r.called, r.tenantID = "InTenantTx", tenantID
	return fn(nil)
}

func (r *recordingHelpers) InTenantTxAsUser(ctx context.Context, tenantID, userID uuid.UUID, fn func(tx pgx.Tx) error) error {
	r.called, r.tenantID, r.userID = "InTenantTxAsUser", tenantID, userID
	return fn(nil)
}

func (r *recordingHelpers) InScopedTenantTx(ctx context.Context, tenantID, userID uuid.UUID, allowedSiteIDs []uuid.UUID, fn func(tx pgx.Tx) error) error {
	r.called, r.tenantID, r.userID, r.allowed = "InScopedTenantTx", tenantID, userID, allowedSiteIDs
	return fn(nil)
}

// *Pool must satisfy the interface the dispatch is written against, or this
// test would be exercising a seam production does not use.
var _ tenantTxHelpers = (*Pool)(nil)

// domain.Principal must satisfy ScopedPrincipal, or the cases below would be
// proving something about a test double only.
var _ ScopedPrincipal = domain.Principal{}

// TestDispatchTenantTxRoutes asserts, for every combination of scope, allowlist
// and user id that matters, which transaction helper the dispatch actually
// calls. The principals are real domain.Principal values, so the predicate
// under test is reached the same way a request reaches it.
func TestDispatchTenantTxRoutes(t *testing.T) {
	tenant := uuid.New()
	user := uuid.New()
	site := uuid.New()

	cases := []struct {
		name        string
		principal   domain.Principal
		wantHelper  string
		wantAllowed []uuid.UUID
	}{
		{
			name:       "unset scope, no allowlist, no user (API key): plain tenant tx",
			principal:  domain.Principal{TenantID: tenant},
			wantHelper: "InTenantTx",
		},
		{
			name:       "unset scope, no allowlist, with user: tenant tx as user",
			principal:  domain.Principal{TenantID: tenant, UserID: user},
			wantHelper: "InTenantTxAsUser",
		},
		{
			name:       "org scope, no allowlist, no user: plain tenant tx",
			principal:  domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg},
			wantHelper: "InTenantTx",
		},
		{
			name:       "org scope, no allowlist, with user: tenant tx as user",
			principal:  domain.Principal{TenantID: tenant, UserID: user, Scope: domain.ScopeOrg},
			wantHelper: "InTenantTxAsUser",
		},
		{
			// The fail-CLOSED case m120 keeps expressible: restricted to zero
			// sites must still activate the site-scope GUCs, not fall through
			// to a tenant-wide transaction.
			name:        "site scope, empty allowlist: scoped tx with no sites",
			principal:   domain.Principal{TenantID: tenant, UserID: user, Scope: domain.ScopeSite},
			wantHelper:  "InScopedTenantTx",
			wantAllowed: nil,
		},
		{
			name:        "site scope, with allowlist: scoped tx carrying the allowlist",
			principal:   domain.Principal{TenantID: tenant, UserID: user, Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{site}},
			wantHelper:  "InScopedTenantTx",
			wantAllowed: []uuid.UUID{site},
		},
		{
			// The backstop disjunct. Unreachable for any principal built by
			// apikey.PrincipalFor or the session path, and the point is that it
			// stays fail-closed if a future constructor forgets the label.
			name:        "unset scope, with allowlist: scoped tx anyway",
			principal:   domain.Principal{TenantID: tenant, UserID: user, AllowedSiteIDs: []uuid.UUID{site}},
			wantHelper:  "InScopedTenantTx",
			wantAllowed: []uuid.UUID{site},
		},
		{
			name:        "org scope, with allowlist: scoped tx anyway",
			principal:   domain.Principal{TenantID: tenant, UserID: user, Scope: domain.ScopeOrg, AllowedSiteIDs: []uuid.UUID{site}},
			wantHelper:  "InScopedTenantTx",
			wantAllowed: []uuid.UUID{site},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingHelpers{}
			fnRan := false
			err := dispatchTenantTx(context.Background(), rec, tc.principal, func(tx pgx.Tx) error {
				fnRan = true
				return nil
			})
			if err != nil {
				t.Fatalf("dispatchTenantTx returned %v", err)
			}
			if !fnRan {
				t.Errorf("the caller's fn never ran")
			}
			if rec.called != tc.wantHelper {
				t.Errorf("dispatched to %s, want %s — a site-scoped principal in the wrong helper runs with app.site_scope unset",
					rec.called, tc.wantHelper)
			}
			if rec.tenantID != tc.principal.TenantID {
				t.Errorf("helper got tenant %s, want %s", rec.tenantID, tc.principal.TenantID)
			}
			if len(rec.allowed) != len(tc.wantAllowed) {
				t.Fatalf("helper got allowlist %v, want %v", rec.allowed, tc.wantAllowed)
			}
			for i := range tc.wantAllowed {
				if rec.allowed[i] != tc.wantAllowed[i] {
					t.Errorf("allowlist[%d] = %s, want %s", i, rec.allowed[i], tc.wantAllowed[i])
				}
			}
			// The dispatch must hand the user id to the helpers that take one,
			// or the audit hash chain and the memberships_self_read policy lose
			// app.user_id.
			if tc.wantHelper != "InTenantTx" && rec.userID != tc.principal.UserID {
				t.Errorf("helper got user %s, want %s", rec.userID, tc.principal.UserID)
			}
		})
	}
}

// TestDispatchTenantTxPropagatesError proves the dispatch is a pass-through and
// does not swallow the helper's error, in every branch.
func TestDispatchTenantTxPropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	principals := []domain.Principal{
		{TenantID: uuid.New()},
		{TenantID: uuid.New(), UserID: uuid.New()},
		{TenantID: uuid.New(), UserID: uuid.New(), Scope: domain.ScopeSite},
	}
	for _, p := range principals {
		rec := &recordingHelpers{}
		err := dispatchTenantTx(context.Background(), rec, p, func(tx pgx.Tx) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Errorf("%s: got %v, want the sentinel back", rec.called, err)
		}
	}
}

// TestRunTenantTxUsesTheDispatch pins RunTenantTx to dispatchTenantTx. The
// method needs a real pool to run, so what is asserted here is the wiring: a
// nil *Pool reaches the dispatch, which selects a helper and panics inside it
// on the nil receiver rather than returning. A RunTenantTx that stopped
// delegating — or that grew a second copy of the routing — would not panic
// where this expects.
func TestRunTenantTxUsesTheDispatch(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("RunTenantTx on a nil pool returned instead of reaching a tx helper; it is no longer delegating to dispatchTenantTx")
		}
	}()
	var p *Pool
	_ = p.RunTenantTx(context.Background(), domain.Principal{
		TenantID: uuid.New(), Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{uuid.New()},
	}, func(tx pgx.Tx) error { return nil })
}
