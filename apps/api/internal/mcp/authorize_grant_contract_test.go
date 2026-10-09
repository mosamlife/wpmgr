// The contract GH #802 pins: what AuthorizeGrant derives (the tenant, the site
// scope, the capabilities and scopes, and the grant it names) comes only from
// a stored grant row, in the tenant that row was read in, never from the
// caller.
package mcp

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// TestGrantVerdictAuthorityIsUnexported pins the compile-level half: a package
// outside mcp cannot name the authority a verdict carries, so it cannot set it
// in a GrantVerdict literal or change it on a verdict it was handed. If a field
// of the authority is exported, or moved back onto GrantVerdict as an exported
// field, this fails.
func TestGrantVerdictAuthorityIsUnexported(t *testing.T) {
	rt := reflect.TypeFor[GrantVerdict]()
	f, ok := rt.FieldByName("stored")
	if !ok {
		t.Fatal("GrantVerdict has no stored field")
	}
	if f.IsExported() || f.Type != reflect.TypeFor[storedGrant]() {
		t.Fatalf("GrantVerdict.stored is exported=%v of type %v; it must be an unexported storedGrant", f.IsExported(), f.Type)
	}
	st := reflect.TypeFor[storedGrant]()
	for _, name := range []string{"found", "tenantID", "authorized", "grantID", "grantName",
		"siteScopeMode", "scopeTagIDs", "scopeSiteIDs", "capabilities", "oauthScopes", "viaOAuth", "setupClient"} {
		if _, ok := st.FieldByName(name); !ok {
			t.Fatalf("storedGrant has no field %q", name)
		}
	}
	for sf := range st.Fields() {
		if sf.IsExported() {
			t.Fatalf("storedGrant.%s is exported; the authority must be settable only by the row constructors", sf.Name)
		}
	}
	uuidSlice := reflect.TypeFor[[]uuid.UUID]()
	for vf := range rt.Fields() {
		if !vf.IsExported() {
			continue
		}
		if vf.Type == uuidSlice {
			t.Fatalf("GrantVerdict.%s is an exported []uuid.UUID; a caller could supply an id set", vf.Name)
		}
		for _, banned := range []string{"SiteScopeMode", "ScopeTagIDs", "ScopeSiteIDs", "TenantID"} {
			if vf.Name == banned {
				t.Fatalf("GrantVerdict.%s is exported", vf.Name)
			}
		}
	}
}

