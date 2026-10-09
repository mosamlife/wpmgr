package mcp

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The token response's `scope` member names the scope set the grant holds,
// mcp_grants.oauth_scopes as stored, and nothing else. It used to be the
// constant "mcp:read" for every grant, so a sign-in approved with the
// advertised list was told it held less than it did.

// exchangeWithGrantScopes redeems one valid code whose grant stores held, and
// returns what Exchange answered and the store it ran against.
func exchangeWithGrantScopes(t *testing.T, held []string) (IssuedToken, *fakeStore, error) {
	t.Helper()
	const verifier = "verifier-for-the-token-scope-cases-000000000000"
	store := &fakeStore{
		codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
		clientOK: true, client: liveClient(registeredRedirect),
		grantOauthScopes: held,
	}
	got, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		ClientID: registeredClientID, CodeVerifier: verifier,
	})
	return got, store, err
}

func TestExchange_ScopeIsTheGrantsStoredSet(t *testing.T) {
	cases := []struct {
		name string
		held []string
		want string
	}{
		{"read-only grant", []string{"mcp:read"}, "mcp:read"},
		{"site and cache", []string{"mcp:read", "mcp:site", "mcp:cache"}, "mcp:cache mcp:read mcp:site"},
		{"site and cache stored in another order", []string{"mcp:cache", "mcp:site", "mcp:read"}, "mcp:cache mcp:read mcp:site"},
		{"site without cache", []string{"mcp:read", "mcp:site"}, "mcp:read mcp:site"},
		{"cache without site", []string{"mcp:read", "mcp:cache"}, "mcp:cache mcp:read"},
		{"a repeated name is named once", []string{"mcp:read", "mcp:read"}, "mcp:read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, store, err := exchangeWithGrantScopes(t, tc.held)
			if err != nil {
				t.Fatalf("exchange refused: %v", err)
			}
			if got.AccessToken == "" || store.tokensMinted != 1 {
				t.Fatalf("no token issued (access_token %q, minted %d)", got.AccessToken, store.tokensMinted)
			}
			// NEVER MORE THAN THE GRANT HOLDS, checked against the stored set
			// rather than against want, so a wrong want cannot hide a widening.
			for _, name := range strings.Split(got.Scope, " ") {
				if !slices.Contains(tc.held, name) {
					t.Errorf("scope names %q, which the grant does not hold (stored %v)", name, tc.held)
				}
			}
			if got.Scope != tc.want {
				t.Errorf("scope = %q, want %q; the grant stores %v", got.Scope, tc.want, tc.held)
			}
		})
	}
}

// A grant holding no scope is unstorable under
// mcp_grants_oauth_scopes_not_empty_check. If one is ever redeemed the answer
// is server_error and no access token, never an empty or defaulted scope.
func TestExchange_AGrantHoldingNoScopeGetsNoToken(t *testing.T) {
	for _, held := range [][]string{nil, {}} {
		got, _, err := exchangeWithGrantScopes(t, held)
		if err == nil {
			t.Fatalf("held %#v: exchange succeeded with scope %q", held, got.Scope)
		}
		if got.AccessToken != "" || got.Scope != "" {
			t.Fatalf("held %#v: a refused exchange returned %+v", held, got)
		}
		if status, body := oauthError(err); status != http.StatusInternalServerError || body.Err != "server_error" {
			t.Fatalf("held %#v: rendered as %d %q, want 500 server_error", held, status, body.Err)
		}
	}
}

func TestTokenResponseScope_NamesOnlyWellFormedStoredNames(t *testing.T) {
	refused := [][]Scope{
		nil,
		{},
		{""},
		{"mcp:read mcp:site"}, // one stored name; a client would read two
		{"mcp:read", "mcp:site\tmcp:cache"},
		{"mcp:read", `mcp:"site"`},
		{"mcp:read", `mcp:\site`},
		{"mcp:réad"},
		{"mcp:read\x7f"},
	}
	for _, held := range refused {
		if got, err := tokenResponseScope(held); err == nil {
			t.Errorf("tokenResponseScope(%q) = %q, want a refusal", held, got)
		}
	}

	// The refusal must not over-fire on any name this server grants, alone or
	// together.
	all := make([]Scope, 0, len(AdvertisedScopes()))
	for _, s := range AdvertisedScopes() {
		got, err := tokenResponseScope([]Scope{Scope(s)})
		if err != nil || got != s {
			t.Errorf("tokenResponseScope([%s]) = %q, %v; want %q", s, got, err, s)
		}
		all = append(all, Scope(s))
	}
	got, err := tokenResponseScope(all)
	if want := strings.Join(SupportedScopes(), " "); err != nil || got != want {
		t.Errorf("tokenResponseScope(every advertised scope) = %q, %v; want %q", got, err, want)
	}
}
