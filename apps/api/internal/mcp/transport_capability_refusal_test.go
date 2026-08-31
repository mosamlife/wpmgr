// Service.Authenticate argues in capitals that a capability refusal must be
// 403 and not 401, because an MCP client that receives 401 re-runs the OAuth
// handshake, a handshake cannot change a stored capability set, so the client
// loops and the operator is sent to rotate a credential that was never the
// problem.
//
// THE SERVICE RETURNED THAT VERDICT AND THE TRANSPORT DISCARDED IT. A domain
// error that was not KindUnauthorized matched neither branch in
// writeUnauthorized and fell through to the 401 at the bottom carrying the
// DEFAULT message and code -- "a valid bearer token is required" and
// mcp_invalid_grant -- so the refusal's own code never reached the client and
// the status reported a credential problem for a configuration state. The
// transport produced precisely the loop the service comment exists to prevent.
//
// THESE ARE EXECUTED REQUESTS THROUGH THE MOUNTED ROUTER, not a code read and
// not a direct call to writeUnauthorized. newTransportRouter calls
// TransportHandler.Register with the shape server.go passes it, so what these
// assert is the status line and body a client actually receives.
//
// THE TWO REFUSALS ARE BUILT BY DIFFERENT CONSTRUCTORS AND BOTH ARE TESTED.
// The empty-set refusal is domain.Forbidden; the NarrowTo refusal -- the
// mcp.content.read arm -- is domain.Validation, %w-wrapped. A fix tested only
// against the first would leave the second falling through.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// capabilityRefusalStore is liveGrantStore with the stored capability column
// set to whatever the case under test needs. The column is the authority
// Authenticate reads, so this is the one field that decides the outcome.
func capabilityRefusalStore(caps []string) *fakeStore {
	s := liveGrantStore(uuid.New())
	s.recheck.GrantCapabilities = caps
	return s
}

// infraFailStore makes the credential lookup fail with a NON-domain error --
// the database being unreachable, not the token being wrong. It wraps rather
// than extends fakeStore so no shared fixture changes shape for one test.
type infraFailStore struct{ *fakeStore }

func (s infraFailStore) LookupConnectionToken(context.Context, string) (sqlc.GetMCPConnectionTokenByHashForLookupRow, error) {
	return sqlc.GetMCPConnectionTokenByHashForLookupRow{}, errors.New("database is unreachable")
}

// rpcErrorData reads the `data.code` the transport attaches, which is where the
// domain code travels. Returns "" when absent, so a missing code is a visible
// empty string rather than a panic.
func rpcErrorData(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var d struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("error data is not an object: %v (%s)", err, string(raw))
	}
	return d.Code
}