func authorizedGrantRow() sqlc.ReCheckMCPGrantAuthorizationInTenantTxRow {
	client := "contract-client"
	setup := "claude-desktop"
	return sqlc.ReCheckMCPGrantAuthorizationInTenantTxRow{
		GrantID:           uuid.New(),
		GrantName:         "Laptop",
		GrantStatus:       "active",
		SiteScopeMode:     "list",
		ScopeSiteIds:      []uuid.UUID{uuid.New(), uuid.New()},
		ScopeTagIds:       []uuid.UUID{uuid.New()},
		ClientID:          &client,
		GrantCapabilities: []string{string(CapSitesRead)},
		GrantOauthScopes:  []string{string(ScopeRead)},
		GrantSetupClient:  &setup,
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
	if slices.Contains(store.callLog(), "ResolveScopeSites") {
		t.Fatalf("AuthorizeGrant reached ResolveScopeSites for a verdict not read from a grant row; calls = %v", store.callLog())
	}
}

// TestAuthorizeGrantAcceptsARowDerivedVerdict is the positive control for every
// refusal in this file: the grant, read through the row constructor in its own
// tenant, derives an authorisation, and what it derives is the row's, in that
// tenant.
func TestAuthorizeGrantAcceptsARowDerivedVerdict(t *testing.T) {
	tenant := uuid.New()
	row := authorizedGrantRow()
	v := grantVerdictFromGrantRow(tenant, row)
	g := v.stored
	if !g.found || g.tenantID != tenant || g.siteScopeMode != row.SiteScopeMode ||
		!slices.Equal(g.scopeSiteIDs, row.ScopeSiteIds) || !slices.Equal(g.scopeTagIDs, row.ScopeTagIds) {
		t.Fatalf("grantVerdictFromGrantRow stored = %+v, want found in tenant %s with the row's scope (%q, %v, %v)",
			g, tenant, row.SiteScopeMode, row.ScopeSiteIds, row.ScopeTagIds)
	}

	store := &fakeStore{scopeSites: []uuid.UUID{row.ScopeSiteIds[0]}}
	auth, err := NewService(store).AuthorizeGrant(context.Background(), tenant, v)
	if err != nil {
		t.Fatalf("AuthorizeGrant(row-derived verdict, its own tenant) err = %v, want nil", err)
	}
	if !slices.Contains(store.callLog(), "ResolveScopeSites") {
		t.Fatalf("AuthorizeGrant did not resolve the stored scope; calls = %v", store.callLog())
	}
	if store.scopeSitesTenant != tenant || auth.TenantID != tenant {
		t.Fatalf("scope resolved in tenant %s, AuthorizedRequest.TenantID = %s; want both %s",
			store.scopeSitesTenant, auth.TenantID, tenant)
	}
	if auth.GrantID != row.GrantID || auth.GrantName != row.GrantName ||
		!auth.Sites.Allows(row.ScopeSiteIds[0]) || !auth.Capabilities.Allows(CapSitesRead) ||
		!auth.ViaOAuth || auth.SetupClient == nil || *auth.SetupClient != *row.GrantSetupClient {
		t.Fatalf("AuthorizeGrant derived %+v, which is not the row's grant, scope and facts", auth)
	}

	// The request-row constructor is the other admitted source, and it records
	// the tenant it is given (Authenticate passes the token's).
	rv := grantVerdictFromRequestRow(tenant, sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
		SiteScopeMode: "tags", ScopeTagIds: []uuid.UUID{uuid.New()}, Authorized: true,
	})
	if !rv.stored.found || rv.stored.tenantID != tenant || rv.stored.siteScopeMode != "tags" {
		t.Fatalf("grantVerdictFromRequestRow did not record a row-derived verdict in tenant %s: %+v", tenant, rv.stored)
	}
}

// TestAuthorizeGrantRefusesAVerdictReadInAnotherTenant: a verdict confers
// authority only in the tenant its row was read in. The tenant a caller passes
// is checked against it, never used in its place.
func TestAuthorizeGrantRefusesAVerdictReadInAnotherTenant(t *testing.T) {
	readIn := uuid.New()
	row := authorizedGrantRow()
	cases := []struct {
		name                string
		verdictIn, callerIn uuid.UUID
	}{
		{"read in one tenant, used in another", readIn, uuid.New()},
		{"read in one tenant, used with no tenant", readIn, uuid.Nil},
		{"recorded with no tenant", uuid.Nil, uuid.Nil},
	}
	for _, tc := range cases {
		store := &fakeStore{scopeSites: []uuid.UUID{row.ScopeSiteIds[0]}}
		auth, err := NewService(store).AuthorizeGrant(context.Background(), tc.callerIn,
			grantVerdictFromGrantRow(tc.verdictIn, row))
		if !errors.Is(err, ErrGrantNotAuthorized) {
			t.Errorf("%s: err = %v (TenantID=%s), want ErrGrantNotAuthorized", tc.name, err, auth.TenantID)
		}
		if slices.Contains(store.callLog(), "ResolveScopeSites") {
			t.Errorf("%s: AuthorizeGrant resolved a scope for a verdict from another tenant", tc.name)
		}
	}
}

