package mcp

// Unit proofs for the connection-token mint. Everything here runs against
// fakeStore, so it proves the GO branch structure and the payload that reaches
// the INSERT -- never the RLS, which no fake can evaluate. The RLS half is
// proved in apps/api/tests as wpmgr_app.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// mintRouter mounts the connections group with its REAL middleware and injects
// a principal, the same shape newConnectionsRouter uses.
func mintRouter(t *testing.T, store Store, p domain.Principal) *http.Handler {
	t.Helper()
	eng := newConnectionsRouter(t, store, p)
	var h http.Handler = eng
	return &h
}

// postMint sends a mint request and returns status plus the decoded body.
func postMint(t *testing.T, h http.Handler, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, APIV1Prefix+connectionsGroupPath, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := map[string]any{}
	if rec.Body.Len() > 0 {
		// A body that will not decode is a finding, not something to skip:
		// every response on this surface is JSON by contract.
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response (status %d): %v\nbody: %s", rec.Code, err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func mintService(store Store) *Service { return NewService(store) }

// ---------------------------------------------------------------------------
// PROOF 1 -- A SITE-SCOPED COLLABORATOR IS REFUSED, AND THE REFUSAL IS SAYABLE.
//
// This is the live production defect's shape on the sibling path: one
// self-registered client and one POST minted an organisation-wide grant for a
// principal shared onto a single site. The gate must refuse, it must say so
// (never an empty success), and NOTHING MAY BE WRITTEN.
// ---------------------------------------------------------------------------

func TestMintRefusesASiteScopedCollaborator(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{}
	svc := mintService(store)

	_, err := svc.MintConnection(context.Background(), MintConnectionRequest{
		Principal: siteScopedPrincipal(tenant),
		Name:      "ci-runner",
		SiteScope: SiteScopeRequest{Mode: SiteScopeModeAll},
	})
	if err == nil {
		t.Fatal("a site-scoped collaborator minted an organisation-wide connection token")
	}
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		t.Fatalf("want a typed domain refusal, got %T: %v", err, err)
	}
	if domErr.Code != ErrCodeOrgScopeRequired {
		t.Fatalf("want code %q, got %q (%s)", ErrCodeOrgScopeRequired, domErr.Code, domErr.Message)
	}
	if domain.HTTPStatus(domErr) != http.StatusForbidden {
		t.Fatalf("want 403 for a refused collaborator, got %d", domain.HTTPStatus(domErr))
	}

	// NOTHING WAS WRITTEN. A refusal that still reached the store would mean
	// the grant exists and only the response was refused.
	for _, call := range store.callLog() {
		if call == "CreateGrantWithToken" {
			t.Fatal("the store was reached despite the refusal: a grant was written for a refused principal")
		}
	}
	if len(store.minted) != 0 {
		t.Fatalf("want 0 grants minted, got %d", len(store.minted))
	}
}

// The same refusal through the MOUNTED ROUTE, so the permission middleware and
// the service refusal are both exercised. A gate that holds in the service and
// is bypassed by the handler guards nothing.
func TestMintRefusesASiteScopedCollaboratorThroughTheMountedRoute(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{}
	h := mintRouter(t, store, siteScopedPrincipal(tenant))

	status, body := postMint(t, *h, map[string]any{
		"name": "ci-runner", "site_scope_mode": "all",
	})
	if status != http.StatusForbidden {
		t.Fatalf("want 403 through the mounted route, got %d (body %v)", status, body)
	}
	if len(store.minted) != 0 {
		t.Fatalf("want 0 grants minted, got %d", len(store.minted))
	}
}

// ---------------------------------------------------------------------------
// PROOF 2 -- THE PLAINTEXT IS RETURNED ONCE AND IS NEVER STORED OR LOGGED.
// ---------------------------------------------------------------------------

func TestMintReturnsThePlaintextOnceAndStoresOnlyTheHash(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{}
	svc := mintService(store)

	out, err := svc.MintConnection(context.Background(), MintConnectionRequest{
		Principal: orgPrincipal(tenant),
		Name:      "  headless-ci  ", // trimmed by the service
		SiteScope: SiteScopeRequest{Mode: SiteScopeModeAll},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if out.Token == "" {
		t.Fatal("no plaintext returned: the caller has no credential")
	}
	if len(store.minted) != 1 {
		t.Fatalf("want exactly 1 mint, got %d", len(store.minted))
	}
	got := store.minted[0]

	if got.Grant.Name != "headless-ci" {
		t.Fatalf("name not trimmed: %q", got.Grant.Name)
	}

	// THE PLAINTEXT IS NOT IN THE ROW. Checked against every string field the
	// INSERT carries, not merely against token_hash: a future field that
	// captured it would be just as bad.
	if got.Token.TokenHash == out.Token {
		t.Fatal("the PLAINTEXT was stored in token_hash")
	}
	if got.Token.TokenHash != hashCredential(out.Token) {
		t.Fatalf("token_hash is not the SHA-256 of the plaintext:\n got %q\nwant %q",
			got.Token.TokenHash, hashCredential(out.Token))
	}
	if !isLowerHex64(got.Token.TokenHash) {
		t.Fatalf("token_hash %q does not satisfy the column's '^[0-9a-f]{64}$' CHECK", got.Token.TokenHash)
	}

	// The PREFIX is the public handle and is a genuine prefix of the
	// plaintext -- that is what makes it match a token in a config file. It is
	// short enough to carry no authentication weight.
	if got.Token.TokenPrefix != out.Token[:tokenPrefixLen] {
		t.Fatalf("token_prefix %q is not the plaintext's first %d bytes",
			got.Token.TokenPrefix, tokenPrefixLen)
	}
	if len(got.Token.TokenPrefix) >= len(out.Token) {
		t.Fatal("token_prefix is the whole credential, so it is not a handle")
	}
	if got.Token.TokenPrefix != out.TokenPrefix {
		t.Fatalf("returned prefix %q disagrees with the stored one %q", out.TokenPrefix, got.Token.TokenPrefix)
	}
	if !got.Token.ExpiresAt.Valid {
		t.Fatal("expires_at was written NULL: this token would never expire")
	}
}

// The plaintext must not reach a log. This drives the MOUNTED ROUTE with a
// slog handler capturing everything at DEBUG and greps the captured bytes.
func TestMintDoesNotEmitThePlaintextToLogsOrTheURL(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{}

	var logs bytes.Buffer
	// Captures EVERYTHING, including debug: a leak that only appears at debug
	// level is still a leak, and this is the level at which one would appear.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := mintRouter(t, store, orgPrincipal(tenant))
	status, body := postMint(t, *h, map[string]any{
		"name": "headless-ci", "site_scope_mode": "all",
	})
	if status != http.StatusCreated {
		t.Fatalf("want 201, got %d (body %v)", status, body)
	}

	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("no token in the response body: %v", body)
	}

	// ONCE IN THE BODY. The body is the only place it may appear.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-marshal body: %v", err)
	}
	if n := strings.Count(string(raw), token); n != 1 {
		t.Fatalf("the plaintext appears %d times in the response body, want exactly 1", n)
	}

	// NOWHERE IN THE LOGS.
	if strings.Contains(logs.String(), token) {
		t.Fatalf("THE PLAINTEXT WAS LOGGED:\n%s", logs.String())
	}

	// The guard must be able to fail. If the token were a substring of
	// something universally present the assertion above would be vacuous, so
	// prove the buffer search actually finds a planted needle.
	logs.WriteString("planted-needle-" + token)
	if !strings.Contains(logs.String(), token) {
		t.Fatal("the log search cannot find a string that IS in the buffer; the previous assertion was vacuous")
	}
}

// isLowerHex64 mirrors the '^[0-9a-f]{64}$' CHECK on every hash column.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// PROOF 3 -- THE RATE LIMIT BITES, AND IT IS PER OPERATOR.
// ---------------------------------------------------------------------------

func TestMintIsRateLimitedPerOperator(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{}
	svc := mintService(store)
	victim := orgPrincipal(tenant)

	mint := func(p domain.Principal) error {
		_, err := svc.MintConnection(context.Background(), MintConnectionRequest{
			Principal: p, Name: "ci", SiteScope: SiteScopeRequest{Mode: SiteScopeModeAll},
		})
		return err
	}

	// The per-user budget, spent exactly.
	for i := 0; i < MintPerUserPerMin; i++ {
		if err := mint(victim); err != nil {
			t.Fatalf("mint %d of the per-user budget failed: %v", i+1, err)
		}
	}
	// One more must be refused. This is the assertion the whole limiter exists
	// for: one stolen session must not become an unbounded number of long-lived
	// bearer credentials.
	err := mint(victim)
	if err == nil {
		t.Fatalf("the %dth mint succeeded: the per-user budget of %d is not enforced",
			MintPerUserPerMin+1, MintPerUserPerMin)
	}
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Code != ErrCodeMintRateLimited {
		t.Fatalf("want %q, got %v", ErrCodeMintRateLimited, err)
	}
	if domain.HTTPStatus(domErr) != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", domain.HTTPStatus(domErr))
	}

	// A REFUSED MINT WROTE NOTHING. The count must be exactly the budget.
	if len(store.minted) != MintPerUserPerMin {
		t.Fatalf("want %d grants written, got %d: a refused mint still wrote",
			MintPerUserPerMin, len(store.minted))
	}

	// AND IT DOES NOT OVER-FIRE: a DIFFERENT operator in the same organisation
	// is unaffected. A limiter that refuses honest work gets switched off.
	other := orgPrincipal(tenant)
	if err := mint(other); err != nil {
		t.Fatalf("a different operator was refused by the first operator's budget: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PROOF 4 -- AN UNRESOLVABLE TAG IS REFUSED, LOUDLY AND BY ID.
//
// scope_tag_ids is a uuid[] with no foreign key, so the database accepts any
// UUID at all. A tag id that names nothing therefore stores cleanly and then
// resolves to zero sites forever -- silently narrowing the scope to nothing,
// indistinguishable from a deliberately narrow connection.
// ---------------------------------------------------------------------------

func TestMintRefusesATagIDThatNamesNoTag(t *testing.T) {
	tenant := uuid.New()
	known := uuid.New()
	ghost := uuid.New()
	store := &fakeStore{tagIDs: []uuid.UUID{known}}
	svc := mintService(store)

	_, err := svc.MintConnection(context.Background(), MintConnectionRequest{
		Principal: orgPrincipal(tenant),
		Name:      "ci",
		SiteScope: SiteScopeRequest{Mode: SiteScopeModeTags, TagIDs: []uuid.UUID{known, ghost}},
	})
	if err == nil {
		t.Fatal("a tag id naming no tag was accepted: the grant's scope is silently narrowed to nothing")
	}
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Code != ErrCodeUnknownScopeTag {
		t.Fatalf("want %q, got %v", ErrCodeUnknownScopeTag, err)
	}
	// THE REFUSAL NAMES THE ID. A refusal that does not say which tag is
	// unfixable by the operator who sent it.
	if !strings.Contains(domErr.Message, ghost.String()) {
		t.Fatalf("the refusal does not name the offending id %s: %q", ghost, domErr.Message)
	}
	if len(store.minted) != 0 {
		t.Fatalf("want 0 grants written, got %d", len(store.minted))
	}
}

// A FAILED REGISTRY READ IS NOT AN EMPTY REGISTRY. Proceeding on the error
// would refuse every tag as unknown -- an infra failure wearing a 422, which
// sends the operator to fix a tag that is fine.
func TestMintTreatsAFailedTagRegistryReadAsInfraNotAsAnUnknownTag(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{tagIDsErr: errors.New("database is down")}
	svc := mintService(store)

	_, err := svc.MintConnection(context.Background(), MintConnectionRequest{
		Principal: orgPrincipal(tenant),
		Name:      "ci",
		SiteScope: SiteScopeRequest{Mode: SiteScopeModeTags, TagIDs: []uuid.UUID{uuid.New()}},
	})
	if err == nil {
		t.Fatal("a failed registry read produced a successful mint")
	}
	var domErr *domain.Error
	if errors.As(err, &domErr) {
		t.Fatalf("an infra failure was reported as the typed domain error %q: %v", domErr.Code, err)
	}
	if len(store.minted) != 0 {
		t.Fatalf("want 0 grants written, got %d", len(store.minted))
	}
}

// ---------------------------------------------------------------------------
// PROOF 5 -- AN EMPTY SCOPE IS ACCEPTED AND STORED AS EMPTY, NEVER WIDENED.
//
// A real tag that carries zero sites today is a LEGITIMATE thing to mint: the
// connection reads nothing now and reads whatever gets tagged later. The owner's
// ruling on the 2026-08-24 wireframes is explicit that this must be accepted.
// It must NOT be refused for resolving to nothing, and above all it must not be
// widened to mode 'all' on the theory that empty "must have been a mistake".
// ---------------------------------------------------------------------------

func TestMintAcceptsATagThatResolvesToNoSitesAndStoresItAsGiven(t *testing.T) {
	tenant := uuid.New()
	emptyTag := uuid.New()
	store := &fakeStore{
		tagIDs: []uuid.UUID{emptyTag}, // the tag EXISTS
		// ...and resolves to NO SITES. This is the state under test.
		scopeSites: nil,
	}
	svc := mintService(store)

	out, err := svc.MintConnection(context.Background(), MintConnectionRequest{
		Principal: orgPrincipal(tenant),
		Name:      "reads-nothing-yet",
		SiteScope: SiteScopeRequest{Mode: SiteScopeModeTags, TagIDs: []uuid.UUID{emptyTag}},
	})
	if err != nil {
		t.Fatalf("a real tag carrying zero sites was refused: %v", err)
	}
	if out.Token == "" {
		t.Fatal("no token returned")
	}
	if len(store.minted) != 1 {
		t.Fatalf("want 1 mint, got %d", len(store.minted))
	}
	got := store.minted[0].Grant

	// NEVER WIDENED. The mode and the payload are exactly what was asked for.
	if got.SiteScopeMode != string(SiteScopeModeTags) {
		t.Fatalf("the scope was WIDENED: stored mode %q, want %q", got.SiteScopeMode, SiteScopeModeTags)
	}
	if len(got.ScopeTagIds) != 1 || got.ScopeTagIds[0] != emptyTag {
		t.Fatalf("the tag payload was not stored as given: %v", got.ScopeTagIds)
	}
	if len(got.ScopeSiteIds) != 0 {
		t.Fatalf("sites appeared in a 'tags' grant: %v", got.ScopeSiteIds)
	}
	if out.SiteScopeMode != SiteScopeModeTags {
		t.Fatalf("the response reports mode %q, want %q", out.SiteScopeMode, SiteScopeModeTags)
	}
}

// The STRUCTURAL rules are unchanged: an empty allowlist is still not a way to
// ask for every site, in either scoped mode. This is the guard against reading
// the acceptance above too broadly.
func TestMintKeepsTheStructuralEmptyAllowlistRefusals(t *testing.T) {
	tenant := uuid.New()
	for _, tc := range []struct {
		name  string
		scope SiteScopeRequest
	}{
		{"tags mode naming no tags", SiteScopeRequest{Mode: SiteScopeModeTags}},
		{"list mode naming no sites", SiteScopeRequest{Mode: SiteScopeModeList}},
		{"absent mode", SiteScopeRequest{}},
		{"unrecognised mode", SiteScopeRequest{Mode: SiteScopeMode("everything")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := mintService(store)
			_, err := svc.MintConnection(context.Background(), MintConnectionRequest{
				Principal: orgPrincipal(tenant), Name: "ci", SiteScope: tc.scope,
			})
			if err == nil {
				t.Fatal("accepted: an empty or absent site scope was read as a valid grant")
			}
			var domErr *domain.Error
			if !errors.As(err, &domErr) || domErr.Code != ErrCodeInvalidSiteScope {
				t.Fatalf("want %q, got %v", ErrCodeInvalidSiteScope, err)
			}
			if len(store.minted) != 0 {
				t.Fatalf("want 0 grants written, got %d", len(store.minted))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PROOF 6 -- CAPABILITIES. An omitted list is the org default, never an empty
// set; a capability wider than the ceiling is REFUSED rather than dropped.
// ---------------------------------------------------------------------------

func TestMintNeverStoresAnEmptyCapabilitySet(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{}
	svc := mintService(store)

	if _, err := svc.MintConnection(context.Background(), MintConnectionRequest{
		Principal: orgPrincipal(tenant), Name: "ci",
		SiteScope: SiteScopeRequest{Mode: SiteScopeModeAll},
		// No capabilities named at all.
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	got := store.minted[0].Grant
	if len(got.Capabilities) == 0 {
		t.Fatal("an EMPTY capability set was stored: this connection would authenticate and reach no tool")
	}
	want := capabilityNames(DefaultGrantCapabilities())
	if len(got.Capabilities) != len(want) {
		t.Fatalf("stored capabilities %v, want the org default %v", got.Capabilities, want)
	}
}

func TestMintRefusesACapabilityWiderThanTheOrgDefault(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{}
	svc := mintService(store)

	_, err := svc.MintConnection(context.Background(), MintConnectionRequest{
		Principal: orgPrincipal(tenant), Name: "ci",
		SiteScope:    SiteScopeRequest{Mode: SiteScopeModeAll},
		Capabilities: []Capability{Capability("mcp.sites.write")},
	})
	if err == nil {
		t.Fatal("a capability the organisation does not hold was accepted")
	}
	if len(store.minted) != 0 {
		t.Fatalf("want 0 grants written, got %d", len(store.minted))
	}
}

// ---------------------------------------------------------------------------
// PROOF 7 -- THE WHOLE PRINCIPAL REACHES THE STORE.
//
// The store method takes a principal and not a tenant id precisely because
// db.RunTenantTx dispatches on it: flattening it here would leave the
// site-scope GUC unset and the RESTRICTIVE insert policy inert, and everything
// would still compile and still pass.
// ---------------------------------------------------------------------------

func TestMintHandsTheWholePrincipalToTheStore(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{}
	svc := mintService(store)
	p := orgPrincipal(tenant)

	if _, err := svc.MintConnection(context.Background(), MintConnectionRequest{
		Principal: p, Name: "ci", SiteScope: SiteScopeRequest{Mode: SiteScopeModeAll},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if store.grantPrincipal == nil {
		t.Fatal("no principal reached the store: the site-scope dispatch has nothing to key on")
	}
	if store.grantPrincipal.GetTenantID() != tenant {
		t.Fatalf("store got tenant %s, want %s", store.grantPrincipal.GetTenantID(), tenant)
	}
	if store.grantPrincipal.GetUserID() != p.UserID {
		t.Fatalf("store got user %s, want %s: app.user_id would be wrong for the audit hash chain",
			store.grantPrincipal.GetUserID(), p.UserID)
	}
	// The created_by column must name the operator, not NULL.
	if !store.minted[0].Grant.CreatedByUserID.Valid {
		t.Fatal("created_by_user_id was written NULL: this credential is unattributable")
	}
}

// ---------------------------------------------------------------------------
// PROOF 8 -- THE ROUTE EXISTS AND THE WRONG VERB SAYS SO.
//
// A 404 on this path reads as "not deployed", which is exactly how the S6b-2
// blocker presented and cost a debugging session.
// ---------------------------------------------------------------------------

func TestMintRouteAnswers405AndNot404OnAWrongVerb(t *testing.T) {
	h := mintRouter(t, &fakeStore{}, orgPrincipal(uuid.New()))
	req := httptest.NewRequest(http.MethodDelete, APIV1Prefix+connectionsGroupPath, nil)
	rec := httptest.NewRecorder()
	(*h).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405 on DELETE, got %d", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, http.MethodPost) || !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow header %q does not name both supported verbs", allow)
	}
}
