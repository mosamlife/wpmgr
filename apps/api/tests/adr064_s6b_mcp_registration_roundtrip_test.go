// adr064_s6b_mcp_registration_roundtrip_test.go: RFC 7591 dynamic client
// registration executed against the REAL schema as wpmgr_app.
//
// WHY THIS FILE EXISTS. RegisterMCPOAuthClient originally used RETURNING while
// the only policy app.mcp_client_register enables is FOR INSERT. Under FORCE
// ROW LEVEL SECURITY the SELECT policy is applied to the returned row as a
// WithCheckOption, so the statement raised SQLSTATE 42501 at
// ExecWithCheckOptions and rolled the transaction back: EVERY registration
// failed at runtime. It is not RETURNING *-specific -- returning the
// server-generated id alone fails identically -- so "return fewer columns" is
// not the fix.
//
// The whole Go suite was green throughout, because every proof above the SQL
// layer runs against a fake store that returned a fully populated row from the
// insert, which Postgres refuses outright. The absence of THIS test is what let
// it through, so it stays regardless of the outcome.
//
// WHAT IS ACTUALLY IN QUESTION HERE. That the register transaction cannot read
// the row is settled (42501). What nobody had established is that the LOOKUP
// transaction can. Registration now writes under one GUC and reads back under
// another, and if mcp_oauth_clients_lookup does not admit the row under
// app.mcp_client_lookup then registration writes a row it can never read: the
// endpoint returns an error having already created the client, leaving a live
// orphan row on every attempt. That is worse than the bug it replaced.
//
// HOW IT IS TESTED. Through mcp.Repo and mcp.Service -- the same dispatch
// production uses, the same generated queries, the same tx helpers. GUCs are
// never hand-set and no connection is opened by this file. A test that opened
// its own connection would leave every policy inert and pass regardless, which
// has happened in this codebase before.
//
// The role is load-bearing: wpmgr_app is NOSUPERUSER NOBYPASSRLS, and either
// privilege would make all of this pass vacuously. It is asserted, and printed.
package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// mcpAssertAndReportRole fails unless the transaction is wpmgr_app with neither
// SUPERUSER nor BYPASSRLS, and logs what it found so the proof carries its own
// evidence rather than asking the reader to trust it.
func mcpAssertAndReportRole(t *testing.T, tx pgx.Tx, where string) {
	t.Helper()
	var role string
	var super, bypass bool
	if err := tx.QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls
		   FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass); err != nil {
		t.Fatalf("%s: read the connection's own role: %v", where, err)
	}
	t.Logf("%s: current_user=%s rolsuper=%t rolbypassrls=%t", where, role, super, bypass)
	if super || bypass {
		t.Fatalf("%s: running as %q with rolsuper=%v rolbypassrls=%v; either one "+
			"bypasses every policy and this proof would pass without testing anything",
			where, role, super, bypass)
	}
	if role != "wpmgr_app" {
		t.Fatalf("%s: running as %q, not wpmgr_app, which is the role every real "+
			"install connects as", where, role)
	}
}

// TestMCPRegistrationRoundTripAsAppRole is the proof. Write under one GUC, read
// back under another, both as wpmgr_app.
func TestMCPRegistrationRoundTripAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)

	// Confirm the role inside each of the two transactions the path actually
	// uses, not on some other connection.
	if err := pool.InMCPClientRegisterTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientRegisterTx")
		return nil
	}); err != nil {
		t.Fatalf("open register tx: %v", err)
	}
	if err := pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open lookup tx: %v", err)
	}

	clientID := "test-client-" + uuid.NewString()
	secretHash := strings.Repeat("a", 64) // matches the '^[0-9a-f]{64}$' CHECK
	name := "Claude Desktop"
	uri := "https://claude.ai"

	// STEP 1: the write. Must report exactly one row.
	affected, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        &secretHash,
		TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris:            []string{"https://claude.ai/api/mcp/auth_callback"},
		ClientName:              &name,
		ClientUri:               &uri,
	})
	if err != nil {
		t.Fatalf("STEP 1 RegisterClient failed as wpmgr_app: %v\n"+
			"the registration INSERT itself is refused; this is the 42501 class", err)
	}
	if affected != 1 {
		t.Fatalf("STEP 1 RegisterClient wrote %d rows, want exactly 1", affected)
	}
	t.Logf("STEP 1 ok: InMCPClientRegisterTx -> RegisterMCPOAuthClient wrote %d row", affected)

	// STEP 2: THE QUESTION. Can the lookup transaction see what the register
	// transaction wrote?
	stored, err := repo.LookupClient(ctx, clientID)
	if err != nil {
		if err == pgx.ErrNoRows {
			t.Fatalf("STEP 2 FAILED: the row was written but mcp_oauth_clients_lookup "+
				"does not admit it under app.mcp_client_lookup. Registration would "+
				"create a live orphan row on every attempt and then return an error. "+
				"client_id=%s", clientID)
		}
		t.Fatalf("STEP 2 LookupClient failed as wpmgr_app: %v", err)
	}
	t.Logf("STEP 2 ok: InMCPClientLookupTx -> GetMCPOAuthClientByClientIDForLookup "+
		"returned the row (id=%s)", stored.ID)

	// STEP 3: the row carries the real stored values, every column the RFC 7591
	// response is built from. Named individually because "it returned a row" is
	// the outcome most easily half-reported.
	if stored.ClientID != clientID {
		t.Errorf("client_id = %q, want %q", stored.ClientID, clientID)
	}
	if stored.ClientSecretHash == nil || *stored.ClientSecretHash != secretHash {
		t.Errorf("client_secret_hash did not survive the round trip: %v", stored.ClientSecretHash)
	}
	if stored.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Errorf("token_endpoint_auth_method = %q", stored.TokenEndpointAuthMethod)
	}
	if len(stored.RedirectUris) != 1 || stored.RedirectUris[0] != "https://claude.ai/api/mcp/auth_callback" {
		t.Errorf("redirect_uris = %v", stored.RedirectUris)
	}
	if stored.ClientName == nil || *stored.ClientName != name {
		t.Errorf("client_name did not survive: %v", stored.ClientName)
	}
	if stored.ClientUri == nil || *stored.ClientUri != uri {
		t.Errorf("client_uri did not survive: %v", stored.ClientUri)
	}
	if stored.ID == uuid.Nil {
		t.Error("id is nil; the server-generated primary key did not come back")
	}
	t.Logf("STEP 3 ok: every column the RFC 7591 response is built from survived")
}