// TestAuthorizeGrantRefusesARevokedVerdictWhoseAuthorizedWasEdited: the
// exported Authorized is a view, so setting it on a verdict whose stored row is
// not authorized confers nothing.
func TestAuthorizeGrantRefusesARevokedVerdictWhoseAuthorizedWasEdited(t *testing.T) {
	tenant := uuid.New()
	row := authorizedGrantRow()
	row.GrantStatus = "revoked"
	row.Authorized = false
	v := grantVerdictFromGrantRow(tenant, row)
	v.Found, v.Authorized, v.GrantStatus = true, true, "active"

	store := &fakeStore{scopeSites: []uuid.UUID{row.ScopeSiteIds[0]}}
	_, err := NewService(store).AuthorizeGrant(context.Background(), tenant, v)
	if !errors.Is(err, ErrGrantNotAuthorized) {
		t.Fatalf("AuthorizeGrant(revoked verdict, Authorized edited to true) err = %v, want ErrGrantNotAuthorized", err)
	}
	if slices.Contains(store.callLog(), "ResolveScopeSites") {
		t.Fatal("AuthorizeGrant resolved a scope for a revoked grant")
	}
}

// TestAuthorizeGrantDerivesNothingFromAnExportedField edits every exported
// field of a row-derived verdict in turn, by assignment and, for a slice or a
// pointer, through its element, and requires the AuthorizedRequest derived from
// the edited verdict to be the one derived from the unedited verdict. A new
// exported field of a kind this test cannot edit fails it, so the field gets a
// case rather than escaping the check.
func TestAuthorizeGrantDerivesNothingFromAnExportedField(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	row := authorizedGrantRow()
	// The row's scopes admit the cache capability and its stored set does not
	// hold it, so an edit that reached the derivation could widen it.
	row.GrantOauthScopes = []string{string(ScopeRead), string(ScopeCache)}
	store := &fakeStore{scopeSites: []uuid.UUID{row.ScopeSiteIds[0]}}
	svc := NewService(store)

	// Every verdict below is built from its own deep copy of the row, so an
	// edit through one verdict's slice or pointer edits only that verdict's row
	// and never the fixture the next one is built from.
	want, err := svc.AuthorizeGrant(ctx, tenant, grantVerdictFromGrantRow(tenant, cloneGrantRow(row)))
	if err != nil {
		t.Fatalf("control: unedited verdict err = %v", err)
	}
	if want.Capabilities.Allows(CapCachePurge) || !want.Capabilities.Allows(CapSitesRead) {
		t.Fatalf("control: unedited verdict capabilities = %+v, want sites.read only", want.Capabilities)
	}

	edits := 0
	for f := range reflect.TypeFor[GrantVerdict]().Fields() {
		if !f.IsExported() {
			continue
		}
		for _, through := range []bool{false, true} {
			v := grantVerdictFromGrantRow(tenant, cloneGrantRow(row))
			if !editExportedField(t, reflect.ValueOf(&v).Elem().FieldByIndex(f.Index), f.Name, through) {
				continue
			}
			edits++
			got, err := svc.AuthorizeGrant(ctx, tenant, v)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("editing GrantVerdict.%s (through=%v) changed what AuthorizeGrant derives: err=%v\n got %+v\nwant %+v",
					f.Name, through, err, got, want)
			}
		}
	}
	if edits == 0 {
		t.Fatal("no exported field was edited, so nothing above was checked")
	}

	// The escalations the edits above cover, by name.
	v := grantVerdictFromGrantRow(tenant, cloneGrantRow(row))
	v.Capabilities = append(v.Capabilities, string(CapCachePurge))
	v.GrantID, v.GrantName = uuid.New(), "someone else's grant"
	got, err := svc.AuthorizeGrant(ctx, tenant, v)
	if err != nil || got.Capabilities.Allows(CapCachePurge) || got.GrantID != row.GrantID || got.GrantName != row.GrantName {
		t.Fatalf("edited Capabilities/GrantID/GrantName reached the derivation: err=%v cache purge=%v grant=%s %q, want the row's grant %s %q without cache purge",
			err, got.Capabilities.Allows(CapCachePurge), got.GrantID, got.GrantName, row.GrantID, row.GrantName)
	}

	// The AuthorizedRequest shares no pointer with the verdict either: an edit
	// through its SetupClient does not reach the next derivation.
	v = grantVerdictFromGrantRow(tenant, cloneGrantRow(row))
	first, err := svc.AuthorizeGrant(ctx, tenant, v)
	if err != nil || first.SetupClient == nil {
		t.Fatalf("control: first derivation err=%v SetupClient=%v", err, first.SetupClient)
	}
	*first.SetupClient = "edited"
	again, err := svc.AuthorizeGrant(ctx, tenant, v)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("an edit through a returned AuthorizedRequest reached the verdict: err=%v\n got %+v\nwant %+v", err, again, want)
	}
}

