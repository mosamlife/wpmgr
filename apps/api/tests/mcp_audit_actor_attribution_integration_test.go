// mcp_audit_actor_attribution_integration_test.go: the two remaining sites of
// the nil-user audit-attribution defect -- Service.RevokeConnection and
// Service.Approve -- proven against the REAL schema, RLS and application role,
// through the SAME dispatch production uses.
//
// THE DEFECT. Both sites hardcoded `ActorType: audit.ActorUser` beside
// `ActorID: Principal.UserID.String()`. apikey.PrincipalFor sets APIKeyID and
// never UserID, so an API-key caller wrote uuid.Nil into actor_id under an
// actor_type asserting a human acted. The row exists, hashes into the chain and
// renders, and it explains nothing: actor_id names no user, and because
// ListAuditEntries' name join is gated on actor_type ('user' -> users.name,
// 'api_key' -> api_keys.name) the row resolves to no name either.
//
// WHY BOTH ARE REACHABLE BY A KEY, WHICH IS THE WHOLE PREMISE.
//
//	revoke   server.go's RegisterConnections(v1) mounts it from the SAME nil
//	         check as the headless connection-token mint. An API key that can
//	         mint can revoke, today, with nothing in between.
//	consent  server.go's Register(v1) mounts /consent, and v1 derives from
//	         sessionAuthGroup, which carries Auth.Authenticate() -- the same
//	         Bearer-accepting group. "Consent is browser-only" is a convention
//	         about who usually calls it, never a constraint on who can.
//
// THE REVOKE ROW IS THE CONSEQUENTIAL ONE. It is what an auditor reaches for
// after an incident: "who killed this credential, and when". Answering that
// with a user id that resolves to no user is worse than answering nothing.
//
// WHAT EVERY ASSERTION BELOW CHECKS, AND WHY IT IS BY VALUE. Each proof asserts
// the actor_type and the actor_id it expects, BY VALUE, and each is paired with
// a does-not-over-fire case driving the same route as a session user. A
// resolver that answered "api_key" for every principal would satisfy the
// API-key half alone; only the pair pins that the fix did not simply invert the
// bug.
//
// Every write and every read goes through the same Service, the same Handler
// and the same tx helpers server.New wires, on the app-role pool. No GUC is
// hand-set anywhere in this file, and mcpAssertAndReportRole prints
// current_user/rolsuper/rolbypassrls from pg_roles INSIDE the transaction under
// test -- a proof that opened its own connection would leave every policy inert
// and pass regardless.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// auditActorAPIKeyPrincipal builds EXACTLY what apikey.PrincipalFor produces
// for an org-scoped owner key, and asserts the premise of every proof in this
// file: that such a principal carries no UserID at all. If that ever stops
// being true these tests still pass while proving nothing, so it is checked
// rather than assumed.
func auditActorAPIKeyPrincipal(t *testing.T, tenantID, keyID uuid.UUID) domain.Principal {
	t.Helper()
	p := domain.Principal{
		Type: domain.PrincipalAPIKey, APIKeyID: keyID, TenantID: tenantID,
		Role: "owner", Scope: domain.ScopeOrg, AuthModel: domain.AuthModelRole,
	}
	if p.UserID != uuid.Nil {
		t.Fatal("an API-key principal carries a UserID; this whole file assumes it does not")
	}
	if p.ActorID() != keyID.String() {
		t.Fatalf("ActorID() = %q, want the key id %s", p.ActorID(), keyID)
	}
	return p
}

// auditActorReportRole prints and asserts the role of the transaction the audit
// read actually runs in.
func auditActorReportRole(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	if err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (the dispatch under test)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}
}

