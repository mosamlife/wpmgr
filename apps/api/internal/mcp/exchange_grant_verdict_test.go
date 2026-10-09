package mcp

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// A code redeems only while its grant is authorized. A grant revoked or
// expired between consent and exchange, or an organisation whose assistant is
// paused, is refused with RFC 6749 invalid_grant, and the redeem transaction
// rolls back: no token is issued and the code is left unconsumed.
//
// The fake models the rollback rather than the refusal alone, for the reason
// atomicity_test.go gives: a fake that consumed and then refused would model a
// committed consume, and these assertions would pass against it.
func TestExchange_RefusesACodeWhoseGrantIsNoLongerAuthorized(t *testing.T) {
	const verifier = "verifier-for-the-unauthorized-grant-case-000000"
	newStore := func(grantAuthorized bool) *fakeStore {
		return &fakeStore{
			codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
			clientOK: true, client: liveClient(registeredRedirect),
			// The grant holds a nameable scope, so the only thing that differs
			// between the control and the refusal is the verdict.
			grantOauthScopes:   []string{string(ScopeRead)},
			grantNotAuthorized: !grantAuthorized,
		}
	}
	req := TokenRequest{
		GrantType: GrantTypeAuthorizationCode, Code: "the-code", RedirectURI: registeredRedirect,
		ClientID: registeredClientID, CodeVerifier: verifier,
	}

	// CONTROL: the identical fixture with the grant authorized issues a token.
	// Without it, a refusal below could come from a broken fixture.
	ctl := newStore(true)
	issued, err := NewService(ctl).Exchange(context.Background(), req)
	if err != nil {
		t.Fatalf("control: an authorized grant's code was refused: %v", err)
	}
	if issued.AccessToken == "" || ctl.tokensMinted != 1 || !ctl.consumed {
		t.Fatalf("control: access_token %q, minted %d, consumed %t; want a token, 1, true",
			issued.AccessToken, ctl.tokensMinted, ctl.consumed)
	}

	store := newStore(false)
	got, err := NewService(store).Exchange(context.Background(), req)
	if err == nil {
		t.Fatalf("a code whose grant is no longer authorized was exchanged for %+v", got)
	}
	if got.AccessToken != "" {
		t.Fatal("a refused exchange returned an access token")
	}

	// The refusal is the domain's invalid_grant, mapped on the wire to 400.
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Code != ErrCodeInvalidGrant {
		t.Fatalf("refusal = %v, want a domain error with code %s", err, ErrCodeInvalidGrant)
	}
	if status, body := oauthError(err); status != http.StatusBadRequest || body.Err != "invalid_grant" {
		t.Fatalf("rendered as %d %q, want 400 invalid_grant", status, body.Err)
	}

	// It was refused AT the redeem, after every earlier check had passed, so
	// this test is about the verdict and not about some earlier refusal.
	if store.consumeCalls != 1 {
		t.Fatalf("the redeem was attempted %d times, want 1", store.consumeCalls)
	}
	if store.tokensMinted != 0 {
		t.Fatalf("%d tokens were issued for a grant that is not authorized, want 0", store.tokensMinted)
	}
	if store.consumed {
		t.Fatal("the code was left consumed by a refused exchange; the redeem " +
			"transaction must roll the consume back with the token insert")
	}
}
