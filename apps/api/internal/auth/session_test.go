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

	m.putOAuth(ctx, "state-1", "nonce-1", "verifier-1", "/sites/abc")
	state, nonce, verifier, returnTo := m.takeOAuth(ctx)
	if state != "state-1" || nonce != "nonce-1" || verifier != "verifier-1" || returnTo != "/sites/abc" {
		t.Fatalf("oauth round trip mismatch: %q %q %q %q", state, nonce, verifier, returnTo)
	}
	// Values are popped (cleared) on read.
	state2, _, _, returnTo2 := m.takeOAuth(ctx)
	if state2 != "" || returnTo2 != "" {
		t.Fatalf("oauth handshake should be cleared after take: state=%q return=%q", state2, returnTo2)
	}
}

// A handshake started with no deep link must not inherit the one an abandoned
// earlier handshake carried: putOAuth replaces the return path too.
func TestSessionOAuthReplacesAnAbandonedReturnPath(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	m.putOAuth(ctx, "state-1", "nonce-1", "verifier-1", "/sites/abc")
	m.putOAuth(ctx, "state-2", "nonce-2", "verifier-2", "")
	state, _, _, returnTo := m.takeOAuth(ctx)
	if state != "state-2" {
		t.Fatalf("state = %q, want the second handshake's", state)
	}
	if returnTo != "" {
		t.Fatalf("return path = %q, want the abandoned handshake's deep link gone", returnTo)
	}
}