// findMCPAuditRow returns the single row for one action and target, failing if
// there is none. "No row at all" and "a row naming the wrong actor" are
// different defects and must not collapse into one assertion -- #613's own RLS
// proof passed for the wrong reason by asserting only that something happened.
func findMCPAuditRow(t *testing.T, pool *db.Pool, tenantID uuid.UUID,
	action, targetID string) mcpAuditRow {
	t.Helper()
	var hits []mcpAuditRow
	for _, r := range queryMCPAuditRowsAsAppRole(t, pool, tenantID, action) {
		if r.targetID == targetID {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("%s rows for target %s = %d, want exactly 1", action, targetID, len(hits))
	}
	return hits[0]
}

// assertActor is the by-value check. It never asserts merely that a row exists.
func assertActor(t *testing.T, r mcpAuditRow, action, wantType, wantID string) {
	t.Helper()
	if r.actorID == uuid.Nil.String() {
		t.Fatalf("%s actor_id is the ZERO UUID: the row exists and explains nothing", action)
	}
	if r.actorType != wantType {
		t.Fatalf("%s actor_type = %q, want %q: the row attributes the act to a kind "+
			"of actor that did not perform it, and the actor-name join is gated on "+
			"this column", action, r.actorType, wantType)
	}
	if r.actorID != wantID {
		t.Fatalf("%s actor_id = %q, want %q", action, r.actorID, wantID)
	}
}

// ---------------------------------------------------------------------------
// SITE 1: Service.RevokeConnection (internal/mcp/service.go).
//
// The most consequential of the three instances. Mounted by the same call as
// the mint, reachable by an API key today, and the row an incident review
// starts from.
// ---------------------------------------------------------------------------

func TestMCPRevokeByAnAPIKeyNamesTheKeyInTheAuditRowAsAppRole(t *testing.T) {
	pool := startPostgres(t)

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-revoke-actor-"+suffix)
	seedSite(t, pool, tenantID, "https://revokeactor-"+suffix+".example.test")
	auditActorReportRole(t, pool, tenantID)

	svc := mcp.NewService(mcp.NewRepo(pool)).
		WithAudit(audit.NewRecorder(pool, domain.SystemClock{}))

	keyID := uuid.New()
	keyEng := mountConnectionsLikeProduction(t, svc,
		auditActorAPIKeyPrincipal(t, tenantID, keyID))

	// Mint a connection with the key, then revoke it with the same key. Both
	// halves go through RegisterConnections, which is the point: one mount,
	// one credential, and until this fix only the mint recorded honestly.
	var minted mintResponse
	if code := mcpDoJSON(t, keyEng, http.MethodPost, mintPath, map[string]any{
		"name":            "ci runner, revoked by its own key",
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	}, nil, &minted); code != http.StatusCreated {
		t.Fatalf("an API-key principal could not mint: %d", code)
	}

	var revoked struct {
		GrantsRevoked int64 `json:"grants_revoked"`
		TokensRevoked int64 `json:"tokens_revoked"`
	}
	if code := mcpDoJSON(t, keyEng, http.MethodPost,
		mcp.ConnectionRevokePathFor(minted.GrantID), nil, nil, &revoked); code != http.StatusOK {
		t.Fatalf("an API-key principal could not revoke: %d", code)
	}
	if revoked.GrantsRevoked != 1 {
		t.Fatalf("grants_revoked = %d, want 1 for a first revoke", revoked.GrantsRevoked)
	}
	if revoked.TokensRevoked != 1 {
		t.Fatalf("tokens_revoked = %d, want 1: the cascade is the point of the method",
			revoked.TokensRevoked)
	}

	row := findMCPAuditRow(t, pool, tenantID, audit.ActionMCPGrantRevoked, minted.GrantID)
	assertActor(t, row, audit.ActionMCPGrantRevoked, audit.ActorAPIKey, keyID.String())
	if row.metadata["already_revoked"] != false {
		t.Fatalf("already_revoked = %v, want false for a first revoke", row.metadata["already_revoked"])
	}
	t.Logf("SITE 1 ok: mcp.grant.revoked attributed to api_key %s for grant %s",
		keyID, minted.GrantID)

	// DOES NOT OVER-FIRE. A session user revoking on the same route is still
	// recorded as a user, with their own id.
	userID := seedUserRow(t, pool, "revoke-actor-human-"+suffix+"@example.test")
	humanEng := mountConnectionsLikeProduction(t, svc, domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	})
	var humanMint mintResponse
	if code := mcpDoJSON(t, humanEng, http.MethodPost, mintPath, map[string]any{
		"name":            "human connection",
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	}, nil, &humanMint); code != http.StatusCreated {
		t.Fatalf("a session user could not mint: %d", code)
	}
	if code := mcpDoJSON(t, humanEng, http.MethodPost,
		mcp.ConnectionRevokePathFor(humanMint.GrantID), nil, nil, nil); code != http.StatusOK {
		t.Fatalf("a session user could not revoke: %d", code)
	}
	humanRow := findMCPAuditRow(t, pool, tenantID, audit.ActionMCPGrantRevoked, humanMint.GrantID)
	assertActor(t, humanRow, audit.ActionMCPGrantRevoked, audit.ActorUser, userID.String())
	t.Logf("SITE 1 does not over-fire: a session user's revoke is still user %s", userID)
}

// ---------------------------------------------------------------------------
// SITE 2: Service.Approve (internal/mcp/service.go), the /consent path.
//
// Scoped out of the earlier review on the grounds that consent is browser-only
// in practice. That reasoning does not survive reading the mount: /consent sits
// on the same Bearer-accepting group as the mint and the revoke, so an API-key
// holder can drive the OAuth flow headlessly and did produce a zero-uuid row
// when it did.
// ---------------------------------------------------------------------------