// cloneGrantRow deep-copies a row: its slices and pointers share nothing with
// the original.
func cloneGrantRow(r sqlc.ReCheckMCPGrantAuthorizationInTenantTxRow) sqlc.ReCheckMCPGrantAuthorizationInTenantTxRow {
	r.ScopeSiteIds = slices.Clone(r.ScopeSiteIds)
	r.ScopeTagIds = slices.Clone(r.ScopeTagIds)
	r.GrantCapabilities = slices.Clone(r.GrantCapabilities)
	r.GrantOauthScopes = slices.Clone(r.GrantOauthScopes)
	r.ClientID = cloneString(r.ClientID)
	r.GrantSetupClient = cloneString(r.GrantSetupClient)
	return r
}

// editExportedField edits one exported GrantVerdict field. through=false
// assigns a different value; through=true writes through the field's slice
// element or pointer, and reports false for a kind that has neither.
func editExportedField(t *testing.T, fv reflect.Value, name string, through bool) bool {
	t.Helper()
	switch {
	case fv.Kind() == reflect.Bool:
		if through {
			return false
		}
		fv.SetBool(!fv.Bool())
	case fv.Kind() == reflect.String:
		if through {
			return false
		}
		fv.SetString(fv.String() + "-edited")
	case fv.Type() == reflect.TypeFor[uuid.UUID]():
		if through {
			return false
		}
		fv.Set(reflect.ValueOf(uuid.New()))
	case fv.Type() == reflect.TypeFor[[]string]():
		if through {
			if fv.Len() == 0 {
				t.Fatalf("GrantVerdict.%s is empty in the fixture, so it cannot be edited through", name)
			}
			fv.Index(0).SetString("edited")
		} else {
			fv.Set(reflect.Zero(fv.Type()))
		}
	case fv.Type() == reflect.TypeFor[*string]():
		if fv.IsNil() {
			t.Fatalf("GrantVerdict.%s is nil in the fixture, so it cannot be edited through", name)
		}
		if through {
			fv.Elem().SetString("edited")
		} else {
			fv.Set(reflect.Zero(fv.Type()))
		}
	default:
		t.Fatalf("GrantVerdict.%s has type %v, which this test cannot edit; add a case for it", name, fv.Type())
	}
	return true
}

// TestAuthenticateDerivesInTheTokensTenant: the token path records the tenant
// its re-check row was read in, which is the token's, and derives there.
func TestAuthenticateDerivesInTheTokensTenant(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	tok := liveToken(tenantID)
	store := &fakeStore{
		tokenOK: true, token: tok,
		recheckOK: true,
		recheck: sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
			GrantID: tok.GrantID, GrantStatus: "active",
			SiteScopeMode: "all", TokenID: tok.ID, TokenStatus: "active",
			TokenExpiresAt:    tok.ExpiresAt,
			GrantCapabilities: []string{string(CapSitesRead)},
			GrantOauthScopes:  []string{string(ScopeRead)},
			GrantExpiresAt:    time.Now().UTC().Add(90 * 24 * time.Hour),
			Authorized:        true,
		},
		scopeSites: []uuid.UUID{siteID},
	}
	auth, err := NewService(store).Authenticate(context.Background(), "the-bearer-token")
	if err != nil {
		t.Fatalf("a live connection was refused: %v", err)
	}
	if !slices.Contains(store.callLog(), "ResolveScopeSites") {
		t.Fatal("Authenticate never reached the scope chokepoint")
	}
	if store.scopeSitesTenant != tenantID || auth.TenantID != tenantID || auth.GrantID != tok.GrantID {
		t.Fatalf("scope resolved in tenant %s, AuthorizedRequest tenant %s grant %s; want the token's tenant %s and grant %s",
			store.scopeSitesTenant, auth.TenantID, auth.GrantID, tenantID, tok.GrantID)
	}
}
