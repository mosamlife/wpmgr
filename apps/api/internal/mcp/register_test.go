package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// REGISTRATION IS A WRITE THEN A SEPARATE READ, AND BOTH HALVES CAN FAIL.
//
// The P1 this file exists for: RegisterMCPOAuthClient used RETURNING while the
// only policy its GUC enables is FOR INSERT. Under FORCE ROW LEVEL SECURITY the
// SELECT policy is enforced against the returned row as a WithCheckOption, so
// the statement raised SQLSTATE 42501 at ExecWithCheckOptions and rolled the
// transaction back. EVERY registration failed at runtime.
//
// It is not RETURNING *-specific -- returning the server-generated `id` alone
// fails identically -- so "return fewer columns" is not the fix. The query is
// now :execrows and hands back a COUNT, and the caller reads the row back in a
// separate lookup transaction.
//
// Why the suite was green while every registration was broken: the fake
// returned a fully populated row from the insert, which the database will not
// do. These tests exercise the count and the read-back as distinct failures so
// the fake cannot say yes to that shape again.
// ---------------------------------------------------------------------------

// The write reporting zero rows must stop the registration. :execrows was
// chosen over :exec precisely so there is something to assert; an
// INSERT ... VALUES with no ON CONFLICT writes one row or raises, so zero
// should be unreachable -- which is exactly why reaching it must be loud.
func TestRegister_ZeroRowsWrittenIsAFailureNotASuccess(t *testing.T) {
	for _, rows := range []int64{0, 2} {
		t.Run(strings.TrimSpace(string(rune('0'+rows)))+" rows", func(t *testing.T) {
			store := &fakeStore{registerRows: rows}
			got, err := NewService(store).Register(context.Background(), RegistrationRequest{
				RedirectURIs:            []string{"https://claude.ai/cb"},
				TokenEndpointAuthMethod: "none",
			})
			if err == nil {
				t.Fatalf("a write reporting %d rows was reported as success, returning "+
					"client_id %q; the caller would hold a credential for a row that "+
					"does not exist", rows, got.ClientID)
			}
			if got.ClientID != "" || got.ClientSecret != "" {
				t.Fatalf("a failed registration still returned credentials: %+v", got)
			}
		})
	}
}

// The write failing outright must surface, not be swallowed. This is the shape
// the real defect took: SQLSTATE 42501 on every call.
func TestRegister_WriteErrorSurfaces(t *testing.T) {
	store := &fakeStore{
		registerRows: 1,
		registerErr: &pgconn.PgError{
			Code:    "42501",
			Message: "new row violates row-level security policy for table \"mcp_oauth_clients\"",
		},
	}
	got, err := NewService(store).Register(context.Background(), RegistrationRequest{
		RedirectURIs:            []string{"https://claude.ai/cb"},
		TokenEndpointAuthMethod: "none",
	})
	if err == nil {
		t.Fatalf("a 42501 from the insert was reported as success: %+v", got)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("the underlying SQLSTATE was lost; got %v", err)
	}
}

// THE READ-BACK FINDING NOTHING IS A BROKEN INVARIANT, NOT AN EMPTY RESPONSE.
// The insert said it wrote one row; if the row cannot then be read, something
// is badly wrong. Rebuilding the response from the request body here would
// report a fabricated success on top of a real failure -- the exact defect
// class this package is built against.
func TestRegister_ReadBackFindingNothingFailsLoudly(t *testing.T) {
	store := &fakeStore{registerRows: 1, registerReadBackMissing: true}
	got, err := NewService(store).Register(context.Background(), RegistrationRequest{
		RedirectURIs:            []string{"https://claude.ai/cb"},
		ClientName:              "Claude Desktop",
		TokenEndpointAuthMethod: "none",
	})
	if err == nil {
		t.Fatalf("the registration wrote a row, could not read it back, and still "+
			"reported success: %+v", got)
	}
	if got.ClientID != "" || got.ClientSecret != "" {
		t.Fatalf("a failed read-back still returned credentials: %+v", got)
	}
	// It must not have papered over the failure with the request's own values.
	if strings.Contains(err.Error(), "Claude Desktop") {
		t.Error("the error echoes the request body; the response must not be " +
			"synthesised from the request when the read-back fails")
	}

	var sawLookup bool
	for _, c := range store.calls {
		if c == "LookupClient" {
			sawLookup = true
		}
	}
	if !sawLookup {
		t.Error("the registration never attempted a read-back at all")
	}
}

// The happy path: the response describes what the DATABASE holds, read back in
// a second transaction, not what the caller sent. The two differ here on
// purpose -- the name is padded and the auth method is defaulted -- so a
// response built from the request body fails this test.
func TestRegister_ResponseComesFromTheStoredRowNotTheRequest(t *testing.T) {
	store := &fakeStore{registerRows: 1}
	got, err := NewService(store).Register(context.Background(), RegistrationRequest{
		RedirectURIs: []string{"https://claude.ai/cb"},
		ClientName:   "  Claude Desktop  ", // trimmed on the way in
		ClientURI:    "  https://claude.ai  ",
		// token_endpoint_auth_method omitted: RFC 7591 defaults it to
		// client_secret_basic, so the stored value differs from the request.
	})
	if err != nil {
		t.Fatalf("a valid registration was refused: %v", err)
	}

	if got.ClientName != "Claude Desktop" {
		t.Errorf("client_name = %q, want the STORED (trimmed) value", got.ClientName)
	}
	if got.ClientURI != "https://claude.ai" {
		t.Errorf("client_uri = %q, want the STORED (trimmed) value", got.ClientURI)
	}
	if got.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Errorf("token_endpoint_auth_method = %q, want the stored default",
			got.TokenEndpointAuthMethod)
	}
	if got.ClientSecret == "" {
		t.Error("a client_secret_basic registration returned no secret")
	}
	if len(got.RedirectURIs) != 1 || got.RedirectURIs[0] != "https://claude.ai/cb" {
		t.Errorf("redirect_uris = %v", got.RedirectURIs)
	}

	// And it must be TWO round trips in the right order: the register
	// transaction cannot read the table, so the read-back is a separate
	// lookup transaction. Collapsing them back into one is the original bug.
	var order []string
	for _, c := range store.calls {
		if c == "RegisterClient" || c == "LookupClient" {
			order = append(order, c)
		}
	}
	if len(order) != 2 || order[0] != "RegisterClient" || order[1] != "LookupClient" {
		t.Fatalf("call order = %v, want [RegisterClient LookupClient]; the read-back "+
			"must be a separate transaction because the register GUC enables no "+
			"SELECT policy", order)
	}
}