func TestMCPConsentByAnAPIKeyNamesTheKeyInTheAuditRowAsAppRole(t *testing.T) {
	pool := startPostgres(t)

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-consent-actor-"+suffix)
	seedSite(t, pool, tenantID, "https://consentactor-"+suffix+".example.test")
	auditActorReportRole(t, pool, tenantID)

	svc := mcp.NewService(mcp.NewRepo(pool)).
		WithAudit(audit.NewRecorder(pool, domain.SystemClock{}))

	keyID := uuid.New()
	keyEng := mountLikeProduction(t, svc, auditActorAPIKeyPrincipal(t, tenantID, keyID))

	keyGrantID := driveConsentAsPrincipal(t, keyEng, "api-key consent connection")
	row := findMCPAuditRow(t, pool, tenantID, audit.ActionMCPGrantCreated, keyGrantID)
	assertActor(t, row, audit.ActionMCPGrantCreated, audit.ActorAPIKey, keyID.String())
	if _, ok := row.metadata["issuance"]; ok {
		t.Fatalf("the consent path wrote an issuance key (%v); that field distinguishes "+
			"a headless mint from a browser-consented grant and belongs only to the mint",
			row.metadata["issuance"])
	}
	t.Logf("SITE 2 ok: mcp.grant.created via /consent attributed to api_key %s for grant %s",
		keyID, keyGrantID)

	// created_by_user_id stays NULL for a key. NULL is the honest answer, not a
	// gap to fill with a synthetic uuid: that column means "the human who
	// created it", and the actor lives on the audit row instead.
	grantUUID, err := uuid.Parse(keyGrantID)
	if err != nil {
		t.Fatalf("parse grant id: %v", err)
	}
	var createdBy *uuid.UUID
	if err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT created_by_user_id FROM mcp_grants WHERE tenant_id=$1 AND id=$2`,
			tenantID, grantUUID).Scan(&createdBy)
	}); err != nil {
		t.Fatalf("read created_by_user_id: %v", err)
	}
	if createdBy != nil {
		t.Fatalf("created_by_user_id = %v for an API-key consent, want NULL", *createdBy)
	}

	// DOES NOT OVER-FIRE. The browser path -- the one this site was assumed to
	// be the only caller of -- still records the approving human.
	userID := seedUserRow(t, pool, "consent-actor-human-"+suffix+"@example.test")
	humanEng := mountLikeProduction(t, svc, domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	})
	humanGrantID := driveConsentAsPrincipal(t, humanEng, "human consent connection")
	humanRow := findMCPAuditRow(t, pool, tenantID, audit.ActionMCPGrantCreated, humanGrantID)
	assertActor(t, humanRow, audit.ActionMCPGrantCreated, audit.ActorUser, userID.String())
	t.Logf("SITE 2 does not over-fire: an approving human is still user %s", userID)
}

// driveConsentAsPrincipal runs register -> authorize -> consent against a
// mounted engine and returns the grant id. The whole flow is driven rather than
// the service being called directly, so the principal reaching Approve is the
// one the route actually carries.
func driveConsentAsPrincipal(t *testing.T, eng *gin.Engine, grantName string) string {
	t.Helper()
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	var reg struct {
		ClientID string `json:"client_id"`
	}
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.RegisterPath, map[string]any{
		"redirect_uris":              []string{redirectURI},
		"client_name":                "Claude Desktop",
		"client_uri":                 "https://claude.ai",
		"token_endpoint_auth_method": "none",
	}, nil, &reg); code != http.StatusCreated {
		t.Fatalf("register answered %d, want 201", code)
	}

	verifier := "actor-verifier-" + uuid.NewString() + uuid.NewString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", string(mcp.ScopeRead))
	q.Set("state", "actor-state")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if code := mcpDoJSON(t, eng, http.MethodGet,
		mcp.AuthorizePath+"?"+q.Encode(), nil, nil, nil); code != http.StatusOK {
		t.Fatalf("authorize answered %d, want 200", code)
	}

	var approval struct {
		GrantID string `json:"grant_id"`
	}
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConsentPath, map[string]any{
		"client_id":             reg.ClientID,
		"redirect_uri":          redirectURI,
		"scopes":                []string{string(mcp.ScopeRead)},
		"state":                 "actor-state",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"name":                  grantName,
		"site_scope_mode":       string(mcp.SiteScopeModeAll),
	}, nil, &approval); code != http.StatusOK {
		t.Fatalf("consent answered %d, want 200", code)
	}
	if approval.GrantID == "" {
		t.Fatal("consent returned no grant id")
	}
	return approval.GrantID
}