// TestMCPRegisterServiceEndToEndAsAppRole drives the whole thing through
// mcp.Service.Register -- the function the handler calls -- so the count
// assertion and the read-back run exactly as they will in production, and the
// response is checked against what the DATABASE holds rather than what the
// caller sent.
func TestMCPRegisterServiceEndToEndAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	svc := mcp.NewService(mcp.NewRepo(pool))

	// Padded on the way in and no auth method supplied, so the stored values
	// differ from the request on three fields. A response echoed from the
	// request body fails this.
	got, err := svc.Register(ctx, mcp.RegistrationRequest{
		RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"},
		ClientName:   "  Claude Desktop  ",
		ClientURI:    "  https://claude.ai  ",
	})
	if err != nil {
		t.Fatalf("Service.Register failed against a real database as wpmgr_app: %v", err)
	}

	if got.ClientID == "" {
		t.Fatal("no client_id returned")
	}
	if got.ClientSecret == "" {
		t.Error("client_secret_basic was defaulted per RFC 7591 but no secret was returned")
	}
	if got.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Errorf("token_endpoint_auth_method = %q, want the RFC 7591 default",
			got.TokenEndpointAuthMethod)
	}
	if got.ClientName != "Claude Desktop" {
		t.Errorf("client_name = %q, want the trimmed STORED value; an untrimmed "+
			"value means the response was echoed from the request", got.ClientName)
	}
	if got.ClientURI != "https://claude.ai" {
		t.Errorf("client_uri = %q, want the trimmed STORED value", got.ClientURI)
	}

	// And the row really is there, read independently.
	stored, err := mcp.NewRepo(pool).LookupClient(ctx, got.ClientID)
	if err != nil {
		t.Fatalf("the registration reported success but the client cannot be read back: %v", err)
	}
	if stored.ClientID != got.ClientID {
		t.Errorf("read-back client_id = %q, want %q", stored.ClientID, got.ClientID)
	}
	t.Logf("Service.Register end to end ok: client_id=%s stored and read back as wpmgr_app",
		got.ClientID)
}

// TestMCPRegisterTxCannotReadTheTable pins the constraint that shaped the
// design: the register GUC enables no SELECT policy, so a read inside that
// transaction finds nothing. This is why RETURNING failed and why the read-back
// must be a second transaction. If this ever starts returning rows, someone has
// widened the registration grant and the unauthenticated POST can enumerate
// every client on the installation, client_secret_hash included.
func TestMCPRegisterTxCannotReadTheTable(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	clientID := "isolation-probe-" + uuid.NewString()
	secretHash := strings.Repeat("b", 64)
	if _, err := mcp.NewRepo(pool).RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        &secretHash,
		TokenEndpointAuthMethod: "client_secret_post",
		RedirectUris:            []string{"https://example.com/cb"},
	}); err != nil {
		t.Fatalf("seed registration: %v", err)
	}

	var visible int
	if err := pool.InMCPClientRegisterTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "register-tx isolation probe")
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_clients`).Scan(&visible)
	}); err != nil {
		t.Fatalf("count inside the register tx: %v", err)
	}
	if visible != 0 {
		t.Fatalf("the register transaction can see %d rows in mcp_oauth_clients; it "+
			"must see none. Registration is an UNAUTHENTICATED POST, so a readable "+
			"table here lets any caller enumerate every registered client and its "+
			"client_secret_hash", visible)
	}
	t.Logf("register-tx isolation ok: 0 rows visible under app.mcp_client_register alone")
}

// compile-time guard: the fixture drives the real Store implementation.
var _ mcp.Store = (*mcp.Repo)(nil)

