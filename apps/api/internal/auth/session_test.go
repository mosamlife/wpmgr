package auth

import (
	"context"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
)

// loadCtx mimics what LoadAndSave does: it produces a context that carries a
// fresh SCS session so Put/Get/Destroy work in a unit test.
func loadCtx(t *testing.T, m *SessionManager) context.Context {
	t.Helper()
	ctx, err := m.SCS().Load(context.Background(), "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	return ctx
}

func TestSessionLoginCurrentDestroy(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	user := uuid.New()
	tenant := uuid.New()

	if _, _, ok := m.Current(ctx); ok {
		t.Fatal("fresh session should have no user")
	}

	if err := m.Login(ctx, user, tenant); err != nil {
		t.Fatalf("login: %v", err)
	}
	gotUser, gotTenant, ok := m.Current(ctx)
	if !ok || gotUser != user || gotTenant != tenant {
		t.Fatalf("Current = (%v,%v,%v), want (%v,%v,true)", gotUser, gotTenant, ok, user, tenant)
	}

	if err := m.Destroy(ctx); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, _, ok := m.Current(ctx); ok {
		t.Fatal("destroyed session should have no user")
	}
}

func TestSessionOAuthRoundTrip(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	m.putOAuth(ctx, "state-1", "nonce-1", "verifier-1")
	nonce, verifier, ok := m.takeOAuthFor(ctx, "state-1")
	if !ok || nonce != "nonce-1" || verifier != "verifier-1" {
		t.Fatalf("oauth round trip mismatch: %q %q %v", nonce, verifier, ok)
	}
	// Single use: the handshake is gone once the callback it belongs to has
	// consumed it.
	if _, _, ok := m.takeOAuthFor(ctx, "state-1"); ok {
		t.Fatal("oauth handshake should be cleared after take")
	}
}

// A wrong state must not consume anything. /auth/oidc/callback is a GET, so a
// cross-site top-level navigation reaches it carrying the session cookie; if it
// popped whatever it found, that navigation would break the sign-in the visitor
// had in flight.
func TestSessionOAuthWrongStateLeavesTheHandshakeIntact(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	m.putOAuth(ctx, "state-1", "nonce-1", "verifier-1")

	if _, _, ok := m.takeOAuthFor(ctx, "forged"); ok {
		t.Fatal("a forged state must not match")
	}
	if _, _, ok := m.takeOAuthFor(ctx, ""); ok {
		t.Fatal("an absent state must not match")
	}
	if _, _, ok := m.takeOAuthFor(ctx, "state-1"); !ok {
		t.Fatal("the real callback lost its handshake to a request that was not it")
	}
}

// The other half of the same invariant: STARTING a handshake must not destroy
// one already in flight. The old single-slot storage could only hold the newest,
// so a top-level navigation to /auth/oidc/login (or to any /start) broke a
// sign-in the visitor had open in another tab.
func TestSessionHandshakesSurviveEachOther(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	m.putSocial(ctx, "google", "google-state", "google-nonce", "google-verifier")
	m.putOAuth(ctx, "oidc-state", "oidc-nonce", "oidc-verifier")
	m.putSocial(ctx, "github", "github-state", "github-nonce", "github-verifier")

	nonce, _, ok := m.takeSocialFor(ctx, "google", "google-state")
	if !ok || nonce != "google-nonce" {
		t.Fatalf("the first handshake did not survive the two started after it: %q %v", nonce, ok)
	}
	if nonce, _, ok := m.takeOAuthFor(ctx, "oidc-state"); !ok || nonce != "oidc-nonce" {
		t.Fatalf("the OIDC handshake was lost: %q %v", nonce, ok)
	}
	if nonce, _, ok := m.takeSocialFor(ctx, "github", "github-state"); !ok || nonce != "github-nonce" {
		t.Fatalf("the newest handshake was lost: %q %v", nonce, ok)
	}
}

// Bounded, because each entry is store space bought by an unauthenticated GET.
// The newest start always works; the oldest is what goes.
func TestSessionHandshakesAreBounded(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	m.putSocial(ctx, "google", "oldest", "n", "v")
	for i := 0; i < maxInFlightHandshakes; i++ {
		m.putSocial(ctx, "google", "state-"+string(rune('a'+i)), "n", "v")
	}

	if _, _, ok := m.takeSocialFor(ctx, "google", "oldest"); ok {
		t.Fatalf("more than %d handshakes are being kept per session", maxInFlightHandshakes)
	}
	if _, _, ok := m.takeSocialFor(ctx, "google", "state-a"); !ok {
		t.Fatal("the newest handshakes must be the ones kept")
	}
}

// A social callback must not be able to consume the generic OIDC handshake, or
// vice versa: the provider is what decides which adapter verifies the code.
func TestSessionHandshakeProviderIsPartOfTheMatch(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	m.putOAuth(ctx, "shared-state", "oidc-nonce", "oidc-verifier")
	if _, _, ok := m.takeSocialFor(ctx, "google", "shared-state"); ok {
		t.Fatal("a social callback consumed the operator-configured OIDC handshake")
	}

	m.putSocial(ctx, "google", "google-state", "n", "v")
	if _, _, ok := m.takeOAuthFor(ctx, "google-state"); ok {
		t.Fatal("the OIDC callback consumed a social handshake")
	}
}
