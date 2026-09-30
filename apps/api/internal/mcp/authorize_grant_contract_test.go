// The contract GH #802 pins: the site scope AuthorizeGrant resolves always
// comes from a stored grant row, never from the caller.
package mcp

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// TestGrantVerdictSiteScopeIsUnexported pins the compile-level half: a package
// outside mcp cannot name the scope fields, so it cannot set them in a
// GrantVerdict literal. If a scope field is exported again this fails.
func TestGrantVerdictSiteScopeIsUnexported(t *testing.T) {
	rt := reflect.TypeFor[GrantVerdict]()
	for _, name := range []string{"siteScopeMode", "scopeTagIDs", "scopeSiteIDs", "fromStoredGrant"} {
		f, ok := rt.FieldByName(name)
		if !ok {
			t.Fatalf("GrantVerdict has no field %q", name)
		}
		if f.IsExported() {
			t.Fatalf("GrantVerdict.%s is exported; the scope must be settable only by the row constructors", name)
		}
	}
	uuidSlice := reflect.TypeFor[[]uuid.UUID]()
	for f := range rt.Fields() {
		if !f.IsExported() {
			continue
		}
		if f.Type == uuidSlice {
			t.Fatalf("GrantVerdict.%s is an exported []uuid.UUID; a caller could supply an id set", f.Name)
		}
		for _, banned := range []string{"SiteScopeMode", "ScopeTagIDs", "ScopeSiteIDs"} {
			if f.Name == banned {
				t.Fatalf("GrantVerdict.%s is exported", f.Name)
			}
		}
	}
}

func authorizedGrantRow() sqlc.ReCheckMCPGrantAuthorizationInTenantTxRow {
	return sqlc.ReCheckMCPGrantAuthorizationInTenantTxRow{
		GrantID:           uuid.New(),
		GrantStatus:       "active",
		SiteScopeMode:     "list",
		ScopeSiteIds:      []uuid.UUID{uuid.New(), uuid.New()},
		ScopeTagIds:       []uuid.UUID{uuid.New()},
		GrantCapabilities: []string{string(CapSitesRead)},
		GrantOauthScopes:  []string{string(ScopeRead)},
		Authorized:        true,
	}
}

// TestAuthorizeGrantRefusesAVerdictNotReadFromAGrantRow: a GrantVerdict
// literal claiming to be found and authorized is refused before the scope
// resolver is reached.
func TestAuthorizeGrantRefusesAVerdictNotReadFromAGrantRow(t *testing.T) {
	store := &fakeStore{scopeSites: []uuid.UUID{uuid.New()}}
	svc := NewService(store)

	forged := GrantVerdict{
		Found:        true,
		Authorized:   true,
		GrantID:      uuid.New(),
		GrantStatus:  "active",
		Capabilities: []string{string(CapSitesRead)},
		OauthScopes:  []string{string(ScopeRead)},
	}
	_, err := svc.AuthorizeGrant(context.Background(), uuid.New(), forged)
	if !errors.Is(err, ErrGrantNotAuthorized) {
		t.Fatalf("AuthorizeGrant(literal verdict) err = %v, want ErrGrantNotAuthorized", err)
	}
	if slices.Contains(store.calls, "ResolveScopeSites") {
		t.Fatalf("AuthorizeGrant reached ResolveScopeSites for a verdict not read from a grant row; calls = %v", store.calls)
	}
}

// TestAuthorizeGrantAcceptsARowDerivedVerdict is the positive control for the
// refusal above: the same grant, read through the row constructor, derives an
// authorisation, and the scope handed to the resolver is the row's.
func TestAuthorizeGrantAcceptsARowDerivedVerdict(t *testing.T) {
	row := authorizedGrantRow()
	v := grantVerdictFromGrantRow(row)
	if v.siteScopeMode != row.SiteScopeMode ||
		!slices.Equal(v.scopeSiteIDs, row.ScopeSiteIds) ||
		!slices.Equal(v.scopeTagIDs, row.ScopeTagIds) {
		t.Fatalf("grantVerdictFromGrantRow scope = (%q, %v, %v), want the row's (%q, %v, %v)",
			v.siteScopeMode, v.scopeSiteIDs, v.scopeTagIDs, row.SiteScopeMode, row.ScopeSiteIds, row.ScopeTagIds)
	}

	store := &fakeStore{scopeSites: []uuid.UUID{row.ScopeSiteIds[0]}}
	svc := NewService(store)
	if _, err := svc.AuthorizeGrant(context.Background(), uuid.New(), v); err != nil {
		t.Fatalf("AuthorizeGrant(row-derived verdict) err = %v, want nil", err)
	}
	if !slices.Contains(store.calls, "ResolveScopeSites") {
		t.Fatalf("AuthorizeGrant did not resolve the stored scope; calls = %v", store.calls)
	}

	// The request-row constructor is the other admitted source.
	rv := grantVerdictFromRequestRow(sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
		SiteScopeMode: "tags", ScopeTagIds: []uuid.UUID{uuid.New()}, Authorized: true,
	})
	if !rv.fromStoredGrant || rv.siteScopeMode != "tags" {
		t.Fatalf("grantVerdictFromRequestRow did not mark the verdict as row-derived: %+v", rv)
	}
}