// TestTransport_CapabilityRefusalIs403WithItsOwnCode is the F1 proof.
//
// Both cases assert THREE things, and the mutation reddens on all three:
// the status is 403 and not 401; the domain code survives rather than being
// replaced by mcp_invalid_grant; and WWW-Authenticate is absent.
//
// THE HEADER IS PART OF THE FIX, NOT A DETAIL. WWW-Authenticate is a CHALLENGE
// header -- it names the scheme and invites the client to try again with a
// credential. RFC 7235 defines it as the 401 companion. Sending it with a 403
// tells a client whose credential is valid to go and get another one, which is
// the retry this arm exists to stop, re-invited by a header. The status and the
// headers have to agree.
func TestTransport_CapabilityRefusalIs403WithItsOwnCode(t *testing.T) {
	cases := []struct {
		name     string
		caps     []string
		wantCode string
		why      string
	}{
		{
			name:     "empty stored capability set",
			caps:     []string{},
			wantCode: ErrCodeCapabilityUnmapped,
			why: "domain.Forbidden -- a grant whose capabilities column is '{}' " +
				"authenticates and can reach no tool",
		},
		{
			name:     "a stored capability no scope confers",
			caps:     []string{string(CapContentRead)},
			wantCode: ErrCodeCapabilityWiderThanDefault,
			why: "domain.Validation, %w-wrapped by Authenticate as " +
				"'resolve grant capabilities: %w' -- the NarrowTo refusal, which a " +
				"KindForbidden-only fix would miss entirely",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTransportRouter(t, capabilityRefusalStore(tc.caps))
			w := post(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)

			if w.Code != http.StatusForbidden {
				t.Errorf("HTTP %d, want 403 (%s).\n"+
					"A 401 here tells the client its credential is bad when the "+
					"credential is fine, and an MCP client answers 401 by re-running "+
					"the OAuth handshake -- which cannot change a stored capability "+
					"set. That is an infinite loop and an operator rotating the wrong "+
					"thing.\nbody: %s", w.Code, tc.why, w.Body.String())
			}

			resp := decodeRPC(t, w)
			if resp.Error == nil {
				t.Fatalf("no JSON-RPC error object: %s", w.Body.String())
			}
			got := rpcErrorData(t, resp.Error.Data)
			if got != tc.wantCode {
				t.Errorf("error data.code = %q, want %q.\n"+
					"The refusal's own code was discarded and replaced with the "+
					"function's default, so the client is told nothing it can act on.\n"+
					"message: %q", got, tc.wantCode, resp.Error.Message)
			}
			if resp.Error.Message == "a valid bearer token is required" {
				t.Errorf("message is the default credential message, so the service's " +
					"own explanation was discarded on the way out")
			}

			if h := w.Header().Get("WWW-Authenticate"); h != "" {
				t.Errorf("WWW-Authenticate = %q on a 403; want it absent. It is a "+
					"challenge header, and sending it here invites the retry this "+
					"status exists to stop", h)
			}
			t.Logf("%s -> HTTP %d code=%q www-authenticate=%q",
				tc.name, w.Code, got, w.Header().Get("WWW-Authenticate"))
		})
	}
}

// TestTransport_CredentialRefusalIsStill401WithAChallenge is the over-fire
// half. A guard that reddens correct work gets switched off, so the 403 arm
// must not swallow the case 401 is RIGHT for: a bearer that resolves to no live
// token. Here re-authenticating IS the remedy, so the status invites it and the
// challenge header belongs.
func TestTransport_CredentialRefusalIsStill401WithAChallenge(t *testing.T) {
	store := capabilityRefusalStore([]string{string(CapSitesRead)})
	store.tokenOK = false // the bearer resolves to nothing
	r := newTransportRouter(t, store)

	w := post(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("HTTP %d on an unresolvable bearer, want 401 -- the 403 arm has "+
			"over-fired and is now answering genuine credential failures with a "+
			"status that tells the client not to retry.\nbody: %s",
			w.Code, w.Body.String())
	}
	if h := w.Header().Get("WWW-Authenticate"); h == "" {
		t.Error("WWW-Authenticate is absent on a 401; a client is not told how to " +
			"authenticate")
	}
	t.Logf("unresolvable bearer -> HTTP %d www-authenticate=%q",
		w.Code, w.Header().Get("WWW-Authenticate"))
}

// TestTransport_InfrastructureFailureIsStill500WithNoChallenge is the other
// over-fire arm. An unreachable database is not an auth verdict at all: it must
// stay 500, must not be reported as a refusal of any kind, and must not carry a
// challenge header -- telling a caller to re-authenticate when the database is
// down sends them to rotate a credential that was never the problem.
func TestTransport_InfrastructureFailureIsStill500WithNoChallenge(t *testing.T) {
	r := newTransportRouter(t, infraFailStore{capabilityRefusalStore([]string{string(CapSitesRead)})})

	w := post(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("HTTP %d on an unreachable database, want 500.\nbody: %s",
			w.Code, w.Body.String())
	}
	if h := w.Header().Get("WWW-Authenticate"); h != "" {
		t.Errorf("WWW-Authenticate = %q on a 500; the infrastructure arm clears it "+
			"precisely so a database outage is not read as a credential problem", h)
	}
	if body := w.Body.String(); strings.Contains(body, "database is unreachable") {
		t.Errorf("the internal error leaked to the client: %s", body)
	}
	t.Logf("unreachable database -> HTTP %d www-authenticate=%q",
		w.Code, w.Header().Get("WWW-Authenticate"))
}
